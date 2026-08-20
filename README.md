# Native AI Gateway

An open-source multimodal AI API gateway that preserves official provider SDKs and native API protocols while unifying authentication, routing, billing, failover, and managed media delivery.

## Status

Phase 0 authentication foundation. The process exposes Gateway-owned health endpoints and uses PostgreSQL for service API key authentication. Provider-native APIs, billing, and routing are intentionally not implemented yet.

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

Invalid configuration fails before binding a listener. Logs are structured JSON and intentionally omit headers, cookies, query strings, and request/response bodies.

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
