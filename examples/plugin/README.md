# Provider Plugin v1 example

External Adapter authors should begin with the isolated
[`go-sidecar-template`](./go-sidecar-template). It imports the public
`plugin-sdk/runtime/v1` contract and does not import Gateway internals.

Validate the trusted manifest directory:

```sh
go run ./cmd/gateway-plugin-validator -manifest-dir "$PWD/examples/plugin/manifests"
```

Generate deterministic, credential-free capability documentation:

```sh
go run ./cmd/gateway-plugin-validator \
  -manifest-dir "$PWD/examples/plugin/manifests" \
  -markdown
```

Run the isolated mock sidecar with the Compose `plugin` profile:

```sh
GATEWAY_PLUGIN_MOCK_TOKEN=local-plugin-token docker compose --profile plugin up plugin-mock
```

Configure Gateway with references. The manifest contains neither an origin nor
a secret; the secret-reference map names an environment variable that Gateway
resolves at startup.

```sh
export EXAMPLE_PLUGIN_TOKEN=local-plugin-token
export GATEWAY_PLUGIN_MODE=required
export GATEWAY_PLUGIN_MANIFEST_DIR="$PWD/examples/plugin/manifests"
export GATEWAY_PLUGIN_ENDPOINTS_JSON='{"example-sidecar":"http://127.0.0.1:58081"}'
export GATEWAY_PLUGIN_AUTH_SECRET_ENV_JSON='{"example-sidecar-token":"EXAMPLE_PLUGIN_TOKEN"}'
```

The public API remains native: use `example-image-v1` through either the
official OpenAI Images or Gemini `generateContent` SDK path. Gateway alone owns
service-key authentication, routing, billing, result storage, and protocol
projection. Sidecars receive only the bounded canonical operation envelope and
their dedicated bearer credential.

Run the black-box contract checks against an already running sidecar. The
secret is resolved through an environment reference and is never accepted as a
literal CLI argument or included in the report.

```sh
export EXAMPLE_PLUGIN_TOKEN=local-plugin-token
go run ./cmd/gateway-plugin-conformance \
  -manifest-dir "$PWD/examples/plugin/manifests" \
  -plugin-id provider.example \
  -endpoints-json '{"example-sidecar":"http://127.0.0.1:58081"}' \
  -auth-secret-env-json '{"example-sidecar-token":"EXAMPLE_PLUGIN_TOKEN"}' \
  -json
```

Exit code `0` means every check passed, `1` is a contract failure, and `2` is
invalid local configuration. Runtime v1 is backward compatible within v1:
fields are not removed or reinterpreted, and incompatible changes use a new
schema version.

For official signed admission, offline verification, compatibility matrix,
release yank, key rotation, and rollback policy, read
[`REGISTRY.md`](./REGISTRY.md). Registry verification never downloads or runs
an artifact.
