// Package runwayassets binds ephemeral Runway URI capabilities to tenants.
package runwayassets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

var (
	ErrInvalid  = errors.New("invalid Runway upload asset")
	ErrDenied   = errors.New("Runway upload asset is not authorized")
	ErrConflict = errors.New("Runway upload asset ownership conflict")
)

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &Store{pool: pool, now: time.Now}, nil
}

func (store *Store) Ready(ctx context.Context) error { return store.pool.Ping(ctx) }

func (store *Store) Bind(ctx context.Context, owner joboperation.Owner, channelID, uri string, expiresAt time.Time) error {
	if !validOwner(owner) || !validChannel(channelID) || !validURI(uri) || expiresAt.IsZero() {
		return ErrInvalid
	}
	issuedAt := store.now().UTC()
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(issuedAt) || expiresAt.After(issuedAt.Add(25*time.Hour)) {
		return ErrInvalid
	}
	digest := sha256.Sum256([]byte(uri))
	id := deterministicID("asset_", append([]byte(channelID), digest[:]...))
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `INSERT INTO runway_upload_assets(id,uri_digest,organization_id,project_id,api_key_id,channel_id,issued_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(channel_id,uri_digest) DO NOTHING`, id, digest[:], owner.OrganizationID, owner.ProjectID, owner.APIKeyID, channelID, issuedAt, expiresAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		var organizationID, projectID, apiKeyID string
		if err = tx.QueryRow(ctx, `SELECT organization_id,project_id,api_key_id FROM runway_upload_assets WHERE channel_id=$1 AND uri_digest=$2`, channelID, digest[:]).Scan(&organizationID, &projectID, &apiKeyID); err != nil {
			return err
		}
		if organizationID != owner.OrganizationID || projectID != owner.ProjectID || apiKeyID != owner.APIKeyID {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	eventID, err := randomID("event_")
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO runway_upload_asset_events(id,asset_id,category) VALUES($1,$2,'issued')`, eventID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) Authorize(ctx context.Context, owner joboperation.Owner, channelID, uri string) error {
	if !validOwner(owner) || !validChannel(channelID) || !validURI(uri) {
		return ErrInvalid
	}
	digest := sha256.Sum256([]byte(uri))
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var assetID string
	err = tx.QueryRow(ctx, `SELECT id FROM runway_upload_assets WHERE uri_digest=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND channel_id=$5 AND expires_at>$6`, digest[:], owner.OrganizationID, owner.ProjectID, owner.APIKeyID, channelID, store.now().UTC()).Scan(&assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	eventID, err := randomID("event_")
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO runway_upload_asset_events(id,asset_id,category) VALUES($1,$2,'authorized')`, eventID, assetID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validURI(value string) bool {
	return strings.HasPrefix(value, "runway://") && len(value) >= 13 && len(value) <= 5000 && strings.TrimSpace(value) == value && strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r == 0x7f }) == -1
}
func validOwner(owner joboperation.Owner) bool {
	return strings.HasPrefix(owner.OrganizationID, "org_") && strings.HasPrefix(owner.ProjectID, "project_") && strings.HasPrefix(owner.APIKeyID, "key_")
}
func validChannel(value string) bool {
	return strings.HasPrefix(value, "channel_") && len(value) == len("channel_")+32
}
func deterministicID(prefix string, value []byte) string {
	digest := sha256.Sum256(value)
	return prefix + hex.EncodeToString(digest[:16])
}
func randomID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}
