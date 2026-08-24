# Phase 6 plugin foundation handoff

Initiative: `phase-6-provider-plugin-foundation`

The Gateway repository is authoritative for these contracts:

- `plugin-sdk/manifest/v1`: strict Provider Manifest v1 parser, canonical digest, and safe directory loader
- `internal/plugins/client.go`: `/plugin/v1/health` and `/plugin/v1/execute` JSON envelopes and error categories
- `cmd/gateway-plugin-mock`: reference sidecar behavior
- `cmd/gateway-plugin-validator`: CI manifest validation entry point
- `examples/plugin/manifests`: valid conformance corpus seed

Repository-local follow-up plans should consume, not duplicate, this contract:

- `conformance`: copy versioned valid/invalid corpus fixtures and run official SDK plus sidecar failure cases against a built Gateway artifact.
- `cloud`: mount the manifest directory read-only, map endpoint refs through ConfigMap, map secret refs through Secret-backed environment variables, and deny sidecar access to PostgreSQL, Redis, and metadata services.
- `dashboard`: expose only plugin ID, version, manifest digest, model capabilities, channel status, and bounded health state. Never expose endpoint origins or secret refs/values.

Compatibility changes require a new schema/envelope version or a backward-compatible Gateway change. Existing manifest digest and `plugin_channel_snapshots` rows are immutable audit evidence and must not be rewritten during rollout or rollback.
