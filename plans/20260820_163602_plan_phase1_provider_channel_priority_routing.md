---
id: gateway-20260820-016
title: Phase 1 Provider Channel Candidates and Priority Routing
status: in_progress
created_at: 2026-08-20T16:36:02+09:00
updated_at: 2026-08-20T16:36:02+09:00
owners:
  - gateway
initiative: phase-1-provider-routing
depends_on:
  - gateway-20260820-007
  - gateway-20260820-010
  - gateway-20260820-014
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Provider Channel Candidates and Priority Routing

## 목적

하나의 native protocol model/capability에 여러 Provider channel 후보를 연결하고, 요청 시점의 활성 후보 중 deterministic fixed 또는 priority 정책으로 정확히 하나를 선택하는 routing foundation을 만든다. 선택된 Provider model/channel을 과금 견적과 executor 호출에 일관되게 사용하고, 기존 단일 route 동작과 native protocol namespace 격리를 보존한다.

## 배경

현재 Capability Registry는 `protocol + model`마다 Provider/channel 하나만 저장한다. 이 구조에서는 복수 credential/channel, priority routing, 가격 routing과 fallback을 표현할 수 없다. 즉시 cross-provider retry를 추가하면 첫 시도의 비용과 Provider outcome이 불명확한 상태에서 두 번째 Provider를 호출할 위험이 있으므로, 먼저 immutable candidate set과 단일 routing decision을 독립적으로 구현해야 한다.

## 범위

- protocol model definition과 Provider channel candidate 분리
- candidate별 provider, provider model, channel ID, enabled, priority
- `fixed`와 `priority` routing policy
- operation/media capability와 protocol 호환성 필터
- deterministic tie-break와 immutable selection result
- OpenAI/xAI image generation/edit 및 Gemini image generation handler 연결
- selected channel 기반 exact price Estimate/Reserve
- selected Provider model을 outbound native request에 적용
- `/v1/models`의 logical model 단일 노출
- disabled/no-compatible candidate의 protocol-native fail-closed 오류
- route decision의 secret-free structured log field
- 기존 단일 candidate registry 호환 및 회귀 테스트

## 제외 범위

- Provider 호출 실패 후 다른 channel로 재시도하는 fallback
- weighted random, lowest-cost, lowest-latency routing
- circuit breaker, health score와 latency telemetry
- 동적 DB/control-plane registry와 hot reload
- 한 Provider의 복수 credential pool
- region, policy, spend limit와 minimum margin 기반 candidate filtering
- request/response protocol conversion
- public routing policy API와 Dashboard

## 설계 및 구현 순서

### 1. Model과 candidate 타입 분리

- logical model은 protocol, public model ID, owner, created와 capabilities를 가진다.
- channel candidate는 stable candidate ID, Provider, provider-native model ID, channel ID, enabled와 priority를 가진다.
- 같은 logical model에 candidate가 하나 이상 필요하며 candidate/channel ID는 registry 전체에서 중복될 수 없다.
- provider/protocol 조합은 OpenAI protocol→OpenAI 또는 xAI, Gemini protocol→Google만 허용한다.

### 2. Routing policy

- `fixed`는 manifest에 지정된 candidate ID 하나만 선택하며 disabled/incompatible이면 unavailable이다.
- `priority`는 enabled 후보를 priority 오름차순, candidate ID 오름차순으로 정렬해 첫 후보를 선택한다.
- capability는 model 수준에서 먼저 검사하고 candidate protocol compatibility를 다시 검사한다.
- selection은 logical model, provider model, Provider, channel, policy와 candidate ID snapshot을 반환한다.

### 3. Registry API와 목록

```go
Resolve(protocol, model, operation, mediaType) (RoutingDecision, error)
List(protocol) []Model
```

- OpenAI `/v1/models`는 candidate 수와 관계없이 logical model을 한 번만 노출한다.
- 다른 protocol의 model은 목록/resolve에 나타나지 않는다.
- caller가 반환 값을 수정해 registry를 변경할 수 없도록 deep copy한다.

### 4. Handler 연결

- handlers는 decision의 channel ID를 Billing Begin에 전달한다.
- decision의 Provider로 executor를 선택하고 provider model을 outbound request에 적용한다.
- OpenAI JSON body는 top-level model string 하나만 provider model로 치환하며 unknown fields와 나머지 raw JSON value를 보존한다.
- multipart edit는 spooled form의 model field만 provider model로 치환하고 file bytes/metadata를 보존한다.
- Gemini path model mapping은 Google outbound path에만 적용하고 client-visible logical path와 fingerprint identity는 유지한다.
- 동일 idempotency request는 최초 선택 channel/provider model을 charge identity로 고정하며 registry 변경 후에도 terminal replay에 Provider를 호출하지 않는다.

### 5. 과금과 관측성

- routing decision은 Estimate/Reserve 전에 한 번만 계산한다.
- active exact price가 없는 candidate는 이 계획에서 다음 후보로 넘어가지 않고 billing unavailable로 실패한다. 가격 기반 candidate filtering은 후속 계획이다.
- logs에는 candidate ID, channel ID, provider와 policy만 기록하고 credential reference는 기록하지 않는다.
- charge의 기존 channel/price snapshot이 routing audit source이며 provider model snapshot 필요 여부는 구현 중 검증한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 native 경로와 schema를 유지한다. `/v1/models`는 logical model을 중복 없이 반환한다.

### 내부 인터페이스

```go
type ChannelCandidate struct {
    ID string
    Provider ProviderID
    ProviderModel string
    ChannelID string
    Enabled bool
    Priority int
}

type RoutingDecision struct {
    Model string
    CandidateID string
    Provider ProviderID
    ProviderModel string
    ChannelID string
    Policy Policy
}
```

기존 `ModelRoute`와 `ResolveProtocol`은 새 model/candidate registry로 교체한다. 공개 Go module 안정성 계약 전이므로 repository 내부 caller를 한 PR에서 원자적으로 변경한다.

### 데이터베이스 및 migration

없음. 초기 candidate manifest는 code-owned immutable registry다. selected channel/price는 기존 image charge에 저장한다. provider model 감사 필드가 반드시 필요하다고 확인되면 별도 change plan을 만든다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 후속 initiative에서 dynamic candidate/channel 관리 API를 소유한다. 현재 Gateway manifest의 candidate ID/channel ID 안정성과 policy semantics를 그대로 사용한다.

## 보안 및 과금 고려사항

- client는 candidate, Provider, channel, priority 또는 provider model을 직접 지정할 수 없다.
- channel 선택과 Billing Begin 사이에 재선택하지 않는다.
- 가격 없는 selected candidate를 다른 candidate로 암묵 전환하지 않는다.
- 이 계획은 Provider 호출 후 fallback하지 않으므로 timeout 이중 실행 가능성을 만들지 않는다.
- provider model rewrite는 credential/prompt/file을 로그하거나 저장하지 않는다.
- Idempotency fingerprint는 logical request identity를 사용하고 charge channel snapshot으로 다른 routing decision과 conflict를 감지한다.

## 테스트 계획

### 단위 테스트

- fixed/priority selection, disabled candidate와 deterministic tie-break
- protocol/Provider/candidate/channel duplicate validation
- capability/media filter와 cross-protocol 격리
- returned decision/list immutability
- JSON/multipart/path provider model rewrite byte·field 보존

### 통합 테스트

- OpenAI logical model→xAI candidate executor/channel price 선택
- priority 변경 fixture의 deterministic selected Provider
- Gemini logical model→Google provider model path와 billing
- price 없음/disabled candidate에서 upstream 미호출
- Idempotency replay가 registry change와 무관하게 단일 charge/effect 유지
- `/v1/models` logical model 단일 노출과 credential filtering

### 호환성 및 장애 테스트

- 기존 gpt-image-1, grok-imagine-image-quality와 gemini-image behavior 회귀
- malformed model rewrite 입력은 Provider 이전 거부
- timeout은 선택 channel charge만 UNKNOWN reconciliation으로 이동
- OpenAI/Gemini native error schema 보존

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

- [ ] logical model과 복수 channel candidate가 분리됨
- [ ] fixed/priority policy가 deterministic 단일 decision을 생성함
- [ ] protocol/capability/disabled candidate가 정확히 필터됨
- [ ] OpenAI/Gemini handlers가 selected Provider/channel/provider model을 사용함
- [ ] selected channel의 exact price로만 Reserve/settlement됨
- [ ] `/v1/models`가 logical model을 중복·cross-protocol 노출하지 않음
- [ ] request rewrite가 prompt/file/unknown field 의미를 보존함
- [ ] timeout에서 fallback 없이 기존 reconciliation 불변 조건을 유지함
- [ ] 전체 race/integration/CI 통과
- [ ] README와 Cloud handoff가 기록됨
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 기존 built-in logical model마다 현재 Provider와 동일한 단일 fixed candidate를 유지해 동작을 되돌린다.
- managed traffic을 중단한 뒤 이전 binary로 rollback하며 charge/Wallet/Ledger/reconciliation data를 수정하지 않는다.
- 이미 선택된 charge는 stored channel/price 기준으로 settlement/replay한다.

## 후속 작업

1. Provider failure taxonomy와 safe priority fallback
2. weighted/lowest-cost routing과 minimum margin candidate filter
3. circuit breaker, health score와 latency routing
4. dynamic Provider channel/credential control plane
5. usage·cost·routing decision 조회 API
