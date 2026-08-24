package videostorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Asset struct {
	ID, JobID, ChargeID, Provider, ChannelID string
	ResultIndex                              int
	ObjectKey, ContentType                   string
	ByteLength                               int64
	SHA256                                   [sha256.Size]byte
	State, LeaseOwner                        string
	LeaseUntil                               *time.Time
}

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) (*Repository, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &Repository{pool: pool}, nil
}
func (repository *Repository) Ready(ctx context.Context) error { return repository.pool.Ping(ctx) }

func AssetID(jobID string, index int) (string, error) {
	if !strings.HasPrefix(jobID, "job_") || index < 0 || index > 9 {
		return "", ErrInvalid
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", jobID, index)))
	return "vasset_" + hex.EncodeToString(digest[:16]), nil
}

func (repository *Repository) Get(ctx context.Context, jobID string, index int) (Asset, bool, error) {
	return scanAsset(repository.pool.QueryRow(ctx, assetSelect+` WHERE job_id=$1 AND result_index=$2`, jobID, index))
}

func (repository *Repository) Begin(ctx context.Context, asset Asset) (Asset, error) {
	if asset.ID == "" || asset.JobID == "" || asset.Provider != "runway" && asset.Provider != "plugin" || asset.ChannelID == "" || asset.ObjectKey == "" || asset.ByteLength < 1 {
		return Asset{}, ErrInvalid
	}
	_, err := repository.pool.Exec(ctx, `WITH inserted AS (
		INSERT INTO video_assets(id,job_id,charge_id,provider,channel_id,result_index,object_key,content_type,byte_length,sha256,state)
		VALUES($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,'PENDING') ON CONFLICT(job_id,result_index) DO NOTHING RETURNING id
	) INSERT INTO video_asset_events(asset_id,category) SELECT id,'created' FROM inserted`, asset.ID, asset.JobID, asset.ChargeID, asset.Provider, asset.ChannelID, asset.ResultIndex, asset.ObjectKey, asset.ContentType, asset.ByteLength, asset.SHA256[:])
	if err != nil {
		return Asset{}, ErrUnavailable
	}
	stored, found, err := repository.Get(ctx, asset.JobID, asset.ResultIndex)
	if err != nil || !found {
		return Asset{}, ErrUnavailable
	}
	if stored.ID != asset.ID || stored.ObjectKey != asset.ObjectKey || stored.ContentType != asset.ContentType || stored.ByteLength != asset.ByteLength || stored.SHA256 != asset.SHA256 || stored.ChannelID != asset.ChannelID || stored.ChargeID != asset.ChargeID {
		return Asset{}, ErrInvalid
	}
	return stored, nil
}

func (repository *Repository) Claim(ctx context.Context, id, owner string, lease time.Duration) (Asset, bool, error) {
	if id == "" || owner == "" || lease <= 0 || lease > 30*time.Minute {
		return Asset{}, false, ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `WITH changed AS (
		UPDATE video_assets SET lease_owner=$2,lease_until=now()+$3::interval,updated_at=now()
		WHERE id=$1 AND state='PENDING' AND (lease_until IS NULL OR lease_until<=now()) RETURNING id
	) INSERT INTO video_asset_events(asset_id,category) SELECT id,'claimed' FROM changed`, id, owner, lease.String())
	if err != nil {
		return Asset{}, false, ErrUnavailable
	}
	asset, found, err := scanAsset(repository.pool.QueryRow(ctx, assetSelect+` WHERE id=$1`, id))
	return asset, found && result.RowsAffected() == 1, err
}

func (repository *Repository) MarkAvailable(ctx context.Context, id, owner string) (Asset, error) {
	result, err := repository.pool.Exec(ctx, `WITH changed AS (
		UPDATE video_assets SET state='AVAILABLE',available_at=now(),lease_owner=NULL,lease_until=NULL,updated_at=now()
		WHERE id=$1 AND state='PENDING' AND lease_owner=$2 AND lease_until>now() RETURNING id
	) INSERT INTO video_asset_events(asset_id,category) SELECT id,'available' FROM changed`, id, owner)
	if err != nil {
		return Asset{}, ErrUnavailable
	}
	asset, found, loadErr := scanAsset(repository.pool.QueryRow(ctx, assetSelect+` WHERE id=$1`, id))
	if loadErr != nil || !found || (result.RowsAffected() == 0 && asset.State != "AVAILABLE") {
		return Asset{}, ErrUnavailable
	}
	return asset, nil
}

func (repository *Repository) Release(ctx context.Context, id, owner string) error {
	_, err := repository.pool.Exec(ctx, `WITH changed AS (
		UPDATE video_assets SET lease_owner=NULL,lease_until=NULL,updated_at=now()
		WHERE id=$1 AND state='PENDING' AND lease_owner=$2 RETURNING id
	) INSERT INTO video_asset_events(asset_id,category) SELECT id,'released' FROM changed`, id, owner)
	return err
}

const assetSelect = `SELECT id,job_id,COALESCE(charge_id,''),provider,channel_id,result_index,object_key,content_type,byte_length,sha256,state,COALESCE(lease_owner,''),lease_until FROM video_assets`

func scanAsset(row pgx.Row) (Asset, bool, error) {
	var asset Asset
	var digest []byte
	err := row.Scan(&asset.ID, &asset.JobID, &asset.ChargeID, &asset.Provider, &asset.ChannelID, &asset.ResultIndex, &asset.ObjectKey, &asset.ContentType, &asset.ByteLength, &digest, &asset.State, &asset.LeaseOwner, &asset.LeaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return asset, false, nil
	}
	if err != nil || len(digest) != sha256.Size {
		return asset, false, ErrUnavailable
	}
	copy(asset.SHA256[:], digest)
	return asset, true, nil
}
