---
id: gateway-20260822-048
title: Phase 4 OpenAI-protocol LLM Routing and Pre-dispatch Fallback
status: completed
created_at: 2026-08-22T04:00:00+09:00
updated_at: 2026-08-22T06:30:00+09:00
owners:
  - gateway
initiative: phase-4-openai-protocol-llm-routing-fallback
depends_on:
  - gateway-20260820-016
  - gateway-20260820-017
  - gateway-20260820-024
  - gateway-20260820-025
  - gateway-20260820-026
  - gateway-20260821-036
  - gateway-20260821-037
  - gateway-20260821-038
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 4 OpenAI-protocol LLM Routing and Pre-dispatch Fallback

## 목적

OpenAI Chat Completions 네이티브 요청을 동일한 wire protocol을 지원하는 OpenAI 및 xAI 채널 사이에서 capability, health, credential, spend cap, 가격과 정책에 따라 선택하고, Provider에 요청을 보내기 전의 확정적 실패에만 안전하게 fallback한다.

## 배경

Phase 4는 OpenAI Chat Completions의 non-streaming, token 정산과 SSE 정산을 완성했지만 현재 모델 registry는 논리 모델을 단일 Provider 실행기로 연결한다. 이미지 operation에는 fixed, priority, weighted, lowest-cost와 pre-dispatch fallback이 있으나 LLM 가격은 input, cached-input, output 축과 최대 token bound를 사용하므로 이미지 단가 router를 그대로 사용할 수 없다.

또한 Provider가 요청을 수신한 뒤의 retry는 응답 유실과 이중 비용을 만들 수 있다. 따라서 첫 단계는 요청·응답 변환이 필요 없는 OpenAI protocol 호환 채널에 한정하고, reserve 전에 결정 가능한 실패만 다음 후보로 넘긴다. Gemini 및 Anthropic protocol 변환은 손실 없는 공통 표현과 conformance 기준을 별도 계획으로 정의한다.

## 범위

- OpenAI `POST /v1/chat/completions`용 versioned logical-model route registry
- OpenAI와 xAI의 OpenAI-compatible Chat executor 및 fixed trusted origin
- candidate별 provider model, channel, priority, weight, enabled와 capability 선언
- fixed, priority, weighted, lowest-cost routing
- non-streaming 및 streaming capability, tool calling, JSON response mode의 exact-match 필터
- credential, circuit, spend cap, executor와 exact price가 reserve 전에 없을 때 다음 후보 선택
- input/cached-input/output 최대 견적을 같은 evaluation time에서 비교하는 LLM lowest-cost ordering
- 선택한 candidate와 price evidence를 chat charge/idempotency identity에 영속화
- streaming과 non-streaming의 동일 route 선택 및 native response pass-through
- route 선택/제외/fallback depth의 bounded telemetry
- 공식 OpenAI Python/JavaScript SDK로 OpenAI 및 xAI 호환 mock conformance

## 제외 범위

- OpenAI Chat을 Gemini generateContent 또는 Anthropic Messages로 변환
- OpenAI Responses의 cross-provider 변환 또는 routing
- Provider dispatch 이후 429, 5xx, timeout, EOF, malformed body에 대한 자동 retry/fallback
- 한 요청을 여러 Provider에 동시에 보내는 hedge/speculative execution
- 응답 품질 기반 routing, semantic cache와 prompt rewriting
- 실제 Provider API를 CI에서 호출하거나 Provider credential을 fixture에 저장
- 관리 API 또는 Dashboard에서 route를 동적으로 편집하는 기능

## 핵심 결정

### 1. Protocol-preserving candidate boundary

- route key는 `protocol=openai + operation=chat.completions + logical model + required capabilities`이다.
- 초기 candidate provider는 OpenAI-compatible Chat wire 계약을 통과한 OpenAI와 xAI로 제한한다.
- inbound JSON과 successful upstream JSON/SSE bytes를 재직렬화하거나 공급자 간 공통 모델로 변환하지 않는다.
- candidate의 `provider_model`만 outbound request의 top-level `model` 값에 bounded JSON token rewrite로 치환한다. 다른 필드와 순서는 보존한다.
- 알려지지 않은 요청 필드는 삭제하지 않으며 capability를 입증하지 못한 candidate는 선택하지 않는다.

### 2. Pre-dispatch-only fallback

다음 실패만 Provider에 body byte가 전송되지 않았음을 보장할 수 있을 때 후보 제외로 처리한다.

- disabled 또는 capability mismatch
- circuit open 또는 half-open permit 획득 실패
- credential 없음, 폐기 또는 decrypt 실패
- Provider channel spend cap 소진
- executor 미구성
- exact active price/model limit 없음
- reserve 전 price race 또는 route configuration 불일치

DNS/TLS/connect/write/read timeout, HTTP status 수신, SSE header/body 수신 이후에는 다른 후보를 호출하지 않는다. 호출 여부가 불확실하면 기존 stream/non-stream reconciliation로 전이한다.

### 3. Deterministic policy semantics

- `fixed`: 지정된 단일 candidate만 평가하며 대체 후보를 사용하지 않는다.
- `priority`: 낮은 priority 값, candidate ID 순으로 평가한다.
- `weighted`: canonical candidate order와 cryptographic entropy로 하나를 뽑고, pre-dispatch 제외 시 남은 weight로 다시 표본화한다.
- `lowest_cost`: 동일한 request bound에서 `maximum_cost`, priority, candidate ID 순으로 정렬한다.
- 모든 비교는 하나의 `evaluation_at`과 candidate별 immutable price ID를 사용한다.
- 동률 규칙과 weighted interval은 인스턴스 및 재시작에 관계없이 동일하다.

### 4. Billing and idempotency identity

- reserve는 최종 선택 candidate에 대해서만 정확히 한 번 생성한다.
- charge에는 candidate ID, provider, channel ID, provider model, routing policy, rank, evaluation time와 price evidence를 기록한다.
- 같은 Idempotency-Key replay는 저장된 candidate/price/response 또는 terminal state를 사용하며 현재 route를 다시 평가하지 않는다.
- 같은 key로 logical model, stream mode, capability 요구 또는 request body가 달라지면 conflict를 반환한다.
- reserve 성공 뒤 발생한 credential rotation, transport 오류와 downstream disconnect는 다음 candidate로 fallback하지 않는다.

### 5. Capability extraction

- request를 실행하기 전에 bounded JSON scanner로 `stream`, `tools`, `tool_choice`, `response_format`과 최대 output token 필드를 추출한다.
- route candidate는 최소 `streaming`, `tools`, `json_mode` capability를 명시한다.
- 알 수 없는 capability 요구는 관대한 fallback 대신 명시적 unsupported 오류로 fail closed한다.
- non-streaming과 streaming은 같은 logical route를 사용하되 streaming 미지원 candidate는 사전에 제외한다.

## 설계 및 구현 순서

1. `operations/chat` registry를 logical route/candidate/policy/capability 구조로 확장하고 정적 validation을 추가한다.
2. LLM maximum quote comparator와 weighted sampler를 이미지 구현에서 독립된 공통 불변식으로 구현한다.
3. xAI Chat executor를 fixed origin, credential resolver, timeout 및 redirect 차단 규칙으로 추가한다.
4. OpenAI Chat handler 앞단에 bounded capability extraction과 healthy candidate filtering을 연결한다.
5. credential, health, spend cap, executor, exact price의 pre-dispatch 평가와 정책별 선택을 구현한다.
6. `chat_request_charges`에 route 및 quote evidence를 additive migration으로 저장한다.
7. idempotency replay가 저장된 route identity를 고정하도록 billing transaction을 확장한다.
8. 선택된 provider model만 안전하게 치환하고 기존 non-stream/stream execution 및 settlement로 전달한다.
9. route/fallback telemetry와 redaction을 추가한다.
10. 단위, PostgreSQL/Redis 통합, fault injection과 공식 SDK conformance를 추가한다.

## 인터페이스와 데이터 변경

### 내부 route 계약

```go
type Candidate struct {
    ID            string
    Provider      providercredentials.Provider
    ProviderModel string
    ChannelID     string
    Priority      int
    Weight        uint32
    Enabled       bool
    Capabilities  Capabilities
}

type Requirements struct {
    Streaming bool
    Tools     bool
    JSONMode  bool
}
```

Registry는 요청 protocol과 logical model을 받아 ordered candidate snapshot을 반환한다. Handler는 registry 내부 slice를 수정할 수 없다.

### 데이터베이스

additive migration은 `chat_request_charges`에 다음 evidence를 추가한다.

- `candidate_id`
- `provider`
- `provider_model`
- `routing_policy`
- `route_rank`
- `price_evaluated_at`
- `route_evidence_version`

기존 단일 OpenAI route charge는 명시적인 legacy fixed candidate로 읽을 수 있어야 한다. append-only ledger row와 기존 response snapshot은 변경하지 않는다.

### 공개 API

client endpoint, 인증과 성공 wire 형식은 바뀌지 않는다. logical model은 `/v1/models`에 OpenAI protocol Chat capability로 노출한다. 모든 후보가 제외되면 credential, price와 내부 topology를 노출하지 않는 기존 OpenAI error envelope을 반환한다.

## 멀티레포 계약

- `conformance`: 같은 initiative로 OpenAI Python/JavaScript SDK의 logical model, streaming, tools, xAI-compatible fixture와 no-post-dispatch-fallback을 검증한다.
- `cloud`: route manifest와 channel credential/price 배포 계약을 정의하되 Gateway DB나 secret 원문을 직접 수정하지 않는다.
- `dashboard`: 후속 read model에서 선택 provider, policy, rank와 제외 사유의 안전한 집계를 표시한다. credential 및 raw prompt/response는 표시하지 않는다.

## 보안 및 과금 고려사항

- Provider origin은 코드 또는 검증된 allowlist에 고정하며 candidate 설정의 임의 URL을 사용하지 않는다.
- inbound service key를 제거하고 선택 channel credential만 outbound 요청에 주입한다.
- credential, prompt, tool arguments, response content와 SSE data는 log/metric/trace에 기록하지 않는다.
- body 전송 여부를 확정할 수 없는 오류는 post-dispatch로 취급해 fallback을 금지한다.
- price lookup과 reserve는 같은 DB transaction/evaluation evidence를 사용해 가격 race를 감지한다.
- 후보 평가 중에는 Wallet reservation을 만들지 않으며 최종 candidate에만 reservation을 생성한다.
- circuit half-open permit은 선택되지 않은 후보에 대해 반드시 release한다.
- topology를 추론하기 어려운 bounded error category만 client와 telemetry에 노출한다.

## 테스트 계획

### 단위 테스트

- route validation, immutable snapshot과 duplicate candidate/channel 거부
- fixed/priority/weighted/lowest-cost ordering과 deterministic tie-break
- input/cached-input/output maximum quote 비교 및 overflow 거부
- streaming/tools/JSON mode capability exact-match
- provider model bounded rewrite와 unknown field byte preservation
- replay identity 및 request fingerprint conflict

### 통합 테스트

- OpenAI와 xAI candidate별 price/channel/credential 선택
- circuit open, spend cap, credential/price 없음의 reserve 이전 fallback
- 선택 후보 하나에만 Wallet reserve 및 charge 생성
- concurrent same-key 요청의 single route/single charge/replay
- route price race에서 다른 candidate 선택 또는 안전한 실패
- streaming terminal capture와 disconnect reconciliation이 선택 route evidence를 유지
- Provider가 status/header/body를 관찰한 뒤에는 429/500/timeout에서도 두 번째 executor 미호출
- half-open permit과 spend reservation 누수 없음

### SDK 및 회귀 테스트

- OpenAI Python 및 JavaScript SDK non-streaming/streaming logical model 호출
- tools와 JSON mode 지원/미지원 candidate 선택
- OpenAI/xAI-compatible native error 및 response pass-through
- 기존 single-provider OpenAI Chat/Responses, Gemini와 Anthropic 회귀
- `make check`, race test와 fresh PostgreSQL/Redis integration suite

## 완료 조건

- [x] logical OpenAI Chat model이 OpenAI/xAI 호환 candidate를 정책에 따라 선택함
- [x] fixed, priority, weighted, lowest-cost 정책과 tie-break가 자동 테스트로 고정됨
- [x] capability, health, credential, spend cap과 price 실패가 reserve 전에만 fallback됨
- [x] dispatch 이후 오류와 disconnect에서 두 번째 Provider 호출이 없음
- [x] 최종 candidate에만 reserve/capture/release/reconciliation이 exactly once 적용됨
- [x] idempotency replay가 최초 route/price identity를 유지함
- [x] native JSON/SSE와 tool calling 데이터가 손실 없이 전달됨
- [x] route evidence와 bounded telemetry가 저장되고 secret/content가 유출되지 않음
- [x] 공식 OpenAI Python/JavaScript SDK 및 전체 회귀 테스트가 통과함
- [x] README, migration, multi-repo handoff와 검증 증거가 갱신됨

## 검증 증거

- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과: gofmt, vet, 전체 race test와 모든 binary build
- fresh PostgreSQL `gateway_plan048`, Redis DB 12에서 `GOFLAGS=-p=1 make integration-test` 전체 통과
- `go test -tags=sdkconformance ./protocols/openai ./protocols/gemini ./protocols/anthropic -count=1` 통과
- OpenAI Python/JavaScript SDK의 logical model non-streaming 및 streaming 요청이 xAI-compatible candidate로 전달됨
- fixed/priority/weighted 재표본화/lowest-cost tie-break, capability·credential·health·price·spend-cap fallback fixture 통과
- Provider 500 및 streaming client disconnect 이후 두 번째 executor 미호출 fixture 통과
- migration `000042` fresh 적용, immutable route evidence와 original-route idempotency replay 통과
- 12개 동시 동일 idempotency 요청에서 하나의 charge와 Wallet reservation만 생성됨
- spend-cap으로 첫 candidate transaction이 rollback된 뒤 최종 candidate 하나만 영속 예약됨
- 구현 PR 및 GitHub CI run은 PR 생성 후 기록한다.

## Rollback 계획

- logical route를 legacy fixed OpenAI candidate로 되돌리고 multi-candidate manifest를 비활성화한다.
- in-flight charge와 route evidence는 삭제하지 않고 기존 reconciliation worker로 종결한다.
- additive column과 ledger evidence는 유지하며 downgrade 시 읽기만 무시한다.
- xAI Chat credential/channel을 retire해 신규 선택만 차단한다.

## 후속 작업

- OpenAI Responses protocol-compatible provider routing
- OpenAI Chat ↔ Gemini/Anthropic lossless subset translation 계약
- cross-protocol streaming event and tool-call conformance
- Anthropic long-context, service-tier와 server-tool usage pricing
- LLM latency/SLO routing 및 evaluation policy
