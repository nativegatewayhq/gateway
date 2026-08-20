---
id: gateway-20260821-044
title: Phase 4 Gemini Native SSE Streaming and Disconnect Settlement
status: completed
created_at: 2026-08-21T19:00:00+09:00
updated_at: 2026-08-21T21:00:00+09:00
owners:
  - gateway
initiative: phase-4-gemini-streaming-settlement
depends_on:
  - gateway-20260821-038
  - gateway-20260821-041
  - gateway-20260821-042
  - gateway-20260821-043
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - cloud
  - conformance
---

# Phase 4 Gemini Native SSE Streaming and Disconnect Settlement

## 목적

공식 Google Gen AI Python·JavaScript SDK가 `models.streamGenerateContent`를 native Gemini SSE로 소비하게 하고, 마지막 유효 응답의 누적 `usageMetadata`로 비용을 exactly-once 정산하며 client disconnect·upstream 단절·terminal usage 유실 시 예약을 durable reconciliation으로 보존한다.

## 배경

Plan 042는 Gemini `generateContent` native facade를, Plan 043은 non-streaming token 예약과 정산을 제공한다. Gemini streaming은 `alt=sse` query와 JSON response chunk sequence를 사용하며 OpenAI의 `[DONE]` 또는 named terminal event가 없다. 따라서 transport EOF만으로 성공을 선언해서는 안 되고, 각 chunk의 strict schema, 누적 usage 단조성, finish reason과 최종 usage 존재 여부를 함께 판정해야 한다.

참조 계약: [Gemini streamGenerateContent](https://ai.google.dev/api/generate-content#method:-models.streamgeneratecontent), [Gemini UsageMetadata](https://ai.google.dev/api/generate-content#UsageMetadata), [Google Gen AI SDK 문서](https://googleapis.github.io/python-genai/)

## 범위

- `POST /v1beta/models/{model}:streamGenerateContent?alt=sse` native endpoint
- Google Gen AI Python sync/async 및 JavaScript async iterator 호환
- SSE comment, CRLF/LF, split/coalesced reads와 future field를 허용하는 byte-preserving bounded relay
- 관리형 요청의 Plan 043 가격·model limit·Wallet/quota/spend-cap 사전 예약
- chunk별 strict `usageMetadata` 관찰과 최종 누적 prompt/cached/tool-use/candidate/thought usage Capture
- finish reason과 clean EOF를 결합한 terminal classification
- confirmed pre-stream non-2xx native snapshot과 전액 Release
- client cancellation, downstream write/flush 실패, malformed/oversized chunk, upstream reset/idle timeout과 usage 유실의 reconciliation hold
- transcript-free streaming idempotency, terminal digest evidence와 worker exactly-once 복구
- stream telemetry, README와 공식 SDK conformance

## 제외 범위

- Gemini Live API와 WebSocket
- `countTokens`, Cached Content 생성·조회와 tokenizer 기반 exact preflight
- 전체 SSE transcript, prompt, candidate 또는 thought content 저장·replay
- partial output 추정 과금과 usage 없는 자동 Release
- cross-provider protocol 변환, routing과 fallback
- grounding/search/code execution 등 tool별 별도 가격
- Dashboard/Cloud/Conformance 저장소 내부 구현

## 핵심 결정

### 1. Native endpoint와 wire 계약

- path action은 정확히 `streamGenerateContent`이고 managed/BYOK 모두 `alt=sse`만 허용한다.
- 요청 body는 `generateContent`와 같은 native JSON이며 관리형 mode에서는 positive integer `generationConfig.maxOutputTokens`가 필수다.
- Provider의 status, safe headers와 SSE bytes를 재직렬화하거나 Gateway metadata를 삽입하지 않고 전달한다.
- parser는 SSE framing만 side-channel로 관찰하며 unknown JSON field와 future enum은 relay를 막지 않는다.

### 2. Reservation과 streaming identity

- request bytes, model, channel, action, normalized query와 streaming media identity로 fingerprint를 만든다.
- Plan 043과 동일한 immutable Gemini token price 및 input byte/output token 상한으로 Provider 연결 전에 원자적 Reserve한다.
- stream/non-stream과 model/body/query가 다른 same idempotency key는 conflict다.
- transcript를 저장하지 않으므로 terminal stream 재사용은 redispatch하지 않고 non-replayable conflict를 반환한다.

### 3. Usage와 terminal 판정

- 각 data event는 정확히 하나의 JSON object이며 duplicate key, invalid numeric type과 음수 usage를 거부한다.
- usage가 여러 chunk에 나타나면 cumulative count가 component별로 감소하지 않아야 하며 마지막 valid usage만 정산 근거로 사용한다.
- cached는 prompt 이하이고 billable input은 prompt+tool-use, billable output은 candidates+thoughts다. `totalTokenCount`는 Google 계약에 따라 prompt+candidates+thoughts와 일치해야 한다.
- 성공 terminal은 valid final usage, 후보의 terminal finish reason 또는 usage-only terminal chunk, frame boundary에서의 clean EOF가 모두 일관될 때만 인정한다.
- actual usage와 amount가 model/reservation 상한을 넘으면 Capture 또는 Release하지 않는다.

### 4. Disconnect와 unknown outcome

- SSE header 전 confirmed non-2xx만 bounded native snapshot을 저장하고 전액 Release한다.
- client disconnect, downstream write/flush 실패, malformed/duplicate terminal usage, unexpected EOF, upstream reset/idle timeout과 missing usage는 예약을 유지하고 `RECONCILING`으로 전환한다.
- terminal usage를 완전히 관찰한 뒤 downstream delivery가 실패하면 Capture를 시도할 수 있다.
- settlement DB 실패는 usage counts와 terminal digest를 durable task에 저장하고 worker가 재시도한다.
- Provider 조회 identity가 없는 ambiguous task는 재호출·자동 Release하지 않고 bounded retry 후 manual review로 수렴한다.

### 5. Backpressure와 자원 한계

- event buffer, cumulative observed bytes와 upstream idle timeout을 설정하고 전체 stream을 메모리에 적재하지 않는다.
- downstream write가 끝나기 전에 무제한 upstream read를 하지 않는다.
- `http.Flusher` 부재는 Provider dispatch 전에 거부한다.
- request cancellation은 upstream context와 body를 닫고 unbounded background drain을 만들지 않는다.
- headers 전송 뒤 내부 JSON 오류를 추가해 native SSE stream을 오염시키지 않는다.

### 6. 보안과 관측성

- prompt, system instruction, tools, candidates, thoughts, response ID, API key와 전체 event payload를 저장하거나 로그하지 않는다.
- first-byte latency, duration, bounded terminal category, disconnect side와 billing transition만 기록한다.
- tenant/model/idempotency/charge identity, token 수와 금액은 metric label에 넣지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1beta/models/{model}:streamGenerateContent?alt=sse
Content-Type: application/json
x-goog-api-key: SERVICE_API_KEY
Idempotency-Key: optional caller-generated key
```

응답은 Provider의 native `text/event-stream`이다. 공식 SDK 사용자는 API key와 base URL만 변경한다.

### 내부 인터페이스

- streaming-aware Gemini executor와 handler dispatch
- bounded Gemini SSE observer와 terminal classification
- `gemini-stream-usage-v1` evidence 및 terminal event digest
- protocol-aware `CompleteStreamUsage`, `MarkReconciling`과 non-replayable idempotency outcome

### 데이터베이스 및 migration

- 기존 token charge/evidence/reconciliation schema가 Gemini stream delivery mode와 schema version을 허용하도록 additive 확장한다.
- unique identity에는 protocol, operation과 delivery mode를 유지해 non-stream/OpenAI stream과 충돌하지 않게 한다.
- 기존 Plan 037~043 row와 구 binary read path를 파괴하지 않는다.
- immutable evidence와 Ledger row를 수정하거나 삭제하지 않는다.

### 멀티레포 handoff

- `conformance`: Python sync/async, JavaScript iterator, abort/disconnect와 malformed terminal fixture
- `cloud`: stream request/idle timeout, event/observation limit과 reconciliation exposure alert
- `dashboard`: Gemini stream delivery mode, bounded terminal category와 manual-review 표시

각 저장소는 같은 initiative의 독립 plan에서 해당 구현을 관리한다.

## 구현 순서

1. route/action/query validation과 streaming executor를 추가한다.
2. 관리형 Reserve와 transcript-free idempotency 상태를 연결한다.
3. byte-preserving bounded SSE relay와 strict cumulative usage observer를 구현한다.
4. terminal Capture, unknown outcome hold와 reconciliation worker 복구를 추가한다.
5. migration, telemetry, README와 Python/JavaScript SDK fixture를 갱신한다.
6. disconnect·timeout·전체 protocol 회귀와 fresh DB migration을 검증한다.

## 테스트 계획

### 단위 테스트

- action, `alt=sse`, body stream flag와 `maxOutputTokens` duplicate/type/range
- LF/CRLF, comment, multiline data, split/coalesced frame와 unknown field
- cumulative usage 증가/감소, cached/tool/thought total 관계와 excess amount
- finish reason, usage-only last chunk, clean/unexpected EOF와 duplicate terminal
- slow writer, write/flush failure, cancellation, idle timeout과 bounded memory
- native bytes/header 보존, digest 안정성과 content redaction

### 통합 테스트

- streaming Reserve 후 final cumulative usage Capture와 차액 반환
- terminal 관찰 전/후 disconnect의 settlement 차이
- settlement crash 후 worker Capture, restart safety와 Ledger exactly-once
- same-key pending/terminal stream의 zero redispatch와 stream/non-stream conflict
- concurrent Wallet, quota/spend-cap rollback과 OpenAI/Gemini operation isolation
- migration repeatability 및 기존 Gemini non-stream 회귀

### 호환성 및 장애 테스트

- Google Gen AI Python sync/async `generate_content_stream`
- Google Gen AI JavaScript `generateContentStream` async iterator와 AbortController
- upstream 400/401/429/500, reset, missing usage, malformed/oversized event와 idle timeout
- immediate/mid-event/after-terminal client disconnect와 slow consumer
- Gemini image/non-stream LLM, OpenAI Chat/Responses, Replicate/fal과 management API 전체 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/gemini -run TestOfficialGeminiLLMStreamingSDKs
```

## 완료 조건

- [x] 공식 Google Gen AI Python/JavaScript SDK가 native Gemini SSE를 소비함
- [x] Provider dispatch 전에 Wallet/quota/spend cap 최대 비용이 원자적으로 예약됨
- [x] valid final cumulative usage가 exact token 금액으로 exactly-once Capture됨
- [x] disconnect/timeout/missing·invalid usage가 자동 Release되지 않고 reconciliation에 보존됨
- [x] confirmed pre-stream non-2xx만 native snapshot과 함께 Release됨
- [x] recoverable settlement failure가 worker로 복구되고 ambiguous 결과는 manual review로 수렴함
- [x] streaming idempotency가 Provider redispatch와 Ledger 중복을 방지함
- [x] relay가 native SSE bytes를 유지하고 bounded backpressure/cancellation로 동작함
- [x] secret, prompt, tool, candidate, thought와 high-cardinality identity가 노출되지 않음
- [x] 전체 unit/race/integration/공식 SDK/장애 회귀가 통과함
- [x] README와 멀티레포 handoff 및 재현 가능한 검증 증거가 갱신됨

## 검증 증거

- 구현 commit `cf798ea`, PR [#56](https://github.com/nativegatewayhq/gateway/pull/56)
- GitHub `check`와 plan-policy `validate` 통과: [Actions run 32410083475](https://github.com/nativegatewayhq/gateway/actions/runs/32410083475), [Actions run 32410083436](https://github.com/nativegatewayhq/gateway/actions/runs/32410083436)
- `GOCACHE=/private/tmp/nativegateway-go-cache make check`: gofmt, vet, 전체 race unit test와 모든 binary build 통과
- fresh PostgreSQL `gateway_plan044`에 migration `000038_gemini_streaming_settlement.sql` 적용 후, 격리된 Redis DB 12에서 `GOFLAGS=-p=1 make integration-test` 전체 통과
- `go test -tags=sdkconformance ./protocols/gemini -run TestOfficialGeminiLLMStreamingSDKs -count=1` 통과
  - `google-genai` Python 2.19.0 sync/async stream
  - `@google/genai` JavaScript 2.18.0 async iterator와 AbortSignal
- CRLF/LF, comment, multiline data, unknown field, cumulative usage regression, duplicate key, truncated event, non-2xx Release, downstream write failure, header/idle timeout과 transcript-free replay 회귀 통과
- Dashboard, Cloud와 Conformance에는 delivery mode, terminal category, timeout과 SDK fixture 계약을 `affected_repos` handoff로 남겼다.

## Rollback 계획

- billing-required mode에서 `streamGenerateContent`를 pre-dispatch fail closed 상태로 되돌린다.
- BYOK relay 결함 시 streaming action 전체를 명시적 unsupported 오류로 비활성화한다.
- 신규 stream reconciliation backlog를 drain하거나 reservation을 유지한 채 manual review한다.
- additive migration, immutable evidence와 Ledger는 감사 보존 기간 동안 삭제하지 않는다.
- 기존 `generateContent` non-stream과 Gemini image route는 유지한다.

## 후속 작업

- Anthropic Messages non-streaming token settlement
- Anthropic Messages SSE streaming과 disconnect settlement
- Gemini `countTokens`와 Cached Content lifecycle
- cross-provider LLM routing과 fallback
