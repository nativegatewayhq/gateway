---
id: gateway-20260820-013
title: Phase 1 Timeout and Provider Reconciliation Worker
status: completed
created_at: 2026-08-20T15:57:24+09:00
updated_at: 2026-08-20T16:11:11+09:00
owners:
  - gateway
initiative: phase-1-timeout-reconciliation
depends_on:
  - gateway-20260820-011
  - gateway-20260820-012
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Timeout and Provider Reconciliation Worker

## 목적

Provider 호출 이후 결과 body 유실, settlement transaction 오류, timeout과 panic으로 `RECONCILING`에 남은 이미지 charge를 durable queue로 관리한다. 관측된 Provider 성공·실패는 Wallet Capture/Release와 안전한 terminal response로 자동 확정하고, 결과가 불명확한 요청은 자동 환불하거나 이중 청구하지 않은 채 제한된 재시도 후 수동 검토 상태로 전환한다.

## 배경

Billable lifecycle과 response replay는 정상 경로에서 원자적이다. 그러나 Provider가 이미 작업을 수행한 뒤 response read가 실패하거나 DB commit 결과가 불명확하면 즉시 Release할 수 없다. 특히 synchronous OpenAI/xAI timeout은 Provider-side lookup ID가 없어 성공 여부를 추론할 수 없으므로, 알려진 outcome과 unknown outcome을 분리하고 재시작 가능한 reconciliation 상태가 필요하다.

## 범위

- charge별 durable reconciliation record
- `KNOWN_SUCCESS`, `KNOWN_FAILURE`, `UNKNOWN` Provider outcome
- response read failure, response size 초과, settlement failure, timeout, panic reason 분류
- 재시도할 terminal response snapshot 저장
- PostgreSQL `FOR UPDATE SKIP LOCKED` worker claim과 lease
- 지수 backoff, attempt 상한과 `MANUAL_REVIEW`
- known success의 Capture, known failure의 Release 자동 정산
- settlement commit-unknown의 동일 operation/snapshot 재확인
- worker crash/restart와 중복 worker 멱등성
- billing required process의 background worker lifecycle
- timeout을 즉시 Release하지 않고 UNKNOWN reconciliation으로 변경
- 구조화된 secret-free reconciliation 로그
- pending/manual count를 readiness와 분리한 운영 query

## 제외 범위

- synchronous OpenAI/xAI 요청의 Provider-side status API: 현재 계약에 없음
- 운영자 수동 resolve HTTP API와 Dashboard
- Replicate/fal asynchronous job reconciliation
- cross-provider retry 또는 fallback
- webhook
- OpenTelemetry metric export: 후속 observability 계획
- 분산 scheduler와 Redis queue
- 자동 chargeback, payment processor와 회계 adjustment

## 설계 및 구현 순서

### 1. Outcome 분류

- Provider HTTP status를 받았으면 2xx는 `KNOWN_SUCCESS`, non-2xx는 `KNOWN_FAILURE`다.
- executor timeout, connection loss와 panic은 `UNKNOWN`이다.
- Provider response body 상한 초과/read failure는 status를 알고 있으므로 known outcome이다.
- settlement method가 오류를 반환하면 원래 observed outcome과 snapshot을 보존해 같은 Complete를 재시도한다.

### 2. Schema

- `image_charge_reconciliations`: charge ID PK, outcome, reason, intended terminal snapshot, attempt count, state, next attempt, lease owner/until, last error category, timestamps.
- 상태는 `PENDING`, `LEASED`, `MANUAL_REVIEW`, `RESOLVED`다.
- snapshot은 response replay와 같은 header allowlist/body hash/크기 규칙을 사용한다.
- charge가 RECONCILING일 때만 신규 PENDING record를 만들 수 있다.
- RESOLVED row와 observation identity/snapshot은 append-only다.

### 3. MarkReconciling

- handler는 charge ID만 전달하지 않고 outcome, reason과 가능한 response snapshot을 전달한다.
- charge RESERVED→RECONCILING과 reconciliation upsert를 같은 transaction에서 수행한다.
- 같은 observation retry는 같은 row를 반환하고 다른 outcome/snapshot은 conflict다.
- DB 전체 장애로 기록 자체가 실패하면 request ID와 reason category만 오류 로그에 남긴다.

### 4. Claim과 lease

- worker는 due PENDING row를 `FOR UPDATE SKIP LOCKED`로 batch claim한다.
- claim 시 state LEASED, owner와 lease_until을 transaction에서 기록한다.
- 만료된 LEASED row는 다른 worker가 다시 claim할 수 있다.
- batch, interval과 lease duration은 제한된 configuration으로 제공한다.

### 5. Known outcome resolve

- known outcome은 저장 observation으로 Billing Complete를 다시 실행한다.
- Wallet operation key와 response snapshot immutability로 commit-unknown retry가 단일 effect를 만든다.
- 성공하면 reconciliation RESOLVED로 전환하고 charge는 CAPTURED/RELEASED terminal이다.
- Complete 성공 후 worker crash 시 다음 claim이 terminal snapshot을 확인하고 RESOLVED만 마무리한다.

### 6. Unknown outcome

- Provider lookup 계약이 없으면 Wallet reservation을 변경하지 않는다.
- attempt마다 제한된 backoff 후 다시 PENDING으로 전환한다.
- 최대 attempt 도달 시 `MANUAL_REVIEW`로 전환하며 balance는 reserved 상태를 유지한다.
- timeout을 자동 Release하는 정책은 명시적으로 금지한다.

### 7. Process lifecycle

- billing required mode에서만 worker를 시작한다.
- process context cancellation 시 신규 claim을 중단하고 현재 transaction을 제한 시간 내 종료한다.
- worker 오류로 Gateway process를 종료하지 않되 secret-free category log를 남긴다.
- readiness는 DB 연결을 계속 기준으로 하며 pending reconciliation 때문에 전체 traffic을 중단하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

새 경로 없음. reconciliation이 필요한 최초 요청은 기존 `billing_reconciliation_required` 503을 유지하고, terminal resolve 후 같은 Idempotency-Key retry가 snapshot을 replay한다.

### 내부 인터페이스

```go
type Observation struct {
    Outcome Outcome
    Reason Reason
    Snapshot ResponseSnapshot
}

MarkReconciling(ctx, chargeID string, observation Observation) error
RunOnce(ctx context.Context) (RunResult, error)
Run(ctx context.Context) error
```

### 데이터베이스 및 migration

forward-only `000007_image_charge_reconciliation.sql`. 기존 RECONCILING charge는 outcome 정보가 없으므로 UNKNOWN PENDING row로 backfill한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `MANUAL_REVIEW` query와 운영 runbook을 소유하는 후속 계획을 같은 initiative로 연결한다. Gateway DB row를 직접 resolve하거나 Wallet/Ledger를 수정하지 않는다.

## 보안 및 과금 고려사항

- UNKNOWN outcome을 비용 절감 목적으로 자동 Release하지 않는다.
- known success를 Release하거나 known failure를 Capture하지 않는다.
- worker와 handler는 동일 Billing/Wallet idempotency command를 사용한다.
- reconciliation row에 prompt, request body, credential과 idempotency key를 저장하지 않는다.
- error는 category만 저장하고 driver/provider 원문을 저장하지 않는다.
- lease owner는 무작위 process ID이며 host secret을 포함하지 않는다.

## 테스트 계획

### 단위 테스트

- HTTP/executor error outcome과 reason 분류
- backoff 경계·상한과 attempt 전이
- observation equality와 conflict
- config interval/batch/lease/max-attempt validation

### 통합 테스트

- known success response loss→RECONCILING→Capture→replay
- known failure response loss→Release→replay
- settlement rollback/commit-unknown retry의 단일 ledger effect
- timeout UNKNOWN이 balance를 reserved로 유지
- max attempt 후 MANUAL_REVIEW
- lease 만료 recovery와 두 worker SKIP LOCKED 단일 claim
- worker crash after Complete before RESOLVED update
- Gateway restart 후 pending row 처리
- 기존 RECONCILING row migration backfill
- tenant/price 변경이 기존 observation resolve에 영향 없음

### 프로세스 테스트

- billing disabled에서 worker 미기동
- billing required에서 worker 시작·graceful stop
- worker 일시 DB 오류 이후 process와 readiness 유지

### 필수 검증 명령

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
make integration-test
```

## 완료 조건

- [x] durable reconciliation schema와 기존 row backfill이 존재함
- [x] handler가 known success/failure/unknown observation을 정확히 기록함
- [x] timeout/panic이 즉시 Release되지 않음
- [x] known success는 Capture, known failure는 Release로 자동 resolve됨
- [x] UNKNOWN은 reservation 유지 후 MANUAL_REVIEW로 전환됨
- [x] claim lease와 SKIP LOCKED가 중복 worker effect를 방지함
- [x] worker crash/restart와 commit-unknown이 멱등 복구됨
- [x] terminal resolve 후 Idempotency-Key replay가 동작함
- [x] billing required process lifecycle에 worker가 연결됨
- [x] 전체 race/integration/CI 통과
- [x] README와 Cloud runbook handoff가 기록됨
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `f259a5009b8807cc9f827a699830506de5e1281a`
- Pull Request: [#13](https://github.com/nativegatewayhq/gateway/pull/13)
- CI: [check run 32342729902](https://github.com/nativegatewayhq/gateway/actions/runs/32342729902) 및 Plan policy validate 통과
- 로컬 검증: `make check` 통과
- PostgreSQL 통합 검증: `TEST_DATABASE_URL=... make integration-test` 통과
- 통합 검증에는 known success/failure resolve와 replay, UNKNOWN reservation/manual review, lease expiry, concurrent claim, Complete 이후 crash 복구 및 단일 Ledger capture가 포함됨

## Rollback 계획

- worker 실행만 중단하고 reconciliation/charge/Wallet data는 유지한다.
- 이전 binary로 rollback해도 RECONCILING reservation을 자동 Release하지 않는다.
- 잘못 resolve된 금액은 ledger row 수정이 아니라 검토된 compensating entry 후속 계획으로 처리한다.
- lease 만료 후 새 worker를 재개할 수 있도록 row를 삭제하지 않는다.

## 후속 작업

1. Cloud manual reconciliation API와 runbook
2. Gemini 이미지 price selector와 billing/idempotency
3. Replicate/fal asynchronous Job reconciliation
4. priority/weighted/lowest-cost routing과 fallback
5. OpenTelemetry billing/reconciliation metrics
