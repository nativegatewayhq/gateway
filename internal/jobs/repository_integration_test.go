//go:build integration

package jobs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/database"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

func jobRepositoryFixture(t *testing.T) (*Repository, joboperation.Owner, CreateRequest) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	pool, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name()+time.Now().String())))[:12]
	owner := joboperation.Owner{OrganizationID: "org_job_" + suffix, ProjectID: "project_job_" + suffix, APIKeyID: "key_job_" + suffix}
	digest := sha256.Sum256([]byte(suffix))
	_, err = pool.Exec(context.Background(), `INSERT INTO organizations(id,name,slug) VALUES($1,'Async job',$2)`, owner.OrganizationID, "job-"+suffix)
	if err == nil {
		_, err = pool.Exec(context.Background(), `INSERT INTO projects(id,organization_id,name,slug) VALUES($1,$2,'Async job',$3)`, owner.ProjectID, owner.OrganizationID, "job-"+suffix)
	}
	if err == nil {
		_, err = pool.Exec(context.Background(), `INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES($1,'Async job',$2,'ngw_job',$3)`, owner.APIKeyID, digest[:], owner.ProjectID)
	}
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewDefaultRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256([]byte("stable request"))
	request := CreateRequest{RequestID: "request-" + suffix, Owner: owner, Protocol: "replicate", Operation: "image.generate", Model: "owner/model-" + suffix, Provider: "openai", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: "idem-" + suffix, Fingerprint: fingerprint}
	return repository, owner, request
}

func testWebhookCallbackSecret() []byte { return []byte("0123456789abcdef0123456789abcdef") }

func TestCreateIsIdempotentAndTenantIsolated(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	if err := repository.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	results := make(chan joboperation.Job, workers)
	failures := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			item, _, err := repository.Create(ctx, request)
			if err != nil {
				failures <- err
				return
			}
			results <- item
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var id string
	for item := range results {
		if id == "" {
			id = item.ID
		}
		if item.ID != id {
			t.Fatalf("ids=%s/%s", id, item.ID)
		}
	}
	if _, err := repository.Get(ctx, joboperation.Owner{OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: "key_other"}, id); !errors.Is(err, joboperation.ErrNotFound) {
		t.Fatalf("cross-tenant get=%v", err)
	}
	if _, _, _, err := repository.ClaimCancel(ctx, joboperation.Owner{OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, APIKeyID: "key_other"}, id, "canceler", time.Minute); !errors.Is(err, joboperation.ErrNotFound) {
		t.Fatalf("cross-tenant cancel=%v", err)
	}
	conflict := request
	conflict.Model = "other/model"
	if _, _, err := repository.Create(ctx, conflict); !errors.Is(err, joboperation.ErrConflict) {
		t.Fatalf("conflict=%v", err)
	}
}

func TestSubmitClaimObservationAndTerminalReplay(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	created, _, err := repository.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute); !errors.Is(err, joboperation.ErrConflict) {
		t.Fatalf("duplicate submit=%v", err)
	}
	if _, err := repository.ConfirmSubmit(ctx, owner, attempt, "provider-"+request.RequestID, joboperation.Queued, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	leases, err := repository.ClaimDue(ctx, "worker-a", time.Now(), time.Minute, 100)
	lease, found := leaseFor(leases, created.ID)
	if err != nil || !found {
		t.Fatalf("leases=%+v err=%v", leases, err)
	}
	body := []byte(`{"status":"succeeded"}`)
	observation := joboperation.Observation{Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}}
	terminal, err := repository.ApplyObservation(ctx, lease, observation, "poll", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != joboperation.Succeeded || terminal.SettlementState != "PENDING" || string(terminal.Snapshot.Body) != string(body) {
		t.Fatalf("terminal=%+v", terminal)
	}
	if _, err := repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, AttemptNo: 1}}, observation, "webhook", time.Time{}); err != nil {
		t.Fatalf("duplicate observation=%v", err)
	}
	if _, err := repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, AttemptNo: 1}}, joboperation.Observation{Status: joboperation.Failed, Snapshot: observation.Snapshot}, "webhook", time.Time{}); !errors.Is(err, joboperation.ErrConflict) {
		t.Fatalf("terminal conflict=%v", err)
	}
	if len(terminal.Snapshot.Headers) != 1 {
		t.Fatalf("headers=%+v", terminal.Snapshot.Headers)
	}
	settlements, err := repository.ClaimSettlements(ctx, "settler-a", time.Now(), time.Minute, 100)
	settlement, found := settlementFor(settlements, created.ID)
	if err != nil || !found {
		t.Fatalf("settlements=%+v err=%v", settlements, err)
	}
	if err := repository.MarkSettled(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkSettled(ctx, settlement); err != nil {
		t.Fatalf("idempotent settlement=%v", err)
	}
	stored, err := repository.Get(ctx, owner, created.ID)
	if err != nil || stored.SettlementState != "SETTLED" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE async_jobs SET provider='changed' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("immutable job identity updated")
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE async_job_events SET source='api' WHERE job_id=$1`, created.ID); err == nil {
		t.Fatal("append-only event updated")
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM async_jobs WHERE id=$1`, created.ID); err == nil {
		t.Fatal("durable job deleted")
	}
}

func TestExpiredLeaseIsRecoveredWithoutConcurrentClaim(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	created, _, _ := repository.Create(ctx, request)
	attempt, _ := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute)
	_, _ = repository.ConfirmSubmit(ctx, owner, attempt, "provider-"+request.RequestID, joboperation.Processing, time.Now().Add(-24*time.Hour))
	now := time.Now()
	claimed, err := repository.ClaimDue(ctx, "worker-a", now, 50*time.Millisecond, 100)
	first, found := leaseFor(claimed, created.ID)
	if err != nil || !found {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	extended, err := repository.Heartbeat(ctx, first, now.Add(100*time.Millisecond))
	if err != nil || !extended.LeaseUntil.Equal(now.Add(100*time.Millisecond)) {
		t.Fatalf("heartbeat=%+v err=%v", extended, err)
	}
	first = extended
	second, err := repository.ClaimDue(ctx, "worker-b", now.Add(10*time.Millisecond), time.Minute, 100)
	_, concurrentlyClaimed := leaseFor(second, created.ID)
	if err != nil || concurrentlyClaimed {
		t.Fatalf("concurrent=%+v err=%v", second, err)
	}
	recoveredRows, err := repository.ClaimDue(ctx, "worker-b", now.Add(time.Second), time.Minute, 100)
	recovered, found := leaseFor(recoveredRows, created.ID)
	if err != nil || !found || recovered.PollCount != 2 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if err := repository.Reschedule(ctx, first, time.Now().Add(time.Minute), "timeout"); !errors.Is(err, joboperation.ErrLeaseLost) {
		t.Fatalf("stale lease reschedule=%v", err)
	}
}

func TestStaleSubmittingLeaseBecomesRecoverableWithoutSecondSubmit(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	created, _, err := repository.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.BeginSubmit(ctx, owner, created.ID, "crashed-submitter", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	claimed, err := repository.ClaimDue(ctx, "recovery-worker", time.Now(), time.Minute, 100)
	lease, found := leaseFor(claimed, created.ID)
	if err != nil || !found || lease.State != "SUBMITTING" || lease.ProviderJobID != "" {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	recovered, err := repository.ApplyObservation(ctx, lease, joboperation.Observation{Status: joboperation.Processing, ProviderJobID: "recovered-" + request.RequestID}, "reconciliation", time.Now().Add(time.Minute))
	if err != nil || recovered.Status != joboperation.Processing {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if _, err := repository.BeginSubmit(ctx, owner, created.ID, "second-submitter", time.Minute); !errors.Is(err, joboperation.ErrConflict) {
		t.Fatalf("second submit=%v", err)
	}
}

func TestConcurrentConflictingTerminalObservationsCreateOneWinner(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	created, _, _ := repository.Create(ctx, request)
	attempt, _ := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute)
	_, _ = repository.ConfirmSubmit(ctx, owner, attempt, "provider-"+request.RequestID, joboperation.Processing, time.Now().Add(time.Hour))
	success := joboperation.Observation{Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{}, Body: []byte("success")}}
	failure := joboperation.Observation{Status: joboperation.Failed, Snapshot: joboperation.Snapshot{Status: 500, Headers: map[string][]string{}, Body: []byte("failure")}}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, observation := range []joboperation.Observation{success, failure} {
		go func(value joboperation.Observation) {
			<-start
			_, err := repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, AttemptNo: 1}}, value, "webhook", time.Time{})
			results <- err
		}(observation)
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("results=%v/%v", first, second)
	}
	loser := first
	if loser == nil {
		loser = second
	}
	if !errors.Is(loser, joboperation.ErrConflict) {
		t.Fatalf("loser=%v", loser)
	}
	var terminalEvents int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM async_job_events WHERE job_id=$1 AND event_type='OBSERVED'`, created.ID).Scan(&terminalEvents); err != nil || terminalEvents != 1 {
		t.Fatalf("events=%d err=%v", terminalEvents, err)
	}
}

func TestSignedWebhookBindingReplayAndProviderIdentity(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	request.Provider = "replicate"
	request.ChannelID = "channel_00000000000000000000000000000004"
	ctx := context.Background()
	created, _, err := repository.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	callbackSecret := testWebhookCallbackSecret()
	binding, err := repository.CreateWebhookBinding(ctx, created.ID, "replicate", request.ChannelID, callbackSecret, time.Hour)
	if err != nil || binding.Token == "" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	providerID := "provider-" + request.RequestID
	earlyBody := []byte(`{"id":"gateway-job","status":"succeeded"}`)
	earlyObservation := joboperation.Observation{Status: joboperation.Succeeded, ProviderJobID: providerID, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: earlyBody, SHA256: sha256.Sum256(earlyBody)}}
	if _, _, err := repository.ApplyWebhook(ctx, WebhookObservation{JobID: created.ID, Provider: "replicate", DeliveryID: "early-" + request.RequestID, Token: binding.Token, ProviderJobID: providerID, Observation: earlyObservation, CallbackSecret: callbackSecret}); !errors.Is(err, ErrWebhookNotReady) {
		t.Fatalf("early webhook=%v", err)
	}
	if _, err := repository.ConfirmSubmit(ctx, owner, attempt, providerID, joboperation.Processing, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"gateway-job","status":"succeeded"}`)
	observation := joboperation.Observation{Status: joboperation.Succeeded, ProviderJobID: providerID, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}}
	deliveryID := "delivery-" + request.RequestID
	requestWebhook := WebhookObservation{JobID: created.ID, Provider: "replicate", DeliveryID: deliveryID, Token: binding.Token, ProviderJobID: providerID, Observation: observation, CallbackSecret: callbackSecret}
	terminal, replay, err := repository.ApplyWebhook(ctx, requestWebhook)
	if err != nil || replay || terminal.Status != joboperation.Succeeded || terminal.SettlementState != "PENDING" {
		t.Fatalf("terminal=%+v replay=%v err=%v", terminal, replay, err)
	}
	replayed, replay, err := repository.ApplyWebhook(ctx, requestWebhook)
	if err != nil || !replay || replayed.ID != created.ID {
		t.Fatalf("replayed=%+v replay=%v err=%v", replayed, replay, err)
	}
	wrong := requestWebhook
	wrong.DeliveryID = deliveryID + "-wrong"
	wrong.Token = "whk_ffffffffffffffffffffffffffffffff"
	if _, _, err := repository.ApplyWebhook(ctx, wrong); !errors.Is(err, ErrWebhookRejected) {
		t.Fatalf("wrong token=%v", err)
	}
	wrong.Token = binding.Token
	wrong.ProviderJobID = "other-provider-id"
	wrong.Observation.ProviderJobID = "other-provider-id"
	if _, _, err := repository.ApplyWebhook(ctx, wrong); !errors.Is(err, ErrWebhookRejected) {
		t.Fatalf("wrong provider identity=%v", err)
	}
	repository.now = func() time.Time { return binding.ExpiresAt.Add(time.Second) }
	expired := requestWebhook
	expired.DeliveryID = deliveryID + "-expired"
	if _, _, err := repository.ApplyWebhook(ctx, expired); !errors.Is(err, ErrWebhookRejected) {
		t.Fatalf("expired capability=%v", err)
	}
	var deliveries, observed int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM async_job_webhook_deliveries WHERE job_id=$1`, created.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM async_job_events WHERE job_id=$1 AND source='webhook' AND event_type='OBSERVED'`, created.ID).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || observed != 1 {
		t.Fatalf("deliveries=%d observed=%d", deliveries, observed)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM async_job_webhook_deliveries WHERE provider='replicate' AND delivery_id=$1`, deliveryID); err == nil {
		t.Fatal("append-only webhook delivery deleted")
	}
}

func TestWebhookAndPollRaceConvergesOnOneTerminalEvent(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	request.Provider = "replicate"
	request.ChannelID = "channel_00000000000000000000000000000004"
	ctx := context.Background()
	created, _, err := repository.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	callbackSecret := testWebhookCallbackSecret()
	binding, err := repository.CreateWebhookBinding(ctx, created.ID, "replicate", request.ChannelID, callbackSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	providerID := "provider-" + request.RequestID
	if _, err := repository.ConfirmSubmit(ctx, owner, attempt, providerID, joboperation.Processing, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	leases, err := repository.ClaimDue(ctx, "poller", time.Now(), time.Minute, 100)
	pollLease, found := leaseFor(leases, created.ID)
	if err != nil || !found {
		t.Fatalf("lease=%+v err=%v", pollLease, err)
	}
	body := []byte(`{"id":"gateway-job","status":"succeeded"}`)
	observation := joboperation.Observation{Status: joboperation.Succeeded, ProviderJobID: providerID, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}}
	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	go func() {
		<-start
		_, err := repository.ApplyObservation(ctx, pollLease, observation, "poll", time.Time{})
		errorsFound <- err
	}()
	go func() {
		<-start
		_, _, err := repository.ApplyWebhook(ctx, WebhookObservation{JobID: created.ID, Provider: "replicate", DeliveryID: "race-" + request.RequestID, Token: binding.Token, ProviderJobID: providerID, Observation: observation, CallbackSecret: callbackSecret})
		errorsFound <- err
	}()
	close(start)
	first, second := <-errorsFound, <-errorsFound
	for _, err := range []error{first, second} {
		if err != nil && !errors.Is(err, joboperation.ErrLeaseLost) {
			t.Fatalf("race error=%v (%v/%v)", err, first, second)
		}
	}
	stored, err := repository.Get(ctx, owner, created.ID)
	if err != nil || stored.Status != joboperation.Succeeded || stored.SettlementState != "PENDING" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	var observed int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM async_job_events WHERE job_id=$1 AND event_type='OBSERVED'`, created.ID).Scan(&observed); err != nil || observed != 1 {
		t.Fatalf("observed=%d err=%v", observed, err)
	}
}

func TestCancelIsAttemptedOnceAndConvergesWithPolling(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	created, _, err := repository.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repository.BeginSubmit(ctx, owner, created.ID, "submitter", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ConfirmSubmit(ctx, owner, attempt, "provider-"+request.RequestID, joboperation.Processing, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, cancelLease, claimed, err := repository.ClaimCancel(ctx, owner, created.ID, "canceler", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%+v/%v err=%v", cancelLease, claimed, err)
	}
	if _, _, claimed, err := repository.ClaimCancel(ctx, owner, created.ID, "other", time.Minute); err != nil || claimed {
		t.Fatalf("duplicate cancel claimed=%v err=%v", claimed, err)
	}
	canceled, err := repository.ApplyObservation(ctx, cancelLease, joboperation.Observation{Status: joboperation.Canceled}, "cancel", time.Time{})
	if err != nil || canceled.Status != joboperation.Canceled || canceled.SettlementState != "PENDING" {
		t.Fatalf("canceled=%+v err=%v", canceled, err)
	}
}

func leaseFor(leases []Lease, id string) (Lease, bool) {
	for _, lease := range leases {
		if lease.JobID == id {
			return lease, true
		}
	}
	return Lease{}, false
}

func settlementFor(leases []SettlementLease, id string) (SettlementLease, bool) {
	for _, lease := range leases {
		if lease.Job.ID == id {
			return lease, true
		}
	}
	return SettlementLease{}, false
}
