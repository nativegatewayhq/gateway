package apikey

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) Create(ctx context.Context, record Record) error {
	_, err := store.pool.Exec(ctx, `INSERT INTO service_api_keys
        (id, name, key_digest, key_prefix, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		record.ID, record.Name, record.Digest[:], record.Prefix, record.ExpiresAt)
	return err
}

func (store *PostgresStore) FindActiveByDigest(ctx context.Context, digest [32]byte, now time.Time) (Principal, error) {
	var principal Principal
	err := store.pool.QueryRow(ctx, `SELECT id FROM service_api_keys
        WHERE key_digest=$1 AND status='active' AND (expires_at IS NULL OR expires_at > $2)`, digest[:], now).Scan(&principal.APIKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	return principal, err
}
