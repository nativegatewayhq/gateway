---
id: gateway-20260821-046
title: Phase 4 Anthropic Messages Token Usage Billing and Settlement
status: completed
created_at: 2026-08-21T23:30:00+09:00
updated_at: 2026-08-22T01:10:00+09:00
owners:
  - gateway
initiative: phase-4-anthropic-token-usage-settlement
depends_on:
  - gateway-20260821-045
  - gateway-20260821-037
  - gateway-20260821-040
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 4 Anthropic Messages Token Usage Billing and Settlement

## 목적

Anthropic native non-streaming Messages 요청에 대해 최대 비용을 원자적으로 예약하고, native `usage`의 input/output 및 prompt-cache token을 엄격히 검증해 실제 원가·판매가를 확정하며, 실패·불확실 상태를 정확히 release 또는 reconciliation한다.

## 배경

Plan 045는 BYOK native facade와 관리형 모드 fail-closed 경계를 완성했다. 관리형 서비스를 활성화하려면 Provider 호출 전에 Wallet·quota·spend-cap을 예약하고, Anthropic 응답의 `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`를 가격 축별로 정산해야 한다. 기존 Chat 원장은 protocol/operation 격리를 제공하지만 Anthropic prompt caching은 일반 input/cached-input보다 세분된 가격 계약이 필요하므로, 손실 가능성이 있는 암묵적 합산 없이 additive schema와 명시적 계산을 사용한다.

참조 계약: [Anthropic Messages API](https://platform.claude.com/docs/en/api/messages), [Anthropic prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)

## 범위

- `protocol=anthropic`, `operation=messages.create` 전용 immutable token price
- input, output, cache-write, cache-read의 네 가지 원가·판매가 rate
- model별 maximum input/output token limit과 요청 `max_tokens` 검증
- body byte 상한을 보수적 maximum input reservation 단위로 사용하는 기존 LLM 정책 유지
- Wallet, cost quota, Provider spend cap의 단일 PostgreSQL transaction 예약
- strict Anthropic native non-streaming usage parser와 overflow/range 검증
- 2xx valid usage capture, known Provider non-2xx release, uncertain result reconciliation
- idempotency fingerprint, native response snapshot replay와 tenant isolation
- 공식 Python sync/async 및 TypeScript SDK의 managed response 호환
- pricing CLI, README, migration, integration 및 concurrency test

## 제외 범위

- `stream=true`, SSE relay, client disconnect와 partial stream settlement
- service-tier, regional 또는 long-context premium의 동적 구간 가격
- web search, code execution 등 server-tool 별 usage 가격
- Message Batches와 token counting endpoint
- cross-provider routing/fallback 및 response 변환
- Dashboard/Cloud/Conformance 저장소 내부 구현

## 핵심 결정

### 1. Reservation boundary

- 인증, header/body envelope, exact model 권한, channel availability를 검증한 후 Provider credential 조회와 dispatch 전에 예약한다.
- `maximum_input_tokens`는 model limit과 bounded raw request byte 수 중 보수적으로 계산하고, `maximum_output_tokens`는 요청의 정확히 하나인 positive `max_tokens`를 사용한다.
- cache-write/read의 사전 분포는 알 수 없으므로 최대 input 전체에 대해 가장 비싼 가능한 input 계열 rate를 적용해 sale과 cost를 각각 상한 예약한다.
- 최소 마진, Wallet, quota, spend-cap 중 하나라도 실패하면 Provider side effect는 0회다.

### 2. Anthropic usage axes

- 2xx response는 정확히 하나의 top-level `usage` object가 있어야 한다.
- `input_tokens`와 `output_tokens`는 필수 non-negative integer다.
- `cache_creation_input_tokens`와 `cache_read_input_tokens`는 optional non-negative integer이며 absent는 0이다.
- cache creation의 nested ephemeral breakdown 등 미래 필드는 native body에 보존하되 이 계획의 rate 축에 포함되는 총계 필드만 읽는다.
- 모든 합과 곱은 checked integer arithmetic을 사용하고 reserved maxima 초과, duplicate/type 오류, 음수와 overflow를 invalid usage로 처리한다.

### 3. Settlement classification

- Provider dispatch 전 실패는 reservation을 생성하지 않는다.
- 명확한 Provider HTTP non-2xx는 native status/body snapshot을 저장하고 reservation을 전액 release한다.
- 2xx + valid usage는 실제 cost/sale을 capture하고 예약 차액을 반환한다.
- timeout, connection loss, response read failure, 2xx missing/invalid usage 또는 settlement transaction 실패는 `RECONCILING`으로 전이한다.
- reconciliation worker는 저장된 usage/snapshot이 있으면 deterministic capture를 재시도하고, 증거 없는 Provider 결과를 임의로 release하지 않는다.

### 4. Price and schema isolation

- 기존 `chat_token_prices`와 `chat_request_charges`를 protocol/operation 기반으로 재사용하되 cache-write rate가 없다면 additive column과 constraint migration을 적용한다.
- price row는 publish 후 immutable이며 동일 channel/protocol/operation/model/currency의 유효 기간이 겹치지 않는다.
- 기존 OpenAI/Gemini price row의 cache-write rate는 명시적 0 또는 기존 의미와 호환되는 default로 backfill한다.
- CLI는 `messages.create` 가격을 네 축 모두 명시하도록 요구하며 비밀이나 실제 prompt를 출력하지 않는다.

### 5. Idempotency and native replay

- fingerprint는 protocol, operation, exact model/channel, media type과 raw body를 포함한다.
- 동일 tenant·idempotency key·fingerprint의 settled 요청은 Provider 재호출 없이 Anthropic native snapshot을 재생한다.
- key 재사용 충돌, pending/reconciling 중복과 다른 tenant의 동일 key는 기존 Chat 원장 규칙을 따른다.

## 인터페이스와 데이터 변경

### 설정

```text
GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS=model:maximum_input_tokens:maximum_output_tokens,...
```

`GATEWAY_BILLING_MODE=required`에서 활성 Anthropic model마다 limit이 없거나 불완전하면 프로세스 시작을 거부한다.

### 가격 CLI

```text
gateway-chat-price \
  -protocol anthropic \
  -operation messages.create \
  -model claude-model \
  -input-cost ... -input-sale ... \
  -cache-write-cost ... -cache-write-sale ... \
  -cached-input-cost ... -cached-input-sale ... \
  -output-cost ... -output-sale ...
```

### migration

- token price/charge usage에 cache-write cost/sale rate와 actual cache-write tokens를 additive하게 저장한다.
- non-negative, checked totals, protocol/operation/delivery-mode constraint를 Anthropic non-streaming까지 확장한다.
- 기존 row와 index를 삭제하거나 의미 변경하지 않는다.

### 멀티레포 handoff

- `conformance`: managed Anthropic success, native non-2xx, replay와 malformed usage fixture
- `cloud`: model limits, four-axis immutable prices, reconciliation worker와 alert 설정
- `dashboard`: reserve/capture/release/reconciling 및 네 usage 축 표시 계약

각 저장소는 동일 initiative의 독립 plan에서 내부 구현을 관리한다.

## 구현 순서

1. model limit 설정과 네 축 pricing/schema migration을 추가한다.
2. pricing estimate/capture arithmetic과 CLI를 protocol/operation 격리로 확장한다.
3. strict `max_tokens` 및 Anthropic usage parser를 구현한다.
4. Messages handler에 reserve, native replay, capture/release/reconciliation을 연결한다.
5. reconciliation evidence와 telemetry를 확장한다.
6. 단위·동시성·통합·공식 SDK 회귀와 README를 완성한다.

## 보안 및 과금 고려사항

- reservation commit 전에 Provider credential을 resolve하거나 outbound body를 dispatch하지 않는다.
- prompt, system, tool input/output, service/provider key와 native response content를 ledger event, log, metric, trace에 넣지 않는다.
- Provider가 보고한 usage라도 reservation 상한을 초과하면 capture하지 않고 reconciliation한다.
- native non-2xx snapshot은 bounded safe headers/body만 저장하며 replay 시 내부 charge metadata를 노출하지 않는다.
- concurrent begin/replay/settlement는 advisory lock과 conditional state transition으로 이중 과금·음수 잔액을 방지한다.

## 테스트 계획

### 단위 테스트

- model limits와 exactly-one positive `max_tokens`
- 네 usage 필드 absent/zero/duplicate/type/negative/overflow/maxima 초과
- cache-write/read 포함 estimate/capture와 rounding/minimum margin
- billing error의 Anthropic-shaped status/body
- idempotency fingerprint, native snapshot replay와 conflict

### 통합 테스트

- reserve/capture/release 원장 합계와 Wallet 불변식
- quota/spend-cap/insufficient balance에서 zero Provider dispatch
- 2xx valid usage, native 400/401/429/500 release
- timeout/connection/read/invalid usage/settlement failure reconciliation
- 동시 동일 key 및 다중 key 요청에서 exactly-once settlement
- migration fresh install 및 기존 OpenAI/Gemini price/charge 회귀

### 호환성 테스트

- Anthropic Python sync/async와 TypeScript managed `messages.create`
- content blocks와 native usage typed decoding
- settled idempotent response replay의 SDK 동등성

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/anthropic -run TestOfficialAnthropicManagedMessagesSDKs
```

## 완료 조건

- [x] 관리형 Anthropic 요청이 Provider 호출 전에 최대 cost/sale을 원자적으로 예약함
- [x] input/output/cache-write/cache-read usage가 네 가격 축으로 정확히 정산됨
- [x] balance/quota/spend-cap/model-limit 실패가 zero dispatch를 보장함
- [x] 2xx valid usage는 capture, known non-2xx는 release, 불확실 결과는 reconciliation됨
- [x] idempotency replay가 Provider 재호출·이중 과금 없이 native 응답을 재생함
- [x] 동시 요청에서도 Wallet이 음수가 되지 않고 ledger/charge 전이가 exactly once임
- [x] 기존 OpenAI/Gemini token 가격과 settlement 의미가 보존됨
- [x] 공식 Anthropic Python sync/async 및 TypeScript managed SDK 검증이 통과함
- [x] README, migration, CLI와 multi-repo handoff가 갱신됨
- [x] 전체 unit/race/integration/security 회귀가 통과함

## 검증 증거

- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과
- 격리 PostgreSQL `gateway_plan046`, Redis DB 14에서 Anthropic package를 포함한 `GOFLAGS=-p=1 make integration-test` 통과
- fresh migration과 `internal/chatbilling` 네 축 price/reserve/capture/evidence integration 통과
- `go test -tags=sdkconformance ./protocols/anthropic -run 'TestOfficialAnthropic(Managed)?MessagesSDKs' -v` 통과
- 공식 SDK: Anthropic Python `0.68.0`, `@anthropic-ai/sdk` `0.68.0`; BYOK 및 managed Python sync/async·TypeScript 검증
- 구현 PR과 GitHub CI run은 PR 생성 후 기록한다.

## Rollback 계획

- Anthropic model allowlist 또는 model limits를 비워 managed dispatch를 즉시 fail closed한다.
- Anthropic price 유효 기간을 종료하고 channel credential을 비활성화한다.
- additive schema와 append-only ledger/charge/audit row는 보존한다.
- BYOK facade와 기존 OpenAI/Gemini settlement는 유지한다.

## 후속 작업

- Anthropic Messages native SSE and disconnect settlement
- long-context and service-tier price dimensions
- server-tool usage pricing
- cross-provider LLM routing and fallback
