//go:build integration

package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

const openAIChannel = "channel_00000000000000000000000000000001"
const falChannel = "channel_00000000000000000000000000000005"

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
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_billing','Billing','billing');
		INSERT INTO projects(id,organization_id,name,slug) VALUES('project_billing','org_billing','Billing project','billing');
		INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_billing','Billing key',decode(repeat('11',32),'hex'),'ngw_sk_test','project_billing')`); err != nil {
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
	service, err := NewServiceWithControls(pool, estimator, wallet, costquota.NewStore(pool), spendcap.NewStore(pool), 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool
}

type sequenceEstimator struct {
	estimates []pricing.Estimate
	next      int
}

func TestFalQueueChargeCapturesDurableResult(t *testing.T) {
	service, pool := billingFixture(t, 1000)
	estimator, err := pricing.NewService(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := estimator.Publish(context.Background(), pricing.Price{ChannelID: falChannel, Protocol: "fal", Operation: "image.generate", Model: "fal-ai/flux/dev", Size: "default", Quality: "default", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "fal-price"); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(context.Background(), BeginRequest{RequestID: "fal-request", OrganizationID: "org_billing", ProjectID: "project_billing", APIKeyID: "key_billing", Protocol: "fal", Operation: "image.generate", Model: "fal-ai/flux/dev", ChannelID: falChannel, Quantity: 1, Size: "default", Quality: "default"})
	if err != nil {
		t.Fatal(err)
	}
	result := ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"images":[{"url":"https://delivery.example/image.png"}]}`)}
	captured, err := service.Complete(context.Background(), charge.ID, true, result)
	if err != nil || captured.State != "CAPTURED" || captured.CapturedSale != 100 {
		t.Fatalf("charge=%+v err=%v", captured, err)
	}
}

func TestCompleteWithQuantityCapturesPartialAmountAndReplaysExactly(t *testing.T) {
	service, pool := billingFixture(t, 1000)
	charge, err := service.Begin(context.Background(), BeginRequest{RequestID: "partial-output", OrganizationID: "org_billing", ProjectID: "project_billing", APIKeyID: "key_billing", Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", ChannelID: openAIChannel, Quantity: 3, Size: "default", Quality: "default"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"images":[{},{}]}`)}
	completed, err := service.CompleteWithQuantity(context.Background(), charge.ID, 2, snapshot)
	if err != nil || completed.CapturedSale != 200 || completed.ActualCost == nil || *completed.ActualCost != 120 {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	replayed, err := service.CompleteWithQuantity(context.Background(), charge.ID, 2, snapshot)
	if err != nil || replayed.CapturedSale != 200 {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	if _, err := service.CompleteWithQuantity(context.Background(), charge.ID, 1, snapshot); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflicting quantity err=%v", err)
	}
	var operations int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "image-capture:"+charge.ID+":usage:2").Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("usage operation count=%d err=%v", operations, err)
	}
	var available, reserved int64
	if err := pool.QueryRow(context.Background(), `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_billing'`).Scan(&available, &reserved); err != nil || available != 800 || reserved != 0 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
}

func (estimator *sequenceEstimator) EstimateInTx(_ context.Context, _ pgx.Tx, request pricing.Request) (pricing.Estimate, error) {
	if estimator.next >= len(estimator.estimates) {
		return pricing.Estimate{}, pricing.ErrPriceUnavailable
	}
	estimate := estimator.estimates[estimator.next]
	estimator.next++
	estimate.ChannelID = request.ChannelID
	estimate.Quantity = request.Quantity
	estimate.EvaluatedAt = request.At
	return estimate, nil
}

func serviceWithEstimator(t *testing.T, pool *pgxpool.Pool, estimator Estimator) *Service {
	t.Helper()
	service, err := NewServiceWithControls(pool, estimator, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool), 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestProviderSpendCapReserveCaptureAndRollback(t *testing.T) {
	service, pool := billingFixture(t, 10_000)
	store := spendcap.NewStore(pool)
	ctx := context.Background()
	policy, err := store.SetPolicy(ctx, spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Day, Limit: 150, Actor: "integration", Reason: "provider budget"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPolicy(ctx, spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Month, Limit: 130, Actor: "integration", Reason: "monthly provider budget"}); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(ctx, billableRequest("spend-capture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Capture(ctx, charge.ID); err != nil {
		t.Fatal(err)
	}
	request := billableRequest("spend-denied")
	request.Quantity = 1
	if _, err := service.Begin(ctx, request); !errors.Is(err, spendcap.ErrExceeded) {
		t.Fatalf("error=%v", err)
	} else {
		var limited *spendcap.LimitError
		if !errors.As(err, &limited) || limited.Period != spendcap.Day {
			t.Fatalf("limit=%+v", limited)
		}
	}
	usage, err := store.Usage(ctx, policy.ID, time.Now())
	if err != nil || usage.Captured != 120 || usage.Reserved != 0 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	var available int64
	var charges int
	if err := pool.QueryRow(ctx, `SELECT available FROM organization_wallets WHERE organization_id='org_billing'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image_request_charges`).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if available != 9_800 || charges != 1 {
		t.Fatalf("available=%d charges=%d", available, charges)
	}
}

func TestSpendCapFailureRollsBackWalletAndUserQuota(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	ctx := context.Background()
	if _, err := costquota.NewStore(pool).SetPolicy(ctx, costquota.PolicyInput{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Day, Limit: 500, Actor: "integration", Reason: "atomic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := spendcap.NewStore(pool).SetPolicy(ctx, spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Day, Limit: 100, Actor: "integration", Reason: "atomic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Begin(ctx, billableRequest("spend-atomic")); !errors.Is(err, spendcap.ErrExceeded) {
		t.Fatalf("error=%v", err)
	}
	var available, walletReserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_billing'`).Scan(&available, &walletReserved); err != nil {
		t.Fatal(err)
	}
	var quotaBuckets, charges int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cost_quota_buckets`).Scan(&quotaBuckets); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image_request_charges`).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if available != 1_000 || walletReserved != 0 || quotaBuckets != 0 || charges != 0 {
		t.Fatalf("effects=%d/%d buckets=%d charges=%d", available, walletReserved, quotaBuckets, charges)
	}
}

func TestSpendCapActualCostDifferenceAndRelease(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	ctx := context.Background()
	store := spendcap.NewStore(pool)
	if _, err := store.SetPolicy(ctx, spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Month, Limit: 220, Actor: "integration", Reason: "actual"}); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(ctx, billableRequest("spend-actual"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteWithAmounts(ctx, charge.ID, 90, 200, ResponseSnapshot{Status: 200, Headers: map[string][]string{}, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	second, err := service.Begin(ctx, billableRequest("spend-release"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	var reserved, captured int64
	if err := pool.QueryRow(ctx, `SELECT reserved,captured FROM provider_channel_spend_buckets`).Scan(&reserved, &captured); err != nil || reserved != 0 || captured != 90 {
		t.Fatalf("bucket=%d/%d err=%v", reserved, captured, err)
	}
}

func TestConcurrentServicesRespectSpendCap(t *testing.T) {
	service, pool := billingFixture(t, 10_000)
	if _, err := spendcap.NewStore(pool).SetPolicy(context.Background(), spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Day, Limit: 180, Actor: "integration", Reason: "concurrent"}); err != nil {
		t.Fatal(err)
	}
	estimator, _ := pricing.NewService(pool, 0)
	second, _ := NewServiceWithControls(pool, estimator, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool), 32*1024*1024)
	services := []*Service{service, second}
	results := make(chan error, 8)
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := billableRequest(fmt.Sprintf("spend-concurrent-%d", index))
			request.Quantity = 1
			_, err := services[index%2].Begin(context.Background(), request)
			results <- err
		}(index)
	}
	group.Wait()
	close(results)
	success, limited := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, spendcap.ErrExceeded) {
			limited++
		} else {
			t.Fatalf("error=%v", err)
		}
	}
	if success != 3 || limited != 5 {
		t.Fatalf("success=%d limited=%d", success, limited)
	}
}

func TestSpendCapPolicyAuditUpdateDisable(t *testing.T) {
	_, pool := billingFixture(t, 1_000)
	store := spendcap.NewStore(pool)
	input := spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Day, Limit: 100, Actor: "operator", Reason: "create"}
	created, err := store.SetPolicy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Limit, input.Reason = 200, "update"
	updated, err := store.SetPolicy(context.Background(), input)
	if err != nil || updated.ID != created.ID || updated.Version != 2 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if _, err := store.SetPolicy(context.Background(), spendcap.PolicyInput{ChannelID: "channel_ffffffffffffffffffffffffffffffff", Period: spendcap.Day, Limit: 1, Actor: "operator", Reason: "invalid"}); !errors.Is(err, spendcap.ErrInvalid) {
		t.Fatalf("invalid channel=%v", err)
	}
	if err := store.DisablePolicy(context.Background(), created.ID, "operator", "retire"); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_channel_spend_policy_events WHERE policy_id=$1`, created.ID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE provider_channel_spend_policy_events SET reason='tampered' WHERE policy_id=$1`, created.ID); err == nil {
		t.Fatal("event mutation succeeded")
	}
}

func billableRequest(requestID string) BeginRequest {
	return BeginRequest{RequestID: requestID, OrganizationID: "org_billing", ProjectID: "project_billing", APIKeyID: "key_billing", Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", ChannelID: openAIChannel, Quantity: 2}
}

func TestHierarchicalQuotaReserveCaptureAndRollback(t *testing.T) {
	service, pool := billingFixture(t, 10_000)
	store := costquota.NewStore(pool)
	ctx := context.Background()
	policies := []costquota.PolicyInput{
		{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Day, Limit: 500},
		{ScopeType: costquota.Project, OrganizationID: "org_billing", ProjectID: "project_billing", Period: costquota.Day, Limit: 250},
		{ScopeType: costquota.APIKey, OrganizationID: "org_billing", ProjectID: "project_billing", APIKeyID: "key_billing", Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", Period: costquota.Month, Limit: 250},
	}
	var organizationPolicyID string
	for index := range policies {
		policies[index].Actor, policies[index].Reason = "integration", "quota test"
		policy, err := store.SetPolicy(ctx, policies[index])
		if err != nil {
			t.Fatal(err)
		}
		if policies[index].ScopeType == costquota.Organization {
			organizationPolicyID = policy.ID
		}
	}
	charge, err := service.Begin(ctx, billableRequest("quota-capture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Capture(ctx, charge.ID); err != nil {
		t.Fatal(err)
	}
	denied := billableRequest("quota-denied")
	denied.Quantity = 1
	if _, err := service.Begin(ctx, denied); !errors.Is(err, costquota.ErrExceeded) {
		t.Fatalf("quota error=%v", err)
	} else {
		var limited *costquota.LimitError
		if !errors.As(err, &limited) || limited.ResetAt.After(time.Now().UTC().Add(48*time.Hour)) {
			t.Fatalf("limit metadata=%+v", limited)
		}
	}
	var available, reserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_billing'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 9_800 || reserved != 0 {
		t.Fatalf("wallet=%d/%d", available, reserved)
	}
	var chargeCount, ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image_request_charges`).Scan(&chargeCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if chargeCount != 1 || ledgerCount != 3 {
		t.Fatalf("effects charges=%d ledger=%d", chargeCount, ledgerCount)
	}
	var allocationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cost_quota_allocations WHERE charge_id=$1 AND state='captured'`, charge.ID).Scan(&allocationCount); err != nil || allocationCount != 3 {
		t.Fatalf("allocations=%d err=%v", allocationCount, err)
	}
	usage, err := store.Usage(ctx, organizationPolicyID, time.Now())
	if err != nil || usage.Captured != 200 || usage.Reserved != 0 || usage.Limit != 500 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}

func TestConcurrentQuotaNeverExceedsLimit(t *testing.T) {
	service, pool := billingFixture(t, 10_000)
	store := costquota.NewStore(pool)
	if _, err := store.SetPolicy(context.Background(), costquota.PolicyInput{ScopeType: costquota.Project, OrganizationID: "org_billing", ProjectID: "project_billing", Period: costquota.Day, Limit: 300, Actor: "integration", Reason: "concurrency"}); err != nil {
		t.Fatal(err)
	}
	secondEstimator, _ := pricing.NewService(pool, 0)
	secondService, _ := NewServiceWithQuota(pool, secondEstimator, ledger.NewService(pool), costquota.NewStore(pool), 32*1024*1024)
	services := []*Service{service, secondService}
	var group sync.WaitGroup
	results := make(chan error, 8)
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := billableRequest(fmt.Sprintf("quota-concurrent-%d", index))
			request.Quantity = 1
			_, err := services[index%len(services)].Begin(context.Background(), request)
			results <- err
		}(index)
	}
	group.Wait()
	close(results)
	succeeded, limited := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, costquota.ErrExceeded) {
			limited++
		} else {
			t.Fatalf("unexpected error=%v", err)
		}
	}
	if succeeded != 3 || limited != 5 {
		t.Fatalf("succeeded=%d limited=%d", succeeded, limited)
	}
	var bucketReserved int64
	if err := pool.QueryRow(context.Background(), `SELECT reserved FROM cost_quota_buckets`).Scan(&bucketReserved); err != nil || bucketReserved != 300 {
		t.Fatalf("bucket=%d err=%v", bucketReserved, err)
	}
}

func TestQuotaReleaseRestoresCapacity(t *testing.T) {
	service, pool := billingFixture(t, 10_000)
	store := costquota.NewStore(pool)
	if _, err := store.SetPolicy(context.Background(), costquota.PolicyInput{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Day, Limit: 200, Actor: "integration", Reason: "release"}); err != nil {
		t.Fatal(err)
	}
	first, err := service.Begin(context.Background(), billableRequest("quota-release-first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Begin(context.Background(), billableRequest("quota-release-second")); err != nil {
		t.Fatal(err)
	}
	var reserved, captured int64
	if err := pool.QueryRow(context.Background(), `SELECT reserved,captured FROM cost_quota_buckets`).Scan(&reserved, &captured); err != nil || reserved != 200 || captured != 0 {
		t.Fatalf("bucket=%d/%d err=%v", reserved, captured, err)
	}
}

func TestQuotaAndWalletReleaseEstimateDifference(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	if _, err := costquota.NewStore(pool).SetPolicy(context.Background(), costquota.PolicyInput{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Day, Limit: 200, Actor: "integration", Reason: "actual sale"}); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(context.Background(), billableRequest("quota-sale-difference"))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.CompleteWithSale(context.Background(), charge.ID, 150, ResponseSnapshot{Status: 200, Headers: map[string][]string{}, Body: []byte(`{}`)})
	if err != nil || completed.CapturedSale != 150 {
		t.Fatalf("charge=%+v err=%v", completed, err)
	}
	var available, reserved, captured int64
	if err := pool.QueryRow(context.Background(), `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_billing'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT captured FROM cost_quota_buckets`).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	if available != 850 || reserved != 0 || captured != 150 {
		t.Fatalf("wallet=%d/%d quota=%d", available, reserved, captured)
	}
	if _, err := service.CompleteWithSale(context.Background(), charge.ID, 150, ResponseSnapshot{Status: 200, Headers: map[string][]string{}, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteWithSale(context.Background(), charge.ID, 149, ResponseSnapshot{Status: 200, Headers: map[string][]string{}, Body: []byte(`{}`)}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("different actual retry=%v", err)
	}
}

func TestQuotaSettlementSurvivesServiceRestart(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	if _, err := costquota.NewStore(pool).SetPolicy(context.Background(), costquota.PolicyInput{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Month, Limit: 500, Actor: "integration", Reason: "restart"}); err != nil {
		t.Fatal(err)
	}
	if _, err := spendcap.NewStore(pool).SetPolicy(context.Background(), spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Month, Limit: 500, Actor: "integration", Reason: "restart"}); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(context.Background(), billableRequest("quota-restart"))
	if err != nil {
		t.Fatal(err)
	}
	estimator, _ := pricing.NewService(pool, 0)
	restarted, err := NewServiceWithControls(pool, estimator, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool), 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Capture(context.Background(), charge.ID); err != nil {
		t.Fatal(err)
	}
	var reserved, captured int64
	if err := pool.QueryRow(context.Background(), `SELECT reserved,captured FROM cost_quota_buckets`).Scan(&reserved, &captured); err != nil || reserved != 0 || captured != 200 {
		t.Fatalf("bucket=%d/%d err=%v", reserved, captured, err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT reserved,captured FROM provider_channel_spend_buckets`).Scan(&reserved, &captured); err != nil || reserved != 0 || captured != 120 {
		t.Fatalf("spend bucket=%d/%d err=%v", reserved, captured, err)
	}
}

func TestQuotaPolicyAuditUpdateDisableAndOwnership(t *testing.T) {
	_, pool := billingFixture(t, 1_000)
	store := costquota.NewStore(pool)
	input := costquota.PolicyInput{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Month, Limit: 500, Actor: "operator", Reason: "create"}
	created, err := store.SetPolicy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Limit, input.Reason = 700, "raise budget"
	updated, err := store.SetPolicy(context.Background(), input)
	if err != nil || updated.ID != created.ID || updated.Version != 2 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	invalid := input
	invalid.ScopeType = costquota.Project
	invalid.ProjectID = "project_other"
	if _, err := store.SetPolicy(context.Background(), invalid); !errors.Is(err, costquota.ErrInvalidPolicy) {
		t.Fatalf("ownership error=%v", err)
	}
	if err := store.DisablePolicy(context.Background(), created.ID, "operator", "retired"); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cost_quota_policy_events WHERE policy_id=$1`, created.ID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE cost_quota_policy_events SET reason='tampered' WHERE policy_id=$1`, created.ID); err == nil {
		t.Fatal("audit event mutation succeeded")
	}
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

func TestQuoteHasNoWalletLedgerOrChargeEffects(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	estimate, err := service.Quote(context.Background(), billableRequest("request-quote"))
	if err != nil || estimate.ChannelID != openAIChannel || estimate.MaximumSale != 200 {
		t.Fatalf("estimate=%+v error=%v", estimate, err)
	}
	assertWalletAndCharge(t, pool, 1_000, 0, 0, "")
}

func TestLowestCostBoundQuotePersistsRoutingEvidence(t *testing.T) {
	_, pool := billingFixture(t, 1_000)
	at := time.Now().UTC().Truncate(time.Microsecond)
	var priceID string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM provider_prices LIMIT 1`).Scan(&priceID); err != nil {
		t.Fatal(err)
	}
	estimate := pricing.Estimate{PriceID: priceID, Currency: ledger.Currency, EstimatedCost: 60, MaximumSale: 100}
	service := serviceWithEstimator(t, pool, &sequenceEstimator{estimates: []pricing.Estimate{estimate, estimate}})
	request := billableRequest("lowest-cost-evidence")
	request.RoutingPolicy = "lowest_cost"
	request.CostRank = 1
	request.EvaluationAt = at
	quoted, err := service.Quote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedQuote = &BoundQuote{PriceID: quoted.PriceID, ChannelID: quoted.ChannelID, Currency: quoted.Currency, EstimatedCost: quoted.EstimatedCost, MaximumSale: quoted.MaximumSale, EvaluatedAt: quoted.EvaluatedAt}
	charge, err := service.Begin(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if charge.RoutingPolicy != "lowest_cost" || charge.CostRank == nil || *charge.CostRank != 1 || charge.PriceEvaluatedAt == nil || !charge.PriceEvaluatedAt.Equal(at) || charge.PriceID != estimate.PriceID || charge.EstimatedCost != 60 || charge.ReservedSale != 100 {
		t.Fatalf("charge=%+v", charge)
	}
	var policy string
	var rank int
	var evaluatedAt time.Time
	if err := pool.QueryRow(context.Background(), `SELECT routing_policy,cost_rank,price_evaluated_at FROM image_request_charges WHERE id=$1`, charge.ID).Scan(&policy, &rank, &evaluatedAt); err != nil || policy != "lowest_cost" || rank != 1 || !evaluatedAt.Equal(at) {
		t.Fatalf("policy=%s rank=%d at=%s err=%v", policy, rank, evaluatedAt, err)
	}
}

func TestLowestCostSnapshotChangeHasNoFinancialEffects(t *testing.T) {
	_, pool := billingFixture(t, 1_000)
	at := time.Now().UTC().Truncate(time.Microsecond)
	first := pricing.Estimate{PriceID: "price_00000000000000000000000000000092", Currency: ledger.Currency, EstimatedCost: 60, MaximumSale: 100}
	changed := pricing.Estimate{PriceID: "price_00000000000000000000000000000093", Currency: ledger.Currency, EstimatedCost: 55, MaximumSale: 90}
	service := serviceWithEstimator(t, pool, &sequenceEstimator{estimates: []pricing.Estimate{first, changed}})
	request := billableRequest("lowest-cost-race")
	request.RoutingPolicy = "lowest_cost"
	request.EvaluationAt = at
	quoted, err := service.Quote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedQuote = &BoundQuote{PriceID: quoted.PriceID, ChannelID: quoted.ChannelID, Currency: quoted.Currency, EstimatedCost: quoted.EstimatedCost, MaximumSale: quoted.MaximumSale, EvaluatedAt: quoted.EvaluatedAt}
	if _, err := service.Begin(context.Background(), request); !errors.Is(err, ErrPriceSnapshotChanged) {
		t.Fatalf("error=%v", err)
	}
	assertWalletAndCharge(t, pool, 1_000, 0, 0, "")
}

func TestWeightedRoutingEvidencePersistsWithoutPriceEvaluationTime(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	request := billableRequest("weighted-evidence")
	request.RoutingPolicy = "weighted"
	request.CostRank = 2
	charge, err := service.Begin(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if charge.RoutingPolicy != "weighted" || charge.CostRank == nil || *charge.CostRank != 2 || charge.PriceEvaluatedAt != nil {
		t.Fatalf("charge=%+v", charge)
	}
	var policy string
	var rank int
	var evaluatedAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT routing_policy,cost_rank,price_evaluated_at FROM image_request_charges WHERE id=$1`, charge.ID).Scan(&policy, &rank, &evaluatedAt); err != nil || policy != "weighted" || rank != 2 || evaluatedAt != nil {
		t.Fatalf("policy=%s rank=%d at=%v err=%v", policy, rank, evaluatedAt, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE image_request_charges SET cost_rank=3 WHERE id=$1`, charge.ID); err == nil {
		t.Fatal("weighted routing evidence mutation succeeded")
	}
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
	if _, err := costquota.NewStore(pool).SetPolicy(ctx, costquota.PolicyInput{ScopeType: costquota.Organization, OrganizationID: "org_billing", Period: costquota.Day, Limit: 500, Actor: "integration", Reason: "reconciliation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := spendcap.NewStore(pool).SetPolicy(ctx, spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Day, Limit: 500, Actor: "integration", Reason: "reconciliation"}); err != nil {
		t.Fatal(err)
	}
	charge, err := service.Begin(ctx, billableRequest("request-reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	conflict := billableRequest("request-reconcile")
	conflict.Quality = "high"
	if _, err := service.Begin(ctx, conflict); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	if err := service.MarkReconciling(ctx, charge.ID, Observation{Outcome: Unknown, Reason: ExecutorTimeout}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(ctx, charge.ID); err != nil {
		t.Fatal(err)
	}
	var quotaReserved int64
	if err := pool.QueryRow(ctx, `SELECT reserved FROM cost_quota_buckets`).Scan(&quotaReserved); err != nil || quotaReserved != 0 {
		t.Fatalf("quota reserved=%d err=%v", quotaReserved, err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM provider_channel_spend_buckets`).Scan(&quotaReserved); err != nil || quotaReserved != 0 {
		t.Fatalf("spend reserved=%d err=%v", quotaReserved, err)
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

func TestIdempotencyReplayConflictAndSafeSnapshot(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	ctx := context.Background()
	if _, err := costquota.NewStore(pool).SetPolicy(ctx, costquota.PolicyInput{ScopeType: costquota.Project, OrganizationID: "org_billing", ProjectID: "project_billing", Period: costquota.Month, Limit: 500, Actor: "integration", Reason: "idempotency"}); err != nil {
		t.Fatal(err)
	}
	if _, err := spendcap.NewStore(pool).SetPolicy(ctx, spendcap.PolicyInput{ChannelID: openAIChannel, Period: spendcap.Month, Limit: 500, Actor: "integration", Reason: "idempotency"}); err != nil {
		t.Fatal(err)
	}
	request := billableRequest("original-request")
	request.IdempotencyKey = "client-idempotency-key"
	request.RequestFingerprint = [32]byte{1, 2, 3}
	charge, err := service.Begin(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Begin(ctx, request); !errors.Is(err, ErrRequestPending) {
		t.Fatalf("in-flight retry error=%v", err)
	}
	snapshot := ResponseSnapshot{Status: 201, Headers: map[string][]string{"Content-Type": {"application/json"}, "Retry-After": {"2"}, "Set-Cookie": {"secret=cookie"}}, Body: []byte(`{"native":"response"}`)}
	completed, err := service.Complete(ctx, charge.ID, true, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := completed.Response.Headers["Set-Cookie"]; exists {
		t.Fatalf("unsafe headers=%v", completed.Response.Headers)
	}
	retryRequest := request
	retryRequest.RequestID = "retry-request"
	restartedEstimator, _ := pricing.NewService(pool, 0)
	restarted, _ := NewService(pool, restartedEstimator, ledger.NewService(pool))
	replayed, err := restarted.Begin(ctx, retryRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replay || replayed.ID != charge.ID || replayed.Response.Status != 201 || string(replayed.Response.Body) != `{"native":"response"}` || replayed.Response.Headers["Retry-After"][0] != "2" {
		t.Fatalf("replay=%+v", replayed)
	}
	var quotaAllocations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cost_quota_allocations WHERE charge_id=$1`, charge.ID).Scan(&quotaAllocations); err != nil || quotaAllocations != 1 {
		t.Fatalf("quota allocations=%d err=%v", quotaAllocations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_channel_spend_allocations WHERE charge_id=$1`, charge.ID).Scan(&quotaAllocations); err != nil || quotaAllocations != 1 {
		t.Fatalf("spend allocations=%d err=%v", quotaAllocations, err)
	}
	routedRetry := retryRequest
	routedRetry.ChannelID = "channel_00000000000000000000000000000002"
	if routed, err := restarted.Begin(ctx, routedRetry); err != nil || !routed.Replay || routed.ID != charge.ID {
		t.Fatalf("route-independent replay=%+v error=%v", routed, err)
	}
	conflict := retryRequest
	conflict.RequestFingerprint = [32]byte{9}
	if _, err := service.Begin(ctx, conflict); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	assertWalletAndCharge(t, pool, 800, 0, 1, "CAPTURED")
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_billing_other','Other billing','other-billing'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_billing_other','org_billing_other','Other billing','other-billing')`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.NewService(pool).Deposit(ctx, "org_billing_other", 500, "other-deposit"); err != nil {
		t.Fatal(err)
	}
	other := request
	other.RequestID = "other-request"
	other.OrganizationID = "org_billing_other"
	other.ProjectID = "project_billing_other"
	if _, err := service.Begin(ctx, other); err != nil {
		t.Fatalf("organization-scoped key error=%v", err)
	}
}

func TestConcurrentIdempotentBeginHasSingleReservation(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	request := billableRequest("concurrent-original")
	request.IdempotencyKey = "concurrent-key"
	request.RequestFingerprint = [32]byte{4}
	type outcome struct {
		err error
	}
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := service.Begin(context.Background(), request)
			results <- outcome{err}
		}()
	}
	wait.Wait()
	close(results)
	success, pending := 0, 0
	for result := range results {
		if result.err == nil {
			success++
		} else if errors.Is(result.err, ErrRequestPending) {
			pending++
		} else {
			t.Fatal(result.err)
		}
	}
	if success != 1 || pending != 1 {
		t.Fatalf("success/pending=%d/%d", success, pending)
	}
	assertWalletAndCharge(t, pool, 800, 200, 1, "RESERVED")
}

func TestResponseLimitAndCorruptionFailClosed(t *testing.T) {
	service, pool := billingFixture(t, 1_000)
	service.maxResponseBytes = 4
	request := billableRequest("response-limit")
	request.IdempotencyKey = "response-limit-key"
	request.RequestFingerprint = [32]byte{5}
	charge, err := service.Begin(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), charge.ID, true, ResponseSnapshot{Status: 200, Body: []byte("12345")}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized error=%v", err)
	}
	assertWalletAndCharge(t, pool, 800, 200, 1, "RESERVED")
	service.maxResponseBytes = 1024
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION fail_test_charge_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test completion failure'; END $$; CREATE TRIGGER fail_test_charge_completion BEFORE UPDATE ON image_request_charges FOR EACH ROW WHEN (NEW.response_snapshot_version=1) EXECUTE FUNCTION fail_test_charge_completion()`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), charge.ID, true, ResponseSnapshot{Status: 200, Body: []byte("safe")}); err == nil {
		t.Fatal("completion succeeded through failing trigger")
	}
	assertWalletAndCharge(t, pool, 800, 200, 1, "RESERVED")
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER fail_test_charge_completion ON image_request_charges; DROP FUNCTION fail_test_charge_completion()`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), charge.ID, true, ResponseSnapshot{Status: 200, Body: []byte("safe")}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `ALTER TABLE image_request_charges DISABLE TRIGGER image_request_charges_update_guard`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE image_request_charges SET response_body='broken' WHERE id=$1`, charge.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `ALTER TABLE image_request_charges ENABLE TRIGGER image_request_charges_update_guard`); err != nil {
		t.Fatal(err)
	}
	retry := request
	retry.RequestID = "corrupt-retry"
	if _, err := service.Begin(context.Background(), retry); !errors.Is(err, ErrSnapshotCorrupt) {
		t.Fatalf("corrupt replay error=%v", err)
	}
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
