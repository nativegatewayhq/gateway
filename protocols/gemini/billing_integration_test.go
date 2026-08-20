//go:build integration

package gemini

import (
	"context"
	"crypto/rand"
	"errors"
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
	"github.com/nativegatewayhq/gateway/internal/reconciliation"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/google"
)

func TestBillableGeminiPostgresLifecycleAndReplay(t *testing.T) {
	pool := isolatedGeminiPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_gemini_billing','Gemini billing','gemini-billing'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_gemini_billing','org_gemini_billing','Gemini billing','gemini-billing')`); err != nil {
		t.Fatal(err)
	}
	record, raw, err := apikey.GenerateForProject(rand.Reader, "gemini billable", "project_gemini_billing", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := apikey.NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err := wallet.Deposit(ctx, "org_gemini_billing", 500, "gemini-deposit"); err != nil {
		t.Fatal(err)
	}
	estimator, _ := pricing.NewService(pool, 0)
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: "channel_00000000000000000000000000000003", Protocol: "gemini", Operation: "image.generate", Model: "gemini-image", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "gemini-price"); err != nil {
		t.Fatal(err)
	}
	dimensionPrice, err := estimator.Publish(ctx, pricing.Price{ChannelID: "channel_00000000000000000000000000000003", Protocol: "gemini", Operation: "image.generate", Model: "gemini-image", Size: "16:9", Quality: "2K", UnitCost: 90, UnitSale: 150, EffectiveFrom: time.Now().Add(-time.Hour)}, "gemini-dimension-price")
	if err != nil {
		t.Fatal(err)
	}
	chargeService, _ := chargebilling.NewService(pool, estimator, wallet)
	if _, err := chargeService.Begin(ctx, chargebilling.BeginRequest{RequestID: "invalid-protocol", OrganizationID: "org_gemini_billing", ProjectID: "project_gemini_billing", Protocol: "anthropic", Operation: "image.generate", Model: "gemini-image", ChannelID: "channel_00000000000000000000000000000003", Quantity: 1}); !errors.Is(err, chargebilling.ErrInvalidRequest) {
		t.Fatalf("invalid protocol error=%v", err)
	}
	calls := 0
	executorFunction := executorFunc(func(_ context.Context, request google.GenerateContentRequest) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(request.Body)
		switch {
		case strings.Contains(string(body), `"timeout":true`):
			return nil, google.ErrTimeout
		case strings.Contains(string(body), `"lost":true`):
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(&failingReader{})}, nil
		case strings.Contains(string(body), `"fail":true`):
			return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"3"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`))}, nil
		default:
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Goog-Request-Id": {"google-native-id"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"image":true}]}`))}, nil
		}
	})
	handler := NewBillableHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), imageoperation.DefaultRegistry(), executorFunction, 4096, chargeService)

	success := geminiBillableRequest(handler, raw, "success-1", "success-key", `{"contents":[]}`)
	replay := geminiBillableRequest(handler, raw, "success-2", "success-key", `{"contents":[]}`)
	if success.Code != 200 || replay.Body.String() != success.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("X-Goog-Request-Id") != "google-native-id" || calls != 1 {
		t.Fatalf("success=%d replay=%d headers=%v calls=%d", success.Code, replay.Code, replay.Header(), calls)
	}
	dimension := geminiBillableRequest(handler, raw, "dimension-1", "dimension-key", `{"contents":[],"generationConfig":{"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}}}`)
	if dimension.Code != 200 || calls != 2 {
		t.Fatalf("dimension=%d calls=%d", dimension.Code, calls)
	}
	var selectedPrice string
	if err := pool.QueryRow(ctx, `SELECT price_id FROM image_request_charges WHERE organization_id='org_gemini_billing' AND request_id='dimension-1'`).Scan(&selectedPrice); err != nil || selectedPrice != dimensionPrice.ID {
		t.Fatalf("dimension price=%s want=%s error=%v", selectedPrice, dimensionPrice.ID, err)
	}
	failure := geminiBillableRequest(handler, raw, "failure-1", "failure-key", `{"contents":[],"fail":true}`)
	failureReplay := geminiBillableRequest(handler, raw, "failure-2", "failure-key", `{"contents":[],"fail":true}`)
	if failure.Code != 429 || failureReplay.Body.String() != failure.Body.String() || failureReplay.Header().Get("Idempotency-Replayed") != "true" || calls != 3 {
		t.Fatalf("failure=%d/%d calls=%d", failure.Code, failureReplay.Code, calls)
	}
	timeout := geminiBillableRequest(handler, raw, "timeout-1", "timeout-key", `{"contents":[],"timeout":true}`)
	timeoutRetry := geminiBillableRequest(handler, raw, "timeout-2", "timeout-key", `{"contents":[],"timeout":true}`)
	if timeout.Code != 503 || timeoutRetry.Code != 409 || calls != 4 {
		t.Fatalf("timeout=%d/%d calls=%d", timeout.Code, timeoutRetry.Code, calls)
	}
	lost := geminiBillableRequest(handler, raw, "lost-1", "lost-key", `{"contents":[],"lost":true}`)
	if lost.Code != 503 || calls != 5 {
		t.Fatalf("lost=%d calls=%d", lost.Code, calls)
	}
	worker, err := reconciliation.New(pool, chargeService, reconciliation.Config{Interval: time.Second, Lease: time.Second, BaseBackoff: time.Second, MaxBackoff: time.Minute, BatchSize: 10, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil || result.Resolved != 1 || result.Retried != 1 {
		t.Fatalf("reconciliation=%+v error=%v", result, err)
	}
	lostReplay := geminiBillableRequest(handler, raw, "lost-2", "lost-key", `{"contents":[],"lost":true}`)
	if lostReplay.Code != 502 || lostReplay.Header().Get("Idempotency-Replayed") != "true" || calls != 5 {
		t.Fatalf("lost replay=%d headers=%v calls=%d", lostReplay.Code, lostReplay.Header(), calls)
	}
	var available, reserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_gemini_billing'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 50 || reserved != 100 {
		t.Fatalf("wallet=%d/%d", available, reserved)
	}
	if _, err := pool.Exec(ctx, `UPDATE image_request_charges SET protocol='openai' WHERE organization_id='org_gemini_billing' AND protocol='gemini'`); err == nil {
		t.Fatal("charge protocol identity mutation succeeded")
	}
}

type executorFunc func(context.Context, google.GenerateContentRequest) (*http.Response, error)

func (function executorFunc) GenerateContent(ctx context.Context, request google.GenerateContentRequest) (*http.Response, error) {
	return function(ctx, request)
}

func geminiBillableRequest(handler http.Handler, key, requestID, idempotencyKey, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-image:generateContent?key="+key+"&safe=value", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set(requestid.HeaderName, requestID)
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	return response
}

func isolatedGeminiPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("gemini_billing_test_%d", time.Now().UnixNano())
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
