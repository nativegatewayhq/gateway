package providercredentials

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidLifecycle           = errors.New("invalid provider credential lifecycle request")
	ErrLifecycleConflict          = errors.New("provider credential lifecycle conflict")
	ErrCredentialNotFound         = errors.New("provider credential not found")
	ErrCredentialStoreUnavailable = errors.New("provider credential store unavailable")
)

type CredentialState string

const (
	Staged  CredentialState = "staged"
	Active  CredentialState = "active"
	Retired CredentialState = "retired"
)

type Metadata struct {
	ID, ChannelID string
	Provider      ProviderID
	Version       int64
	State         CredentialState
}

type StageRequest struct {
	ChannelID, Actor, Reason, OperationKey string
	Provider                               ProviderID
	Plaintext                              []byte
}

type LifecycleRequest struct {
	CredentialID, Actor, Reason, OperationKey string
}

type Event struct {
	ID, CredentialID, ChannelID, Action, Actor, Reason, OperationKey string
	Provider                                                         ProviderID
	Version                                                          int64
}

type Store struct {
	pool    *pgxpool.Pool
	keyring MasterKeyProvider
	entropy io.Reader
}

func NewStore(pool *pgxpool.Pool, keyring MasterKeyProvider) *Store {
	return &Store{pool: pool, keyring: keyring, entropy: rand.Reader}
}

func ValidateStageRequest(request StageRequest) error {
	if !validChannelID(request.ChannelID) || request.Provider.validateExact() != nil || !validAudit(request.Actor, 200) || !validAudit(request.Reason, 500) || !validOperationKey(request.OperationKey) || validateCredentialBytes(request.Plaintext) != nil {
		return ErrInvalidLifecycle
	}
	return nil
}

func ValidateLifecycleRequest(request LifecycleRequest) error {
	if !validCredentialID(request.CredentialID) || !validAudit(request.Actor, 200) || !validAudit(request.Reason, 500) || !validOperationKey(request.OperationKey) {
		return ErrInvalidLifecycle
	}
	return nil
}

func ValidChannelID(channelID string) bool { return validChannelID(channelID) }

func (store *Store) Stage(ctx context.Context, request StageRequest) (Metadata, error) {
	if store == nil || store.pool == nil || store.keyring == nil || ValidateStageRequest(request) != nil {
		return Metadata{}, ErrInvalidLifecycle
	}
	plaintext := append([]byte(nil), request.Plaintext...)
	defer zero(plaintext)
	tags, err := store.keyring.OperationTags(plaintext, "stage", request.ChannelID, string(request.Provider), request.OperationKey)
	if err != nil {
		return Metadata{}, err
	}
	defer func() {
		for _, tag := range tags {
			zero(tag)
		}
	}()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Metadata{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, request.OperationKey); err != nil {
		return Metadata{}, err
	}
	if replay, found, replayErr := lifecycleReplay(ctx, tx, request.OperationKey, "stage", tags); found || replayErr != nil {
		return replay, replayErr
	}
	var channelProvider string
	if err := tx.QueryRow(ctx, `SELECT provider FROM provider_channels WHERE id=$1 FOR UPDATE`, request.ChannelID).Scan(&channelProvider); errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, ErrInvalidLifecycle
	} else if err != nil {
		return Metadata{}, err
	}
	if channelProvider != string(request.Provider) {
		return Metadata{}, ErrScopeMismatch
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM provider_credentials WHERE channel_id=$1`, request.ChannelID).Scan(&version); err != nil {
		return Metadata{}, err
	}
	id, err := store.id("pcred_")
	if err != nil {
		return Metadata{}, err
	}
	envelope, err := store.encrypt(plaintext, id, request.ChannelID, request.Provider, version)
	if err != nil {
		return Metadata{}, err
	}
	defer envelope.destroy()
	metadata := Metadata{ID: id, ChannelID: request.ChannelID, Provider: request.Provider, Version: version, State: Staged}
	_, err = tx.Exec(ctx, `INSERT INTO provider_credentials(id,channel_id,provider,version,state,ciphertext,nonce,wrapped_data_key,wrap_nonce,master_key_id,created_by,created_reason) VALUES($1,$2,$3,$4,'staged',$5,$6,$7,$8,$9,$10,$11)`, id, request.ChannelID, request.Provider, version, envelope.ciphertext, envelope.nonce, envelope.wrappedKey, envelope.wrapNonce, envelope.keyID, request.Actor, request.Reason)
	if err != nil {
		return Metadata{}, err
	}
	if err := store.recordLifecycle(ctx, tx, metadata, "stage", request.Actor, request.Reason, request.OperationKey, tags[0]); err != nil {
		return Metadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (store *Store) Activate(ctx context.Context, request LifecycleRequest) (Metadata, error) {
	return store.transition(ctx, request, "activate", Active)
}

func (store *Store) Retire(ctx context.Context, request LifecycleRequest) (Metadata, error) {
	return store.transition(ctx, request, "retire", Retired)
}

func (store *Store) transition(ctx context.Context, request LifecycleRequest, action string, target CredentialState) (Metadata, error) {
	if store == nil || store.pool == nil || store.keyring == nil || ValidateLifecycleRequest(request) != nil {
		return Metadata{}, ErrInvalidLifecycle
	}
	tags, err := store.keyring.OperationTags(nil, action, request.CredentialID, request.OperationKey)
	if err != nil {
		return Metadata{}, err
	}
	defer func() {
		for _, tag := range tags {
			zero(tag)
		}
	}()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Metadata{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, request.OperationKey); err != nil {
		return Metadata{}, err
	}
	if replay, found, replayErr := lifecycleReplay(ctx, tx, request.OperationKey, action, tags); found || replayErr != nil {
		return replay, replayErr
	}
	metadata, err := metadataInTx(ctx, tx, request.CredentialID, true)
	if err != nil {
		return Metadata{}, err
	}
	var channelProvider string
	if err := tx.QueryRow(ctx, `SELECT provider FROM provider_channels WHERE id=$1 FOR UPDATE`, metadata.ChannelID).Scan(&channelProvider); err != nil {
		return Metadata{}, err
	}
	if channelProvider != string(metadata.Provider) {
		return Metadata{}, ErrScopeMismatch
	}
	var automaticallyRetired *Metadata
	switch action {
	case "activate":
		if metadata.State != Staged {
			return Metadata{}, ErrLifecycleConflict
		}
		var previous Metadata
		previousErr := tx.QueryRow(ctx, `SELECT id,channel_id,provider,version,state FROM provider_credentials WHERE channel_id=$1 AND state='active' FOR UPDATE`, metadata.ChannelID).Scan(&previous.ID, &previous.ChannelID, &previous.Provider, &previous.Version, &previous.State)
		if previousErr == nil {
			previous.State = Retired
			automaticallyRetired = &previous
		} else if !errors.Is(previousErr, pgx.ErrNoRows) {
			return Metadata{}, previousErr
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET state='retired',retired_at=now() WHERE channel_id=$1 AND state='active'`, metadata.ChannelID); err != nil {
			return Metadata{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET state='active',activated_at=now(),retired_at=NULL WHERE id=$1`, metadata.ID); err != nil {
			return Metadata{}, err
		}
	case "retire":
		if metadata.State == Retired {
			return Metadata{}, ErrLifecycleConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET state='retired',retired_at=now() WHERE id=$1`, metadata.ID); err != nil {
			return Metadata{}, err
		}
	default:
		return Metadata{}, ErrInvalidLifecycle
	}
	metadata.State = target
	if err := store.recordLifecycle(ctx, tx, metadata, action, request.Actor, request.Reason, request.OperationKey, tags[0]); err != nil {
		return Metadata{}, err
	}
	if automaticallyRetired != nil {
		if err := store.appendEvent(ctx, tx, *automaticallyRetired, "retire", request.Actor, request.Reason, request.OperationKey); err != nil {
			return Metadata{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (store *Store) Resolve(ctx context.Context, channelID string, provider ProviderID) (Credential, error) {
	if store == nil || store.pool == nil || store.keyring == nil || !validChannelID(channelID) || provider.validateExact() != nil {
		return Credential{}, ErrCredentialUnavailable
	}
	var channelProvider string
	if err := store.pool.QueryRow(ctx, `SELECT provider FROM provider_channels WHERE id=$1`, channelID).Scan(&channelProvider); errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrCredentialUnavailable
	} else if err != nil {
		return Credential{}, ErrCredentialStoreUnavailable
	}
	if channelProvider != string(provider) {
		return Credential{}, ErrScopeMismatch
	}
	var id, storedProvider, keyID string
	var version int64
	var ciphertext, nonce, wrappedKey, wrapNonce []byte
	err := store.pool.QueryRow(ctx, `SELECT id,provider,version,ciphertext,nonce,wrapped_data_key,wrap_nonce,master_key_id FROM provider_credentials WHERE channel_id=$1 AND state='active'`, channelID).Scan(&id, &storedProvider, &version, &ciphertext, &nonce, &wrappedKey, &wrapNonce, &keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrCredentialUnavailable
	}
	if err != nil {
		return Credential{}, ErrCredentialStoreUnavailable
	}
	if storedProvider != string(provider) {
		return Credential{}, ErrScopeMismatch
	}
	defer zero(ciphertext)
	defer zero(nonce)
	defer zero(wrappedKey)
	defer zero(wrapNonce)
	plaintext, err := store.decrypt(envelope{ciphertext: ciphertext, nonce: nonce, wrappedKey: wrappedKey, wrapNonce: wrapNonce, keyID: keyID}, id, channelID, provider, version)
	if err != nil {
		return Credential{}, err
	}
	return Credential{provider: provider, channelID: channelID, value: plaintext}, nil
}

func (store *Store) ConfiguredChannels(ctx context.Context) (map[string]ProviderID, error) {
	if store == nil || store.pool == nil || store.keyring == nil {
		return map[string]ProviderID{}, nil
	}
	rows, err := store.pool.Query(ctx, `SELECT credential.channel_id,credential.provider FROM provider_credentials credential JOIN provider_channels channel ON channel.id=credential.channel_id AND channel.provider=credential.provider WHERE credential.state='active' ORDER BY credential.channel_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]ProviderID{}
	for rows.Next() {
		var channel string
		var provider ProviderID
		if err := rows.Scan(&channel, &provider); err != nil {
			return nil, err
		}
		result[channel] = provider
	}
	return result, rows.Err()
}

func (store *Store) List(ctx context.Context, channelID string) ([]Metadata, error) {
	if store == nil || store.pool == nil || !validChannelID(channelID) {
		return nil, ErrInvalidLifecycle
	}
	rows, err := store.pool.Query(ctx, `SELECT id,channel_id,provider,version,state FROM provider_credentials WHERE channel_id=$1 ORDER BY version`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []Metadata
	for rows.Next() {
		var item Metadata
		if err := rows.Scan(&item.ID, &item.ChannelID, &item.Provider, &item.Version, &item.State); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

func (store *Store) Events(ctx context.Context, credentialID string) ([]Event, error) {
	if store == nil || store.pool == nil || !validCredentialID(credentialID) {
		return nil, ErrInvalidLifecycle
	}
	rows, err := store.pool.Query(ctx, `SELECT id,credential_id,channel_id,provider,credential_version,action,actor,reason,operation_key FROM provider_credential_events WHERE credential_id=$1 ORDER BY created_at,id`, credentialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var output []Event
	for rows.Next() {
		var item Event
		if err := rows.Scan(&item.ID, &item.CredentialID, &item.ChannelID, &item.Provider, &item.Version, &item.Action, &item.Actor, &item.Reason, &item.OperationKey); err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	return output, rows.Err()
}

type envelope struct {
	ciphertext, nonce, wrappedKey, wrapNonce []byte
	keyID                                    string
}

func (value *envelope) destroy() {
	zero(value.ciphertext)
	zero(value.nonce)
	zero(value.wrappedKey)
	zero(value.wrapNonce)
}

func (store *Store) encrypt(plaintext []byte, id, channelID string, provider ProviderID, version int64) (envelope, error) {
	keyID, err := store.keyring.CurrentID()
	if err != nil {
		return envelope{}, err
	}
	dataKey := make([]byte, 32)
	if _, err := io.ReadFull(store.entropy, dataKey); err != nil {
		return envelope{}, ErrKeyUnavailable
	}
	defer zero(dataKey)
	payloadAEAD, err := newAEAD(dataKey)
	if err != nil {
		return envelope{}, ErrKeyUnavailable
	}
	nonce := make([]byte, payloadAEAD.NonceSize())
	if _, err := io.ReadFull(store.entropy, nonce); err != nil {
		return envelope{}, ErrKeyUnavailable
	}
	aad := envelopeAAD("payload", id, channelID, provider, version)
	ciphertext := payloadAEAD.Seal(nil, nonce, plaintext, aad)
	wrapped, wrapNonce, err := store.keyring.WrapDataKey(keyID, dataKey, envelopeAAD("wrap", id, channelID, provider, version))
	if err != nil {
		return envelope{}, err
	}
	return envelope{ciphertext: ciphertext, nonce: nonce, wrappedKey: wrapped, wrapNonce: wrapNonce, keyID: keyID}, nil
}

func (store *Store) decrypt(value envelope, id, channelID string, provider ProviderID, version int64) ([]byte, error) {
	if !validCredentialID(id) || !validChannelID(channelID) || provider.validateExact() != nil || version < 1 || !validKeyID(value.keyID) || len(value.nonce) != 12 || len(value.wrapNonce) != 12 || len(value.wrappedKey) != 48 || len(value.ciphertext) < 17 || len(value.ciphertext) > maxCredentialLength+16 {
		return nil, ErrDecrypt
	}
	dataKey, err := store.keyring.UnwrapDataKey(value.keyID, value.wrappedKey, value.wrapNonce, envelopeAAD("wrap", id, channelID, provider, version))
	if err != nil {
		return nil, ErrDecrypt
	}
	defer zero(dataKey)
	if len(dataKey) != 32 {
		return nil, ErrDecrypt
	}
	payloadAEAD, err := newAEAD(dataKey)
	if err != nil {
		return nil, ErrDecrypt
	}
	plaintext, err := payloadAEAD.Open(nil, value.nonce, value.ciphertext, envelopeAAD("payload", id, channelID, provider, version))
	if err != nil || validateCredentialBytes(plaintext) != nil {
		zero(plaintext)
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func envelopeAAD(purpose, id, channelID string, provider ProviderID, version int64) []byte {
	return []byte(fmt.Sprintf("nativegateway/provider-credential/v1\x00%s\x00%s\x00%s\x00%s\x00%d", purpose, id, channelID, provider, version))
}

func (store *Store) recordLifecycle(ctx context.Context, tx pgx.Tx, metadata Metadata, action, actor, reason, operationKey string, tag []byte) error {
	if _, err := tx.Exec(ctx, `INSERT INTO provider_credential_lifecycle_operations(operation_key,action,request_tag,credential_id) VALUES($1,$2,$3,$4)`, operationKey, action, tag, metadata.ID); err != nil {
		return err
	}
	return store.appendEvent(ctx, tx, metadata, action, actor, reason, operationKey)
}

func (store *Store) appendEvent(ctx context.Context, tx pgx.Tx, metadata Metadata, action, actor, reason, operationKey string) error {
	eventID, err := store.id("pcevt_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO provider_credential_events(id,credential_id,channel_id,provider,credential_version,action,actor,reason,operation_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, eventID, metadata.ID, metadata.ChannelID, metadata.Provider, metadata.Version, action, actor, reason, operationKey)
	return err
}

func lifecycleReplay(ctx context.Context, tx pgx.Tx, operationKey, action string, tags [][]byte) (Metadata, bool, error) {
	var storedAction, credentialID string
	var storedTag []byte
	err := tx.QueryRow(ctx, `SELECT action,request_tag,credential_id FROM provider_credential_lifecycle_operations WHERE operation_key=$1`, operationKey).Scan(&storedAction, &storedTag, &credentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, err
	}
	matched := false
	for _, tag := range tags {
		matched = subtle.ConstantTimeCompare(storedTag, tag) == 1 || matched
	}
	if storedAction != action || !matched {
		return Metadata{}, true, ErrLifecycleConflict
	}
	metadata, err := metadataInTx(ctx, tx, credentialID, false)
	if err == nil {
		switch storedAction {
		case "stage":
			metadata.State = Staged
		case "activate":
			metadata.State = Active
		case "retire":
			metadata.State = Retired
		}
	}
	return metadata, true, err
}

func metadataInTx(ctx context.Context, tx pgx.Tx, id string, lock bool) (Metadata, error) {
	query := `SELECT id,channel_id,provider,version,state FROM provider_credentials WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var metadata Metadata
	err := tx.QueryRow(ctx, query, id).Scan(&metadata.ID, &metadata.ChannelID, &metadata.Provider, &metadata.Version, &metadata.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, ErrCredentialNotFound
	}
	return metadata, err
}

func (store *Store) id(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(store.entropy, raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%x", prefix, raw), nil
}

func validateCredentialBytes(value []byte) error {
	if len(value) == 0 || len(value) > maxCredentialLength || !bytes.Equal(bytes.TrimSpace(value), value) || !utf8.Valid(value) {
		return ErrMalformedCredential
	}
	for len(value) > 0 {
		character, size := utf8.DecodeRune(value)
		if unicode.IsControl(character) {
			return ErrMalformedCredential
		}
		value = value[size:]
	}
	return nil
}

func validAudit(value string, maximum int) bool {
	return strings.TrimSpace(value) == value && len(value) >= 1 && len(value) <= maximum
}
func validOperationKey(value string) bool { return validAudit(value, 200) }
func validCredentialID(value string) bool { return validPrefixedHexID(value, "pcred_") }
func validChannelID(value string) bool    { return validPrefixedHexID(value, "channel_") }
func validPrefixedHexID(value, prefix string) bool {
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, character := range value[len(prefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (credential *Credential) Destroy() {
	if credential == nil {
		return
	}
	zero(credential.value)
	credential.value = nil
}

func (credential Credential) ApplyChannel(request *http.Request, channelID string, provider ProviderID) error {
	if credential.channelID == "" || credential.channelID != channelID {
		return ErrScopeMismatch
	}
	return credential.Apply(request, provider)
}
