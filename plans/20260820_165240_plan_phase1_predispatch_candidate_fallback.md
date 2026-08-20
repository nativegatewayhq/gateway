---
id: gateway-20260820-017
title: Phase 1 Pre-dispatch Candidate Fallback
status: in_progress
created_at: 2026-08-20T16:52:40+09:00
updated_at: 2026-08-20T16:52:40+09:00
owners:
  - gateway
initiative: phase-1-provider-routing
depends_on:
  - gateway-20260820-016
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Pre-dispatch Candidate Fallback

## 목적

priority routing의 ordered candidate 중 Provider 호출과 Wallet Reserve가 아직 시작되지 않았음이 증명되는 candidate만 건너뛰어 다음 candidate를 선택한다. 활성 credential/executor와 exact price가 준비된 첫 candidate에서 Reserve 후 단 한 번 Provider를 호출하며, 호출 이후에는 fallback하지 않아 이중 생성·이중 과금을 방지한다.

## 배경

계획 #16은 deterministic candidate 하나를 선택하지만 첫 candidate의 credential 또는 가격이 준비되지 않으면 다른 healthy candidate가 있어도 실패한다. 반대로 timeout이나 connection loss 이후 fallback하면 첫 Provider가 이미 이미지를 생성했을 수 있다. 초기 basic fallback은 pre-dispatch eligibility만 다루고 post-dispatch retry는 Provider operation ID와 attempt accounting이 설계된 후 별도 도입해야 한다.

## 범위

- priority candidate의 deterministic ordered enumeration
- fixed policy는 지정 candidate 하나만 반환
- executor 존재 여부와 Provider credential configured preflight
- selected candidate exact price/margin unavailable 시 다음 priority candidate 평가
- Billing Reserve 성공 시 candidate 선택 확정 및 단일 Provider 호출
- 모든 candidate 거부 원인의 stable secret-free category 집계
- OpenAI/xAI image generation/edit 및 Gemini image generation 연결
- logical Idempotency-Key 동시 요청과 terminal replay 보존
- `/v1/models`가 실제 dispatch 가능한 candidate 존재 여부를 반영
- fallback 선택 channel의 charge/price audit
- 선택/skip/final failure structured logs

## 제외 범위

- Reserve 성공 이후 credential race, timeout, connection loss, panic, HTTP 4xx/429/5xx fallback
- 첫 candidate reservation Release 후 새 candidate 재과금
- weighted, lowest-cost, latency/health routing
- circuit breaker와 runtime health score
- 복수 credential pool과 credential rotation snapshot
- dynamic DB registry/control-plane
- cross-protocol conversion

## 설계 및 구현 순서

### 1. Ordered candidate API

- Registry는 protocol/model/capability가 유효하면 policy 순서의 immutable candidate decision 목록을 반환한다.
- fixed는 enabled fixed candidate 최대 하나, priority는 enabled candidate 전체를 priority/candidate ID 순으로 반환한다.
- 기존 `Resolve`는 목록 첫 항목을 반환하는 convenience API로 유지한다.

### 2. Preflight availability

- handler는 Provider별 executor 존재와 credential registry의 `Configured(provider)` snapshot을 확인한다.
- executor/credential 없음은 Provider 호출·Billing Begin 전에 `executor_unavailable` 또는 `credential_unavailable` category로 skip한다.
- credential 원문, env name과 내부 오류 문자열은 log/response에 포함하지 않는다.
- fixed policy는 candidate가 unavailable이어도 다른 candidate를 선택하지 않는다.

### 3. Price eligibility

- Billing에 금전 변경 없이 exact price/margin eligibility를 검사하는 `Quote` boundary를 추가하거나 기존 estimator를 안전하게 노출한다.
- preferred 방식은 candidate별 Quote 후 최종 candidate에 Begin을 한 번 실행하는 것이다.
- Quote와 Begin 사이 price validity race가 발생하면 Begin의 price unavailable은 Provider 미호출 상태이므로 다음 candidate를 다시 평가할 수 있다.
- insufficient funds, tenant unavailable, invalid request와 DB unknown error는 candidate-specific 문제가 아니므로 즉시 종료한다.

### 4. Selection과 Reserve

- first eligible candidate를 골라도 Billing Begin이 성공할 때까지 selection은 잠정 상태다.
- Begin 성공 이후 decision을 고정하고 executor를 정확히 한 번 호출한다.
- Begin 성공 후 발생하는 모든 기존 error/outcome은 Release 또는 reconciliation으로 처리하고 candidate loop로 돌아가지 않는다.
- Idempotency terminal replay는 candidate preflight보다 먼저 가능하도록 logical request lookup/quote orchestration을 구성한다.

### 5. Protocol handlers

- JSON body와 multipart file은 candidate가 확정된 후 provider model로 한 번만 rewrite한다.
- Gemini path provider model도 확정 후 적용한다.
- client-visible response와 fingerprint는 logical request를 유지한다.
- 모든 candidate가 unavailable이면 OpenAI는 `provider_unavailable`, Gemini는 `UNAVAILABLE` native envelope를 반환한다.

### 6. 관측성

- skip log fields: request ID, logical model, candidate ID, channel ID, provider, category.
- 최종 request log에는 selected candidate/channel/provider와 `fallback_depth`를 기록한다.
- prompt, body, file name, credential과 price amount는 log하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

새 경로 없음. 기존 native unavailable/error envelope를 유지한다.

### 내부 인터페이스

```go
Candidates(protocol, model, operation, mediaType) ([]RoutingDecision, error)

type CandidateAvailability interface {
    Configured(provider ProviderID) bool
}

Quote(ctx, BeginRequest) (Estimate, error)
```

Billing Quote는 Wallet/charge/Ledger를 변경하지 않는다. Begin은 기존 원자적 Reserve 계약을 유지한다.

### 데이터베이스 및 migration

없음. 최종 선택 channel/price는 기존 charge row에 저장한다. skipped candidate는 금전 row를 만들지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 managed deployment에서 candidate에 해당하는 Provider credential과 exact price를 함께 publish한다. 누락 시 Gateway는 다음 priority candidate를 선택할 수 있지만 fixed policy는 fail closed한다.

## 보안 및 과금 고려사항

- fallback은 Provider 호출과 Reserve 이전에만 허용한다.
- Quote는 append-only 금전 data를 만들지 않는다.
- Begin 성공 후 candidate 변경을 금지한다.
- client가 retry/fallback depth와 candidate를 지정할 수 없다.
- idempotent terminal replay는 현재 credential/price availability에 의존하지 않는다.
- unknown Provider outcome은 기존 reservation과 reconciliation을 유지하고 fallback하지 않는다.

## 테스트 계획

### 단위 테스트

- fixed/priority candidate ordered enumeration과 deep-copy
- executor/credential skip category와 deterministic fallback depth
- price unavailable/margin violation만 candidate-specific skip
- insufficient funds/tenant/DB error 즉시 종료
- Begin 성공 이후 timeout/HTTP/panic에서 다음 executor 미호출
- JSON/multipart/Gemini provider model은 최종 candidate 값만 사용

### 통합 테스트

- first credential 없음→second candidate exact price Reserve/Capture
- first price 없음→second candidate Reserve/Capture, skipped charge/Ledger 없음
- all unavailable→upstream·Wallet·charge 변경 없음
- fixed unavailable→alternate 미선택
- concurrent same Idempotency-Key의 Provider 단일 호출 또는 stable pending
- terminal replay가 credential/price 제거 후에도 동작
- timeout UNKNOWN에서 second candidate 미호출과 reservation 유지

### 호환성 및 장애 테스트

- built-in single fixed candidate 동작 회귀
- OpenAI JSON/multipart, Gemini native response 보존
- credential preflight와 실제 executor 사이 credential 제거 race는 safe pre-dispatch failure 또는 기존 settlement 경로로 처리
- process restart와 reconciliation 회귀

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

- [ ] fixed/priority ordered candidates가 deterministic함
- [ ] executor/credential unavailable candidate가 Reserve/Provider 전에 skip됨
- [ ] price/margin unavailable candidate만 안전하게 다음 후보로 넘어감
- [ ] insufficient funds/tenant/unknown DB 오류는 fallback하지 않음
- [ ] Begin 성공 이후에는 어떤 Provider 결과에도 fallback하지 않음
- [ ] 최종 candidate channel/price/provider model이 charge와 outbound에 일치함
- [ ] all-unavailable에서 Wallet/Ledger/charge/upstream effect가 없음
- [ ] terminal idempotency replay가 현재 candidate availability와 무관함
- [ ] native protocol response와 기존 reconciliation 불변 조건이 유지됨
- [ ] 전체 race/integration/CI 통과
- [ ] README와 Cloud handoff가 기록됨
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- candidate loop를 중단하고 계획 #16의 첫 decision 선택으로 되돌린다.
- 이전 binary rollback 시 기존 charge/Wallet/Ledger/reconciliation row를 유지한다.
- skipped candidate에는 금전 row가 없으므로 data compensation이 필요하지 않다.

## 후속 작업

1. Provider attempt schema와 known retryable HTTP response fallback
2. circuit breaker와 health score
3. lowest-cost/minimum-margin routing
4. weighted routing과 spend caps
5. dynamic channel/credential control plane
