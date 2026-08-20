package ratelimit

import (
	"context"
	"errors"
	"fmt"

	"github.com/nativegatewayhq/gateway/internal/apikey"
)

var ErrLimited = errors.New("rate limit exceeded")

type LimitError struct {
	Decision  Decision
	APIKeyID  string
	ProjectID string
}

func (err *LimitError) Error() string { return ErrLimited.Error() }
func (err *LimitError) Unwrap() error { return ErrLimited }

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}

type GuardedAuthenticator struct {
	authenticator Authenticator
	limiter       Limiter
}

func NewGuardedAuthenticator(authenticator Authenticator, limiter Limiter) (*GuardedAuthenticator, error) {
	if authenticator == nil || limiter == nil {
		return nil, errors.New("rate limited authenticator dependencies required")
	}
	return &GuardedAuthenticator{authenticator: authenticator, limiter: limiter}, nil
}

func (guard *GuardedAuthenticator) Authenticate(ctx context.Context, raw string) (apikey.Principal, error) {
	principal, err := guard.authenticator.Authenticate(ctx, raw)
	if err != nil || !principal.RateLimit.Enabled() {
		return principal, err
	}
	decision, err := guard.limiter.Allow(ctx, principal.APIKeyID, Policy{RequestsPerMinute: principal.RateLimit.RequestsPerMinute, Burst: principal.RateLimit.Burst})
	if err != nil {
		return apikey.Principal{}, fmt.Errorf("%w: request limiter", ErrUnavailable)
	}
	if !decision.Allowed {
		return apikey.Principal{}, &LimitError{Decision: decision, APIKeyID: principal.APIKeyID, ProjectID: principal.ProjectID}
	}
	principal.RateLimitState = &apikey.RateLimitState{Limit: decision.Limit, Remaining: decision.Remaining, ResetAt: decision.ResetAt}
	return principal, nil
}
