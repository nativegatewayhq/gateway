---
id: gateway-20260820-026
title: Phase 2 Provider Health Score and Circuit Breaker
status: in_progress
created_at: 2026-08-20T20:18:10+09:00
updated_at: 2026-08-20T20:18:10+09:00
owners:
  - gateway
initiative: phase-2-provider-health
depends_on:
  - gateway-20260820-016
  - gateway-20260820-017
  - gateway-20260820-023
  - gateway-20260820-024
  - gateway-20260820-025
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 2 Provider Health Score and Circuit Breaker

## 목적

Provider channel의 최근 실제 dispatch 결과를 Redis에 집계해 다중 Gateway 인스턴스가 공유하는 health score와 CLOSED/OPEN/HALF_OPEN circuit 상태를 운영한다. 실패 임계치를 넘은 channel은 후속 요청의 routing eligibility에서 제외하고, open 기간이 끝난 뒤 단 하나의 분산 half-open probe만 허용해 회복을 검증한다.

## 배경

Gateway는 fixed, priority, lowest-cost, weighted routing과 pre-dispatch fallback을 지원하지만 channel credential·price·spend cap 같은 정적 eligibility만 판단한다. upstream이 반복적으로 429, 5xx, timeout 또는 connection failure를 반환해도 매 요청이 같은 장애 channel을 다시 선택해 고객 지연과 실패율을 높인다. 인스턴스 로컬 circuit은 Kubernetes나 수평 확장 환경에서 서로 다른 상태를 만들며, half-open probe를 원자적으로 제한하지 않으면 회복 시점에 요청이 동시에 몰린다.

## 범위

- Provider channel 단위 Redis health store
- dispatch outcome의 bounded 분류와 rolling score
- CLOSED, OPEN, HALF_OPEN 분산 circuit state machine
- open 만료 후 단일 half-open probe lease
- fixed/priority/lowest-cost/weighted route의 health eligibility filter
- probe claim 이후 pre-dispatch 제외 시 안전한 permit release
- actual Provider dispatch 결과만 health observation으로 기록
- disabled/required mode와 fail-closed readiness
- OpenAI Images/Edits 및 Gemini 동일 channel-health 계약
- native response와 기존 billing/refund/reconciliation 의미 보존
- structured bounded logs와 Prometheus-ready internal counters
- 운영 문서 및 Cloud configuration handoff

## 제외 범위

- 자동 weight/priority 조정과 multi-armed bandit
- latency 기반 최저 지연 라우팅
- active synthetic health check와 Provider account balance polling
- 지역별 또는 tenant별 circuit
- dispatch 이후 다른 Provider로의 자동 fallback/hedging
- Dashboard, 공개 health endpoint와 수동 circuit API
- Redis Cluster cross-slot multi-key transaction
- LLM streaming outcome 분류

## 설계 및 구현 순서

### 1. Outcome과 score 계약

- health observation은 `success`, `rate_limited`, `server_error`, `timeout`, `connection`, `neutral`로 제한한다.
- Provider 2xx는 success, 429/5xx와 typed timeout/connection 오류는 failure로 기록한다.
- Provider 4xx 중 429를 제외한 요청 오류는 customer/request 영향 가능성이 있으므로 neutral로 분류하고 circuit numerator/denominator에서 제외한다.
- Gateway 인증, body validation, authorization, rate limit, price, Wallet, quota, spend cap, credential와 entropy 오류는 dispatch 전이므로 observation을 만들지 않는다.
- unknown outcome과 response loss는 billing reconciliation과 별개로 connection failure 한 건만 기록하며 중복 settlement poll은 health를 다시 기록하지 않는다.
- score는 고정 크기 Redis time bucket의 success/failure 합으로 계산하고 raw response/error/request content를 저장하지 않는다.

### 2. Circuit 상태 전이

- CLOSED는 최소 표본 수 이후 failure ratio가 설정 임계치 이상이면 OPEN으로 전이한다.
- OPEN은 `open_until` 전까지 routing에서 제외한다.
- 시간이 지나면 논리적으로 HALF_OPEN이며, Redis Lua script가 channel당 단일 probe token과 lease를 원자적으로 발급한다.
- probe success는 counters를 초기화하고 CLOSED로 복귀한다.
- probe failure는 새 `open_until`로 OPEN을 연장하며 exponential open duration을 상한까지 증가시킨다.
- probe lease가 observation 없이 만료되면 다음 요청이 새 probe를 claim할 수 있다.
- 수동 clock과 deterministic token generator를 주입해 boundary를 재현 가능하게 테스트한다.

### 3. Routing eligibility와 permit lifecycle

- 인증·권한·rate limit·request parsing·terminal replay 이후 candidate health를 조회한다.
- OPEN candidate는 price 조회, weighted draw와 Billing Begin 전에 제외한다.
- CLOSED candidate는 기존 route 정책에 그대로 참여한다.
- HALF_OPEN candidate는 실제 선택된 뒤 Billing Begin 전에 probe permit을 claim한다. claim 경쟁에서 패한 candidate는 pre-dispatch unavailable로 처리해 다음 candidate를 평가한다.
- selected candidate가 price/margin/spend cap/credential race 때문에 dispatch되지 않으면 permit을 idempotently release한다.
- Billing Begin 성공 후에는 다른 candidate로 fallback하지 않으며 dispatch 결과가 permit을 success/failure로 완료한다.
- request cancellation과 panic도 permit이 lease 만료까지 불필요하게 남지 않도록 best-effort release/observation을 수행한다.

### 4. Redis 저장과 원자성

- key는 non-secret canonical channel ID만 포함하고 deployment prefix를 지원한다.
- state, rolling buckets, probe lease와 transition은 Lua로 원자적으로 처리한다.
- TTL은 rolling window와 최대 open duration보다 길고 비활성 channel state는 자동 만료된다.
- observation ID는 request ID와 channel에서 비가역 digest로 만들고 짧은 dedupe TTL을 적용해 동일 dispatch 중복 기록을 막는다.
- Redis timeout/cancel을 구분하고 required mode에서는 routing 전역 503으로 fail closed한다.
- Redis payload/log에 Provider credential, prompt, response body, customer ID, price, balance와 raw error를 저장하지 않는다.

### 5. 설정과 readiness

- `GATEWAY_PROVIDER_HEALTH_MODE=disabled|required`
- required는 `GATEWAY_REDIS_URL`과 유효한 window, bucket, minimum samples, failure threshold, open duration, maximum open duration, probe lease와 command timeout을 요구한다.
- disabled는 기존 route 동작을 완전히 유지하고 Redis를 조회하지 않는다.
- required mode Redis ping/command 실패는 `/health/live`를 유지하되 `/health/ready`를 unavailable로 만든다.
- 설정 상한으로 bucket/key cardinality, Redis command duration과 circuit open 폭주를 제한한다.

### 6. 관측성

- logs는 request ID, provider, channel ID, state transition과 bounded outcome/category만 포함한다.
- failure ratio, raw counters, thresholds, probe token과 remaining open duration은 client response에 노출하지 않는다.
- internal metrics는 channel label cardinality 상한을 고려해 transition/allowed/rejected/observation counter를 제공할 수 있는 인터페이스로 분리한다.
- health failure로 모든 candidate가 제외되면 기존 native provider-unavailable 응답을 반환한다.

## 인터페이스와 데이터 변경

### 공개 API

새 endpoint와 native response field는 없다. OpenAI/Gemini 공식 SDK wire behavior를 유지한다. circuit-open과 probe 경쟁은 client에게 channel/state 세부 정보 없이 provider-unavailable로 표현한다.

### 내부 인터페이스

```go
type State string

const (
    Closed   State = "CLOSED"
    Open     State = "OPEN"
    HalfOpen State = "HALF_OPEN"
)

type Gate interface {
    Inspect(ctx context.Context, channelID string) (Snapshot, error)
    ClaimProbe(ctx context.Context, channelID, requestID string) (Permit, error)
    Release(ctx context.Context, permit Permit) error
    Observe(ctx context.Context, observation Observation) error
}
```

No-op Gate는 disabled mode를 구현한다. Redis Gate는 typed `ErrOpen`, `ErrProbeBusy`, `ErrUnavailable`을 반환하고 raw Redis 오류를 protocol handler가 직접 해석하지 않게 한다.

### 데이터베이스 및 migration

PostgreSQL migration은 없다. health는 짧은 수명의 운영 상태이며 Redis TTL 자료구조로만 저장한다. Billing charge, Wallet/Ledger, quota, spend cap과 reconciliation schema는 변경하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-2-provider-health` initiative에서 required-mode Redis와 동일 hash-slot prefix, health threshold 설정, secret Redis URL과 readiness alert를 배포한다. route publication은 channel ID cardinality 상한을 검증하며 circuit 상태나 score를 고객 API에 노출하지 않는다. Conformance는 장애 Provider mock으로 native wire response와 non-post-dispatch-fallback을 검증한다.

## 보안 및 과금 고려사항

- health 판단은 channel 단위이며 customer/request content를 Redis key/value에 포함하지 않는다.
- observation dedupe로 한 dispatch가 failure ratio를 여러 번 올리지 않게 한다.
- circuit은 Provider dispatch 이전 후보 선택만 바꾸며 예약 성공 이후 다른 Provider를 호출하지 않는다.
- known Provider failure의 기존 release, unknown outcome의 reconciliation reserve 의미를 변경하지 않는다.
- half-open permit을 얻었지만 Billing/credential 문제로 dispatch하지 않은 요청은 Provider failure로 기록하지 않는다.
- Redis 장애를 healthy로 간주하지 않고 required mode에서 fail closed해 장애 channel 집중을 막는다.
- inbound client가 channel ID, outcome, token이나 state를 조작할 공개 입력은 없다.
- logs/metrics/Redis에 credentials, request/response body, price, margin, Wallet/quota/cap 값과 raw upstream error를 기록하지 않는다.

## 테스트 계획

### 단위 테스트

- outcome status/error 분류와 neutral 제외
- rolling bucket boundary와 오래된 bucket 제거
- minimum samples와 exact failure threshold
- CLOSED→OPEN→HALF_OPEN→CLOSED/OPEN 전이
- exponential open duration 상한
- 단일 probe claim, lease 만료와 idempotent release/observe
- observation dedupe
- invalid config/key/channel rejection와 log redaction

### Redis 통합 테스트

- 다중 Gate 인스턴스의 동일 channel state 공유
- concurrent failure observation에서 단일 OPEN transition
- concurrent half-open claim에서 정확히 하나의 permit
- probe success reset과 probe failure reopen
- TTL expiry, Redis timeout/cancel과 script atomicity
- required readiness failure 및 recovery

### Protocol/Billing 통합 테스트

- priority/lowest-cost/weighted에서 OPEN channel 사전 제외
- fixed OPEN channel의 native provider-unavailable
- half-open selected candidate의 단일 Provider dispatch
- price/margin/spend-cap failure가 permit을 release하고 다른 candidate를 평가
- Wallet/customer/global 오류는 fallback하지 않으며 permit release
- 2xx, 429, 5xx, timeout, connection과 neutral 4xx observation parity
- OpenAI generation/JSON+multipart edit/Gemini 동일 동작
- post-dispatch failure에서 다른 Provider 호출 및 이중 charge 없음
- terminal replay가 health inspect/claim/observe를 호출하지 않음

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

- [ ] 실제 Provider dispatch만 bounded health observation을 생성함
- [ ] channel rolling score가 minimum sample/failure threshold를 정확히 적용함
- [ ] CLOSED/OPEN/HALF_OPEN 전이가 Redis에서 원자적임
- [ ] 다중 인스턴스에서 half-open probe가 하나만 발급됨
- [ ] probe success reset, failure reopen과 lease expiry가 정확함
- [ ] OPEN candidate가 모든 route policy에서 dispatch 전에 제외됨
- [ ] probe pre-dispatch 실패가 permit을 release하고 failure로 집계되지 않음
- [ ] global/customer 오류와 post-dispatch 실패는 다른 Provider로 fallback하지 않음
- [ ] observation dedupe가 동일 dispatch 중복 집계를 막음
- [ ] terminal replay가 health store를 읽거나 변경하지 않음
- [ ] Redis required-mode 장애가 fail closed하고 readiness를 낮춤
- [ ] OpenAI 생성·편집과 Gemini가 동일 health 계약을 사용함
- [ ] response/Redis/log에 secret/request content/raw error/비용 정보가 노출되지 않음
- [ ] disabled mode와 기존 fixed/priority/lowest-cost/weighted wire behavior가 회귀하지 않음
- [ ] README와 Cloud/Conformance handoff가 갱신됨
- [ ] 전체 race/integration/CI 통과
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## 후속 작업

- health score를 입력으로 사용하는 lowest-latency routing
- Prometheus metric exporter와 managed Dashboard
- 수동 circuit override 및 운영 감사 API

## Rollback 계획

- `GATEWAY_PROVIDER_HEALTH_MODE=disabled`로 즉시 health gate를 no-op으로 전환한다.
- health Redis key는 TTL로 만료되며 PostgreSQL과 Billing row를 수정하지 않는다.
- 이전 binary는 health 설정과 Redis key를 무시하고 기존 route 정책을 사용한다.
- 이미 Begin된 charge는 기존 channel/price snapshot과 reconciliation 규칙으로 settlement를 계속한다.
