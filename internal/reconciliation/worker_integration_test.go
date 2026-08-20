//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
)

const reconciliationChannel = "channel_00000000000000000000000000000001"

func reconciliationFixture(t *testing.T) (*billing.Service, *pgxpool.Pool) {
	t.Helper()
	pool := reconciliationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_reconcile','Reconcile','reconcile'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_reconcile','org_reconcile','Reconcile project','reconcile')`); err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err := wallet.Deposit(ctx, "org_reconcile", 1_000, "deposit"); err != nil {
		t.Fatal(err)
	}
	estimator, _ := pricing.NewService(pool, 0)
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: reconciliationChannel, Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "reconciliation-price"); err != nil {
		t.Fatal(err)
	}
	service, _ := billing.NewService(pool, estimator, wallet)
	return service, pool
}

func reconciliationRequest(id string) billing.BeginRequest {
	return billing.BeginRequest{RequestID: id, OrganizationID: "org_reconcile", ProjectID: "project_reconcile", Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", ChannelID: reconciliationChannel, Quantity: 1}
}

func TestKnownOutcomesResolveAndReplay(t *testing.T) {
	service, pool := reconciliationFixture(t)
	ctx := context.Background()
	successRequest := reconciliationRequest("known-success")
	successRequest.IdempotencyKey = "known-success-key"
	successRequest.RequestFingerprint = [32]byte{1}
	success, err := service.Begin(ctx, successRequest)
	if err != nil {
		t.Fatal(err)
	}
	successSnapshot := billing.ResponseSnapshot{Status: 502, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"code":"response_unavailable"}`)}
	if err := service.MarkReconciling(ctx, success.ID, billing.Observation{Outcome: billing.KnownSuccess, Reason: billing.ResponseUnavailable, Snapshot: successSnapshot}); err != nil {
		t.Fatal(err)
	}
	failureRequest := reconciliationRequest("known-failure")
	failureRequest.IdempotencyKey = "known-failure-key"
	failureRequest.RequestFingerprint = [32]byte{2}
	failure, err := service.Begin(ctx, failureRequest)
	if err != nil {
		t.Fatal(err)
	}
	failureSnapshot := billing.ResponseSnapshot{Status: 502, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"code":"response_unavailable"}`)}
	if err := service.MarkReconciling(ctx, failure.ID, billing.Observation{Outcome: billing.KnownFailure, Reason: billing.ResponseUnavailable, Snapshot: failureSnapshot}); err != nil {
		t.Fatal(err)
	}
	worker := testWorker(t, pool, service, time.Now)
	result, err := worker.RunOnce(ctx)
	if err != nil || result.Claimed != 2 || result.Resolved != 2 {
		t.Fatalf("run=%+v error=%v", result, err)
	}
	var available, reserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_reconcile'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 900 || reserved != 0 {
		t.Fatalf("wallet=%d/%d", available, reserved)
	}
	retry := successRequest
	retry.RequestID = "known-success-retry"
	replayed, err := service.Begin(ctx, retry)
	if err != nil || !replayed.Replay || string(replayed.Response.Body) != string(successSnapshot.Body) {
		t.Fatalf("replay=%+v error=%v", replayed, err)
	}
}

func TestUnknownOutcomeRetainsReservationThenBecomesManual(t *testing.T) {
	service, pool := reconciliationFixture(t)
	ctx := context.Background()
	charge, err := service.Begin(ctx, reconciliationRequest("unknown"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.MarkReconciling(ctx, charge.ID, billing.Observation{Outcome: billing.Unknown, Reason: billing.ExecutorTimeout}); err != nil {
		t.Fatal(err)
	}
	current := time.Now().UTC()
	now := func() time.Time { return current }
	worker := testWorker(t, pool, service, now)
	worker.config.MaxAttempts = 2
	first, err := worker.RunOnce(ctx)
	if err != nil || first.Retried != 1 {
		t.Fatalf("first=%+v error=%v", first, err)
	}
	current = current.Add(2 * time.Second)
	second, err := worker.RunOnce(ctx)
	if err != nil || second.Manual != 1 {
		t.Fatalf("second=%+v error=%v", second, err)
	}
	var available, reserved int64
	var state string
	if err := pool.QueryRow(ctx, `SELECT w.available,w.reserved,r.state FROM organization_wallets w JOIN image_charge_reconciliations r ON r.charge_id=$1 WHERE w.organization_id='org_reconcile'`, charge.ID).Scan(&available, &reserved, &state); err != nil {
		t.Fatal(err)
	}
	if available != 900 || reserved != 100 || state != "MANUAL_REVIEW" {
		t.Fatalf("wallet/state=%d/%d %s", available, reserved, state)
	}
}

func TestConcurrentWorkersClaimOnceAndRecoverExpiredLease(t *testing.T) {
	service, pool := reconciliationFixture(t)
	ctx := context.Background()
	charge, err := service.Begin(ctx, reconciliationRequest("leased"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := billing.ResponseSnapshot{Status: 502, Body: []byte("unavailable")}
	if err := service.MarkReconciling(ctx, charge.ID, billing.Observation{Outcome: billing.KnownSuccess, Reason: billing.ResponseUnavailable, Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	current := time.Now().UTC()
	first := testWorker(t, pool, service, func() time.Time { return current })
	claimed, err := first.claim(ctx, current)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%d error=%v", len(claimed), err)
	}
	second := testWorker(t, pool, service, func() time.Time { return current })
	if result, err := second.RunOnce(ctx); err != nil || result.Claimed != 0 {
		t.Fatalf("unexpired lease=%+v error=%v", result, err)
	}
	current = current.Add(2 * time.Second)
	if result, err := second.RunOnce(ctx); err != nil || result.Resolved != 1 {
		t.Fatalf("expired lease=%+v error=%v", result, err)
	}

	other, err := service.Begin(ctx, reconciliationRequest("concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.MarkReconciling(ctx, other.ID, billing.Observation{Outcome: billing.KnownFailure, Reason: billing.ResponseUnavailable, Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	workers := []*Worker{testWorker(t, pool, service, time.Now), testWorker(t, pool, service, time.Now)}
	results := make(chan RunResult, 2)
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for _, worker := range workers {
		wait.Add(1)
		go func(worker *Worker) {
			defer wait.Done()
			result, err := worker.RunOnce(ctx)
			results <- result
			errorsChannel <- err
		}(worker)
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	claimedCount, resolvedCount := 0, 0
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		claimedCount += result.Claimed
		resolvedCount += result.Resolved
	}
	if claimedCount != 1 || resolvedCount != 1 {
		t.Fatalf("claimed/resolved=%d/%d", claimedCount, resolvedCount)
	}
}

type completeThenFail struct {
	service *billing.Service
	failed  bool
}

func (completer *completeThenFail) Complete(ctx context.Context, id string, success bool, snapshot billing.ResponseSnapshot) (billing.Charge, error) {
	charge, err := completer.service.Complete(ctx, id, success, snapshot)
	if err == nil && !completer.failed {
		completer.failed = true
		return charge, errors.New("lost completion response")
	}
	return charge, err
}

func TestCrashAfterCompleteIsRecoveredIdempotently(t *testing.T) {
	service, pool := reconciliationFixture(t)
	ctx := context.Background()
	charge, _ := service.Begin(ctx, reconciliationRequest("commit-unknown"))
	snapshot := billing.ResponseSnapshot{Status: 502, Body: []byte("unavailable")}
	_ = service.MarkReconciling(ctx, charge.ID, billing.Observation{Outcome: billing.KnownSuccess, Reason: billing.SettlementFailed, Snapshot: snapshot})
	flaky := &completeThenFail{service: service}
	worker := testWorker(t, pool, flaky, time.Now)
	first, err := worker.RunOnce(ctx)
	if err != nil || first.Retried != 1 {
		t.Fatalf("first=%+v error=%v", first, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE image_charge_reconciliations SET next_attempt_at=now() WHERE charge_id=$1`, charge.ID); err != nil {
		t.Fatal(err)
	}
	second, err := worker.RunOnce(ctx)
	if err != nil || second.Resolved != 1 {
		t.Fatalf("second=%+v error=%v", second, err)
	}
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE reservation_id=$1 AND entry_type='capture'`, charge.ReservationID).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("capture entries=%d error=%v", entries, err)
	}
}

func testWorker(t *testing.T, pool *pgxpool.Pool, completer Completer, now func() time.Time) *Worker {
	t.Helper()
	worker, err := newWorker(pool, completer, Config{Interval: time.Second, Lease: time.Second, BaseBackoff: time.Second, MaxBackoff: time.Minute, BatchSize: 10, MaxAttempts: 3}, zeroReader{}, now)
	if err != nil {
		t.Fatal(err)
	}
	worker.owner = fmt.Sprintf("worker_test_%d", testWorkerSequence.Add(1))
	return worker
}

var testWorkerSequence atomic.Int64

type zeroReader struct{}

func (zeroReader) Read(value []byte) (int, error) {
	for index := range value {
		value[index] = 1
	}
	return len(value), nil
}

func reconciliationPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("reconciliation_test_%d", time.Now().UnixNano())
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
