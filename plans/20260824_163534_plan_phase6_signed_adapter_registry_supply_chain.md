---
id: gateway-20260824-064
title: Phase 6 Signed Official Adapter Registry and Supply-chain Verification
status: accepted
created_at: 2026-08-24T16:35:34+09:00
updated_at: 2026-08-24T16:35:34+09:00
owners:
  - gateway
initiative: phase-6-signed-adapter-registry
depends_on:
  - gateway-20260824-063
supersedes: []
affected_repos:
  - gateway
  - registry
  - conformance
  - cloud
---

# Phase 6 Signed Official Adapter Registry and Supply-chain Verification

## 목적

공식 Provider Adapter의 identity, version, Gateway/runtime 호환 범위, Provider Manifest, conformance 결과와 배포 artifact를 하나의 서명된 admission statement로 결속하고, Gateway와 CI가 operator가 고정한 trust root로 이를 offline 검증하도록 한다. Registry metadata를 신뢰하지 않은 manifest·artifact와 섞어 쓰거나 이전 index로 rollback하는 것을 fail closed한다.

## 배경

Plan 062는 operator가 신뢰한 로컬 manifest와 HTTP sidecar runtime을 도입했고, Plan 063은 public runtime SDK, fixture corpus, black-box conformance report와 standalone template를 제공했다. 그러나 현재는 누가 Adapter를 공식 승인했는지, conformance report가 어떤 manifest와 OCI artifact를 대상으로 생성됐는지, 어느 version이 어떤 Gateway와 호환되는지 검증하는 배포 계약이 없다.

최초 Phase 6 로드맵의 남은 핵심 완료 조건은 공식 Adapter Registry와 Adapter/Gateway 호환 범위 표시다. Registry가 URL 목록이나 mutable tag만 제공하면 compromise, tag 이동, report 바꿔치기와 rollback에 취약하므로 다음 단계는 다운로드나 marketplace UI보다 content-addressed descriptor와 operator-pinned signature verification을 먼저 고정한다.

표준 기반은 다음과 같다.

- [OCI Content Descriptor](https://github.com/opencontainers/image-spec/blob/main/descriptor.md)는 artifact의 media type, digest와 byte size를 결속한다.
- [in-toto Attestation Statement v1](https://github.com/in-toto/attestation/blob/main/spec/v1/statement.md) subject는 admission predicate가 가리키는 immutable artifact를 결속한다.
- [in-toto Envelope](https://github.com/in-toto/attestation/blob/main/spec/v1/envelope.md)는 DSSE v1 envelope와 payload type을 권장하고 signature array와 key ID를 정의한다.
- [SLSA Provenance v1](https://slsa.dev/spec/v1.0/provenance)는 artifact와 build provenance를 연결하는 장래 Registry publication pipeline의 입력이다.

## 범위

- `plugin-sdk/registry/v1` strict Registry Index, Admission Statement, Trust Policy와 DSSE envelope codec
- OCI SHA-256 descriptor의 media type, digest, byte size, platform과 immutable reference 검증
- plugin ID/version, manifest digest, runtime SDK/schema, Gateway compatibility와 artifact descriptor 결속
- strict conformance report digest, check set/outcome과 admission statement 결속
- source repository/commit, builder identity, build invocation digest, SBOM/provenance descriptor 결속
- operator-pinned Ed25519 trust policy, key ID, threshold signature와 validity window
- index sequence, created/expires, previous-index digest와 operator minimum sequence를 통한 rollback 방지
- yanked version을 삭제하지 않는 append-only release status와 bounded reason
- deterministic version/capability/compatibility matrix와 Markdown/JSON 출력
- local file만 입력으로 받는 `gateway-plugin-registry` verify CLI
- optional/required Gateway startup admission: 로컬 manifest가 signed active Registry entry와 정확히 일치해야 함
- selected plugin request/channel snapshot에 registry index/admission digest와 sequence 불변 증거 추가
- fixtures, contributor policy, Makefile, CI와 멀티레포 handoff 갱신

## 제외 범위

- Gateway 또는 CLI가 remote Registry, OCI registry, GitHub release나 arbitrary URL에서 파일을 다운로드하는 기능
- container image pull, unpack, process 실행, Kubernetes 배포와 sidecar lifecycle 관리
- private signing key 생성·저장·복호화 또는 CI signing service 구현
- public transparency log, Rekor/Fulcio/OIDC keyless signing과 online inclusion proof
- Registry 웹 UI, 검색, ranking, submission workflow와 maintainer review 운영
- mutable OCI tag, semver range만으로 artifact를 선택하거나 unsigned metadata를 fallback으로 쓰는 방식
- Adapter가 Gateway DB, Wallet, Ledger, Provider credential 또는 public request identity에 접근하는 방식
- async Job/webhook, streaming LLM/audio, gRPC/mTLS와 runtime hot reload
- registry, conformance, cloud 저장소의 내부 배포 구현

## 핵심 결정

### 1. Registry admission은 다운로드 계약이 아니라 승인 증거다

- Gateway는 이미 operator가 배치한 manifest와 sidecar만 사용한다.
- Registry entry는 origin, endpoint, secret ref/value, command, environment, prompt, raw response와 image를 포함하지 않는다.
- OCI artifact는 repository tag가 아니라 exact `sha256:<64 lowercase hex>` digest, media type과 size로만 식별한다.
- Gateway는 artifact를 pull하거나 실행하지 않고 signed admission과 local manifest/channel evidence의 일치만 확인한다.

### 2. 서명 대상은 canonical Registry Index가 아니라 typed admission payload다

- envelope는 DSSE PAE 규칙으로 payload type과 exact payload bytes를 함께 서명한다.
- payload는 strict canonical JSON인 `nativegateway.adapter-admission/v1` in-toto Statement subset이다.
- admission subject는 OCI artifact descriptor이며 predicate가 plugin/manifest/conformance/build identity를 결속한다.
- parser는 duplicate/unknown/trailing/oversized JSON, duplicate signature/key ID와 unsupported algorithm을 거부한다.
- 서명 검증 뒤 decoded typed payload를 다시 canonical encode한 bytes가 payload와 일치해야 한다.

### 3. Trust policy는 Registry payload 밖에서 operator가 고정한다

- payload가 제공한 key는 절대 신뢰하지 않는다.
- absolute regular mode-safe local trust policy file에 허용 Ed25519 public key, key ID, validity와 threshold를 둔다.
- key ID는 public key bytes의 SHA-256으로 유도하고 임의 alias를 허용하지 않는다.
- threshold는 서로 다른 trusted key의 유효 signature 개수로 계산한다.
- key rotation은 overlap 기간에 old/new key를 trust policy에 함께 배치하고 threshold를 만족한 뒤 old key를 제거한다.

### 4. Rollback과 삭제는 명시적으로 다룬다

- index는 단조 증가 `sequence`, `created_at`, bounded `expires_at`과 직전 canonical index digest를 가진다.
- operator는 `minimum_sequence`를 trust policy/config에 고정하며 이보다 낮은 index를 거부한다.
- sequence가 1보다 큰 index는 previous digest가 필수이고 self-reference나 같은 sequence의 다른 payload를 허용하지 않는다.
- release는 `active` 또는 `yanked`이며 yanked row도 index에서 삭제하지 않는다.
- Gateway required mode는 expired/yanked/missing/mismatch entry를 startup에서 fail closed한다.

### 5. Conformance report 자체가 아니라 공식 재검증 admission을 신뢰한다

- admission에는 strict report schema/sdk version, report SHA-256, outcome `pass`와 required check-set digest가 포함된다.
- publisher가 외부 제출 report를 그대로 서명하지 않고 isolated build artifact에 conformance를 재실행하는 것은 Registry 저장소의 책임이다.
- Gateway verifier는 local report가 제공된 경우 digest와 strict decode를 재검사하지만 report만으로 official status를 부여하지 않는다.
- conformance report와 Registry JSON에는 endpoint, secret, prompt, raw body, media result가 계속 금지된다.

### 6. Build evidence는 immutable descriptor로 결속한다

- admission은 source repository의 exact HTTPS origin과 full commit SHA, builder identity, build invocation/config SHA-256을 요구한다.
- OCI subject와 SBOM, SLSA provenance는 media type/digest/size descriptor로 표현한다.
- provenance의 내부 policy evaluation과 builder trust는 publication pipeline이 소유하고 Gateway는 admission에 결속된 descriptor identity를 검증한다.
- architecture별 artifact는 별도 subject/admission을 사용하며 `linux/amd64`, `linux/arm64`만 v1에서 허용한다.

## 공개 데이터 모델

### Registry Index v1

```json
{
  "schema_version": "nativegateway.adapter-registry/v1",
  "sequence": 7,
  "created_at": "2026-08-24T07:00:00Z",
  "expires_at": "2026-09-23T07:00:00Z",
  "previous_index_digest": "sha256:...",
  "releases": [
    {
      "plugin_id": "provider.example",
      "plugin_version": "1.0.0",
      "status": "active",
      "admissions": [
        {
          "platform": "linux/arm64",
          "envelope_digest": "sha256:..."
        }
      ]
    }
  ]
}
```

Index와 release/admission은 plugin ID, semantic version, platform 순으로 canonical 정렬한다. 하나의 plugin/version/platform에 admission은 정확히 하나이며 duplicate와 ambiguous latest selection을 거부한다.

### Admission predicate v1

```json
{
  "plugin_id": "provider.example",
  "plugin_version": "1.0.0",
  "manifest_digest": "sha256:...",
  "runtime_schema": "nativegateway.plugin-request/v1",
  "runtime_sdk": "runtime/v1",
  "gateway_compatibility": ">=0.1.0 <1.0.0",
  "platform": "linux/arm64",
  "conformance": {
    "report_digest": "sha256:...",
    "schema_version": "nativegateway.plugin-conformance/v1",
    "required_checks_digest": "sha256:...",
    "outcome": "pass"
  },
  "source": {
    "repository": "https://github.com/example/provider-adapter",
    "commit": "..."
  },
  "builder": {
    "id": "https://github.com/nativegatewayhq/registry/builders/release-v1",
    "invocation_digest": "sha256:..."
  },
  "sbom": {"media_type": "application/spdx+json", "digest": "sha256:...", "size": 1234},
  "provenance": {"media_type": "application/vnd.in-toto+json", "digest": "sha256:...", "size": 2345}
}
```

실제 DSSE payload는 in-toto Statement v1의 `_type`, `subject`, `predicateType`과 위 predicate를 포함한다. OCI subject descriptor의 digest/size/platform과 predicate artifact identity가 다르면 거부한다.

## 설계 및 구현 순서

### 1. Strict Registry SDK

- `plugin-sdk/registry/v1`에 bounded index, release, descriptor, trust policy, DSSE envelope와 admission types를 추가한다.
- existing `jsonstrict`를 사용해 duplicate/unknown/trailing JSON을 재귀적으로 거부한다.
- RFC3339 UTC second precision, size bound, semantic version, ID, URL origin과 SHA-256을 strict 검증한다.
- canonical encode/digest와 deterministic ordering golden을 제공한다.

### 2. DSSE와 threshold verifier

- Go standard library Ed25519과 DSSE PAE를 사용해 exact payload type/bytes를 검증한다.
- untrusted embedded key, duplicate key ID/signature, invalid key length, insufficient threshold와 expired/not-yet-valid key를 거부한다.
- test signing helper는 `_test.go`/fixture generator에만 두고 production CLI는 private key를 읽지 않는다.
- signature/detail error는 stable category로만 외부에 노출한다.

### 3. Admission과 content binding

- index entry→envelope digest→admission identity→OCI subject→manifest/report/SBOM/provenance descriptor를 순서대로 결속한다.
- local Provider Manifest canonical digest와 admission manifest digest를 비교한다.
- strict conformance report가 제공되면 report digest, plugin/version/manifest/sdk/outcome/check set을 비교한다.
- active/yanked, platform, Gateway compatibility와 runtime schema를 fail closed한다.

### 4. Offline CLI와 compatibility matrix

- `cmd/gateway-plugin-registry`는 absolute mode-safe trust/index/envelope/manifest/report directories만 읽는다.
- `verify`는 human summary 또는 secret-safe strict JSON 결과와 stable exit code를 제공한다.
- `matrix`는 plugin/version/platform/status/Gateway/runtime/capability와 digest만 deterministic Markdown/JSON으로 출력한다.
- CLI는 remote URL fetch, private key, endpoint/secret mapping과 artifact execution option을 제공하지 않는다.

### 5. Gateway startup admission

- `disabled`는 기존 trusted manifest 동작을 유지하고 `required`는 signed Registry admission 없는 plugin을 로드하지 않는다.
- safe local file config와 operator minimum sequence를 추가한다.
- registry 검증은 route/channel publication 전에 완료하고 partially verified snapshot을 노출하지 않는다.
- registry index/admission digest, sequence와 status를 immutable plugin binding/channel snapshot에 추가한다.
- endpoint와 auth secret ref 해석은 기존 local operator config가 계속 소유한다.

### 6. 운영·문서·멀티레포 handoff

- official release/yank/key rotation/expiry/rollback 절차와 compatibility policy를 문서화한다.
- Registry 저장소에는 signing/publication pipeline, Conformance 저장소에는 isolated rerun, Cloud 저장소에는 verified bundle delivery 계획을 독립적으로 요구한다.
- Dashboard는 public identity/status/digest/compatibility만 표시하고 trust path, local file path, endpoint와 secret은 받지 않는다.
- Makefile과 CI가 fixtures, public external module, race/integration/SDK regression을 실행한다.

## 보안 및 과금 고려사항

- Registry verification은 Adapter가 안전하다는 sandbox 보장이 아니라 승인된 bytes/identity라는 공급망 증거다. runtime network/process isolation은 operator가 별도로 적용한다.
- signed metadata에도 credential, endpoint, tenant/user identity, price/currency와 Ledger instruction을 허용하지 않는다.
- Gateway-managed price와 channel은 계속 별도 immutable operator publication이며 registry admission이 가격을 생성하지 않는다.
- registry mismatch는 dispatch 전에 발생하므로 Reserve를 만들지 않는다. 이미 dispatch된 charge는 index expiry/yank와 무관하게 기존 reconciliation로 수렴한다.
- verification error는 key bytes, signature, local path, raw payload와 report body를 로그/JSON에 포함하지 않는다.
- index/envelope/report/body는 각각 독립 byte/count/depth limit를 가지며 서명 검증 전에 size와 JSON ambiguity를 검사한다.

## 테스트 계획

### 단위 테스트

- index/admission/trust/envelope strict codec, canonical digest와 golden
- duplicate/unknown/trailing/oversized/deep JSON과 unordered release 정규화
- valid threshold, insufficient/duplicate/untrusted/expired/future signature
- payload type substitution, PAE mismatch와 canonical payload mismatch
- OCI digest/media type/size/platform과 manifest/report identity mismatch
- active/yanked, compatibility, sequence/previous digest/minimum rollback policy
- matrix deterministic output와 secret/path/raw-content 부재
- CLI ref/path/options, stable result category와 exit code

### 통합 테스트

- signed active admission+local manifest가 exact plugin route/channel snapshot으로 로드됨
- unsigned, expired, yanked, wrong platform, old sequence와 digest mismatch가 route publication 전에 실패함
- registry disabled mode가 Plan 063 local trusted manifest behavior를 보존함
- immutable request/charge snapshot이 registry sequence/index/admission digest를 보존함
- duplicate idempotency와 managed billing이 단일 sidecar dispatch/Capture로 수렴함
- Gateway restart가 같은 bundle을 동일 snapshot으로 복구하고 다른 same-sequence index를 거부함

### 호환성 테스트

- Plan 063 public runtime/template/conformance corpus와 report strict decode 유지
- official OpenAI/Gemini SDK가 admitted plugin model을 Base URL/Key 변경만으로 호출함
- external consumer가 registry v1 SDK와 matrix JSON을 strict decode함
- existing Plan 062 manifest digest와 channel snapshot row를 rewrite하지 않음

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai ./protocols/gemini -count=1
GOWORK=off go test ./plugin-sdk/...
go run ./cmd/gateway-plugin-registry verify ...
go run ./cmd/gateway-plugin-registry matrix ...
```

## 완료 조건

- [ ] strict Registry Index/Admission/Trust/DSSE public SDK와 canonical golden이 제공됨
- [ ] operator-pinned threshold signature, expiry와 key rotation 경계가 fail closed함
- [ ] OCI artifact, manifest, conformance, source/build/SBOM/provenance identity가 admission에 결속됨
- [ ] sequence/previous digest/minimum floor가 stale rollback과 same-sequence equivocation을 거부함
- [ ] offline CLI가 verify와 deterministic compatibility matrix를 secret-safe하게 제공함
- [ ] Gateway required mode가 active exact admission만 route/channel로 publish함
- [ ] request/charge snapshot이 registry index/admission identity를 불변 보존함
- [ ] managed price/Wallet/Ledger와 endpoint/secret 경계가 Registry와 분리됨
- [ ] unit/race/integration/SDK/public-module 검사가 통과함
- [ ] release/yank/key rotation/rollback 문서와 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- `GATEWAY_PLUGIN_REGISTRY_MODE=disabled`로 signed admission gate만 비활성화하고 기존 operator-trusted manifest mode로 복귀한다.
- required mode rollout 실패 시 이전의 아직 유효하고 minimum sequence 이상인 signed index bundle로 되돌린다. sequence floor를 낮추는 자동 rollback은 금지한다.
- key rotation 오류는 overlap trust policy로 복구하며 payload에서 발견한 key를 임시 신뢰하지 않는다.
- additive registry evidence column과 기존 channel snapshot은 감사 증거로 유지하고 rewrite/delete하지 않는다.
- yanked release는 index에서 삭제하지 않고 새 higher-sequence index로 status를 되돌린다.

## 후속 작업

- Registry 저장소 signing/publication pipeline, isolated rebuild와 transparency log
- Cloud의 verified OCI digest 배포, admission controller와 runtime sandbox profile
- Dashboard official/yanked/compatibility read projection
- async Job/webhook plugin SDK and conformance
- streaming LLM/audio plugin SDK and conformance
- gRPC/mTLS sidecar transport와 controlled runtime reload
