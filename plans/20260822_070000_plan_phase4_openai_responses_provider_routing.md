---
id: gateway-20260822-049
title: Phase 4 OpenAI Responses Protocol-compatible Provider Routing
status: accepted
created_at: 2026-08-22T07:00:00+09:00
updated_at: 2026-08-22T07:00:00+09:00
owners:
  - gateway
initiative: phase-4-openai-responses-provider-routing
depends_on:
  - gateway-20260821-039
  - gateway-20260821-040
  - gateway-20260821-041
  - gateway-20260822-048
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 4 OpenAI Responses Protocol-compatible Provider Routing

## 목적

OpenAI `POST /v1/responses` 요청을 native Responses wire를 지원하는 OpenAI 및 xAI 채널 사이에서 capability, health, credential, spend cap과 token 원가에 따라 선택하고, Provider dispatch 이전의 확정적 실패에만 fallback한다.

## 배경

Plan 039~041은 OpenAI Responses non-streaming, usage 정산과 native SSE를 완성했다. Plan 048은 Chat Completions에 logical route registry, OpenAI/xAI transport, policy selector, immutable route evidence와 pre-dispatch-only fallback을 도입했다. Responses는 같은 OpenAI SDK와 token price 원장을 사용하지만 typed output item, built-in server tool, `previous_response_id`, stored response와 terminal SSE grammar가 Chat과 다르므로 Chat handler로 변환하지 않고 Responses operation 안에서 독립 capability를 평가해야 한다.

xAI 공식 문서는 OpenAI Python/JavaScript SDK가 `https://api.x.ai/v1/responses`를 호출하는 경로, synchronous/streaming Responses와 web/X search, code execution 등 server tools를 문서화한다. 이 계획은 해당 native-compatible create 경로만 사용하며 Provider별 미지원 기능을 추측하지 않는다.

참조 계약:

- [xAI Responses and Chat comparison](https://docs.x.ai/developers/model-capabilities/text/comparison)
- [xAI Inference API Responses endpoint](https://docs.x.ai/developers/rest-api-reference/inference/chat)
- [xAI streaming and synchronous tools](https://docs.x.ai/developers/tools/streaming-and-sync)

## 범위

- Responses logical model route와 OpenAI/xAI candidate registry
- fixed, priority, weighted, lowest-cost policy 및 deterministic tie-break
- xAI `POST https://api.x.ai/v1/responses` fixed-origin executor
- candidate별 provider model, channel, enabled, priority, weight와 capability 선언
- streaming, function tools, web search, X search, code interpreter, image generation, JSON/text format, stored response capability 필터
- unknown built-in tool과 unsupported request option의 pre-dispatch fail-closed
- credential, executor, circuit/half-open permit, exact price와 spend-cap pre-dispatch fallback
- input/cached-input/output maximum quote의 단일 evaluation timestamp 비교
- top-level `model` string만 치환하고 나머지 native request/response/SSE byte 보존
- route-independent request fingerprint와 original-route idempotency replay
- Responses charge의 candidate/provider/model/policy/rank/price evidence
- Provider non-2xx release, terminal usage capture와 불확실 stream reconciliation의 선택 route 유지
- `/v1/models`, bounded telemetry, README와 공식 SDK conformance 갱신

## 제외 범위

- Responses를 Chat Completions, Gemini 또는 Anthropic wire로 변환
- `previous_response_id`가 있는 요청의 cross-candidate routing 또는 Provider affinity 추론
- response retrieve/delete/cancel endpoint와 Provider response-ID ownership table
- `background=true`, deferred completion과 Job reconciliation
- built-in tool 호출량 또는 결과 크기별 별도 과금
- Provider dispatch 이후 429, 5xx, timeout, EOF, malformed JSON/SSE의 retry/fallback
- hedge/speculative execution, quality routing과 latency routing
- Control Plane 또는 Dashboard route 편집 API

## 핵심 결정

### 1. Native Responses boundary

- route key는 `protocol=openai + operation=responses.create + logical model + requirements`이다.
- candidate는 OpenAI Responses create 및 terminal event conformance를 통과한 OpenAI 또는 xAI만 허용한다.
- Gateway는 Chat request/event로 변환하거나 typed output item, reasoning, citations, tool call과 future field를 재구성하지 않는다.
- bounded lexical rewrite로 top-level `model` 문자열만 provider model로 바꾸고 나머지 request bytes를 유지한다.
- Provider JSON, error body와 SSE event bytes는 기존 Responses relay를 그대로 사용한다.

### 2. Exact capability extraction

요청에서 다음 요구를 bounded parser로 추출한다.

- `stream`
- function tools
- `web_search`/`web_search_preview`
- `x_search`
- `code_interpreter`
- `image_generation`
- text/JSON response format
- `store`
- `previous_response_id`
- `background`

unknown tool type 또는 해석하지 못한 capability는 candidate fallback으로 의미를 추측하지 않고 unsupported 오류를 반환한다. `previous_response_id`와 `background=true`는 이 계획에서 명시적으로 거부한다. `store=true`는 candidate가 stored-response capability를 선언한 경우에만 전달한다.

### 3. Routing and fallback

- fixed, priority, weighted와 lowest-cost 의미 및 tie-break는 Plan 048과 동일하다.
- route/policy primitives는 operation-neutral package로 추출할 수 있지만 Chat과 Responses capability type은 분리한다.
- availability, capability, circuit, half-open claim, credential, executor, exact price와 spend-cap 실패만 body dispatch 전 다음 candidate로 전이한다.
- weighted route는 후보 제외마다 남은 weight로 다시 표본화한다.
- HTTP transport가 시작됐거나 status/header/body를 관찰한 뒤에는 다음 candidate를 호출하지 않는다.
- streaming client disconnect도 선택 Provider를 취소하고 reconciliation할 뿐 fallback하지 않는다.

### 4. Billing, replay and evidence

- 모든 quote와 Begin은 하나의 UTC `price_evaluated_at`을 사용한다.
- 최종 candidate transaction만 charge, Wallet, quota와 spend allocation을 commit한다.
- `chat_request_charges`의 route evidence column을 재사용하되 schema version은 `openai-responses-route-v1`로 구분한다.
- route-independent fingerprint는 logical model, delivery mode와 원본 body를 결합한다.
- terminal idempotency replay는 registry, current credential/health/price보다 먼저 조회하고 저장된 response와 route를 유지한다.
- streaming transcript는 기존 정책대로 replay하지 않으며 동일 key는 Provider redispatch 없이 conflict/pending을 반환한다.

### 5. Responses-specific terminal safety

- non-streaming capture는 선택 candidate의 native `usage.input_tokens`, cached details와 `output_tokens`만 사용한다.
- streaming capture는 기존 strict sequence와 단일 `response.completed.response.usage` evidence를 사용한다.
- non-2xx가 확인된 경우만 release한다.
- timeout, reset, invalid/missing usage, incomplete/error event, write failure와 disconnect는 선택 route evidence를 보존한 채 reconciliation한다.

## 설계 및 구현 순서

1. `operations/responses`를 immutable route/candidate/policy/capability registry로 확장한다.
2. Plan 048 policy ordering과 weighted sampling을 operation-neutral helper로 재사용하거나 동등 불변식으로 구현한다.
3. Responses request capability extractor와 top-level model byte rewrite를 추가한다.
4. OpenAI Responses executor를 provider-scoped fixed origin으로 일반화하고 xAI wrapper를 추가한다.
5. handler에 candidate preflight, health permit, policy ordering과 BYOK routing을 연결한다.
6. managed non-stream/stream에 early replay, quote, transactional candidate fallback과 no-post-dispatch 규칙을 연결한다.
7. migration으로 Responses route evidence version을 허용하고 billing insert/load validation을 operation-aware하게 만든다.
8. `/v1/models`, telemetry, configuration JSON과 README를 갱신한다.
9. unit, PostgreSQL/Redis integration, fault injection과 official SDK conformance를 추가한다.

## 인터페이스와 데이터 변경

### 설정

`GATEWAY_OPENAI_RESPONSES_ROUTES_JSON`은 legacy `GATEWAY_OPENAI_RESPONSES_MODELS` 및 model limits를 대체하는 bounded static array다. 기본 legacy 설정은 단일 fixed OpenAI candidate로 계속 동작한다.

candidate capability 예시:

```json
{
  "streaming": true,
  "function_tools": true,
  "web_search": true,
  "x_search": true,
  "code_interpreter": true,
  "image_generation": false,
  "json_mode": true,
  "stored_response": true
}
```

### 데이터베이스

- 기존 route evidence columns와 immutable trigger를 재사용한다.
- `route_evidence_version` constraint가 `openai-chat-route-v1`과 `openai-responses-route-v1`을 operation과 일치하게 허용하도록 additive migration을 추가한다.
- Responses usage/stream evidence schema와 append-only Ledger는 변경하지 않는다.

### 공개 API

endpoint, 인증, 성공/error JSON과 SSE wire는 바뀌지 않는다. logical model은 사용 가능한 candidate가 하나 이상일 때 `/v1/models`에 한 번만 표시한다. 모든 candidate가 제외되면 내부 topology를 노출하지 않는 native unavailable/unsupported envelope을 반환한다.

## 멀티레포 계약

- `conformance`: OpenAI Python sync/async와 JavaScript Responses SDK로 OpenAI/xAI logical route, streaming, function/server-tool fixtures와 no-post-dispatch-fallback을 검증한다.
- `cloud`: versioned route manifest와 candidate channel/price/credential 배포를 소유하고 secret을 manifest, CI log 또는 Terraform state에 기록하지 않는다.
- `dashboard`: route policy, selected provider/rank와 bounded rejection 집계만 표시하고 input/output/tool payload와 credential을 노출하지 않는다.

## 보안 및 과금 고려사항

- origin은 `api.openai.com` 또는 `api.x.ai`로 고정하고 route JSON에서 URL을 받지 않는다.
- service key와 inbound sensitive headers를 제거하고 선택 channel credential만 적용한다.
- input, typed output, reasoning, citations, tool arguments/results와 SSE data를 log/metric/trace에 기록하지 않는다.
- unknown tool 또는 stateful affinity를 임의 provider에 보내지 않는다.
- candidate 평가 중 영속 reservation을 남기지 않으며 실패 transaction은 완전히 rollback한다.
- half-open permit은 선택 실패 시 release하고 dispatch 결과는 정확한 channel에 observe한다.
- idempotency replay가 current route 상태에 종속되지 않도록 billing lookup을 먼저 수행한다.

## 테스트 계획

### 단위 테스트

- route validation, immutable snapshot, duplicate candidate/channel과 invalid Provider 거부
- fixed/priority/weighted/lowest-cost 및 재표본화/tie-break
- 모든 Responses capability 조합과 unknown tool/stateful option 거부
- top-level model만 변경되고 unknown JSON bytes가 보존됨
- OpenAI/xAI fixed origin, credential scope, redirect/timeout/idle timeout
- logical fingerprint와 route evidence validation

### 통합 테스트

- credential/circuit/half-open/executor/price/spend-cap candidate fallback
- quote/Begin evaluation timestamp와 lowest-cost ordering
- 실패 candidate transaction rollback 및 최종 route 단일 reservation
- concurrent same-key single charge/route/replay
- terminal route replay가 current registry/credential/health/price 없이 성공
- non-stream and stream usage capture/release/reconciliation에 route evidence 유지
- Provider 429/500/timeout 및 stream disconnect 이후 두 번째 executor 미호출
- migration의 legacy charge 호환성과 route evidence immutability

### SDK 및 회귀 테스트

- OpenAI Python sync/async와 JavaScript Responses non-streaming/streaming logical model 호출
- xAI-compatible function tool와 server-tool native item/event fixture
- OpenAI Chat routing, legacy Responses, Gemini와 Anthropic regression
- `make check`, fresh PostgreSQL/Redis integration suite와 SDK conformance

## 완료 조건

- [ ] logical Responses model이 OpenAI/xAI candidate를 네 정책으로 선택함
- [ ] Responses capability가 exact-match되고 unknown/stateful unsupported 요청이 fail closed함
- [ ] xAI fixed-origin Responses non-stream/stream transport와 credential scope가 검증됨
- [ ] 모든 fallback이 pre-dispatch에 한정되고 dispatch 이후 두 번째 호출이 없음
- [ ] 최종 candidate만 exactly-once 예약·정산되고 immutable route evidence가 저장됨
- [ ] idempotency replay가 current route 평가 없이 original route/result를 유지함
- [ ] native typed JSON/SSE/tool/reasoning data가 손실 없이 전달됨
- [ ] route/health/billing telemetry가 bounded되고 content/secret이 유출되지 않음
- [ ] 공식 SDK와 전체 unit/race/integration 회귀가 통과함
- [ ] README, migration, multi-repo handoff와 검증 증거가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- `GATEWAY_OPENAI_RESPONSES_ROUTES_JSON`을 제거하고 legacy fixed OpenAI Responses 설정으로 되돌린다.
- xAI Responses channel을 retire해 신규 선택을 차단한다.
- in-flight route charge는 기존 worker/manual review로 종결한다.
- additive evidence와 Ledger row는 삭제하지 않고 downgrade consumer가 무시하게 한다.

## 후속 작업

- Responses response-ID Provider affinity와 retrieve/delete/cancel
- Responses background/deferred Job reconciliation
- built-in server-tool usage/cost settlement
- OpenAI Chat/Responses ↔ Gemini/Anthropic lossless subset translation
- cross-protocol streaming/tool conformance
