package providerhealth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDefaultConfigAndNoopGate(t *testing.T) {
	if config := DefaultConfig(); !config.Valid() {
		t.Fatalf("config=%+v", config)
	}
	gate := NoopGate{}
	snapshot, err := gate.Inspect(context.Background(), "anything")
	if err != nil || snapshot.State != Closed {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	permit, err := gate.ClaimProbe(context.Background(), "channel", "request")
	if err != nil || permit.Probe || permit.ChannelID != "channel" {
		t.Fatalf("permit=%+v err=%v", permit, err)
	}
}

func TestConfigBoundsAndOutcomeClassification(t *testing.T) {
	base := DefaultConfig()
	tests := []Config{
		{},
		func() Config { value := base; value.Window = 0; return value }(),
		func() Config { value := base; value.Bucket = 7 * time.Second; return value }(),
		func() Config { value := base; value.MinimumSamples = 0; return value }(),
		func() Config { value := base; value.FailureThresholdBPS = 10_001; return value }(),
		func() Config { value := base; value.ProbeLease = value.OpenDuration + time.Second; return value }(),
		func() Config { value := base; value.KeyPrefix = "secret value"; return value }(),
	}
	for _, config := range tests {
		if config.Valid() {
			t.Fatalf("invalid config accepted: %+v", config)
		}
	}
	for _, outcome := range []Outcome{RateLimited, ServerError, Timeout, Connection} {
		if !IsFailure(outcome) {
			t.Fatalf("outcome=%s", outcome)
		}
	}
	if IsFailure(Success) || IsFailure(Neutral) || validOutcome("unknown") {
		t.Fatal("outcome classification invalid")
	}
}

func TestNewRedisRejectsSecretsWithoutEchoingThem(t *testing.T) {
	_, err := NewRedis("not-a-url-password=secret", DefaultConfig())
	if !errors.Is(err, ErrUnavailable) || err == nil || contains(err.Error(), "password=secret") {
		t.Fatalf("error=%v", err)
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
