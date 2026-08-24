# Provider Plugin v1 example

Validate the trusted manifest directory:

```sh
go run ./cmd/gateway-plugin-validator -manifest-dir "$PWD/examples/plugin/manifests"
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
