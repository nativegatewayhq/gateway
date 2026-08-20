package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/nativegatewayhq/gateway/internal/telemetry"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

var ErrProviderUnavailable = errors.New("asynchronous provider unavailable")

type ProviderError struct {
	Category    string
	Known       bool
	Observation joboperation.Observation
}

func (err *ProviderError) Error() string { return "asynchronous provider operation failed" }

type SubmitResult struct {
	ProviderJobID string
	Observation   joboperation.Observation
	PollAfter     time.Duration
}

type Provider interface {
	Submit(context.Context, joboperation.Job, any) (SubmitResult, error)
	Poll(context.Context, ProviderAttempt) (joboperation.Observation, error)
	Cancel(context.Context, ProviderAttempt) (joboperation.Observation, error)
}

type ServiceConfig struct {
	SubmitLease time.Duration
	PollDelay   time.Duration
}

type Service struct {
	repository *Repository
	providers  map[string]Provider
	config     ServiceConfig
	owner      string
	telemetry  *telemetry.Recorder
}

func (service *Service) SetTelemetry(recorder *telemetry.Recorder) { service.telemetry = recorder }

func NewService(repository *Repository, providers map[string]Provider, config ServiceConfig, owner string) (*Service, error) {
	if repository == nil || len(providers) == 0 || config.SubmitLease <= 0 || config.PollDelay <= 0 || owner == "" || len(owner) > 128 {
		return nil, joboperation.ErrInvalid
	}
	copyProviders := make(map[string]Provider, len(providers))
	for name, provider := range providers {
		if name == "" || provider == nil {
			return nil, joboperation.ErrInvalid
		}
		copyProviders[name] = provider
	}
	return &Service{repository: repository, providers: copyProviders, config: config, owner: owner}, nil
}

func (service *Service) Submit(ctx context.Context, request CreateRequest, payload any) (joboperation.Job, error) {
	created, replay, err := service.repository.Create(ctx, request)
	if err != nil {
		return joboperation.Job{}, err
	}
	if replay && created.Status != joboperation.Pending {
		return created, nil
	}
	provider := service.providers[created.Provider]
	if provider == nil {
		return joboperation.Job{}, ErrProviderUnavailable
	}
	attempt, err := service.repository.BeginSubmit(ctx, created.Owner, created.ID, service.owner, service.config.SubmitLease)
	if err != nil {
		if errors.Is(err, joboperation.ErrConflict) {
			return service.repository.Get(ctx, created.Owner, created.ID)
		}
		return joboperation.Job{}, err
	}
	result, submitErr := provider.Submit(ctx, created, payload)
	if submitErr != nil {
		return service.handleSubmitError(ctx, created, attempt, submitErr)
	}
	status := result.Observation.Status
	if status == "" {
		status = joboperation.Queued
	}
	pollAfter := result.PollAfter
	if pollAfter <= 0 {
		pollAfter = service.config.PollDelay
	}
	if status != joboperation.Queued && status != joboperation.Processing && !status.Terminal() {
		return service.handleSubmitError(ctx, created, attempt, &ProviderError{Category: "invalid_response"})
	}
	if result.ProviderJobID == "" {
		return service.handleSubmitError(ctx, created, attempt, &ProviderError{Category: "missing_provider_job_id"})
	}
	initial := status
	if status.Terminal() {
		initial = joboperation.Processing
	}
	confirmed, err := service.repository.ConfirmSubmit(ctx, created.Owner, attempt, result.ProviderJobID, initial, time.Now().Add(pollAfter))
	if err != nil {
		return joboperation.Job{}, err
	}
	if !status.Terminal() {
		if result.Observation.Snapshot.Status != 0 {
			confirmed, err = service.repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, Model: created.Model, AttemptNo: 1, Provider: created.Provider, ChannelID: created.ChannelID, ProviderJobID: result.ProviderJobID, State: "SUBMITTED"}}, result.Observation, "submit", time.Now().Add(pollAfter))
			if err != nil {
				return joboperation.Job{}, err
			}
		}
		service.record(ctx, created.Protocol, "submit", status, "success")
		return confirmed, nil
	}
	terminal, err := service.repository.ApplyObservation(ctx, Lease{ProviderAttempt: ProviderAttempt{JobID: created.ID, Model: created.Model, AttemptNo: 1, Provider: created.Provider, ChannelID: created.ChannelID, ProviderJobID: result.ProviderJobID, State: "SUBMITTED"}}, result.Observation, "submit", time.Time{})
	if err == nil {
		service.record(ctx, created.Protocol, "submit", terminal.Status, "success")
	}
	return terminal, err
}

func (service *Service) handleSubmitError(ctx context.Context, created joboperation.Job, attempt ProviderAttempt, submitErr error) (joboperation.Job, error) {
	var providerErr *ProviderError
	if errors.As(submitErr, &providerErr) && providerErr.Known {
		observation := providerErr.Observation
		if observation.Status == "" {
			observation.Status = joboperation.Failed
		}
		if observation.FailureCategory == "" {
			observation.FailureCategory = boundedProviderCategory(providerErr.Category)
		}
		terminal, err := service.repository.ApplyObservation(ctx, Lease{ProviderAttempt: attempt}, observation, "submit", time.Time{})
		if err == nil {
			service.record(ctx, created.Protocol, "submit", terminal.Status, "failure")
		}
		return terminal, err
	}
	_, err := service.repository.MarkSubmitUnknown(ctx, created.Owner, attempt, time.Now().Add(service.config.PollDelay))
	if err != nil {
		return joboperation.Job{}, err
	}
	result, getErr := service.repository.Get(ctx, created.Owner, created.ID)
	if getErr == nil {
		service.record(ctx, created.Protocol, "submit", result.Status, "failure")
	}
	return result, getErr
}

func (service *Service) Get(ctx context.Context, owner joboperation.Owner, id string) (joboperation.Job, error) {
	return service.repository.Get(ctx, owner, id)
}

func (service *Service) Cancel(ctx context.Context, owner joboperation.Owner, id string) (joboperation.Job, error) {
	current, lease, claimed, err := service.repository.ClaimCancel(ctx, owner, id, service.owner, service.config.SubmitLease)
	if err != nil || !claimed {
		return current, err
	}
	provider := service.providers[lease.Provider]
	if provider == nil {
		_, applyErr := service.repository.ApplyObservation(ctx, lease, joboperation.Observation{Status: joboperation.Reconciling, FailureCategory: "provider_unavailable"}, "cancel", time.Now().Add(service.config.PollDelay))
		if applyErr != nil {
			return joboperation.Job{}, applyErr
		}
		return service.repository.Get(ctx, owner, id)
	}
	observation, cancelErr := provider.Cancel(ctx, lease.ProviderAttempt)
	if cancelErr != nil {
		observation = joboperation.Observation{Status: joboperation.Reconciling, FailureCategory: "cancel_unknown"}
	}
	next := time.Time{}
	if !observation.Status.Terminal() {
		if observation.Status == "" {
			observation.Status = joboperation.Reconciling
		}
		next = time.Now().Add(service.config.PollDelay)
	}
	result, err := service.repository.ApplyObservation(ctx, lease, observation, "cancel", next)
	if err == nil {
		outcome := "success"
		if !result.Status.Terminal() {
			outcome = "failure"
		}
		service.record(ctx, result.Protocol, "cancel", result.Status, outcome)
	}
	return result, err
}

func (service *Service) record(ctx context.Context, protocol, stage string, status joboperation.Status, outcome string) {
	if service.telemetry != nil {
		service.telemetry.Job(ctx, telemetry.JobRecord{Protocol: protocol, Stage: stage, Status: string(status), Outcome: outcome})
	}
}

func boundedProviderCategory(value string) string {
	switch value {
	case "rejected", "invalid_request", "rate_limited", "unavailable", "timeout", "connection", "invalid_response", "missing_provider_job_id":
		return value
	default:
		return "provider_error"
	}
}
