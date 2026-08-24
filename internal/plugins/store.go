package plugins

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Sync atomically publishes an immutable plugin identity snapshot and its
// stable billing channel before any request can select it.
func (store *Store) Sync(ctx context.Context, registry *Registry) error {
	if store == nil || store.pool == nil || registry == nil {
		return ErrInvalidConfiguration
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, binding := range registry.Bindings() {
		name := "plugin-" + binding.ChannelID[len("channel_"):len("channel_")+16]
		if _, err = tx.Exec(ctx, `INSERT INTO provider_channels(id,provider,name,status) VALUES($1,'plugin',$2,'active') ON CONFLICT(id) DO NOTHING`, binding.ChannelID, name); err != nil {
			return err
		}
		var provider string
		if err = tx.QueryRow(ctx, `SELECT provider FROM provider_channels WHERE id=$1`, binding.ChannelID).Scan(&provider); err != nil || provider != "plugin" {
			return ErrInvalidConfiguration
		}
		if _, err = tx.Exec(ctx, `INSERT INTO plugin_channel_snapshots(channel_id,plugin_id,plugin_version,manifest_digest,model,protocol) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(channel_id) DO NOTHING`, binding.ChannelID, binding.PluginID, binding.Version, binding.ManifestDigest[:], binding.Model, binding.Protocol); err != nil {
			return err
		}
		var pluginID, version, model, protocol string
		var digest []byte
		if err = tx.QueryRow(ctx, `SELECT plugin_id,plugin_version,manifest_digest,model,protocol FROM plugin_channel_snapshots WHERE channel_id=$1`, binding.ChannelID).Scan(&pluginID, &version, &digest, &model, &protocol); err != nil {
			return err
		}
		if pluginID != binding.PluginID || version != binding.Version || model != binding.Model || protocol != binding.Protocol || len(digest) != 32 || subtle.ConstantTimeCompare(digest, binding.ManifestDigest[:]) != 1 {
			return ErrInvalidConfiguration
		}
	}
	if err = tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}
