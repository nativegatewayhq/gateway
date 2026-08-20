// Package ratelimit implements distributed API-key request limiting.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrUnavailable   = errors.New("rate limiter unavailable")
	ErrInvalidPolicy = errors.New("invalid rate limit policy")
)

type Policy struct {
	RequestsPerMinute int64
	Burst             int64
}

func (policy Policy) Valid() bool {
	return policy.RequestsPerMinute >= 1 && policy.RequestsPerMinute <= 1_000_000 && policy.Burst >= 1 && policy.Burst <= policy.RequestsPerMinute
}

type Decision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	RetryAfter time.Duration
	ResetAt    time.Time
}

type Limiter interface {
	Allow(context.Context, string, Policy) (Decision, error)
}

type redisCommander interface {
	Eval(context.Context, string, []string, ...any) *redis.Cmd
	Ping(context.Context) *redis.StatusCmd
	Close() error
}

type RedisLimiter struct {
	client  redisCommander
	timeout time.Duration
}

func NewRedis(redisURL string, timeout time.Duration) (*RedisLimiter, error) {
	if timeout <= 0 || timeout > time.Second {
		return nil, fmt.Errorf("%w: timeout", ErrInvalidPolicy)
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("%w: redis URL", ErrUnavailable)
	}
	options.DialTimeout = timeout
	options.ReadTimeout = timeout
	options.WriteTimeout = timeout
	return &RedisLimiter{client: redis.NewClient(options), timeout: timeout}, nil
}

func (limiter *RedisLimiter) Close() error { return limiter.client.Close() }

func (limiter *RedisLimiter) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, limiter.timeout)
	defer cancel()
	if err := limiter.client.Ping(ctx).Err(); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (limiter *RedisLimiter) Allow(ctx context.Context, apiKeyID string, policy Policy) (Decision, error) {
	if !policy.Valid() || !validAPIKeyID(apiKeyID) {
		return Decision{}, ErrInvalidPolicy
	}
	key := "ngw:rl:v1:" + apiKeyID + ":" + strconv.FormatInt(policy.RequestsPerMinute, 10) + ":" + strconv.FormatInt(policy.Burst, 10)
	callCtx, cancel := context.WithTimeout(ctx, limiter.timeout)
	defer cancel()
	result, err := limiter.client.Eval(callCtx, tokenBucketScript, []string{key}, policy.RequestsPerMinute, policy.Burst).Slice()
	if err != nil || len(result) != 4 {
		return Decision{}, ErrUnavailable
	}
	values := make([]int64, 4)
	for index := range result {
		values[index], err = redisInteger(result[index])
		if err != nil {
			return Decision{}, ErrUnavailable
		}
	}
	if values[0] != 0 && values[0] != 1 || values[1] < 0 || values[1] > policy.Burst || values[2] < 0 || values[3] < 0 {
		return Decision{}, ErrUnavailable
	}
	return Decision{Allowed: values[0] == 1, Limit: policy.RequestsPerMinute, Remaining: values[1], RetryAfter: time.Duration(values[2]) * time.Millisecond, ResetAt: time.UnixMilli(values[3])}, nil
}

func validAPIKeyID(value string) bool {
	if len(value) < 5 || len(value) > 128 || value[:4] != "key_" {
		return false
	}
	for _, character := range value[4:] {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func redisInteger(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	default:
		return 0, errors.New("unexpected redis value")
	}
}

const tokenBucketScript = `
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
if not rate or not burst or rate < 1 or burst < 1 or burst > rate then
  return redis.error_reply('invalid policy')
end
local scale = 1000000
local capacity = burst * scale
local now_parts = redis.call('TIME')
local now = (tonumber(now_parts[1]) * 1000) + math.floor(tonumber(now_parts[2]) / 1000)
local state = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local exists = redis.call('EXISTS', KEYS[1])
local tokens = capacity
local previous = now
if exists == 1 then
  tokens = tonumber(state[1])
  previous = tonumber(state[2])
  if not tokens or not previous or tokens < 0 or tokens > capacity or previous < 0 or previous > now then
    return redis.error_reply('invalid bucket state')
  end
end
local elapsed = now - previous
tokens = math.min(capacity, tokens + math.floor((elapsed * rate * scale) / 60000))
local allowed = 0
if tokens >= scale then
  tokens = tokens - scale
  allowed = 1
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], 120000)
local remaining = math.floor(tokens / scale)
local retry = 0
if allowed == 0 then
  retry = math.ceil(((scale - tokens) * 60000) / (rate * scale))
end
local reset = now + math.ceil(((capacity - tokens) * 60000) / (rate * scale))
return {allowed, remaining, retry, reset}
`
