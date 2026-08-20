//go:build integration

package chatbilling

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
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "chat.completions:capture:"+charge.ID).Scan(&captures)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM chat_usage_evidence WHERE charge_id=$1`, charge.ID).Scan(&evidence)
	if captures != 1 || evidence != 1 {
		t.Fatalf("captures=%d evidence=%d", captures, evidence)
	}
}

func TestResponsesOperationPriceSettlementAndEvidenceAreIsolated(t *testing.T) {
	service, pool := chatBillingFixture(t)
	ctx := context.Background()
	prices, _ := chatpricing.New(pool, 0)
	_, err := prices.Publish(ctx, chatpricing.Price{Operation: "responses.create", ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4.1", EffectiveFrom: time.Now().Add(-time.Hour), Rates: chatpricing.Rates{InputCost: 2_000_000, InputSale: 3_000_000, CachedInputCost: 1_000_000, CachedInputSale: 2_000_000, OutputCost: 4_000_000, OutputSale: 5_000_000}}, "responses-test-price")
	if err != nil {
		t.Fatal(err)
	}
	request := BeginRequest{Operation: "responses.create", RequestID: "responses-request", OrganizationID: "org_chat", ProjectID: "project_chat", APIKeyID: "key_chat", Model: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: 100, MaximumOutputTokens: 50, IdempotencyKey: "responses-idempotency", Fingerprint: [32]byte{3}}
	charge, err := service.Begin(ctx, request)
	if err != nil || charge.Operation != "responses.create" || charge.ReservedSale != 550 || charge.EstimatedCost != 400 {
		t.Fatalf("charge=%+v err=%v", charge, err)
	}
	snapshot := billing.ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`)}
	settled, err := service.CompleteUsage(ctx, charge.ID, chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 4, CompletionTokens: 5}, snapshot)
	if err != nil || settled.CapturedSale != 51 || settled.ActualCost == nil || *settled.ActualCost != 36 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	var schema, operation string
	var captures int
	if err = pool.QueryRow(ctx, `SELECT c.operation,e.schema_version FROM chat_request_charges c JOIN chat_usage_evidence e ON e.charge_id=c.id WHERE c.id=$1`, charge.ID).Scan(&operation, &schema); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "responses.create:capture:"+charge.ID).Scan(&captures)
	if operation != "responses.create" || schema != "openai-responses-usage-v1" || captures != 1 {
		t.Fatalf("operation=%s schema=%s captures=%d", operation, schema, captures)
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

func TestStreamingUsageSettlementDoesNotStoreTranscriptAndCannotReplay(t *testing.T) {
	service, pool := chatBillingFixture(t)
	ctx := context.Background()
	request := BeginRequest{RequestID: "stream-request", OrganizationID: "org_chat", ProjectID: "project_chat", APIKeyID: "key_chat", Model: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: 100, MaximumOutputTokens: 50, IdempotencyKey: "stream-idempotency", DeliveryMode: "stream"}
	request.Fingerprint = [32]byte{2}
	charge, err := service.Begin(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	digest := [32]byte{9}
	settled, err := service.CompleteStreamUsage(ctx, charge.ID, chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 4, CompletionTokens: 5}, digest)
	if err != nil || !settled.StreamCompleted || settled.CapturedSale != 36 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if _, found, err := service.Replay(ctx, request); !errors.Is(err, ErrConflict) || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	var snapshotVersion int
	var responseBody []byte
	var mode string
	var storedDigest []byte
	if err = pool.QueryRow(ctx, `SELECT c.response_snapshot_version,c.response_body,e.delivery_mode,e.terminal_event_sha256 FROM chat_request_charges c JOIN chat_usage_evidence e ON e.charge_id=c.id WHERE c.id=$1`, charge.ID).Scan(&snapshotVersion, &responseBody, &mode, &storedDigest); err != nil {
		t.Fatal(err)
	}
	if snapshotVersion != 0 || responseBody != nil || mode != "stream" || !bytes.Equal(storedDigest, digest[:]) {
		t.Fatalf("snapshot=%d body=%v mode=%s digest=%x", snapshotVersion, responseBody, mode, storedDigest)
	}
}
