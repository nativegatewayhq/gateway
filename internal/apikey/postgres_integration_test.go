//go:build integration

package apikey

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/database"
)

func TestPostgresStorePersistsNetworkPolicySnapshotAndCascade(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	pool, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	record, raw, err := GenerateForProjectWithAccess(bytes.NewReader(bytes.Repeat([]byte{7}, randomKeyBytes+16)), "network integration", "project_legacy", nil, RateLimitPolicy{}, nil, []netip.Prefix{netip.MustParsePrefix("192.0.2.9/24")})
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), `DELETE FROM service_api_keys WHERE id=$1`, record.ID)
	principal, err := NewService(store).Authenticate(context.Background(), raw)
	if err != nil || !principal.AuthorizeNetwork(netip.MustParseAddr("192.0.2.10")) || principal.AuthorizeNetwork(netip.MustParseAddr("198.51.100.10")) {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM service_api_keys WHERE id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM service_api_key_network_prefixes WHERE api_key_id=$1`, record.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("prefix rows=%d err=%v", count, err)
	}
}

func TestPostgresStoreLifecycle(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	record, raw, err := GenerateForProjectWithPolicies(bytes.NewReader(bytes.Repeat([]byte{9}, randomKeyBytes+16)), "integration", "project_legacy", nil, RateLimitPolicy{RequestsPerMinute: 60, Burst: 5}, []ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	principal, err := NewService(store).Authenticate(ctx, raw)
	if err != nil || principal.APIKeyID != record.ID || principal.ProjectID != "project_legacy" || principal.OrganizationID != "org_legacy" || principal.RateLimit.RequestsPerMinute != 60 || principal.RateLimit.Burst != 5 || !principal.AuthorizeModel("openai", "image.generate", "gpt-image-1") || principal.AuthorizeModel("openai", "image.edit", "gpt-image-1") {
		t.Fatalf("Authenticate()=%+v, %v", principal, err)
	}
	principal.ModelPermissions[0].Model = "mutated"
	fresh, err := NewService(store).Authenticate(ctx, raw)
	if err != nil || !fresh.AuthorizeModel("openai", "image.generate", "gpt-image-1") {
		t.Fatalf("fresh snapshot=%+v error=%v", fresh, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_api_keys SET status='disabled' WHERE id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store).Authenticate(ctx, raw); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled error=%v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	var permissionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_api_key_model_permissions WHERE api_key_id=$1`, record.ID).Scan(&permissionCount); err != nil || permissionCount != 0 {
		t.Fatalf("permission count=%d error=%v", permissionCount, err)
	}
	expired := time.Now().Add(-time.Minute)
	record2, raw2, err := Generate(bytes.NewReader(bytes.Repeat([]byte{8}, randomKeyBytes+16)), "expired", &expired)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record2.ID)
	if err := store.Create(ctx, record2); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store).Authenticate(ctx, raw2); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired error=%v", err)
	}
}

func TestPostgresStoreEnforcesTenantStatusAndOwnership(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_tenanttest','Tenant test','tenant-test') ON CONFLICT(id) DO UPDATE SET status='active';
		INSERT INTO projects(id,organization_id,name,slug) VALUES('project_tenanttest','org_tenanttest','Tenant project','tenant-project') ON CONFLICT(id) DO UPDATE SET status='active'`)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE project_id='project_tenanttest'; DELETE FROM projects WHERE id='project_tenanttest'; DELETE FROM organizations WHERE id='org_tenanttest'`)
	record, raw, err := GenerateForProject(bytes.NewReader(bytes.Repeat([]byte{7}, randomKeyBytes+16)), "tenant", "project_tenanttest", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	principal, err := NewService(store).Authenticate(ctx, raw)
	if err != nil || principal.ProjectID != "project_tenanttest" || principal.OrganizationID != "org_tenanttest" {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE projects SET status='disabled' WHERE id='project_tenanttest'`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store).Authenticate(ctx, raw); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled project=%v", err)
	}
	_, _ = pool.Exec(ctx, `UPDATE projects SET status='active' WHERE id='project_tenanttest'; UPDATE organizations SET status='disabled' WHERE id='org_tenanttest'`)
	if _, err := NewService(store).Authenticate(ctx, raw); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled org=%v", err)
	}
	missing, _, _ := GenerateForProject(bytes.NewReader(bytes.Repeat([]byte{6}, randomKeyBytes+16)), "missing", "project_missing", nil)
	if err := store.Create(ctx, missing); !errors.Is(err, ErrProjectUnavailable) {
		t.Fatalf("missing project=%v", err)
	}
}

func TestPostgresStoreConnectionLossBecomesUnavailable(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	pool.Close()

	_, err = NewService(store).Authenticate(ctx, "ngw_sk_connection_loss")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("connection loss error = %v", err)
	}
}

func TestPostgresStoreModelPolicyIsAtomicAndCorruptionFailsClosed(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	record, _, err := Generate(bytes.NewReader(bytes.Repeat([]byte{5}, randomKeyBytes+16)), "atomic-invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	record.ModelAccessMode = ModelAccessAllowlist
	record.ModelPermissions = []ModelPermission{{Protocol: "openai", Operation: "chat", Model: "model"}}
	if err := store.Create(ctx, record); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("create error=%v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_api_keys WHERE id=$1`, record.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial key count=%d error=%v", count, err)
	}

	corrupt, raw, err := Generate(bytes.NewReader(bytes.Repeat([]byte{4}, randomKeyBytes+16)), "corrupt", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, corrupt.ID)
	if _, err := pool.Exec(ctx, `INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id,model_access_mode) VALUES($1,$2,$3,$4,'project_legacy','allowlist')`, corrupt.ID, corrupt.Name, corrupt.Digest[:], corrupt.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store).Authenticate(ctx, raw); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt policy error=%v", err)
	}
}
