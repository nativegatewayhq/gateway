---
id: gateway-20260822-047
title: Phase 4 Anthropic Messages Native SSE and Disconnect Settlement
status: completed
created_at: 2026-08-22T01:30:00+09:00
updated_at: 2026-08-22T03:20:00+09:00
owners:
  - gateway
initiative: phase-4-anthropic-messages-streaming-settlement
depends_on:
  - gateway-20260821-045
  - gateway-20260821-046
  - gateway-20260821-038
  - gateway-20260821-041
  - gateway-20260821-044
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 4 Anthropic Messages Native SSE and Disconnect Settlement

## 목적

Anthropic 공식 Python sync/async 및 TypeScript SDK의 `stream=true` Messages 호출을 byte-preserving native SSE로 지원하고, 정상 terminal usage는 네 가격 축으로 확정하며 client disconnect·timeout·불완전 stream은 증거 기반 reconciliation로 전이한다.

## 배경

Plan 045는 Anthropic non-streaming native facade를, Plan 046은 input/cache-read/cache-write/output 예약과 정산을 완성했다. streaming은 HTTP status가 확정된 뒤에도 usage와 terminal event가 늦게 도착하고 client write failure가 Provider completion과 분리되므로, 성공으로 추정하거나 자동 환불할 수 없다. 기존 OpenAI/Gemini stream 원장의 delivery mode와 terminal digest를 재사용하되 Anthropic event grammar와 cumulative usage 의미를 독립 parser로 검증해야 한다.

참조 계약: [Anthropic streaming Messages](https://platform.claude.com/docs/en/build-with-claude/streaming), [Anthropic Messages API](https://platform.claude.com/docs/en/api/messages)

## 범위

- `POST /v1/messages`의 exactly-one `stream: true` 지원
- official Anthropic Python sync/async 및 TypeScript streaming SDK 호환
- fixed trusted Anthropic origin으로 native streaming request 전달
- response-header timeout과 stream idle timeout 분리
- SSE field/order/comment/unknown future event의 byte-preserving relay
- bounded event/frame/line/total evidence와 slowloris 방어
- `message_start`, content block delta, `message_delta`, `message_stop`, `error`, `ping` grammar 검증
- input/cache-read/cache-write/output cumulative usage 추출
- terminal event digest 및 protocol-specific evidence schema
- Provider non-2xx release, valid `message_stop` capture, 모든 불확실 결과 reconciliation
- client disconnect, upstream EOF, idle timeout, malformed SSE와 downstream write failure 분류
- Provider health, streaming telemetry, README와 conformance 갱신

## 제외 범위

- Anthropic SDK helper의 고수준 text/event aggregation을 Gateway가 재구현하는 것
- stream response idempotent byte replay
- partial usage 기반 부분 capture 또는 자동 release
- server tool 별 별도 usage 가격
- cross-provider streaming conversion과 fallback
- Message Batches, token counting, Files API

## 핵심 결정

### 1. Native stream preservation

- inbound JSON은 재직렬화하지 않고 `stream=true`를 확인한 원문을 전달한다.
- upstream의 valid SSE bytes는 event name, data, comments, blank lines와 unknown future fields를 그대로 client에 쓴다.
- Gateway는 synthetic content event, usage event 또는 `[DONE]`을 삽입하지 않는다.
- hop-by-hop headers와 sensitive headers는 제거하고 documented safe stream headers만 전달한다.

### 2. Anthropic terminal grammar

- 정상 lifecycle은 exactly one `message_start`, zero or more content events, one or more `message_delta`, exactly one terminal `message_stop`이다.
- `message_start.message.usage`에서 input/cache creation/cache read token을 읽는다.
- `message_delta.usage.output_tokens`는 cumulative output으로 해석하며 감소할 수 없다.
- 마지막 valid usage와 `message_stop`이 모두 있어야 capture 가능하다.
- `error` event, duplicate terminal, terminal 이후 semantic event, missing usage와 malformed JSON/SSE는 capture하지 않는다.
- `ping`, comments와 unknown future event는 relay하되 정산 terminal로 간주하지 않는다.

### 3. Reservation and settlement

- non-streaming과 동일한 model limit, price, Wallet/quota/spend-cap reservation을 stream 시작 전에 commit한다.
- `delivery_mode=stream`으로 charge를 격리하고 idempotency key의 settled stream replay는 지원하지 않는다.
- upstream non-2xx body는 bounded native snapshot으로 release한다.
- valid terminal usage는 `CompleteStreamUsage`로 exactly-once capture한다.
- client disconnect 후에도 자동 환불하지 않고 last valid usage, terminal digest, disconnect side/category를 reconciliation evidence로 저장한다.

### 4. Timeouts and cancellation

- `GATEWAY_ANTHROPIC_REQUEST_TIMEOUT`은 connection/response headers까지 적용한다.
- `GATEWAY_ANTHROPIC_STREAM_IDLE_TIMEOUT`은 각 successful upstream read 사이의 최대 idle interval이다.
- downstream cancellation은 upstream context/body를 즉시 닫되 settlement는 `context.WithoutCancel`의 bounded DB context로 기록한다.
- 전체 stream을 메모리에 적재하지 않고 parser buffer와 terminal evidence만 bounded하게 유지한다.

### 5. Failure taxonomy

```text
complete             -> capture
provider_non_2xx     -> release
client_disconnect    -> reconciliation
upstream_eof         -> reconciliation
upstream_idle        -> reconciliation
provider_error_event -> reconciliation
invalid_sse          -> reconciliation
missing_usage        -> reconciliation
settlement_failure   -> reconciliation
```

## 인터페이스와 데이터 변경

### 설정

```text
GATEWAY_ANTHROPIC_STREAM_IDLE_TIMEOUT=30s
```

### Provider executor

- `MessagesRequest.Streaming` 또는 별도 `CreateMessageStream` 계약
- response header timeout 이후 response body read에는 idle watchdog 적용
- redirects와 automatic retries는 계속 금지

### settlement evidence

- 기존 `chat_request_charges.delivery_mode=stream`, terminal digest와 reconciliation table을 사용한다.
- Anthropic cache-write token evidence와 `anthropic-stream-usage-v1` schema를 허용하는 additive migration을 적용한다.
- terminal category/disconnect side enum을 Anthropic failure taxonomy와 호환되게 확장한다.

### 멀티레포 handoff

- `conformance`: Python sync/async와 TypeScript text/tool/future event fixtures, disconnect/error cases
- `cloud`: stream idle timeout, proxy buffering disable, long-lived connection limits와 alerts
- `dashboard`: streaming charge terminal category, disconnect side와 reconciliation evidence 표시 계약

각 저장소는 동일 initiative의 독립 plan에서 내부 구현을 관리한다.

## 구현 순서

1. stream idle 설정, Provider transport와 response lifecycle을 분리한다.
2. bounded byte-preserving Anthropic SSE relay/parser를 구현한다.
3. input/cache-write/cache-read/output usage와 terminal digest 검증을 추가한다.
4. reserve/non-2xx release/capture/reconciliation 상태 전이를 handler에 연결한다.
5. disconnect/timeout/health/telemetry와 proxy-safe headers를 완성한다.
6. 공식 SDK, fault injection, integration 및 전체 회귀를 검증한다.

## 보안 및 과금 고려사항

- stream event/data, prompt, tool input/result와 content delta를 로그·metric·trace에 넣지 않는다.
- SSE line/event/JSON nesting/total bytes를 제한하고 terminal evidence digest만 장기 저장한다.
- client disconnect, timeout, EOF와 malformed terminal을 성공 또는 무료 실패로 추정하지 않는다.
- reservation 없이 response headers/body를 client에게 전달하지 않는다.
- Provider key와 service key는 upstream event/error, response header와 reconciliation evidence에 포함되지 않는다.

## 테스트 계획

### 단위 테스트

- `stream` absent/false/true/duplicate/type 오류와 `max_tokens` limits
- split reads, CRLF/LF, comments, multi-line data, unknown future event의 byte preservation
- message_start/cache usage, cumulative output, message_stop terminal digest
- duplicate/out-of-order terminal, error event, malformed JSON/SSE와 bound 초과
- downstream write failure, cancellation, idle timeout과 upstream EOF 분류

### 통합 테스트

- stream reservation/capture와 네 축 ledger/usage evidence
- Provider non-2xx release 및 no SSE start
- client disconnect/timeout/invalid terminal reconciliation exactly once
- reconciliation worker의 terminal usage retry
- simultaneous streams에서 Wallet non-negative 및 health permit recovery
- Gateway restart 이후 pending reconciliation 복구

### 공식 SDK 호환성 테스트

- Anthropic Python `messages.stream` sync/async
- Anthropic Python `messages.create(..., stream=True)`
- TypeScript `messages.stream` 및 async iteration
- text delta, tool-use JSON delta, usage와 stop reason typed decoding

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/anthropic -run TestOfficialAnthropicStreamingSDKs
```

## 완료 조건

- [x] 공식 Anthropic Python sync/async 및 TypeScript SDK가 native SSE를 소비함
- [x] valid SSE bytes와 text/tool/future events가 변형 없이 relay됨
- [x] input/cache-write/cache-read/output usage와 message_stop이 정확히 capture됨
- [x] Provider non-2xx만 release되고 불확실 stream은 reconciliation됨
- [x] client disconnect가 upstream 취소 및 exactly-once evidence 저장을 보장함
- [x] response-header/idle timeout과 body/event bounds가 독립적으로 동작함
- [x] stream event/content/key가 log·metric·trace·response header로 유출되지 않음
- [x] 동시 stream에서 Wallet/ledger/health 불변식이 유지됨
- [x] 기존 Anthropic non-streaming 및 OpenAI/Gemini stream 회귀가 통과함
- [x] README, migration, multi-repo handoff와 검증 증거가 갱신됨

## 검증 증거

- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과
- fresh PostgreSQL `gateway_plan047`, Redis DB 15에서 `GOFLAGS=-p=1 make integration-test` 통과
- `go test -tags=sdkconformance ./protocols/anthropic -run TestOfficialAnthropic -v` 통과
- 공식 SDK Anthropic Python `0.68.0` sync/async `messages.stream`, `@anthropic-ai/sdk` `0.68.0` `messages.stream` 검증
- byte preservation, four-axis cumulative usage, malformed lifecycle, error event, missing terminal, idle timeout과 exactly-once terminal evidence fixture 통과
- 구현 PR 및 GitHub CI run은 PR 생성 후 기록한다.

## Rollback 계획

- streaming feature flag/model capability를 비활성화해 `stream=true`만 pre-dispatch 거부한다.
- in-flight reserved/reconciling charge는 worker와 수동 review로 보존한다.
- additive evidence schema와 append-only ledger row는 삭제하지 않는다.
- non-streaming Anthropic Messages와 기존 Provider streaming은 유지한다.

## 후속 작업

- Phase 4 cross-provider LLM routing and fallback
- Anthropic long-context/service-tier pricing
- Anthropic server-tool usage pricing
- LLM observability and evaluation policy
