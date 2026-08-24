package speechstorage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/apikey"
)

var (
	ErrDenied   = errors.New("speech output asset not found")
	ErrConflict = errors.New("speech output asset conflict")
	ErrPending  = errors.New("speech output asset pending")
)

type State string

const (
	Capturing   State = "CAPTURING"
	Persisting  State = "PERSISTING"
	Available   State = "AVAILABLE"
	Reconciling State = "RECONCILING"
	Deleting    State = "DELETING"
	Deleted     State = "DELETED"
	Failed      State = "FAILED"
)

type Asset struct {
	ID, OrganizationID, ProjectID, APIKeyID, ChargeID, ObjectKey, ContentType string
	RequestFingerprint, SHA256                                                [32]byte
	ByteLength                                                                int64
	State                                                                     State
	ExpiresAt, CreatedAt                                                      time.Time
}
type BeginRequest struct {
	Owner          apikey.Principal
	ChargeID       string
	IdempotencyKey string
	Fingerprint    [32]byte
	ExpiresAt      time.Time
}
type Lease struct{ ID, AssetID, Owner string }
type Repository struct {
	pool    *pgxpool.Pool
	entropy io.Reader
	now     func() time.Time
}

func NewRepository(pool *pgxpool.Pool) (*Repository, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &Repository{pool: pool, entropy: rand.Reader, now: time.Now}, nil
}

func (repository *Repository) Begin(ctx context.Context, request BeginRequest) (Asset, error) {
	if !validOwner(request.Owner) || len(request.IdempotencyKey) < 1 || len(request.IdempotencyKey) > 200 || strings.TrimSpace(request.IdempotencyKey) != request.IdempotencyKey || request.Fingerprint == ([32]byte{}) || !request.ExpiresAt.After(repository.now()) {
		return Asset{}, ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Asset{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, request.Owner.OrganizationID+":speech.output:"+request.IdempotencyKey); err != nil {
		return Asset{}, err
	}
	asset, fingerprint, found, err := loadPublication(ctx, tx, request.Owner.OrganizationID, request.IdempotencyKey)
	if err != nil {
		return Asset{}, err
	}
	if found {
		if !bytes.Equal(fingerprint, request.Fingerprint[:]) || asset.OrganizationID != request.Owner.OrganizationID || asset.ProjectID != request.Owner.ProjectID || asset.APIKeyID != request.Owner.APIKeyID || asset.ChargeID != request.ChargeID {
			return Asset{}, ErrConflict
		}
		if asset.State == Available {
			return asset, nil
		}
		if asset.State == Capturing || asset.State == Persisting || asset.State == Reconciling {
			return Asset{}, ErrPending
		}
		return Asset{}, ErrConflict
	}
	id, err := randomID(repository.entropy, "speechasset_")
	if err != nil {
		return Asset{}, err
	}
	asset = Asset{ID: id, OrganizationID: request.Owner.OrganizationID, ProjectID: request.Owner.ProjectID, APIKeyID: request.Owner.APIKeyID, ChargeID: request.ChargeID, RequestFingerprint: request.Fingerprint, State: Capturing, ExpiresAt: request.ExpiresAt.UTC()}
	var charge any
	if asset.ChargeID != "" {
		charge = asset.ChargeID
	}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_assets(id,organization_id,project_id,api_key_id,charge_id,request_fingerprint,state,expires_at) VALUES($1,$2,$3,$4,$5,$6,'CAPTURING',$7)`, asset.ID, asset.OrganizationID, asset.ProjectID, asset.APIKeyID, charge, asset.RequestFingerprint[:], asset.ExpiresAt); err != nil {
		return Asset{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_publications(organization_id,idempotency_key,asset_id,request_fingerprint) VALUES($1,$2,$3,$4)`, asset.OrganizationID, request.IdempotencyKey, asset.ID, asset.RequestFingerprint[:]); err != nil {
		return Asset{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'CREATED')`, asset.ID); err != nil {
		return Asset{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, err
	}
	return asset, nil
}

func (repository *Repository) MarkCaptured(ctx context.Context, id, objectKey, contentType string, size int64, digest [32]byte) error {
	result, err := repository.pool.Exec(ctx, `UPDATE speech_output_assets SET object_key=$2,content_type=$3,byte_length=$4,sha256=$5,state='PERSISTING' WHERE id=$1 AND state='CAPTURING'`, id, objectKey, contentType, size, digest[:])
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'CAPTURED')`, id)
	return err
}
func (repository *Repository) MarkAvailable(ctx context.Context, id string) (Asset, error) {
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return Asset{}, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE speech_output_assets SET state='AVAILABLE',available_at=now(),failure_category=NULL WHERE id=$1 AND state IN('PERSISTING','RECONCILING')`, id)
	if err != nil {
		return Asset{}, err
	}
	if result.RowsAffected() != 1 {
		return Asset{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'AVAILABLE')`, id); err != nil {
		return Asset{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, err
	}
	return repository.getID(ctx, id)
}
func (repository *Repository) MarkFailure(ctx context.Context, id, reason string, uncertain bool) error {
	state, category := "FAILED", "FAILED"
	if uncertain {
		state, category = "RECONCILING", "RECONCILING"
	}
	result, err := repository.pool.Exec(ctx, `UPDATE speech_output_assets SET state=$2,failure_category=$3 WHERE id=$1 AND state IN('CAPTURING','PERSISTING','RECONCILING')`, id, state, reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category,reason) VALUES($1,$2,$3)`, id, category, reason)
	return err
}
func (repository *Repository) Resolve(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	if !validOwner(owner) || !validID(id, "speechasset_") {
		return Asset{}, ErrDenied
	}
	var asset Asset
	var fingerprint, digest []byte
	err := repository.pool.QueryRow(ctx, assetSelect+` WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND state='AVAILABLE' AND expires_at>now()`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID).Scan(scanAsset(&asset, &fingerprint, &digest)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrDenied
	}
	if err != nil {
		return Asset{}, err
	}
	copy(asset.RequestFingerprint[:], fingerprint)
	copy(asset.SHA256[:], digest)
	return asset, nil
}

func (repository *Repository) AcquireRead(ctx context.Context, owner apikey.Principal, id, leaseOwner string, duration time.Duration) (Asset, Lease, error) {
	if !validOwner(owner) || !validID(id, "speechasset_") || strings.TrimSpace(leaseOwner) == "" || duration <= 0 || duration > 10*time.Minute {
		return Asset{}, Lease{}, ErrDenied
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return Asset{}, Lease{}, err
	}
	defer tx.Rollback(ctx)
	var asset Asset
	var fingerprint, digest []byte
	err = tx.QueryRow(ctx, assetSelect+` WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND state='AVAILABLE' AND expires_at>now() FOR UPDATE`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID).Scan(scanAsset(&asset, &fingerprint, &digest)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, Lease{}, ErrDenied
	}
	if err != nil {
		return Asset{}, Lease{}, err
	}
	copy(asset.RequestFingerprint[:], fingerprint)
	copy(asset.SHA256[:], digest)
	leaseID, err := randomID(repository.entropy, "speechlease_")
	if err != nil {
		return Asset{}, Lease{}, err
	}
	lease := Lease{ID: leaseID, AssetID: id, Owner: leaseOwner}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_leases(asset_id,lease_id,owner,expires_at) VALUES($1,$2,$3,now()+$4::interval)`, id, lease.ID, lease.Owner, duration.String()); err != nil {
		return Asset{}, Lease{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'LEASED')`, id); err != nil {
		return Asset{}, Lease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, Lease{}, err
	}
	return asset, lease, nil
}

func (repository *Repository) Release(ctx context.Context, lease Lease) error {
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `DELETE FROM speech_output_asset_leases WHERE asset_id=$1 AND lease_id=$2 AND owner=$3`, lease.AssetID, lease.ID, lease.Owner)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 1 {
		if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'RELEASED')`, lease.AssetID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (repository *Repository) ClaimRecovery(ctx context.Context, owner string, duration time.Duration) (Asset, Lease, bool, error) {
	if strings.TrimSpace(owner) == "" || duration <= 0 || duration > 10*time.Minute {
		return Asset{}, Lease{}, false, ErrInvalid
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	defer tx.Rollback(ctx)
	_, _ = tx.Exec(ctx, `DELETE FROM speech_output_asset_leases WHERE expires_at<=now()`)
	var a Asset
	var f, d []byte
	err = tx.QueryRow(ctx, assetSelect+` WHERE state IN('PERSISTING','RECONCILING') AND object_key IS NOT NULL AND NOT EXISTS(SELECT 1 FROM speech_output_asset_leases l WHERE l.asset_id=speech_output_assets.id AND l.expires_at>now()) ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(scanAsset(&a, &f, &d)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, Lease{}, false, nil
	}
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	copy(a.RequestFingerprint[:], f)
	copy(a.SHA256[:], d)
	leaseID, err := randomID(repository.entropy, "speechlease_")
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	lease := Lease{ID: leaseID, AssetID: a.ID, Owner: owner}
	if _, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_leases(asset_id,lease_id,owner,expires_at) VALUES($1,$2,$3,now()+$4::interval)`, a.ID, lease.ID, lease.Owner, duration.String()); err != nil {
		return Asset{}, Lease{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, Lease{}, false, err
	}
	return a, lease, true, nil
}
func (repository *Repository) RequestDelete(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	if !validOwner(owner) || !validID(id, "speechasset_") {
		return Asset{}, ErrDenied
	}
	result, err := repository.pool.Exec(ctx, `UPDATE speech_output_assets SET state='DELETING' WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND state IN('CAPTURING','PERSISTING','AVAILABLE','RECONCILING','FAILED')`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID)
	if err != nil {
		return Asset{}, err
	}
	if result.RowsAffected() != 1 {
		return Asset{}, ErrDenied
	}
	_, err = repository.pool.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'DELETE_REQUESTED')`, id)
	if err != nil {
		return Asset{}, err
	}
	return repository.getID(ctx, id)
}
func (repository *Repository) ClaimCleanup(ctx context.Context, owner string, duration time.Duration) (Asset, Lease, bool, error) {
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	defer tx.Rollback(ctx)
	_, _ = tx.Exec(ctx, `DELETE FROM speech_output_asset_leases WHERE expires_at<=now()`)
	var a Asset
	var f, d []byte
	err = tx.QueryRow(ctx, assetSelect+` WHERE (state='DELETING' OR (expires_at<=now() AND state<>'DELETED') OR (state IN('CAPTURING','PERSISTING','RECONCILING','FAILED') AND updated_at<now()-interval '15 minutes')) AND NOT EXISTS(SELECT 1 FROM speech_output_asset_leases l WHERE l.asset_id=speech_output_assets.id AND l.expires_at>now()) ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(scanAsset(&a, &f, &d)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, Lease{}, false, nil
	}
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	copy(a.RequestFingerprint[:], f)
	copy(a.SHA256[:], d)
	if a.State != Deleting {
		_, err = tx.Exec(ctx, `UPDATE speech_output_assets SET state='DELETING' WHERE id=$1`, a.ID)
		if err != nil {
			return Asset{}, Lease{}, false, err
		}
		a.State = Deleting
	}
	lid, _ := randomID(repository.entropy, "speechlease_")
	lease := Lease{ID: lid, AssetID: a.ID, Owner: owner}
	_, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_leases(asset_id,lease_id,owner,expires_at) VALUES($1,$2,$3,now()+$4::interval)`, a.ID, lid, owner, duration.String())
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, Lease{}, false, err
	}
	return a, lease, true, nil
}
func (repository *Repository) MarkDeleted(ctx context.Context, l Lease) error {
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE speech_output_assets SET state='DELETED',deleted_at=now() WHERE id=$1 AND state='DELETING' AND EXISTS(SELECT 1 FROM speech_output_asset_leases WHERE asset_id=$1 AND lease_id=$2 AND owner=$3 AND expires_at>now())`, l.AssetID, l.ID, l.Owner)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `DELETE FROM speech_output_asset_leases WHERE asset_id=$1 AND lease_id=$2`, l.AssetID, l.ID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO speech_output_asset_events(asset_id,category) VALUES($1,'DELETED')`, l.AssetID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const assetColumns = `speech_output_assets.id,speech_output_assets.organization_id,speech_output_assets.project_id,speech_output_assets.api_key_id,COALESCE(speech_output_assets.charge_id,''),speech_output_assets.request_fingerprint,COALESCE(speech_output_assets.object_key,''),COALESCE(speech_output_assets.content_type,''),COALESCE(speech_output_assets.byte_length,0),COALESCE(speech_output_assets.sha256,'\x'::bytea),speech_output_assets.state,speech_output_assets.expires_at,speech_output_assets.created_at`
const assetSelect = `SELECT ` + assetColumns + ` FROM speech_output_assets`

func scanAsset(a *Asset, f, d *[]byte) []any {
	return []any{&a.ID, &a.OrganizationID, &a.ProjectID, &a.APIKeyID, &a.ChargeID, f, &a.ObjectKey, &a.ContentType, &a.ByteLength, d, &a.State, &a.ExpiresAt, &a.CreatedAt}
}
func (repository *Repository) getID(ctx context.Context, id string) (Asset, error) {
	var a Asset
	var f, d []byte
	err := repository.pool.QueryRow(ctx, assetSelect+` WHERE id=$1`, id).Scan(scanAsset(&a, &f, &d)...)
	if err != nil {
		return Asset{}, err
	}
	copy(a.RequestFingerprint[:], f)
	copy(a.SHA256[:], d)
	return a, nil
}
func loadPublication(ctx context.Context, tx pgx.Tx, organization, key string) (Asset, []byte, bool, error) {
	var a Asset
	var f, d, p []byte
	err := tx.QueryRow(ctx, `SELECT `+assetColumns+`,p.request_fingerprint FROM speech_output_asset_publications p JOIN speech_output_assets ON p.asset_id=speech_output_assets.id WHERE p.organization_id=$1 AND p.idempotency_key=$2`, organization, key).Scan(append(scanAsset(&a, &f, &d), &p)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, nil, false, nil
	}
	if err != nil {
		return Asset{}, nil, false, err
	}
	copy(a.RequestFingerprint[:], f)
	copy(a.SHA256[:], d)
	return a, p, true, nil
}
func validOwner(o apikey.Principal) bool {
	return strings.HasPrefix(o.OrganizationID, "org_") && strings.HasPrefix(o.ProjectID, "project_") && strings.HasPrefix(o.APIKeyID, "key_")
}
func validID(v, p string) bool { return strings.HasPrefix(v, p) && len(v) == len(p)+32 }
func randomID(r io.Reader, p string) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return p + hex.EncodeToString(b), nil
}
