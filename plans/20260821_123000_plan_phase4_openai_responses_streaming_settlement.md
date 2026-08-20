---
id: gateway-20260821-041
title: Phase 4 OpenAI Responses SSE Streaming and Disconnect Settlement
status: in_progress
created_at: 2026-08-21T12:30:00+09:00
updated_at: 2026-08-21T12:45:00+09:00
owners:
  - gateway
initiative: phase-4-openai-responses-streaming-settlement
depends_on:
  - gateway-20260821-038
  - gateway-20260821-039
  - gateway-20260821-040
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - cloud
  - conformance
---

# Phase 4 OpenAI Responses SSE Streaming and Disconnect Settlement

## 목적

공식 OpenAI Python/JavaScript SDK가 `stream=true`인 Responses 요청을 native SSE event stream으로 소비하게 하고, terminal `response.completed`의 native usage를 근거로 비용을 exactly-once 정산하며 disconnect·upstream 단절·terminal event 유실 시 예약을 안전하게 reconciliation으로 전환한다.

## 배경

Plan 039는 Responses non-streaming native facade를, Plan 040은 operation별 immutable 가격과 usage 정산을 제공한다. 공식 OpenAI Responses streaming 계약은 각 SSE frame에 `event`와 `data`를 전달하고, 성공 terminal `response.completed` event의 `response.usage`에 input, cached input, output과 reasoning token usage를 포함한다. Chat streaming과 달리 `[DONE]` sentinel을 과금 완료 근거로 가정할 수 없으며, `response.failed`, `response.incomplete`, `error`와 transport EOF를 서로 다른 결과로 판정해야 한다.

참조 계약: [OpenAI Responses create API](https://developers.openai.com/api/reference/typescript/resources/beta/subresources/responses/methods/create), [OpenAI Responses streaming events](https://platform.openai.com/docs/api-reference/responses-streaming/response/refusal/delta)

## 범위

- `POST /v1/responses`의 `stream=true` native SSE byte relay
- Python sync/async stream과 JavaScript async iterator 공식 SDK 호환
- event name, data framing과 sequence를 변형하지 않는 bounded incremental relay
- Provider dispatch 전 Plan 040 Wallet/quota/spend-cap 예약
- `response.completed.response.usage` strict 추출과 actual usage Capture
- completed response의 input/cached/output/reasoning usage 상한 검증
- confirmed pre-stream non-2xx의 native response 전달과 전액 Release
- `response.failed`, `response.incomplete`, top-level `error`, malformed/duplicate terminal과 unexpected EOF의 명시적 분류
- client disconnect, downstream write/flush 실패, upstream reset/idle timeout의 durable reconciliation hold
- streaming terminal digest evidence와 transcript-free idempotency 계약
- Provider/billing/stream telemetry, README와 공식 SDK conformance

## 제외 범위

- `GET/DELETE /v1/responses/{response_id}`, cancel과 input-items 조회
- `background=true`와 deferred polling/webhook lifecycle
- completed SSE transcript 저장 또는 byte-for-byte replay
- Provider response ID를 이용한 자동 조회·취소·재요청
- built-in web/file/code-interpreter/computer/MCP tool별 별도 가격
- audio/image generation event payload의 Gateway-side 변환 또는 저장
- cross-provider Responses 변환, routing과 fallback
- partial output 추정 과금과 Gateway tokenizer

## 핵심 결정

### 1. Native SSE 계약

- BYOK mode는 `stream=true` 요청 bytes와 Provider SSE bytes를 변형하지 않고 전달한다.
- `Content-Type: text/event-stream`과 안전한 cache 관련 header만 downstream에 허용한다.
- parser는 CRLF/LF, comment, unknown future event와 split/coalesced reads를 허용하되 원본 frame을 재직렬화하지 않는다.
- event payload, output delta, tool arguments, response ID와 reasoning content를 로그 또는 영속 저장하지 않는다.

### 2. Streaming reservation과 request identity

- 관리형 mode는 Plan 040과 동일하게 정확히 하나의 positive `max_output_tokens`와 model limit을 요구한다.
- `stream=true`를 포함한 원본 body, logical model, channel과 `text/event-stream` media identity로 fingerprint를 계산한다.
- Reserve는 Provider 연결 전에 token price, Wallet, quota와 spend cap을 하나의 PostgreSQL transaction으로 고정한다.
- terminal streaming 요청은 transcript를 저장하지 않으므로 같은 idempotency key로 replay하지 않는다. 재사용은 Provider와 Ledger를 다시 호출하지 않고 conflict 또는 pending을 반환한다.

### 3. Terminal event와 usage evidence

- 성공 정산 근거는 정확히 하나의 `response.completed` event와 그 내부 `response.status=completed`, strict `response.usage`다.
- `usage.input_tokens`, optional cached input, `output_tokens`와 optional reasoning tokens는 integer/non-negative이며 cached≤input, reasoning≤output이어야 한다.
- actual usage는 reserved input/output와 maximum sale/cost를 넘지 않아야 한다.
- evidence에는 usage counts, Responses stream schema version, terminal event SHA-256와 delivery mode만 저장한다.

### 4. 실패와 unknown outcome

- HTTP non-2xx가 SSE 시작 전에 확정되면 bounded native snapshot을 저장하고 Release한다.
- `response.failed`, `response.incomplete`와 top-level `error` event는 비용과 usage가 확정됐다고 가정하지 않고 reservation을 유지한다.
- terminal usage 누락/중복/불일치, invalid event JSON, unexpected EOF, client disconnect, write failure, idle timeout과 upstream reset은 `RECONCILING`으로 전환한다.
- 완전한 terminal usage가 관찰된 뒤 downstream write가 실패해도 Capture를 시도할 수 있으며, DB 실패 시 usage와 terminal digest를 durable task로 남긴다.
- Provider 조회 identity가 없는 task는 요청을 자동 재실행하거나 자동 Release하지 않고 bounded retry 후 manual review로 수렴한다.

### 5. Backpressure와 cancellation

- relay는 bounded event buffer와 cumulative observation limit을 사용하며 전체 transcript를 메모리에 적재하지 않는다.
- downstream이 느리면 write 완료 전 무제한 upstream read를 하지 않는다.
- `http.Flusher` 부재는 dispatch 전에 거부한다.
- client cancellation은 Provider context와 response body를 닫고, 분리된 무제한 drain goroutine을 만들지 않는다.
- Provider request timeout과 stream idle timeout을 구분해 설정하고 모든 종료 경로에서 resource를 회수한다.

### 6. Observability와 호환성

- first-byte latency, stream duration, terminal category, disconnect side와 billing transition만 bounded label로 기록한다.
- model, tenant, token/금액, response ID, event type의 무제한 값, idempotency key와 charge ID는 metric label에 넣지 않는다.
- 공식 SDK의 typed event iteration이 유지되도록 event/data field와 unknown event를 pass-through한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/responses
Content-Type: application/json
Authorization: Bearer SERVICE_API_KEY
Idempotency-Key: optional caller key
```

관리형 streaming request 예:

```json
{
  "model": "gpt-4.1",
  "input": "hello",
  "stream": true,
  "max_output_tokens": 1024
}
```

응답은 Provider의 native `text/event-stream`이며 Gateway 과금 metadata를 삽입하지 않는다.

### 내부 인터페이스

- streaming-aware `ResponsesExecutor.Create`
- bounded Responses SSE observer와 terminal classification result
- operation-aware `CompleteStreamUsage` 및 stream reconciliation methods
- transcript-free terminal digest evidence와 non-replayable idempotency outcome

### 데이터베이스 및 migration

- Responses operation도 사용할 수 있도록 streaming charge/evidence 제약을 additive 확장한다.
- terminal schema version과 terminal event digest를 Responses stream에 명시한다.
- 기존 Chat stream과 Responses non-stream charge를 구 binary가 계속 읽을 수 있어야 한다.
- 기존 evidence/원장 row는 변경하거나 삭제하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

- `conformance`: Python/JavaScript Responses streaming SDK와 abort/disconnect fixture
- `cloud`: stream idle timeout, body/event limit과 reconciliation backlog 운영 설정
- `dashboard`: Responses stream charge의 delivery mode와 bounded terminal category 표시

각 저장소는 같은 initiative로 자체 plan을 만들며 Gateway 내부 구현을 공유 계획으로 통제하지 않는다.

## 보안 및 과금 고려사항

- delta, prompt, tool call arguments/results, reasoning, response ID와 전체 transcript를 저장하거나 로그하지 않는다.
- client disconnect와 Provider error event를 무료 실패로 단정하지 않는다.
- usage는 trusted OpenAI TLS response의 terminal event에서만 읽으며 downstream 입력을 신뢰하지 않는다.
- usage가 예약을 넘거나 terminal 의미가 모호하면 Capture/Release 모두 하지 않고 manual review 대상으로 보존한다.
- same key의 stream/non-stream 또는 body 변경은 conflict이며 terminal stream 재사용은 redispatch되지 않는다.
- headers가 전송된 뒤 발생한 내부 오류는 JSON error를 추가해 SSE wire를 오염시키지 않는다.

## 테스트 계획

### 단위 테스트

- strict `stream`/`max_output_tokens` duplicate와 type parsing
- LF/CRLF, comment, multiline data, split/coalesced frame와 unknown future event
- completed/failed/incomplete/error terminal 분류와 sequence disorder
- missing/duplicate/malformed/negative/excess usage 및 reasoning detail
- slow writer, write/flush failure, cancellation, idle timeout과 bounded memory
- safe response headers, terminal digest와 content redaction

### 통합 테스트

- streaming Reserve 후 completed usage의 exact Capture와 차액 반환
- disconnect 전/후 terminal observation의 settlement 차이
- settlement crash 후 restart-safe worker Capture와 Ledger exactly-once
- same-key pending/terminal stream의 zero redispatch와 non-replayable conflict
- quota/spend-cap rollback, concurrent Wallet safety와 operation isolation
- Chat stream 및 Responses non-stream evidence/migration 회귀

### 호환성 및 장애 테스트

- OpenAI Python Responses sync/async streaming iterator
- OpenAI JavaScript Responses async iterator와 AbortController
- upstream 400/401/429/500, response.failed/incomplete/error, reset, missing terminal과 oversized event
- immediate/mid-event/after-terminal client disconnect와 slow consumer
- Chat, Images, Replicate/fal Jobs와 management API 전체 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAIResponsesStreamingSDKs
```

## 완료 조건

- [ ] 공식 OpenAI Python/JavaScript SDK가 native Responses SSE event를 소비함
- [ ] Provider dispatch 전에 Wallet/quota/spend cap 최대 비용이 원자적으로 예약됨
- [ ] valid `response.completed` usage가 exact token 금액으로 exactly-once Capture됨
- [ ] failed/incomplete/error/disconnect/unknown outcome이 자동 Release되지 않음
- [ ] recoverable settlement failure가 worker로 복구되고 나머지는 manual review로 수렴함
- [ ] streaming idempotency가 Provider redispatch와 Ledger 중복을 방지함
- [ ] relay가 native SSE bytes를 변형하지 않고 bounded backpressure로 동작함
- [ ] header 전송 후 오류와 cancellation이 wire/resource를 안전하게 종료함
- [ ] secret, prompt, output, tool/reasoning event와 high-cardinality identity가 로그·telemetry에 없음
- [ ] 전체 unit/race/integration/공식 SDK/장애 회귀가 통과함
- [ ] README와 Dashboard/Cloud/Conformance handoff가 갱신됨
- [ ] 재현 가능한 검증 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- billing-required mode에서 Responses `stream=true`를 다시 pre-dispatch fail closed한다.
- BYOK streaming에 결함이 있으면 Responses `stream=true` 전체를 명시적 unsupported 오류로 되돌린다.
- 신규 streaming reservation/reconciliation을 drain 또는 manual review한 후 worker 경로를 중단한다.
- additive migration과 immutable evidence/Ledger는 감사 보존 기간 동안 유지한다.

## 후속 작업

- Responses retrieve/delete/cancel 및 tenant-safe Provider identity
- Responses background mode와 deferred Job reconciliation
- built-in tool별 usage/cost settlement
- Gemini native streaming
- Anthropic Messages 및 SSE streaming
- cross-provider LLM routing/fallback
