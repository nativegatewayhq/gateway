---
id: gateway-20260820-024
title: Phase 2 Lowest-Cost Provider Routing
status: in_progress
created_at: 2026-08-20T19:34:08+09:00
updated_at: 2026-08-20T19:34:08+09:00
owners:
  - gateway
initiative: phase-2-cost-routing
depends_on:
  - gateway-20260820-010
  - gateway-20260820-011
  - gateway-20260820-016
  - gateway-20260820-017
  - gateway-20260820-021
  - gateway-20260820-022
  - gateway-20260820-023
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 2 Lowest-Cost Provider Routing

## 목적

동일한 logical image operation을 수행할 수 있는 eligible Provider channel을 하나의 가격 평가 시점에서 비교하고 예상 upstream 원가가 가장 낮은 candidate를 선택한다. 최저 원가 channel의 가격·credential·spend cap이 dispatch 전에 변경되거나 소진되면 동일 snapshot 규칙으로 다음 최저 원가 candidate를 평가하되, Provider dispatch 이후에는 fallback하지 않는다.

## 배경

Gateway는 fixed/priority routing, exact channel price, minimum margin, credential availability, customer quota, Wallet과 Provider spend cap을 지원한다. 그러나 priority 정책은 운영자가 정한 순서만 사용하므로 더 저렴한 channel이 준비되어도 자동으로 선택하지 않는다. 단순히 candidate마다 `Quote`를 호출해 가장 낮은 값을 고르면 가격 유효기간 경계와 Quote→Begin race에서 서로 다른 시점 또는 다른 price version을 비교·예약할 수 있다. 비용 라우팅은 선택 근거와 실제 charge snapshot이 일치해야 과금·마진 감사가 가능하다.

## 범위

- image model route의 `lowest_cost` policy
- eligible candidate 전체의 동일 UTC evaluation timestamp 가격 조회
- 예상 upstream `EstimatedCost` 오름차순 정렬
- cost tie의 priority, candidate ID deterministic tie-break
- exact price ID와 evaluated-at을 Begin에 binding
- Quote→Begin price change의 typed pre-dispatch re-evaluation
- credential, capability, active channel과 minimum margin filter
- spend cap 소진 시 다음 최저 원가 candidate fallback
- 선택된 정책, rank, evaluated cost와 price ID charge snapshot
- secret/cost-safe routing log와 bounded outcome category
- OpenAI Images/Edits와 Gemini 동일 정책 적용
- README 및 Cloud route publication handoff

## 제외 범위

- weighted random routing
- latency/success health score와 circuit breaker
- 지역, 조직, 고객 또는 API Key별 routing policy
- 실제 Provider account balance polling
- 가격 예측, spot price와 예약 구매 최적화
- dispatch 이후 429/5xx/timeout fallback
- 복수 Provider 동시 실행 또는 hedging
- 관리형 routing policy API와 Dashboard

## 설계 및 구현 순서

### 1. Route policy와 candidate quote

- image registry에 `lowest_cost`를 추가하고 candidate priority는 동률 tie-break로 유지한다.
- handler는 인증·권한·body/selector 검증과 idempotency terminal replay 후 route evaluation timestamp 하나를 생성한다.
- executor와 channel credential이 없는 candidate는 가격 조회 전에 제외한다.
- 모든 remaining candidate에 logical protocol/operation/model, channel, selector와 동일 `EvaluatedAt`을 전달한다.
- price 없음, inactive channel 또는 minimum margin 위반 candidate는 제외하고 다른 candidate 평가를 계속한다.
- DB/가격 엔진 자체 장애는 candidate unavailable로 축소하지 않고 native service-unavailable로 fail closed한다.

### 2. Lowest-cost ordering

- 유효 quote는 `EstimatedCost` 오름차순으로 정렬한다.
- cost가 같으면 candidate priority 오름차순, candidate ID 사전순으로 결정한다.
- 판매가가 낮다는 이유만으로 원가가 높은 candidate를 선택하지 않는다.
- zero/negative cost, currency mismatch, overflow 또는 candidate/channel mismatch quote는 내부 오류로 거부한다.
- fixed와 priority policy의 기존 ordering과 동작은 변경하지 않는다.

### 3. Quote snapshot binding

- quote는 price ID, channel ID, currency, estimated cost, maximum sale와 evaluated-at을 immutable selection snapshot으로 반환한다.
- Billing `BeginRequest`는 optional expected quote를 받고 동일 price ID/channel/evaluated-at selector로 다시 검증한다.
- Begin이 다른 price version, cost/sale/currency 또는 inactive interval을 관찰하면 typed `price_snapshot_changed`를 반환하고 transaction·금전 effect를 만들지 않는다.
- handler는 Provider dispatch 전 candidate 전체를 새 timestamp로 한 번 재평가한다.
- 반복 mutation으로 snapshot이 다시 바뀌면 요청을 service-unavailable로 종료하며 무한 retry하지 않는다.

### 4. Reserve와 fallback

- ordered candidate의 bound quote로 Wallet sale, customer quota와 channel upstream cost cap을 기존 단일 transaction에서 reserve한다.
- spend cap 소진은 다음 ordered candidate로 이동한다.
- Wallet 부족, customer quota 초과, DB 오류와 idempotency conflict는 customer/request 전역 오류이므로 다른 candidate로 이동하지 않는다.
- 한 candidate의 Begin 성공 후 credential race가 발생하면 기존 release 규칙을 사용하고 다른 Provider를 호출하지 않는다.
- 모든 candidate가 price/credential/cap 때문에 제외되면 Provider 및 금전 effect 없이 native provider-unavailable을 반환한다.

### 5. 감사와 관측성

- charge는 routing policy, cost rank, selected candidate ID와 bound price ID/evaluation timestamp를 보존한다.
- 기존 channel/provider/model snapshot과 원가·판매가 정산 의미를 유지한다.
- completion log는 policy와 rank를 기록할 수 있지만 candidate별 cost, sale, margin, cap remaining, credential 또는 request content를 기록하지 않는다.
- skip log는 request ID, provider, channel ID와 bounded category만 사용한다.
- 운영 조회는 charge의 selected price/rank로 당시 결정 근거를 재구성할 수 있어야 한다.

## 인터페이스와 데이터 변경

### 공개 API

새 endpoint나 응답 필드는 없다. 기존 logical model과 native OpenAI/Gemini 응답을 유지한다. Provider/channel/가격 정보는 client response에 노출하지 않는다.

### 내부 인터페이스

```go
type BoundQuote struct {
    PriceID string
    ChannelID string
    Currency string
    EstimatedCost int64
    MaximumSale int64
    EvaluatedAt time.Time
}

type BeginRequest struct {
    // existing fields
    ExpectedQuote *BoundQuote
    RoutingPolicy string
    CostRank int
}
```

### 데이터베이스 및 migration

- `image_request_charges.routing_policy`
- `image_request_charges.cost_rank`
- 기존 `price_id`, `estimated_cost`, `maximum_sale`, `channel_id`를 bound quote 증거로 재사용한다.
- migration은 nullable/default additive이며 기존 charge는 policy/rank가 비어 있다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-2-cost-routing` initiative에서 동일 logical model에 복수 candidate와 겹치는 유효기간의 exact channel price를 publish한다. Gateway는 price ID나 channel ID를 client에게 노출하지 않는다. Cloud는 `lowest_cost` route를 활성화하기 전에 각 candidate의 credential, price, spend cap과 최소 마진을 준비한다.

## 보안 및 과금 고려사항

- 가격 비교와 Begin은 동일 selector 및 bound snapshot을 사용해 표시된 선택 원가와 실제 reserve 원가가 달라지지 않게 한다.
- minimum margin을 통과하지 못한 저가 sale channel은 선택하지 않는다.
- candidate-specific 실패만 fallback하며 customer/global 오류를 비용 우회로 숨기지 않는다.
- quote와 routing log에 prompt, file metadata, API Key, credential, Wallet balance, quota limit과 cap remaining을 포함하지 않는다.
- price mutation retry는 최대 1회이며 Provider dispatch와 금전 effect 전에만 수행한다.
- terminal idempotency replay는 현재 가격과 route ordering을 다시 평가하거나 소비하지 않는다.

## 테스트 계획

### 단위 테스트

- lowest cost ordering과 cost/priority/candidate-ID tie-break
- fixed/priority policy 회귀
- 동일 evaluation timestamp 전파
- invalid quote channel/currency/amount/overflow 거부
- price/margin/credential unavailable candidate filtering
- snapshot changed typed 분류와 단 1회 re-evaluation
- routing response/log redaction

### 통합 테스트

- 복수 Provider channel exact prices에서 최저 upstream cost 선택
- sale가 더 낮지만 cost가 높은 candidate 비선택
- bound price ID/cost/sale/evaluated-at과 charge snapshot 일치
- Quote 이후 price version 변경 시 rollback 후 새 최저 cost 재선택
- cheapest spend cap exhausted 시 next-cheapest dispatch와 정확한 charge
- 모든 candidate exhausted/unpriced 시 Provider·Wallet·quota·cap effect 0
- idempotency replay가 가격 변경 후에도 원래 response/charge 유지
- OpenAI 생성·편집과 Gemini routing parity

### 호환성 및 장애 테스트

- policy 미변경 fixed/priority model wire behavior 회귀
- 가격 유효기간 경계와 transaction cancellation
- Gateway 다중 인스턴스에서 동일 price snapshot binding
- timeout/connection loss에서 post-dispatch fallback 없음
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
go build ./cmd/gateway-provider-credential
make integration-test
```

## 완료 조건

- [ ] lowest_cost가 eligible candidate의 upstream estimated cost 최솟값을 선택함
- [ ] 모든 candidate quote가 동일 evaluation timestamp를 사용함
- [ ] cost tie-break가 priority와 candidate ID로 deterministic함
- [ ] selected quote와 Begin charge의 price/cost/sale/currency가 일치함
- [ ] price snapshot race가 금전 effect 없이 최대 한 번 재평가됨
- [ ] minimum margin, credential과 inactive/unpriced channel이 dispatch 전에 제외됨
- [ ] cheapest cap 소진 시 next-cheapest로 fallback함
- [ ] global/customer 오류와 post-dispatch 실패는 fallback하지 않음
- [ ] terminal replay가 현재 가격과 route를 재평가하지 않음
- [ ] OpenAI 생성·편집과 Gemini가 동일 ordering 계약을 사용함
- [ ] response/log에 내부 가격·마진·credential·request content가 노출되지 않음
- [ ] fixed/priority와 native SDK wire behavior가 회귀하지 않음
- [ ] README와 Cloud handoff가 갱신됨
- [ ] 전체 race/integration/CI 통과
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- affected logical model policy를 기존 `priority` 또는 `fixed`로 되돌린다.
- 이전 binary는 새 nullable charge columns를 무시하며 기존 price/charge/Wallet/Ledger row를 수정하지 않는다.
- 이미 Begin된 charge는 stored channel/price snapshot으로 settlement와 replay를 계속한다.
- rollback 중에도 dispatch 이후 fallback을 활성화하지 않는다.

## 후속 작업

1. weighted routing
2. Provider health score와 circuit breaker
3. lowest latency routing
4. Provider balance polling과 low-balance alert
5. routing decision 조회 API와 Dashboard
