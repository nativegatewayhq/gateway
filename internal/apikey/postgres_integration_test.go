//go:build integration

package apikey

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/database"
)

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
	record, raw, err := Generate(bytes.NewReader(bytes.Repeat([]byte{9}, randomKeyBytes+16)), "integration", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	principal, err := NewService(store).Authenticate(ctx, raw)
	if err != nil || principal.APIKeyID != record.ID || principal.ProjectID != "project_legacy" || principal.OrganizationID != "org_legacy" {
		t.Fatalf("Authenticate()=%+v, %v", principal, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_api_keys SET status='disabled' WHERE id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store).Authenticate(ctx, raw); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled error=%v", err)
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
