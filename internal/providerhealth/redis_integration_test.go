//go:build integration

package providerhealth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

const healthTestChannel = "channel_00000000000000000000000000000091"

func healthIntegrationConfig() Config {
	config := DefaultConfig()
	config.Window = 10 * time.Second
	config.Bucket = time.Second
	config.MinimumSamples = 2
	config.OpenDuration = time.Second
	config.MaximumOpenDuration = 2 * time.Second
	config.ProbeLease = time.Second
	config.CommandTimeout = time.Second
	config.KeyPrefix = fmt.Sprintf("gateway:provider-health:test:%d", time.Now().UnixNano())
	return config
}

func healthRedisURL(t *testing.T) string {
	t.Helper()
	value := os.Getenv("TEST_REDIS_URL")
	if value == "" {
		t.Skip("TEST_REDIS_URL is required")
	}
	return value
}

func TestRedisGateSharesCircuitAndIssuesOneProbe(t *testing.T) {
	config := healthIntegrationConfig()
	first, err := NewRedis(healthRedisURL(t), config)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewRedis(healthRedisURL(t), config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()
	if _, err := first.Observe(ctx, Observation{ChannelID: healthTestChannel, ObservationID: "success-1", Outcome: Success}); err != nil {
		t.Fatal(err)
	}
	opened, err := second.Observe(ctx, Observation{ChannelID: healthTestChannel, ObservationID: "failure-1", Outcome: ServerError})
	if err != nil || opened.State != Open || opened.Successes != 1 || opened.Failures != 1 {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	duplicate, err := first.Observe(ctx, Observation{ChannelID: healthTestChannel, ObservationID: "failure-1", Outcome: ServerError})
	if err != nil || duplicate.Failures != 1 {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	if _, err := first.ClaimProbe(ctx, healthTestChannel, "too-early"); !errors.Is(err, ErrOpen) {
		t.Fatalf("early claim err=%v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if snapshot, err := second.Inspect(ctx, healthTestChannel); err != nil || snapshot.State != HalfOpen {
		t.Fatalf("half-open=%+v err=%v", snapshot, err)
	}

	var group sync.WaitGroup
	results := make(chan struct {
		permit Permit
		err    error
	}, 2)
	for index, gate := range []*RedisGate{first, second} {
		group.Add(1)
		go func(index int, gate *RedisGate) {
			defer group.Done()
			permit, err := gate.ClaimProbe(ctx, healthTestChannel, fmt.Sprintf("probe-%d", index))
			results <- struct {
				permit Permit
				err    error
			}{permit, err}
		}(index, gate)
	}
	group.Wait()
	close(results)
	var winner Permit
	busy := 0
	for result := range results {
		if result.err == nil {
			winner = result.permit
		} else if errors.Is(result.err, ErrProbeBusy) {
			busy++
		} else {
			t.Fatalf("claim err=%v", result.err)
		}
	}
	if !winner.Probe || busy != 1 {
		t.Fatalf("winner=%+v busy=%d", winner, busy)
	}
	closed, err := first.Observe(ctx, Observation{ChannelID: healthTestChannel, ObservationID: "probe-success", Outcome: Success, Permit: winner})
	if err != nil || closed.State != Closed || closed.Successes != 1 || closed.Failures != 0 {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
}

func TestRedisGateProbeFailureReopensWithBoundedBackoffAndReleaseIsIdempotent(t *testing.T) {
	config := healthIntegrationConfig()
	gate, err := NewRedis(healthRedisURL(t), config)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	ctx := context.Background()
	for index := 0; index < 2; index++ {
		if _, err := gate.Observe(ctx, Observation{ChannelID: healthTestChannel, ObservationID: fmt.Sprintf("failure-%d", index), Outcome: Timeout}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	permit, err := gate.ClaimProbe(ctx, healthTestChannel, "probe-failure")
	if err != nil || !permit.Probe {
		t.Fatalf("permit=%+v err=%v", permit, err)
	}
	before := time.Now()
	reopened, err := gate.Observe(ctx, Observation{ChannelID: healthTestChannel, ObservationID: "probe-failure-observation", Outcome: Connection, Permit: permit})
	if err != nil || reopened.State != Open || reopened.OpenUntil.Before(before.Add(1500*time.Millisecond)) || reopened.OpenUntil.After(before.Add(2500*time.Millisecond)) {
		t.Fatalf("reopened=%+v err=%v", reopened, err)
	}
	if err := gate.Release(ctx, permit); err != nil {
		t.Fatal(err)
	}
	if err := gate.Release(ctx, permit); err != nil {
		t.Fatal(err)
	}
}
