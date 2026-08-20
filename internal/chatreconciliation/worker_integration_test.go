//go:build integration

package chatreconciliation

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
)

func streamingWorkerFixture(t *testing.T) (*Worker, *chatbilling.Service, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("chat_reconcile_test_%d", time.Now().UnixNano())
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
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_stream','Stream','stream');INSERT INTO projects(id,organization_id,name,slug) VALUES('project_stream','org_stream','Stream project','stream');INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_stream','Stream key',decode(repeat('33',32),'hex'),'ngw_sk_stream','project_stream')`)
	if err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err = wallet.Deposit(ctx, "org_stream", 1000, "stream-fixture"); err != nil {
		t.Fatal(err)
	}
	prices, _ := chatpricing.New(pool, 0)
	if _, err = prices.Publish(ctx, chatpricing.Price{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4.1", EffectiveFrom: time.Now().Add(-time.Hour), Rates: chatpricing.Rates{InputCost: 1_000_000, InputSale: 2_000_000, CachedInputCost: 500_000, CachedInputSale: 1_000_000, OutputCost: 3_000_000, OutputSale: 4_000_000}}, "stream-worker-price"); err != nil {
		t.Fatal(err)
	}
	service, err := chatbilling.New(pool, prices, wallet, 4096)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := New(pool, service, "worker-test", time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	return worker, service, pool
}

func TestWorkerRetriesStreamingSettlementExactlyOnce(t *testing.T) {
	worker, service, pool := streamingWorkerFixture(t)
	ctx := context.Background()
	charge, err := service.Begin(ctx, chatbilling.BeginRequest{RequestID: "stream-reconcile", OrganizationID: "org_stream", ProjectID: "project_stream", APIKeyID: "key_stream", Model: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: 100, MaximumOutputTokens: 50, DeliveryMode: "stream"})
	if err != nil {
		t.Fatal(err)
	}
	usage := chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 4, CompletionTokens: 5}
	digest := [32]byte{7}
	if err = service.MarkStreamReconcilingUsage(ctx, charge.ID, usage, digest); err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOne(ctx)
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	var chargeState, taskState string
	var captureCount int
	if err = pool.QueryRow(ctx, `SELECT state FROM chat_request_charges WHERE id=$1`, charge.ID).Scan(&chargeState); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM chat_charge_reconciliations WHERE charge_id=$1`, charge.ID).Scan(&taskState); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "chat-stream-capture:"+charge.ID).Scan(&captureCount); err != nil {
		t.Fatal(err)
	}
	if chargeState != "CAPTURED" || taskState != "RESOLVED" || captureCount != 1 {
		t.Fatalf("charge=%s task=%s captures=%d", chargeState, taskState, captureCount)
	}
}

func TestWorkerRetriesResponsesStreamingSettlementExactlyOnce(t *testing.T) {
	worker, service, pool := streamingWorkerFixture(t)
	ctx := context.Background()
	prices, _ := chatpricing.New(pool, 0)
	if _, err := prices.Publish(ctx, chatpricing.Price{Operation: "responses.create", ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4.1", EffectiveFrom: time.Now().Add(-time.Hour), Rates: chatpricing.Rates{InputCost: 1_000_000, InputSale: 2_000_000, CachedInputCost: 500_000, CachedInputSale: 1_000_000, OutputCost: 3_000_000, OutputSale: 4_000_000}}, "responses-stream-worker-price"); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(ctx, chatbilling.BeginRequest{Operation: "responses.create", RequestID: "responses-stream-reconcile", OrganizationID: "org_stream", ProjectID: "project_stream", APIKeyID: "key_stream", Model: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: 100, MaximumOutputTokens: 50, DeliveryMode: "stream"})
	if err != nil {
		t.Fatal(err)
	}
	usage := chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 4, CompletionTokens: 5}
	digest := [32]byte{6}
	if err = service.MarkStreamReconcilingUsage(ctx, charge.ID, usage, digest); err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOne(ctx)
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	var chargeState, taskState, schema string
	var captures int
	if err = pool.QueryRow(ctx, `SELECT c.state,r.state,e.schema_version FROM chat_request_charges c JOIN chat_charge_reconciliations r ON r.charge_id=c.id JOIN chat_usage_evidence e ON e.charge_id=c.id WHERE c.id=$1`, charge.ID).Scan(&chargeState, &taskState, &schema); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "responses.create:stream:capture:"+charge.ID).Scan(&captures); err != nil {
		t.Fatal(err)
	}
	if chargeState != "CAPTURED" || taskState != "RESOLVED" || schema != "openai-responses-stream-usage-v1" || captures != 1 {
		t.Fatalf("charge=%s task=%s schema=%s captures=%d", chargeState, taskState, schema, captures)
	}
}
