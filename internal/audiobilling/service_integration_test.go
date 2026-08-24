//go:build integration

package audiobilling

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

func fixture(t *testing.T) (*Service, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("audio_billing_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgxpool.ParseConfig(url)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_audio','Audio','audio'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_audio','org_audio','Audio','audio'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_audio','Audio',decode(repeat('33',32),'hex'),'ngw_audio','project_audio')`)
	if err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err = wallet.Deposit(ctx, "org_audio", 10000, "audio-fixture"); err != nil {
		t.Fatal(err)
	}
	prices, _ := audiopricing.New(pool, 0)
	_, err = prices.Publish(ctx, audiopricing.Price{ChannelID: "channel_00000000000000000000000000000001", Model: "tts-1", CostPerMillion: 1_000_000, SalePerMillion: 2_000_000, EffectiveFrom: time.Now().Add(-time.Hour)}, "audio-price")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithControls(pool, prices, wallet, costquota.NewStore(pool), spendcap.NewStore(pool))
	if err != nil {
		t.Fatal(err)
	}
	return service, pool
}
func testRequest(key string) BeginRequest {
	return BeginRequest{RequestID: "request-" + key, OrganizationID: "org_audio", ProjectID: "project_audio", APIKeyID: "key_audio", Model: "tts-1", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: [32]byte{1}, Quantity: 5}
}

func TestConcurrentBeginAndCompleteAreExactlyOnce(t *testing.T) {
	s, pool := fixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan Charge, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); c, e := s.Begin(ctx, testRequest("same-key")); results <- c; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	var charge Charge
	success, pending := 0, 0
	for c := range results {
		if c.ID != "" {
			charge = c
		}
	}
	for err := range errs {
		if err == nil {
			success++
		} else if err == ErrPending {
			pending++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || pending != 1 {
		t.Fatalf("success=%d pending=%d", success, pending)
	}
	digest := [32]byte{9}
	evidence := StreamEvidence{Status: 200, Headers: map[string][]string{"Content-Type": {"audio/mpeg"}, "Set-Cookie": {"secret"}}, Bytes: 12, SHA256: digest}
	settled, err := s.Complete(ctx, charge.ID, evidence)
	if err != nil || settled.CapturedSale != 10 || settled.ActualCost == nil || *settled.ActualCost != 5 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if _, err = s.Complete(ctx, charge.ID, evidence); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Begin(ctx, testRequest("same-key"))
	if err != nil || replay.ID != charge.ID || replay.State != "CAPTURED" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var available, reserved int64
	if err = pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_audio'`).Scan(&available, &reserved); err != nil || available != 9990 || reserved != 0 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
	var captures int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "audio.speech:capture:"+charge.ID).Scan(&captures)
	if captures != 1 {
		t.Fatalf("captures=%d", captures)
	}
}

func TestReleaseAndReconciliationPreserveReservation(t *testing.T) {
	s, pool := fixture(t)
	ctx := context.Background()
	released, err := s.Begin(ctx, testRequest("release-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Release(ctx, released.ID, "provider_non_2xx"); err != nil {
		t.Fatal(err)
	}
	uncertain, err := s.Begin(ctx, testRequest("uncertain-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkReconciling(ctx, uncertain.ID, "stream_uncertain"); err != nil {
		t.Fatal(err)
	}
	var available, reserved int64
	if err = pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_audio'`).Scan(&available, &reserved); err != nil || available != 9990 || reserved != 10 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
}
