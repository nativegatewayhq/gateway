---
id: gateway-20260824-066
title: Phase 6 Async Video Plugin Runtime, Runway Native Projection, and Managed Delivery
status: accepted
created_at: 2026-08-24T18:23:59+09:00
updated_at: 2026-08-24T18:23:59+09:00
owners:
  - gateway
initiative: phase-6-async-video-plugin-runtime
depends_on:
  - gateway-20260822-143000
  - gateway-20260822-170000
  - gateway-20260822-213000
  - gateway-20260824-064
  - gateway-20260824-065
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - registry
---

# Phase 6 Async Video Plugin Runtime, Runway Native Projection, and Managed Delivery

## 목적

외부 Provider Adapter가 Gateway 코어 수정 없이 public video sidecar SDK로 text-to-video와 image-to-video Job을 submit/poll/cancel하고, 기존 Runway native API·durable Job·provider-credit 정산·managed video delivery에 수렴하게 한다. 이미지 async wire를 확장하지 않고 video operation의 입력, 결과, usage와 conformance를 독립 versioned contract로 제공한다.

## 배경

Plan 065는 Replicate/fal image Job을 public async sidecar에 연결하고 signed callback, polling fallback, exact-once output usage settlement와 managed image 저장을 완성했다. 영상은 이미 built-in Runway Adapter, `/v1/text_to_video`, `/v1/image_to_video`, `/v1/tasks/{id}`, ephemeral upload, provider-credit 정산과 managed object delivery를 갖고 있지만 `jobs.Provider`가 코어에 컴파일되어 있어 외부 영상 Provider를 추가할 수 없다.

Video 입력은 prompt 외에도 duration, ratio, audio, seed와 image asset을 포함하고 결과는 대용량 HTTPS video descriptor와 verified Provider credit usage를 사용한다. 이를 `plugin-sdk/async/v1`의 image schema에 optional field로 누적하면 modality 분리, 결과 bound와 Registry admission digest가 불명확해진다. 따라서 공통 Job 불변 조건은 재사용하되 public video wire와 conformance profile은 분리한다.

## 범위

- Manifest의 backward-compatible async video model/capability 선언
- `plugin-sdk/video/v1` submit, poll, cancel, observation과 signed callback wire
- text-to-video, image-to-video, duration, ratio, audio와 bounded source asset descriptor
- fixed sidecar video endpoints와 existing health/auth contract
- video plugin `jobs.Provider`, immutable channel/ref/admission identity와 restart recovery
- Runway native route의 built-in 또는 plugin candidate 선택
- Gateway-owned source asset authorization과 sidecar-safe input projection
- canonical video result를 Runway task response로 projection
- existing managed video storage/CDN transform과 settlement-before-delivery ordering
- verified provider-credit usage, partial credit Capture와 unknown usage hold
- Plan 065 capability/HMAC/replay callback ingress의 modality-safe 확장
- video conformance runner, stable report/check digest, fixtures와 standalone Go template
- signed Registry `video/v1` runtime/conformance admission profile
- README, CONTRIBUTING, Makefile와 멀티레포 handoff 갱신

## 제외 범위

- 고객이 임의 callback/webhook URL을 지정하는 기능
- arbitrary Provider-native video route, payload 또는 response pass-through
- Replicate/fal video protocol projection과 cross-provider protocol conversion
- image/video mixed manifest execution 또는 한 Job의 post-submit Provider 이전
- 대용량 원본 media bytes를 sidecar JSON이나 Gateway 메모리에 적재하는 방식
- sidecar가 upload signing, object storage credential, DB, Redis, Wallet 또는 tenant identity를 받는 방식
- LLM/audio streaming, SSE, WebSocket과 realtime transport
- gRPC/mTLS, workload identity, process lifecycle와 hot reload
- dynamic Registry publication/signing pipeline의 타 저장소 구현

## 핵심 결정

### 1. Video wire는 image async/v1과 분리한다

- 공개 package는 `plugin-sdk/video/v1`, runtime profile은 `video/v1`이다.
- fixed endpoints는 `POST /plugin/video/v1/submit`, `/poll`, `/cancel`이다.
- Job identity, opaque Provider ref와 callback signature PAE 규칙은 Plan 065와 의미를 맞추되 video request/result 타입을 독립 소유한다.
- 기존 `plugin-sdk/async/v1` schema, check digest와 signed admission은 변경하지 않는다.

### 2. Manifest 확장은 기존 digest를 보존한다

- 기존 image `models` canonical form과 digest는 byte-for-byte 유지한다.
- video model은 additive optional collection과 명시적 `video.generate` contract/capability를 사용한다.
- v1 activation은 protocol `runway`, JSON media type, text-to-video 또는 image-to-video 중 하나 이상, HTTPS video output만 허용한다.
- 한 manifest가 서로 다른 runtime profile을 혼합하거나 model/protocol/channel을 shadowing하면 거부한다.

### 3. Source asset capability는 Gateway가 소유한다

- native Runway upload endpoint와 existing `runway://` asset binding/owner/channel authorization은 Gateway가 계속 검증한다.
- sidecar에는 raw upload bytes, S3 credential 또는 customer URL 대신 bounded Gateway-authorized asset descriptor만 전달한다.
- plugin이 arbitrary source URL을 요청하거나 redirect/fetch 정책을 결정하지 못한다.
- image-to-video Adapter가 요구하는 외부 업로드는 별도 후속 계약 없이는 지원하지 않고 명시적 capability error로 실패한다.

### 4. Video 결과와 비용은 canonical evidence다

- success observation은 non-empty HTTPS video URL, bounded duration/content type와 verified provider-credit micro-unit usage를 반환한다.
- Gateway는 URL origin을 manifest/operator allowlist로 검증하고 existing video collector가 SSRF, redirect, MIME와 size bound를 적용한다.
- plugin schema에는 판매가, 마진, currency, Wallet 또는 Ledger instruction이 없다.
- actual credit가 예약 상한을 넘거나 missing/conflicting하면 Capture/Release하지 않고 manual reconciliation으로 보낸다.

### 5. Native API와 durable Job은 동일하다

- 고객은 기존 Runway SDK/REST 경로와 Gateway Job ID만 본다.
- selected plugin channel은 Job 생성 전에 고정되고 Provider ref는 attempt 내부에만 보존한다.
- submit 시작 이후 timeout은 `RECONCILING`, callback 유실은 polling fallback이며 다른 Adapter로 redispatch하지 않는다.
- poll/callback/cancel은 existing terminal CAS, usage evidence, settlement lease와 Ledger operation key를 재사용한다.

### 6. Callback capability는 operation identity까지 결속한다

- Plan 065의 purpose-separated HMAC keyring, per-Job capability digest, timestamp/delivery replay와 bounded route를 재사용한다.
- callback envelope는 protocol, operation, model, plugin/version/manifest, Provider ref와 video observation을 결속한다.
- image callback handler가 video body를 해석하거나 반대로 처리할 수 없어야 한다.
- callback이 비활성화되거나 실패해도 polling은 계속 동작한다.

## 설계 및 구현 순서

### 1. Manifest와 Registry video profile

- optional video model/capability 구조와 strict parser/canonical digest test를 추가한다.
- text/image input, duration/ratio/audio, output origin과 maximum duration을 bounded 선언한다.
- 기존 sync image와 async image manifest digest golden을 유지한다.
- signed admission이 runtime `video/v1`, video conformance schema/check digest와 exact manifest를 결속하도록 확장한다.

### 2. Public video SDK

- identity, source asset, submit/control, observation, result, verified credit usage, error와 callback 타입을 추가한다.
- strict duplicate/unknown/trailing/oversized JSON과 canonical encode/digest golden을 제공한다.
- status/result/error exclusivity, ref, MIME/URL, duration, progress와 microcredit bound를 검증한다.
- SDK는 Gateway `internal/`, DB, HTTP client, storage client, private key와 Provider payload를 import하지 않는다.

### 3. Video plugin Provider와 route

- existing fixed-origin transport의 proxy/redirect 차단, bearer, timeout, response/body/concurrency bound를 공유한다.
- video submit/poll/cancel response를 existing `jobs.SubmitResult`와 `job.Observation`으로 변환한다.
- video Registry가 built-in Runway와 plugin candidate의 model/capability collision을 거부하고 exact channel을 반환한다.
- Runway handler가 candidate Provider를 허용하고 native body를 bounded canonical video input으로 projection한다.
- already-authorized `runway://` source asset만 plugin request에 포함한다.

### 4. Native result, callback과 managed delivery

- canonical video terminal result를 `/v1/tasks/{gateway_job_id}` Runway response로 변환한다.
- video callback ingress가 signature, capability, modality, Job/channel/ref/admission identity와 replay를 검증한다.
- managed mode는 existing video result manager로 URL을 수집하고 immutable object/evidence를 저장한 뒤 정산한다.
- provider mode는 validated Provider URL을 반환하되 Provider ref/control URL을 노출하지 않는다.
- process restart, duplicate callback/poll과 storage retry가 동일 snapshot/object/charge로 수렴하게 한다.

### 5. Usage와 장애 수렴

- maximum estimated provider credit Reserve와 verified actual microcredit Capture를 검증한다.
- success partial credit는 actual만 Capture/remainder Release하고 failure/cancel known zero는 Release한다.
- submit response loss, malformed result, excessive/missing/conflicting usage와 storage failure는 reservation을 유지한다.
- plugin disable/yank/expired admission 시 신규 submit은 차단하고 existing Job은 persisted channel/ref로 drain 또는 manual reconciliation한다.

### 6. Conformance, template와 운영 문서

- video runner가 health/auth, text submit, image submit, poll processing/success, cancel, callback signature/tamper, malformed/oversize/timeout/cancellation을 검사한다.
- secret-safe deterministic report, valid/invalid fixture corpus와 `-profile video-v1` CLI를 제공한다.
- isolated Go template가 in-memory deterministic Job과 signed callback을 구현하고 유료 Provider 호출을 하지 않는다.
- public module dependency audit, README/config table, contributor policy와 Cloud/Registry/Conformance handoff를 갱신한다.

## 인터페이스와 데이터 변경

### 공개 sidecar API

```text
GET  /plugin/v1/health
POST /plugin/video/v1/submit
POST /plugin/video/v1/poll
POST /plugin/video/v1/cancel
POST /internal/webhooks/plugin-video/{gateway_job_id}/{capability_token}
```

고객용 Runway API는 변경하지 않는다.

```text
POST   /v1/text_to_video
POST   /v1/image_to_video
GET    /v1/tasks/{gateway_job_id}
DELETE /v1/tasks/{gateway_job_id}
```

### 내부 인터페이스

- video plugin client가 existing `jobs.Provider`를 구현한다.
- video Registry는 exact logical model, provider model, channel, capabilities와 immutable admission evidence를 제공한다.
- callback verifier는 raw body를 verified typed video observation으로만 repository에 전달한다.
- existing Job worker와 `videostorage.Manager` interface는 바꾸지 않고 provider/channel dispatch만 확장한다.

### 데이터베이스 및 migration

- existing plugin channel snapshot에 modality/runtime profile을 additive 저장하거나 exact manifest digest를 통해 도출 가능하게 한다.
- webhook binding/delivery의 provider 또는 modality constraint를 video plugin callback까지 확장한다.
- 필요하면 source asset/channel binding과 plugin admission evidence reference를 additive 저장한다.
- 기존 image plugin, Runway task, asset, Job, usage, charge와 Ledger row는 backfill/rewrite하지 않는다.
- migration은 fresh/current upgrade와 구 binary read compatibility를 검증한다.

### 다른 저장소 계약

- `conformance`: 공식 Runway SDK/REST text/image submit, task get/cancel, restart와 managed/provider 결과를 video plugin candidate에서 독립 검증한다.
- `registry`: `video/v1` isolated rerun, artifact/SBOM/provenance 검증과 signed admission publication을 소유한다.
- `cloud`: callback key injection, read-only Registry bundle, sidecar egress/storage isolation, non-terminal backlog와 storage/settlement alert를 소유한다.
- Gateway는 versioned video SDK, fixture, report/check digest, retry status와 native projection을 제공한다.

## 보안 및 과금 고려사항

- public Authorization, tenant/API Key ID, provider credential, price/margin, callback key와 storage credential은 sidecar wire에 포함하지 않는다.
- source/result URL은 customer 또는 manifest가 임의 origin으로 확장할 수 없고 exact operator allowlist와 collector를 통과한다.
- callback capability/token/path, signature, Provider ref, source asset identity와 raw content는 log, trace, metric label, report와 durable error에 기록하지 않는다.
- submit timeout과 response loss는 Provider 생성 여부가 불명확하므로 fallback/Release하지 않는다.
- verified actual microcredit만 정산하며 output URL 존재만으로 success Capture하지 않는다.
- terminal CAS, callback delivery unique key, usage evidence unique key, settlement lease와 Ledger operation key가 duplicate effect를 방지한다.
- Registry admission은 artifact provenance를 증명하지만 runtime/network sandbox를 대체하지 않는다.

## 테스트 계획

### 단위 테스트

- Manifest image digest backward compatibility, video contradiction/collision/capability
- video request/response/callback strict codec와 wire golden
- fixed-origin/auth/redirect/timeout/body/concurrency와 secret-safe error
- Runway native input/result projection, source asset authorization와 URL/MIME/credit validation
- callback HMAC rotation/timestamp/delivery/capability/modality identity
- video report check ordering/digest와 fixture corpus

### 통합 테스트

- plugin candidate text/image submit→persisted attempt→poll/callback terminal→native task result
- cancel, response loss, timeout/backoff, restart recovery와 disabled callback polling
- callback/poll/cancel race와 duplicate delivery exactly-once terminal/usage/Ledger effect
- Reserve→partial credit Capture/remainder Release, cancel Release와 unknown hold
- managed video retry/restart same object/snapshot, immutable channel/Registry/Job/charge evidence
- migration fresh/current upgrade와 existing Runway/image plugin compatibility

### 호환성 및 장애 테스트

- 공식 Runway SDK/REST가 Base URL/Key만 바꿔 plugin-backed text/image task를 submit/get/cancel함
- sidecar 401/429/500, redirect, reset, malformed/oversized response, timeout과 restart
- result origin/MIME/size mismatch, storage unavailable와 callback tamper/replay/key rotation
- Registry disabled/required/yanked/expired/rollback과 existing Job drain
- existing async image plugin, synchronous image plugin, built-in Runway와 전체 billing regression
- isolated video template가 Gateway `internal/` package 없이 build/test/conformance함

### 필수 검증 명령

```text
GOCACHE=/private/tmp/nativegateway-go-cache make check
GOCACHE=/private/tmp/nativegateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/nativegateway-go-cache go test -count=1 -tags=sdkconformance ./protocols/runway
GOCACHE=/private/tmp/nativegateway-go-cache go test -count=1 -tags=sdkconformance ./protocols/replicate ./protocols/fal ./protocols/openai ./protocols/gemini
GOWORK=off GOCACHE=/private/tmp/nativegateway-go-cache go test -race ./plugin-sdk/...
go run ./cmd/gateway-plugin-conformance -profile video-v1 ...
```

## 완료 조건

- [ ] 기존 sync/async image manifest digest와 runtime/conformance profile이 변경되지 않음
- [ ] 외부 Adapter가 public video SDK만으로 text/image submit, poll, cancel과 signed callback을 구현함
- [ ] plugin-backed Runway native API가 Gateway Job ID와 terminal native task 결과로 수렴함
- [ ] source asset과 Provider ref가 tenant/channel/capability 경계를 벗어나지 않음
- [ ] callback/poll/cancel/restart가 post-submit redispatch나 duplicate effect 없이 수렴함
- [ ] partial credit/failure/cancel/unknown usage가 Wallet/Ledger 정책대로 정확히 한 번 정산됨
- [ ] managed video 저장과 immutable manifest/Registry/channel/Job/charge evidence가 보존됨
- [ ] video conformance report, fixtures, CLI profile과 standalone template가 제공됨
- [ ] secret/ref/asset/content가 log·telemetry·report·DB 오류에 노출되지 않음
- [ ] built-in Runway, image plugin과 공식 SDK 회귀가 없음
- [ ] unit/race/integration/SDK/public-module 검사가 통과함
- [ ] README, CONTRIBUTING, examples, Makefile와 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 신규 video manifest/admission publication과 plugin candidate 선택을 중단한다.
- submit 전 요청은 built-in Runway 또는 명시적 unavailable로 처리한다.
- 이미 submit된 plugin video Job은 persisted channel/ref/admission identity로 동일 sidecar에서 drain한다.
- sidecar 복구가 불가능하면 reservation을 임의 Release하지 않고 manual reconciliation에 보존한다.
- callback ingress만 비활성화하고 polling fallback과 managed storage retry를 유지할 수 있다.
- immutable Job, asset, Registry evidence와 Ledger entry는 삭제·rewrite하지 않는다.

## 후속 작업

- streaming LLM plugin runtime and conformance
- streaming audio/realtime plugin runtime and conformance
- gRPC/mTLS sidecar identity and workload-level network policy
- controlled runtime reload, connection drain and health rollout
- callback delivery retention/archival and management audit projection
