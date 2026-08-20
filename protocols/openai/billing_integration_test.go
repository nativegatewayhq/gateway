//go:build integration

package openai

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/apikey"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

func TestBillableImagesPostgresLifecyclePreservesNativeResponses(t *testing.T) {
	pool := isolatedOpenAIPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_protocol_billing','Protocol billing','protocol-billing'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_protocol_billing','org_protocol_billing','Protocol billing','protocol-billing')`); err != nil {
		t.Fatal(err)
	}
	record, raw, err := apikey.GenerateForProject(rand.Reader, "billable protocol", "project_protocol_billing", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := apikey.NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err := wallet.Deposit(ctx, "org_protocol_billing", 500, "fixture-deposit"); err != nil {
		t.Fatal(err)
	}
	estimator, _ := pricing.NewService(pool, 0)
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: "channel_00000000000000000000000000000001", Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "protocol-billing-price"); err != nil {
		t.Fatal(err)
	}
	chargeService, _ := chargebilling.NewServiceWithControls(pool, estimator, wallet, nil, spendcap.NewStore(pool), 32*1024*1024)
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: "channel_00000000000000000000000000000001", Protocol: "openai", Operation: "image.generate", Model: "logical-image", UnitCost: 40, UnitSale: 50, EffectiveFrom: time.Now().Add(-time.Hour)}, "routing-first-price"); err != nil {
		t.Fatal(err)
	}
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: "channel_00000000000000000000000000000002", Protocol: "openai", Operation: "image.generate", Model: "logical-image", UnitCost: 30, UnitSale: 50, EffectiveFrom: time.Now().Add(-time.Hour)}, "routing-price"); err != nil {
		t.Fatal(err)
	}
	spendPolicy, err := spendcap.NewStore(pool).SetPolicy(ctx, spendcap.PolicyInput{ChannelID: "channel_00000000000000000000000000000001", Period: spendcap.Day, Limit: 30, Actor: "integration", Reason: "fallback"})
	if err != nil {
		t.Fatal(err)
	}
	routingRegistry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	routingCalls := 0
	routingHandler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), routingRegistry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("candidate without an exact logical-model price reached provider")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			routingCalls++
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"model":"xai-provider-model"}` {
				t.Fatalf("routed body=%s", body)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"routed":true}`))}, nil
		}),
	}, 2048, chargeService)
	routedRequest := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"logical-image"}`))
	routedRequest.Header.Set("Content-Type", "application/json")
	routedRequest.Header.Set("Authorization", "Bearer "+raw)
	routedRequest.Header.Set(requestid.HeaderName, "priority-routing")
	routedResponse := httptest.NewRecorder()
	requestid.Middleware(routingHandler).ServeHTTP(routedResponse, routedRequest)
	if routedResponse.Code != 200 || routingCalls != 1 {
		t.Fatalf("routed response=%d calls=%d", routedResponse.Code, routingCalls)
	}
	var routedChannel string
	if err := pool.QueryRow(ctx, `SELECT channel_id FROM image_request_charges WHERE organization_id='org_protocol_billing' AND request_id='priority-routing'`).Scan(&routedChannel); err != nil || routedChannel != "channel_00000000000000000000000000000002" {
		t.Fatalf("routed channel=%s error=%v", routedChannel, err)
	}
	var spendEffects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_channel_spend_buckets`).Scan(&spendEffects); err != nil || spendEffects != 0 {
		t.Fatalf("spend effects=%d err=%v", spendEffects, err)
	}
	if err := spendcap.NewStore(pool).DisablePolicy(ctx, spendPolicy.ID, "integration", "routing test complete"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	executor := executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), `"executor":true`) {
			return nil, openaiimages.ErrUpstream
		}
		if strings.Contains(string(body), `"fail":true`) {
			return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"native limit"}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":[{"url":"native-success"}]}`))}, nil
	})
	handler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), imageoperation.DefaultRegistry(), map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executor}, 2048, chargeService)

	success := billableProtocolRequest(handler, raw, "billable-success", `{"model":"gpt-image-1","n":2}`)
	if success.Code != 200 || success.Body.String() != `{"data":[{"url":"native-success"}]}` {
		t.Fatalf("success=%d %s", success.Code, success.Body.String())
	}
	successReplay := billableProtocolRequest(handler, raw, "billable-success-retry", `{"model":"gpt-image-1","n":2}`)
	if successReplay.Code != 200 || successReplay.Body.String() != success.Body.String() || successReplay.Header().Get("Idempotency-Replayed") != "true" || calls != 1 {
		t.Fatalf("success replay=%d %s headers=%v calls=%d", successReplay.Code, successReplay.Body.String(), successReplay.Header(), calls)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_api_keys SET model_access_mode='allowlist' WHERE id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO service_api_key_model_permissions(api_key_id,protocol,operation,model) VALUES($1,'openai','image.edit','gpt-image-1')`, record.ID); err != nil {
		t.Fatal(err)
	}
	deniedReplay := billableProtocolRequest(handler, raw, "billable-success-denied", `{"model":"gpt-image-1","n":2}`)
	if deniedReplay.Code != 403 || deniedReplay.Header().Get("Idempotency-Replayed") != "" || calls != 1 {
		t.Fatalf("denied replay=%d headers=%v calls=%d", deniedReplay.Code, deniedReplay.Header(), calls)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM service_api_key_model_permissions WHERE api_key_id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE service_api_keys SET model_access_mode='all' WHERE id=$1`, record.ID); err != nil {
		t.Fatal(err)
	}
	failure := billableProtocolRequest(handler, raw, "billable-failure", `{"model":"gpt-image-1","fail":true}`)
	failureReplay := billableProtocolRequest(handler, raw, "billable-failure-retry", `{"model":"gpt-image-1","fail":true}`)
	if failure.Code != 429 || failure.Body.String() != `{"error":{"message":"native limit"}}` || failureReplay.Body.String() != failure.Body.String() || failureReplay.Header().Get("Idempotency-Replayed") != "true" || calls != 2 {
		t.Fatalf("failure=%d %s calls=%d", failure.Code, failure.Body.String(), calls)
	}
	executorFailure := billableProtocolRequest(handler, raw, "billable-executor", `{"model":"gpt-image-1","executor":true}`)
	executorReplay := billableProtocolRequest(handler, raw, "billable-executor-retry", `{"model":"gpt-image-1","executor":true}`)
	if executorFailure.Code != 503 || executorReplay.Code != 409 || executorReplay.Header().Get("Idempotency-Replayed") != "" || calls != 3 {
		t.Fatalf("executor replay=%d/%d calls=%d", executorFailure.Code, executorReplay.Code, calls)
	}
	var available, reserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_protocol_billing'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	var captured, released, reconciling int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='CAPTURED'),count(*) FILTER (WHERE state='RELEASED'),count(*) FILTER (WHERE state='RECONCILING') FROM image_request_charges WHERE organization_id='org_protocol_billing'`).Scan(&captured, &released, &reconciling); err != nil {
		t.Fatal(err)
	}
	if available != 150 || reserved != 100 || captured != 2 || released != 1 || reconciling != 1 {
		t.Fatalf("wallet=%d/%d charges=%d/%d/%d", available, reserved, captured, released, reconciling)
	}
	if _, err := pool.Exec(ctx, `UPDATE projects SET status='disabled' WHERE id='project_protocol_billing'`); err != nil {
		t.Fatal(err)
	}
	disabledReplay := billableProtocolRequest(handler, raw, "billable-success-disabled", `{"model":"gpt-image-1","n":2}`)
	if disabledReplay.Code != 401 || calls != 3 {
		t.Fatalf("disabled replay=%d calls=%d", disabledReplay.Code, calls)
	}
}

func isolatedOpenAIPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("openai_billing_test_%d", time.Now().UnixNano())
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

func billableProtocolRequest(handler http.Handler, key, id, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set(requestid.HeaderName, id)
	if strings.HasPrefix(id, "billable-success") {
		request.Header.Set("Idempotency-Key", "billable-success-key")
	}
	if strings.HasPrefix(id, "billable-failure") {
		request.Header.Set("Idempotency-Key", "billable-failure-key")
	}
	if strings.HasPrefix(id, "billable-executor") {
		request.Header.Set("Idempotency-Key", "billable-executor-key")
	}
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	return response
}
