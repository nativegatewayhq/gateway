// Package job defines protocol-neutral durable asynchronous job contracts.
package job

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Status string

const (
	Pending     Status = "PENDING"
	Queued      Status = "QUEUED"
	Processing  Status = "PROCESSING"
	Succeeded   Status = "SUCCEEDED"
	Failed      Status = "FAILED"
	Canceled    Status = "CANCELED"
	Reconciling Status = "RECONCILING"
)

var (
	ErrInvalid      = errors.New("invalid asynchronous job")
	ErrInvalidState = errors.New("invalid asynchronous job state transition")
	ErrConflict     = errors.New("asynchronous job conflict")
	ErrNotFound     = errors.New("asynchronous job not found")
	ErrLeaseLost    = errors.New("asynchronous job lease lost")
	idPattern       = regexp.MustCompile(`^job_[a-f0-9]{32}$`)
)

const MaximumObservedUsage = int64(128)

type Owner struct {
	OrganizationID string
	ProjectID      string
	APIKeyID       string
}

type Snapshot struct {
	Status  int
	Headers map[string][]string
	Body    []byte
	SHA256  [32]byte
}

// Usage is bounded, content-free billing evidence. Estimate usage is stored on
// Job creation; verified actual usage is supplied only by a Provider adapter.
type Usage struct {
	Dimension, Unit, Provenance, ExtractorVersion, ResultExtractorVersion string
	Quantity                                                              int64
}

type Job struct {
	ID, RequestID, Protocol, Operation, Model string
	Owner                                     Owner
	Provider, ChannelID, ChargeID             string
	IdempotencyKey                            string
	Fingerprint                               [32]byte
	Status                                    Status
	SettlementState                           string
	EstimatedUsage                            *Usage
	ActualUsage                               *Usage
	UsageReconciliationReason                 string
	Version                                   int64
	Snapshot                                  Snapshot
	FailureCategory                           string
	CreatedAt, UpdatedAt                      time.Time
	CompletedAt                               *time.Time
}

type Observation struct {
	Status          Status
	Snapshot        Snapshot
	FailureCategory string
	ProviderJobID   string
	Usage           *Usage
}

type PublicResult struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}
type PublicJob struct {
	ID              string        `json:"id"`
	Status          Status        `json:"status"`
	Result          *PublicResult `json:"result,omitempty"`
	FailureCategory string        `json:"failure_category,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

func Public(value Job) PublicJob {
	result := PublicJob{ID: value.ID, Status: value.Status, FailureCategory: value.FailureCategory, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
	if value.Status == Succeeded || value.Status == Failed {
		result.Result = &PublicResult{Status: value.Snapshot.Status, Headers: value.Snapshot.Headers, Body: append([]byte(nil), value.Snapshot.Body...)}
	}
	return result
}

func ValidID(value string) bool { return idPattern.MatchString(value) }

func (status Status) Terminal() bool {
	return status == Succeeded || status == Failed || status == Canceled
}

func CanTransition(from, to Status) bool {
	if from == to {
		return true
	}
	if from.Terminal() {
		return false
	}
	switch from {
	case Pending:
		return to == Queued || to == Processing || to.Terminal() || to == Reconciling
	case Queued:
		return to == Processing || to.Terminal() || to == Reconciling
	case Processing:
		return to.Terminal() || to == Reconciling
	case Reconciling:
		return to == Queued || to == Processing || to.Terminal()
	default:
		return false
	}
}

func ValidateObservation(current Status, observation Observation, maximumBodyBytes int64) error {
	if !CanTransition(current, observation.Status) || maximumBodyBytes < 1 {
		return ErrInvalidState
	}
	if current.Terminal() {
		if current != observation.Status {
			return ErrConflict
		}
		return nil
	}
	if observation.Status == Succeeded || observation.Status == Failed {
		if err := ValidateSnapshot(observation.Snapshot, maximumBodyBytes); err != nil {
			return err
		}
	}
	if observation.Status == Canceled && len(observation.Snapshot.Body) != 0 {
		return ErrInvalid
	}
	if observation.FailureCategory != "" && !ValidFailureCategory(observation.FailureCategory) {
		return ErrInvalid
	}
	if len(observation.ProviderJobID) > 500 || strings.ContainsAny(observation.ProviderJobID, "\r\n") {
		return ErrInvalid
	}
	if observation.Usage != nil && !ValidActualUsage(*observation.Usage) {
		return ErrInvalid
	}
	return nil
}

func ValidEstimatedUsage(value Usage) bool {
	validQuantity := (value.Dimension == "output" && value.Unit == "image" && value.Quantity > 0 && value.Quantity <= 10) || (value.Dimension == "provider_credit" && value.Unit == "microcredit" && value.Quantity > 0 && value.Quantity <= 1_000_000_000_000_000)
	return validQuantity && validUsageText(value.ExtractorVersion, 80) && validUsageText(value.ResultExtractorVersion, 80) && value.Provenance == "request"
}

func ValidActualUsage(value Usage) bool {
	validQuantity := (value.Dimension == "output" && value.Unit == "image" && value.Quantity >= 0 && value.Quantity <= MaximumObservedUsage) || (value.Dimension == "provider_credit" && value.Unit == "microcredit" && value.Quantity >= 0 && value.Quantity <= 1_000_000_000_000_000)
	return validQuantity && validUsageText(value.ExtractorVersion, 80) && (value.Provenance == "poll" || value.Provenance == "webhook" || value.Provenance == "submit" || value.Provenance == "cancel")
}

func validUsageText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\r\n")
}

func ValidFailureCategory(value string) bool {
	switch value {
	case "rejected", "invalid_request", "rate_limited", "unavailable", "timeout", "connection", "canceled", "invalid_response", "missing_provider_job_id", "provider_error", "provider_unavailable", "cancel_unknown", "settlement_failed", "manual_review", "usage_unknown", "usage_exceeds_estimate", "partial_terminal_conflict", "usage_identity_mismatch":
		return true
	}
	return false
}

func ValidateSnapshot(snapshot Snapshot, maximumBodyBytes int64) error {
	if snapshot.Status < 100 || snapshot.Status > 599 || int64(len(snapshot.Body)) > maximumBodyBytes || len(snapshot.Headers) > 16 {
		return ErrInvalid
	}
	for name, values := range snapshot.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical != "Content-Type" && canonical != "Content-Disposition" && canonical != "Cache-Control" {
			return ErrInvalid
		}
		if len(values) > 8 {
			return ErrInvalid
		}
		for _, value := range values {
			if len(value) > 1024 || strings.ContainsAny(value, "\r\n") {
				return ErrInvalid
			}
		}
	}
	if snapshot.SHA256 != ([32]byte{}) && snapshot.SHA256 != sha256.Sum256(snapshot.Body) {
		return ErrInvalid
	}
	return nil
}

func SameTerminal(job Job, observation Observation) bool {
	if !job.Status.Terminal() || job.Status != observation.Status {
		return false
	}
	if job.Status == Canceled {
		return true
	}
	return job.Snapshot.Status == observation.Snapshot.Status &&
		job.Snapshot.SHA256 == sha256.Sum256(observation.Snapshot.Body)
}
