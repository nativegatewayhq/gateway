//go:build integration

package chatbilling

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

func chatBillingFixture(t *testing.T) (*Service, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("chat_billing_test_%d", time.Now().UnixNano())
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
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_chat','Chat','chat'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_chat','org_chat','Chat project','chat'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_chat','Chat key',decode(repeat('22',32),'hex'),'ngw_sk_chat','project_chat')`)
	if err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err = wallet.Deposit(ctx, "org_chat", 1000, "chat-fixture"); err != nil {
		t.Fatal(err)
	}
	prices, _ := chatpricing.New(pool, 0)
	_, err = prices.Publish(ctx, chatpricing.Price{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4.1", EffectiveFrom: time.Now().Add(-time.Hour), Rates: chatpricing.Rates{InputCost: 1_000_000, InputSale: 2_000_000, CachedInputCost: 500_000, CachedInputSale: 1_000_000, OutputCost: 3_000_000, OutputSale: 4_000_000}}, "chat-test-price")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithControls(pool, prices, wallet, costquota.NewStore(pool), spendcap.NewStore(pool), 4096)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool
}

func TestReserveUsageSettlementAndReplayAreExactlyOnce(t *testing.T) {
	service, pool := chatBillingFixture(t)
	ctx := context.Background()
	request := BeginRequest{RequestID: "chat-request", OrganizationID: "org_chat", ProjectID: "project_chat", APIKeyID: "key_chat", Model: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: 100, MaximumOutputTokens: 50, IdempotencyKey: "chat-idempotency"}
	request.Fingerprint = [32]byte{1}
	charge, err := service.Begin(ctx, request)
	if err != nil || charge.ReservedSale != 400 || charge.EstimatedCost != 250 {
		t.Fatalf("charge=%+v err=%v", charge, err)
	}
	snapshot := billing.ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`)}
	settled, err := service.CompleteUsage(ctx, charge.ID, chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 4, CompletionTokens: 5}, snapshot)
	if err != nil || settled.CapturedSale != 36 || settled.ActualCost == nil || *settled.ActualCost != 23 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	replay, found, err := service.Replay(ctx, request)
	if err != nil || !found || !replay.Replay || replay.CapturedSale != 36 {
		t.Fatalf("replay=%+v found=%v err=%v", replay, found, err)
	}
	var available, reserved int64
	if err = pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_chat'`).Scan(&available, &reserved); err != nil || available != 964 || reserved != 0 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
	var captures, evidence int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "chat-capture:"+charge.ID).Scan(&captures)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM chat_usage_evidence WHERE charge_id=$1`, charge.ID).Scan(&evidence)
	if captures != 1 || evidence != 1 {
		t.Fatalf("captures=%d evidence=%d", captures, evidence)
	}
}

func TestUnknownOutcomeKeepsReservation(t *testing.T) {
	service, pool := chatBillingFixture(t)
	charge, err := service.Begin(context.Background(), BeginRequest{RequestID: "unknown", OrganizationID: "org_chat", ProjectID: "project_chat", APIKeyID: "key_chat", Model: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: 100, MaximumOutputTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkReconciling(context.Background(), charge.ID, "executor_timeout", nil); err != nil {
		t.Fatal(err)
	}
	var state string
	var reserved int64
	_ = pool.QueryRow(context.Background(), `SELECT state FROM chat_request_charges WHERE id=$1`, charge.ID).Scan(&state)
	_ = pool.QueryRow(context.Background(), `SELECT reserved FROM organization_wallets WHERE organization_id='org_chat'`).Scan(&reserved)
	if state != "RECONCILING" || reserved != 400 {
		t.Fatalf("state=%s reserved=%d", state, reserved)
	}
}
