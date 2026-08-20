---
id: gateway-20260820-022
title: Phase 1 Provider Channel Spend Caps
status: completed
created_at: 2026-08-20T18:37:44+09:00
updated_at: 2026-08-20T18:56:30+09:00
owners:
  - gateway
initiative: phase-1-provider-spend-controls
depends_on:
  - gateway-20260820-010
  - gateway-20260820-011
  - gateway-20260820-013
  - gateway-20260820-016
  - gateway-20260820-017
  - gateway-20260820-021
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Provider Channel Spend Caps

## 목적

Provider channel별 UTC 일·월 원가 지출 한도를 원자적으로 적용하고, 한도가 소진된 candidate를 Provider dispatch 전에 제외해 가능한 다음 channel로 fallback하면서 Gateway의 upstream 비용 노출을 제한한다.

## 배경

Gateway는 사용자 판매가 quota와 Wallet을 보호하지만 Provider에 실제로 지불하는 원가에는 channel별 상한이 없다. 특정 credential의 계약 한도, 선불 잔액 또는 운영 예산이 소진되어도 요청이 계속 선택될 수 있다. 사용자 quota와 달리 channel spend cap은 candidate별 상태이므로 한 channel의 소진이 전체 요청 실패로 즉시 전파되지 않고 fallback routing에 참여해야 한다.

## 범위

- Provider channel별 UTC calendar day/month 원가 cap
- estimate cost reserve, actual cost capture, 실패 release
- charge와 cap allocation의 immutable snapshot
- Billing transaction 내 Wallet·user quota·channel cap 원자 예약
- cap 소진 candidate의 pre-dispatch skip 및 다음 candidate fallback
- 모든 candidate 소진 시 native provider-unavailable 응답
- timeout/unknown reconciliation과 정확히 한 번 정산
- 운영자 create/update/disable CLI와 append-only audit
- channel별 current reserved/captured usage 조회 계약
- cap skip의 secret-free log와 bounded metric label 계약
- README와 Cloud provisioning handoff

## 제외 범위

- Provider 계정 balance API polling과 자동 cap 계산
- credential 암호화 저장, rotation과 다중 credential control plane
- user sale quota 또는 Wallet 정책 변경
- per-region, per-organization 또는 per-model Provider spend cap
- token/image quantity와 동시 request cap
- warning notification과 Dashboard
- dispatch 이후 다른 Provider로 retry하는 post-dispatch fallback

## 설계 및 구현 순서

### 1. Channel cap 정책과 bucket

- policy는 provider channel ID, UTC period, currency와 positive cost limit을 가진다.
- 하나의 channel/period에는 활성 policy 하나만 허용하고 mutation은 version과 append-only actor/reason event로 감사한다.
- bucket은 `(policy_id, period_start)`별 reserved/captured upstream cost를 저장한다.
- allocation은 charge, policy/version, limit, period, reserved/actual cost와 상태를 snapshot한다.
- channel/provider ownership과 currency는 기존 Provider Registry와 price contract로 검증한다.

### 2. Candidate별 reserve 계약

- price estimate가 선택한 channel ID와 `EstimatedCost`를 cap reserve 입력으로 사용한다.
- Billing `Begin`에서 charge, Wallet sale reserve, user quota sale reserve와 channel cost reserve를 한 transaction에 포함한다.
- deterministic policy/bucket lock과 overflow-safe integer 비교로 `captured + reserved + estimate <= limit`을 보장한다.
- cap 부족은 candidate-specific typed error를 반환하고 전체 transaction을 rollback한다.
- policy가 없는 channel은 unlimited이며 billing-disabled 경로는 cap을 조회하지 않는다.

### 3. Routing fallback

- OpenAI/Gemini candidate loop는 channel cap error를 price-unavailable과 동일한 pre-dispatch skip 계열로 취급한다.
- skip된 candidate에는 Provider call, charge, Wallet/Ledger와 user quota effect가 없어야 한다.
- 다음 candidate는 자신의 price와 cap을 새 transaction에서 평가한다.
- 모든 candidate가 unavailable/exhausted이면 기존 native `provider_unavailable` 응답을 사용하고 channel topology나 limit을 공개하지 않는다.
- idempotency replay는 저장된 완료 응답을 반환하며 현재 channel cap을 재소비하거나 재평가하지 않는다.

### 4. Settlement와 reconciliation

- 성공 시 actual Provider cost만 reserved에서 captured로 이동하고 차액을 release한다.
- known failure/cancel은 reserved를 전부 release한다.
- timeout/connection loss/response loss는 reservation을 유지하고 기존 reconciliation 결과에서 capture 또는 release한다.
- 동일 settlement 재시도는 idempotent하고 다른 actual cost는 conflict다.
- process restart 후 charge allocation만으로 정산을 재개할 수 있어야 한다.

### 5. 운영·관측 인터페이스

- `gateway-spend-cap` CLI는 channel ownership, day/month, `USD_TICKS`, actor/reason을 검증한다.
- log는 request ID, provider, channel ID, period/reset과 category만 포함하고 credential, limit, remaining과 request content를 기록하지 않는다.
- metric label은 provider, protocol, operation, outcome(`reserved`, `exhausted`, `captured`, `released`)만 허용한다.
- cap usage 조회는 policy ID와 period bucket의 reserved/captured/limit을 내부 운영 계약으로 제공한다.

## 인터페이스와 데이터 변경

### 공개 API

새 endpoint와 새 client-visible cap 오류는 없다. 소진 channel은 내부 fallback하며 모든 후보가 실패한 경우 기존 OpenAI/Gemini provider-unavailable 응답을 유지한다.

### 내부 인터페이스

```go
type SpendReservation struct {
    ChargeID string
    ChannelID string
    Currency string
    EstimatedCost int64
}

type SpendCapStore interface {
    ReserveInTx(context.Context, pgx.Tx, SpendReservation) (Allocation, error)
    CaptureInTx(context.Context, pgx.Tx, string, int64) error
    ReleaseInTx(context.Context, pgx.Tx, string) error
}
```

### 데이터베이스 및 migration

- `provider_channel_spend_policies`
- `provider_channel_spend_policy_events`
- `provider_channel_spend_buckets`
- `provider_channel_spend_allocations`
- schema는 additive이며 기존 channel은 policy가 없으므로 unlimited다.
- allocation/audit row는 rollback 시에도 삭제하지 않고 이전 binary는 신규 table을 무시한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-1-provider-spend-controls` initiative에서 channel별 계약/선불 예산을 `gateway-spend-cap`으로 provision하고 actor/reason을 제공한다. Provider balance 수집과 alerting은 별도 Cloud 계획이며 Gateway는 외부 balance API를 polling하지 않는다.

## 보안 및 과금 고려사항

- channel cap은 사용자 quota나 Wallet을 대신하지 않으며 세 조건이 모두 성공해야 dispatch한다.
- cap denial transaction은 charge, Wallet/Ledger와 user quota allocation까지 전부 rollback한다.
- channel cap은 내부 원가와 credential 운영 상태이므로 client response에 limit/usage를 노출하지 않는다.
- unknown Provider 결과의 reservation을 조기 release하지 않아 지연 성공으로 cap을 초과하지 않게 한다.
- cap policy ID와 bucket key에 credential, API Key, prompt 또는 customer identifier를 넣지 않는다.
- fallback은 dispatch 전 실패에만 허용해 이중 Provider 원가를 만들지 않는다.

## 테스트 계획

### 단위 테스트

- UTC day/month, leap year와 reset
- policy/currency/channel/amount 검증과 overflow
- typed candidate exhaustion과 routing 분류
- log/response redaction
- policy 없는 unlimited regression

### 통합 테스트

- migration constraints, ownership FK, audit append-only와 CLI lifecycle
- 다중 service/goroutine에서 cap 이하만 reserve되는 원자성
- Wallet/user quota/channel cap 중 하나 실패 시 전체 rollback
- reserve→capture/release와 estimate/actual 차액
- fallback 첫 channel exhausted→두 번째 channel dispatch 및 정확한 charge snapshot
- 모든 channel exhausted→Provider 호출 0회
- idempotency replay 무소비
- timeout reconciliation과 process restart

### 호환성 및 장애 테스트

- policy 없는 기존 OpenAI/Gemini 공식 protocol 동작 회귀
- PostgreSQL cancellation과 concurrent policy mutation
- period boundary 직전 reservation의 snapshot settlement
- disabled policy와 이전 binary rollback 영향
- `go test -race` data race 없음

### 필수 검증 명령

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
go build ./cmd/gateway-quota
go build ./cmd/gateway-spend-cap
make integration-test
```

## 완료 조건

- [x] policy 없는 channel이 unlimited로 동일하게 동작함
- [x] day/month channel cost cap이 UTC 기준으로 적용됨
- [x] 동시 요청에서도 reserved+captured가 limit을 초과하지 않음
- [x] Wallet, user quota와 channel cap reserve/rollback이 원자적임
- [x] exhausted candidate가 dispatch 없이 skip되고 다음 candidate로 fallback함
- [x] 모든 candidate 소진 시 Provider 호출과 금전 effect가 없음
- [x] actual cost 차액, failure와 reconciliation 정산이 정확히 한 번 수행됨
- [x] idempotency replay와 process restart가 cap을 중복 소비하지 않음
- [x] 정책 CLI, append-only audit, ownership과 usage 조회가 검증됨
- [x] 내부 원가·limit·credential·request content가 response/log에 노출되지 않음
- [x] README와 Cloud handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `4eac31c`
- PR: `https://github.com/nativegatewayhq/gateway/pull/21`
- 로컬 release gate: `GOCACHE=/private/tmp/nativegateway-go-cache make check` 통과
- 로컬 통합 검증: PostgreSQL `127.0.0.1:55433`, Redis `127.0.0.1:56379`를 사용한 `make integration-test` 통과
- cap 원자성: UTC day/month 정책과 2개 Billing service의 8개 동시 요청에서 limit 이내 3개만 예약됨을 실제 PostgreSQL에서 검증
- 정산: actual cost 차액, capture/release, idempotent retry, unknown reconciliation과 process restart 후 bucket/Wallet/quota 일치 검증
- fallback: OpenAI/Gemini 첫 channel 소진 시 두 번째 channel dispatch, 전체 소진 시 Provider 0회와 native provider-unavailable 검증
- 운영·보안: `gateway-spend-cap` lifecycle, append-only audit, ownership/usage 조회 및 response/log redaction 검증
- GitHub Actions: `check` pass (`32356165223`), `validate` pass (`32356209384`)

## Rollback 계획

- active spend policy를 disable해 해당 channel을 unlimited 상태로 되돌린다.
- 이전 binary rollback 전 channel cap enforcement가 사라짐을 운영자가 승인한다.
- in-flight allocation은 reconciliation 종료 전 유지한다.
- policy, bucket, allocation과 audit table은 삭제하지 않는다.
- cap 때문에 skip된 pre-dispatch candidate는 Provider/금전 effect가 없어 compensation이 필요하지 않다.

## 후속 작업

1. encrypted multi-credential Provider channel storage와 rotation
2. Provider balance polling과 low-balance alert
3. latency/success health score와 circuit breaker
4. weighted/lowest-cost routing
5. 관리형 channel/credential Dashboard
