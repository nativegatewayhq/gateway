package providerhealth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type redisCommander interface {
	Eval(context.Context, string, []string, ...any) *redis.Cmd
	Ping(context.Context) *redis.StatusCmd
	Close() error
}

type RedisGate struct {
	client  redisCommander
	config  Config
	entropy func([]byte) (int, error)
}

func NewRedis(redisURL string, config Config) (*RedisGate, error) {
	if !config.Valid() {
		return nil, ErrInvalid
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil || options.Addr == "" {
		return nil, fmt.Errorf("%w: redis URL", ErrUnavailable)
	}
	return &RedisGate{client: redis.NewClient(options), config: config, entropy: rand.Read}, nil
}

func (gate *RedisGate) Close() error {
	if gate == nil || gate.client == nil {
		return nil
	}
	return gate.client.Close()
}

func (gate *RedisGate) Ping(ctx context.Context) error {
	if gate == nil || gate.client == nil || !gate.config.Valid() {
		return ErrUnavailable
	}
	commandContext, cancel := context.WithTimeout(ctx, gate.config.CommandTimeout)
	defer cancel()
	if err := gate.client.Ping(commandContext).Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return nil
}

func (gate *RedisGate) Inspect(ctx context.Context, channelID string) (Snapshot, error) {
	if !validChannelID(channelID) || gate == nil || gate.client == nil || !gate.config.Valid() {
		return Snapshot{}, ErrInvalid
	}
	result, err := gate.eval(ctx, inspectScript, []string{gate.healthKey(channelID)}, gate.bucketMilliseconds(), gate.bucketCount())
	if err != nil {
		return Snapshot{}, err
	}
	return parseSnapshot(result)
}

func (gate *RedisGate) ClaimProbe(ctx context.Context, channelID, requestID string) (Permit, error) {
	if !validChannelID(channelID) || requestID == "" || len(requestID) > 128 || gate == nil || gate.client == nil || !gate.config.Valid() {
		return Permit{}, ErrInvalid
	}
	var random [16]byte
	if read, err := gate.entropy(random[:]); err != nil || read != len(random) {
		return Permit{}, ErrUnavailable
	}
	token := hex.EncodeToString(random[:])
	result, err := gate.eval(ctx, claimScript, []string{gate.healthKey(channelID)}, token, gate.config.ProbeLease.Milliseconds(), gate.ttlMilliseconds())
	if err != nil {
		return Permit{}, err
	}
	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return Permit{}, ErrUnavailable
	}
	code, err := redisInt(values[0])
	if err != nil {
		return Permit{}, ErrUnavailable
	}
	switch code {
	case 0:
		return Permit{ChannelID: channelID}, nil
	case 1:
		return Permit{}, ErrOpen
	case 2:
		return Permit{}, ErrProbeBusy
	case 3:
		returned, ok := values[1].(string)
		if !ok || returned != token {
			return Permit{}, ErrUnavailable
		}
		return Permit{ChannelID: channelID, Token: token, Probe: true}, nil
	default:
		return Permit{}, ErrUnavailable
	}
}

func (gate *RedisGate) Release(ctx context.Context, permit Permit) error {
	if !permit.Probe {
		return nil
	}
	if !validPermit(permit) || gate == nil || gate.client == nil || !gate.config.Valid() {
		return ErrInvalid
	}
	result, err := gate.eval(ctx, releaseScript, []string{gate.healthKey(permit.ChannelID)}, permit.Token)
	if err != nil {
		return err
	}
	if _, err := redisInt(result); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (gate *RedisGate) Observe(ctx context.Context, observation Observation) (Snapshot, error) {
	if !validObservation(observation) || gate == nil || gate.client == nil || !gate.config.Valid() {
		return Snapshot{}, ErrInvalid
	}
	digest := sha256.Sum256([]byte(observation.ChannelID + "\x00" + observation.ObservationID))
	permitToken := ""
	if observation.Permit.Probe {
		permitToken = observation.Permit.Token
	}
	result, err := gate.eval(ctx, observeScript, []string{gate.healthKey(observation.ChannelID), gate.dedupeKey(observation.ChannelID, hex.EncodeToString(digest[:]))}, string(observation.Outcome), permitToken, gate.bucketMilliseconds(), gate.bucketCount(), gate.config.MinimumSamples, gate.config.FailureThresholdBPS, gate.config.OpenDuration.Milliseconds(), gate.config.MaximumOpenDuration.Milliseconds(), gate.ttlMilliseconds())
	if err != nil {
		return Snapshot{}, err
	}
	return parseSnapshot(result)
}

func (gate *RedisGate) eval(ctx context.Context, script string, keys []string, arguments ...any) (any, error) {
	commandContext, cancel := context.WithTimeout(ctx, gate.config.CommandTimeout)
	defer cancel()
	result, err := gate.client.Eval(commandContext, script, keys, arguments...).Result()
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	return result, nil
}

func (gate *RedisGate) healthKey(channelID string) string {
	return gate.config.KeyPrefix + ":{" + channelID + "}:state"
}

func (gate *RedisGate) dedupeKey(channelID, digest string) string {
	return gate.config.KeyPrefix + ":{" + channelID + "}:observation:" + digest
}

func (gate *RedisGate) bucketMilliseconds() int64 { return gate.config.Bucket.Milliseconds() }
func (gate *RedisGate) bucketCount() int64        { return int64(gate.config.Window / gate.config.Bucket) }
func (gate *RedisGate) ttlMilliseconds() int64 {
	return (gate.config.Window + gate.config.MaximumOpenDuration + gate.config.ProbeLease).Milliseconds()
}

func parseSnapshot(result any) (Snapshot, error) {
	values, ok := result.([]any)
	if !ok || len(values) != 5 {
		return Snapshot{}, ErrUnavailable
	}
	parsed := make([]int64, len(values))
	for index := range values {
		value, err := redisInt(values[index])
		if err != nil {
			return Snapshot{}, ErrUnavailable
		}
		parsed[index] = value
	}
	states := []State{Closed, Open, HalfOpen}
	if parsed[0] < 0 || parsed[0] >= int64(len(states)) || parsed[1] < 0 || parsed[2] < 0 || parsed[3] < 0 || parsed[4] < 0 {
		return Snapshot{}, ErrUnavailable
	}
	snapshot := Snapshot{State: states[parsed[0]], Successes: parsed[1], Failures: parsed[2]}
	if parsed[3] > 0 {
		snapshot.OpenUntil = time.UnixMilli(parsed[3]).UTC()
	}
	if parsed[4] > 0 {
		snapshot.ProbeUntil = time.UnixMilli(parsed[4]).UTC()
	}
	return snapshot, nil
}

func redisInt(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, ErrUnavailable
	}
}

func validObservation(observation Observation) bool {
	if !validChannelID(observation.ChannelID) || observation.ObservationID == "" || len(observation.ObservationID) > 128 || !validOutcome(observation.Outcome) {
		return false
	}
	return !observation.Permit.Probe || (validPermit(observation.Permit) && observation.Permit.ChannelID == observation.ChannelID)
}

func validPermit(permit Permit) bool {
	return permit.Probe && validChannelID(permit.ChannelID) && len(permit.Token) == 32 && strings.IndexFunc(permit.Token, func(character rune) bool { return !strings.ContainsRune("0123456789abcdef", character) }) == -1
}

func validChannelID(channelID string) bool {
	if len(channelID) != len("channel_")+32 || !strings.HasPrefix(channelID, "channel_") {
		return false
	}
	return strings.IndexFunc(strings.TrimPrefix(channelID, "channel_"), func(character rune) bool { return !strings.ContainsRune("0123456789abcdef", character) }) == -1
}

const inspectScript = `
local now_parts = redis.call('TIME')
local now = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
local bucket_ms = tonumber(ARGV[1])
local bucket_count = tonumber(ARGV[2])
local current = math.floor(now / bucket_ms)
local successes = 0
local failures = 0
for index = current - bucket_count + 1, current do
  successes = successes + tonumber(redis.call('HGET', KEYS[1], 's:' .. index) or '0')
  failures = failures + tonumber(redis.call('HGET', KEYS[1], 'f:' .. index) or '0')
end
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
local open_until = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
local probe_token = redis.call('HGET', KEYS[1], 'probe_token') or ''
local probe_until = tonumber(redis.call('HGET', KEYS[1], 'probe_until') or '0')
if state == 'open' and open_until > now then
  return {1, successes, failures, open_until, 0}
end
if state == 'open' then
  if probe_token ~= '' and probe_until > now then
    return {2, successes, failures, open_until, probe_until}
  end
  if probe_token ~= '' then redis.call('HDEL', KEYS[1], 'probe_token', 'probe_until') end
  return {2, successes, failures, open_until, 0}
end
return {0, successes, failures, 0, 0}
`

const claimScript = `
local now_parts = redis.call('TIME')
local now = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state ~= 'open' then return {0, ''} end
local open_until = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
if open_until > now then return {1, ''} end
local existing = redis.call('HGET', KEYS[1], 'probe_token') or ''
local probe_until = tonumber(redis.call('HGET', KEYS[1], 'probe_until') or '0')
if existing ~= '' and probe_until > now then return {2, ''} end
redis.call('HSET', KEYS[1], 'probe_token', ARGV[1], 'probe_until', now + tonumber(ARGV[2]))
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[3]))
return {3, ARGV[1]}
`

const releaseScript = `
local existing = redis.call('HGET', KEYS[1], 'probe_token') or ''
if existing ~= ARGV[1] then return 0 end
redis.call('HDEL', KEYS[1], 'probe_token', 'probe_until')
return 1
`

const observeScript = `
local now_parts = redis.call('TIME')
local now = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
local ttl = tonumber(ARGV[9])
local outcome = ARGV[1]
local permit = ARGV[2]
local bucket_ms = tonumber(ARGV[3])
local bucket_count = tonumber(ARGV[4])
local current = math.floor(now / bucket_ms)
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
local existing_probe = redis.call('HGET', KEYS[1], 'probe_token') or ''
local is_probe = permit ~= '' and permit == existing_probe
if permit ~= '' and not is_probe then return redis.error_reply('invalid probe') end
if not redis.call('SET', KEYS[2], '1', 'NX', 'PX', ttl) then
  local open_until = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
  local successes = 0
  local failures = 0
  for index = current - bucket_count + 1, current do
    successes = successes + tonumber(redis.call('HGET', KEYS[1], 's:' .. index) or '0')
    failures = failures + tonumber(redis.call('HGET', KEYS[1], 'f:' .. index) or '0')
  end
  local code = 0
  if state == 'open' then if open_until > now then code = 1 else code = 2 end end
  return {code, successes, failures, open_until, tonumber(redis.call('HGET', KEYS[1], 'probe_until') or '0')}
end
local is_failure = outcome == 'rate_limited' or outcome == 'server_error' or outcome == 'timeout' or outcome == 'connection'
if is_probe and outcome == 'neutral' then
  redis.call('HDEL', KEYS[1], 'probe_token', 'probe_until')
elseif is_probe and outcome == 'success' then
  redis.call('DEL', KEYS[1])
  state = 'closed'
elseif is_probe and is_failure then
  local backoff = tonumber(redis.call('HGET', KEYS[1], 'backoff') or ARGV[7])
  backoff = math.min(backoff * 2, tonumber(ARGV[8]))
  redis.call('HSET', KEYS[1], 'state', 'open', 'open_until', now + backoff, 'backoff', backoff)
  redis.call('HDEL', KEYS[1], 'probe_token', 'probe_until')
  state = 'open'
end
if outcome == 'success' then redis.call('HINCRBY', KEYS[1], 's:' .. current, 1) end
if is_failure then redis.call('HINCRBY', KEYS[1], 'f:' .. current, 1) end
redis.call('HDEL', KEYS[1], 's:' .. (current - bucket_count), 'f:' .. (current - bucket_count))
local successes = 0
local failures = 0
for index = current - bucket_count + 1, current do
  successes = successes + tonumber(redis.call('HGET', KEYS[1], 's:' .. index) or '0')
  failures = failures + tonumber(redis.call('HGET', KEYS[1], 'f:' .. index) or '0')
end
if state == 'closed' and successes + failures >= tonumber(ARGV[5]) and failures * 10000 >= (successes + failures) * tonumber(ARGV[6]) then
  redis.call('HSET', KEYS[1], 'state', 'open', 'open_until', now + tonumber(ARGV[7]), 'backoff', tonumber(ARGV[7]))
  state = 'open'
end
redis.call('PEXPIRE', KEYS[1], ttl)
local open_until = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
local probe_until = tonumber(redis.call('HGET', KEYS[1], 'probe_until') or '0')
local code = 0
if state == 'open' then if open_until > now then code = 1 else code = 2 end end
return {code, successes, failures, open_until, probe_until}
`
