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

type Job struct {
	ID, RequestID, Protocol, Operation, Model string
	Owner                                     Owner
	Provider, ChannelID, ChargeID             string
	IdempotencyKey                            string
	Fingerprint                               [32]byte
	Status                                    Status
	SettlementState                           string
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
	if len(observation.FailureCategory) > 80 || strings.ContainsAny(observation.FailureCategory, "\r\n") {
		return ErrInvalid
	}
	return nil
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
