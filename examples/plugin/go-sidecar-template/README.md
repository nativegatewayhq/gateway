# Native Gateway Go sidecar template

This directory is an isolated Go module. It imports only the public
`plugin-sdk/runtime/v1` wire package from Gateway. After copying the template,
remove the local `replace` in `go.mod` and select a compatible tagged SDK.

The server validates a dedicated bearer token, exposes the fixed v1 health and
execute paths, uses bounded HTTP timeouts, shuts down gracefully, and never
logs prompts, images, credentials, or raw envelopes.

```sh
GOWORK=off go test ./...
PLUGIN_AUTH_TOKEN=0123456789abcdef-template go run .
```

From the Gateway checkout, validate the running sidecar without making a paid
upstream call:

```sh
export PLUGIN_AUTH_TOKEN=0123456789abcdef-template
go run ./cmd/gateway-plugin-conformance \
  -manifest-dir "$PWD/examples/plugin/manifests" \
  -plugin-id provider.example \
  -endpoints-json '{"example-sidecar":"http://127.0.0.1:8081"}' \
  -auth-secret-env-json '{"example-sidecar-token":"PLUGIN_AUTH_TOKEN"}'
```
