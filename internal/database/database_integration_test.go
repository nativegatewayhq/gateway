//go:build integration

package database

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestAllMigrationsApplyToEmptySchema(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(714821306)`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(714821306)`)
	_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS gateway_fresh_migration_test CASCADE`)
	if _, err := conn.Exec(ctx, `CREATE SCHEMA gateway_fresh_migration_test; SET search_path TO gateway_fresh_migration_test`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `SET search_path TO public; DROP SCHEMA IF EXISTS gateway_fresh_migration_test CASCADE`)
	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	for _, name := range entries {
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
	}
}

func TestMigrateIsRepeatable(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wait.Add(1)
		go func() { defer wait.Done(); errors <- Migrate(ctx, pool) }()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent Migrate()=%v", err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (
		SELECT required.name FROM (VALUES ('users'),('organizations'),('organization_memberships'),('projects'),('service_api_keys'),('service_api_key_model_permissions'),('organization_wallets'),('wallet_reservations'),('wallet_operations'),('ledger_entries'),('provider_channels'),('provider_prices'),('price_publications'),('image_request_charges'),('image_charge_reconciliations'),('image_assets'),('async_jobs'),('async_job_provider_attempts'),('async_job_events'),('async_job_webhook_bindings'),('async_job_webhook_deliveries'),('async_job_usage_evidence')) required(name)
		WHERE NOT EXISTS (SELECT 1 FROM information_schema.tables t WHERE t.table_schema='public' AND t.table_name=required.name)
	)`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("tenant schema was not created")
	}
	var nullable string
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema='public' AND table_name='service_api_keys' AND column_name='project_id'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatalf("service_api_keys.project_id nullable=%s", nullable)
	}
	var rateLimitConstraint string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='service_api_keys'::regclass AND conname='service_api_keys_rate_limit_policy_check'`).Scan(&rateLimitConstraint); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rateLimitConstraint, "requests_per_minute") || !strings.Contains(rateLimitConstraint, "burst") {
		t.Fatalf("rate limit constraint=%s", rateLimitConstraint)
	}
	var accessDefault string
	if err := pool.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_schema='public' AND table_name='service_api_keys' AND column_name='model_access_mode'`).Scan(&accessDefault); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(accessDefault, "all") {
		t.Fatalf("model access default=%s", accessDefault)
	}
	var modelPermissionConstraint string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='service_api_key_model_permissions'::regclass AND conname='service_api_key_model_permissions_operation_check'`).Scan(&modelPermissionConstraint); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(modelPermissionConstraint, "audio.translation") || !strings.Contains(modelPermissionConstraint, "audio.transcription") {
		t.Fatalf("model permission constraint=%s", modelPermissionConstraint)
	}
	var protocolConstraint string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='image_request_charges'::regclass AND conname='image_request_charges_protocol_check'`).Scan(&protocolConstraint); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(protocolConstraint, "openai") || !strings.Contains(protocolConstraint, "gemini") {
		t.Fatalf("protocol constraint=%s", protocolConstraint)
	}
}

func TestTenantMigrationBackfillsExistingKeyWithoutChangingCredential(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS tenant_migration_test CASCADE`)
	if _, err := conn.Exec(ctx, `CREATE SCHEMA tenant_migration_test; SET search_path TO tenant_migration_test`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `SET search_path TO public; DROP SCHEMA IF EXISTS tenant_migration_test CASCADE`)
	first, _ := migrationFiles.ReadFile("migrations/000001_service_api_keys.sql")
	second, _ := migrationFiles.ReadFile("migrations/000002_tenant_ownership.sql")
	if _, err := conn.Exec(ctx, string(first)); err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	if _, err := conn.Exec(ctx, `INSERT INTO service_api_keys(id,name,key_digest,key_prefix) VALUES('key_existing','existing',$1,'ngw_sk_existing')`, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(second)); err != nil {
		t.Fatal(err)
	}
	var projectID, prefix string
	var sameDigest bool
	if err := conn.QueryRow(ctx, `SELECT project_id,key_prefix,key_digest=$1 FROM service_api_keys WHERE id='key_existing'`, digest).Scan(&projectID, &prefix, &sameDigest); err != nil {
		t.Fatal(err)
	}
	if projectID != "project_legacy" || prefix != "ngw_sk_existing" || !sameDigest {
		t.Fatalf("backfill=%q %q %v", projectID, prefix, sameDigest)
	}
}

func TestModelPermissionMigrationDefaultsExistingKeyToAll(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS model_permission_migration_test CASCADE`)
	if _, err := conn.Exec(ctx, `CREATE SCHEMA model_permission_migration_test; SET search_path TO model_permission_migration_test`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `SET search_path TO public; DROP SCHEMA IF EXISTS model_permission_migration_test CASCADE`)
	first, _ := migrationFiles.ReadFile("migrations/000001_service_api_keys.sql")
	second, _ := migrationFiles.ReadFile("migrations/000002_tenant_ownership.sql")
	tenth, _ := migrationFiles.ReadFile("migrations/000010_api_key_model_permissions.sql")
	if _, err := conn.Exec(ctx, string(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_existing_model','existing',decode(repeat('00',32),'hex'),'ngw_sk_existing','project_legacy')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(tenth)); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := conn.QueryRow(ctx, `SELECT model_access_mode FROM service_api_keys WHERE id='key_existing_model'`).Scan(&mode); err != nil || mode != "all" {
		t.Fatalf("mode=%s error=%v", mode, err)
	}
}
