---
id: gateway-20260820-025
title: Phase 2 Weighted Provider Routing
status: completed
created_at: 2026-08-20T19:59:29+09:00
updated_at: 2026-08-20T20:14:57+09:00
owners:
  - gateway
initiative: phase-2-weighted-routing
depends_on:
  - gateway-20260820-016
  - gateway-20260820-017
  - gateway-20260820-022
  - gateway-20260820-023
  - gateway-20260820-024
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 2 Weighted Provider Routing

## 목적

동일 logical image operation의 eligible Provider channel에 운영자가 게시한 정수 weight를 적용해 요청 트래픽을 확률적으로 분산한다. 선택은 암호학적으로 안전한 무편향 난수를 사용하고, 선택된 candidate가 dispatch 전에 credential·price·spend cap 문제로 제외되면 남은 weight를 다시 정규화해 중복 없이 다음 candidate를 선택한다.

## 배경

Gateway는 fixed, priority, lowest-cost 정책과 pre-dispatch fallback을 지원한다. 그러나 canary rollout, 공급자 구매 약정 배분, 신규 channel 점진 확대에는 순서나 원가만으로 표현할 수 없는 비율 기반 분산이 필요하다. 단순 random shuffle은 weight 비율을 보장하지 못하고, 실패 candidate를 제거하지 않은 재추첨은 같은 channel을 반복하거나 무한 loop를 만들 수 있다. 또한 idempotency replay와 Provider dispatch 이후 retry가 재추첨되면 한 논리 요청이 여러 Provider에 전달될 수 있다.

## 범위

- image model route의 `weighted` policy
- candidate별 양의 정수 weight와 route 단위 총합 제한
- crypto/rand 기반 rejection sampling과 무편향 weighted draw
- eligible candidate 집합만 대상으로 하는 weight 정규화
- 선택 candidate의 pre-dispatch 실패 시 제거 후 단 한 번씩만 재선택
- spend cap, credential, executor, price와 minimum margin filtering
- customer/global 오류와 post-dispatch 실패의 non-fallback 보장
- terminal idempotency replay의 무추첨 응답
- charge에 routing policy와 weighted draw rank 보존
- OpenAI Images/Edits와 Gemini 동일 선택 계약
- 통계적 편향 탐지 테스트와 deterministic injected entropy 테스트
- README 및 Cloud route publication handoff

## 제외 범위

- latency/success health score와 circuit breaker
- weight 자동 조정 및 bandit/ML routing
- 조직, 프로젝트, API Key 또는 지역별 weight override
- sticky session, customer affinity와 consistent hashing
- 동일 요청의 Provider hedging 또는 동시 실행
- dispatch 이후 429/5xx/timeout 재추첨
- weighted-lowest-cost 복합 정책
- 관리형 route CRUD API와 Dashboard

## 설계 및 구현 순서

### 1. Registry 계약

- `weighted` policy를 추가하고 `ChannelCandidate.Weight`를 정수로 정의한다.
- weighted route의 enabled candidate는 모두 `weight >= 1`이어야 한다.
- candidate 수, 개별 weight와 합계에 명시적 상한을 두어 overflow와 과도한 연산을 막는다.
- fixed/priority/lowest-cost route에서는 weight를 비워 두거나 기본값으로만 취급하며 기존 ordering을 변경하지 않는다.
- duplicate candidate/channel, unsupported Provider와 capability 검증은 기존 registry 계약을 유지한다.

### 2. 무편향 weighted sampler

- sampler는 `io.Reader` entropy dependency를 주입받고 production에서는 `crypto/rand.Reader`를 사용한다.
- 전체 weight 합 `N`에 대해 modulo bias가 없는 rejection sampling으로 `[0,N)` 정수를 만든다.
- 누적 weight 구간으로 candidate를 선택하며 입력 순서와 무관하도록 candidate ID 기준 canonical ordering을 사용한다.
- entropy read 실패, short read, invalid weight와 합계 overflow는 typed internal error로 fail closed한다.
- sampler API는 선택된 candidate와 draw 순번만 반환하고 random 원문이나 구간 값을 로그에 남기지 않는다.

### 3. Eligibility와 fallback

- 인증, 권한, rate limit, body/selector 검증과 terminal replay 후에만 최초 draw를 수행한다.
- executor와 active channel credential이 없는 candidate는 draw 전에 제거한다.
- weighted draw 후 exact price/minimum margin을 검증하고 unavailable이면 해당 candidate를 제거해 남은 집합에서 재추첨한다.
- Billing Begin의 spend-cap exhaustion도 선택 candidate를 제거하고 남은 weight를 재정규화한다.
- Wallet 부족, customer quota, DB/entropy 오류, idempotency conflict는 전역 오류로 즉시 종료한다.
- Begin 성공 후 credential race, Provider dispatch 또는 Provider 응답 오류에는 다른 candidate를 추첨하지 않는다.
- 각 candidate는 요청당 최대 한 번만 시도되며 모든 candidate 소진 시 native provider-unavailable을 반환한다.

### 4. 과금 및 감사 증거

- Billing Begin은 `routing_policy=weighted`와 선택 시도 순번을 받는다.
- charge의 기존 nullable routing evidence를 weighted에도 허용하도록 additive constraint migration을 적용한다.
- stored channel, Provider model snapshot, price, estimated cost와 reserved sale 의미는 변경하지 않는다.
- routing draw 자체는 가격 snapshot을 고정하지 않으며 선택된 candidate의 기존 exact price transaction을 사용한다.
- idempotency terminal replay는 저장된 response/charge를 반환하고 entropy, price, credential, quota와 spend cap을 소비하지 않는다.

### 5. 관측성과 운영 안전장치

- completion log는 `routing_policy=weighted`와 bounded attempt rank를 기록한다.
- skip log는 request ID, provider, channel ID와 bounded category만 포함한다.
- weight, total weight, random draw, price, margin, cap remaining, credential과 request content는 로그/응답에 노출하지 않는다.
- weight 분포 검증은 운영 로그의 customer content가 아니라 aggregate metric 후속 작업으로 남긴다.

## 인터페이스와 데이터 변경

### 공개 API

새 client endpoint와 wire 필드는 없다. 공식 OpenAI/Google SDK 요청과 native response를 유지하며 Provider/channel/weight 정보는 반환하지 않는다.

### 내부 인터페이스

```go
type ChannelCandidate struct {
    // existing fields
    Weight uint32
}

type WeightedSampler interface {
    Pick(candidates []RoutingDecision) (RoutingDecision, error)
}
```

실제 구현은 남은 candidate slice를 입력받아 한 candidate만 선택한다. handler가 성공/제외 상태를 소유해 sampler가 가격, credential 또는 Billing dependency를 갖지 않게 한다.

### 데이터베이스 및 migration

- `image_request_charges.routing_policy` constraint가 `weighted`를 허용하도록 교체한다.
- 기존 `cost_rank`는 이름 변경 없이 candidate attempt rank로 재사용하고 `price_evaluated_at`은 weighted charge에서 null을 유지한다.
- route/weight publication은 현재 code-backed registry 계약을 따르며 이 계획에서 별도 route table을 추가하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-2-weighted-routing` initiative에서 logical model candidate별 positive integer weight를 게시한다. 게시 전 모든 candidate의 credential, exact price, minimum margin과 spend cap을 준비하고, Gateway가 허용하는 개별/총 weight 상한을 검증한다. 가중치 변경은 새 Gateway 배포 전 conformance fixture로 검증하며 client에게 weight를 노출하지 않는다.

## 보안 및 과금 고려사항

- 예측 가능한 PRNG나 request/customer identifier를 seed로 사용하지 않아 특정 Provider 선택 조작을 막는다.
- rejection sampling으로 modulo bias를 제거하고 integer overflow를 사전에 거부한다.
- entropy 실패는 priority fallback으로 조용히 축소하지 않고 fail closed한다.
- candidate-specific pre-dispatch 실패만 재추첨하며 전역 과금 오류를 routing fallback으로 숨기지 않는다.
- Begin 성공 이후에는 어떤 Provider 오류에도 재추첨하지 않아 중복 생성과 이중 원가를 방지한다.
- replay는 새 random draw와 새 charge를 만들지 않는다.
- 로그와 response는 weight, draw, 가격, 잔액, 정책 한도, credential과 request content를 포함하지 않는다.

## 테스트 계획

### 단위 테스트

- registry weighted policy와 weight 0/overflow/합계 상한 거부
- canonical candidate ordering과 injected entropy별 구간 선택
- rejection sampling boundary, short read와 entropy error
- candidate 제거 후 남은 weight 재정규화와 중복 시도 방지
- fixed/priority/lowest-cost policy 회귀
- log/response redaction

### 통합 테스트

- OpenAI 생성·JSON 편집·multipart 편집과 Gemini에서 동일 weighted selection
- credential/executor unavailable candidate가 draw 전에 제외됨
- selected price/margin unavailable과 spend cap exhausted 후 남은 candidate 선택
- 모든 candidate 소진 시 Wallet/Ledger/quota/cap/Provider effect 0
- Wallet/customer quota/DB 오류 시 다른 candidate 미시도
- terminal replay가 entropy와 현재 eligibility를 재평가하지 않음
- charge에 weighted policy/attempt rank 저장 및 immutable constraint

### 통계 및 장애 테스트

- 고정 weight 집합을 충분히 반복해 허용 오차 내 분포를 검증하되 deterministic sampler 단위 테스트를 주 정확성 증거로 사용
- `go test -race`에서 shared entropy/sampler data race 없음
- entropy failure, context cancellation, Provider timeout과 connection loss
- 다중 Gateway 인스턴스에서 독립 draw 및 단일 idempotent charge

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

- [x] weighted route가 positive integer weight 비율로 eligible candidate를 선택함
- [x] modulo bias, weight overflow와 predictable PRNG가 없음
- [x] candidate ordering과 injected entropy 결과가 deterministic함
- [x] executor/credential unavailable candidate가 draw 전에 제외됨
- [x] price/margin/spend-cap candidate failure 후 남은 weight가 재정규화됨
- [x] candidate는 요청당 최대 한 번만 시도됨
- [x] global/customer 오류와 post-dispatch 실패는 재추첨하지 않음
- [x] terminal replay가 entropy와 route/price를 소비하지 않음
- [x] charge가 weighted policy와 attempt rank를 보존함
- [x] OpenAI 생성·편집과 Gemini가 동일 sampler 계약을 사용함
- [x] response/log에 weight/draw/가격/credential/request content가 노출되지 않음
- [x] fixed/priority/lowest-cost와 native SDK wire behavior가 회귀하지 않음
- [x] README와 Cloud handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `51b0e32ddaec91749a0942748975b23818ba0dd6`
- PR: https://github.com/nativegatewayhq/gateway/pull/24
- local check: `GOCACHE=/private/tmp/nativegateway-go-cache make check` 통과
- local integration: PostgreSQL 17/Redis 8에서 `make integration-test` 통과
- GitHub Actions check: https://github.com/nativegatewayhq/gateway/actions/runs/32362729459/job/96405597130
- GitHub Actions plan validation: https://github.com/nativegatewayhq/gateway/actions/runs/32362755059/job/96405672590
- 검증 범위: registry weight/candidate/total 상한, canonical interval과 modulo-bias rejection, deterministic 분포, shared entropy race, executor/credential prefilter, price/margin/spend-cap 재정규화, global/post-dispatch non-redraw, terminal replay 무추첨, OpenAI JSON/multipart edit와 Gemini parity, weighted charge evidence와 immutability

## 후속 작업

- Provider health score와 circuit breaker가 open candidate를 weighted eligibility 집합에서 제외하는 계약
- aggregate candidate selection metric과 managed route distribution dashboard
- Cloud route publication 및 configuration validation

## Rollback 계획

- affected logical model policy를 기존 `priority`, `fixed` 또는 `lowest_cost`로 되돌린다.
- 이전 binary는 weighted route를 로드하지 않으며 additive charge constraint 외 기존 row를 변경하지 않는다.
- 이미 Begin된 charge는 stored channel/price snapshot으로 settlement와 replay를 계속한다.
