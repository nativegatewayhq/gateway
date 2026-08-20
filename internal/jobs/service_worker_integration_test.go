//go:build integration

package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type fakeAsyncProvider struct {
	submits           atomic.Int64
	polls             atomic.Int64
	cancels           atomic.Int64
	submitResult      SubmitResult
	submitError       error
	pollObservation   joboperation.Observation
	pollError         error
	cancelObservation joboperation.Observation
	cancelError       error
	submitStarted     chan struct{}
	submitRelease     chan struct{}
	pollAttempt       chan ProviderAttempt
}

func (provider *fakeAsyncProvider) Submit(_ context.Context, _ joboperation.Job, _ any) (SubmitResult, error) {
	provider.submits.Add(1)
	if provider.submitStarted != nil {
		select {
		case provider.submitStarted <- struct{}{}:
		default:
			{
			}
		}
	}
	if provider.submitRelease != nil {
		<-provider.submitRelease
	}
	return provider.submitResult, provider.submitError
}

func TestConcurrentServiceSubmitDispatchesProviderOnce(t *testing.T) {
	repository, _, request := jobRepositoryFixture(t)
	provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Hour}, submitStarted: make(chan struct{}, 1), submitRelease: make(chan struct{})}
	service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	ctx := context.Background()
	first := make(chan error, 1)
	go func() { _, err := service.Submit(ctx, request, nil); first <- err }()
	<-provider.submitStarted
	secondDone := make(chan error, 1)
	go func() { _, err := service.Submit(ctx, request, nil); secondDone <- err }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent replay blocked")
	}
	close(provider.submitRelease)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if provider.submits.Load() != 1 {
		t.Fatalf("submits=%d", provider.submits.Load())
	}
}
func (provider *fakeAsyncProvider) Poll(_ context.Context, attempt ProviderAttempt) (joboperation.Observation, error) {
	provider.polls.Add(1)
	if provider.pollAttempt != nil {
		select {
		case provider.pollAttempt <- attempt:
		default:
		}
	}
	return provider.pollObservation, provider.pollError
}
func (provider *fakeAsyncProvider) Cancel(_ context.Context, _ ProviderAttempt) (joboperation.Observation, error) {
	provider.cancels.Add(1)
	return provider.cancelObservation, provider.cancelError
}

func jobServiceConfig() ServiceConfig {
	return ServiceConfig{SubmitLease: time.Minute, PollDelay: time.Millisecond}
}
func jobWorkerConfig() WorkerConfig {
	return WorkerConfig{Interval: time.Second, Lease: time.Minute, PollDelay: time.Second, BaseBackoff: time.Millisecond, MaximumBackoff: time.Second, BatchSize: 100, MaximumAttempts: 3}
}

func TestServiceSubmitsOnceAndWorkerRecoversToTerminal(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Millisecond}, pollObservation: joboperation.Observation{Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"output":"done"}`)}}, pollAttempt: make(chan ProviderAttempt, 1)}
	service, err := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := service.Submit(ctx, request, []byte("secret input"))
	if err != nil || created.Status != joboperation.Queued {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	replayed, err := service.Submit(ctx, request, []byte("secret input"))
	if err != nil || replayed.ID != created.ID || provider.submits.Load() != 1 {
		t.Fatalf("replay=%+v submits=%d err=%v", replayed, provider.submits.Load(), err)
	}
	worker, err := NewWorker(repository, map[string]Provider{"openai": provider}, nil, jobWorkerConfig(), "worker-instance")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case attempt := <-provider.pollAttempt:
		if attempt.Model != request.Model {
			t.Fatalf("poll model=%q want=%q", attempt.Model, request.Model)
		}
	default:
		t.Fatal("poll attempt was not observed")
	}
	terminal, err := service.Get(ctx, owner, created.ID)
	if err != nil || terminal.Status != joboperation.Succeeded || terminal.SettlementState != "SETTLED" {
		t.Fatalf("terminal=%+v err=%v", terminal, err)
	}
	var leaked bool
	if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM async_job_events WHERE job_id=$1 AND (category LIKE '%secret%' OR source LIKE '%secret%'))`, created.ID).Scan(&leaked); err != nil || leaked {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}

func TestSharedWorkerIsolatesReplicateAndFalProviders(t *testing.T) {
	repository, owner, replicateRequest := jobRepositoryFixture(t)
	replicateRequest.Provider = "replicate"
	replicateRequest.Model = "owner/model:version"
	falRequest := replicateRequest
	falRequest.RequestID += "-fal"
	falRequest.IdempotencyKey += "-fal"
	falRequest.Model = "fal-ai/flux/dev"
	falRequest.Provider = "fal"
	falRequest.ChannelID = "channel_00000000000000000000000000000005"
	falRequest.Fingerprint = [32]byte{2}
	snapshot := joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"output":"done"}`)}
	replicateProvider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "replicate-job", Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Millisecond}, pollObservation: joboperation.Observation{Status: joboperation.Succeeded, Snapshot: snapshot}, pollAttempt: make(chan ProviderAttempt, 1)}
	falProvider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "fal-job", Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Millisecond}, pollObservation: joboperation.Observation{Status: joboperation.Succeeded, Snapshot: snapshot}, pollAttempt: make(chan ProviderAttempt, 1)}
	providers := map[string]Provider{"replicate": replicateProvider, "fal": falProvider}
	service, err := NewService(repository, providers, jobServiceConfig(), "shared-api")
	if err != nil {
		t.Fatal(err)
	}
	replicateJob, err := service.Submit(context.Background(), replicateRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	falJob, err := service.Submit(context.Background(), falRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	worker, _ := NewWorker(repository, providers, nil, jobWorkerConfig(), "shared-worker")
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, expected := range map[string]string{replicateJob.ID: replicateRequest.Model, falJob.ID: falRequest.Model} {
		stored, err := service.Get(context.Background(), owner, id)
		if err != nil || stored.Status != joboperation.Succeeded || stored.Model != expected {
			t.Fatalf("job=%+v expected=%s err=%v", stored, expected, err)
		}
	}
	if replicateProvider.polls.Load() != 1 || falProvider.polls.Load() != 1 {
		t.Fatalf("polls replicate=%d fal=%d", replicateProvider.polls.Load(), falProvider.polls.Load())
	}
}

func TestKnownSubmitFailureSettlesWithoutRetryingProvider(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	failure := joboperation.Observation{Status: joboperation.Failed, FailureCategory: "rejected", Snapshot: joboperation.Snapshot{Status: 400, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"rejected"}`)}}
	provider := &fakeAsyncProvider{submitError: &ProviderError{Category: "rejected", Known: true, Observation: failure}}
	service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	failed, err := service.Submit(context.Background(), request, nil)
	if err != nil || failed.Status != joboperation.Failed {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	worker, _ := NewWorker(repository, map[string]Provider{"openai": provider}, nil, jobWorkerConfig(), "worker-instance")
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(context.Background(), owner, failed.ID)
	if err != nil || stored.SettlementState != "SETTLED" || provider.submits.Load() != 1 {
		t.Fatalf("stored=%+v counts=%d/%d err=%v", stored, provider.submits.Load(), provider.polls.Load(), err)
	}
}

func TestSettlementConvergesExactlyOnceAfterCrashBoundary(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	wallet := ledger.NewService(repository.pool)
	if _, err := wallet.Deposit(ctx, owner.OrganizationID, 1000, "async-job-deposit-"+request.RequestID); err != nil {
		t.Fatal(err)
	}
	estimator, err := pricing.NewService(repository.pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: request.ChannelID, Protocol: "openai", Operation: "image.generate", Model: request.Model, UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "async-job-price-"+request.RequestID); err != nil {
		t.Fatal(err)
	}
	billingService, err := billing.NewService(repository.pool, estimator, wallet)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := billingService.Begin(ctx, billing.BeginRequest{RequestID: "charge-" + request.RequestID, OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: owner.APIKeyID, Protocol: "openai", Operation: "image.generate", Model: request.Model, ChannelID: request.ChannelID, Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}
	request.ChargeID = charge.ID
	snapshot := joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"output":"charged"}`)}
	provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Succeeded, Snapshot: snapshot}}}
	service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	terminal, err := service.Submit(ctx, request, nil)
	if err != nil || terminal.SettlementState != "PENDING" {
		t.Fatalf("terminal=%+v err=%v", terminal, err)
	}
	claimed, err := repository.ClaimSettlements(ctx, "crashed-worker", time.Now(), 50*time.Millisecond, 100)
	lease, found := settlementFor(claimed, terminal.ID)
	if err != nil || !found {
		t.Fatalf("lease=%+v err=%v", claimed, err)
	}
	billingSnapshot := billing.ResponseSnapshot{Status: snapshot.Status, Headers: snapshot.Headers, Body: snapshot.Body}
	if _, err := billingService.Complete(ctx, charge.ID, true, billingSnapshot); err != nil {
		t.Fatal(err)
	}
	worker, _ := NewWorker(repository, map[string]Provider{"openai": provider}, billingService, jobWorkerConfig(), "recovery-worker")
	worker.now = func() time.Time { return time.Now().Add(time.Second) }
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(ctx, owner, terminal.ID)
	if err != nil || stored.SettlementState != "SETTLED" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	var available, reserved int64
	if err := repository.pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id=$1`, owner.OrganizationID).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 900 || reserved != 0 {
		t.Fatalf("wallet=%d/%d", available, reserved)
	}
	_ = lease // intentionally abandoned to simulate the crash after Billing commit.
}

func TestSubmitUnknownAndCancelUnknownRemainReconciling(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	provider := &fakeAsyncProvider{submitError: context.DeadlineExceeded, pollError: errors.New("unavailable"), cancelObservation: joboperation.Observation{Status: joboperation.Canceled}}
	service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	unknown, err := service.Submit(context.Background(), request, nil)
	if err != nil || unknown.Status != joboperation.Reconciling {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if _, _, claimed, err := repository.ClaimCancel(context.Background(), owner, unknown.ID, "cancel", time.Minute); err != nil || claimed {
		t.Fatalf("unknown submit cancel claimed=%v err=%v", claimed, err)
	}
	if provider.submits.Load() != 1 {
		t.Fatalf("submits=%d", provider.submits.Load())
	}
}

func TestServiceCancelInvokesProviderOnlyOnce(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Processing}, PollAfter: time.Hour}, cancelObservation: joboperation.Observation{Status: joboperation.Canceled}}
	service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	created, err := service.Submit(context.Background(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := service.Cancel(context.Background(), owner, created.ID)
	if err != nil || canceled.Status != joboperation.Canceled {
		t.Fatalf("canceled=%+v err=%v", canceled, err)
	}
	again, err := service.Cancel(context.Background(), owner, created.ID)
	if err != nil || again.Status != joboperation.Canceled || provider.cancels.Load() != 1 {
		t.Fatalf("again=%+v cancels=%d err=%v", again, provider.cancels.Load(), err)
	}
}

func TestWorkerExhaustionLeavesReservationPathInManualReconciliation(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Millisecond}, pollError: &ProviderError{Category: "timeout"}}
	service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
	created, err := service.Submit(context.Background(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := jobWorkerConfig()
	config.MaximumAttempts = 2
	worker, _ := NewWorker(repository, map[string]Provider{"openai": provider}, nil, config, "worker-instance")
	current := time.Now().Add(time.Second)
	worker.now = func() time.Time { return current }
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Second)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(context.Background(), owner, created.ID)
	if err != nil || stored.Status != joboperation.Reconciling || stored.SettlementState != "NONE" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	var state, next string
	if err := repository.pool.QueryRow(context.Background(), `SELECT state,next_poll_at::text FROM async_job_provider_attempts WHERE job_id=$1`, created.ID).Scan(&state, &next); err != nil || state != "RECONCILING" || next != "infinity" {
		t.Fatalf("attempt=%s/%s err=%v", state, next, err)
	}
}

func TestFailedAndCanceledJobsReleaseBillingReservation(t *testing.T) {
	for _, scenario := range []string{"failed", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			repository, owner, request, billingService := billableAsyncFixture(t)
			var provider *fakeAsyncProvider
			recoveryObservation := joboperation.Observation{Status: joboperation.Reconciling, FailureCategory: "provider_error"}
			if scenario == "failed" {
				provider = &fakeAsyncProvider{submitError: &ProviderError{Category: "rejected", Known: true, Observation: joboperation.Observation{Status: joboperation.Failed, FailureCategory: "rejected", Snapshot: joboperation.Snapshot{Status: 400, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"rejected"}`)}}}, pollObservation: recoveryObservation}
			} else {
				provider = &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Processing}, PollAfter: time.Hour}, cancelObservation: joboperation.Observation{Status: joboperation.Canceled}, pollObservation: recoveryObservation}
			}
			service, _ := NewService(repository, map[string]Provider{"openai": provider}, jobServiceConfig(), "api-instance")
			terminal, err := service.Submit(context.Background(), request, nil)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "canceled" {
				terminal, err = service.Cancel(context.Background(), owner, terminal.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			worker, _ := NewWorker(repository, map[string]Provider{"openai": provider}, billingService, jobWorkerConfig(), "worker-instance")
			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			stored, err := service.Get(context.Background(), owner, terminal.ID)
			if err != nil || stored.SettlementState != "SETTLED" {
				t.Fatalf("stored=%+v err=%v", stored, err)
			}
			var available, reserved int64
			if err := repository.pool.QueryRow(context.Background(), `SELECT available,reserved FROM organization_wallets WHERE organization_id=$1`, owner.OrganizationID).Scan(&available, &reserved); err != nil {
				t.Fatal(err)
			}
			if available != 1000 || reserved != 0 {
				t.Fatalf("wallet=%d/%d", available, reserved)
			}
		})
	}
}

func billableAsyncFixture(t *testing.T) (*Repository, joboperation.Owner, CreateRequest, *billing.Service) {
	t.Helper()
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	wallet := ledger.NewService(repository.pool)
	if _, err := wallet.Deposit(ctx, owner.OrganizationID, 1000, "async-job-deposit-"+request.RequestID); err != nil {
		t.Fatal(err)
	}
	estimator, err := pricing.NewService(repository.pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: request.ChannelID, Protocol: "openai", Operation: "image.generate", Model: request.Model, UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "async-job-price-"+request.RequestID); err != nil {
		t.Fatal(err)
	}
	billingService, err := billing.NewService(repository.pool, estimator, wallet)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := billingService.Begin(ctx, billing.BeginRequest{RequestID: "charge-" + request.RequestID, OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: owner.APIKeyID, Protocol: "openai", Operation: "image.generate", Model: request.Model, ChannelID: request.ChannelID, Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}
	request.ChargeID = charge.ID
	return repository, owner, request, billingService
}
