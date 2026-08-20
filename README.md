# Native AI Gateway

An open-source multimodal AI API gateway that preserves official provider SDKs and native API protocols while unifying authentication, routing, billing, failover, and managed media delivery.

## Status

Phase 0 native protocol validation, Phase 1 image billing foundations, and early Phase 2 routing. The process exposes health endpoints, tenant-owned PostgreSQL service API key authentication, a capability-backed models endpoint, non-streaming Gemini `generateContent`, and OpenAI-compatible image generation and editing. Billing-required mode supports exact channel pricing, Wallet settlement, customer quotas, Provider spend caps, fixed/priority/lowest-cost routing, and Gemini/OpenAI/xAI image billing. Production payment deposits and managed route publication remain outside this repository.

## Development workflow

This repository follows Plan First development.

1. Read [`AGENTS.md`](./AGENTS.md).
2. Read [`CONTRIBUTING.md`](./CONTRIBUTING.md).
3. Review [`plans/`](./plans/).
4. Implement only an accepted plan.

The first accepted implementation plan is [Phase 0 Gateway Bootstrap](./plans/20260820_113825_plan_phase0_gateway_bootstrap.md).

## Requirements

- Go 1.25 or newer supported release
- PostgreSQL 17; Redis 8 when distributed rate limiting is enabled

## Run locally

```bash
docker compose up -d postgres redis
export GATEWAY_DATABASE_URL='postgres://gateway:gateway-local@127.0.0.1:55433/gateway?sslmode=disable'
go run ./cmd/gateway
```

The default listener is `:8080`.

```bash
curl -i http://127.0.0.1:8080/health/live
curl -i http://127.0.0.1:8080/health/ready
```

Both endpoints return:

```json
{"status":"ok"}
```

Every response includes `X-Request-Id`. A caller-provided request ID is accepted only when it is at most 128 characters and contains ASCII letters, numbers, `.`, `_`, `-`, or `:`.

## Configuration

| Environment variable | Default | Description |
|---|---|---|
| `GATEWAY_HTTP_ADDR` | `:8080` | TCP host and numeric port to listen on |
| `GATEWAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Positive Go duration for graceful shutdown |
| `GATEWAY_DATABASE_URL` | required | PostgreSQL connection URL; treated as a secret and never logged |
| `GATEWAY_GOOGLE_API_KEY` | unset | Optional Google upstream credential |
| `GATEWAY_ANTHROPIC_API_KEY` | unset | Optional Anthropic upstream credential |
| `GATEWAY_OPENAI_API_KEY` | unset | Optional OpenAI upstream credential |
| `GATEWAY_OPENAI_CHAT_MODELS` | unset | Comma-separated exact OpenAI Chat model IDs; a non-empty value enables `POST /v1/chat/completions` |
| `GATEWAY_OPENAI_CHAT_MODEL_LIMITS` | unset | Required in billing mode: comma-separated `model:maximum_input_tokens:maximum_output_tokens` entries |
| `GATEWAY_OPENAI_CHAT_ROUTES_JSON` | unset | Versioned static logical-model route array; replaces the two legacy Chat model settings and supports OpenAI/xAI fixed, priority, weighted, and lowest-cost candidates |
| `GATEWAY_OPENAI_CHAT_REQUEST_TIMEOUT` | `2m` | Non-streaming OpenAI Chat request timeout; maximum `10m` |
| `GATEWAY_OPENAI_CHAT_STREAM_IDLE_TIMEOUT` | `30s` | Maximum idle interval between upstream streaming reads; maximum `10m` |
| `GATEWAY_OPENAI_CHAT_MAX_BODY_BYTES` | `8388608` | Maximum Chat request and response body; maximum 32 MiB |
| `GATEWAY_OPENAI_RESPONSES_MODELS` | unset | Comma-separated exact OpenAI Responses model IDs |
| `GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS` | unset | Required in billing mode: comma-separated `model:maximum_input_tokens:maximum_output_tokens` entries |
| `GATEWAY_OPENAI_RESPONSES_ROUTES_JSON` | unset | Static logical Responses route array; replaces the two legacy Responses model settings and supports native OpenAI/xAI fixed, priority, weighted, and lowest-cost candidates |
| `GATEWAY_OPENAI_RESPONSES_REQUEST_TIMEOUT` | `2m` | Responses request timeout; maximum `10m` |
| `GATEWAY_OPENAI_RESPONSES_STREAM_IDLE_TIMEOUT` | `30s` | Maximum idle interval between upstream Responses streaming reads; maximum `10m` |
| `GATEWAY_OPENAI_RESPONSES_MAX_BODY_BYTES` | `8388608` | Maximum Responses request and response body; maximum 32 MiB |
| `GATEWAY_ANTHROPIC_MESSAGES_MODELS` | unset | Comma-separated exact Anthropic Messages model IDs |
| `GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS` | unset | Required in billing mode: comma-separated `model:maximum_input_tokens:maximum_output_tokens` entries |
| `GATEWAY_ANTHROPIC_REQUEST_TIMEOUT` | `2m` | Anthropic response-header/request timeout; maximum `10m` |
| `GATEWAY_ANTHROPIC_STREAM_IDLE_TIMEOUT` | `30s` | Maximum idle interval between Anthropic SSE reads; maximum `10m` |
| `GATEWAY_ANTHROPIC_MAX_BODY_BYTES` | `8388608` | Maximum Anthropic request and response body; maximum 32 MiB |
| `GATEWAY_XAI_API_KEY` | unset | Optional xAI upstream credential |
| `GATEWAY_REPLICATE_API_TOKEN` | unset | Optional Replicate upstream credential; enables the native Predictions route when models and a public base URL are configured |
| `GATEWAY_REPLICATE_API_ENDPOINT` | `https://api.replicate.com` | Fixed Replicate API origin; loopback HTTP is accepted only for local testing |
| `GATEWAY_REPLICATE_MODELS` | unset | Comma-separated exact community model versions in `owner/model:version` form |
| `GATEWAY_REPLICATE_REQUEST_TIMEOUT` | `2m` | Replicate submit, poll, and cancel request timeout; maximum `10m` |
| `GATEWAY_REPLICATE_MAX_BODY_BYTES` | `1048576` | Maximum Replicate native request or response body; maximum 256 MiB |
| `GATEWAY_REPLICATE_WEBHOOK_MODE` | `disabled` | `required` injects a Gateway-owned completed-event callback into every new Replicate Prediction |
| `GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS` | unset | One active, or active and previous, comma-separated Replicate `whsec_...` secrets; never log these values |
| `GATEWAY_REPLICATE_WEBHOOK_CALLBACK_SECRET` | unset | Base64-encoded 32-byte deployment secret used to HMAC per-Job callback capabilities |
| `GATEWAY_REPLICATE_WEBHOOK_TOLERANCE` | `5m` | Accepted signed timestamp window; configurable from `1m` through `15m` |
| `GATEWAY_REPLICATE_WEBHOOK_BINDING_TTL` | `168h` | Callback capability lifetime; configurable from `1h` through `720h` |
| `GATEWAY_FAL_API_KEY` | unset | Optional fal upstream credential; enables native Queue routes when models and a public base URL are configured |
| `GATEWAY_FAL_QUEUE_ENDPOINT` | `https://queue.fal.run` | Fixed fal Queue origin; loopback HTTP is accepted only for local testing |
| `GATEWAY_FAL_MODELS` | unset | Comma-separated exact model-scoped IDs such as `fal-ai/flux/dev` |
| `GATEWAY_FAL_REQUEST_TIMEOUT` | `2m` | fal submit, status, result, and cancel timeout; maximum `10m` |
| `GATEWAY_FAL_MAX_BODY_BYTES` | `1048576` | Maximum fal native request or response body; maximum 256 MiB |
| `GATEWAY_FAL_WEBHOOK_MODE` | `disabled` | `required` injects a Gateway-owned signed callback into every new fal Queue request |
| `GATEWAY_FAL_WEBHOOK_CALLBACK_SECRET` | unset | Base64-encoded 32-byte deployment secret used to HMAC per-Job callback capabilities |
| `GATEWAY_FAL_WEBHOOK_BINDING_TTL` | `168h` | Callback capability lifetime; configurable from `1h` through `720h` |
| `GATEWAY_FAL_JWKS_URL` | `https://rest.fal.ai/.well-known/jwks.json` | Fixed fal ED25519 key-set URL; only the exact well-known path is accepted |
| `GATEWAY_FAL_JWKS_TIMEOUT` | `5s` | Bounded JWKS request timeout; maximum `1m` |
| `GATEWAY_FAL_JWKS_CACHE_TTL` | `24h` | Maximum successful key cache lifetime; HTTP cache headers may shorten it |
| `GATEWAY_FAL_JWKS_REFRESH_COOLDOWN` | `1m` | Minimum interval between signature-mismatch refresh attempts |
| `GATEWAY_RUNWAY_API_KEY` | unset | Optional Runway upstream credential; enables native video task routes when models are configured |
| `GATEWAY_RUNWAY_MODELS` | unset | Comma-separated exact logical video model IDs |
| `GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON` | unset | Optional logical-to-provider model map with exact `text_to_video` and `image_to_video` booleans |
| `GATEWAY_RUNWAY_REQUEST_TIMEOUT` | `2m` | Runway submit, poll, and cancel timeout; maximum `10m` |
| `GATEWAY_RUNWAY_MAX_BODY_BYTES` | `8388608` | Maximum native Runway request or response body; maximum 256 MiB |
| `GATEWAY_RUNWAY_POLL_INTERVAL` | `5s` | Minimum Runway task polling interval; cannot be less than 5 seconds |
| `GATEWAY_JOB_MANAGEMENT_MODE` | `disabled` | `required` enables tenant-scoped `GET /gateway/v1/jobs` and `GET /gateway/v1/jobs/{job_id}` when an asynchronous provider is enabled |
| `GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS` | unset | One active, or active and previous, comma-separated base64-encoded 32-byte HMAC secrets for opaque pagination cursors |

| `GATEWAY_PUBLIC_BASE_URL` | unset | Public HTTPS Gateway origin used for durable Prediction get/cancel URLs; loopback HTTP is accepted for local testing |
| `GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID` | unset | Current envelope-encryption write key ID; enables the database credential control plane with the keyring settings below |
| `GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS` | unset | Ordered comma-separated key IDs; index corresponds to `GATEWAY_PROVIDER_CREDENTIAL_KEY_N` |
| `GATEWAY_PROVIDER_CREDENTIAL_KEY_N` | unset | Base64-encoded 32-byte master key injected by the deployment secret manager; keep previous keys while their ciphertext exists |
| `GATEWAY_GOOGLE_REQUEST_TIMEOUT` | `2m` | Google request timeout; maximum `10m` |
| `GATEWAY_GEMINI_STREAM_IDLE_TIMEOUT` | `30s` | Maximum silence between Gemini SSE reads; maximum `10m` |
| `GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES` | `33554432` | Positive Gemini body limit up to 32 MiB |
| `GATEWAY_GEMINI_LLM_MODELS` | unset | Comma-separated exact Gemini LLM model IDs enabled for native `generateContent` and `streamGenerateContent` |
| `GATEWAY_GEMINI_LLM_MODEL_LIMITS` | unset | Comma-separated `model:maximum_input:maximum_output` token bounds; required for every enabled Gemini LLM model when billing is required |
| `GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT` | `2m` | OpenAI/xAI image request timeout; maximum `10m` |
| `GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES` | `1048576` | Positive OpenAI Images JSON body limit up to 1 MiB |
| `GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES` | `67108864` | Image edit body limit; maximum 256 MiB |
| `GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS` | `8` | Concurrent multipart edit spool limit; maximum 128 |
| `GATEWAY_BILLING_MODE` | `disabled` | `disabled` preserves BYOK pass-through; `required` enforces price and Wallet settlement |
| `GATEWAY_MINIMUM_MARGIN_BPS` | `0` | Minimum sale margin from 0 to 10000 basis points |
| `GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES` | `33554432` | Maximum native response snapshot size; maximum 256 MiB |
| `GATEWAY_RECONCILIATION_INTERVAL` | `5s` | Billing-required worker polling interval; maximum 1 minute |
| `GATEWAY_RECONCILIATION_LEASE` | `30s` | Durable task lease; maximum 10 minutes |
| `GATEWAY_RECONCILIATION_BASE_BACKOFF` | `5s` | Initial retry backoff; maximum 1 hour |
| `GATEWAY_RECONCILIATION_MAX_BACKOFF` | `1h` | Retry backoff ceiling; maximum 24 hours |
| `GATEWAY_RECONCILIATION_BATCH_SIZE` | `10` | Tasks claimed per cycle; range 1–100 |
| `GATEWAY_RECONCILIATION_MAX_ATTEMPTS` | `5` | Attempts before manual review; range 1–100 |
| `GATEWAY_RATE_LIMIT_MODE` | `disabled` | `disabled` ignores stored policies; `required` enforces them and fails closed when Redis is unavailable |
| `GATEWAY_REDIS_URL` | unset | Secret Redis URL required by rate-limit `required` mode |
| `GATEWAY_RATE_LIMIT_TIMEOUT` | `100ms` | Redis command timeout; maximum 1 second |
| `GATEWAY_PROVIDER_HEALTH_MODE` | `disabled` | `required` enables the distributed Provider channel circuit breaker and fails closed when Redis is unavailable |
| `GATEWAY_PROVIDER_HEALTH_WINDOW` | `1m` | Rolling outcome window from 10 seconds to 1 hour |
| `GATEWAY_PROVIDER_HEALTH_BUCKET` | `10s` | Fixed Redis score bucket from 1 second to 1 minute; must divide the window |
| `GATEWAY_PROVIDER_HEALTH_MINIMUM_SAMPLES` | `10` | Dispatch outcomes required before a closed circuit may open |
| `GATEWAY_PROVIDER_HEALTH_FAILURE_THRESHOLD_BPS` | `5000` | Failure ratio threshold in basis points, from 1 to 10000 |
| `GATEWAY_PROVIDER_HEALTH_OPEN_DURATION` | `30s` | Initial circuit-open duration |
| `GATEWAY_PROVIDER_HEALTH_MAXIMUM_OPEN_DURATION` | `5m` | Exponential reopen-duration ceiling |
| `GATEWAY_PROVIDER_HEALTH_PROBE_LEASE` | `10s` | Distributed half-open single-probe lease; no longer than the initial open duration |
| `GATEWAY_PROVIDER_HEALTH_COMMAND_TIMEOUT` | `100ms` | Health Redis command timeout; maximum 1 second |
| `GATEWAY_PROVIDER_HEALTH_KEY_PREFIX` | `gateway:provider-health:v1` | Non-secret Redis key namespace using channel hash tags |
| `GATEWAY_IMAGE_STORAGE_MODE` | `provider` | `provider` preserves native Provider results; `managed` persists generated images and returns CDN URLs |
| `GATEWAY_IMAGE_STORAGE_ENDPOINT` | unset | S3-compatible HTTPS endpoint; loopback HTTP is accepted only for local testing |
| `GATEWAY_IMAGE_STORAGE_REGION` | unset | S3 signing region; use `auto` for R2 |
| `GATEWAY_IMAGE_STORAGE_BUCKET` | unset | Private internal object bucket name |
| `GATEWAY_IMAGE_STORAGE_ACCESS_KEY_ID` | unset | Secret-manager supplied least-privilege S3/R2 access key ID |
| `GATEWAY_IMAGE_STORAGE_SECRET_ACCESS_KEY` | unset | Secret-manager supplied S3/R2 secret access key; never logged |
| `GATEWAY_IMAGE_STORAGE_CDN_BASE_URL` | unset | Public HTTPS CDN origin used to construct result URLs |
| `GATEWAY_IMAGE_STORAGE_MAX_IMAGES` | `10` | Maximum managed images in one Provider response; maximum 100 |
| `GATEWAY_IMAGE_STORAGE_MAX_IMAGE_BYTES` | `33554432` | Maximum decoded bytes per image; maximum 256 MiB |
| `GATEWAY_IMAGE_STORAGE_MAX_TOTAL_BYTES` | `67108864` | Maximum decoded bytes across one response; maximum 512 MiB |
| `GATEWAY_IMAGE_STORAGE_FETCH_TIMEOUT` | `30s` | Provider asset download timeout; maximum 5 minutes |
| `GATEWAY_IMAGE_STORAGE_UPLOAD_TIMEOUT` | `1m` | Object upload/readiness timeout; maximum 10 minutes |
| `GATEWAY_IMAGE_STORAGE_TEMP_DIR` | system temp | Directory for bounded mode-0600 image spools |
| `GATEWAY_IMAGE_STORAGE_FETCH_ORIGINS_OPENAI` | unset | Exact comma-separated HTTPS origins allowed for OpenAI result URL collection |
| `GATEWAY_IMAGE_STORAGE_FETCH_ORIGINS_XAI` | unset | Exact comma-separated HTTPS origins allowed for xAI result URL collection |
| `GATEWAY_IMAGE_STORAGE_FETCH_ORIGINS_GOOGLE` | unset | Exact comma-separated HTTPS origins allowed for Google result URL collection |
| `GATEWAY_TELEMETRY_MODE` | `disabled` | `optional` or `required` enables process-owned OTLP tracing and metrics; runtime collector failures never fail traffic or readiness |
| `GATEWAY_TELEMETRY_OTLP_ENDPOINT` | unset | OTLP HTTP/protobuf base endpoint; production requires HTTPS and local development may use loopback HTTP |
| `GATEWAY_TELEMETRY_OTLP_AUTHORIZATION` | unset | Secret-manager supplied collector authorization value; never logged |
| `GATEWAY_TELEMETRY_SERVICE_NAME` | `native-ai-gateway` | Bounded OpenTelemetry service name |
| `GATEWAY_TELEMETRY_SERVICE_VERSION` | `development` | Bounded deployed service version |
| `GATEWAY_TELEMETRY_ENVIRONMENT` | `development` | Bounded deployment environment resource attribute |
| `GATEWAY_TELEMETRY_SAMPLE_RATIO` | `0.1` | Parent-based trace sampling ratio from 0 through 1 |
| `GATEWAY_TELEMETRY_EXPORT_INTERVAL` | `30s` | Periodic metric export interval from 1 second through 10 minutes |
| `GATEWAY_TELEMETRY_EXPORT_TIMEOUT` | `10s` | Per-export timeout up to 1 minute |
| `GATEWAY_TELEMETRY_SHUTDOWN_TIMEOUT` | `5s` | Bounded metric flush and trace shutdown timeout up to 1 minute |
| `GATEWAY_TRUSTED_PROXY_CIDRS` | unset | Comma-separated reverse-proxy CIDRs allowed to supply `Forwarded` or `X-Forwarded-For` |

Invalid configuration fails before binding a listener. Logs are structured JSON and intentionally omit headers, cookies, query strings, and request/response bodies.

## Gemini LLM generateContent and streaming

Set `GATEWAY_GEMINI_LLM_MODELS` to enable exact native Gemini LLM models. BYOK requests and Google error responses remain native JSON. In billing-required mode every model needs a `GATEWAY_GEMINI_LLM_MODEL_LIMITS` entry, and requests must contain a positive `generationConfig.maxOutputTokens` within that limit.

Publish a protocol-isolated price with `gateway-chat-price -protocol gemini -operation chat.completions`. Before dispatch, the Gateway reserves the request-byte input upper bound plus the requested output bound across Wallet, quota, and Google channel spend cap. Successful settlement strictly validates `usageMetadata`: cached tokens are priced as a subset of prompt input, tool-use prompt tokens use the input rate, and thought tokens use the output rate without double counting.

Confirmed non-2xx responses release the reservation. Timeout, connection loss, truncated responses, missing/invalid/excess usage, and settlement failure retain it for durable reconciliation. `Idempotency-Key` is isolated by protocol and operation and replays the stored native response without another Provider call or Ledger mutation. Prompts, system instructions, tool payloads, candidates, and thought content are never stored for billing.

`POST /v1beta/models/{model}:streamGenerateContent?alt=sse` relays Google native SSE bytes with bounded backpressure. Managed streams use the same maximum-cost reservation, then Capture only after a clean terminal frame with strict cumulative `usageMetadata`. Streaming transcripts are never retained or replayed. Client disconnect, write failure, upstream reset or idle timeout, malformed events, and missing terminal usage keep the reservation for reconciliation; only a confirmed HTTP non-2xx before the stream starts releases it.

```bash
gateway-chat-price \
  -protocol gemini -operation chat.completions \
  -channel-id channel_00000000000000000000000000000003 \
  -model gemini-2.5-pro \
  -publication-key gemini-2.5-pro-2026-08-21 \
  -effective-from 2026-08-21T00:00:00Z \
  -input-cost 1000000 -input-sale 1200000 \
  -cached-input-cost 500000 -cached-input-sale 600000 \
  -output-cost 3000000 -output-sale 3600000
```

## OpenAI Chat Completions

Set `GATEWAY_OPENAI_CHAT_MODELS` to enable the exact non-streaming models accepted by the native route. BYOK mode preserves native pass-through behavior. In billing-required mode, every enabled model also needs a `GATEWAY_OPENAI_CHAT_MODEL_LIMITS` entry and each request must provide exactly one of `max_completion_tokens` or legacy `max_tokens`.

Managed Chat pricing is immutable and expressed in `USD_TICKS` per one million tokens for input, cached input, and output. Publish it with `gateway-chat-price`; sale rates must satisfy `GATEWAY_MINIMUM_MARGIN_BPS`. Reservation uses the request byte length as a conservative input-token upper bound plus the caller's output-token bound. Final settlement uses only validated native `usage.prompt_tokens`, `prompt_tokens_details.cached_tokens`, and `completion_tokens`, applying ceiling division independently to each usage class.

`Idempotency-Key` replays the stored native response without another Provider call or Ledger mutation. Confirmed non-2xx responses release the reservation. Timeout, connection loss, truncated responses, and missing or invalid usage retain funds in `RECONCILING`; the worker never repeats the Provider request and moves irrecoverable outcomes to manual review after bounded attempts.

```bash
gateway-chat-price \
  -channel-id channel_00000000000000000000000000000001 \
  -model gpt-4.1 \
  -publication-key gpt-4.1-2026-08-21 \
  -effective-from 2026-08-21T00:00:00Z \
  -input-cost 1000000 -input-sale 1200000 \
  -cached-input-cost 500000 -cached-input-sale 600000 \
  -output-cost 3000000 -output-sale 3600000
```

Both BYOK and managed modes support native `stream: true` SSE. Managed requests must explicitly send `stream_options.include_usage=true`; the Gateway never mutates the request to add it. SSE payloads and `[DONE]` are relayed byte-for-byte with bounded incremental buffering. Only the terminal usage event digest and token counts are retained—never the transcript.

Streaming settlement captures validated terminal usage exactly once. A client disconnect, upstream reset or idle timeout, missing `[DONE]`, malformed/duplicate/missing usage, and downstream write failure retain the reservation for durable reconciliation; the Gateway never repeats the Provider request. Completed streaming idempotency keys are deliberately non-replayable because transcripts are not stored: reuse returns a conflict without another Provider call or Ledger mutation.

For protocol-preserving multi-provider routing, set `GATEWAY_OPENAI_CHAT_ROUTES_JSON` instead of `GATEWAY_OPENAI_CHAT_MODELS`. Each logical model declares exact OpenAI/xAI candidates, provider models, channels, capabilities, and one of `fixed`, `priority`, `weighted`, or `lowest_cost`. Example:

```json
[
  {
    "model": "logical-chat",
    "owner": "gateway",
    "policy": "lowest_cost",
    "maximum_input_tokens": 128000,
    "maximum_output_tokens": 16384,
    "candidates": [
      {
        "id": "candidate_openai",
        "provider": "openai",
        "provider_model": "gpt-4.1",
        "channel_id": "channel_00000000000000000000000000000001",
        "priority": 1,
        "enabled": true,
        "streaming": true,
        "tools": true,
        "json_mode": true
      },
      {
        "id": "candidate_xai",
        "provider": "xai",
        "provider_model": "grok-4",
        "channel_id": "channel_00000000000000000000000000000002",
        "priority": 2,
        "enabled": true,
        "streaming": true,
        "tools": true,
        "json_mode": true
      }
    ]
  }
]
```

The Gateway rewrites only the top-level `model` string and preserves all other request bytes and native JSON/SSE response bytes. Candidate capability, credential, circuit, exact price, and spend-cap failures may select another candidate only before a Provider dispatch. Once a request may have reached a Provider—including HTTP 429/5xx, timeout, reset, or stream failure—the Gateway never calls a second Provider. Managed idempotency replay retains the originally committed route and immutable route/price evidence.

Cross-protocol conversion to Gemini or Anthropic and exact tokenizer-based preflight counting remain outside this plan.

## OpenAI Responses

Set `GATEWAY_OPENAI_RESPONSES_MODELS` to enable native non-streaming and SSE streaming `POST /v1/responses`. The Gateway preserves typed input/output items, function tools, reasoning options, future fields, success bodies, Provider error bodies, and SSE event bytes without converting them to Chat Completions.

```python
from openai import OpenAI
client = OpenAI(api_key="SERVICE_API_KEY", base_url="https://gateway.example/v1")
response = client.responses.create(model="gpt-4.1", input="hello", max_output_tokens=1024)
print(response.output_text)
```

```python
stream = client.responses.create(
    model="gpt-4.1",
    input="hello",
    stream=True,
    max_output_tokens=1024,
)
for event in stream:
    if event.type == "response.output_text.delta":
        print(event.delta, end="")
```

BYOK mode preserves native pass-through behavior. In billing-required mode, every enabled model needs a `GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS` entry and every request must provide a positive `max_output_tokens` no greater than its model limit. Publish an operation-isolated immutable price with `gateway-chat-price -operation responses.create`; input, cached input, and output rates use `USD_TICKS` per million tokens.

For native-compatible multi-provider routing, set `GATEWAY_OPENAI_RESPONSES_ROUTES_JSON` instead of the two legacy model variables. Each candidate declares its OpenAI or xAI Provider model, channel, policy metadata, and exact `streaming`, `function_tools`, `web_search`, `x_search`, `code_interpreter`, `image_generation`, `json_mode`, and `stored_response` capabilities. Only the top-level model string is rewritten; typed items, tool data, future JSON fields, Provider JSON, and SSE bytes remain native.

Capability, credential, health, exact-price, margin, and spend-cap rejection may move to another candidate only before dispatch. A Provider HTTP response, timeout, reset, malformed response, incomplete stream, or client disconnect never triggers a second Provider call. Managed charges record immutable candidate, Provider model, policy, rank, and price-evaluation time using `openai-responses-route-v1`; idempotency replay uses the original logical request fingerprint and route-independent lookup.

`previous_response_id` and `background=true` are rejected until durable Provider response ownership and background Job reconciliation are implemented. Unknown tool types also fail closed instead of being sent to a guessed Provider.

### Anthropic Messages foundation

Set `GATEWAY_ANTHROPIC_MESSAGES_MODELS` and `GATEWAY_ANTHROPIC_API_KEY` to enable native non-streaming `POST /v1/messages`. Official Anthropic clients can use the Gateway URL as `base_url` and a service key as their API key. The Gateway validates `anthropic-version`, bounded `anthropic-beta` headers, exact model authorization, and then replaces the service credential with the upstream credential at the fixed Anthropic origin. Native success and error bodies are returned without schema conversion.

In billing-required mode, every enabled model needs a `GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS` entry. Publish an immutable `anthropic/messages.create` token price with `gateway-chat-price`; input, cache-read (`cached-input-*`), cache-write (`cache-write-*`), and output cost/sale rates are expressed in `USD_TICKS` per million tokens. The Gateway reserves the most expensive possible input class plus `max_tokens`, then captures native `usage` or releases a known non-2xx response. Missing or invalid usage and uncertain Provider outcomes enter reconciliation.

`stream=true` is relayed as byte-preserving Anthropic SSE. A stream is captured only after valid `message_start` input/cache usage, cumulative `message_delta.usage.output_tokens`, and `message_stop`; client disconnect, idle timeout, Provider error event, malformed or incomplete streams remain reserved for reconciliation. Streaming transcripts are not retained or replayed.

The Gateway reserves the request byte upper bound plus the output limit before Provider dispatch, then captures strict native `usage.input_tokens`, `input_tokens_details.cached_tokens`, and `output_tokens`. Reasoning tokens must be a valid subset of output tokens and are not charged twice. Confirmed non-2xx responses release the reservation. Timeout, response loss, invalid or missing usage, and settlement failure keep it for durable reconciliation. `Idempotency-Key` replays the bounded native terminal response without another Provider call or Ledger mutation.

For `stream=true`, the Gateway relays native `event` and `data` frames and settles only one valid `response.completed.response.usage`. A failed/incomplete/error event, missing or duplicate terminal, invalid usage, Provider reset/idle timeout, or client write failure retains the reservation for reconciliation. Completed stream transcripts are not stored, so a reused streaming `Idempotency-Key` returns a conflict without Provider redispatch or Ledger mutation.

Response retrieval/deletion/cancel, background mode, stored conversation lifecycle, and built-in-tool-specific pricing remain deferred.

```python
from openai import OpenAI

client = OpenAI(api_key="SERVICE_API_KEY", base_url="https://gateway.example/v1")
response = client.chat.completions.create(
    model="gpt-4.1",
    messages=[{"role": "user", "content": "Hello"}],
)
print(response.choices[0].message.content)
```

API Keys using an explicit model allowlist require `openai:chat.completions:<model>`. Requests preserve unknown native JSON fields, but compressed bodies and responses above the configured bound are rejected. The Gateway never logs messages, prompts, tool arguments, authorization headers, or Provider credentials.

## Asynchronous Job management

When Job management is enabled, a service API key can read only the asynchronous Jobs created by that exact organization, project, and key. Responses deliberately omit prompts, raw provider payloads, provider Job IDs, credentials, upstream costs, and internal ledger identities.

```http
GET /gateway/v1/jobs?protocol=fal&status=SUCCEEDED&settlement_state=SETTLED&limit=25
Authorization: Bearer SERVICE_API_KEY

GET /gateway/v1/jobs/job_...
Authorization: Bearer SERVICE_API_KEY
```

Lists use signed opaque keyset cursors. Keep the active cursor secret first and the previous secret second during rotation; remove the previous secret only after the maximum client pagination window has elapsed.

## OpenTelemetry observability

Telemetry is disabled by default. Both enabled modes export OTLP HTTP/protobuf traces and metrics; `required` additionally treats exporter construction failure as a startup error. Collector connection, timeout, 401, 429, or 5xx failures after startup are isolated from native responses, Wallet/Ledger settlement, managed assets, readiness, and reconciliation leases. Graceful shutdown flushes metrics and traces only within `GATEWAY_TELEMETRY_SHUTDOWN_TIMEOUT`.

The stable instruments are `gateway.http.server.*`, `gateway.authentication.decisions`, `gateway.routing.decisions`, `gateway.provider.*`, `gateway.billing.transitions`, `gateway.storage.operations`, `gateway.reconciliation.tasks`, and `gateway.jobs.transitions`. Their labels use bounded protocol, operation, policy, Provider, stage, status class, rejection, transition, and outcome values. Never add prompts, bodies, headers, query strings, raw URLs or errors, credentials, tenant/API Key/request/charge/object identities, model/channel/candidate IDs, prices, balances, margins, or limits as telemetry attributes. Inbound propagation reads only W3C `traceparent` and `tracestate`; baggage is ignored and inbound trace headers are not forwarded to Providers. Active request logs include trace and span IDs for correlation.

## Durable asynchronous Job foundation

Runway native video tasks are available in BYOK mode through `POST /v1/text_to_video`, `POST /v1/image_to_video`, `GET /v1/tasks/{id}`, and `DELETE /v1/tasks/{id}`. Official SDKs can use the Gateway as their custom base URL and a service key as the API key; the Gateway requires `X-Runway-Version: 2024-11-06`, keeps the upstream task ID private, and returns a tenant-owned Gateway Job ID. Example configuration:

```sh
export GATEWAY_RUNWAY_API_KEY='your-runway-provider-key'
export GATEWAY_RUNWAY_MODELS='gateway-video'
export GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON='{"gateway-video":{"provider_model":"gen4_turbo","text_to_video":true,"image_to_video":true}}'
```

Runway output URLs are returned in provider mode and normally expire after roughly 24–48 hours; callers must download results they need to retain. Managed video storage, upload proxying, and video credit billing are deferred. When billing is required, video submit fails before Job creation or Provider dispatch.

The protocol-neutral Job core persists tenant ownership, immutable routing/charge identity, a separate internal Provider Job identifier, state transitions, append-only events, polling leases, cancellation intent, and terminal result snapshots in PostgreSQL. This release intentionally exposes no new HTTP route; the subsequent Replicate and fal native facades consume this application contract.

`PENDING`, `QUEUED`, `PROCESSING`, and `RECONCILING` Jobs are non-terminal. `SUCCEEDED`, `FAILED`, and `CANCELED` are immutable terminal states. A submit intent is persisted before the single Provider call. If that call or its response is uncertain, the Job moves to `RECONCILING`; the Gateway never submits it again or falls back to another Provider. Expired submit/poll leases are recovered after restart with `FOR UPDATE SKIP LOCKED` claims.

Terminal observations create a durable settlement intent. Success captures the associated charge, confirmed failure or cancellation releases it, and unknown outcomes retain the reservation. Billing completion is idempotent, so a crash after the Ledger commit but before marking the Job settled safely converges on retry. Polling, cancellation, and future signed webhooks use the same compare-and-set observation path; duplicates are no-ops and conflicting terminal results are rejected. Public Job views omit Provider Job IDs, channel/lease/charge metadata, credentials, tenant identity, prices, balances, and raw Provider errors. Result bodies and allowlisted headers are size-bounded.

## API Key rate limiting

Create an opt-in limited Key with the provisioning CLI:

```bash
go run ./cmd/gateway-key \
  -name development \
  -project-id project_legacy \
  -requests-per-minute 60 \
  -burst 10 \
  -allow-model openai:image.generate:gpt-image-1 \
  -allow-model gemini:image.generate:gemini-image
```

Both policy values must be omitted for an unlimited Key or supplied together with `1 <= burst <= requests-per-minute <= 1000000`. Enable distributed enforcement with:

```bash
export GATEWAY_RATE_LIMIT_MODE=required
export GATEWAY_REDIS_URL='redis://127.0.0.1:56379/0'
```

The token bucket is keyed only by the non-secret API Key ID and is shared across Gateway instances. Authentication failures do not consume a token. Authenticated generation, edit, Gemini, model-list and idempotent replay requests each consume one token before body parsing, price lookup, Wallet reservation or Provider dispatch. Rejected requests return native `429` errors plus `Retry-After` and `X-RateLimit-*` headers and never trigger routing fallback. Redis timeout or failure in required mode returns native `503`; `/health/live` stays live while `/health/ready` reports unavailable.

`--allow-model` is optional and repeatable. Omitting it preserves backward-compatible access to all registered models; production Keys should use an explicit least-privilege allowlist. Supplying it changes the Key to an exact logical allowlist using `protocol:operation:model`; supported operations include `openai:image.generate`, `openai:image.edit`, `openai:chat.completions`, and `gemini:image.generate`. Wildcards and Provider-native model names are not expanded. A denied request consumes its rate-limit token but returns native `403` before Provider dispatch. `/v1/models` returns only the intersection of the Key permissions, model capabilities and configured Provider credentials.

## API Key network restrictions

Add one or more canonical IPv4 or IPv6 networks while provisioning a Key:

```bash
go run ./cmd/gateway-key \
  -name production \
  -allow-cidr 203.0.113.0/24 \
  -allow-cidr 2001:db8:1234::/48
```

Omitting `--allow-cidr` allows all source networks for backward compatibility. A restricted Key is checked after credential authentication and before rate limiting, body parsing, replay, billing, or provider dispatch. Denials return OpenAI `403 permission_error/network_not_allowed` or Gemini `403 PERMISSION_DENIED` and consume no rate-limit token.

By default the Gateway uses the direct TCP peer and ignores all forwarding headers. Behind an ingress, configure only the ingress egress networks, for example `GATEWAY_TRUSTED_PROXY_CIDRS=10.20.0.0/16`. Requests from those trusted peers must contain exactly one of RFC 7239 `Forwarded` or `X-Forwarded-For`; the Gateway strips trusted hops right-to-left. Missing, malformed, or ambiguous chains fail closed for restricted Keys while health endpoints and unrestricted Keys remain available. Never set this value to arbitrary client networks or `0.0.0.0/0`.

## Hierarchical cost quotas

Billing-required deployments can apply UTC calendar-day or calendar-month sale-cost limits to an organization, project, API Key, or exact logical model. Policies are additive: every matching organization, project, Key, and model policy must have capacity. A request reserves its maximum sale price in every matching bucket in the same PostgreSQL transaction as its Wallet reservation and charge; success captures it, known failure releases it, and unknown Provider outcomes retain it until reconciliation.

Use the operator CLI with integer `USD_TICKS` amounts. An organization-wide daily policy is:

```bash
go run ./cmd/gateway-quota \
  -scope organization \
  -organization-id org_example \
  -period day \
  -limit 1000000 \
  -actor operator@example.com \
  -reason 'daily managed-service budget'
```

An API Key and logical-model monthly policy additionally supplies `-project-id`, `-api-key-id`, `-protocol`, `-operation`, and `-model`. Repeat the same dimension to update its limit; the returned `quota_...` ID remains stable and an append-only audit event records the new version. Disable it with `-action disable -policy-id quota_... -actor ... -reason ...`.

Quota exhaustion returns native `429` (`quota_exceeded` for OpenAI, `RESOURCE_EXHAUSTED` for Gemini), `Retry-After`, and `X-Quota-Reset` before Provider dispatch. Responses do not disclose budget amounts or remaining spend. Deployments without active policies remain unlimited, and billing-disabled BYOK mode does not consult quota tables.

## Provider channel spend caps

Billing-required deployments can also cap upstream cost per Provider channel for each UTC day or month. Unlike customer quotas, an exhausted channel is candidate-specific: its Billing transaction is rolled back, the Provider is not called, and priority routing evaluates the next configured candidate. If every candidate is unavailable or exhausted, clients receive the existing native provider-unavailable response without internal cost or channel-budget details.

Configure an integer `USD_TICKS` cap with the operator CLI:

```bash
go run ./cmd/gateway-spend-cap \
  -channel-id channel_00000000000000000000000000000001 \
  -period day \
  -limit 500000 \
  -actor operator@example.com \
  -reason 'daily OpenAI purchasing budget'
```

Repeating the channel/period updates the stable policy ID and appends an audit version. Disable with `-action disable -policy-id spcap_... -actor ... -reason ...`. Day and month policies may coexist and both must have capacity. Estimated Provider cost is reserved atomically with the charge, Wallet, and customer quota; actual cost is captured, known failures release it, and uncertain outcomes remain reserved until reconciliation. Channels without policies are unlimited.

## Lowest-cost image routing

An image model route may use the `lowest_cost` policy in billing-required mode. The Gateway filters candidates without an executor, active channel credential, valid exact price, or required margin, then compares every remaining candidate at one UTC evaluation timestamp. Selection uses estimated upstream cost first, configured priority second, and candidate ID last, so ties are deterministic; a lower customer sale price never overrides a higher upstream cost.

The selected price ID, cost, sale, currency, channel, evaluation timestamp, policy, and cost rank are bound to Billing `Begin` and stored with the immutable charge. If the price snapshot changes between quote and reserve, the Gateway re-evaluates all candidates once before dispatch. A second price race fails closed. Provider channel spend-cap exhaustion moves to the next-lowest candidate, while Wallet, customer quota, database, and post-dispatch failures never trigger cost-routing fallback.

Route publishers must prepare every candidate's active credential, exact channel price, minimum margin, and optional spend cap before enabling `lowest_cost`. Client responses and routing skip logs do not expose prices, margins, credentials, request content, balances, or remaining limits.

## Weighted image routing

An image model route may instead use `weighted` with a positive integer weight on every enabled candidate. The Gateway removes candidates without a configured executor or active channel credential before drawing, canonicalizes candidates by ID, and uses cryptographic rejection sampling so integer intervals are not affected by modulo bias. Weight configuration is bounded to 128 candidates, `1,000,000,000` per candidate, and `4,000,000,000` total.

After a draw, an unavailable exact price, minimum-margin violation, or exhausted Provider channel spend cap removes that candidate. The remaining weights are re-normalized and another candidate is drawn; no candidate is attempted twice. Wallet, customer quota, database, idempotency, and entropy failures stop globally, and no redraw occurs after Billing reservation or Provider dispatch. Terminal idempotency replay returns the stored native response without consuming entropy or evaluating current route state.

Weighted charges preserve the selected channel and price snapshot plus `routing_policy=weighted` and the bounded attempt rank. Client responses and logs never expose configured weights, random draws, prices, margins, credentials, request content, balances, or remaining limits. Route publishers must validate every candidate's credential, exact price, margin, and optional spend cap before rollout.

## Provider channel health and circuit breaking

Set `GATEWAY_PROVIDER_HEALTH_MODE=required` to share Provider channel health across Gateway instances through Redis. Only actual upstream dispatch contributes an outcome: 2xx is success; 429, 5xx, typed timeout, and connection loss are failures; other Provider 4xx and client cancellation are neutral. Gateway authentication, authorization, parsing, price, Wallet, quota, spend-cap, credential, and routing failures never affect Provider health.

After the configured minimum sample count, a channel opens when its rolling failure ratio reaches the threshold. OPEN candidates are removed before pricing, weighted drawing, or Billing reservation. Once the open period expires, Redis atomically grants one HALF_OPEN probe lease across all instances. Probe success closes and resets the circuit; probe failure reopens it with exponential duration up to the configured maximum. A probe that cannot dispatch because of a pre-dispatch failure is released without recording a Provider failure.

Health filtering applies to fixed, priority, lowest-cost, and weighted routes. It never causes a second Provider call after Billing reservation or dispatch, and terminal idempotency replay does not inspect or mutate health state. Required-mode Redis failure returns a native 503 before dispatch and makes `/health/ready` unavailable while `/health/live` remains healthy. Redis keys and bounded logs contain channel metadata and outcome categories only—never credentials, prompts, response bodies, raw errors, customer identifiers, prices, balances, or limits.

## Provider credentials

Provider credentials are optional until their adapters are enabled. Inject them through environment variables backed by your deployment platform's secret manager; never commit them to source files or Compose configuration.

```bash
export GATEWAY_GOOGLE_API_KEY='...'
export GATEWAY_OPENAI_API_KEY='...'
export GATEWAY_XAI_API_KEY='...'
```

Provider credentials are held in an opaque, provider-scoped registry. They are not placed in the general process configuration and are never returned through an API. Outbound request preparation clones the request, removes inbound `Authorization`, API-key headers, cookies, and sensitive query parameters, then applies only the credential scoped to the selected provider. Missing credentials and provider-scope mismatches fail before any network request.

For managed channel credentials, configure an envelope-encryption keyring. The current key encrypts new versions while every listed previous key remains available for decrypting existing versions and replaying lifecycle operations. Key IDs are metadata; each corresponding base64 key is a secret. Do not place either Provider credentials or master keys in manifests, Terraform state, CI output, or shell history.

```bash
export GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID='2026-08'
export GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS='2026-07,2026-08'
export GATEWAY_PROVIDER_CREDENTIAL_KEY_0='base64-encoded-32-byte-previous-key'
export GATEWAY_PROVIDER_CREDENTIAL_KEY_1='base64-encoded-32-byte-current-key'
```

Stage a credential from a protected file or secret-manager stream, then activate it atomically:

```bash
gateway-provider-credential \
  -action stage \
  -channel-id channel_00000000000000000000000000000001 \
  -provider openai \
  -actor operator@example.com \
  -reason 'scheduled rotation' \
  -operation-key rotation-2026-08-stage \
  < /run/secrets/openai-provider-key

gateway-provider-credential \
  -action activate \
  -credential-id pcred_... \
  -actor operator@example.com \
  -reason 'scheduled rotation' \
  -operation-key rotation-2026-08-activate
```

`stage`, `activate`, `retire`, and `list` output only credential metadata. Plaintext, ciphertext, nonce, wrapped data key, master key ID, hashes, prefixes, and lengths are never returned. Activation retires the previous active version in the same transaction. Database credentials override environment credentials; the three built-in legacy channels fall back to their Provider environment variable only when no active database version exists. Other channels never receive environment fallback. A missing or undecryptable active credential is treated as unavailable before dispatch and is not silently replaced with a stale credential.

Treat `actor`, `reason`, and `operation-key` as non-secret audit metadata; never place a Provider key, master key, request content, or customer identifier in those fields. Retired encrypted rows and lifecycle events are retained for audit and must not be deleted manually.

## Gemini native API

The Gateway supports the non-streaming Gemini Developer API route:

```text
POST /v1beta/models/{model}:generateContent
```

Configure a Google provider credential, then use a service API key with the official Google Gen AI SDK:

```bash
export GATEWAY_GOOGLE_API_KEY='your-google-provider-key'
```

```python
from google import genai
from google.genai import types

client = genai.Client(
    api_key="SERVICE_API_KEY",
    http_options=types.HttpOptions(
        base_url="http://127.0.0.1:8080",
        api_version="v1beta",
    ),
)

response = client.models.generate_content(
    model="gemini-image",
    contents="Draw a cat astronaut",
    config=types.GenerateContentConfig(response_modalities=["IMAGE"]),
)
```

The Gateway authenticates the service key, removes it from the outbound request, and applies only `GATEWAY_GOOGLE_API_KEY` to the fixed Google origin. Google success and error JSON bodies are passed through without schema conversion. Redirects and automatic retries are disabled.

In `GATEWAY_BILLING_MODE=required`, only models registered with the Gemini `image.generate` capability are accepted on this route. The built-in `gemini-image` route uses the Google channel. `generationConfig.imageConfig.aspectRatio` and `imageSize` select the exact price; omitted values use `default` and the initial quantity is one. Unregistered text models fail closed instead of passing through without charge. Billing-disabled self-hosted deployments retain raw Gemini pass-through behavior.

Streaming, model listing, file upload, managed image storage, and cross-provider conversion are not included in this phase. Official Python and JavaScript SDK version compatibility will be maintained in the separate conformance repository.

## OpenAI Images native API

The Gateway supports the OpenAI-compatible image generation route:

```text
POST /v1/images/generations
```

The authenticated `GET /v1/models` endpoint returns configured OpenAI/xAI image models in the native OpenAI list schema. Models are sorted by ID and omitted when their Provider credential is not configured.

The request's exact `model` value selects a provider. Phase 0 does not guess by prefix or fall back to another provider.

| Model | Provider | Upstream credential |
|---|---|---|
| `gpt-image-1` | OpenAI | `GATEWAY_OPENAI_API_KEY` |
| `grok-imagine-image-quality` | xAI | `GATEWAY_XAI_API_KEY` |

Image models are logical protocol models backed by one or more immutable channel candidates. Each candidate identifies a Provider, provider-native model, pricing channel, enabled state, and priority. Built-in models currently use a single `fixed` candidate. A `priority` model evaluates enabled candidates by numeric priority and candidate ID. Before any reservation or Provider call, candidates without an executor, configured credential, or valid exact price/margin are skipped. The first candidate whose Wallet reservation succeeds is fixed for that request. The selected provider model is applied only to the outbound request; the client-visible model and idempotency identity remain logical. `/v1/models` lists a logical model when at least one ordered candidate has a configured Provider credential.

Fallback is strictly pre-dispatch. Once a Wallet reservation succeeds, the Gateway calls exactly one Provider and never tries another candidate after credential races, timeout, connection loss, panic, or any HTTP response. Those outcomes use the existing release or reconciliation paths, preventing ambiguous duplicate generation and charging. A `fixed` model never selects an alternate candidate. Weighted, lowest-cost, and health-aware routing remain separate plans.

Python:

```python
from openai import OpenAI

client = OpenAI(
    api_key="SERVICE_API_KEY",
    base_url="http://127.0.0.1:8080/v1",
)

response = client.images.generate(
    model="gpt-image-1",
    prompt="Draw a cat astronaut",
)
```

JavaScript:

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: "SERVICE_API_KEY",
  baseURL: "http://127.0.0.1:8080/v1",
});

const response = await client.images.generate({
  model: "grok-imagine-image-quality",
  prompt: "Draw a cat astronaut",
});
```

In the default `provider` storage mode, the Gateway preserves native success/error response bytes, including URL, `b64_json`, usage, and Provider extension fields. Provider credentials are applied only to their fixed origins. Redirects and post-dispatch Provider retries remain excluded; billing is opt-in through the required mode described below.

## Managed image storage

`GATEWAY_IMAGE_STORAGE_MODE=managed` copies OpenAI/xAI URL or Base64 image results and Gemini inline image parts to an S3-compatible bucket, then stores and returns a native response containing the configured CDN URL. Unknown response fields, result order, text parts and revised prompts are preserved. The final managed response—not the temporary Provider response—is captured for idempotent replay, so replay performs no Provider fetch or object upload.

Provider URL collection is opt-in per Provider through exact HTTPS origin lists. The collector rejects URL credentials, IP literals, redirects, DNS answers containing private, loopback, link-local, multicast or reserved addresses, oversized bodies, and mismatched image MIME/magic bytes. Inbound authorization, cookies and tracing headers are never forwarded. Images are streamed through bounded mode-`0600` temporary files and uploaded under deterministic content-addressed keys; prompts, customer IDs and original URLs are not object metadata.

Managed mode adds object-store readiness to `/health/ready` while `/health/live` remains process-only. A Provider success followed by storage uncertainty is not refunded: the original bounded Provider response enters durable reconciliation, a leased worker retries the idempotent upload, and Capture stores the managed response only after persistence succeeds. Repeated failures remain reserved and eventually require manual review. Set the mode back to `provider` to bypass storage without deleting existing objects or asset history. Cloud deployments must configure bucket lifecycle, a CDN domain, least-privilege credentials and `GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES` large enough for the configured decoded response limit.

## Image editing

The Gateway exposes `POST /v1/images/edits` with each provider's native wire format:

- OpenAI models use `multipart/form-data`, including the official OpenAI SDK `images.edit()` request.
- xAI models use `application/json` with `image` or `images` URL, data URI, or file references.

xAI explicitly does not support the OpenAI SDK `images.edit()` multipart format. The Gateway preserves this boundary and does not convert multipart requests into xAI JSON. OpenAI multipart bodies are spooled to bounded mode-`0600` temporary files so file parts are not loaded entirely into memory; files are deleted before the request finishes.

## Create a service API key

With `GATEWAY_DATABASE_URL` set, create a development key:

```bash
go run ./cmd/gateway-key -name local-development
```

Every service key belongs to a project. Self-hosted migrations create `org_legacy` and `project_legacy`, and the CLI uses that development project by default. Select another active project with `-project-id project_example`. Disabling a key, its project, or its organization immediately prevents authentication.

The plaintext `ngw_sk_...` value is printed exactly once. Store it securely: PostgreSQL contains only its SHA-256 digest and a non-secret display prefix, so the plaintext cannot be recovered. An optional expiration can be supplied with `-expires-at 2026-09-01T00:00:00Z`.

Protected native routes will accept exactly one of `Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, or the Gemini-compatible `key` query parameter. Supplying credentials in multiple locations is rejected. Health endpoints remain unauthenticated.

## Wallet and ledger foundation

Phase 1 includes an internal organization wallet with append-only accounting entries. Amounts use integer `USD_TICKS`, where one USD equals 10,000,000,000 ticks. Deposit, reserve, capture, release, and refund commands are transactional and idempotent by organization-scoped operation key.

This is currently an internal domain boundary only. No public wallet endpoint exists, inference requests are not charged yet, and self-hosted operators must not treat the Deposit command as proof of payment. A managed Cloud service must validate payment events before calling Deposit.

## Provider pricing foundation

Provider channels can publish append-only, time-versioned image prices through the internal pricing domain. Cost and sale amounts use `USD_TICKS`; estimates match channel, protocol, operation, model, size, and quality exactly, enforce a configured minimum margin, and retain the selected price ID for later audit.

There is no public price-management endpoint, and clients cannot supply trusted prices or historical evaluation times. A managed Cloud service must authenticate and audit price publications through the internal domain rather than modifying pricing tables directly.

When `GATEWAY_BILLING_MODE=required`, OpenAI/xAI generation and editing plus registered Gemini image generation resolve the exact active price, reserve project-owned organization credits, and call the Provider only after the reservation commits. Provider 2xx responses are returned only after Capture; native non-2xx responses and errors known to occur before an upstream call release the reservation first. An uncertain settlement fails closed with the protocol-native unavailable response. Managed Cloud deployments must use `required`; the default `disabled` mode exists for self-hosted BYOK compatibility.

Billable image requests may include a visible-ASCII `Idempotency-Key` of up to 200 bytes. Repeating the same exact request under the same organization returns the stored native response with `Idempotency-Replayed: true` without calling the Provider or changing the Wallet. Reusing a key with different wire bytes returns a protocol-native conflict. Response snapshots retain only `Content-Type`, `Retry-After`, and the safe Google request ID where present; credentials, cookies, prompts, and request bodies are not stored.

In billing-required mode, response loss and settlement uncertainty create durable reconciliation tasks. Known Provider success is captured and known failure is released by a leased PostgreSQL worker. Timeout, connection loss, and panic remain reserved because the Provider outcome is unknown; after bounded retries they move to `MANUAL_REVIEW` and are never automatically refunded. Operators can inspect the backlog without changing readiness:

```sql
SELECT state, outcome, reason, count(*)
FROM image_charge_reconciliations
WHERE state <> 'RESOLVED'
GROUP BY state, outcome, reason;
```

Reconciliation rows and Ledger entries are append-only. Operators must not resolve them with direct SQL; manual resolution belongs to the Cloud control-plane follow-up.

## Replicate native Predictions

Set an exact allowlist of community model versions and a stable public Gateway origin to publish Replicate-compatible create, get, and cancel routes:

```bash
export GATEWAY_REPLICATE_API_TOKEN='your-replicate-provider-token'
export GATEWAY_REPLICATE_MODELS='owner/model:version'
export GATEWAY_PUBLIC_BASE_URL='https://gateway.example.com'
```

The Gateway accepts `POST /v1/predictions`, `GET /v1/predictions/{id}`, and `POST /v1/predictions/{id}/cancel`. It replaces the upstream Prediction ID and control URLs with a durable Gateway Job ID, and GET reads the stored native snapshot rather than polling Replicate directly. A background worker owns upstream polling and exact-once terminal billing settlement.

Signed Provider callbacks can reduce terminal observation latency while retaining polling as the recovery path:

```bash
export GATEWAY_REPLICATE_WEBHOOK_MODE='required'
export GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS='whsec_BASE64_SECRET'
export GATEWAY_REPLICATE_WEBHOOK_CALLBACK_SECRET='BASE64_32_BYTE_SECRET'
```

Webhook mode requires an HTTPS `GATEWAY_PUBLIC_BASE_URL`. The Gateway injects its own completed-event callback, verifies Replicate's raw-body HMAC and timestamp, then checks a per-Job capability and exact upstream Prediction identity before applying the terminal observation. Delivery replay, polling, cancellation and settlement converge through the same PostgreSQL CAS and append-only Ledger path. Client-supplied webhook fields remain rejected, and callback URLs, tokens, signatures and bodies must never appear in logs or metrics. During rollback, set webhook mode to `disabled`; polling continues to reconcile existing Predictions.

The official JavaScript client can use its `baseUrl` option, and the Replicate Python v2 client can use `base_url` (or `REPLICATE_BASE_URL`). In both cases, pass the service API key as the client credential. See [`examples/replicate`](./examples/replicate). Legacy Python v1 clients do not expose the same custom base-URL contract; use v2 or direct HTTP.

## fal native Queue

Enable fal with an exact model allowlist and stable public origin:

```bash
export GATEWAY_FAL_API_KEY='your-fal-provider-key'
export GATEWAY_FAL_MODELS='fal-ai/flux/dev'
export GATEWAY_PUBLIC_BASE_URL='https://gateway.example.com'
```

The Gateway preserves fal's model-scoped Queue routes: submit at `POST /{model}`, status at `GET /{model}/requests/{id}/status`, result at `GET /{model}/requests/{id}`, and cancellation at `PUT /{model}/requests/{id}/cancel`. Public request IDs and control URLs belong to the Gateway. Client status/result reads use durable snapshots; only the background worker polls fal, and it retrieves a completed result before scheduling Capture.

Managed billing publishes each initial fal model price with `size=default` and `quality=default`; runtime or compute-unit pricing is deferred. Replicate asynchronous prices use the same default dimensions.

Async image models declare an output-count usage capability in the model registry. Replicate reads `input.num_outputs`; fal reads `num_images`. Both default to one image and reject quantities above the registered maximum of 10 before reserving funds or dispatching upstream. Billing reserves the maximum requested quantity using the immutable price selected at submit time.

On a successful terminal result, Provider adapters count only supported HTTPS or inline image outputs and persist a content-free usage record tied to the terminal observation hash. If fewer images are returned, the Gateway captures only the verified actual quantity and releases the remainder of the reservation atomically. Missing, zero, excessive, extractor-mismatched, or failure/cancel results containing usable output are held in `MANUAL_REVIEW`; the Gateway does not guess a quantity or automatically capture/release those reservations. Legacy async Jobs created before usage evidence continue to settle with quantity one. To roll back usage-aware admission, disable the affected model/channel and drain already-reserved Jobs with a binary that understands migration `000027`; immutable usage and Ledger evidence must remain in place for audit.

The official JavaScript client is supported through its documented `proxyUrl` middleware at `/fal/proxy`; the Gateway validates `x-fal-target-url` as an exact `https://queue.fal.run` target and uses it only to recover the native path, never as a fetch destination. The official Python client reads `FAL_QUEUE_RUN_HOST` at import time and can point directly at the Gateway HTTPS host. Both clients continue to send `Authorization: Key SERVICE_KEY`. See [`examples/fal`](./examples/fal). Upload, streaming, and synchronous `fal.run` remain outside this release.

Signed callbacks can be enabled while retaining polling as the recovery path:

```bash
export GATEWAY_FAL_WEBHOOK_MODE='required'
export GATEWAY_FAL_WEBHOOK_CALLBACK_SECRET='BASE64_32_BYTE_SECRET'
```

Required mode needs an HTTPS public base URL and outbound access to fal's fixed JWKS endpoint. The Gateway injects only its own `fal_webhook` URL, verifies the four official signature headers against the raw body with ED25519, then checks the per-Job capability and upstream request identity before committing a terminal observation. Callback retries, polling and cancellation use the same PostgreSQL CAS and Ledger settlement path. Callback tokens, signatures, fal user/request IDs and payloads are excluded from telemetry. Cache refresh or signature failures should be alerted; to roll back, set the mode to `disabled`, after which durable polling continues to reconcile existing jobs. Rotate the callback deployment secret only after its maximum binding TTL has elapsed, or keep the previous deployment available until outstanding jobs terminate.

## Verify

Run all required checks:

```bash
make check
```

Individual commands:

```bash
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
```

Run PostgreSQL integration and process tests with:

```bash
export TEST_DATABASE_URL='postgres://gateway:gateway-local@127.0.0.1:55433/gateway?sslmode=disable'
export TEST_REDIS_URL='redis://127.0.0.1:56379/0'
make integration-test
```

`SIGINT` and `SIGTERM` stop new connections and wait up to `GATEWAY_SHUTDOWN_TIMEOUT` for active requests.

## License

Apache-2.0. See [`LICENSE`](./LICENSE).
