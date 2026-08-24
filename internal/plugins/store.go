package plugins

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (store *Store) LastRegistryIndex(ctx context.Context) (uint64, string, error) {
	if store == nil || store.pool == nil {
		return 0, "", ErrInvalidConfiguration
	}
	var sequence int64
	var digest []byte
	err := store.pool.QueryRow(ctx, `SELECT sequence,index_digest FROM plugin_registry_index_snapshots ORDER BY sequence DESC LIMIT 1`).Scan(&sequence, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil || sequence < 1 || len(digest) != 32 {
		return 0, "", ErrInvalidConfiguration
	}
	return uint64(sequence), "sha256:" + hex.EncodeToString(digest), nil
}

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
	if evidence, admitted := registry.IndexEvidence(); admitted {
		var lastSequence int64
		var lastDigest []byte
		lastErr := tx.QueryRow(ctx, `SELECT sequence,index_digest FROM plugin_registry_index_snapshots ORDER BY sequence DESC LIMIT 1`).Scan(&lastSequence, &lastDigest)
		if lastErr != nil && !errors.Is(lastErr, pgx.ErrNoRows) {
			return lastErr
		}
		if lastErr == nil {
			switch {
			case evidence.Sequence < uint64(lastSequence):
				return ErrInvalidConfiguration
			case evidence.Sequence == uint64(lastSequence) && subtle.ConstantTimeCompare(lastDigest, evidence.IndexDigest[:]) != 1:
				return ErrInvalidConfiguration
			case evidence.Sequence > uint64(lastSequence) && (evidence.Sequence != uint64(lastSequence)+1 || subtle.ConstantTimeCompare(lastDigest, evidence.PreviousDigest[:]) != 1):
				return ErrInvalidConfiguration
			}
		}
		var previous any
		if evidence.Sequence > 1 {
			previous = evidence.PreviousDigest[:]
		}
		if _, err = tx.Exec(ctx, `INSERT INTO plugin_registry_index_snapshots(sequence,index_digest,envelope_digest,previous_index_digest,registry_created_at,registry_expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(sequence) DO NOTHING`, evidence.Sequence, evidence.IndexDigest[:], evidence.EnvelopeDigest[:], previous, evidence.CreatedAt, evidence.ExpiresAt); err != nil {
			return err
		}
		var storedIndex, storedEnvelope, storedPrevious []byte
		if err = tx.QueryRow(ctx, `SELECT index_digest,envelope_digest,previous_index_digest FROM plugin_registry_index_snapshots WHERE sequence=$1`, evidence.Sequence).Scan(&storedIndex, &storedEnvelope, &storedPrevious); err != nil || subtle.ConstantTimeCompare(storedIndex, evidence.IndexDigest[:]) != 1 || subtle.ConstantTimeCompare(storedEnvelope, evidence.EnvelopeDigest[:]) != 1 || evidence.Sequence == 1 && storedPrevious != nil || evidence.Sequence > 1 && subtle.ConstantTimeCompare(storedPrevious, evidence.PreviousDigest[:]) != 1 {
			return ErrInvalidConfiguration
		}
	}
	for _, binding := range registry.Bindings() {
		name := "plugin-" + binding.ChannelID[len("channel_"):len("channel_")+16]
		if _, err = tx.Exec(ctx, `INSERT INTO provider_channels(id,provider,name,status) VALUES($1,'plugin',$2,'active') ON CONFLICT(id) DO NOTHING`, binding.ChannelID, name); err != nil {
			return err
		}
		var provider string
		if err = tx.QueryRow(ctx, `SELECT provider FROM provider_channels WHERE id=$1`, binding.ChannelID).Scan(&provider); err != nil || provider != "plugin" {
			return ErrInvalidConfiguration
		}
		var registrySequence, registryIndex, registryEnvelope, admission any
		if binding.RegistrySequence > 0 {
			registrySequence, registryIndex, registryEnvelope, admission = binding.RegistrySequence, binding.RegistryIndexDigest[:], binding.RegistryEnvelopeDigest[:], binding.AdmissionDigest[:]
		}
		if _, err = tx.Exec(ctx, `INSERT INTO plugin_channel_snapshots(channel_id,plugin_id,plugin_version,manifest_digest,model,protocol,registry_sequence,registry_index_digest,registry_envelope_digest,registry_admission_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(channel_id) DO NOTHING`, binding.ChannelID, binding.PluginID, binding.Version, binding.ManifestDigest[:], binding.Model, binding.Protocol, registrySequence, registryIndex, registryEnvelope, admission); err != nil {
			return err
		}
		var pluginID, version, model, protocol string
		var digest, storedRegistryIndex, storedRegistryEnvelope, storedAdmission []byte
		var storedRegistrySequence *int64
		if err = tx.QueryRow(ctx, `SELECT plugin_id,plugin_version,manifest_digest,model,protocol,registry_sequence,registry_index_digest,registry_envelope_digest,registry_admission_digest FROM plugin_channel_snapshots WHERE channel_id=$1`, binding.ChannelID).Scan(&pluginID, &version, &digest, &model, &protocol, &storedRegistrySequence, &storedRegistryIndex, &storedRegistryEnvelope, &storedAdmission); err != nil {
			return err
		}
		if pluginID != binding.PluginID || version != binding.Version || model != binding.Model || protocol != binding.Protocol || len(digest) != 32 || subtle.ConstantTimeCompare(digest, binding.ManifestDigest[:]) != 1 {
			return ErrInvalidConfiguration
		}
		if binding.RegistrySequence == 0 && (storedRegistrySequence != nil || storedRegistryIndex != nil || storedRegistryEnvelope != nil || storedAdmission != nil) || binding.RegistrySequence > 0 && (storedRegistrySequence == nil || *storedRegistrySequence != int64(binding.RegistrySequence) || subtle.ConstantTimeCompare(storedRegistryIndex, binding.RegistryIndexDigest[:]) != 1 || subtle.ConstantTimeCompare(storedRegistryEnvelope, binding.RegistryEnvelopeDigest[:]) != 1 || subtle.ConstantTimeCompare(storedAdmission, binding.AdmissionDigest[:]) != 1) {
			return ErrInvalidConfiguration
		}
	}
	if err = tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}
