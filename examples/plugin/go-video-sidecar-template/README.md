# Native Gateway video Go sidecar template

This isolated module implements only the public `plugin-sdk/video/v1` contract.
It provides deterministic in-memory submit, poll, cancel, and signed completion
callback behavior without a paid Provider call or any Gateway `internal/` import.

```sh
export PLUGIN_AUTH_TOKEN=0123456789abcdef-template
export PLUGIN_CALLBACK_SECRET=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
GOWORK=off go test ./...
go run .
```

Run the video profile from the Gateway checkout with the same purpose-specific
callback key exposed through a separate environment variable:

```sh
go run ./cmd/gateway-plugin-conformance -profile video-v1 \
  -manifest-dir "$PWD/examples/plugin/manifests" \
  -plugin-id provider.video-example \
  -endpoints-json '{"video-sidecar":"http://127.0.0.1:8082"}' \
  -auth-secret-env-json '{"video-sidecar-token":"PLUGIN_AUTH_TOKEN"}' \
  -callback-secret-env PLUGIN_CALLBACK_SECRET \
  -result-origin https://assets.example.com
```
