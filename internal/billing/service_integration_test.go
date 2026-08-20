//go:build integration

package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
)

const openAIChannel = "channel_00000000000000000000000000000001"

func billingPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("billing_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func billingFixture(t *testing.T, balance int64) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool := billingPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_billing','Billing','billing'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_billing','org_billing','Billing project','billing')`); err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err := wallet.Deposit(ctx, "org_billing", balance, "fixture-deposit"); err != nil {
		t.Fatal(err)
	}
	estimator, err := pricing.NewService(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: openAIChannel, Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "billing-price"); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(pool, estimator, wallet)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool
}

func billableRequest(requestID string) BeginRequest {
	return BeginRequest{RequestID: requestID, OrganizationID: "org_billing", ProjectID: "project_billing", Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", ChannelID: openAIChannel, Quantity: 2}
}

func TestBeginAndCaptureAreAtomicAndIdempotent(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	ctx := context.Background()
	charge, err := service.Begin(ctx, billableRequest("request-capture"))
	if err != nil {
		t.Fatal(err)
	}
	if charge.State != "RESERVED" || charge.EstimatedCost != 120 || charge.ReservedSale != 200 {
		t.Fatalf("charge=%+v", charge)
	}
	if _, err := service.Begin(ctx, billableRequest("request-capture")); !errors.Is(err, ErrRequestPending) {
		t.Fatalf("retry error=%v", err)
	}
	captured, err := service.Capture(ctx, charge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if captured.State != "CAPTURED" || captured.ActualCost == nil || *captured.ActualCost != 120 || captured.CapturedSale != 200 {
		t.Fatalf("captured=%+v", captured)
	}
	retry, err := service.Capture(ctx, charge.ID)
	if err != nil || retry.State != "CAPTURED" {
		t.Fatalf("capture retry=%+v error=%v", retry, err)
	}
	assertWalletAndCharge(t, pool, 800, 0, 1, "CAPTURED")
}

func TestReleaseAndBeginFailuresNeverCharge(t *testing.T) {
	service, pool := billingFixture(t, 250)
	ctx := context.Background()
	charge, err := service.Begin(ctx, billableRequest("request-release"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(ctx, charge.ID); err != nil {
		t.Fatal(err)
	}
	assertWalletAndCharge(t, pool, 250, 0, 1, "RELEASED")
	tooLarge := billableRequest("request-insufficient")
	tooLarge.Quantity = 3
	if _, err := service.Begin(ctx, tooLarge); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("insufficient error=%v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image_request_charges`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("charge count=%d", count)
	}
}

func TestRequestConflictAndReconcilingSettlement(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	ctx := context.Background()
	charge, err := service.Begin(ctx, billableRequest("request-reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	conflict := billableRequest("request-reconcile")
	conflict.Quality = "high"
	if _, err := service.Begin(ctx, conflict); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	if err := service.MarkReconciling(ctx, charge.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(ctx, charge.ID); err != nil {
		t.Fatal(err)
	}
	assertWalletAndCharge(t, pool, 1_000, 0, 1, "RELEASED")
	if _, err := pool.Exec(ctx, `UPDATE image_request_charges SET reserved_sale=1 WHERE id=$1`, charge.ID); err == nil {
		t.Fatal("immutable charge estimate update succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM image_request_charges WHERE id=$1`, charge.ID); err == nil {
		t.Fatal("charge delete succeeded")
	}
}

func TestBeginRollsBackWalletWhenChargeCreationFails(t *testing.T) {
	service, pool := billingFixture(t, 500)
	service.entropy = bytes.NewReader(nil)
	if _, err := service.Begin(context.Background(), billableRequest("request-rollback")); err == nil {
		t.Fatal("begin succeeded without charge ID entropy")
	}
	assertWalletAndCharge(t, pool, 500, 0, 0, "RESERVED")
}

func assertWalletAndCharge(t *testing.T, pool *pgxpool.Pool, available, reserved int64, charges int, state string) {
	t.Helper()
	ctx := context.Background()
	var gotAvailable, gotReserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_billing'`).Scan(&gotAvailable, &gotReserved); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image_request_charges WHERE state=$1`, state).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if gotAvailable != available || gotReserved != reserved || count != charges {
		t.Fatalf("wallet=%d/%d charges=%d", gotAvailable, gotReserved, count)
	}
	var ledgerAvailable, ledgerReserved int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(delta_available),0),coalesce(sum(delta_reserved),0) FROM ledger_entries WHERE organization_id='org_billing'`).Scan(&ledgerAvailable, &ledgerReserved); err != nil {
		t.Fatal(err)
	}
	if ledgerAvailable != gotAvailable || ledgerReserved != gotReserved {
		t.Fatalf("ledger=%d/%d wallet=%d/%d", ledgerAvailable, ledgerReserved, gotAvailable, gotReserved)
	}
}
