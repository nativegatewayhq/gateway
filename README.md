# Native AI Gateway

An open-source multimodal AI API gateway that preserves official provider SDKs and native API protocols while unifying authentication, routing, billing, failover, and managed media delivery.

## Status

Phase 0 native protocol validation and early Phase 1 billing foundations. The process exposes health endpoints, tenant-owned PostgreSQL service API key authentication, a capability-backed models endpoint, non-streaming Gemini `generateContent`, and OpenAI-compatible image generation and editing. OpenAI/xAI image requests can use required price-and-Wallet settlement; production payment deposits, dynamic routing, and Gemini billing remain unavailable.

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
| `GATEWAY_OPENAI_API_KEY` | unset | Optional OpenAI upstream credential |
| `GATEWAY_XAI_API_KEY` | unset | Optional xAI upstream credential |
| `GATEWAY_GOOGLE_REQUEST_TIMEOUT` | `2m` | Google request timeout; maximum `10m` |
| `GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES` | `33554432` | Positive Gemini body limit up to 32 MiB |
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
| `GATEWAY_TRUSTED_PROXY_CIDRS` | unset | Comma-separated reverse-proxy CIDRs allowed to supply `Forwarded` or `X-Forwarded-For` |

Invalid configuration fails before binding a listener. Logs are structured JSON and intentionally omit headers, cookies, query strings, and request/response bodies.

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

`--allow-model` is optional and repeatable. Omitting it preserves backward-compatible access to all registered models; production Keys should use an explicit least-privilege allowlist. Supplying it changes the Key to an exact logical allowlist using `protocol:operation:model`; supported image operations are `openai:image.generate`, `openai:image.edit`, and `gemini:image.generate`. Wildcards and Provider-native model names are not expanded. A denied request consumes its rate-limit token but returns native `403` before routing, idempotency replay, pricing, Wallet reservation or Provider dispatch. `/v1/models` returns only the intersection of the Key permissions, model capabilities and configured Provider credentials. Removing a permission also prevents that Key from replaying a previously stored response for the removed model.

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

## Provider credentials

Provider credentials are optional until their adapters are enabled. Inject them through environment variables backed by your deployment platform's secret manager; never commit them to source files or Compose configuration.

```bash
export GATEWAY_GOOGLE_API_KEY='...'
export GATEWAY_OPENAI_API_KEY='...'
export GATEWAY_XAI_API_KEY='...'
```

Provider credentials are held in an opaque, provider-scoped registry. They are not placed in the general process configuration and are never returned through an API. Outbound request preparation clones the request, removes inbound `Authorization`, API-key headers, cookies, and sensitive query parameters, then applies only the credential scoped to the selected provider. Missing credentials and provider-scope mismatches fail before any network request.

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

The Gateway preserves the JSON body and native success/error response bytes, including URL, `b64_json`, usage, and provider extension fields. Provider credentials are applied only to their fixed origins. Redirects, post-dispatch retries, and storage remain excluded; billing is opt-in through the required mode described below.

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
