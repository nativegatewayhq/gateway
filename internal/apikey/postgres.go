package apikey

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) Create(ctx context.Context, record Record) error {
	mode := record.ModelAccessMode
	if mode == "" {
		mode = ModelAccessAll
	}
	permissions, err := CanonicalModelPermissions(record.ModelPermissions)
	if err != nil || (mode == ModelAccessAll && len(permissions) != 0) || (mode == ModelAccessAllowlist && len(permissions) == 0) || (mode != ModelAccessAll && mode != ModelAccessAllowlist) {
		return ErrPolicyInvalid
	}
	var requestsPerMinute, burst any
	if record.RateLimit.Enabled() {
		requestsPerMinute, burst = record.RateLimit.RequestsPerMinute, record.RateLimit.Burst
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `INSERT INTO service_api_keys (id, name, key_digest, key_prefix, expires_at, project_id, requests_per_minute, burst, model_access_mode)
		SELECT $1, $2, $3, $4, $5, p.id, $7, $8, $9 FROM projects p JOIN organizations o ON o.id=p.organization_id
		WHERE p.id=$6 AND p.status='active' AND o.status='active'`, record.ID, record.Name, record.Digest[:], record.Prefix, record.ExpiresAt, record.ProjectID, requestsPerMinute, burst, mode)
	if err == nil && result.RowsAffected() == 0 {
		return ErrProjectUnavailable
	}
	if err != nil {
		return err
	}
	for _, permission := range permissions {
		if _, err := tx.Exec(ctx, `INSERT INTO service_api_key_model_permissions(api_key_id,protocol,operation,model) VALUES($1,$2,$3,$4)`, record.ID, permission.Protocol, permission.Operation, permission.Model); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (store *PostgresStore) FindActiveByDigest(ctx context.Context, digest [32]byte, now time.Time) (Principal, error) {
	var principal Principal
	rows, err := store.pool.Query(ctx, `SELECT k.id, p.id, o.id, k.requests_per_minute, k.burst, k.model_access_mode, mp.protocol, mp.operation, mp.model FROM service_api_keys k
		JOIN projects p ON p.id=k.project_id JOIN organizations o ON o.id=p.organization_id
		LEFT JOIN service_api_key_model_permissions mp ON mp.api_key_id=k.id
		WHERE k.key_digest=$1 AND k.status='active' AND p.status='active' AND o.status='active'
		AND (k.expires_at IS NULL OR k.expires_at > $2)
		ORDER BY mp.protocol,mp.operation,mp.model`, digest[:], now)
	if err != nil {
		return Principal{}, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var requestsPerMinute, burst *int64
		var protocol, operation, model *string
		if err := rows.Scan(&principal.APIKeyID, &principal.ProjectID, &principal.OrganizationID, &requestsPerMinute, &burst, &principal.ModelAccessMode, &protocol, &operation, &model); err != nil {
			return Principal{}, err
		}
		found = true
		if requestsPerMinute != nil && burst != nil {
			principal.RateLimit = RateLimitPolicy{RequestsPerMinute: *requestsPerMinute, Burst: *burst}
		}
		if protocol != nil && operation != nil && model != nil {
			principal.ModelPermissions = append(principal.ModelPermissions, ModelPermission{Protocol: *protocol, Operation: *operation, Model: *model})
		}
	}
	if err := rows.Err(); err != nil {
		return Principal{}, err
	}
	if !found {
		return Principal{}, ErrUnauthorized
	}
	permissions, policyErr := CanonicalModelPermissions(principal.ModelPermissions)
	if policyErr != nil || (principal.ModelAccessMode == ModelAccessAll && len(permissions) != 0) || (principal.ModelAccessMode == ModelAccessAllowlist && len(permissions) == 0) || (principal.ModelAccessMode != ModelAccessAll && principal.ModelAccessMode != ModelAccessAllowlist) {
		return Principal{}, ErrPolicyInvalid
	}
	principal.ModelPermissions = permissions
	return principal, nil
}
