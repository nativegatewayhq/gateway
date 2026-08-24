// Package audioassets manages private, tenant-scoped reusable audio inputs.
package audioassets

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
	ErrInvalid  = errors.New("invalid audio asset")
	ErrDenied   = errors.New("audio asset not found")
	ErrConflict = errors.New("audio asset conflict")
	ErrPending  = errors.New("audio asset pending")
)

const maximumAssetsPerProject int64 = 1000
const maximumBytesPerProject int64 = 10 << 30

type State string

const (
	Uploading State = "UPLOADING"
	Available State = "AVAILABLE"
	Deleting  State = "DELETING"
	Deleted   State = "DELETED"
	Failed    State = "FAILED"
)

type Asset struct {
	ID, OrganizationID, ProjectID, APIKeyID, ObjectKey, ContentType string
	ByteLength                                                      int64
	SHA256                                                          [32]byte
	State                                                           State
	ExpiresAt                                                       time.Time
	CreatedAt                                                       time.Time
}

type BeginRequest struct {
	Owner                  apikey.Principal
	IdempotencyKey         string
	Fingerprint            [32]byte
	ObjectKey, ContentType string
	ByteLength             int64
	SHA256                 [32]byte
	ExpiresAt              time.Time
}

type Lease struct {
	ID, AssetID, Owner string
	ExpiresAt          time.Time
}

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
	if !validBegin(request, repository.now()) {
		return Asset{}, ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Asset{}, err
	}
	defer tx.Rollback(ctx)
	lock := request.Owner.OrganizationID + ":audio.asset:" + request.IdempotencyKey
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return Asset{}, err
	}
	asset, fingerprint, found, err := loadPublication(ctx, tx, request.Owner.OrganizationID, request.IdempotencyKey)
	if err != nil {
		return Asset{}, err
	}
	if found {
		if !bytes.Equal(fingerprint, request.Fingerprint[:]) || !sameAssetRequest(asset, request) {
			return Asset{}, ErrConflict
		}
		if asset.State == Uploading {
			return Asset{}, ErrPending
		}
		if asset.State == Available {
			return asset, nil
		}
		return Asset{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, request.Owner.ProjectID+":audio.assets:quota"); err != nil {
		return Asset{}, err
	}
	var count, total int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(byte_length),0) FROM audio_input_assets WHERE project_id=$1 AND state<>'DELETED'`, request.Owner.ProjectID).Scan(&count, &total); err != nil {
		return Asset{}, err
	}
	if count >= maximumAssetsPerProject || total > maximumBytesPerProject-request.ByteLength {
		return Asset{}, ErrDenied
	}
	id, err := randomID(repository.entropy, "audasset_")
	if err != nil {
		return Asset{}, err
	}
	asset = Asset{ID: id, OrganizationID: request.Owner.OrganizationID, ProjectID: request.Owner.ProjectID, APIKeyID: request.Owner.APIKeyID, ObjectKey: request.ObjectKey, ContentType: request.ContentType, ByteLength: request.ByteLength, SHA256: request.SHA256, State: Uploading, ExpiresAt: request.ExpiresAt.UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO audio_input_assets(id,organization_id,project_id,api_key_id,object_key,content_type,byte_length,sha256,state,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'UPLOADING',$9)`, asset.ID, asset.OrganizationID, asset.ProjectID, asset.APIKeyID, asset.ObjectKey, asset.ContentType, asset.ByteLength, asset.SHA256[:], asset.ExpiresAt)
	if err != nil {
		return Asset{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_publications(organization_id,idempotency_key,asset_id,request_fingerprint) VALUES($1,$2,$3,$4)`, asset.OrganizationID, request.IdempotencyKey, asset.ID, request.Fingerprint[:]); err != nil {
		return Asset{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category) VALUES($1,'CREATED')`, asset.ID); err != nil {
		return Asset{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, err
	}
	return asset, nil
}

func (repository *Repository) MarkAvailable(ctx context.Context, id string) (Asset, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Asset{}, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_input_assets SET state='AVAILABLE',available_at=now(),failure_category=NULL WHERE id=$1 AND state='UPLOADING'`, id)
	if err != nil {
		return Asset{}, err
	}
	if result.RowsAffected() == 1 {
		_, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category) VALUES($1,'AVAILABLE')`, id)
		if err != nil {
			return Asset{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, err
	}
	return repository.getID(ctx, id)
}

func (repository *Repository) Resolve(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	if !validOwner(owner) || !validID(id, "audasset_") {
		return Asset{}, ErrDenied
	}
	var asset Asset
	var digest []byte
	err := repository.pool.QueryRow(ctx, assetSelect+` WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND state='AVAILABLE' AND expires_at>$5`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID, repository.now().UTC()).Scan(scanAsset(&asset, &digest)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrDenied
	}
	if err != nil {
		return Asset{}, err
	}
	copy(asset.SHA256[:], digest)
	return asset, nil
}

func (repository *Repository) RequestDelete(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	if !validOwner(owner) || !validID(id, "audasset_") {
		return Asset{}, ErrDenied
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Asset{}, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_input_assets SET state='DELETING' WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND state IN('UPLOADING','AVAILABLE','FAILED')`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID)
	if err != nil {
		return Asset{}, err
	}
	if result.RowsAffected() == 0 {
		return Asset{}, ErrDenied
	}
	_, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category) VALUES($1,'DELETE_REQUESTED')`, id)
	if err != nil {
		return Asset{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, err
	}
	return repository.getID(ctx, id)
}

func (repository *Repository) Acquire(ctx context.Context, owner apikey.Principal, id, leaseOwner string, duration time.Duration) (Asset, Lease, error) {
	if !validOwner(owner) || !validID(id, "audasset_") || strings.TrimSpace(leaseOwner) == "" || len(leaseOwner) > 128 || duration <= 0 || duration > 10*time.Minute {
		return Asset{}, Lease{}, ErrDenied
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Asset{}, Lease{}, err
	}
	defer tx.Rollback(ctx)
	var asset Asset
	var digest []byte
	err = tx.QueryRow(ctx, assetSelect+` WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4 AND state='AVAILABLE' AND expires_at>$5 FOR UPDATE`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID, repository.now().UTC()).Scan(scanAsset(&asset, &digest)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, Lease{}, ErrDenied
	}
	if err != nil {
		return Asset{}, Lease{}, err
	}
	copy(asset.SHA256[:], digest)
	leaseID, err := randomID(repository.entropy, "audlease_")
	if err != nil {
		return Asset{}, Lease{}, err
	}
	lease := Lease{ID: leaseID, AssetID: id, Owner: leaseOwner, ExpiresAt: repository.now().UTC().Add(duration)}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_leases(asset_id,lease_id,owner,expires_at) VALUES($1,$2,$3,$4)`, id, lease.ID, lease.Owner, lease.ExpiresAt); err != nil {
		return Asset{}, Lease{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category) VALUES($1,'LEASED')`, id); err != nil {
		return Asset{}, Lease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, Lease{}, err
	}
	return asset, lease, nil
}

func (repository *Repository) Release(ctx context.Context, lease Lease) error {
	if !validID(lease.AssetID, "audasset_") || !validID(lease.ID, "audlease_") || lease.Owner == "" {
		return ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `DELETE FROM audio_input_asset_leases WHERE asset_id=$1 AND lease_id=$2 AND owner=$3`, lease.AssetID, lease.ID, lease.Owner)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 1 {
		if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category) VALUES($1,'RELEASED')`, lease.AssetID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (repository *Repository) ClaimCleanup(ctx context.Context, owner string, duration time.Duration) (Asset, Lease, bool, error) {
	if strings.TrimSpace(owner) == "" || len(owner) > 128 || duration <= 0 || duration > 10*time.Minute {
		return Asset{}, Lease{}, false, ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM audio_input_asset_leases WHERE expires_at<=now()`); err != nil {
		return Asset{}, Lease{}, false, err
	}
	var asset Asset
	var digest []byte
	err = tx.QueryRow(ctx, assetSelect+` WHERE (state='DELETING' OR (state='AVAILABLE' AND expires_at<=now()) OR (state IN('UPLOADING','FAILED') AND updated_at<now()-interval '15 minutes')) AND NOT EXISTS(SELECT 1 FROM audio_input_asset_leases l WHERE l.asset_id=audio_input_assets.id AND l.expires_at>now()) ORDER BY updated_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(scanAsset(&asset, &digest)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, Lease{}, false, nil
	}
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	copy(asset.SHA256[:], digest)
	if asset.State != Deleting {
		if _, err = tx.Exec(ctx, `UPDATE audio_input_assets SET state='DELETING' WHERE id=$1`, asset.ID); err != nil {
			return Asset{}, Lease{}, false, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category,reason) VALUES($1,'DELETE_REQUESTED','expired_or_orphaned')`, asset.ID); err != nil {
			return Asset{}, Lease{}, false, err
		}
		asset.State = Deleting
	}
	leaseID, err := randomID(repository.entropy, "audlease_")
	if err != nil {
		return Asset{}, Lease{}, false, err
	}
	lease := Lease{ID: leaseID, AssetID: asset.ID, Owner: owner, ExpiresAt: repository.now().UTC().Add(duration)}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_leases(asset_id,lease_id,owner,expires_at) VALUES($1,$2,$3,$4)`, asset.ID, lease.ID, lease.Owner, lease.ExpiresAt); err != nil {
		return Asset{}, Lease{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Asset{}, Lease{}, false, err
	}
	return asset, lease, true, nil
}

func (repository *Repository) MarkDeleted(ctx context.Context, lease Lease) error {
	if !validID(lease.AssetID, "audasset_") || !validID(lease.ID, "audlease_") || lease.Owner == "" {
		return ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_input_assets SET state='DELETED',deleted_at=now(),failure_category=NULL WHERE id=$1 AND state='DELETING' AND EXISTS(SELECT 1 FROM audio_input_asset_leases WHERE asset_id=$1 AND lease_id=$2 AND owner=$3 AND expires_at>now())`, lease.AssetID, lease.ID, lease.Owner)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `DELETE FROM audio_input_asset_leases WHERE asset_id=$1 AND lease_id=$2`, lease.AssetID, lease.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category) VALUES($1,'DELETED')`, lease.AssetID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (repository *Repository) MarkFailed(ctx context.Context, id, reason string) error {
	if !validID(id, "audasset_") || strings.TrimSpace(reason) == "" || len(reason) > 100 {
		return ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_input_assets SET state='FAILED',failure_category=$2 WHERE id=$1 AND state='UPLOADING'`, id, reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_input_asset_events(asset_id,category,reason) VALUES($1,'FAILED',$2)`, id, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (repository *Repository) getID(ctx context.Context, id string) (Asset, error) {
	var a Asset
	var digest []byte
	err := repository.pool.QueryRow(ctx, assetSelect+` WHERE id=$1`, id).Scan(scanAsset(&a, &digest)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrDenied
	}
	if err != nil {
		return Asset{}, err
	}
	copy(a.SHA256[:], digest)
	return a, nil
}

const assetColumns = `audio_input_assets.id,audio_input_assets.organization_id,audio_input_assets.project_id,audio_input_assets.api_key_id,audio_input_assets.object_key,audio_input_assets.content_type,audio_input_assets.byte_length,audio_input_assets.sha256,audio_input_assets.state,audio_input_assets.expires_at,audio_input_assets.created_at`
const assetSelect = `SELECT ` + assetColumns + ` FROM audio_input_assets`

func scanAsset(asset *Asset, digest *[]byte) []any {
	return []any{&asset.ID, &asset.OrganizationID, &asset.ProjectID, &asset.APIKeyID, &asset.ObjectKey, &asset.ContentType, &asset.ByteLength, digest, &asset.State, &asset.ExpiresAt, &asset.CreatedAt}
}
func loadPublication(ctx context.Context, tx pgx.Tx, organization, key string) (Asset, []byte, bool, error) {
	var a Asset
	var digest, fingerprint []byte
	err := tx.QueryRow(ctx, `SELECT `+assetColumns+`,p.request_fingerprint FROM audio_input_asset_publications p JOIN audio_input_assets ON p.asset_id=audio_input_assets.id WHERE p.organization_id=$1 AND p.idempotency_key=$2`, organization, key).Scan(append(scanAsset(&a, &digest), &fingerprint)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, nil, false, nil
	}
	if err != nil {
		return Asset{}, nil, false, err
	}
	copy(a.SHA256[:], digest)
	return a, fingerprint, true, nil
}
func sameAssetRequest(a Asset, r BeginRequest) bool {
	return a.OrganizationID == r.Owner.OrganizationID && a.ProjectID == r.Owner.ProjectID && a.APIKeyID == r.Owner.APIKeyID && a.ObjectKey == r.ObjectKey && a.ContentType == r.ContentType && a.ByteLength == r.ByteLength && a.SHA256 == r.SHA256
}
func validBegin(r BeginRequest, now time.Time) bool {
	return validOwner(r.Owner) && len(r.IdempotencyKey) >= 1 && len(r.IdempotencyKey) <= 200 && strings.TrimSpace(r.IdempotencyKey) == r.IdempotencyKey && r.Fingerprint != ([32]byte{}) && validObjectKey(r.ObjectKey) && strings.HasPrefix(r.ContentType, "audio/") && len(r.ContentType) <= 100 && r.ByteLength > 0 && r.SHA256 != ([32]byte{}) && r.ExpiresAt.After(now) && r.ExpiresAt.Before(now.Add(31*24*time.Hour))
}
func validOwner(o apikey.Principal) bool {
	return strings.HasPrefix(o.OrganizationID, "org_") && strings.HasPrefix(o.ProjectID, "project_") && strings.HasPrefix(o.APIKeyID, "key_")
}
func validObjectKey(v string) bool {
	return strings.HasPrefix(v, "audio/") && len(v) <= 500 && !strings.Contains(v, "..") && !strings.ContainsAny(v, "\r\n")
}
func validID(v, p string) bool { return strings.HasPrefix(v, p) && len(v) == len(p)+32 }
func randomID(reader io.Reader, prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}
