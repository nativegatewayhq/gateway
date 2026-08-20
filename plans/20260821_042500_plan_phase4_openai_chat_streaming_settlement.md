---
id: gateway-20260821-038
title: Phase 4 OpenAI Chat SSE Streaming and Disconnect Settlement
status: completed
created_at: 2026-08-21T04:25:00+09:00
updated_at: 2026-08-22T10:30:00+09:00
owners:
  - gateway
initiative: phase-4-openai-chat-streaming-settlement
depends_on:
  - gateway-20260821-036
  - gateway-20260821-037
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - cloud
  - conformance
---

# Phase 4 OpenAI Chat SSE Streaming and Disconnect Settlement

## 목적

공식 OpenAI SDK의 `stream=true` Chat Completions를 native SSE wire 그대로 전달하면서, 마지막 usage chunk를 근거로 token 비용을 정확히 정산하고 client disconnect·upstream 단절·usage 유실 시 잔액을 성급히 환불하지 않는 durable streaming 과금 경계를 제공한다.

## 배경

Plan 036은 non-streaming native Chat path를, Plan 037은 최대 비용 선예약과 native usage 기반 정산을 제공한다. Streaming은 헤더가 전송된 뒤 오류 응답으로 되돌릴 수 없고, client가 연결을 끊어도 Provider 생성과 과금이 이미 계속될 수 있다. 또한 OpenAI Chat stream의 usage는 `stream_options.include_usage=true`일 때 마지막 chunk에만 나타날 수 있으므로, Gateway는 요청을 임의 변환하지 않으면서도 관리형 과금에 필요한 usage 계약을 명시적으로 검증해야 한다.

## 범위

- OpenAI Chat Completions `stream=true`와 `text/event-stream` native relay
- billing-required mode의 `stream_options.include_usage=true` 필수 계약
- request bound, output limit, Wallet/quota/spend-cap 선예약과 idempotency fingerprint 재사용
- SSE framing을 손상하지 않는 bounded incremental usage 관찰
- `[DONE]`, terminal usage, Provider EOF와 protocol error 구분
- 성공 stream의 actual usage Capture와 예약 차액 Release
- upstream non-2xx의 기존 snapshot Release
- client disconnect, upstream reset, malformed/missing/excess usage의 durable reconciliation hold
- streaming idempotency의 안전한 재시도 계약
- backpressure, flush, cancellation, timeout과 메모리 상한
- streaming Provider/billing/reconciliation telemetry 및 공식 SDK conformance

## 제외 범위

- OpenAI Responses API streaming
- Gemini/Anthropic streaming protocol
- cross-provider streaming 변환과 fallback
- 이미 종료된 SSE 전체 transcript 저장 및 byte-for-byte replay
- Gateway가 `stream_options`를 자동 삽입하는 요청 변환
- partial token 추정 과금, Gateway tokenizer와 추정 usage
- tool 실행의 별도 외부 비용

## 핵심 결정

### 1. Native stream 계약

- BYOK mode는 `stream=true` 요청을 변형 없이 전달한다.
- billing-required mode는 정확한 정산을 위해 `stream_options.include_usage=true`를 요청 전에 요구한다.
- `Content-Type: text/event-stream`과 허용된 cache/connection 관련 header만 전달하고 SSE `data:` payload 및 `[DONE]` bytes는 재직렬화하지 않는다.
- non-SSE 2xx, 잘못된 event framing과 body 상한 위반은 unknown outcome으로 분류한다.

### 2. Streaming reservation

- Plan 037과 동일한 model input/output limit, immutable token price, minimum margin, Wallet, quota와 spend cap을 사용한다.
- Provider dispatch 전에 최대 input byte bound와 requested output bound를 하나의 transaction에서 Reserve한다.
- terminal idempotency replay는 stream transcript를 재생하지 않는다. 같은 key의 완료된 streaming 요청은 명시적 conflict를 반환하고 과금·Provider 호출을 반복하지 않는다.
- 진행 중 또는 reconciliation 상태의 같은 key도 pending conflict로 차단한다.

### 3. Incremental relay와 usage evidence

- 전체 stream을 메모리에 적재하지 않고 bounded scanner가 SSE event boundary를 관찰하는 동시에 원본 bytes를 client에 flush한다.
- JSON decode는 `data:`가 단일 JSON object인 terminal usage candidate에만 적용하며 choices/content/tool arguments를 저장하거나 로그하지 않는다.
- 정확히 하나의 유효한 terminal usage를 허용하고 prompt/cached/completion token을 Plan 037 상한과 대조한다.
- usage evidence에는 token count, schema version과 terminal event digest만 저장하며 prompt, completion과 전체 transcript는 저장하지 않는다.

### 4. 종료 상태와 정산

- valid usage와 정상 Provider EOF/`[DONE]`가 확인되면 client 연결 상태와 무관하게 idempotent Capture를 시도한다.
- client disconnect는 Provider request를 즉시 취소하되 이미 발생한 upstream 비용이 불명확하므로 valid terminal usage가 없으면 Release하지 않는다.
- upstream timeout/reset, malformed event, missing/duplicate/excess usage, flush/write failure는 예약을 유지하고 `RECONCILING`으로 기록한다.
- Chat Completions 결과 조회 API가 없으므로 Provider 요청을 자동 재실행하지 않으며 bounded attempt 후 manual review로 전환한다.
- headers 전송 후 발생한 Gateway 오류는 새 JSON body를 쓰지 않고 stream을 종료하며 내부 상태와 telemetry로만 기록한다.

### 5. Backpressure와 자원 제한

- relay는 client write가 느릴 때 bounded buffer 이상 선행 read하지 않는다.
- per-event 및 cumulative observed bytes 상한, Provider idle timeout과 전체 request timeout을 분리한다.
- `http.Flusher`가 없는 writer는 dispatch 전에 거부한다.
- goroutine, response body와 Provider connection은 모든 disconnect/error path에서 종료되며 detached 무제한 drain을 금지한다.

### 6. Observability와 운영 계약

- first-byte latency, stream duration, terminal category, disconnect side와 billing transition만 bounded label로 기록한다.
- model, tenant, token count, 금액, event/body, idempotency key와 charge ID는 label 또는 log에 넣지 않는다.
- reconciliation backlog는 non-streaming과 streaming reason을 구분하되 prompt나 Provider raw error를 저장하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

기존 endpoint를 유지한다.

```text
POST /v1/chat/completions
Content-Type: application/json
Idempotency-Key: optional caller key
```

관리형 streaming request는 다음을 포함해야 한다.

```json
{
  "stream": true,
  "stream_options": {"include_usage": true},
  "max_completion_tokens": 1024
}
```

### 내부 인터페이스

- streaming-aware Chat executor와 relay result
- `chatbilling.CompleteStreamUsage` 또는 operation-key가 분리된 exactly-once settlement
- terminal usage observer와 bounded SSE parser
- streaming reconciliation reason taxonomy
- non-replayable terminal idempotency outcome

### 데이터베이스 및 migration

- Chat charge에 delivery mode와 replay policy를 immutable identity로 추가
- streaming terminal usage evidence digest와 completion marker
- reconciliation에 disconnect side와 bounded terminal category 추가
- 기존 non-streaming charge와 worker가 읽을 수 있는 additive migration만 허용

## 보안 및 과금 고려사항

- SSE content, tool arguments와 prompt를 영속화하거나 구조화 로그에 기록하지 않는다.
- client disconnect를 무료 요청으로 취급하지 않으며 Provider outcome이 불명확하면 reservation을 보존한다.
- 사용자가 위조한 downstream event는 존재하지 않으며 usage는 고정 upstream TLS 연결에서 읽은 Provider bytes만 신뢰한다.
- terminal usage가 예약 상한을 넘으면 Wallet Capture를 하지 않고 manual review로 보낸다.
- 동일 idempotency key의 stream/non-stream request 전환과 body 차이는 fingerprint conflict다.
- Provider credential과 upstream authorization header는 client response 및 telemetry에 노출하지 않는다.

## 테스트 계획

### 단위 테스트

- `stream` 및 `include_usage` strict parsing, duplicate key와 잘못된 타입
- split/coalesced CRLF SSE frames, multiline data, comments와 `[DONE]`
- missing/duplicate/malformed/negative/excess terminal usage
- writer flush failure, client cancellation, upstream timeout/reset
- bounded memory와 header allowlist

### 통합 테스트

- PostgreSQL Reserve 후 successful terminal usage Capture/차액 Release
- disconnect before/after terminal usage의 서로 다른 settlement 상태
- same idempotency key의 zero redispatch와 non-replayable completion conflict
- quota/spend cap atomic rollback과 concurrent Wallet safety
- crash/restart 후 reconciliation retry 및 manual-review convergence
- non-streaming Plan 037과 streaming charge의 tenant isolation

### 호환성 및 장애 테스트

- OpenAI Python `client.chat.completions.create(..., stream=True, stream_options={"include_usage": True})`
- OpenAI JavaScript async iterator와 AbortController
- slow consumer, immediate disconnect, mid-event reset, missing `[DONE]`, oversized event
- upstream 400/401/429/500 native error body
- image, Replicate/fal async Job, non-streaming Chat 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAIStreamingSDKs
```

## 완료 조건

- [x] 공식 OpenAI Python/JavaScript SDK가 native SSE Chat stream을 소비함
- [x] Provider dispatch 전 최대 비용이 Wallet/quota/spend cap에 원자적으로 예약됨
- [x] terminal usage가 actual token 금액으로 exactly-once Capture되고 차액이 반환됨
- [x] client disconnect와 upstream unknown outcome이 자동 Release되지 않음
- [x] missing/invalid/excess usage가 durable reconciliation 후 manual review로 수렴함
- [x] streaming idempotency가 Provider 재호출과 Ledger 중복을 방지함
- [x] relay가 원본 SSE payload를 변형하지 않고 bounded backpressure로 동작함
- [x] header 전송 후 오류가 JSON/SSE wire를 오염시키지 않음
- [x] secret, prompt, completion, raw event와 high-cardinality 과금 identity가 로그·telemetry에 없음
- [x] 전체 unit/race/integration/공식 SDK/장애 테스트가 통과함
- [x] README와 Dashboard/Cloud/Conformance handoff가 갱신됨

## 검증 증거

- 구현 및 머지: `71e05da` (`feat: settle OpenAI Chat streams`, PR #44)
- `make check`
- `TEST_DATABASE_URL=... TEST_REDIS_URL=... make integration-test`
- `go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAIStreamingSDKs`
- fresh PostgreSQL schema에서 migration 000001~000032 반복 적용
- streaming terminal usage Capture, transcript 비저장, non-replayable idempotency 통합 테스트
- settlement failure task의 restart-safe worker Capture와 Ledger exactly-once 통합 테스트
- client write failure, missing/duplicate usage, CRLF wire preservation과 idle timeout 장애 테스트

## Rollback 계획

- billing-required mode에서 `stream=true`를 다시 fail closed하고 non-streaming Plan 037 route를 유지한다.
- 신규 streaming reservation과 reconciliation을 drain 또는 manual review한 뒤 worker를 중단한다.
- additive schema와 usage evidence는 감사 보존 기간 동안 삭제하지 않는다.

## 후속 작업

- OpenAI Responses API non-streaming 및 streaming
- tool calling 완전성·usage conformance
- Gemini native streaming
- Anthropic Messages 및 SSE streaming
- cross-provider LLM routing/fallback
