---
id: gateway-20260820-011
title: Phase 1 Billable Image Request Lifecycle
status: in_progress
created_at: 2026-08-20T15:11:15+09:00
updated_at: 2026-08-20T15:11:15+09:00
owners:
  - gateway
initiative: phase-1-billable-image-lifecycle
depends_on:
  - gateway-20260820-008
  - gateway-20260820-009
  - gateway-20260820-010
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Billable Image Request Lifecycle

## 목적

인증된 OpenAI/xAI 이미지 생성·편집 요청을 가격 견적, Wallet 예약, Provider 호출, 성공 Capture 또는 실패 Release에 연결한다. 요청별 선택 가격·원가·판매가·정산 상태를 영속화하고 Provider 호출 이전 실패와 호출 이후 정산 실패를 구분해 이중 과금과 미정산 성공을 방지한다.

## 배경

Tenant ownership, Wallet/Ledger와 Provider Pricing이 각각 독립적으로 완성됐다. 현재 native image handler는 인증된 요청을 Provider로 전달하지만 Principal을 버리고 금액 lifecycle을 실행하지 않는다. 유료 MVP를 위해서는 세 도메인을 하나의 명시적인 orchestration 경계로 연결하고, Provider 결과가 성공인지 실패인지에 따라 Wallet 상태를 결정해야 한다.

## 범위

- 인증 Principal의 organization/project를 handler lifecycle에 전달
- 이미지 model route와 Provider channel ID 연결
- OpenAI/xAI JSON generation 및 JSON/multipart edit 가격 selector 사용
- request charge aggregate와 상태 이력
- Estimate→Reserve→Provider→Capture/Release orchestration
- 선택 price/channel, 예상 원가, 예약 판매가, 실제 원가와 capture 판매가 기록
- Provider 2xx 성공과 non-2xx 실패의 명시적 정산
- Provider 호출 전 실패 시 upstream 미호출 보장
- Provider 호출 후 settlement 실패 시 `RECONCILING` 상태와 fail-closed 응답
- 동일 Gateway request ID에 대한 lifecycle operation key 파생
- 기존 native success/error body와 header pass-through 유지
- billing 활성/비활성 deployment mode
- OpenAI/xAI handler 단위·통합·프로세스 테스트

## 제외 범위

- Gemini 이미지 과금: `gateway-20260820-010`에서 selector가 unavailable로 고정됨
- 고객 Idempotency-Key와 완료 응답 replay
- timeout 후 Provider 상태 조회와 자동 reconciliation worker
- cross-provider fallback과 부분 성공
- 실제 Provider usage 기반 가변 capture
- 가격 관리·Wallet 관리 공개 API와 Dashboard
- 결제 processor와 실제 Deposit 검증
- LLM, 영상, 음성 과금

## 설계 및 구현 순서

### 1. Principal 전달

- authentication helper는 성공 시 `apikey.Principal`을 반환하고 handler가 organization/project를 billing coordinator에 전달한다.
- Principal은 request context에 전역 저장하지 않고 명시적 method argument로 전달한다.
- API key ID는 charge record에 저장하지 않아 key rotation과 금전 감사를 분리한다.

### 2. Billable model route

- 이미지 `ModelRoute`에 Provider channel ID를 추가한다.
- channel ID는 가격표와 동일한 canonical ID이며 Provider credential 원문이나 secret reference를 포함하지 않는다.
- billing mode에서 channel 없는 route는 Provider 호출 전에 unavailable 처리한다.
- self-hosted unbilled mode는 기존 Provider native pass-through를 유지한다.

### 3. Request charge schema

- `image_request_charges`: request ID, organization/project, protocol, operation, model, channel/price, quantity/options, estimated cost, reserved/captured sale, actual cost, reservation ID, 상태, timestamps.
- 상태는 `RESERVING`, `RESERVED`, `CAPTURED`, `RELEASED`, `RECONCILING`으로 제한한다.
- 금액은 모두 `USD_TICKS` 정수이고 price ID와 Wallet reservation을 FK로 연결한다.
- organization/project/request ID는 unique이며 다른 payload 재사용을 conflict로 거부한다.
- charge row의 identity와 금액 snapshot은 UPDATE할 수 없고 상태·settlement 필드만 허용된 transition으로 변경한다.

### 4. 원자적 Begin

- Billing coordinator `Begin`은 selector로 Estimate를 구하고 판매가 최대치를 Wallet에 Reserve한다.
- charge 생성과 Wallet reservation은 동일 PostgreSQL transaction에서 commit한다.
- 이를 위해 Ledger가 caller-owned transaction에서 동일 검증·lock order를 재사용할 수 있는 내부 command boundary를 제공한다.
- 가격 없음, margin 위반, insufficient funds와 ownership 실패 시 Provider executor를 호출하지 않는다.
- 같은 organization/request ID와 동일 selector retry는 기존 RESERVED 결과를 반환하고 다른 selector는 conflict다.

### 5. Provider 결과 분류

- HTTP `200..299`는 Provider 성공으로 분류하고 Capture 대상이다.
- 그 외 native Provider response는 실패로 분류하고 예약 전액 Release 대상이다.
- network timeout, cancel, credential unavailable과 executor error도 Release 대상이다.
- Provider response body copy 실패는 이미 Provider가 성공했으면 Capture 결과를 뒤집지 않는다.

### 6. Capture와 Release

- 성공 시 초기 고정 단가 모델은 `actual_cost = estimated_cost`, `captured_sale = reserved_sale`로 Capture한다.
- 실패 시 Wallet reservation을 전액 Release하고 charge를 RELEASED로 전이한다.
- Wallet settlement와 charge 상태 update는 동일 transaction에서 commit한다.
- 같은 settlement operation retry는 단일 ledger effect를 만들고 반대 transition은 거부한다.

### 7. 정산 실패

- Provider 호출 이후 Capture/Release transaction이 실패하면 charge를 가능한 경우 RECONCILING으로 표시한다.
- 성공 upstream body는 정산이 확정되기 전에 고객에게 전달하지 않는다. 정산 실패 시 native body 대신 `503 billing_reconciliation_required`를 반환한다.
- DB 자체가 불가해 RECONCILING 표시도 실패할 수 있는 경우 구조화 로그에 request category만 남기고 동일 request ID 재시도를 허용한다.
- 자동 recovery는 후속 reconciliation 계획이 담당한다.

### 8. 배포 모드

- `GATEWAY_BILLING_MODE=disabled|required`를 추가하고 기본은 self-hosting 호환을 위해 `disabled`다.
- `required`에서는 pricing, channel, Wallet이 모두 준비되지 않으면 readiness 또는 요청을 fail closed한다.
- Cloud 관리형 배포는 `required`만 허용한다는 계약을 README에 기록한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 OpenAI-compatible 경로와 native schema를 유지한다. 과금 단계 오류는 OpenAI error envelope의 안정적인 code로 반환한다.

```text
POST /v1/images/generations
POST /v1/images/edits
```

### 내부 인터페이스

```go
type BeginRequest struct {
    RequestID, OrganizationID, ProjectID string
    Protocol, Operation, Model, ChannelID string
    Quantity int64
    Size, Quality string
}

Begin(ctx context.Context, request BeginRequest) (Charge, error)
Capture(ctx context.Context, chargeID string) (Charge, error)
Release(ctx context.Context, chargeID string) (Charge, error)
MarkReconciling(ctx context.Context, chargeID string, outcome ProviderOutcome) error
```

### 데이터베이스 및 migration

forward-only `000005_image_request_charges.sql`. Wallet/Ledger transaction helper 변경은 기존 공개 command semantics를 유지한다. 이전 binary는 새 table을 무시한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `GATEWAY_BILLING_MODE=required`, active channel/price publication과 충분한 Wallet funding을 배포 전 보장한다. Conformance는 유료 credential 없이 mock Provider/가격/Wallet fixture로 native SDK response compatibility를 검증한다.

## 보안 및 과금 고려사항

- client가 organization, project, channel, price나 금액을 직접 지정하지 못한다.
- selector는 이미 검증한 native body에서 추출하며 prompt와 image bytes를 charge에 저장하지 않는다.
- operation key는 organization/request/transition에서 내부 생성하고 credential이나 payload를 포함하지 않는다.
- Provider 호출은 Reserve commit 이후에만 수행한다.
- Provider 성공은 Capture 확정 전 고객 성공으로 노출하지 않는다.
- non-2xx response도 반드시 Release commit 이후 native response를 전달한다.
- 로그에는 금액 snapshot 전체나 balance를 기본 노출하지 않는다.

## 테스트 계획

### 단위 테스트

- Provider HTTP status outcome 분류
- billing error→HTTP error code mapping
- operation key 결정성과 tenant 분리
- disabled/required mode validation
- selector와 model route channel 불일치 거부

### 통합 테스트

- 충분한 잔액: Estimate→Reserve→2xx→Capture와 charge audit
- Provider 4xx/5xx→Release 및 native error pass-through
- timeout/cancel/credential failure→Release
- insufficient funds/price 없음/channel disabled에서 upstream 미호출
- multipart edit 파일을 보관하지 않고 selector 견적
- 동일 request retry가 이중 Reserve/Capture를 만들지 않음
- settlement DB failure에서 RECONCILING/fail-closed
- charge projection, Wallet과 Ledger delta 일치
- tenant/project mismatch와 disabled tenant 거부

### 프로세스 및 호환성 테스트

- billing disabled에서 기존 SDK/native pass-through 회귀 없음
- billing required에서 OpenAI Python/JavaScript generation/edit mock flow
- xAI JSON generation/edit mock flow
- response byte/header 보존
- migration 반복·동시 실행

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

- [ ] request charge schema와 상태 제약이 존재함
- [ ] Principal organization/project가 모든 charge에 귀속됨
- [ ] active exact price 견적 후에만 Wallet Reserve가 commit됨
- [ ] Reserve 이전에는 Provider가 호출되지 않음
- [ ] Provider 2xx는 Capture, non-2xx와 executor error는 Release됨
- [ ] charge와 Wallet settlement가 transaction 단위로 일치함
- [ ] 성공 응답은 Capture 확정 이후에만 전달됨
- [ ] 정산 불명확 요청은 RECONCILING과 fail-closed로 처리됨
- [ ] 동일 request/transition retry가 이중 과금되지 않음
- [ ] 가격·잔액·tenant 실패에서 upstream 미호출이 검증됨
- [ ] billing disabled mode의 기존 native 호환성이 유지됨
- [ ] billing required mode가 bootstrap/config에 연결됨
- [ ] 전체 race/integration/CI 통과
- [ ] README와 Cloud/Conformance 계약이 기록됨
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Cloud 트래픽을 중단한 뒤 이전 binary로 rollback하고 charge/Wallet/Ledger data는 유지한다.
- 관리형 환경에서 billing mode를 disabled로 낮춰 유료 요청을 무과금 통과시키지 않는다.
- RECONCILING row를 후속 reconciliation 절차로 확인한 뒤 compensating entry를 append한다.
- schema와 금전 row를 drop/update하지 않는다.

## 후속 작업

1. 고객 Idempotency-Key와 완료 response replay
2. timeout/provider job reconciliation worker
3. Gemini 이미지 가격 selector와 billable lifecycle
4. priority/weighted/lowest-cost routing과 fallback
5. usage·cost 관리 API와 Dashboard
