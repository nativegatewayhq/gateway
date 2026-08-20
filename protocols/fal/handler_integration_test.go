//go:build integration

package fal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	providerfal "github.com/nativegatewayhq/gateway/providers/fal"
)

type billingRecorder struct {
	service *billing.Service
	err     error
}

func (recorder *billingRecorder) Begin(ctx context.Context, request billing.BeginRequest) (billing.Charge, error) {
	value, err := recorder.service.Begin(ctx, request)
	recorder.err = err
	return value, err
}

func falIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("fal_protocol_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(base)
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

func TestFalNativeLifecycleCapturesAfterDurableResult(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Key fal-integration-secret" {
			t.Fatalf("authorization=%q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "POST /fal-ai/flux/dev":
			_, _ = writer.Write([]byte(`{"request_id":"provider-internal","response_url":"https://queue.fal.run/internal","status_url":"https://queue.fal.run/internal/status","cancel_url":"https://queue.fal.run/internal/cancel"}`))
		case "GET /fal-ai/flux/dev/requests/provider-internal/status":
			_, _ = writer.Write([]byte(`{"status":"COMPLETED"}`))
		case "GET /fal-ai/flux/dev/requests/provider-internal":
			_, _ = writer.Write([]byte(`{"images":[{"url":"https://delivery.example/image.png"}]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	pool := falIntegrationPool(t)
	ctx := context.Background()
	digest := sha256.Sum256([]byte("fal-service-key"))
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO organizations(id,name,slug) VALUES('org_fal','fal','fal')`, nil},
		{`INSERT INTO projects(id,organization_id,name,slug) VALUES('project_fal','org_fal','fal','fal')`, nil},
		{`INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_fal','fal',$1,'ngw_fal','project_fal')`, []any{digest[:]}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	wallet := ledger.NewService(pool)
	if _, err := wallet.Deposit(ctx, "org_fal", 1000, "fal-deposit"); err != nil {
		t.Fatal(err)
	}
	estimator, _ := pricing.NewService(pool, 0)
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: "channel_00000000000000000000000000000005", Protocol: "fal", Operation: "image.generate", Model: "fal-ai/flux/dev", Size: "default", Quality: "default", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "fal-price"); err != nil {
		t.Fatal(err)
	}
	billingService, err := billing.NewServiceWithControls(pool, estimator, wallet, costquota.NewStore(pool), spendcap.NewStore(pool), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_FAL_API_KEY" {
			return "fal-integration-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := providerfal.New(providerfal.Config{Endpoint: upstream.URL, PublicBaseURL: "https://gateway.example", Timeout: time.Second, MaximumBodyBytes: 1 << 20}, credentials)
	if err != nil {
		t.Fatal(err)
	}
	repository, _ := jobs.NewRepository(pool, 1<<20)
	providers := map[string]jobs.Provider{"fal": adapter}
	service, _ := jobs.NewService(repository, providers, jobs.ServiceConfig{SubmitLease: time.Minute, PollDelay: time.Millisecond}, "fal-api")
	worker, _ := jobs.NewWorker(repository, providers, billingService, jobs.WorkerConfig{Interval: time.Second, Lease: time.Minute, PollDelay: time.Millisecond, BaseBackoff: time.Millisecond, MaximumBackoff: time.Second, BatchSize: 10, MaximumAttempts: 3}, "fal-worker")
	registry, _ := imageoperation.DefaultRegistryWithAsync(nil, []string{"fal-ai/flux/dev"})
	principal := apikey.Principal{APIKeyID: "key_fal", ProjectID: "project_fal", OrganizationID: "org_fal"}
	billable := &billingRecorder{service: billingService}
	handler := requestid.Middleware(NewHandler(nil, authStub{principal}, registry, service, billable, credentials, 1<<20, "https://gateway.example"))

	submit := httptest.NewRequest(http.MethodPost, "/fal-ai/flux/dev", strings.NewReader(`{"prompt":"cat"}`))
	submit.Header.Set("Authorization", "Key fal-service-key")
	submit.Header.Set("Content-Type", "application/json")
	submit.Header.Set("Idempotency-Key", "fal-lifecycle-idempotency")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, submit)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "provider-internal") || strings.Contains(response.Body.String(), "queue.fal.run") {
		t.Fatalf("submit=%d billing=%v body=%s", response.Code, billable.err, response.Body.String())
	}
	var queued map[string]any
	if json.Unmarshal(response.Body.Bytes(), &queued) != nil {
		t.Fatal("invalid submit response")
	}
	jobID, _ := queued["request_id"].(string)
	if _, err := pool.Exec(ctx, `UPDATE async_job_provider_attempts SET next_poll_at=now() WHERE job_id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	resultRequest := httptest.NewRequest(http.MethodGet, "/fal-ai/flux/dev/requests/"+jobID, nil)
	resultRequest.Header.Set("Authorization", "Key fal-service-key")
	resultResponse := httptest.NewRecorder()
	handler.ServeHTTP(resultResponse, resultRequest)
	if resultResponse.Code != http.StatusOK || !strings.Contains(resultResponse.Body.String(), "delivery.example") || resultResponse.Header().Get("X-Fal-Request-Id") != jobID {
		t.Fatalf("result=%d headers=%v body=%s", resultResponse.Code, resultResponse.Header(), resultResponse.Body.String())
	}
	var available, reserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_fal'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 900 || reserved != 0 || calls.Load() != 3 {
		t.Fatalf("wallet=%d/%d calls=%d", available, reserved, calls.Load())
	}
	replay := httptest.NewRequest(http.MethodPost, "/fal-ai/flux/dev", strings.NewReader(`{"prompt":"cat"}`))
	replay.Header.Set("Authorization", "Key fal-service-key")
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("Idempotency-Key", "fal-lifecycle-idempotency")
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusOK || !strings.Contains(replayResponse.Body.String(), jobID) || strings.Contains(replayResponse.Body.String(), "delivery.example") || calls.Load() != 3 {
		t.Fatalf("replay=%d calls=%d body=%s", replayResponse.Code, calls.Load(), replayResponse.Body.String())
	}
	for range 5 {
		statusRequest := httptest.NewRequest(http.MethodGet, "/fal-ai/flux/dev/requests/"+jobID+"/status?logs=0", nil)
		statusRequest.Header.Set("Authorization", "Key fal-service-key")
		handler.ServeHTTP(httptest.NewRecorder(), statusRequest)
	}
	if calls.Load() != 3 {
		t.Fatalf("public polling dispatched upstream calls=%d", calls.Load())
	}
}
