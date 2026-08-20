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
	request := CreateRequest{RequestID: "request-" + suffix, Owner: owner, Protocol: "replicate", Operation: "image.generate", Model: "owner/model", Provider: "openai", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: "idem-" + suffix, Fingerprint: fingerprint}
	return repository, owner, request
}

func TestCreateIsIdempotentAndTenantIsolated(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
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
	if _, err := repository.BeginSubmit(ctx, owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginSubmit(ctx, owner, created.ID); !errors.Is(err, joboperation.ErrConflict) {
		t.Fatalf("duplicate submit=%v", err)
	}
	if _, err := repository.ConfirmSubmit(ctx, owner, created.ID, "provider-internal-id", joboperation.Queued, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	leases, err := repository.ClaimDue(ctx, "worker-a", time.Now(), time.Minute, 10)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases=%+v err=%v", leases, err)
	}
	body := []byte(`{"status":"succeeded"}`)
	observation := joboperation.Observation{Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}}
	terminal, err := repository.ApplyObservation(ctx, leases[0], observation, "poll")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != joboperation.Succeeded || terminal.SettlementState != "PENDING" || string(terminal.Snapshot.Body) != string(body) {
		t.Fatalf("terminal=%+v", terminal)
	}
	if _, err := repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, AttemptNo: 1}}, observation, "webhook"); err != nil {
		t.Fatalf("duplicate observation=%v", err)
	}
	if _, err := repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, AttemptNo: 1}}, joboperation.Observation{Status: joboperation.Failed, Snapshot: observation.Snapshot}, "webhook"); !errors.Is(err, joboperation.ErrConflict) {
		t.Fatalf("terminal conflict=%v", err)
	}
	if len(terminal.Snapshot.Headers) != 1 {
		t.Fatalf("headers=%+v", terminal.Snapshot.Headers)
	}
}

func TestExpiredLeaseIsRecoveredWithoutConcurrentClaim(t *testing.T) {
	repository, owner, request := jobRepositoryFixture(t)
	ctx := context.Background()
	created, _, _ := repository.Create(ctx, request)
	_, _ = repository.BeginSubmit(ctx, owner, created.ID)
	_, _ = repository.ConfirmSubmit(ctx, owner, created.ID, "provider-recovery", joboperation.Processing, time.Now().Add(-time.Second))
	now := time.Now()
	first, err := repository.ClaimDue(ctx, "worker-a", now, 50*time.Millisecond, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := repository.ClaimDue(ctx, "worker-b", now.Add(10*time.Millisecond), time.Minute, 1)
	if err != nil || len(second) != 0 {
		t.Fatalf("concurrent=%+v err=%v", second, err)
	}
	recovered, err := repository.ClaimDue(ctx, "worker-b", now.Add(time.Second), time.Minute, 1)
	if err != nil || len(recovered) != 1 || recovered[0].PollCount != 2 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if err := repository.Reschedule(ctx, first[0], time.Now().Add(time.Minute), "timeout"); !errors.Is(err, joboperation.ErrLeaseLost) {
		t.Fatalf("stale lease reschedule=%v", err)
	}
}
