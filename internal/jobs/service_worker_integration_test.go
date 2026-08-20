//go:build integration

package jobs

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
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
	submitPayload     any
}

func (provider *fakeAsyncProvider) Submit(_ context.Context, _ joboperation.Job, payload any) (SubmitResult, error) {
	provider.submits.Add(1)
	provider.submitPayload = payload
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

type fakeWebhookPayload struct{ callback string }

func (payload fakeWebhookPayload) WithWebhook(callback string) (any, error) {
	payload.callback = callback
	return payload, nil
}

func TestServiceCreatesBindingAndInjectsGatewayWebhookBeforeSubmit(t *testing.T) {
	repository, _, request := jobRepositoryFixture(t)
	request.Provider = "replicate"
	request.ChannelID = "channel_00000000000000000000000000000004"
	provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "provider-" + request.RequestID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Hour}}
	config := jobServiceConfig()
	config.Webhooks = map[string]WebhookConfig{"replicate": {PublicBaseURL: "https://gateway.example", BindingTTL: time.Hour, CallbackSecret: testWebhookCallbackSecret()}}
	service, err := NewService(repository, map[string]Provider{"replicate": provider}, config, "api-instance")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Submit(context.Background(), request, fakeWebhookPayload{})
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := provider.submitPayload.(fakeWebhookPayload)
	if !ok || !strings.HasPrefix(payload.callback, "https://gateway.example/internal/webhooks/replicate/"+created.ID+"/whk_") {
		t.Fatalf("payload=%+v", provider.submitPayload)
	}
	if strings.Contains(string(created.Snapshot.Body), "whk_") {
		t.Fatal("callback capability leaked to public snapshot")
	}
	var digestBytes int
	if err := repository.pool.QueryRow(context.Background(), `SELECT octet_length(token_digest) FROM async_job_webhook_bindings WHERE job_id=$1`, created.ID).Scan(&digestBytes); err != nil || digestBytes != sha256.Size {
		t.Fatalf("digest=%d err=%v", digestBytes, err)
	}
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
	replicateProvider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "replicate-" + replicateRequest.RequestID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Millisecond}, pollObservation: joboperation.Observation{Status: joboperation.Succeeded, Snapshot: snapshot}, pollAttempt: make(chan ProviderAttempt, 1)}
	falProvider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: "fal-" + falRequest.RequestID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Millisecond}, pollObservation: joboperation.Observation{Status: joboperation.Succeeded, Snapshot: snapshot}, pollAttempt: make(chan ProviderAttempt, 1)}
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

func TestWebhookTerminalSettlementCapturesOrReleasesExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    joboperation.Status
		expected  int64
		entryType string
	}{
		{name: "success", status: joboperation.Succeeded, expected: 900, entryType: "capture"},
		{name: "failure", status: joboperation.Failed, expected: 1000, entryType: "release"},
		{name: "canceled", status: joboperation.Canceled, expected: 1000, entryType: "release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, owner, request := jobRepositoryFixture(t)
			request.Provider = "replicate"
			request.ChannelID = "channel_00000000000000000000000000000004"
			ctx := context.Background()
			wallet := ledger.NewService(repository.pool)
			if _, err := wallet.Deposit(ctx, owner.OrganizationID, 1000, "webhook-deposit-"+request.RequestID); err != nil {
				t.Fatal(err)
			}
			estimator, err := pricing.NewService(repository.pool, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: request.ChannelID, Protocol: "replicate", Operation: "image.generate", Model: request.Model, Size: "default", Quality: "default", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "webhook-price-"+request.RequestID); err != nil {
				t.Fatal(err)
			}
			billingService, err := billing.NewService(repository.pool, estimator, wallet)
			if err != nil {
				t.Fatal(err)
			}
			charge, err := billingService.Begin(ctx, billing.BeginRequest{RequestID: "charge-" + request.RequestID, OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: owner.APIKeyID, Protocol: "replicate", Operation: "image.generate", Model: request.Model, ChannelID: request.ChannelID, Quantity: 1, Size: "default", Quality: "default"})
			if err != nil {
				t.Fatal(err)
			}
			request.ChargeID = charge.ID
			providerID := "provider-" + request.RequestID
			provider := &fakeAsyncProvider{submitResult: SubmitResult{ProviderJobID: providerID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Hour}}
			config := jobServiceConfig()
			config.Webhooks = map[string]WebhookConfig{"replicate": {PublicBaseURL: "https://gateway.example", BindingTTL: time.Hour, CallbackSecret: testWebhookCallbackSecret()}}
			service, err := NewService(repository, map[string]Provider{"replicate": provider}, config, "webhook-api")
			if err != nil {
				t.Fatal(err)
			}
			created, err := service.Submit(ctx, request, fakeWebhookPayload{})
			if err != nil {
				t.Fatal(err)
			}
			callback := provider.submitPayload.(fakeWebhookPayload).callback
			token := callback[strings.LastIndex(callback, "/")+1:]
			observation := joboperation.Observation{Status: test.status, ProviderJobID: providerID}
			if test.status != joboperation.Canceled {
				body := []byte(`{"id":"` + created.ID + `","status":"` + strings.ToLower(string(test.status)) + `"}`)
				statusCode := 200
				if test.status == joboperation.Failed {
					statusCode = 500
					observation.FailureCategory = "provider_error"
				}
				observation.Snapshot = joboperation.Snapshot{Status: statusCode, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}
			}
			webhook := WebhookObservation{JobID: created.ID, Provider: "replicate", DeliveryID: "settlement-" + request.RequestID, Token: token, ProviderJobID: providerID, Observation: observation}
			if _, replay, err := service.ApplyWebhook(ctx, webhook); err != nil || replay {
				t.Fatalf("apply replay=%v err=%v", replay, err)
			}
			if _, replay, err := service.ApplyWebhook(ctx, webhook); err != nil || !replay {
				t.Fatalf("duplicate replay=%v err=%v", replay, err)
			}
			if _, err := repository.pool.Exec(ctx, `UPDATE async_jobs SET settlement_next_attempt_at='epoch' WHERE id=$1`, created.ID); err != nil {
				t.Fatal(err)
			}
			workerConfig := jobWorkerConfig()
			workerConfig.BatchSize = 1
			worker, _ := NewWorker(repository, map[string]Provider{"replicate": provider}, billingService, workerConfig, "webhook-worker")
			settlements, err := repository.ClaimSettlements(ctx, "webhook-worker", time.Now(), time.Minute, 1)
			settlement, found := settlementFor(settlements, created.ID)
			if err != nil || !found {
				t.Fatalf("settlement=%+v err=%v", settlement, err)
			}
			if err := worker.settle(ctx, settlement); err != nil {
				t.Fatal(err)
			}
			if err := repository.MarkSettled(ctx, settlement); err != nil {
				t.Fatal(err)
			}
			var available, reserved int64
			if err := repository.pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id=$1`, owner.OrganizationID).Scan(&available, &reserved); err != nil {
				t.Fatal(err)
			}
			var entries int
			if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE reservation_id=$1 AND entry_type=$2`, charge.ReservationID, test.entryType).Scan(&entries); err != nil {
				t.Fatal(err)
			}
			if available != test.expected || reserved != 0 || entries != 1 {
				t.Fatalf("wallet=%d/%d entries=%d", available, reserved, entries)
			}
		})
	}
}

func TestAsyncPartialOutputUsageSettlesExactlyOnce(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	request.Provider = "replicate"
	request.ChannelID = "channel_00000000000000000000000000000004"
	request.EstimatedUsage = &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 3, Provenance: "request", ExtractorVersion: "replicate-input-num_outputs-v1", ResultExtractorVersion: "replicate-output-v1"}
	ctx := context.Background()
	wallet := ledger.NewService(repository.pool)
	if _, err := wallet.Deposit(ctx, owner.OrganizationID, 1000, "partial-deposit-"+request.RequestID); err != nil {
		t.Fatal(err)
	}
	estimator, err := pricing.NewService(repository.pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: request.ChannelID, Protocol: "replicate", Operation: "image.generate", Model: request.Model, Size: "default", Quality: "default", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "partial-price-"+request.RequestID); err != nil {
		t.Fatal(err)
	}
	billingService, err := billing.NewService(repository.pool, estimator, wallet)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := billingService.Begin(ctx, billing.BeginRequest{RequestID: "charge-" + request.RequestID, OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: owner.APIKeyID, Protocol: "replicate", Operation: "image.generate", Model: request.Model, ChannelID: request.ChannelID, Quantity: 3, Size: "default", Quality: "default"})
	if err != nil {
		t.Fatal(err)
	}
	request.ChargeID = charge.ID
	providerID := "provider-" + request.RequestID
	body := []byte(`{"id":"` + request.RequestID + `","status":"succeeded","output":["https://delivery.example/a.png","https://delivery.example/b.png"]}`)
	provider := &fakeAsyncProvider{
		submitResult:    SubmitResult{ProviderJobID: providerID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: time.Hour},
		pollObservation: joboperation.Observation{Status: joboperation.Succeeded, ProviderJobID: providerID, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}, Usage: &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 2, Provenance: "poll", ExtractorVersion: "replicate-output-v1"}},
	}
	service, err := NewService(repository, map[string]Provider{"replicate": provider}, jobServiceConfig(), "partial-api")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Submit(ctx, request, fakeWebhookPayload{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE async_job_provider_attempts SET next_poll_at='epoch' WHERE job_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(repository, map[string]Provider{"replicate": provider}, billingService, jobWorkerConfig(), "partial-worker")
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.RunOnce(ctx)
	if err != nil || result.Observed != 1 || result.Settled < 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored, err := repository.Get(ctx, owner, created.ID)
	if err != nil || stored.ActualUsage == nil || stored.ActualUsage.Quantity != 2 || stored.EstimatedUsage == nil || stored.EstimatedUsage.Quantity != 3 || stored.SettlementState != "SETTLED" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	var available, reserved int64
	if err := repository.pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id=$1`, owner.OrganizationID).Scan(&available, &reserved); err != nil || available != 800 || reserved != 0 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
	var evidence, captures int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM async_job_usage_evidence WHERE job_id=$1 AND quantity=2`, created.ID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE reservation_id=$1 AND entry_type='capture'`, charge.ReservationID).Scan(&captures); err != nil {
		t.Fatal(err)
	}
	if evidence != 1 || captures != 1 {
		t.Fatalf("evidence=%d captures=%d", evidence, captures)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE async_jobs SET estimated_quantity=1 WHERE id=$1`, created.ID); err == nil {
		t.Fatal("immutable estimate was updated")
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE async_job_usage_evidence SET quantity=1 WHERE job_id=$1`, created.ID); err == nil {
		t.Fatal("append-only usage evidence was updated")
	}
	second, err := worker.RunOnce(ctx)
	if err != nil || second.Settled != 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestUnsafeAsyncUsageHoldsReservationWithoutSettlement(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		status       joboperation.Status
		quantity     int64
	}{{"unknown", "usage_unknown", joboperation.Succeeded, 0}, {"exceeds estimate", "usage_exceeds_estimate", joboperation.Succeeded, 11}, {"failed with output", "partial_terminal_conflict", joboperation.Failed, 1}} {
		t.Run(test.name, func(t *testing.T) {
			repository, owner, request := jobRepositoryFixture(t)
			request.Provider = "replicate"
			request.ChannelID = "channel_00000000000000000000000000000004"
			request.EstimatedUsage = &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 3, Provenance: "request", ExtractorVersion: "replicate-input-num_outputs-v1", ResultExtractorVersion: "replicate-output-v1"}
			ctx := context.Background()
			wallet := ledger.NewService(repository.pool)
			if _, err := wallet.Deposit(ctx, owner.OrganizationID, 1000, "unsafe-deposit-"+request.RequestID); err != nil {
				t.Fatal(err)
			}
			estimator, _ := pricing.NewService(repository.pool, 0)
			if _, err := estimator.Publish(ctx, pricing.Price{ChannelID: request.ChannelID, Protocol: "replicate", Operation: "image.generate", Model: request.Model, Size: "default", Quality: "default", UnitCost: 60, UnitSale: 100, EffectiveFrom: time.Now().Add(-time.Hour)}, "unsafe-price-"+request.RequestID); err != nil {
				t.Fatal(err)
			}
			billingService, _ := billing.NewService(repository.pool, estimator, wallet)
			charge, err := billingService.Begin(ctx, billing.BeginRequest{RequestID: "unsafe-charge-" + request.RequestID, OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: owner.APIKeyID, Protocol: "replicate", Operation: "image.generate", Model: request.Model, ChannelID: request.ChannelID, Quantity: 3, Size: "default", Quality: "default"})
			if err != nil {
				t.Fatal(err)
			}
			request.ChargeID = charge.ID
			created, _, err := repository.Create(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := repository.BeginSubmit(ctx, owner, created.ID, "unsafe-submit", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			providerID := "provider-" + request.RequestID
			if _, err := repository.ConfirmSubmit(ctx, owner, attempt, providerID, joboperation.Queued, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"status":"terminal"}`)
			observation := joboperation.Observation{Status: test.status, ProviderJobID: providerID, Usage: &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: test.quantity, Provenance: "poll", ExtractorVersion: "replicate-output-v1"}}
			if test.status != joboperation.Canceled {
				observation.Snapshot = joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}
			}
			stored, err := repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, AttemptNo: 1, Provider: "replicate", ChannelID: request.ChannelID, ProviderJobID: providerID, State: "SUBMITTED"}}, observation, "poll", time.Time{})
			if err != nil || stored.SettlementState != "MANUAL_REVIEW" || stored.UsageReconciliationReason != test.reason {
				t.Fatalf("stored=%+v err=%v", stored, err)
			}
			var available, reserved int64
			if err := repository.pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id=$1`, owner.OrganizationID).Scan(&available, &reserved); err != nil || available != 700 || reserved != 300 {
				t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
			}
			var terminalEntries int
			if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE reservation_id=$1 AND entry_type IN ('capture','release')`, charge.ReservationID).Scan(&terminalEntries); err != nil || terminalEntries != 0 {
				t.Fatalf("terminal entries=%d err=%v", terminalEntries, err)
			}
		})
	}
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
