# Native AI Gateway

An open-source multimodal AI API gateway that preserves official provider SDKs and native API protocols while unifying authentication, routing, billing, failover, and managed media delivery.

## Status

Phase 0 native protocol validation. The process exposes health endpoints, PostgreSQL-backed service API key authentication, non-streaming Gemini `generateContent`, and OpenAI-compatible image generation for OpenAI and xAI. Billing and dynamic routing are intentionally not implemented yet.

## Development workflow

This repository follows Plan First development.

1. Read [`AGENTS.md`](./AGENTS.md).
2. Read [`CONTRIBUTING.md`](./CONTRIBUTING.md).
3. Review [`plans/`](./plans/).
4. Implement only an accepted plan.

The first accepted implementation plan is [Phase 0 Gateway Bootstrap](./plans/20260820_113825_plan_phase0_gateway_bootstrap.md).

## Requirements

- Go 1.25 or newer supported release

## Run locally

```bash
docker compose up -d postgres
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

Invalid configuration fails before binding a listener. Logs are structured JSON and intentionally omit headers, cookies, query strings, and request/response bodies.

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
    model="gemini-image-model",
    contents="Draw a cat astronaut",
    config=types.GenerateContentConfig(response_modalities=["IMAGE"]),
)
```

The Gateway authenticates the service key, removes it from the outbound request, and applies only `GATEWAY_GOOGLE_API_KEY` to the fixed Google origin. Google success and error JSON bodies are passed through without schema conversion. Redirects and automatic retries are disabled.

Streaming, model listing, file upload, billing, managed image storage, and cross-provider conversion are not included in this phase. Official Python and JavaScript SDK version compatibility will be maintained in the separate conformance repository.

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

The Gateway preserves the JSON body and native success/error response bytes, including URL, `b64_json`, usage, and provider extension fields. Provider credentials are applied only to their fixed origins. Redirects, retries, fallback, storage, and billing are excluded from this phase.

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

The plaintext `ngw_sk_...` value is printed exactly once. Store it securely: PostgreSQL contains only its SHA-256 digest and a non-secret display prefix, so the plaintext cannot be recovered. An optional expiration can be supplied with `-expires-at 2026-09-01T00:00:00Z`.

Protected native routes will accept exactly one of `Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, or the Gemini-compatible `key` query parameter. Supplying credentials in multiple locations is rejected. Health endpoints remain unauthenticated.

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
make integration-test
```

`SIGINT` and `SIGTERM` stop new connections and wait up to `GATEWAY_SHUTDOWN_TIMEOUT` for active requests.

## License

Apache-2.0. See [`LICENSE`](./LICENSE).
