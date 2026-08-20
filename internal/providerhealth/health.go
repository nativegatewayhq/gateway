// Package providerhealth coordinates distributed Provider channel circuit state.
package providerhealth

import (
	"context"
	"errors"
	"time"
)

type State string
type Outcome string

const (
	Closed   State = "CLOSED"
	Open     State = "OPEN"
	HalfOpen State = "HALF_OPEN"

	Success     Outcome = "success"
	RateLimited Outcome = "rate_limited"
	ServerError Outcome = "server_error"
	Timeout     Outcome = "timeout"
	Connection  Outcome = "connection"
	Neutral     Outcome = "neutral"
)

var (
	ErrInvalid     = errors.New("invalid provider health request")
	ErrOpen        = errors.New("provider channel circuit open")
	ErrProbeBusy   = errors.New("provider channel probe busy")
	ErrUnavailable = errors.New("provider health unavailable")
)

type Config struct {
	Window              time.Duration
	Bucket              time.Duration
	MinimumSamples      int64
	FailureThresholdBPS int64
	OpenDuration        time.Duration
	MaximumOpenDuration time.Duration
	ProbeLease          time.Duration
	CommandTimeout      time.Duration
	KeyPrefix           string
}

func DefaultConfig() Config {
	return Config{Window: time.Minute, Bucket: 10 * time.Second, MinimumSamples: 10, FailureThresholdBPS: 5_000, OpenDuration: 30 * time.Second, MaximumOpenDuration: 5 * time.Minute, ProbeLease: 10 * time.Second, CommandTimeout: 100 * time.Millisecond, KeyPrefix: "gateway:provider-health:v1"}
}

func (config Config) Valid() bool {
	return config.Window >= 10*time.Second && config.Window <= time.Hour && config.Bucket >= time.Second && config.Bucket <= time.Minute && config.Window%config.Bucket == 0 && config.Window/config.Bucket <= 120 && config.MinimumSamples >= 1 && config.MinimumSamples <= 1_000_000 && config.FailureThresholdBPS >= 1 && config.FailureThresholdBPS <= 10_000 && config.OpenDuration >= time.Second && config.OpenDuration <= time.Hour && config.MaximumOpenDuration >= config.OpenDuration && config.MaximumOpenDuration <= 24*time.Hour && config.ProbeLease >= time.Second && config.ProbeLease <= config.OpenDuration && config.CommandTimeout > 0 && config.CommandTimeout <= time.Second && validPrefix(config.KeyPrefix)
}

type Snapshot struct {
	State      State
	Successes  int64
	Failures   int64
	OpenUntil  time.Time
	ProbeUntil time.Time
}

type Permit struct {
	ChannelID string
	Token     string
	Probe     bool
}

type Observation struct {
	ChannelID     string
	ObservationID string
	Outcome       Outcome
	Permit        Permit
}

type Gate interface {
	Inspect(context.Context, string) (Snapshot, error)
	ClaimProbe(context.Context, string, string) (Permit, error)
	Release(context.Context, Permit) error
	Observe(context.Context, Observation) (Snapshot, error)
}

type NoopGate struct{}

func (NoopGate) Inspect(context.Context, string) (Snapshot, error) {
	return Snapshot{State: Closed}, nil
}
func (NoopGate) ClaimProbe(_ context.Context, channelID, _ string) (Permit, error) {
	return Permit{ChannelID: channelID}, nil
}
func (NoopGate) Release(context.Context, Permit) error { return nil }
func (NoopGate) Observe(context.Context, Observation) (Snapshot, error) {
	return Snapshot{State: Closed}, nil
}

func IsFailure(outcome Outcome) bool {
	return outcome == RateLimited || outcome == ServerError || outcome == Timeout || outcome == Connection
}

func validOutcome(outcome Outcome) bool {
	return outcome == Success || outcome == Neutral || IsFailure(outcome)
}

func validPrefix(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == ':' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
