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
- `plugin-sdk/async/v1`: strict submit/poll/cancel/result and signed callback contract
- `plugin-sdk/conformance/async/v1`: stable secret-safe async admission report and runner
- `examples/plugin/go-async-sidecar-template`: isolated deterministic async reference module
- `plugin-sdk/video/v1`: isolated Runway-native video submit/poll/cancel/result and signed callback contract
- `plugin-sdk/conformance/video/v1`: stable video admission report and black-box runner
- `examples/plugin/go-video-sidecar-template`: standalone deterministic video Adapter module

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

Initiative `phase-6-async-plugin-runtime` keeps native Replicate/fal public
routes and durable Job/Ledger state in Gateway while the sidecar sees only a
bounded operation request and opaque Provider Job ref. Conformance owns SDK
restart/cancel verification, Registry reruns `async-v1` before admission, and
Cloud injects the callback HMAC independently from the outbound bearer. A
submitted Job must drain through its persisted channel/ref during rollback;
it must never be redispatched or released merely because callback delivery was
missed.

Initiative `phase-6-async-video-plugin-runtime` applies the same durable Job,
callback replay, exact-once settlement, and signed Registry boundaries to the
separate `video/v1` profile. Gateway retains Runway native routes, source asset
authorization, provider-credit evidence, and managed video collection. Cloud,
Registry, and conformance consumers must select the profile by manifest contract
and must never reinterpret video envelopes as image async envelopes.
