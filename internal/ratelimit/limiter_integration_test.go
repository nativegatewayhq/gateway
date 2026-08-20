//go:build integration

package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisLimiterIsAtomicAcrossInstances(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is required")
	}
	first, err := NewRedis(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewRedis(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("key_integration_%d", time.Now().UnixNano())
	policy := Policy{RequestsPerMinute: 60, Burst: 5}
	var allowed atomic.Int64
	var failures atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 40; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			limiter := first
			if index%2 == 1 {
				limiter = second
			}
			decision, callErr := limiter.Allow(context.Background(), key, policy)
			if callErr != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}(index)
	}
	group.Wait()
	if failures.Load() != 0 || allowed.Load() != policy.Burst {
		t.Fatalf("allowed=%d failures=%d", allowed.Load(), failures.Load())
	}
	decision, err := first.Allow(context.Background(), key, policy)
	if err != nil || decision.Allowed || decision.RetryAfter <= 0 || decision.ResetAt.Before(time.Now()) {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
}

func TestRedisLimiterPolicyChangeUsesIsolatedBucket(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is required")
	}
	limiter, err := NewRedis(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	key := fmt.Sprintf("key_policy_%d", time.Now().UnixNano())
	for index := 0; index < 2; index++ {
		decision, callErr := limiter.Allow(context.Background(), key, Policy{RequestsPerMinute: 60, Burst: 2})
		if callErr != nil || !decision.Allowed {
			t.Fatalf("old decision=%+v error=%v", decision, callErr)
		}
	}
	decision, err := limiter.Allow(context.Background(), key, Policy{RequestsPerMinute: 120, Burst: 3})
	if err != nil || !decision.Allowed || decision.Remaining != 2 {
		t.Fatalf("new decision=%+v error=%v", decision, err)
	}
	options, _ := redis.ParseURL(url)
	client := redis.NewClient(options)
	defer client.Close()
	bucketKey := "ngw:rl:v1:" + key + ":120:3"
	ttl, err := client.PTTL(context.Background(), bucketKey).Result()
	if err != nil || ttl <= 0 || ttl > 120*time.Second {
		t.Fatalf("ttl=%v error=%v", ttl, err)
	}
}

func TestRedisLimiterFailsClosedOnMalformedOrFutureState(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is required")
	}
	limiter, err := NewRedis(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	options, _ := redis.ParseURL(url)
	client := redis.NewClient(options)
	defer client.Close()
	for _, state := range []map[string]any{{"tokens": "invalid", "ts": 1}, {"tokens": 1, "ts": time.Now().Add(time.Hour).UnixMilli()}} {
		key := fmt.Sprintf("key_malformed_%d", time.Now().UnixNano())
		bucketKey := "ngw:rl:v1:" + key + ":60:5"
		if err := client.HSet(context.Background(), bucketKey, state).Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := limiter.Allow(context.Background(), key, Policy{RequestsPerMinute: 60, Burst: 5}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("state=%v error=%v", state, err)
		}
		_ = client.Del(context.Background(), bucketKey).Err()
	}
}
