---
id: gateway-20260824-065
title: Phase 6 Public Async Plugin Runtime, Signed Callback, and Conformance
status: accepted
created_at: 2026-08-24T17:26:46+09:00
updated_at: 2026-08-24T17:26:46+09:00
owners:
  - gateway
initiative: phase-6-async-plugin-runtime
depends_on:
  - gateway-20260820-029
  - gateway-20260821-034
  - gateway-20260824-063
  - gateway-20260824-064
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - registry
---

# Phase 6 Public Async Plugin Runtime, Signed Callback, and Conformance

## 목적

외부 Provider Adapter가 Gateway 코어 수정 없이 public async sidecar SDK로 image Job을 submit/poll/cancel하고, 선택적으로 서명된 completion callback을 보내며, 기존 Replicate/fal native facade·durable Job·usage-aware Wallet 정산에 정확히 한 번 수렴하게 한다. 독립 Adapter가 실제 Provider 호출 없이 이 계약을 검증할 async conformance profile과 reference template도 함께 제공한다.

## 배경

Plan 062~064는 synchronous image sidecar, public runtime/conformance SDK와 signed official Registry admission을 제공했다. 그러나 manifest와 runtime v1은 `image.generate` 동기 실행만 허용하며, `jobs.Provider`는 코어에 컴파일된 Replicate/fal/Runway Adapter만 구현한다. 외부 Adapter는 장시간 작업을 등록하거나 Gateway 재시작 후 poll/cancel을 재개할 수 없고, Provider webhook을 durable Job과 Ledger에 연결할 수도 없다.

Plan 029~034의 Job 코어는 Gateway Job ID와 Provider Job ID 분리, lease polling, cancel, webhook/poll terminal CAS, 실제 output usage와 partial Capture를 이미 검증했다. 이번 계획은 이 불변 조건을 새로 만들지 않고 public sidecar wire와 plugin route에 연결한다. 첫 vertical slice는 기존 native SDK와 과금 증거가 완성된 Replicate/fal image operation으로 제한한다. Video, streaming과 Provider 고유 webhook signature 검증을 한 계획에 섞지 않는다.

## 범위

- Provider Manifest v1의 backward-compatible async image execution 선언
- `plugin-sdk/async/v1` submit, poll, cancel, observation과 callback canonical wire
- fixed sidecar paths, request identity, opaque upstream Job ref와 bounded image/usage 결과
- Gateway `jobs.Provider`를 구현하는 async plugin client와 fixed-origin/auth/concurrency 경계
- Replicate/fal native route가 plugin candidate를 선택하고 기존 Job service를 사용하도록 확장
- per-Job Gateway-owned callback capability와 purpose-separated plugin callback HMAC
- plugin callback delivery replay protection과 poll/cancel terminal CAS 통합
- success/partial success Capture, failure/cancel Release, unknown usage reservation 유지
- process restart 후 persisted Provider Job ref를 사용한 poll/cancel 복구
- async conformance runner, stable report/check set, fixture corpus와 CLI profile
- standalone Go sidecar template의 deterministic async test mode
- signed Registry admission의 `async/v1` runtime/conformance profile 결속
- immutable plugin channel/Job/charge/Registry evidence 유지
- README, contributor policy, Makefile와 멀티레포 handoff 갱신

## 제외 범위

- video generation, image-to-video, managed video output와 Runway plugin route
- LLM/audio streaming, SSE, WebSocket, realtime 또는 bidirectional transport
- Provider가 고객 callback URL로 결과를 전달하거나 Gateway가 사용자 webhook을 발송하는 기능
- Gateway가 임의 Provider webhook signature/JWKS 규격을 plugin 대신 검증하는 기능
- sidecar가 Gateway DB, Redis, Wallet, Ledger, object storage 또는 tenant identity에 접근하는 방식
- submit 이후 cross-provider fallback, hedging 또는 다른 Adapter로 Provider Job 이전
- arbitrary native response body/header pass-through와 plugin-defined public route
- remote manifest/artifact 다운로드, process/container lifecycle와 hot reload
- gRPC/mTLS, workload identity와 network sandbox 자동 구성
- async Registry publication/signing pipeline과 타 저장소 내부 구현

## 핵심 결정

### 1. Async wire는 synchronous runtime과 별도 versioned package다

- 기존 `plugin-sdk/runtime/v1` wire와 conformance digest를 변경하지 않는다.
- `plugin-sdk/async/v1`은 submit/poll/cancel/callback 전용 schema와 codec을 소유한다.
- fixed endpoints는 `POST /plugin/async/v1/submit`, `POST /plugin/async/v1/poll`, `POST /plugin/async/v1/cancel`이다.
- request는 plugin/version/manifest, Gateway request/Job, protocol/operation/model과 bounded typed image input을 결속한다.
- public service API Key, organization/project/API Key ID, channel price, Provider credential과 callback signing key는 wire에 포함하지 않는다.

### 2. Provider Job ref는 opaque 내부 identity다

- submit success는 non-empty bounded `provider_job_ref`와 `QUEUED|PROCESSING|terminal` observation을 반환한다.
- Gateway 공개 응답에는 항상 Gateway Job ID만 사용하고 ref는 Provider attempt와 sidecar request에만 존재한다.
- poll/cancel은 persisted ref와 exact plugin/channel/manifest identity를 sidecar에 다시 보낸다.
- submit timeout 또는 response loss는 다른 Provider에 redispatch하지 않고 `RECONCILING`으로 남긴다.
- ref, poll URL과 upstream control URL은 log, telemetry, event detail과 public snapshot에서 제외한다.

### 3. Sidecar 결과는 canonical operation result다

- plugin은 arbitrary Replicate/fal body를 반환하지 않고 typed status, image descriptors, verified usage와 bounded error category를 반환한다.
- Gateway가 canonical result를 기존 Replicate/fal native terminal snapshot으로 projection한다.
- image는 Base64 또는 manifest와 operator가 허용한 HTTPS result origin만 사용하며 managed storage는 기존 collector 경계를 재사용한다.
- actual usage는 output/image count와 result 개수가 일치해야 하고 reserved maximum을 넘을 수 없다.
- malformed, excessive 또는 conflicting terminal usage는 자동 Capture/Release하지 않고 기존 manual reconciliation으로 보낸다.

### 4. Callback은 per-Job capability와 별도 HMAC을 모두 요구한다

- Gateway는 callback 지원 manifest에만 자체 HTTPS callback URL을 submit request에 포함한다.
- callback route는 `POST /internal/webhooks/plugin/{gateway_job_id}/{capability_token}`으로 고정한다.
- capability 원문은 충분한 entropy로 생성해 sidecar에 한 번 전달하고 DB에는 deployment callback secret으로 계산한 keyed digest만 저장한다.
- callback HMAC key는 Gateway→sidecar bearer와 다른 operator secret ref를 사용한다. key reuse나 manifest 내 secret 값은 허용하지 않는다.
- signature는 timestamp, delivery ID와 exact raw canonical body를 결속하며 timestamp tolerance, duplicate header, body bound와 constant-time comparison을 적용한다.
- sidecar는 자신이 사용하는 upstream Provider webhook을 검증·정규화한 뒤에만 이 callback 계약을 호출한다.

### 5. Polling은 callback의 fallback이며 정산 경로는 하나다

- callback을 지원해도 durable polling을 제거하지 않는다.
- poll, callback과 cancel observation은 existing repository terminal CAS, usage evidence와 settlement lease를 사용한다.
- delivery ID replay, semantic terminal replay와 worker lease race가 event/usage/Ledger effect를 늘리지 않아야 한다.
- callback/JWKS/upstream 검증의 정확성은 Adapter 책임이지만 Gateway는 plugin signature, capability, Job/channel/ref identity와 observation schema를 독립 검증한다.
- terminal 확인 전 timeout, transport error, signature failure와 unknown usage는 reservation을 유지한다.

### 6. Native protocol 선택과 가격은 계속 Gateway가 소유한다

- Replicate/fal handler는 route Provider가 built-in 또는 `plugin`인지 검사한 뒤 동일 Job service에 immutable selected channel을 전달한다.
- plugin candidate는 해당 native protocol과 `image.generate`를 manifest에 명시해야 한다.
- Gateway-published channel price, model capability와 usage extractor가 Reserve 상한을 결정한다.
- Adapter가 반환한 price, currency, cost, margin, refund 또는 Ledger instruction은 schema에 존재하지 않는다.
- pre-dispatch route 실패만 기존 정책상 fallback 가능하고 Job submit이 시작된 뒤에는 candidate를 바꾸지 않는다.

## 설계 및 구현 순서

### 1. Manifest async capability와 Registry profile

- 기존 manifest가 canonical digest 변경 없이 계속 parse되도록 optional async block을 추가한다.
- async block은 mode, callback 지원 여부와 operation contract version만 선언하며 timeout, endpoint, secret 또는 가격을 선언하지 않는다.
- v1 async activation은 `image.generate`와 protocol `replicate|fal`, JSON input, Base64/URL image output만 허용한다.
- sync OpenAI/Gemini와 async Replicate/fal capability의 모순, mixed mode, duplicate model/protocol과 unsupported operation을 거부한다.
- Registry admission이 runtime `async/v1`, async conformance schema/check-set digest와 exact manifest를 결속하도록 strict enum/profile validation을 확장한다.

### 2. Public async SDK

- identity, submit/poll/cancel request, submit response, observation, image result, usage, error와 callback envelope 타입을 추가한다.
- strict duplicate/unknown/trailing/oversized JSON, canonical encode/digest와 wire golden을 제공한다.
- status/transition, result/error exclusivity, Provider Job ref, callback URL, image MIME/magic/URL, usage와 failure category를 bounded 검증한다.
- SDK는 Gateway internal package를 import하지 않고 HTTP client, DB, signing private key나 Provider별 payload 타입을 포함하지 않는다.
- callback signature PAE/message helper는 제공하되 secret loading·rotation과 HTTP ingress는 각 실행 환경이 소유한다.

### 3. Async plugin Provider와 Job 연결

- fixed-origin client가 proxy/redirect를 차단하고 bearer auth, timeout, response/body/concurrency limit를 sync client와 공유한다.
- submit/poll/cancel을 public async codec으로 호출하고 typed result를 `jobs.SubmitResult`/`job.Observation`으로 변환한다.
- Provider attempt가 plugin ID/version/manifest/admission identity를 잃지 않도록 immutable channel snapshot과 ref lookup을 사용한다.
- Replicate/fal handler의 hard-coded Provider check를 built-in 또는 admitted/configured plugin channel check로 대체한다.
- async plugin model route를 image Registry와 availability에 추가하되 built-in shadowing과 ambiguous provider selection을 거부한다.

### 4. Signed plugin callback ingress

- callback-secret ref→runtime secret mapping과 public base/TTL/tolerance/body bound 설정을 추가한다.
- existing async webhook binding/delivery schema를 plugin provider와 callback signer/plugin identity까지 additive 확장한다.
- raw body 서명 검증 후 capability, Job, channel, manifest, Provider Job ref와 delivery replay를 검증한다.
- observation/delivery/usage intent를 한 transaction으로 적용하고 성공 commit 이후에만 2xx를 반환한다.
- malformed/auth/identity 오류는 bounded 4xx, early submit-confirm race와 transient DB 오류는 retry 가능한 5xx로 구분한다.
- callback path/token, signature, delivery/ref와 body는 access log, trace attribute, metric label과 durable error에 기록하지 않는다.

### 5. Async conformance와 template

- `plugin-sdk/conformance/async/v1` runner가 health/auth, submit queued, poll processing/success, cancel, callback signing, duplicate/malformed/timeout/cancellation을 test mode로 검사한다.
- report는 async schema/SDK/check ID/outcome/category/duration과 manifest digest만 포함한다.
- `gateway-plugin-conformance -profile async-v1` 또는 별도 명확한 subcommand로 async runner를 실행한다.
- fixture corpus는 valid request/observation과 identity/ref/status/result/usage/signature invalid cases를 제공한다.
- standalone template가 in-memory deterministic Job state와 signed callback을 구현하되 실제 Provider call이나 유료 사용을 요구하지 않는다.

### 6. 과금·저장·운영 회귀

- maximum output Reserve, terminal actual usage, partial Capture/remainder Release와 unknown hold를 실제 Wallet/Ledger에서 검증한다.
- managed image output은 terminal settlement 전에 기존 storage manager로 변환되고 재시작/재처리에서 동일 object/evidence로 수렴한다.
- callback-disabled 또는 plugin mode disabled rollback에서도 이미 submit된 Job은 persisted channel/ref로 drain하거나 manual reconciliation에 보존한다.
- readiness는 required sidecar health를 반영하되 개별 long-running Job이나 missed callback은 readiness를 내리지 않는다.
- README, template, contributor policy와 Cloud/Registry/Conformance handoff를 갱신한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 고객용 Replicate/fal native endpoint와 response schema는 변경하지 않는다. Provider sidecar contract와 Provider 전용 callback ingress가 추가된다.

```text
POST /plugin/async/v1/submit
POST /plugin/async/v1/poll
POST /plugin/async/v1/cancel
POST /internal/webhooks/plugin/{gateway_job_id}/{capability_token}
```

### 내부 인터페이스

- async plugin client가 기존 `jobs.Provider`를 구현한다.
- plugin registry가 sync image route와 async image route를 mode별로 구분해 제공한다.
- protocol handler는 selected candidate의 provider/channel을 그대로 Job `CreateRequest`에 저장한다.
- callback verifier는 clock, HMAC keyring과 body limit를 주입받고 repository에는 verified typed observation만 전달한다.

### 데이터베이스 및 migration

- existing webhook binding/delivery provider constraint를 `plugin`까지 확장한다.
- binding에 immutable plugin ID/version/manifest digest 또는 이를 가리키는 channel snapshot identity를 결속한다.
- 필요하면 callback signer key version과 provider-job-ref digest를 추가하되 capability/ref 원문은 저장하지 않는다.
- 기존 Replicate/fal rows, Job/attempt/event, usage evidence와 Ledger는 rewrite/backfill하지 않는다.
- migration은 additive 또는 constraint replacement이며 구 binary가 기존 row를 계속 읽을 수 있어야 한다.

### 다른 저장소에 제공하거나 요구하는 계약

- `conformance`: 동일 `phase-6-async-plugin-runtime` initiative로 공식 Replicate/fal SDK submit/poll/cancel/restart와 async fixture corpus를 독립 검증한다.
- `registry`: async runtime/conformance profile의 isolated rerun과 signed admission publication을 소유한다.
- `cloud`: sidecar callback secret injection, HTTPS ingress, network policy, non-terminal backlog와 callback/poll failure alert를 소유한다.
- Gateway는 public SDK schema, callback signature/input bounds, retry status, manifest/Registry profile과 native projection을 versioned artifact로 제공한다.

## 보안 및 과금 고려사항

- sidecar는 content 수행에 필요한 prompt/options와 opaque Job ref만 받고 tenant/API Key, Wallet, price, credential keyring과 public Authorization은 받지 않는다.
- endpoint와 secret refs는 operator trust input이고 customer request/manifest가 URL, callback origin, header나 secret 값을 선택하지 못한다.
- callback은 signature 검증 전에 DB lookup하지 않고 exact raw body, timestamp와 delivery ID를 HMAC에 결속한다.
- callback bearer와 outbound bearer를 분리하고 capability는 keyed digest만 보존한다.
- result URL은 fetch하지 않고 syntax/declared origin을 검증한 뒤 managed collector의 SSRF/DNS/content 경계를 사용한다.
- submit 시작 뒤 timeout은 Provider 생성 여부가 불명확하므로 fallback/Release하지 않는다.
- success verified usage만 Capture하고 known failed/canceled zero output만 Release한다. excessive/missing/conflicting usage는 reservation을 유지한다.
- Job terminal CAS, webhook replay key, usage evidence unique key, settlement lease와 Ledger operation key가 중복 poll/callback/restart의 이중 정산을 방어한다.
- Registry admission은 Adapter artifact identity를 증명하지만 runtime sandbox를 대체하지 않는다.

## 테스트 계획

### 단위 테스트

- manifest sync/async backward compatibility, contradiction과 canonical digest
- async request/response/callback strict codec, identity/status/ref/result/usage wire golden
- fixed-origin/auth/header stripping, timeout/body/concurrency와 error stage mapping
- callback raw-body HMAC, key rotation, timestamp/duplicate header/capability와 secret-safe error
- canonical result→Replicate/fal native snapshot과 image/usage validation
- async conformance check ordering, stable report digest와 invalid corpus

### 통합 테스트

- plugin candidate→Replicate/fal native submit→durable Provider attempt→poll terminal 결과
- cancel, submit response loss, poll timeout/backoff와 Gateway restart recovery
- signed callback success/failure/cancel, duplicate delivery와 early callback retry
- callback/poll/cancel concurrent terminal CAS와 stale lease rejection
- maximum Reserve→partial actual Capture/remainder Release 및 unknown usage hold
- managed image storage, immutable channel/Registry/Job/charge evidence
- migration fresh/current upgrade와 existing Replicate/fal webhook row compatibility

### 호환성 및 장애 테스트

- 공식 Replicate Python과 fal JavaScript/Python SDK가 Base URL/Key만 바꿔 plugin-backed model을 submit/get/result/cancel함
- sidecar 401/429/500, redirect, reset, malformed/oversized body, timeout과 restart
- callback signature/body 변조, replay, response loss, stale/future timestamp와 key rotation
- plugin disabled/Registry required/yanked/expired admission과 polling fallback
- existing synchronous plugin OpenAI/Gemini SDK, built-in Replicate/fal/Runway와 전체 billing regression
- external Go module/template가 Gateway `internal/` package 없이 build/test/conformance함

### 필수 검증 명령

```text
GOCACHE=/private/tmp/nativegateway-go-cache make check
GOCACHE=/private/tmp/nativegateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/nativegateway-go-cache go test -count=1 -tags=sdkconformance ./protocols/replicate ./protocols/fal
GOCACHE=/private/tmp/nativegateway-go-cache go test -count=1 -tags=sdkconformance ./protocols/openai ./protocols/gemini
GOWORK=off GOCACHE=/private/tmp/nativegateway-go-cache go test -race ./plugin-sdk/...
go run ./cmd/gateway-plugin-conformance -profile async-v1 ...
```

## 완료 조건

- [ ] manifest가 기존 sync digest/동작을 보존하면서 async Replicate/fal image capability를 strict 선언함
- [ ] 외부 Adapter가 public SDK만으로 submit/poll/cancel과 signed callback을 구현함
- [ ] Gateway Job ID와 opaque Provider Job ref가 public/durable 경계에서 분리됨
- [ ] plugin-backed Replicate/fal native SDK 요청이 durable Job과 terminal native 결과로 수렴함
- [ ] callback capability·HMAC·Job/channel/ref identity와 replay가 fail closed함
- [ ] polling fallback, cancel, timeout과 재시작이 post-submit redispatch 없이 복구됨
- [ ] partial success/failure/cancel/unknown usage가 기존 Wallet/Ledger 정책으로 정확히 한 번 정산됨
- [ ] managed image와 immutable manifest/Registry/channel/Job/charge evidence가 보존됨
- [ ] async conformance runner, report, fixtures와 standalone template가 제공됨
- [ ] callback/ref/secret/tenant/content가 log·telemetry·report·DB 오류에 노출되지 않음
- [ ] 기존 synchronous plugin, built-in Provider와 공식 OpenAI/Gemini/Replicate/fal SDK가 회귀하지 않음
- [ ] unit/race/integration/SDK/public-module 검사가 통과함
- [ ] README, CONTRIBUTING, examples, Makefile와 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 신규 async manifest/channel publication을 중단하고 plugin async mode를 비활성화한다.
- submit 전 요청은 built-in candidate 또는 명시적 unavailable로 처리한다.
- 이미 submit된 plugin Job은 같은 manifest/channel/ref를 유지한 worker로 drain한다. sidecar를 즉시 제거하거나 다른 Provider로 redispatch하지 않는다.
- sidecar 복구가 불가능한 Job은 reservation을 임의 Release하지 않고 manual reconciliation 상태로 보존한다.
- callback ingress만 문제가 있으면 callback injection을 끄고 polling fallback을 유지한다.
- additive binding/delivery/evidence row와 이미 기록된 Ledger entry는 삭제·rewrite하지 않는다.

## 후속 작업

- async video plugin runtime, Runway/native video projection과 managed video storage
- streaming LLM/audio plugin runtime and conformance
- gRPC/mTLS sidecar identity와 workload-level network policy
- controlled manifest/runtime reload, connection drain과 health rollout
- callback delivery retention/archival과 management audit projection
