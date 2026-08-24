# Signed Adapter Registry v1

The Registry contract proves that trusted maintainers admitted an exact
Provider Manifest and OCI artifact after conformance and build-evidence review.
It does not sandbox an Adapter, pull an image, choose an endpoint, distribute a
credential, or publish a managed price.

Gateway consumes three operator-delivered local admission inputs:

- a mode-safe trust policy containing Ed25519 public keys and a threshold;
- one DSSE-signed Registry index with sequence, expiry, status, and admission digests;
- digest-named DSSE admission envelopes for the selected platform.

The offline verification CLI additionally requires the digest-named canonical
conformance reports referenced by those admissions and revalidates their exact
identity, outcome, SDK, and required check set.

The signed in-toto Statement binds plugin/version, manifest digest, runtime
schema, Gateway compatibility, OCI descriptor, conformance check-set digest,
source commit, builder invocation, SBOM, and provenance descriptors. OCI tags
are not trusted; every descriptor uses exact SHA-256 and byte size.

## Offline verification

```sh
go run ./cmd/gateway-plugin-registry verify \
  -trust-file /absolute/registry/trust.json \
  -index-file /absolute/registry/index.dsse.json \
  -admission-dir /absolute/registry/admissions \
  -manifest-dir /absolute/provider-manifests \
  -report-dir /absolute/registry/reports \
  -platform linux/arm64 \
  -minimum-sequence 17
```

Generate a credential-free compatibility matrix from the same verified
snapshot:

```sh
go run ./cmd/gateway-plugin-registry matrix \
  -trust-file /absolute/registry/trust.json \
  -index-file /absolute/registry/index.dsse.json \
  -admission-dir /absolute/registry/admissions \
  -manifest-dir /absolute/provider-manifests \
  -report-dir /absolute/registry/reports \
  -platform linux/arm64 \
  -minimum-sequence 17 \
  -json
```

Exit code `0` is verified, `1` is signature/content/policy failure, and `2` is
invalid CLI configuration. Errors and matrix output omit local paths, public
key bytes, signatures, endpoint/secret references, prompts, raw envelopes, and
media results.

## Gateway required mode

Set `GATEWAY_PLUGIN_REGISTRY_MODE=required` plus the three local paths,
platform, and sequence floor. Gateway verifies the complete snapshot before
publishing any plugin route. The admitted envelope digest changes the channel
ID, so a new artifact admission cannot overwrite an old channel or silently
reuse its price. PostgreSQL stores append-only index and channel admission
evidence; restart rejects an older sequence, same-sequence different index, or
a broken direct previous-index link.

## Release and yank

The separate Registry publication pipeline must rebuild or inspect the exact
OCI digest, rerun the official conformance check set, evaluate provenance, and
threshold-sign both admission and the next index. Gateway contains no private
signing-key path.

Yank a release by publishing a higher-sequence signed index with status
`yanked` and a bounded public reason. Never delete the historical row or reuse
its version for different bytes. Required mode refuses a yanked release before
creating a billing reservation.

## Key rotation

Add new public keys to the operator trust policy while old keys remain valid,
publish an index satisfying the configured threshold with distinct trusted
keys, deploy it, then remove retired keys after the overlap window. A key found
inside an envelope is never trusted. Do not lower the threshold as an automatic
recovery action.

## Rollback and recovery

Roll forward with a new signed sequence. Do not lower the operator minimum or
delete the persisted highest index to accept stale metadata. A same-sequence
payload must have the exact previously accepted digest; the immediate next
sequence must name the persisted index digest as its predecessor. Disabling
Registry required mode returns to explicitly operator-trusted local manifests,
but does not delete audit snapshots or settle in-flight charges differently.
