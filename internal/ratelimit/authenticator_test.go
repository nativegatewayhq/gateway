package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
)

type authFunc func(context.Context, string) (apikey.Principal, error)

func (fn authFunc) Authenticate(ctx context.Context, raw string) (apikey.Principal, error) {
	return fn(ctx, raw)
}

type limiterFunc func(context.Context, string, Policy) (Decision, error)

func (fn limiterFunc) Allow(ctx context.Context, key string, policy Policy) (Decision, error) {
	return fn(ctx, key, policy)
}

func TestGuardedAuthenticatorSkipsUnlimitedKeys(t *testing.T) {
	calls := 0
	guard, err := NewGuardedAuthenticator(authFunc(func(context.Context, string) (apikey.Principal, error) {
		return apikey.Principal{APIKeyID: "key_unlimited"}, nil
	}), limiterFunc(func(context.Context, string, Policy) (Decision, error) { calls++; return Decision{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := guard.Authenticate(context.Background(), "secret")
	if err != nil || principal.APIKeyID != "key_unlimited" || calls != 0 {
		t.Fatalf("principal=%+v calls=%d err=%v", principal, calls, err)
	}
}

func TestGuardedAuthenticatorAllowedLimitedAndUnavailable(t *testing.T) {
	base := authFunc(func(context.Context, string) (apikey.Principal, error) {
		return apikey.Principal{APIKeyID: "key_limited", ProjectID: "project_test", RateLimit: apikey.RateLimitPolicy{RequestsPerMinute: 60, Burst: 2}}, nil
	})
	reset := time.Unix(2_000_000_000, 0)
	allowed, _ := NewGuardedAuthenticator(base, limiterFunc(func(_ context.Context, key string, policy Policy) (Decision, error) {
		if key != "key_limited" || policy.RequestsPerMinute != 60 || policy.Burst != 2 {
			t.Fatalf("key=%s policy=%+v", key, policy)
		}
		return Decision{Allowed: true, Limit: 60, Remaining: 1, ResetAt: reset}, nil
	}))
	principal, err := allowed.Authenticate(context.Background(), "secret")
	if err != nil || principal.RateLimitState == nil || principal.RateLimitState.Remaining != 1 || !principal.RateLimitState.ResetAt.Equal(reset) {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}

	limited, _ := NewGuardedAuthenticator(base, limiterFunc(func(context.Context, string, Policy) (Decision, error) {
		return Decision{Limit: 60, Remaining: 0, RetryAfter: time.Second, ResetAt: reset}, nil
	}))
	_, err = limited.Authenticate(context.Background(), "secret")
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Decision.RetryAfter != time.Second {
		t.Fatalf("error=%v", err)
	}

	unavailable, _ := NewGuardedAuthenticator(base, limiterFunc(func(context.Context, string, Policy) (Decision, error) { return Decision{}, errors.New("redis secret") }))
	if _, err = unavailable.Authenticate(context.Background(), "secret"); !errors.Is(err, ErrUnavailable) || errors.Is(err, errors.New("redis secret")) {
		t.Fatalf("error=%v", err)
	}
}
