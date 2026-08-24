# Phase 6 plugin foundation handoff

Initiative: `phase-6-provider-plugin-foundation`

The Gateway repository is authoritative for these contracts:

- `plugin-sdk/manifest/v1`: strict Provider Manifest v1 parser, canonical digest, and safe directory loader
- `plugin-sdk/runtime/v1`: public strict `/plugin/v1/health` and `/plugin/v1/execute` wire contract
- `plugin-sdk/conformance/v1`: stable black-box check and secret-safe report contract
- `plugin-sdk/fixtures/v1`: versioned valid/invalid manifest and runtime corpus
- `internal/plugins/client.go`: Gateway-owned fixed-origin transport and result-origin enforcement
- `cmd/gateway-plugin-mock`: reference sidecar behavior
- `cmd/gateway-plugin-validator`: CI manifest validation entry point
- `cmd/gateway-plugin-conformance`: env/file-ref black-box sidecar validation entry point
- `examples/plugin/go-sidecar-template`: isolated external Go module reference implementation
- `plugin-sdk/registry/v1`: strict Registry/Trust/DSSE/admission and compatibility-matrix contract
- `cmd/gateway-plugin-registry`: offline local bundle verification; no remote fetch or execution
- `examples/plugin/manifests`: valid conformance corpus seed

Repository-local follow-up plans should consume, not duplicate, this contract:

- `conformance`: consume the v1 fixture index and strict JSON report, run official SDK plus sidecar failure cases against a built Gateway artifact, and never persist endpoint, secret, prompt, raw body, or image data.
- `cloud`: mount the manifest directory read-only, map endpoint refs through ConfigMap, map secret refs through Secret-backed environment variables, and deny sidecar access to PostgreSQL, Redis, and metadata services.
- `dashboard`: expose only plugin ID, version, manifest digest, model capabilities, channel status, and bounded health state. Never expose endpoint origins or secret refs/values.

Initiative `phase-6-signed-adapter-registry` adds these repository boundaries:

- `registry`: owns isolated rebuild/reverification, private-key service integration, threshold publication, release/yank history, provenance policy, and future transparency log.
- `conformance`: returns the strict v1 report to the isolated Registry pipeline; a submitted report is not official until Registry reruns and admits it.
- `cloud`: delivers verified local trust/index/admission files read-only and deploys only the exact OCI digest. Gateway must not receive registry signing keys.
- `dashboard`: consumes the secret-safe matrix projection only. It never receives signature bytes, trust paths, endpoint origins, or credentials.

Compatibility changes require a new schema/envelope version or a backward-compatible additive v1 change. Consumers must key artifacts by schema and SDK version rather than repository commit. Existing manifest digest and `plugin_channel_snapshots` rows are immutable audit evidence and must not be rewritten during rollout or rollback.
