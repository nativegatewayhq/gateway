package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type Settler interface {
	Complete(context.Context, string, bool, billing.ResponseSnapshot) (billing.Charge, error)
}

type WorkerConfig struct {
	Interval, Lease, PollDelay, BaseBackoff, MaximumBackoff time.Duration
	BatchSize, MaximumAttempts                              int
}
type RunResult struct{ Polled, Observed, Retried, Settled, Manual int }
type Worker struct {
	repository *Repository
	providers  map[string]Provider
	settler    Settler
	config     WorkerConfig
	owner      string
	now        func() time.Time
	telemetry  *telemetry.Recorder
}

func (worker *Worker) SetTelemetry(recorder *telemetry.Recorder) { worker.telemetry = recorder }

func NewWorker(repository *Repository, providers map[string]Provider, settler Settler, config WorkerConfig, owner string) (*Worker, error) {
	if repository == nil || len(providers) == 0 || config.Interval <= 0 || config.Lease <= 0 || config.PollDelay <= 0 || config.BaseBackoff <= 0 || config.MaximumBackoff < config.BaseBackoff || config.BatchSize < 1 || config.BatchSize > 100 || config.MaximumAttempts < 1 || config.MaximumAttempts > 100 || owner == "" || len(owner) > 128 {
		return nil, joboperation.ErrInvalid
	}
	copyProviders := make(map[string]Provider, len(providers))
	for name, provider := range providers {
		if name == "" || provider == nil {
			return nil, joboperation.ErrInvalid
		}
		copyProviders[name] = provider
	}
	return &Worker{repository: repository, providers: copyProviders, settler: settler, config: config, owner: owner, now: time.Now}, nil
}

func (worker *Worker) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(worker.config.Interval)
	defer ticker.Stop()
	for {
		if _, err := worker.RunOnce(ctx); err != nil && onError != nil && !errors.Is(err, context.Canceled) {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *Worker) RunOnce(ctx context.Context) (RunResult, error) {
	at := worker.now().UTC()
	result := RunResult{}
	leases, err := worker.repository.ClaimDue(ctx, worker.owner, at, worker.config.Lease, worker.config.BatchSize)
	if err != nil {
		return result, err
	}
	for _, lease := range leases {
		result.Polled++
		provider := worker.providers[lease.Provider]
		if provider == nil {
			if err := worker.retryPoll(ctx, lease, "provider_unavailable"); err != nil {
				return result, err
			}
			if lease.PollCount >= worker.config.MaximumAttempts {
				result.Manual++
			} else {
				result.Retried++
			}
			continue
		}
		observation, pollErr := provider.Poll(ctx, lease.ProviderAttempt)
		if pollErr != nil {
			if err := worker.retryPoll(ctx, lease, providerErrorCategory(pollErr)); err != nil {
				return result, err
			}
			if lease.PollCount >= worker.config.MaximumAttempts {
				result.Manual++
			} else {
				result.Retried++
			}
			continue
		}
		next := time.Time{}
		if !observation.Status.Terminal() {
			if observation.Status == "" {
				observation.Status = joboperation.Processing
			}
			next = at.Add(worker.config.PollDelay)
		}
		if _, err := worker.repository.ApplyObservation(ctx, lease, observation, "poll", next); err != nil {
			return result, err
		}
		result.Observed++
		worker.record(ctx, "poll", observation.Status, "success")
	}
	settlements, err := worker.repository.ClaimSettlements(ctx, worker.owner, at, worker.config.Lease, worker.config.BatchSize)
	if err != nil {
		return result, err
	}
	for _, lease := range settlements {
		if err := worker.settle(ctx, lease); err != nil {
			if lease.Attempt >= worker.config.MaximumAttempts {
				if manualErr := worker.repository.MarkSettlementManual(ctx, lease, "settlement_failed"); manualErr != nil {
					return result, manualErr
				}
				result.Manual++
			} else {
				if retryErr := worker.repository.RescheduleSettlement(ctx, lease, at.Add(backoff(worker.config.BaseBackoff, worker.config.MaximumBackoff, lease.Attempt)), "settlement_failed"); retryErr != nil {
					return result, retryErr
				}
				result.Retried++
			}
			continue
		}
		if err := worker.repository.MarkSettled(ctx, lease); err != nil {
			return result, err
		}
		result.Settled++
		worker.record(ctx, "settlement", lease.Job.Status, "success")
	}
	return result, nil
}

func (worker *Worker) record(ctx context.Context, stage string, status joboperation.Status, outcome string) {
	if worker.telemetry != nil {
		worker.telemetry.Job(ctx, telemetry.JobRecord{Protocol: "gateway", Stage: stage, Status: string(status), Outcome: outcome})
	}
}

func (worker *Worker) retryPoll(ctx context.Context, lease Lease, category string) error {
	if lease.PollCount >= worker.config.MaximumAttempts {
		return worker.repository.MarkPollManual(ctx, lease, category)
	}
	return worker.repository.Reschedule(ctx, lease, worker.now().UTC().Add(backoff(worker.config.BaseBackoff, worker.config.MaximumBackoff, lease.PollCount)), category)
}
func (worker *Worker) settle(ctx context.Context, lease SettlementLease) error {
	if lease.Job.ChargeID == "" {
		return nil
	}
	if worker.settler == nil {
		return errors.New("job settlement unavailable")
	}
	snapshot := billing.ResponseSnapshot{Status: lease.Job.Snapshot.Status, Headers: lease.Job.Snapshot.Headers, Body: lease.Job.Snapshot.Body}
	if lease.Job.Status == joboperation.Canceled {
		snapshot = billing.ResponseSnapshot{Status: 204, Headers: map[string][]string{}, Body: []byte{}}
	}
	_, err := worker.settler.Complete(ctx, lease.Job.ChargeID, lease.Job.Status == joboperation.Succeeded, snapshot)
	return err
}
func providerErrorCategory(err error) string {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return boundedProviderCategory(providerErr.Category)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "provider_error"
}
func backoff(base, maximum time.Duration, attempt int) time.Duration {
	delay := base
	for index := 1; index < attempt && delay < maximum; index++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
