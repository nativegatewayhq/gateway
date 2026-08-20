//go:build integration

package runwayassets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

func assetPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("runway_assets_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, _ := pgxpool.ParseConfig(url)
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func TestBindingIsTenantScopedExpiringAndAppendOnly(t *testing.T) {
	pool := assetPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_asset','Asset','asset'),('org_other','Other','other'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_asset','org_asset','Asset','asset'),('project_other','org_other','Other','other'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_asset','Asset',decode(repeat('11',32),'hex'),'ngw_asset','project_asset'),('key_other','Other',decode(repeat('22',32),'hex'),'ngw_other','project_other')`); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(pool)
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	owner := joboperation.Owner{OrganizationID: "org_asset", ProjectID: "project_asset", APIKeyID: "key_asset"}
	other := joboperation.Owner{OrganizationID: "org_other", ProjectID: "project_other", APIKeyID: "key_other"}
	channel := "channel_00000000000000000000000000000007"
	uri := "runway://uploads/private-capability"
	if err := store.Bind(ctx, owner, channel, uri, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Authorize(ctx, owner, channel, uri); err != nil {
		t.Fatal(err)
	}
	if err := store.Authorize(ctx, other, channel, uri); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross tenant error=%v", err)
	}
	if err := store.Bind(ctx, other, channel, uri, now.Add(24*time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("ownership conflict=%v", err)
	}
	store.now = func() time.Time { return now.Add(24 * time.Hour) }
	if err := store.Authorize(ctx, owner, channel, uri); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired error=%v", err)
	}
	var rawColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='runway_upload_assets' AND column_name IN ('uri','runway_uri','filename','upload_url','fields')`).Scan(&rawColumns); err != nil {
		t.Fatal(err)
	}
	if rawColumns != 0 {
		t.Fatal("secret-bearing upload columns exist")
	}
	if _, err := pool.Exec(ctx, `UPDATE runway_upload_assets SET expires_at=expires_at+interval '1 hour'`); err == nil {
		t.Fatal("asset mutation succeeded")
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runway_upload_asset_events`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events=%d err=%v", events, err)
	}
}
