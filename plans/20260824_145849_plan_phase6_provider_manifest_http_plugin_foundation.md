---
id: gateway-20260824-062
title: Phase 6 Provider Manifest and HTTP Sidecar Plugin Foundation
status: completed
created_at: 2026-08-24T14:58:49+09:00
updated_at: 2026-08-24T15:42:08+09:00
owners:
  - gateway
initiative: phase-6-provider-plugin-foundation
depends_on:
  - gateway-20260820-003
  - gateway-20260820-007
  - gateway-20260820-016
  - gateway-20260820-021
  - gateway-20260820-023
  - gateway-20260820-028
  - gateway-20260821-029
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 6 Provider Manifest and HTTP Sidecar Plugin Foundation

## 목적

외부 개발자가 Gateway 코어를 수정하거나 같은 프로세스에 코드를 로드하지 않고도 versioned Provider manifest와 격리된 HTTP sidecar로 새로운 Provider를 추가할 수 있는 최초 Plugin 계약을 구축한다. 첫 vertical slice는 synchronous image generation을 지원하며, manifest 검증·capability 등록·고정 origin dispatch·credential 격리·호환성 검사를 end-to-end로 증명한다.

## 배경

현재 공식 Provider adapter는 `providers/` 내부 Go 코드로 컴파일된다. 이 구조는 native protocol과 과금 경계를 강하게 보호하지만 외부 Provider 추가마다 Gateway release가 필요하다. 최초 로드맵의 Phase 6은 Provider Manifest, Adapter SDK, Mock Server, Conformance Test, Sidecar Plugin과 Registry를 요구한다.

Go `plugin` 또는 동적 shared library는 프로세스 crash·메모리·credential 경계를 공유하고 빌드/ABI 결합이 강하므로 사용하지 않는다. 최초 foundation은 operator가 신뢰한 manifest를 startup에서 검증하고, Gateway가 고정된 loopback/private HTTPS origin의 sidecar에 bounded canonical operation request를 전송하는 out-of-process 모델을 사용한다. 공개 native protocol, API Key 인증, Wallet/가격/라우팅/저장/관측성은 계속 Gateway가 소유한다.

## 범위

- canonical JSON Provider Manifest v1 schema와 strict startup validation
- stable plugin/provider/version identity와 Gateway compatibility range
- protocol, operation, modality, model capability와 media constraints 선언
- synchronous `image.generate` HTTP sidecar execution contract
- operator-configured exact sidecar origin, timeout, body limit와 concurrency cap
- Gateway→sidecar request authentication을 위한 secret reference 기반 bearer/HMAC 계약
- public service key와 managed Provider credential을 plugin payload에서 분리
- fixed `/plugin/v1/execute` 및 `/plugin/v1/health` endpoint
- canonical request/response/error envelope, request ID와 bounded usage/cost evidence
- routing candidate에 manifest-owned Provider/model/channel executor 연결
- pre-dispatch fallback, circuit breaker와 existing managed image billing/storage 연계
- manifest digest/version과 selected plugin identity의 immutable request evidence
- mock sidecar server, manifest validator CLI/library와 conformance fixtures
- startup/readiness, OpenTelemetry bounded dimensions와 secret-safe logging
- Docker Compose example plugin과 contributor documentation

## 제외 범위

- untrusted manifest의 원격 URL 다운로드 또는 runtime 자동 설치
- Go shared object, WASM in-process runtime와 arbitrary code execution
- dynamic hot reload, marketplace/registry 서명·배포·자동 업데이트
- plugin이 Gateway DB, Wallet, Ledger, API Key 또는 object storage에 직접 접근하는 방식
- public service API Key나 Gateway master key를 plugin에 전달하는 방식
- streaming LLM/SSE, asynchronous Job, webhook, video/audio와 multipart edit plugin
- plugin-defined protocol route 또는 arbitrary public HTTP path
- plugin-defined 가격을 검증 없이 managed billing에 사용하는 방식
- gRPC transport와 cross-host service mesh 운영
- Dashboard/Cloud/Conformance 저장소 내부 구현

## 핵심 결정

### 1. Manifest는 선언이고 executable code가 아니다

- manifest는 JSON이며 unknown/duplicate field, duplicate identity, unsupported schema와 capability contradiction을 거부한다.
- file path는 operator configuration에서만 제공하고 customer request가 manifest/path/origin을 선택하지 못한다.
- manifest는 secret, credential 원문, executable command, shell argument와 arbitrary header를 포함할 수 없다.
- canonical JSON SHA-256 digest를 계산해 startup snapshot과 request evidence에 사용한다.

### 2. Native protocol과 공통 operation 경계는 Gateway가 소유한다

- OpenAI/Gemini facade가 public request를 인증·검증한 뒤 existing `image.generate` operation으로 변환한다.
- plugin은 public Authorization, cookie, query, Host, forwarding header와 raw native credential을 받지 않는다.
- plugin response는 canonical result를 반환하고 facade가 native protocol response로 projection한다.
- protocol-specific pass-through가 필요한 Provider는 후속 versioned contract에서 명시적으로 추가한다.

### 3. Sidecar origin과 인증은 startup에 고정한다

- origin은 exact loopback HTTP 또는 configured private HTTPS만 허용하며 request/manifest가 URL/path를 바꾸지 못한다.
- redirect, proxy environment, userinfo, query와 fragment를 금지한다.
- secret reference가 가리키는 runtime secret으로 Gateway→plugin 인증 header를 생성하며 manifest에는 값이 없다.
- response header는 사용하지 않고 bounded canonical body만 해석한다.

### 4. Plugin은 가격·과금 권한을 갖지 않는다

- routing과 Wallet reserve는 existing Gateway channel/immutable price가 결정한다.
- plugin usage/cost evidence는 manifest capability와 Gateway price unit에 맞는 bounded scalar만 허용한다.
- evidence 누락·초과·identity mismatch는 Capture/Release하지 않고 reconciliation으로 보낸다.
- plugin이 반환한 arbitrary price/currency, object URL 또는 ledger instruction은 무시하거나 schema 오류로 거부한다.

### 5. 최초 vertical slice는 synchronous image generation으로 제한한다

- request는 logical/provider model, prompt와 bounded known image options를 canonical JSON으로 전달한다.
- result는 bounded Base64 image 또는 exact allowlisted Provider URL descriptor만 허용한다.
- managed image storage가 기존 SSRF/content 검증과 object persistence를 수행한 뒤 native 응답으로 projection한다.
- response commit 또는 plugin dispatch 뒤 fallback/redispatch 규칙은 기존 image executor와 동일하다.

## 설계 및 구현 순서

### 1. Manifest v1 schema와 loader

- `plugin-sdk/manifest/v1` JSON schema, Go types와 canonical validator를 추가한다.
- provider ID/name/version, compatibility, operations, models, capabilities와 transport identity를 제한한다.
- configured manifest directory의 regular file만 읽고 symlink, device, oversized file과 unsafe permission을 거부한다.
- 전체 registry를 listener bind 전에 원자적으로 생성한다.

### 2. Plugin registry와 capability bridge

- validated manifest를 immutable plugin registry snapshot으로 변환한다.
- model/candidate ID collision과 built-in Provider shadowing을 거부한다.
- `/v1/models`와 image registry가 plugin capability를 기존 projection 규칙으로 표시한다.
- manifest digest/version/provider identity를 candidate snapshot에 결합한다.

### 3. HTTP sidecar transport

- proxy environment를 사용하지 않는 fixed-origin client, timeout, connection pool과 concurrency semaphore를 제공한다.
- canonical request envelope에 bounded request/operation/plugin IDs와 options만 포함한다.
- secret-ref resolver가 outbound authentication을 적용하고 secret을 error/log/trace에서 제거한다.
- non-2xx, malformed/oversized response, timeout, reset과 panic을 bounded plugin error로 변환한다.

### 4. Image generation adapter와 Gateway orchestration

- plugin candidate를 existing image routing/executor map에 등록한다.
- request schema 변환, canonical response 검증과 managed image result collection을 연결한다.
- Provider dispatch 여부를 명확히 분류해 pre-dispatch failure만 fallback 가능하게 한다.
- immutable Gateway price/channel과 plugin usage evidence를 기존 reservation/settlement에 연결한다.

### 5. Mock server와 conformance

- `cmd/gateway-plugin-mock` 또는 test fixture가 health, success, known error, timeout, malformed/oversized result를 제공한다.
- manifest validator 명령은 CI에서 schema, compatibility, collision과 secret-pattern 검사를 수행한다.
- example manifest/sidecar와 Docker Compose profile로 end-to-end native SDK 호출을 검증한다.

## 인터페이스와 데이터 변경

### Manifest v1 개요

```json
{
  "schema_version": "nativegateway.provider/v1",
  "id": "provider.example",
  "version": "1.0.0",
  "gateway_compatibility": ">=0.1.0 <1.0.0",
  "transport": {
    "kind": "http-sidecar",
    "endpoint_ref": "example-sidecar",
    "auth_secret_ref": "example-sidecar-token"
  },
  "models": [{
    "id": "example-image-v1",
    "operations": ["image.generate"],
    "capabilities": {"media_type": "json", "output": ["base64"]}
  }]
}
```

`endpoint_ref`와 `auth_secret_ref`는 operator-owned runtime mapping의 이름이며 URL/secret 값이 아니다.

### Sidecar HTTP contract

```text
GET  /plugin/v1/health
POST /plugin/v1/execute
```

Execute envelope는 schema version, request ID, plugin/provider/version, operation, provider model과 bounded typed input을 포함한다. 응답은 success result 또는 bounded error category 중 정확히 하나를 포함한다. unknown field와 identity mismatch는 fail closed한다.

### 설정

- `GATEWAY_PLUGIN_MODE=disabled|optional|required`
- `GATEWAY_PLUGIN_MANIFEST_DIR`
- endpoint-ref→exact origin mapping
- auth-secret-ref→secret environment/file mapping
- request/response byte limit, timeout, maximum concurrency와 health interval

### 데이터베이스 및 migration

- built-in provider enum/constraint를 plugin-safe stable provider identity로 확장한다.
- selected plugin ID/version/manifest digest를 request/charge evidence에 immutable하게 저장한다.
- manifest JSON, prompt, image bytes, endpoint URL과 authentication secret은 DB에 저장하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

- `conformance`: manifest schema corpus, mock sidecar, protocol SDK end-to-end와 failure/security fixtures
- `cloud`: manifest ConfigMap/volume, endpoint/secret-ref mapping, network policy와 sidecar deployment
- `dashboard`: plugin ID/version/health/capability read projection; secret·endpoint 원문 비노출

각 저장소는 `phase-6-provider-plugin-foundation` initiative로 독립 local plan을 소유한다.

## 보안 및 과금 고려사항

- manifest directory와 endpoint mapping은 operator trust boundary이며 customer input으로 선택하지 않는다.
- sidecar network policy는 Gateway와 필요한 Provider egress 외 접근을 제한하고 DB/Redis/metadata service 접근을 금지한다.
- plugin에는 public service key, Provider master keyring, Wallet/ledger row, user/org identity 원문을 전달하지 않는다.
- prompt와 image는 operation 수행에 필요한 content이므로 sidecar payload에는 포함되지만 Gateway observability/manifest/DB에는 저장하지 않는다.
- manifest ID/version/model/options는 bounded character set과 길이로 제한해 log/metric injection과 high cardinality를 차단한다.
- health 실패, timeout, malformed output과 identity mismatch는 known pre/post-dispatch 단계에 따라 fallback·reconciliation을 결정한다.
- managed mode는 Gateway-published immutable price 없이는 plugin candidate를 활성화하지 않는다.
- plugin result URL은 manifest/provider exact origin allowlist와 existing safe collector를 통과하지 않으면 거부한다.

## 테스트 계획

### 단위 테스트

- canonical manifest parsing, unknown/duplicate field, semver compatibility와 digest stability
- ID/model/candidate collision, built-in shadowing과 capability contradiction
- endpoint/secret ref lookup, fixed origin, proxy/redirect/header stripping
- request/response schema, body/concurrency/timeout bounds와 identity mismatch
- Base64/URL result validation과 pre/post-dispatch error classification

### 통합 테스트

- manifest directory permission/symlink/oversize/startup atomicity
- mock sidecar health/execute and exact authentication
- plugin image request의 API Key/model permission/rate limit/routing/health 경계
- immutable price reservation, result evidence, Capture/Release/Reconciliation
- managed image storage와 native OpenAI/Gemini response projection
- Gateway restart, plugin unavailable/recovery와 circuit state

### 호환성 및 장애 테스트

- official OpenAI/Gemini SDK가 plugin-backed logical image model을 Base URL/Key 변경만으로 호출함
- sidecar 401/429/5xx, timeout, reset, invalid JSON, oversized/Base64/URL result
- duplicate idempotency가 단일 plugin dispatch와 Ledger effect로 수렴함
- manifest/secret/endpoint/prompt/image가 log·trace·metric·billing DB에 누출되지 않음
- malicious manifest path, symlink, secret-like field와 private URL result 거부

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai ./protocols/gemini -count=1
```

## 완료 조건

- [x] strict canonical Provider Manifest v1과 compatibility validation이 제공됨
- [x] 외부 sidecar가 코어 수정·in-process code loading 없이 image Provider를 등록함
- [x] native OpenAI/Gemini facade와 `/v1/models`가 plugin capability를 안전하게 사용함
- [x] endpoint와 authentication secret이 operator ref로만 해석되고 manifest/API/log에 노출되지 않음
- [x] fixed-origin transport가 redirect/proxy/body/timeout/concurrency를 fail closed함
- [x] managed billing이 Gateway price와 verified plugin evidence로 exactly-once 정산됨
- [x] managed image storage가 plugin result를 기존 SSRF/object 경계로 처리함
- [x] duplicate/fallback/circuit/restart가 단일 dispatch·Ledger 효과와 bounded recovery로 수렴함
- [x] mock sidecar, validator와 contributor example이 제공됨
- [x] 전체 unit/race/integration/SDK 검사가 통과함
- [x] README, migration, Docker Compose와 멀티레포 handoff가 갱신됨

## 검증 증거

- `plugin-sdk/manifest/v1`에 duplicate/unknown field 거부, typed canonical JSON SHA-256, semver compatibility와 safe directory loader를 구현했다.
- `internal/plugins`가 manifest snapshot을 collision 검사된 OpenAI/Gemini image route와 exact channel binding으로 변환하고, migration `000056` 및 `plugin_channel_snapshots`에 plugin ID/version/digest/model/protocol을 불변 저장한다.
- fixed-origin HTTP client는 proxy와 redirect를 차단하고 bearer secret ref, 전체 timeout, request/response limit, concurrency semaphore, strict identity/error/result 검증과 exact URL origin 검사를 적용한다.
- `providers/plugin`이 bounded prompt/options를 canonical sidecar envelope로 변환하고 Base64/URL 결과를 native OpenAI Images 및 Gemini `generateContent` 응답으로 projection한다. Gemini routing·storage·telemetry도 실제 selected provider를 유지한다.
- managed billing integration test `TestPluginBillingSnapshotAndIdempotencyAreExactlyOnce`가 Gateway-published immutable price, 단일 sidecar dispatch, Capture, replay와 plugin charge snapshot을 검증한다.
- `cmd/gateway-plugin-validator`, `cmd/gateway-plugin-mock`, `examples/plugin`, Compose profile과 `examples/plugin/HANDOFF.md`를 제공했다. 실제 authenticated health/execute smoke도 통과했다.
- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과.
- fresh `gateway_plan062` DB와 Redis DB 14에서 `GOFLAGS=-p=1 make integration-test` 통과.
- `GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai ./protocols/gemini -count=1` 통과. Plugin-backed model을 공식 OpenAI/Gemini Python 및 JavaScript SDK가 Base URL과 service key만 바꿔 호출했다.

## Rollback 계획

- `GATEWAY_PLUGIN_MODE=disabled`로 plugin manifest loading, route candidate와 health probe를 중단한다.
- built-in Provider adapter와 기존 native protocol route는 영향 없이 계속 동작한다.
- plugin-backed 진행 중 charge는 기존 reconciliation 정책으로 drain하고 임의 Release/redispatch하지 않는다.
- additive provider/plugin evidence schema는 감사와 downgrade 호환성을 위해 유지한다.

## 후속 작업

- async Job/webhook plugin contract for image/video
- streaming LLM/audio plugin contract
- gRPC sidecar transport and mTLS identity
- signed official Adapter Registry and supply-chain verification
- Adapter templates, generated documentation and multi-language SDK
- runtime version negotiation, controlled hot reload and health rollout
