---
id: gateway-20260820-030
title: Phase 3 Replicate Native Predictions
status: in_progress
created_at: 2026-08-20T22:14:27+09:00
updated_at: 2026-08-20T22:14:27+09:00
owners:
  - gateway
initiative: phase-3-replicate-native-predictions
depends_on:
  - gateway-20260820-007
  - gateway-20260820-011
  - gateway-20260820-018
  - gateway-20260820-019
  - gateway-20260820-023
  - gateway-20260820-026
  - gateway-20260820-027
  - gateway-20260820-028
  - gateway-20260820-029
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 3 Replicate Native Predictions

## 목적

공식 Replicate SDK 또는 REST client가 서비스 Key와 Gateway Base URL만 사용해 community model Prediction을 생성·조회·취소하고, Gateway의 durable Job·과금·credential·storage 불변식을 유지하면서 Replicate native JSON을 받게 한다.

## 배경

Plan 029는 protocol-neutral Job, submit/poll/cancel lease와 terminal settlement를 제공하지만 공개 HTTP facade와 실제 Provider adapter는 없다. Replicate 공식 HTTP API는 Bearer 인증 아래 `POST /v1/predictions`, `GET /v1/predictions/{id}`, `POST /v1/predictions/{id}/cancel`을 사용하며 `starting`, `processing`, `succeeded`, `failed`, `canceled` 상태를 반환한다. JavaScript SDK는 custom `baseUrl`을 공식 지원하고, 현재 Python v2 SDK는 constructor 또는 `REPLICATE_BASE_URL`로 base URL을 지정할 수 있다. Gateway는 upstream Prediction ID 대신 opaque Gateway Job ID를 반환해야 하므로 응답의 ID와 `urls.get/cancel`을 일관되게 재작성하고 Provider identity를 durable native snapshot에서도 제거해야 한다.

## 범위

- Replicate Bearer 서비스 Key 인증과 native 오류 envelope
- `POST /v1/predictions`
- `GET /v1/predictions/{gateway_job_id}`
- `POST /v1/predictions/{gateway_job_id}/cancel`
- version 기반 community model 요청 validation과 exact capability/routing
- `Prefer: wait`/`Prefer: respond-async`의 bounded native 동작
- `Cancel-After` validation과 upstream 전달
- Replicate Provider credential/channel resolution과 outbound header sanitization
- 실제 Replicate submit/get/cancel adapter와 no-redirect/timeout/body limit
- Replicate 상태와 Gateway Job 상태의 명시적 매핑
- Gateway Job ID 및 configured public base URL을 사용한 native `id`/`urls` 재작성
- non-terminal/terminal native prediction snapshot의 bounded durable 저장
- Job create idempotency, Wallet reserve, terminal Capture/Release와 unknown reconciliation
- Provider polling worker bootstrap과 readiness
- Replicate protocol을 API Key permission, pricing, charge와 telemetry allowlist에 추가
- mock upstream 및 JavaScript/Python SDK wire conformance fixture

## 제외 범위

- official model `POST /v1/models/{owner}/{name}/predictions`
- deployment prediction endpoint와 Prediction list
- trainings, models, collections, deployments와 account API
- streaming/SSE output
- inbound Replicate webhook과 signature verification
- client-supplied `webhook`, `webhook_events_filter`와 arbitrary callback delivery
- file upload/data URI 자동 변환 및 upload endpoint
- fal queue protocol
- cross-provider protocol conversion과 post-submit fallback
- dynamic per-second runtime pricing과 partial-output billing
- Provider output URL의 managed storage 변환; 후속 async result storage 계획으로 분리

## 설계 및 구현 순서

### 1. Replicate native request/response contract

- create body는 bounded JSON object이며 `version`과 object `input`을 요구한다.
- version은 exact logical model identifier로 취급하며 prefix guess나 임의 upstream version을 허용하지 않는다.
- unknown JSON fields는 upstream pass-through와 native snapshot에서 보존하되 `webhook` 관련 필드는 초기 릴리스에서 명시적으로 거부한다.
- native status는 Gateway `PENDING/QUEUED→starting`, `PROCESSING/RECONCILING→processing`, terminal 상태는 동일 의미로 매핑한다.
- public `id`, `urls.get`, `urls.cancel`은 Gateway Job ID와 configured HTTPS public base URL로 재작성하고 Provider ID/URL을 제거한다.
- `input`, `output`, timestamps, metrics와 알려지지 않은 safe fields는 SDK decoding 호환을 위해 크기 제한된 protocol snapshot으로 보존한다.

### 2. 인증·권한·registry·과금 확장

- 기존 Bearer service Key 추출, network restriction와 distributed rate limit을 재사용한다.
- API Key model permission과 Capability Registry에 `replicate:image.generate:<version>`을 추가한다.
- Replicate protocol route는 초기에는 Replicate Provider candidate만 허용해 native pass-through를 보장한다.
- billing charge protocol constraint/validation을 Replicate image generation까지 additive migration으로 확장한다.
- fixed per-prediction price snapshot으로 Wallet/Quota/Spend-cap을 reserve하며 terminal succeeded만 Capture한다.
- pricing/credential/spend-cap 실패는 submit 전에만 candidate fallback할 수 있고 Provider submit 이후 fallback은 금지한다.

### 3. Provider adapter

- configured Replicate origin의 고정 path만 조합하고 client 또는 upstream `urls`를 요청 대상으로 사용하지 않는다.
- inbound Authorization, cookies, forwarding/tracing headers와 secret query를 제거한 뒤 selected channel credential만 Bearer로 적용한다.
- create에는 validated body와 allowlisted `Prefer`, `Cancel-After`, content negotiation만 전달한다.
- get/cancel은 내부 Provider Job ID로 고정 origin path를 구성한다.
- redirect를 따르지 않고 connect/response/body 제한을 적용한다.
- 2xx prediction object는 typed observation으로 변환하며 429/5xx/timeout/connection loss는 known/unknown 범주로 분리한다.
- raw upstream error, input/output, Provider ID와 URL을 log/event/telemetry에 기록하지 않는다.

### 4. Job과 native snapshot 결합

- Job create, charge reserve와 submit intent 순서를 고정하고 `Idempotency-Key` replay가 같은 Gateway Job을 반환하게 한다.
- Plan 029 snapshot 계약을 non-terminal native state에도 확장하되 settlement snapshot과 public protocol snapshot의 책임을 구분한다.
- Provider response는 저장 전에 Gateway ID/URL로 sanitize하며 terminal snapshot hash와 size/header allowlist를 검증한다.
- create가 즉시 terminal이면 동일 응답에서 settlement intent를 만들고, async이면 worker polling이 snapshot과 상태를 함께 갱신한다.
- GET은 Provider를 직접 호출하지 않고 durable snapshot을 반환해 client polling 폭주가 upstream 호출 수를 늘리지 않게 한다.
- worker만 due Job을 poll하며 Provider observation과 native snapshot을 원자적으로 적용한다.

### 5. Cancel과 wait semantics

- cancel은 tenant ownership을 확인하고 Plan 029 cancel-once lease로 Provider를 한 번 호출한다.
- Provider가 canceled를 확정한 경우만 native `canceled`와 Release를 적용한다.
- success/cancel 경합은 terminal CAS로 하나만 승리하며 conflict는 reconciliation 대상으로 남긴다.
- `Prefer: wait`은 1~60초 범위만 허용하고 Gateway request context 안에서 durable Job 상태를 기다린다. timeout 시 현재 native non-terminal Prediction을 반환하며 submit을 반복하지 않는다.
- client disconnect는 Job 또는 Provider 작업을 자동 취소하지 않는다.

### 6. Runtime과 SDK compatibility

- Replicate credential, upstream endpoint/timeouts, public base URL, worker polling/lease/backoff 설정을 검증한다.
- adapter/worker가 설정된 경우에만 Replicate route를 registry와 readiness에 게시한다.
- JavaScript `Replicate({auth, baseUrl})`와 Python v2 `Replicate(bearer_token, base_url)` create/get/cancel 요청을 fixture 또는 subprocess로 검증한다.
- legacy Python v1의 custom endpoint 제약은 conformance 문서에 명시하고 가능한 custom HTTP client 경로를 별도 검증한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/predictions
GET  /v1/predictions/{gateway_job_id}
POST /v1/predictions/{gateway_job_id}/cancel
```

성공 응답은 Replicate Prediction JSON을 유지한다. `id`와 Gateway 소유 `urls.get/cancel`만 의도적으로 Gateway namespace로 변환한다. 인증·validation·not-found·rate-limit·Provider 오류는 Replicate의 `{"detail": ...}` 계열 JSON과 적절한 HTTP status로 반환한다.

### 내부 인터페이스

Plan 029 `jobs.Provider`를 구현하는 Replicate adapter를 추가한다. observation은 sanitized native snapshot을 포함하고 Provider Job ID는 별도 typed field로만 전달한다. Protocol handler는 Job public snapshot store와 service의 Create/Get/Cancel을 사용하며 Provider credential을 직접 읽지 않는다.

### 데이터베이스 및 migration

- charge와 API Key model permission의 protocol check를 `replicate` image generation까지 확장한다.
- Replicate protocol snapshot은 Job에 bounded bytea/hash로 저장하고 Provider ID/URL이 포함되지 않는 invariant를 application layer에서 검증한다.
- 기존 OpenAI/Gemini rows와 indexes는 변경하지 않는 additive migration을 사용한다.
- migration은 rolling deployment에서 구 binary가 Replicate rows를 읽지 않아도 기존 endpoint를 계속 처리할 수 있어야 한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 `phase-3-replicate-native-predictions` initiative에서 현재 공식 JavaScript/Python SDK 버전, Base URL 설정과 create/get/cancel fixture를 소유한다.
- Cloud는 Replicate channel credential, exact per-version price, public base URL, worker configuration과 Job backlog/manual reconciliation alert를 배포한다.
- Gateway는 public Job ID namespace, native response rewrite와 error/status mapping을 versioned 계약으로 제공한다.

## 보안 및 과금 고려사항

- 서비스 Key와 Provider credential은 각각 inbound/outbound 경계에서 분리하며 로그·response·snapshot에 저장하지 않는다.
- client webhook URL은 SSRF와 data exfiltration 방지를 위해 초기 릴리스에서 거부한다.
- upstream `urls`를 fetch target으로 신뢰하지 않고 fixed configured origin과 internal Provider Job ID만 사용한다.
- native input/output snapshot은 크기 제한을 적용하고 logs/events/telemetry에는 포함하지 않는다.
- public base URL은 startup 설정으로 고정해 Host header injection을 차단한다.
- submit 전 `Reserve`, 성공 `Capture`, 확정 실패/취소 `Release`, timeout/unknown `RECONCILING` 순서를 보존한다.
- SDK/client retry와 Idempotency-Key는 Provider submit, Job, charge와 Ledger operation을 중복 생성하지 않는다.
- Provider submit 이후 어떤 HTTP status/timeout에서도 다른 Provider로 fallback하지 않는다.
- GET polling은 과금이나 Provider dispatch를 발생시키지 않는다.

## 테스트 계획

### 단위 테스트

- create body, version/input, webhook/size/unknown field validation
- Prefer/Cancel-After parsing과 bounds
- Replicate↔Gateway status/error mapping
- ID/URL rewrite와 Provider identity 제거
- outbound header/query sanitization, fixed-origin path와 redirect rejection
- SDK-safe native prediction marshal/unmarshal

### 통합 테스트

- Replicate API Key permission, registry route, price와 billing migration
- create idempotency와 concurrent submit 1회
- async create→worker poll→GET succeeded→Capture
- known failed/canceled→Release, timeout/connection→reservation 유지
- GET 폭주에서 upstream poll 증가 없음
- cancel/poll terminal race와 duplicate cancel
- Gateway restart와 stale worker lease recovery
- non-terminal/terminal native snapshot durability와 hash/size bound

### 호환성 및 장애 테스트

- official Replicate JavaScript와 Python v2 SDK Base URL/Key 교체 create/get/cancel
- upstream 400/401/404/409/429/500, malformed/oversized JSON과 redirect
- submit response loss, slow poll, cancel response loss와 database conflict
- malicious Provider ID/URL, Host header, webhook and credential/header injection
- existing OpenAI/Gemini SDK, billing, storage, telemetry regression

### 필수 검증 명령

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
go build ./cmd/gateway-quota
go build ./cmd/gateway-spend-cap
go build ./cmd/gateway-provider-credential
make integration-test
```

## 완료 조건

- [ ] Replicate create/get/cancel native routes와 error envelope가 구현됨
- [ ] service Key와 configured Base URL만으로 공식 JavaScript/Python v2 SDK 호출이 성공함
- [ ] version/input/Prefer/Cancel-After와 body bounds가 native하게 검증됨
- [ ] exact registry/permission/price/channel/credential route만 submit됨
- [ ] Gateway Job ID가 모든 public ID/URL에 사용되고 Provider ID/URL이 노출되지 않음
- [ ] GET은 durable native snapshot만 읽고 Provider 또는 Billing을 호출하지 않음
- [ ] concurrent/idempotent create가 Provider submit과 reserve를 한 번만 수행함
- [ ] succeeded Capture, failed/canceled Release, unknown reservation 유지가 검증됨
- [ ] cancel/poll/webhook terminal 경합과 재시작 lease recovery가 수렴함
- [ ] upstream redirect/raw error/credential/input/output이 보안 경계를 넘지 않음
- [ ] Provider submit 이후 fallback이 없음
- [ ] 기존 OpenAI/Gemini 동작과 전체 race/integration 테스트가 회귀하지 않음
- [ ] README·SDK 예제·멀티레포 handoff가 갱신됨
- [ ] commit, PR과 최종 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Replicate route publication과 worker를 비활성화하고 이전 binary로 rollback한다.
- non-terminal Job을 drain하거나 `RECONCILING`으로 보존하며 예약을 임의 Release하지 않는다.
- additive protocol/schema 변경은 유지해 rolling rollback 중 기존 OpenAI/Gemini endpoint에 영향을 주지 않는다.
- Provider submit이 이미 발생한 Job을 재제출하거나 다른 Provider로 fallback하지 않는다.

## 후속 작업

- official model/deployment Prediction endpoints
- signed Replicate webhook ingress
- async output managed storage와 authenticated file delivery
- dynamic runtime/metric-based pricing
- legacy Replicate Python v1 compatibility shim if custom transport alone is insufficient
- fal native Queue protocol
