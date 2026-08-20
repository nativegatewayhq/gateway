package apikey

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
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
	networkMode := record.NetworkAccessMode
	if networkMode == "" {
		networkMode = NetworkAccessAll
	}
	networkPrefixes, err := CanonicalNetworkPrefixes(record.NetworkPrefixes)
	if err != nil || (networkMode == NetworkAccessAll && len(networkPrefixes) != 0) || (networkMode == NetworkAccessAllowlist && len(networkPrefixes) == 0) || (networkMode != NetworkAccessAll && networkMode != NetworkAccessAllowlist) {
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
	result, err := tx.Exec(ctx, `INSERT INTO service_api_keys (id, name, key_digest, key_prefix, expires_at, project_id, requests_per_minute, burst, model_access_mode, network_access_mode)
		SELECT $1, $2, $3, $4, $5, p.id, $7, $8, $9, $10 FROM projects p JOIN organizations o ON o.id=p.organization_id
		WHERE p.id=$6 AND p.status='active' AND o.status='active'`, record.ID, record.Name, record.Digest[:], record.Prefix, record.ExpiresAt, record.ProjectID, requestsPerMinute, burst, mode, networkMode)
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
	for _, prefix := range networkPrefixes {
		if _, err := tx.Exec(ctx, `INSERT INTO service_api_key_network_prefixes(api_key_id,prefix) VALUES($1,$2::cidr)`, record.ID, prefix.String()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (store *PostgresStore) FindActiveByDigest(ctx context.Context, digest [32]byte, now time.Time) (Principal, error) {
	var principal Principal
	var requestsPerMinute, burst *int64
	var permissionsJSON, prefixesJSON []byte
	err := store.pool.QueryRow(ctx, `SELECT k.id, p.id, o.id, k.requests_per_minute, k.burst, k.model_access_mode, k.network_access_mode,
		COALESCE((SELECT jsonb_agg(jsonb_build_object('protocol',mp.protocol,'operation',mp.operation,'model',mp.model) ORDER BY mp.protocol,mp.operation,mp.model) FROM service_api_key_model_permissions mp WHERE mp.api_key_id=k.id),'[]'::jsonb),
		COALESCE((SELECT jsonb_agg(np.prefix::text ORDER BY np.prefix::text) FROM service_api_key_network_prefixes np WHERE np.api_key_id=k.id),'[]'::jsonb)
		FROM service_api_keys k
		JOIN projects p ON p.id=k.project_id JOIN organizations o ON o.id=p.organization_id
		WHERE k.key_digest=$1 AND k.status='active' AND p.status='active' AND o.status='active'
		AND (k.expires_at IS NULL OR k.expires_at > $2)`, digest[:], now).Scan(&principal.APIKeyID, &principal.ProjectID, &principal.OrganizationID, &requestsPerMinute, &burst, &principal.ModelAccessMode, &principal.NetworkAccessMode, &permissionsJSON, &prefixesJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	if requestsPerMinute != nil && burst != nil {
		principal.RateLimit = RateLimitPolicy{RequestsPerMinute: *requestsPerMinute, Burst: *burst}
	}
	if err := json.Unmarshal(permissionsJSON, &principal.ModelPermissions); err != nil {
		return Principal{}, ErrPolicyInvalid
	}
	var encodedPrefixes []string
	if err := json.Unmarshal(prefixesJSON, &encodedPrefixes); err != nil {
		return Principal{}, ErrPolicyInvalid
	}
	for _, encoded := range encodedPrefixes {
		prefix, parseErr := netip.ParsePrefix(encoded)
		if parseErr != nil {
			return Principal{}, ErrPolicyInvalid
		}
		principal.NetworkPrefixes = append(principal.NetworkPrefixes, prefix)
	}
	permissions, policyErr := CanonicalModelPermissions(principal.ModelPermissions)
	if policyErr != nil || (principal.ModelAccessMode == ModelAccessAll && len(permissions) != 0) || (principal.ModelAccessMode == ModelAccessAllowlist && len(permissions) == 0) || (principal.ModelAccessMode != ModelAccessAll && principal.ModelAccessMode != ModelAccessAllowlist) {
		return Principal{}, ErrPolicyInvalid
	}
	principal.ModelPermissions = permissions
	prefixes, networkErr := CanonicalNetworkPrefixes(principal.NetworkPrefixes)
	if networkErr != nil || (principal.NetworkAccessMode == NetworkAccessAll && len(prefixes) != 0) || (principal.NetworkAccessMode == NetworkAccessAllowlist && len(prefixes) == 0) || (principal.NetworkAccessMode != NetworkAccessAll && principal.NetworkAccessMode != NetworkAccessAllowlist) {
		return Principal{}, ErrPolicyInvalid
	}
	principal.NetworkPrefixes = prefixes
	return principal, nil
}
