---
id: gateway-20260820-029
title: Phase 3 Durable Asynchronous Job Foundation
status: in_progress
created_at: 2026-08-20T21:45:40+09:00
updated_at: 2026-08-20T21:45:40+09:00
owners:
  - gateway
initiative: phase-3-async-job-foundation
depends_on:
  - gateway-20260820-008
  - gateway-20260820-011
  - gateway-20260820-012
  - gateway-20260820-013
  - gateway-20260820-016
  - gateway-20260820-023
  - gateway-20260820-028
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 3 Durable Asynchronous Job Foundation

## 목적

Replicate와 fal 같은 장시간 Provider 작업을 안전하게 수용할 수 있도록 Gateway Job ID, Provider Job ID, 상태 전이, durable lease, polling/cancel/webhook 수렴과 정확히 한 번의 과금 정산을 제공하는 프로토콜 독립 비동기 Job 코어를 구축한다.

## 배경

현재 Gateway 요청은 동기 Provider 호출과 timeout reconciliation을 전제로 한다. 비동기 Provider는 submit 성공과 결과 완료가 분리되고, client polling·worker polling·webhook·재시작이 같은 작업을 동시에 갱신할 수 있다. Provider Job ID를 Gateway 공개 ID로 노출하거나 각 Protocol handler가 상태와 정산을 직접 구현하면 tenant 경계, 중복 정산, 취소 경합과 복구 규칙이 Provider마다 달라진다. 따라서 Replicate/fal native facade보다 먼저 공통 durable Job aggregate와 원자적 상태 전이 계약을 고정한다.

## 범위

- tenant 소유 Gateway Job과 Provider attempt의 PostgreSQL 영속화
- `PENDING`, `QUEUED`, `PROCESSING`, `SUCCEEDED`, `FAILED`, `CANCELED`, `RECONCILING` 공통 상태
- Gateway Job ID와 Provider Job ID의 분리 및 opaque public identifier
- submit 결과 기록, polling claim lease, heartbeat, retry backoff와 stale lease recovery
- terminal state compare-and-set 및 append-only Job event history
- client poll/cancel용 protocol-neutral application service
- Provider poll과 webhook 결과를 하나의 idempotent transition 함수로 수렴
- Billing charge와 Job의 결합, 성공 Capture·확정 실패/취소 Release·불명확 결과 Reconciliation
- native 결과 snapshot의 bounded durable 저장과 terminal replay
- worker readiness, bounded structured logs와 OpenTelemetry Job metrics
- PostgreSQL integration 및 동시성/재시작 테스트

## 제외 범위

- Replicate `/v1/predictions` HTTP facade와 공식 SDK 호환
- fal queue submit/status/result HTTP facade와 공식 SDK 호환
- 실제 Replicate/fal Provider adapter, webhook signature 규격과 외부 callback endpoint
- image/video/audio별 payload 변환 또는 cross-provider 변환
- Provider submit 이후 다른 Provider로 fallback, retry 또는 hedging
- 사용자 callback URL 전달과 outbound webhook delivery
- Job 목록·검색, 관리자 UI, retention/archival과 삭제 API
- 동시 Job quota와 priority queue scheduling
- PostgreSQL 외 Redis Streams/Kafka queue

## 설계 및 구현 순서

### 1. Job aggregate와 상태 전이

- `operations/job`에 typed Job, ProviderAttempt, Status, ResultSnapshot과 transition command를 정의한다.
- 상태 전이는 명시적 allowlist로 검증하고 terminal 상태는 불변으로 유지한다.
- `SUCCEEDED`, `FAILED`, `CANCELED` 전이는 같은 terminal 결과에 대해 멱등이며 다른 terminal 결과와 충돌한다.
- client-visible Job ID는 `job_` prefix의 고엔트로피 opaque 값이고 Provider ID는 내부 attempt에만 저장한다.

### 2. PostgreSQL schema와 repository

- additive migration으로 `jobs`, `job_provider_attempts`, `job_events`와 필요한 unique/check/index를 추가한다.
- organization/project/API Key 소유권, protocol, operation, logical model, selected Provider/channel, charge ID를 immutable snapshot으로 저장한다.
- Provider Job ID unique scope는 Provider와 channel을 포함하며 빈 submit 결과를 허용하지 않는다.
- 모든 상태 변경은 row lock/CAS와 append-only event를 한 transaction에서 기록한다.
- result snapshot은 status/content type/allowlisted headers/body 크기를 제한하고 secret-bearing headers를 거부한다.

### 3. Submit과 과금 결합

- API layer가 기존 인증·권한·라우팅·가격·Wallet `Begin`을 완료한 뒤 Job을 생성하고 Provider submit은 최대 한 번 수행한다.
- submit 전 실패는 기존 charge를 Release하고 Job을 확정 실패로 닫는다.
- submit 응답이 Provider Job ID를 확정하면 attempt를 atomically bind하고 `QUEUED` 또는 `PROCESSING`으로 전이한다.
- submit timeout/connection loss처럼 Provider 생성 여부가 불명확하면 다른 Provider를 호출하지 않고 `RECONCILING`으로 전이해 예약을 유지한다.
- client idempotency key 재요청은 동일 Job을 반환하며 Provider submit과 Wallet reserve를 반복하지 않는다.

### 4. Polling worker와 lease

- PostgreSQL `FOR UPDATE SKIP LOCKED` 기반 claim으로 due Job을 bounded batch 처리한다.
- lease owner/expiry, attempt count, next attempt와 exponential backoff를 기록하고 재시작 후 만료 lease를 재회수한다.
- Provider poll은 DB transaction 밖에서 실행하며 결과 적용은 expected version/lease token으로 보호한다.
- poll timeout은 실패 확정이 아니라 retry/reconciliation이며 최대 시도 초과는 자동 Release 대신 manual reconciliation 상태를 유지한다.
- worker/Provider telemetry 실패는 Job lease와 과금 상태를 바꾸지 않는다.

### 5. Cancel과 외부 결과 수렴

- cancel 요청은 소유권을 확인하고 non-terminal Job에 대해 Provider cancel을 한 번 시도한다.
- Provider가 취소를 확정한 경우만 `CANCELED`와 Release를 적용한다. 이미 성공했거나 결과가 불명확하면 polling/reconciliation을 계속한다.
- polling과 향후 signed webhook은 동일한 provider observation command를 사용한다.
- 중복·역순 observation은 no-op 또는 conflict로 처리하며 Capture/Release는 정확히 한 번만 수행한다.

### 6. Application service와 운영 계약

- Create/Get/Cancel/ApplyObservation/ClaimDue 인터페이스를 Protocol facade가 사용할 수 있게 제공한다.
- public view는 Provider credential, internal channel/attempt/lease, 원가·마진과 raw Provider error를 제외한다.
- readiness는 required PostgreSQL Job repository 상태를 반영하되 개별 Provider 지연은 readiness를 내리지 않는다.
- README에 상태 의미, 정산 규칙, recovery와 후속 Protocol adapter 계약을 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

이 계획에서는 새 HTTP endpoint를 제공하지 않는다. 후속 Replicate/fal facade가 사용할 protocol-neutral public Job view만 내부에 정의한다. Gateway Job ID는 향후 native response의 identifier로 매핑할 수 있지만 Provider Job ID는 외부에 노출하지 않는다.

### 내부 인터페이스

```go
type Service interface {
    Create(context.Context, CreateRequest) (Job, error)
    Get(context.Context, Owner, JobID) (Job, error)
    Cancel(context.Context, CancelRequest) (Job, error)
    ApplyObservation(context.Context, Observation) (Job, error)
}

type AsyncProvider interface {
    Submit(context.Context, SubmitRequest) (ProviderJob, error)
    Poll(context.Context, ProviderJob) (Observation, error)
    Cancel(context.Context, ProviderJob) (Observation, error)
}
```

Provider transport 오류는 typed `known failure`, `unknown outcome`, `canceled caller` 범주로만 domain에 전달한다. raw error text와 response body는 durable event나 client view에 저장하지 않는다.

### 데이터베이스 및 migration

- 다음 additive migration 번호로 Job aggregate, Provider attempt와 append-only event table을 생성한다.
- 기존 charge를 nullable foreign key로 참조해 billing-disabled BYOK와 billing-required managed mode를 모두 지원한다.
- status/version/check constraint와 partial indexes로 due/non-terminal claim을 제한한다.
- migration은 기존 동기 endpoint에 영향을 주지 않는다. rollback은 새 worker와 사용 경로를 먼저 비활성화한 후 아직 외부 API가 없는 동안 새 table을 제거하는 별도 down migration으로 수행한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 같은 `phase-3-async-job-foundation` initiative 이후 Replicate/fal native facade별 공식 SDK polling/cancel/restart 시나리오를 소유한다.
- Cloud는 PostgreSQL migration 배포, worker replica/lease 설정, backlog·stale lease·manual reconciliation alert를 소유한다.
- 후속 Protocol 계획은 이 Job service의 상태/소유권/정산 계약을 사용하며 별도 Job table을 만들지 않는다.

## 보안 및 과금 고려사항

- 모든 Get/Cancel은 organization/project/API Key 소유권으로 조회하며 Job ID 존재 여부를 tenant 간에 구분해 노출하지 않는다.
- Provider Job ID, raw status URL, credential, webhook secret, prompt/input/result 원문과 raw error를 로그·event·telemetry에 남기지 않는다.
- Provider가 반환한 polling URL을 임의 fetch하지 않고 adapter가 고정한 origin/path에서 Provider Job ID만 사용한다.
- result snapshot은 크기와 content type/header allowlist를 적용하고 향후 managed storage가 대형 결과를 소유하게 한다.
- Wallet 예약은 submit부터 terminal observation까지 유지한다. 성공만 Capture하고 확정 실패/취소만 Release하며 timeout·unknown은 Release하지 않는다.
- Job terminal CAS와 Billing Complete를 재시도 가능한 하나의 application transition으로 묶어 crash 경계에서 reconciliation이 수렴하게 한다.
- 중복 polling/webhook/cancel과 worker lease 경합은 Provider 호출 횟수와 Ledger transition을 증가시키지 않아야 한다.
- Provider submit 이후에는 다른 candidate로 fallback하지 않는다.

## 테스트 계획

### 단위 테스트

- 모든 허용/금지 상태 전이와 terminal immutability
- Job/Provider ID, owner, snapshot/header/body validation
- known failure/unknown outcome/cancel observation mapping
- exponential backoff와 lease expiry 계산
- public view와 log/telemetry redaction

### 통합 테스트

- migration up/down과 schema/check/unique constraint
- concurrent Create idempotency가 Job/charge/submit intent 하나만 생성
- 다중 worker `SKIP LOCKED` claim과 stale lease recovery
- poll/webhook/cancel의 중복·역순 경합에서 terminal event와 Capture/Release 한 번
- submit/poll timeout에서 reservation 유지와 reconciliation 재개
- process restart 후 queued/processing/reconciling Job 복구
- tenant 간 Get/Cancel 격리와 Provider Job ID 비노출

### 호환성 및 장애 테스트

- PostgreSQL transaction conflict, connection loss와 worker panic
- Provider submit success response 유실, poll 429/500/timeout, cancel timeout
- terminal result 저장 직전/직후 process crash
- telemetry disabled/exporter failure parity
- 기존 OpenAI/Gemini 동기 SDK 및 과금/replay 회귀 없음

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

- [ ] durable Job/attempt/event schema와 typed 상태 전이가 구현됨
- [ ] Gateway Job ID와 Provider Job ID가 저장·공개 계약에서 분리됨
- [ ] tenant 소유 Get/Cancel이 존재 정보와 내부 metadata를 누출하지 않음
- [ ] submit은 논리 Job당 최대 한 번이며 idempotent Create가 중복 reserve/dispatch를 만들지 않음
- [ ] worker lease와 stale recovery가 다중 instance 및 재시작에서 안전함
- [ ] polling/cancel/향후 webhook observation이 하나의 idempotent terminal transition으로 수렴함
- [ ] 성공 Capture, 확정 실패/취소 Release, timeout/unknown reservation 유지가 검증됨
- [ ] terminal 경합과 crash recovery에서도 Ledger 정산이 정확히 한 번 수행됨
- [ ] Provider submit 이후 fallback이 발생하지 않음
- [ ] result snapshot과 logs/events/telemetry가 secret·tenant·raw Provider 데이터를 제한함
- [ ] 기존 동기 OpenAI/Gemini wire, billing, replay와 readiness 동작이 회귀하지 않음
- [ ] README와 멀티레포 handoff가 갱신됨
- [ ] 전체 race/integration/CI 통과
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 후속 native facade가 활성화되기 전에는 Job worker를 비활성화하고 이전 binary로 rollback한다.
- additive table은 기존 동기 요청이 참조하지 않으므로 안전하게 유지할 수 있다.
- 외부 Job이 존재하는 운영 환경에서는 table을 제거하지 않고 worker를 drain한 뒤 모든 non-terminal Job을 reconciliation/manual 상태로 보존한다.
- rollback 중에도 reservation을 임의 Release하거나 Provider submit을 재시도하지 않는다.

## 후속 작업

- Replicate native Prediction protocol과 adapter
- fal native Queue protocol과 adapter
- signed Provider webhook ingress와 replay protection
- async result managed storage 및 video/audio payload
- concurrent Job quota, retention/archival과 관리자 UI
