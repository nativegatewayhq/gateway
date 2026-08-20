---
id: gateway-20260820-021
title: Phase 1 Hierarchical Cost Quotas
status: in_progress
created_at: 2026-08-20T18:11:08+09:00
updated_at: 2026-08-20T18:11:08+09:00
owners:
  - gateway
initiative: phase-1-cost-quotas
depends_on:
  - gateway-20260820-009
  - gateway-20260820-010
  - gateway-20260820-011
  - gateway-20260820-014
  - gateway-20260820-019
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Hierarchical Cost Quotas

## 목적

조직, 프로젝트, API Key 및 logical model 단위의 기간별 판매가 한도를 정의하고, 동시 요청에서도 모든 적용 범위를 원자적으로 예약해 설정된 예산을 초과하는 Provider 지출을 dispatch 전에 차단한다.

## 배경

현재 Gateway는 Wallet 잔액, 요청별 가격, Reserve/Capture/Release와 API Key RPM을 지원한다. 그러나 충분한 Wallet 잔액이 있으면 특정 프로젝트, 유출된 Key 또는 고가 모델이 조직 예산을 모두 사용할 수 있다. 단순 사용량 조회 후 차단하는 방식은 동시 요청에서 초과 지출을 허용하므로 기존 과금 transaction과 결합된 quota reservation이 필요하다.

## 범위

- organization, project, API Key와 logical model quota scope
- UTC calendar day 및 calendar month 판매가 한도
- scope별 optional policy와 계층 전체의 AND 적용
- estimate 기준 quota reserve, 실제 판매가 capture, 실패·차액 release
- OpenAI/Gemini native quota 오류와 secret-free metadata
- idempotency replay, fallback, timeout reconciliation과 정확히 한 번 정산
- 제한 정책을 생성·변경·비활성화하는 운영자 CLI
- 기간별 reserved/captured 조회를 위한 내부 store 계약
- PostgreSQL 기반 원자 동시성 및 감사 가능한 정책 변경 이력
- README와 Cloud provisioning handoff

## 제외 범위

- 요청 수, token, image quantity와 동시 Job quota
- rolling window, 사용자 timezone과 custom billing cycle
- hard quota 이외의 경고 threshold 및 notification
- end-user quota 관리 REST API와 Dashboard
- Provider credential/channel별 원가 spend cap
- 조직 간 shared budget과 quota transfer
- 결제 자동 충전 및 subscription entitlement

## 설계 및 구현 순서

### 1. 정책과 기간 bucket 데이터 모델

- quota policy는 scope type/ID, optional protocol/operation/logical model, period와 limit minor units를 가진다.
- logical model quota는 Provider-native model이나 channel이 아닌 `protocol + operation + logical model`에 결합한다.
- 하나의 scope/dimension/period에는 활성 policy 하나만 허용하고 변경 이력을 append-only audit row로 남긴다.
- UTC day/month bucket은 `[period_start, period_end)`로 고정하며 요청 시점의 bucket을 snapshot한다.
- 금액은 기존 Wallet/Ledger와 동일한 currency/minor-unit 정수만 사용하고 float 계산을 금지한다.

### 2. 계층 policy 해석

- 인증 Principal과 검증된 logical operation으로 organization, project, API Key, model scope를 결정한다.
- 적용되는 모든 활성 policy를 조회하며 policy가 없는 scope는 unlimited로 취급한다.
- scope 우선순위로 하나를 선택하지 않고 모든 한도를 동시에 만족해야 한다.
- policy snapshot에는 policy ID/version, limit, period와 bucket 경계를 저장해 중간 정책 변경이 in-flight 정산을 바꾸지 않게 한다.
- 잘못된 scope ownership, currency mismatch와 중복 활성 policy는 startup/mutation 또는 request에서 fail closed한다.

### 3. Billing Begin과 원자 quota reserve

- 인증, network/rate/model authorization, model/candidate/price 검증 후 Billing `Begin` transaction에서 Wallet Reserve와 quota reserve를 함께 수행한다.
- quota bucket row를 deterministic key로 생성하고 일정한 scope 순서로 lock해 교착 가능성을 제한한다.
- 각 bucket에 대해 `captured + reserved + estimate <= limit`을 한 SQL transaction에서 보장한다.
- 어느 한 scope라도 부족하면 모든 quota 변경과 Wallet Reserve를 rollback하고 Provider를 호출하지 않는다.
- idempotency replay는 기존 charge/quota reservation 결과를 재사용하며 quota를 다시 소비하지 않는다.
- fallback candidate가 판매가 estimate를 바꾸면 dispatch 전 기존 예약을 원자적으로 교체하거나 해당 candidate를 건너뛴다.

### 4. Capture, release와 reconciliation

- 성공 시 charge의 실제 판매가만 reserved에서 captured로 이동하고 estimate 차액을 release한다.
- 확정 실패와 취소는 quota reserved를 전부 release하며 captured를 감소시키지 않는다.
- timeout/unknown은 Wallet과 동일하게 reservation을 유지하고 reconciliation 결과에서 한 번만 capture 또는 release한다.
- quota allocation row는 charge ID와 policy snapshot별 unique constraint를 가져 webhook/retry/crash에도 중복 정산되지 않는다.
- 수동 금전 correction은 quota usage를 암묵적으로 변경하지 않으며 별도 quota adjustment audit가 필요한 경우 후속 계획으로 분리한다.

### 5. Native 오류와 운영자 인터페이스

- quota 부족은 OpenAI `429 rate_limit_error/quota_exceeded`, Gemini `429 RESOURCE_EXHAUSTED`로 반환한다.
- `Retry-After`는 가장 이른 적용 bucket 종료까지의 초 단위 상한을 반환한다.
- 응답과 log는 scope type, period, request ID, Key/project ID와 reset만 포함하고 limit/잔액, 조직 topology와 credential은 노출하지 않는다.
- `gateway-quota` CLI는 scope ownership과 금액을 검증하고 create/update/disable을 transaction과 audit actor/reason으로 기록한다.
- 기존 policy가 없는 설치와 self-hosted BYOK billing-disabled mode는 동작이 변하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

새 endpoint는 없다. quota 초과 시 native 429와 `Retry-After`, `X-Quota-Reset` 응답이 추가된다.

### 내부 인터페이스

```go
type Scope struct {
    Type      ScopeType
    ID        string
    Protocol  string
    Operation string
    Model     string
}

type ReservationRequest struct {
    ChargeID string
    Amount   int64
    Currency string
    At       time.Time
    Scopes   []Scope
}

type QuotaStore interface {
    Reserve(context.Context, pgx.Tx, ReservationRequest) ([]Allocation, error)
    Capture(context.Context, pgx.Tx, string, int64) error
    Release(context.Context, pgx.Tx, string) error
}
```

Quota store는 독립 transaction을 열지 않고 Billing이 소유한 transaction을 사용한다.

### 데이터베이스 및 migration

- `cost_quota_policies`: immutable ID/version, scope, model dimension, period, currency, limit, status
- `cost_quota_policy_events`: create/update/disable actor, reason, timestamp의 append-only audit
- `cost_quota_buckets`: policy/version, UTC period 경계, reserved/captured minor units와 non-negative check
- `cost_quota_allocations`: charge/policy snapshot, reserved/captured/released 상태와 unique key
- 기존 row는 backfill하지 않으며 policy가 없으면 unlimited다.
- migration은 additive이고 이전 binary는 table을 무시한다. rollback 전 신규 policy enforcement가 사라짐을 운영자가 승인해야 한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-1-cost-quotas` initiative에서 managed deployment의 기본 조직/프로젝트/Key budget을 `gateway-quota`로 provision하고 정책 변경 actor/reason을 보존한다. Dashboard와 공개 관리 API는 별도 Cloud/Gateway 계획으로 만들며 이번 Gateway 계획은 CLI와 DB 계약만 제공한다.

## 보안 및 과금 고려사항

- quota는 Wallet 대체물이 아니라 추가 AND 조건이며 잔액과 quota가 모두 충분해야 한다.
- 모든 scope reserve와 Wallet Reserve는 동일 transaction에서 성공하거나 모두 rollback해야 한다.
- quota denial은 Provider 호출, routing fallback과 Ledger 금전 row를 만들지 않는다.
- raw API Key, prompt, forwarding chain과 Provider credential은 quota key/log/metric에 포함하지 않는다.
- policy mutation CLI는 scope가 같은 organization tree에 속하는지 검증해 타 tenant ID 연결을 막는다.
- period 계산은 DB/application clock 차이를 피하도록 transaction의 PostgreSQL timestamp를 기준으로 한다.
- timeout reservation은 섣불리 release하지 않아 뒤늦은 Provider 성공으로 quota를 초과하지 않게 한다.

## 테스트 계획

### 단위 테스트

- UTC day/month boundary, leap year와 exact reset 계산
- organization/project/Key/model policy intersection과 unlimited default
- currency, ownership, amount, period와 duplicate policy 검증
- native OpenAI/Gemini 429 envelope와 header
- idempotency replay, model denial 및 network/rate ordering 회귀
- quota error/log redaction

### 통합 테스트

- migration constraint, policy audit, disable과 ownership FK
- 다중 goroutine에서 limit 이하만 reserve되는 atomicity
- 여러 scope 중 하나 실패 시 bucket과 Wallet 전체 rollback
- Reserve→Capture/Release, estimate 차액과 allocation 중복 호출
- fallback price 교체와 charge snapshot
- timeout→reconciliation success/failure/unknown 및 process restart
- CLI create/update/disable과 plaintext secret 비접근

### 호환성 및 장애 테스트

- policy 없는 기존 Key와 billing-disabled self-hosted 경로 회귀
- PostgreSQL serialization/deadlock retry와 transaction cancellation
- 기간 경계 직전 시작한 in-flight 요청이 snapshot bucket에 정산됨
- Gateway 다중 인스턴스 동시 quota 경쟁
- `go test -race`에서 policy snapshot data race 없음

### 필수 검증 명령

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
go build ./cmd/gateway-quota
make integration-test
```

## 완료 조건

- [ ] 기존 설치와 policy 없는 tenant가 unlimited로 동일하게 동작함
- [ ] organization/project/API Key/model 한도가 계층적으로 모두 적용됨
- [ ] day/month bucket과 reset이 UTC 기준으로 정확함
- [ ] 동시 요청에서도 reserved+captured가 limit을 초과하지 않음
- [ ] Wallet과 모든 quota scope가 한 transaction에서 reserve/rollback됨
- [ ] 성공, 실패, 차액과 timeout reconciliation 정산이 정확히 한 번 수행됨
- [ ] idempotency replay와 fallback이 quota를 중복 소비하지 않음
- [ ] native 429가 body/Provider effect 전에 반환됨
- [ ] 정책 mutation audit와 tenant ownership이 검증됨
- [ ] 민감한 금액·credential·request content가 log/response에 노출되지 않음
- [ ] README와 Cloud handoff가 갱신됨
- [ ] 전체 race/integration/CI 통과
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 신규 quota policy를 disable해 요청을 unlimited 상태로 되돌린다.
- 이전 binary rollback 전 quota enforcement가 사라지는 tenant를 식별하고 명시적으로 승인한다.
- quota table과 allocation/audit row는 삭제하거나 수정하지 않는다.
- in-flight quota reservation은 reconciliation이 종료될 때까지 보존한다.
- quota 때문에 거부된 요청에는 금전/Provider effect가 없으므로 compensation이 필요하지 않다.

## 후속 작업

1. Provider credential/channel 원가 spend cap
2. 요청 수, image quantity와 동시 Job quota
3. quota warning notification과 usage export
4. Key/Quota 관리 REST API, audit log와 Dashboard
5. subscription entitlement와 payment-backed deposit
