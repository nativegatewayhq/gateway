---
id: gateway-20260821-040
title: Phase 4 OpenAI Responses Token Usage Billing and Settlement
status: proposed
created_at: 2026-08-21T08:00:00+09:00
updated_at: 2026-08-21T08:00:00+09:00
owners:
  - gateway
initiative: phase-4-openai-responses-token-settlement
depends_on:
  - gateway-20260821-037
  - gateway-20260821-039
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - cloud
  - conformance
---

# Phase 4 OpenAI Responses Token Usage Billing and Settlement

## 목적

OpenAI Responses non-streaming 요청의 input, cached input과 output usage를 immutable 정수 가격으로 계산하고, 최대 비용 선예약부터 실제 usage Capture·차액 반환·unknown outcome reconciliation까지 관리형 과금 경계를 제공한다.

## 배경

Plan 039는 Responses native BYOK facade를 제공하지만 billing-required mode에서는 무료 dispatch를 막기 위해 fail closed한다. 관리형 서비스를 열려면 Chat 가격을 암묵적으로 재사용하지 않고 `responses.create` operation 전용 가격과 usage evidence를 가져야 한다. Responses typed output, reasoning과 built-in tool usage를 보존하면서도 고객 응답에 내부 금액이나 정산 identity를 섞지 않아야 한다.

## 범위

- `responses.create` 전용 input/cached-input/output per-million immutable 가격
- Responses model maximum input/output capability와 `max_output_tokens` 필수 계약
- request byte 기반 보수적 input 상한과 maximum sale/cost Reserve
- Wallet, hierarchical cost quota와 Provider spend cap 원자적 예약
- `Idempotency-Key` fingerprint, terminal native response snapshot과 replay
- `usage.input_tokens`, cached input details, `output_tokens` strict extraction
- output token에 포함된 reasoning token의 감사 검증
- actual usage Capture와 예약 차액 Release
- confirmed non-2xx 전액 Release
- timeout, response loss, missing/malformed/excess usage와 settlement failure reconciliation
- minimum margin, model authorization와 Provider credential pre-dispatch enforcement
- 가격 발행 CLI/control-plane, telemetry, README와 공식 SDK conformance

## 제외 범위

- Responses SSE streaming
- web search, file search, code interpreter, computer use 등 built-in tool별 추가 비용
- Provider-side `store=true` response retrieve/delete/cancel lifecycle
- background mode와 deferred result polling
- cross-provider routing/fallback
- Gateway tokenizer를 이용한 exact preflight count
- 조직 결제, 세금, invoice와 Dashboard UI 구현

## 설계 및 구현 순서

### 1. Capability와 request bound

- Responses model별 maximum input/output token을 명시한다.
- billing-required mode는 `max_output_tokens`를 정확히 하나의 positive integer로 요구하고 model maximum 이하인지 검사한다.
- input reservation upper bound는 native request byte length이며 Provider usage가 이를 넘으면 자동 Capture하지 않는다.
- BYOK mode의 request 계약은 변경하지 않는다.

### 2. Immutable operation price

- channel/protocol=`openai`/operation=`responses.create`/model별 input, cached input, output cost/sale를 effective-dated append-only로 발행한다.
- Chat price와 table/service를 재사용할 수 있어도 operation identity와 publication key 충돌 영역은 분리한다.
- 각 가격 차원은 minimum margin을 만족하고 checked integer ceiling arithmetic을 사용한다.
- cached input은 input total에서 차감한 뒤 cached rate를 적용한다.

### 3. Reservation과 idempotency

- selected price version, exact model/channel, input byte bound와 output bound를 immutable charge identity로 저장한다.
- Wallet/quota/spend cap과 charge 생성은 하나의 PostgreSQL transaction이다.
- 동일 key/동일 body의 terminal 요청은 stored native response를 replay하고 Provider와 Ledger를 재호출하지 않는다.
- key conflict, pending/reconciling charge, current tenant/model authorization 실패는 dispatch 전에 종료한다.

### 4. Usage settlement

- bounded 2xx native response에서 usage를 strict integer/non-negative schema로 추출한다.
- `input_tokens >= cached_tokens`, input/output bounds와 reservation maximum을 검사한다.
- `output_tokens_details.reasoning_tokens`는 output total 이하인지 검증하되 별도 가격을 적용하지 않는다.
- valid usage만 immutable price로 Capture하고 차액을 Release한다.
- usage evidence에는 counts, schema version과 response digest를 append-only로 저장한다.

### 5. Unknown outcome과 reconciliation

- timeout, connection loss, truncated/oversized response, missing/invalid/excess usage와 DB settlement failure는 reservation을 유지한다.
- complete native snapshot과 valid usage가 있는 settlement failure만 worker가 exactly-once 재정산한다.
- Provider 조회 identity가 없는 outcome은 자동 재호출/Release하지 않고 bounded attempts 후 manual review로 전환한다.
- confirmed non-2xx만 비용 없음 정책으로 Release한다.

### 6. 운영 및 telemetry

- reserve/capture/release/reconciling, usage validity와 bounded outcome만 기록한다.
- token count, amount, model, tenant, response ID, input/output/tool content와 idempotency key를 metric label에 넣지 않는다.
- CLI는 secret 없이 current effective Responses price를 발행/확인한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/responses
Idempotency-Key: optional caller-generated key
```

관리형 요청은 `max_output_tokens`를 포함한다. 응답은 OpenAI native JSON이며 Gateway price, balance, reservation과 margin을 포함하지 않는다.

### 내부 인터페이스

- operation-aware token pricing request
- `responsesbilling.Begin`, `Replay`, `CompleteUsage`, `Release`, `MarkReconciling`
- strict Responses usage extractor
- Responses reconciliation worker/task taxonomy

### 데이터베이스

- operation-aware token price version 또는 Responses 전용 immutable price table
- `responses_request_charges`
- `responses_usage_evidence`
- `responses_charge_reconciliations`
- tenant idempotency unique index와 due-task claim index

모든 변경은 additive migration으로 제공한다.

## 보안 및 과금 고려사항

- input, instructions, tool arguments/results, reasoning/output과 response ID를 과금 목적으로 저장하지 않는다.
- missing usage를 zero로 처리하거나 timeout을 무료 실패로 처리하지 않는다.
- actual sale/cost가 reservation을 넘으면 Capture하지 않고 manual review한다.
- response snapshot은 bounded allowlisted headers와 body digest를 사용하고 Provider credential을 포함하지 않는다.
- built-in tool cost를 일반 output token 비용에 숨겨 합산하지 않는다.

## 테스트 계획

### 단위 테스트

- max output strict parsing, byte upper bound와 model limits
- input/cached/output ceiling arithmetic, margin과 overflow
- missing/type/negative/inconsistent/excess usage와 reasoning details
- fingerprint stability, key conflict와 native snapshot preservation

### 통합 테스트

- concurrent Reserve Wallet safety와 quota/spend-cap atomicity
- actual usage exact Capture/차액 Release
- same-key replay와 Provider/Ledger exactly-once
- price version race, tenant/model isolation과 margin rejection
- settlement crash 후 worker retry, unknown outcome manual review

### 공식 SDK 및 회귀

- OpenAI Python/JavaScript Responses native response와 usage
- upstream 400/401/429/500, timeout/reset/truncated/oversized response
- Chat non-stream/stream, Images, Replicate/fal와 management API 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAIResponsesSDKs
```

## 완료 조건

- [ ] Responses 전용 immutable token price와 model limit이 관리됨
- [ ] Provider dispatch 전에 Wallet/quota/spend cap이 원자적으로 예약됨
- [ ] valid native usage가 exact integer amount로 Capture되고 차액이 반환됨
- [ ] non-2xx만 Release되며 timeout/missing usage는 reservation을 유지함
- [ ] reconciliation이 recoverable settlement를 exactly-once 완료하고 unknown outcome을 manual review로 보냄
- [ ] idempotency replay가 Provider와 Ledger 중복을 방지함
- [ ] minimum margin, tenant/model authorization과 Provider availability가 dispatch 전에 적용됨
- [ ] 내부 과금 identity, prompt/tool/output/credential이 응답·로그·telemetry에 노출되지 않음
- [ ] 공식 SDK와 전체 unit/race/integration/장애 회귀가 통과함
- [ ] README와 Dashboard/Cloud/Conformance handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Responses model allowlist를 제거해 신규 managed 요청을 중단한다.
- 기존 reconciliation을 drain 또는 manual review한 뒤 worker를 중단한다.
- additive charge/usage/Ledger evidence는 감사 보존 기간 동안 유지한다.

## 후속 작업

- Responses SSE streaming 및 disconnect settlement
- response retrieve/delete/cancel lifecycle
- built-in tool별 usage/cost settlement
- OpenAI tool calling conformance와 policy controls
