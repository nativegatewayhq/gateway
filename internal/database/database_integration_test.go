//go:build integration

package database

import (
	"context"
	"os"
	"sync"
	"testing"
)

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
		SELECT required.name FROM (VALUES ('users'),('organizations'),('organization_memberships'),('projects'),('service_api_keys'),('organization_wallets'),('wallet_reservations'),('wallet_operations'),('ledger_entries')) required(name)
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
