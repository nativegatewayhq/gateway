---
id: gateway-20260820-031
title: Phase 3 fal Native Queue
status: in_progress
created_at: 2026-08-20T22:39:33+09:00
updated_at: 2026-08-20T22:39:33+09:00
owners:
  - gateway
initiative: phase-3-fal-native-queue
depends_on:
  - gateway-20260820-007
  - gateway-20260820-011
  - gateway-20260820-018
  - gateway-20260820-019
  - gateway-20260820-023
  - gateway-20260820-028
  - gateway-20260820-029
  - gateway-20260820-030
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 3 fal Native Queue

## 목적

공식 fal JavaScript/Python client 또는 REST client가 서비스 Key와 Gateway endpoint만 사용해 model-scoped Queue 작업을 제출·조회·결과 수신·취소하고, durable Job과 정확한 과금 정산을 유지하면서 fal native JSON을 받게 한다.

## 배경

Plan 029는 protocol-neutral durable Job을, Plan 030은 이를 사용하는 첫 Replicate facade와 worker bootstrap을 제공했다. fal Queue는 단일 `/queue/submit` 경로가 아니라 현재 `queue.fal.run/{model_id}`에 작업을 제출하고 같은 model namespace의 `/requests/{request_id}/status`, `/requests/{request_id}`, `/requests/{request_id}/cancel`을 사용하는 model-scoped API다. 공식 client의 `queue.submit/status/result/cancel`은 model ID와 request ID를 함께 요구하므로 Gateway도 이 wire 구조를 보존해야 한다.

fal 응답은 `IN_QUEUE`, `IN_PROGRESS`, `COMPLETED` 등 Replicate와 다른 상태와 status/result 분리 계약을 가진다. public request ID는 opaque Gateway Job ID로 치환하되, client가 전달하는 model ID와 Job에 저장된 canonical model이 반드시 일치해야 tenant 간 조회나 confused-deputy 문제가 발생하지 않는다.

## 범위

- fal `Key SERVICE_KEY` 및 Bearer 서비스 Key 인증 호환
- `POST /{model_id}` Queue submit
- `GET /{model_id}/requests/{gateway_job_id}/status`
- `GET /{model_id}/requests/{gateway_job_id}` result
- `PUT /{model_id}/requests/{gateway_job_id}/cancel`
- slash를 포함하는 exact fal model ID의 안전한 route parsing과 canonicalization
- Queue submit/status/result/cancel native envelope 및 status mapping
- fal Provider credential/channel resolution과 fixed-origin outbound adapter
- upstream `request_id`, response/result URL과 control identity의 Gateway Job ID 치환
- Plan 029 Job repository/service/worker를 복수 async Provider로 일반화한 runtime bootstrap
- Job별 status snapshot과 result snapshot의 분리 또는 명시적 projection
- Idempotency-Key, Wallet reserve, terminal Capture/Release와 reconciliation
- query `logs`와 native validation, body/timeout/redirect bounds
- fal protocol의 API Key permission, capability, pricing, charge와 telemetry allowlist
- JavaScript/Python SDK wire fixtures, examples와 멀티레포 handoff

## 제외 범위

- synchronous `fal.run`과 realtime/WebSocket/SSE streaming
- file upload, CDN upload, data URI 변환과 arbitrary remote URL fetch
- webhook 제출 및 inbound webhook 처리
- fal Serverless management, deployments, secrets와 model lifecycle API
- client-side browser credential proxy
- image/video/audio 결과의 managed storage 변환
- cross-provider protocol conversion과 submit 이후 fallback
- dynamic runtime/compute-unit pricing과 partial-output billing
- Queue list, priority mutation과 administrator cancellation

## 설계 및 구현 순서

### 1. Model-scoped native route contract

- route parser는 model ID를 `{owner}/{model}` 이상의 bounded slash-separated identifier로 받고 `requests` control suffix를 우측에서 분리한다.
- percent-encoded slash, dot segment, 빈 segment, query 기반 model override와 중복 decoding을 거부한다.
- submit body는 bounded JSON object로 pass-through하고 client webhook 관련 query/body option은 초기 릴리스에서 거부한다.
- status는 native `IN_QUEUE`, `IN_PROGRESS`, `COMPLETED`와 실패/cancel 응답을 보존하며 result endpoint는 완료 전 native conflict/not-ready 응답을 반환한다.
- 공개 `request_id`와 response/control URL은 Gateway Job ID 및 startup public base URL만 사용한다.

### 2. Capability, permission과 pricing

- canonical model key는 protocol `fal`, operation `image.generate`, exact fal model ID의 조합으로 등록한다.
- 초기 Gateway 범위는 image models allowlist만 게시하지만 Job/adapter 자체는 modality-neutral payload를 유지한다.
- candidate는 fal Provider와 configured credential/channel이 모두 존재할 때만 활성화한다.
- charge, quota, spend-cap protocol constraints에 fal을 additive migration으로 추가한다.
- submit 전 exact fixed price를 reserve하고 `COMPLETED` result가 durable하게 저장된 뒤 Capture하며, 확정 failure/cancel은 Release한다.
- Provider submit 이후에는 다른 fal channel 또는 Provider로 fallback하지 않는다.

### 3. fal Provider adapter

- configured `https://queue.fal.run` origin과 validated model/request ID로만 submit/status/result/cancel URL을 만든다.
- inbound credential, cookie, forwarding/tracing header를 제거하고 selected channel credential만 `Authorization: Key ...`로 적용한다.
- redirect와 automatic retry를 끄고 timeout, response body, log list와 error detail 크기를 제한한다.
- upstream response의 `request_id`는 typed internal Provider Job ID로 분리하고 durable public snapshot에는 저장하지 않는다.
- upstream이 제공한 response/status/cancel URL은 fetch target으로 사용하지 않고 fixed origin만 사용한다.
- 4xx known rejection과 408/409/429/5xx/timeout/connection unknown을 분류해 Job reservation 정책에 전달한다.

### 4. Durable status와 result projection

- Plan 029 Job에는 fal status snapshot과 final result snapshot의 역할을 명시적으로 구분할 수 있는 bounded schema를 추가한다.
- submit 응답은 Gateway `request_id`와 Gateway-owned status/response/cancel URL을 즉시 반환한다.
- status GET은 durable status snapshot만 읽고 Provider를 직접 poll하지 않는다.
- result GET은 terminal success의 durable native result만 반환하고, 미완료·실패·취소를 fal native 상태로 변환한다.
- worker가 due Job을 status poll하고 `COMPLETED` 관측 시 result를 정확히 한 번 fetch한 뒤 snapshot과 settlement intent를 원자적으로 적용한다.
- result fetch 성공과 DB commit 사이 crash는 재시도 가능하되 Capture와 result publication은 정확히 한 번 수렴한다.

### 5. Cancel과 동시성

- cancel은 tenant ownership과 path model 일치를 확인한 뒤 Plan 029 cancel-once lease를 획득한다.
- poll, result fetch와 cancel 경합은 terminal CAS 및 attempt fencing token으로 하나의 결과만 채택한다.
- Provider가 취소를 확정한 경우만 `CANCELED`로 Release하고 unknown 응답은 `RECONCILING`에서 reservation을 유지한다.
- repeated SDK polling, duplicate cancel과 동일 Idempotency-Key submit은 Provider 호출과 Ledger 전이를 늘리지 않는다.
- client disconnect는 submitted Job을 자동 취소하지 않는다.

### 6. Runtime과 SDK compatibility

- fal credential, Queue origin, allowed models, public base URL, request/body/log limits를 startup에서 검증한다.
- Replicate와 fal adapter는 하나의 Job worker provider map과 repository를 공유하고 readiness check를 중복 등록하지 않는다.
- 하나의 async Provider만 비활성화해도 다른 Provider worker가 계속 처리되도록 provider availability를 분리한다.
- JavaScript `fal.queue.submit/status/result/cancel`과 Python `fal_client.submit/status/result/cancel`의 실제 wire를 fixture 또는 subprocess로 검증한다.
- 공식 client가 임의 Queue base URL을 직접 지원하지 않는 버전은 documented request middleware/proxy adapter를 제공하고 Conformance가 버전별 경계를 고정한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /{fal_model_id}
GET  /{fal_model_id}/requests/{gateway_job_id}/status
GET  /{fal_model_id}/requests/{gateway_job_id}
PUT  /{fal_model_id}/requests/{gateway_job_id}/cancel
```

Gateway health, OpenAI, Gemini와 Replicate route보다 낮은 우선순위의 별도 fal router에서만 model wildcard를 해석한다. 알 수 없는 top-level 경로를 무조건 fal upstream으로 전달하지 않는다.

### 내부 인터페이스

- Plan 029 `jobs.Provider`를 구현하는 fal adapter를 추가한다.
- status poll과 terminal result fetch가 분리되므로 Provider observation 또는 worker에 `FetchResult` capability를 추가하되 Replicate adapter의 기존 계약을 깨지 않는다.
- public status/result renderer는 raw Provider Job ID가 아닌 Gateway Job 및 sanitized snapshots만 받는다.

### 데이터베이스 및 migration

- API Key permission, image charge와 quota protocol constraints에 `fal`을 추가한다.
- 필요 시 async Job에 bounded status/result snapshot column과 hash를 additive하게 추가한다.
- 기존 Replicate rows는 backfill 없이 동일 worker에서 계속 처리되어야 한다.
- rolling rollback 중 구 binary가 새 fal rows를 claim하지 않도록 protocol/provider claim 조건 또는 route feature flag를 둔다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 `phase-3-fal-native-queue` initiative로 공식 JavaScript/Python client 버전, endpoint override 방식과 submit/status/result/cancel wire matrix를 소유한다.
- Cloud는 fal credential/channel, exact model allowlist와 price, public base URL, worker backlog 및 result-fetch reconciliation alert를 제공한다.
- Gateway는 path grammar, Gateway Job ID, native 상태/error/result projection과 SDK middleware 계약을 versioned handoff로 제공한다.

## 보안 및 과금 고려사항

- wildcard model route를 SSRF용 arbitrary host/path로 사용할 수 없도록 fixed origin, segment validation과 route allowlist를 적용한다.
- service Key와 fal Provider credential을 inbound/outbound 경계에서 분리하고 logs, snapshots, events, metrics에 저장하지 않는다.
- `logs=true` 응답은 개수·문자열 크기를 제한하며 prompt, input, output과 raw error는 telemetry에 기록하지 않는다.
- client webhook과 arbitrary URL fetch를 거부한다.
- submit 전 Reserve, durable result 성공 후 Capture, known fail/cancel Release, unknown reservation 유지 순서를 보존한다.
- status/result GET은 Provider dispatch, reserve 또는 settlement를 수행하지 않는다.
- model path와 stored Job model 불일치는 tenant가 같더라도 not-found로 처리한다.

## 테스트 계획

### 단위 테스트

- model-scoped path parsing, encoding, collision과 traversal rejection
- fal 상태↔Gateway Job 상태 mapping과 status/result renderer
- request ID 및 URL rewrite와 Provider identity 제거
- outbound header/query sanitization, fixed origin과 redirect rejection
- error category, body/log bounds와 webhook rejection

### 통합 테스트

- fal permission, registry, price, charge와 migration fresh/current upgrade
- submit idempotency와 concurrent Provider dispatch 1회
- submit→status worker→result fetch→GET result→Capture
- failed/canceled→Release와 timeout/connection/result-fetch loss→reservation 유지
- status/result GET 폭주에서 upstream 호출 증가 없음
- cancel/poll/result terminal race, duplicate cancel과 stale lease recovery
- Replicate와 fal Job을 동일 worker가 공정하게 처리하고 서로 격리함

### 호환성 및 장애 테스트

- 공식 fal JavaScript/Python client endpoint/credential 교체 submit/status/result/cancel
- upstream 400/401/404/409/422/429/500, malformed/oversized JSON과 redirect
- submit 응답 유실, status 완료 후 result 지연, result 응답 유실과 DB conflict
- malicious model/request ID, Host header, webhook, log와 credential injection
- 기존 OpenAI/Gemini/Replicate wire, billing, storage와 telemetry regression

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

- [ ] fal submit/status/result/cancel native routes와 error envelope가 구현됨
- [ ] service Key와 configured endpoint만으로 공식 JavaScript/Python client 계약이 검증됨
- [ ] exact model path, permission, price, channel과 credential만 submit됨
- [ ] Gateway Job ID만 public response와 URL에 노출됨
- [ ] status/result GET은 durable snapshot만 읽고 Provider나 Billing을 호출하지 않음
- [ ] `COMPLETED` result 저장과 Capture가 정확히 한 번 수렴함
- [ ] known failed/canceled Release와 unknown reservation 유지가 검증됨
- [ ] cancel/poll/result race와 재시작 stale lease recovery가 수렴함
- [ ] Replicate와 fal이 동일 runtime worker에서 안전하게 공존함
- [ ] wildcard route, redirect, raw error, credential과 payload 보안 경계가 검증됨
- [ ] Provider submit 이후 fallback이 없음
- [ ] 기존 프로토콜과 전체 race/integration 테스트가 회귀하지 않음
- [ ] README, SDK 예제와 Cloud/Conformance handoff가 갱신됨
- [ ] commit, PR과 최종 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- fal route publication과 fal provider entry만 비활성화하고 Replicate Job worker는 계속 실행한다.
- 이미 제출된 fal Job은 drain하거나 `RECONCILING`/manual 상태로 보존하고 다른 Provider로 재제출하지 않는다.
- additive schema와 fal protocol rows는 rolling rollback 호환을 위해 유지한다.
- reservation은 Provider terminal 상태가 확인되지 않으면 임의 Release하지 않는다.

## 후속 작업

- signed fal webhook ingress와 replay protection
- fal file/CDN upload 및 authenticated large input delivery
- async image/video/audio result managed storage
- SSE/realtime streaming과 synchronous `fal.run`
- model별 dynamic compute-unit/runtime pricing
- async Job retention, archival과 관리자 UI
