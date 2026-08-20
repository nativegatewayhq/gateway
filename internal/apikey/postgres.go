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
	result, err := store.pool.Exec(ctx, `INSERT INTO service_api_keys (id, name, key_digest, key_prefix, expires_at, project_id)
		SELECT $1, $2, $3, $4, $5, p.id FROM projects p JOIN organizations o ON o.id=p.organization_id
		WHERE p.id=$6 AND p.status='active' AND o.status='active'`, record.ID, record.Name, record.Digest[:], record.Prefix, record.ExpiresAt, record.ProjectID)
	if err == nil && result.RowsAffected() == 0 {
		return ErrProjectUnavailable
	}
	return err
}

func (store *PostgresStore) FindActiveByDigest(ctx context.Context, digest [32]byte, now time.Time) (Principal, error) {
	var principal Principal
	err := store.pool.QueryRow(ctx, `SELECT k.id, p.id, o.id FROM service_api_keys k
		JOIN projects p ON p.id=k.project_id JOIN organizations o ON o.id=p.organization_id
		WHERE k.key_digest=$1 AND k.status='active' AND p.status='active' AND o.status='active'
		AND (k.expires_at IS NULL OR k.expires_at > $2)`, digest[:], now).Scan(&principal.APIKeyID, &principal.ProjectID, &principal.OrganizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	return principal, err
}
