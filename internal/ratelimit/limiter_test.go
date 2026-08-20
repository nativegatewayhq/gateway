package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPolicyValidation(t *testing.T) {
	for _, test := range []struct {
		policy Policy
		valid  bool
	}{
		{Policy{60, 10}, true}, {Policy{1, 1}, true}, {Policy{}, false}, {Policy{10, 11}, false}, {Policy{1_000_001, 1}, false},
	} {
		if test.policy.Valid() != test.valid {
			t.Fatalf("policy=%+v valid=%v", test.policy, test.valid)
		}
	}
}

func TestNewRedisRejectsInvalidConfigurationWithoutEchoingSecret(t *testing.T) {
	for _, test := range []struct {
		url     string
		timeout time.Duration
	}{{"not a url password=secret", time.Millisecond}, {"redis://localhost", 0}} {
		_, err := NewRedis(test.url, test.timeout)
		if err == nil || !errors.Is(err, map[bool]error{true: ErrInvalidPolicy, false: ErrUnavailable}[test.timeout == 0]) {
			t.Fatalf("error=%v", err)
		}
		if err != nil && containsSecret(err.Error()) {
			t.Fatalf("secret leaked: %v", err)
		}
	}
}

func containsSecret(value string) bool {
	return len(value) >= 6 && (value == "secret" || stringContains(value, "secret"))
}
func stringContains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}

func TestAllowRejectsInvalidPolicyBeforeRedis(t *testing.T) {
	limiter := &RedisLimiter{timeout: time.Millisecond}
	if _, err := limiter.Allow(context.Background(), "key_test", Policy{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("error=%v", err)
	}
}
