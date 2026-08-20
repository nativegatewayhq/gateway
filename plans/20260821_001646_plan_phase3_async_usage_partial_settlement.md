---
id: gateway-20260821-034
title: Phase 3 Async Usage and Partial-output Settlement
status: in_progress
created_at: 2026-08-21T00:16:46+09:00
updated_at: 2026-08-21T00:16:46+09:00
owners:
  - gateway
initiative: phase-3-async-usage-partial-settlement
depends_on:
  - gateway-20260820-009
  - gateway-20260820-010
  - gateway-20260820-011
  - gateway-20260820-013
  - gateway-20260820-029
  - gateway-20260820-030
  - gateway-20260820-031
  - gateway-20260820-032
  - gateway-20260820-033
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 3 Async Usage and Partial-output Settlement

## 목적

Replicate와 fal 비동기 요청이 예약한 최대 비용과 Provider가 실제 완료한 billable output 수량을 분리하고, 부분 결과·중복 callback·polling race에서도 실제 확정 수량만 정확히 한 번 Capture한 뒤 차액을 Release한다.

## 배경

Plan 029~033은 durable Job, Provider submit/poll/cancel, signed webhook과 terminal settlement를 구현했다. 현재 가격과 정산은 요청당 고정 quantity `1`을 전제로 하므로 여러 이미지를 요청했지만 일부만 생성된 경우 최대 예약액과 실제 판매액을 구분할 수 없다. Provider가 성공 terminal과 사용 가능한 결과를 반환해도 요청 수량보다 결과가 적으면 전체 성공 가격을 Capture하거나 전체 실패로 Release하는 두 선택지만 남는다.

Phase 3의 원래 완료 범위에는 작업별 비용 예약, 부분 성공 및 취소 정산이 포함된다. 취소와 known failure의 zero-usage Release는 기존 exact-once 경로에서 완료됐으므로, 이번 계획은 검증 가능한 output-count 사용량을 첫 실제 사용량 dimension으로 추가한다. Provider별 runtime·compute-unit·영상 duration처럼 계약이 다른 측정치는 같은 확장 계약을 사용하되 별도 가격 계획 전에는 활성화하지 않는다.

## 범위

- 요청에서 maximum billable output quantity를 추출하고 그 수량으로 Reserve
- Provider terminal native result에서 실제 usable output quantity 추출
- 요청 수량보다 적은 성공 결과의 partial-output Capture와 예약 차액 Release
- Replicate와 fal image output의 Provider별 typed usage extractor
- 추정 usage, 실제 usage와 추출 근거를 immutable charge/Job usage record로 보존
- price version과 dimension을 terminal 정산까지 고정
- webhook, polling, result fetch와 cancel race의 usage-aware exactly-once settlement
- invalid, absent, excessive 또는 모순된 usage의 fail-closed reconciliation
- native public response를 변경하지 않는 내부 정산 계약
- usage/settlement telemetry와 운영 문서

## 제외 범위

- Provider runtime seconds, GPU seconds, compute units 또는 token 기반 가격
- video duration, audio seconds, file bytes와 storage egress 과금
- Provider invoice import와 월말 원가 reconciliation
- 실패·취소 작업의 Provider-side sunk cost를 고객에게 청구하는 정책
- client가 주장한 output count를 검증 없이 actual usage로 사용하는 기능
- 부분 결과를 새로운 public Job status로 노출하는 변경
- 가격 변경 후 기존 Job을 새 가격으로 재평가하는 기능
- 관리자 UI, 환불 승인 UI와 manual Adjustment
- cross-provider post-submit fallback 또는 결과 병합

## 설계 및 구현 순서

### 1. Usage와 가격 계약 고정

- 내부 `UsageEstimate`와 `UsageActual`을 operation, dimension, unit, quantity와 provenance를 갖는 typed value로 정의한다.
- 첫 dimension은 `output`/`image`이며 quantity는 positive integer, configured maximum 이하만 허용한다.
- submit 시 native body에서 requested image count를 protocol adapter가 추출한다. 필드가 없으면 모델 capability의 default `1`을 사용한다.
- Pricing estimate가 선택한 price ID/version, unit sale/cost와 maximum quantity를 charge 및 Job에 고정해 terminal 시점 가격 변경이 기존 예약을 바꾸지 않게 한다.
- unsupported quantity나 maximum 초과는 Provider dispatch와 Reserve 전에 native invalid-request 오류로 거부한다.

### 2. Immutable usage evidence

- Job별 estimate와 terminal actual usage를 append-only event 또는 전용 immutable row로 기록한다.
- actual usage record는 Job, charge, Provider attempt, terminal observation hash, source(`poll|webhook|cancel`)와 extractor version을 연결한다.
- 같은 terminal snapshot hash와 usage는 semantic replay로 처리하고, 이미 확정된 usage를 다른 값으로 덮어쓰지 않는다.
- raw Provider payload, prompt, image URL, credential과 webhook identity는 usage row에 저장하지 않는다.
- observation과 usage intent는 동일 transaction에서 terminal CAS와 함께 commit한다.

### 3. Provider별 actual usage 추출

- Replicate adapter는 성공 output의 documented image collection shape에서 usable output만 세되 문자열 URL 또는 명시적으로 지원한 객체 shape만 인정한다.
- fal adapter는 성공 payload의 image/images collection에서 usable result 수를 세고 control URL이나 malformed item은 제외한다.
- extractor는 native snapshot sanitizer 이전 raw signed/fetched body를 입력으로 받지만 결과는 bounded typed usage만 반환한다.
- actual quantity는 `0 <= actual <= reserved maximum`이어야 한다. actual이 maximum을 넘거나 payload shape가 모순되면 terminal response는 보존하되 settlement를 `RECONCILING`에 남기고 자동 Capture/Release하지 않는다.
- 성공 status인데 usage를 신뢰할 수 없으면 기존 고정 `1`로 추측하지 않고 명시적 `usage_unknown` reconciliation reason을 기록한다.

### 4. 부분 결과 정산

- submit은 maximum quantity × 고정 price unit으로 Reserve한다.
- 성공 terminal에서 `actual > 0`이면 actual quantity에 해당하는 원가·판매가만 Capture하고 나머지 reservation을 같은 Billing transaction에서 Release한다.
- 성공 terminal에서 verified actual `0`은 policy error로 자동 Release하지 않고 reconciliation에 남긴다.
- known failure/cancel이며 usable output이 없으면 기존처럼 전체 Release한다.
- failure/cancel payload에 usable output이 섞여 있으면 자동으로 고객에게 청구하지 않고 `partial_terminal_conflict`로 보존해 후속 정책이 결정할 때까지 reservation을 유지한다.
- Ledger operation key는 charge와 terminal usage version에 고정해 worker restart와 duplicate delivery가 Capture 또는 Release entry를 늘리지 않게 한다.

### 5. Race와 reconciliation

- webhook과 polling이 동일 result hash/usage를 보고하면 하나만 actual usage와 settlement intent를 생성한다.
- 서로 다른 terminal payload 또는 usage가 경합하면 최초 유효 terminal CAS를 유지하고 후속 관측을 conflict audit event로 남긴다.
- cancel과 success가 경합하면 Provider-confirmed usable success가 먼저 commit된 경우 usage Capture, cancel이 먼저 terminal commit된 경우 기존 Release 정책을 따른다. stale lease는 정산할 수 없다.
- crash가 usage commit 후 Billing 전, Billing commit 후 Job settled mark 전에 발생하는 두 경계를 실제 PostgreSQL에서 재현한다.
- unknown usage Job은 backoff 후 Provider result를 다시 조회하되 maximum attempts 이후에도 reservation을 자동 해제하지 않고 manual reconciliation 상태로 둔다.

### 6. Configuration, telemetry와 rollout

- usage-aware settlement는 provider/model capability로 opt-in하며 기존 fixed-price Job에는 legacy quantity `1` 정책을 유지한다.
- capability에 maximum output count, request extractor version과 result extractor version을 선언한다.
- metric은 provider/channel/model의 bounded ID, estimated/actual quantity, settlement outcome과 reconciliation reason만 기록한다.
- raw result, URL, Job/Provider ID, organization/project/key와 금액을 고 cardinality label로 기록하지 않는다.
- README에 maximum reservation, partial Capture, unknown usage hold와 rollback 절차를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 Replicate Predictions와 fal Queue wire/response는 변경하지 않는다. 지원 모델의 native output-count 입력을 그대로 받으며 Gateway-specific usage 필드를 client response에 추가하지 않는다.

### 내부 인터페이스

- protocol submit parser가 `UsageEstimate`를 생성한다.
- async Provider terminal observation이 optional verified `UsageActual`을 포함한다.
- Billing settlement가 reserved estimate와 actual usage를 받아 partial Capture/Release를 한 transaction으로 수행한다.
- Capability Registry가 model별 billable dimension, default/max quantity와 extractor version을 제공한다.

### 데이터베이스 및 migration

- charge 또는 async Job에 immutable estimated quantity, price reference와 dimension을 추가한다.
- terminal actual usage/evidence는 append-only table로 저장하고 Job/charge당 active terminal usage unique constraint를 둔다.
- 기존 row는 legacy quantity `1`로 해석하며 backfill이나 기존 Ledger row 수정은 하지 않는다.
- migration은 additive하고 구 binary가 새 column/table을 무시할 수 있어야 한다. rollback 시 capability opt-in만 끄고 usage evidence와 Ledger는 감사용으로 보존한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 `phase-3-async-usage-partial-settlement` initiative에서 Replicate/fal 공식 SDK의 multi-image input과 partial/malformed result fixture를 검증한다.
- Cloud는 capability/price rollout, unknown-usage reservation alert, reconciliation queue와 partial margin dashboard를 소유한다.
- Gateway는 usage dimension 이름, quantity bounds, reconciliation reason과 Ledger 결과를 versioned handoff로 제공한다.

## 보안 및 과금 고려사항

- client request quantity는 body bound, integer 형식, model maximum을 모두 통과해야 하며 Provider payload가 actual usage의 유일한 근거다.
- result extractor는 URL을 fetch하지 않고 JSON shape만 검사하므로 SSRF 경계를 넓히지 않는다.
- estimate와 actual의 원 단위 정수 연산만 사용하며 overflow, 음수, decimal truncation과 currency 혼합을 거부한다.
- actual은 reserved maximum을 초과해 Capture할 수 없고 부족 잔액을 terminal 시점에 새로 인출하지 않는다.
- unknown/conflicting usage는 임의 Capture나 Release 대신 reservation을 유지한다.
- usage evidence, terminal CAS, settlement lease와 Ledger operation key가 중복 callback/poll/crash의 이중 과금을 방어한다.
- public native snapshot과 로그에는 내부 price ID, 원가, 마진 또는 tenant identity를 추가하지 않는다.

## 테스트 계획

### 단위 테스트

- Replicate/fal requested output count의 default, maximum, invalid 형식과 overflow
- Provider별 success result shape의 0/1/N output, malformed item과 duplicate URL 처리
- estimate/actual validation, integer multiplication과 maximum Capture bound
- partial Capture 및 remainder Release 계산
- unknown/conflicting usage reconciliation reason과 secret-safe telemetry

### 통합 테스트

- 실제 PostgreSQL에서 maximum Reserve → N보다 적은 actual → partial Capture와 remainder Release
- full success, partial success, known failure/cancel과 unknown usage의 Wallet/Ledger 불변 조건
- webhook/poll/cancel race와 duplicate terminal usage의 exactly-once event/entry
- usage commit 전후 및 Billing commit 직후 crash recovery
- price publication 변경 중에도 submit 시 고정된 price reference 사용
- fresh/current migration과 legacy Job quantity `1` 호환

### 호환성 및 장애 테스트

- official Replicate Python/JavaScript와 fal Python/JavaScript multi-output request wire
- missing/malformed/excessive output, response loss, callback retry와 out-of-order terminal result
- Provider 429/500/timeout 동안 reservation 유지 및 polling fallback
- 기존 single-output Replicate/fal, signed webhook, OpenAI/Gemini와 전체 billing regression

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

- [ ] supported multi-output request가 maximum quantity 기준으로 Reserve됨
- [ ] Replicate/fal terminal result에서 verified actual output quantity가 추출됨
- [ ] partial success가 actual quantity만 Capture하고 예약 차액을 Release함
- [ ] full success와 known zero-output failure/cancel의 기존 정산이 유지됨
- [ ] unknown/excessive/conflicting usage가 자동 Capture/Release되지 않음
- [ ] duplicate webhook/poll과 worker crash가 usage 또는 Ledger entry를 늘리지 않음
- [ ] price 변경이 진행 중인 Job의 고정 단가와 quantity를 바꾸지 않음
- [ ] legacy quantity `1` Job과 기존 native SDK wire가 회귀하지 않음
- [ ] usage evidence와 telemetry에 payload/URL/credential/tenant secret이 노출되지 않음
- [ ] migration fresh/current, race와 전체 integration test가 통과함
- [ ] README와 Cloud/Conformance handoff가 갱신됨
- [ ] commit, PR과 최종 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- model capability의 usage-aware opt-in을 끄고 신규 요청을 legacy single-quantity 또는 비활성 channel로 제한한다.
- 이미 estimate가 고정된 Job은 해당 binary/worker로 drain하며 새 가격으로 재평가하지 않는다.
- unknown/conflicting usage reservation은 임의 Release하지 않고 reconciliation queue에 보존한다.
- additive usage schema와 이미 기록된 Ledger entry는 삭제하거나 역분개하지 않고 감사 데이터로 유지한다.

## 후속 작업

- runtime/compute-unit, video duration, audio seconds와 storage egress pricing
- Provider invoice import 및 원가 차이 reconciliation
- failure/cancel partial-output 고객 청구 정책
- async Job 검색·감사 API, retention/archival과 관리자 UI
- manual reconciliation, Refund와 Adjustment 운영 workflow
