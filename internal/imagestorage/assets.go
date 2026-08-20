package imagestorage

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

type AssetState string

const (
	Pending   AssetState = "PENDING"
	Available AssetState = "AVAILABLE"
	Failed    AssetState = "FAILED"
	Orphaned  AssetState = "ORPHANED"
)

type Asset struct {
	ID, ChargeID, RequestID, Protocol, Provider, ChannelID string
	ResultIndex                                            int
	ObjectKey, ContentType                                 string
	ByteLength                                             int64
	SHA256                                                 [sha256.Size]byte
	State                                                  AssetState
	FailureCategory                                        string
	CreatedAt, UpdatedAt                                   time.Time
}

type AssetRepository interface {
	Begin(context.Context, Asset) (Asset, error)
	MarkAvailable(context.Context, string) (Asset, error)
	MarkFailed(context.Context, string, string) (Asset, error)
}

type AssetStore struct{ pool *pgxpool.Pool }

func NewAssetStore(pool *pgxpool.Pool) *AssetStore { return &AssetStore{pool: pool} }

func AssetID(protocol, requestID string, resultIndex int) (string, error) {
	if !keyPartPattern.MatchString(protocol) || strings.TrimSpace(requestID) == "" || len(requestID) > 128 || resultIndex < 0 || resultIndex > 999 {
		return "", ErrInvalidObject
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", protocol, requestID, resultIndex)))
	return "asset_" + hex.EncodeToString(digest[:16]), nil
}

func (store *AssetStore) Begin(ctx context.Context, asset Asset) (Asset, error) {
	if store == nil || store.pool == nil || asset.ID == "" {
		return Asset{}, ErrUnavailable
	}
	_, err := store.pool.Exec(ctx, `INSERT INTO image_assets(id,charge_id,request_id,protocol,provider,channel_id,result_index,object_key,content_type,byte_length,sha256,state)
        VALUES($1,NULLIF($2,''),$3,$4,$5,$6,$7,$8,$9,$10,$11,'PENDING') ON CONFLICT(protocol,request_id,result_index) DO NOTHING`, asset.ID, asset.ChargeID, asset.RequestID, asset.Protocol, asset.Provider, asset.ChannelID, asset.ResultIndex, asset.ObjectKey, asset.ContentType, asset.ByteLength, asset.SHA256[:])
	if err != nil {
		return Asset{}, fmt.Errorf("persist image asset: %w", ErrUnavailable)
	}
	stored, err := store.load(ctx, asset.Protocol, asset.RequestID, asset.ResultIndex)
	if err != nil {
		return Asset{}, err
	}
	if stored.ID != asset.ID || stored.ObjectKey != asset.ObjectKey || stored.ContentType != asset.ContentType || stored.ByteLength != asset.ByteLength || stored.SHA256 != asset.SHA256 || stored.ChannelID != asset.ChannelID || stored.Provider != asset.Provider || stored.ChargeID != asset.ChargeID {
		return Asset{}, ErrInvalidObject
	}
	return stored, nil
}

func (store *AssetStore) MarkAvailable(ctx context.Context, id string) (Asset, error) {
	return store.transition(ctx, id, Available, "")
}

func (store *AssetStore) MarkFailed(ctx context.Context, id, category string) (Asset, error) {
	valid := map[string]bool{"fetch_rejected": true, "fetch_failed": true, "invalid_content": true, "upload_failed": true, "persistence_failed": true}
	if !valid[category] {
		return Asset{}, ErrInvalidObject
	}
	return store.transition(ctx, id, Failed, category)
}

func (store *AssetStore) transition(ctx context.Context, id string, state AssetState, category string) (Asset, error) {
	if store == nil || store.pool == nil || !strings.HasPrefix(id, "asset_") {
		return Asset{}, ErrUnavailable
	}
	command, err := store.pool.Exec(ctx, `UPDATE image_assets SET state=$2,failure_category=NULLIF($3,''),available_at=CASE WHEN $2='AVAILABLE' THEN now() ELSE NULL END,updated_at=now() WHERE id=$1 AND state='PENDING'`, id, state, category)
	if err != nil {
		return Asset{}, ErrUnavailable
	}
	if command.RowsAffected() == 0 {
		var existing Asset
		existing, err = store.loadID(ctx, id)
		if err != nil || existing.State != state || existing.FailureCategory != category {
			return Asset{}, ErrInvalidObject
		}
		return existing, nil
	}
	return store.loadID(ctx, id)
}

func (store *AssetStore) load(ctx context.Context, protocol, requestID string, index int) (Asset, error) {
	return scanAsset(store.pool.QueryRow(ctx, assetSelect+` WHERE protocol=$1 AND request_id=$2 AND result_index=$3`, protocol, requestID, index))
}

func (store *AssetStore) loadID(ctx context.Context, id string) (Asset, error) {
	return scanAsset(store.pool.QueryRow(ctx, assetSelect+` WHERE id=$1`, id))
}

const assetSelect = `SELECT id,COALESCE(charge_id,''),request_id,protocol,provider,channel_id,result_index,object_key,content_type,byte_length,sha256,state,COALESCE(failure_category,''),created_at,updated_at FROM image_assets`

func scanAsset(row pgx.Row) (Asset, error) {
	var asset Asset
	var digest []byte
	if err := row.Scan(&asset.ID, &asset.ChargeID, &asset.RequestID, &asset.Protocol, &asset.Provider, &asset.ChannelID, &asset.ResultIndex, &asset.ObjectKey, &asset.ContentType, &asset.ByteLength, &digest, &asset.State, &asset.FailureCategory, &asset.CreatedAt, &asset.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, ErrInvalidObject
		}
		return Asset{}, ErrUnavailable
	}
	if len(digest) != sha256.Size {
		return Asset{}, ErrInvalidObject
	}
	copy(asset.SHA256[:], digest)
	return asset, nil
}
