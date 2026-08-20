---
id: gateway-20260821-037
title: Phase 4 OpenAI Chat Token Usage Billing and Settlement
status: accepted
created_at: 2026-08-21T01:38:12+09:00
updated_at: 2026-08-21T01:38:12+09:00
owners:
  - gateway
initiative: phase-4-openai-chat-token-settlement
depends_on:
  - gateway-20260820-008
  - gateway-20260820-009
  - gateway-20260820-010
  - gateway-20260820-012
  - gateway-20260820-013
  - gateway-20260820-021
  - gateway-20260820-022
  - gateway-20260820-023
  - gateway-20260821-036
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - cloud
  - conformance
---

# Phase 4 OpenAI Chat Token Usage Billing and Settlement

## 목적

OpenAI Chat Completions의 입력·cached 입력·출력 token usage를 정수 단가로 정확히 계산하고, 동시 요청에서도 최대 비용을 선예약한 뒤 Provider의 실제 usage로 Capture/Release 또는 durable reconciliation하는 관리형 과금 경계를 제공한다.

## 배경

Plan 036은 공식 SDK 호환 비스트리밍 Chat path를 BYOK 전용으로 제공하며 `billing=required`에서 Chat model 활성화를 fail closed한다. 관리형 서비스에서 Chat을 열려면 이미지의 요청당 고정 가격과 다른 token 차원 가격, 요청 전 안전한 상한, Provider usage 증거, timeout 이후 상태 확인 한계와 idempotent replay가 필요하다. 입력 토큰을 Gateway tokenizer 추정치로 확정하면 Provider 계산과 불일치하므로, 예약에는 보수적 상한만 쓰고 최종 금액은 native response의 검증된 usage만 사용한다.

## 범위

- input, cached input, output token별 immutable effective-dated 가격
- Chat model별 maximum input/output token capability와 명시적 output limit 계약
- UTF-8 request byte length 기반 보수적 input token 예약 상한
- `max_completion_tokens`/legacy `max_tokens` 파싱과 충돌 거부
- Wallet Reserve 후 단일 Provider dispatch
- native response `usage.prompt_tokens`, `completion_tokens`, cached token detail 추출
- 실제 usage 기반 Capture와 예약 차액 Release
- non-2xx confirmed response Release 및 native snapshot replay
- timeout/connection/response-loss/usage-invalid의 durable reconciliation hold
- Idempotency-Key fingerprint와 native response replay
- cost quota, provider spend cap와 minimum margin enforcement
- 요청별 estimated/actual cost, sale, margin과 usage evidence 감사 조회
- Chat billing/reconciliation telemetry와 운영 문서

## 제외 범위

- SSE streaming과 disconnect 중 usage recovery
- cross-provider retry/fallback
- OpenAI Responses API, Gemini/Anthropic token schema
- tokenizer를 이용한 정확한 preflight token count
- cached token 할인이 없는 Provider에 대한 합성 할인
- tool 실행 자체의 외부 비용, storage와 web search 별도 과금
- Batch API, flex/service tier와 priority processing 가격
- 조직 결제, 세금, invoice와 Dashboard UI 자체

## 설계 및 구현 순서

### 1. Chat capability와 request bounds

- Chat model에는 `maximum_input_tokens`, `maximum_output_tokens`와 usage schema version을 필수로 둔다.
- billing-required mode에서는 `max_completion_tokens` 또는 legacy `max_tokens` 중 정확히 하나를 요구하고 1 이상 model maximum 이하만 허용한다.
- 입력 예약 상한은 prompt를 로깅/토큰화하지 않고 canonical native request byte length를 token upper bound로 사용한다. byte-level tokenizer에서 실제 input token이 이를 넘으면 정산을 중단하고 manual review한다.
- BYOK mode의 기존 native 요청 계약은 유지하며 output limit 강제는 관리형 과금 경로에만 적용한다.

### 2. Immutable token price

- channel/protocol/operation/model별 input, cached input, output `cost_per_million`과 `sale_per_million`을 정수 micro-currency로 저장한다.
- 가격 version과 effective interval은 기존 가격 정책처럼 append-only로 운영하고 요청 시작 시 하나의 immutable version을 선택한다.
- 각 sale 단가는 cost 이상이고 minimum margin을 만족해야 하며 overflow-safe ceiling division으로 token 금액을 계산한다.
- cached input은 `prompt_tokens`에 포함되므로 일반 input에서 cached quantity를 차감한 뒤 별도 단가를 적용한다.

### 3. Reserve와 idempotency

- 최대 input byte bound와 requested output bound를 가격 version에 적용해 원가·판매가 상한을 계산한다.
- organization Wallet, hierarchical cost quota와 provider channel spend cap을 하나의 transaction 경계에서 예약한다.
- Idempotency fingerprint는 protocol, operation, logical model, media type와 원본 body를 포함하고 같은 key/같은 요청은 Provider를 재호출하지 않는다.
- key 재사용 충돌, 권한 제거와 price/channel identity mismatch는 dispatch 전에 fail closed한다.

### 4. Usage evidence와 settlement

- 2xx native response를 bounded buffer에 보관한 후 usage object를 strict integer/non-negative schema로 추출한다.
- `prompt_tokens >= cached_tokens`, input/output/model maximum과 예약 상한을 모두 검증한다.
- 검증된 실제 usage에 immutable price를 적용해 actual cost/sale를 계산하고 예약 이하 금액만 Capture하며 차액을 Release한다.
- confirmed non-2xx는 비용 없음 정책으로 전액 Release하고 response snapshot을 replay 가능하게 저장한다. Provider 계약상 오류 응답 과금이 도입되면 별도 change plan을 요구한다.

### 5. Unknown outcome과 reconciliation

- timeout, connection reset, body loss, malformed/missing usage, usage upper-bound 초과와 DB settlement 실패는 예약을 유지하고 `RECONCILING`으로 기록한다.
- Chat Completions에는 Provider-side 조회 ID로 결과를 복구할 표준 API가 없으므로 자동 재호출하지 않는다.
- 완전한 native response snapshot과 valid usage가 확보된 settlement failure만 worker가 idempotently 재정산한다.
- Provider 결과가 불명확한 timeout/connection과 usage-invalid는 bounded attempt 후 `MANUAL_REVIEW`로 전환하며 자동 Release하지 않는다.

### 6. Observability와 운영 계약

- telemetry는 reserve/capture/release/reconciling, usage-validity와 bounded outcome만 기록한다.
- token 수, 금액, model, tenant, prompt, response와 idempotency key는 metric label에 넣지 않는다.
- CLI 또는 control-plane command로 token price와 model limit을 publish하고 현재 effective version을 secret 없이 확인한다.
- README에 가격 단위, rounding, reservation formula, timeout hold와 replay semantics를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 `POST /v1/chat/completions` wire를 유지한다. 관리형 과금 모드에서는 다음 header를 지원한다.

```text
Idempotency-Key: caller-generated-key
```

응답은 OpenAI native JSON을 유지하며 Gateway 내부 price, balance, reservation과 margin을 포함하지 않는다.

### 내부 인터페이스

- `chatpricing.Estimate`와 immutable price version
- `chatbilling.Begin`, `Replay`, `CompleteUsage`, `MarkReconciling`
- strict OpenAI Chat usage evidence extractor
- Chat-specific reconciliation task와 replay snapshot
- model token-limit capability

### 데이터베이스 및 migration

- append-only `chat_token_prices`
- `chat_request_charges`와 immutable selected price/limit/reservation identity
- `chat_usage_evidence`
- `chat_charge_reconciliations`
- idempotency uniqueness와 claim indexes

모든 migration은 additive다. 구 binary는 새 table을 무시할 수 있고 Chat model 활성화를 제거하면 새 요청 없이 기존 reconciliation만 drain한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Dashboard는 initiative `phase-4-openai-chat-token-settlement`에서 input/cached/output usage, reserved/captured sale와 manual-review 표시를 소유한다.
- Cloud는 token price/limit secret-free 배포, reconciliation alert와 spend exposure dashboard를 소유한다.
- Conformance는 usage variants, idempotent replay, timeout hold와 native response 비변형 fixture를 소유한다.

## 보안 및 과금 고려사항

- prompt/message/tool content를 token 계산 목적으로 저장하거나 로그하지 않는다. fingerprint만 one-way hash로 저장한다.
- 금액 계산은 checked integer arithmetic와 ceiling division을 사용해 overflow나 무료 fractional usage를 방지한다.
- Wallet reservation, quota와 spend cap은 transaction/row lock으로 동시성 음수 잔액을 차단한다.
- 실제 sale이 예약 상한을 넘으면 Capture하지 않고 manual review하며 새 Provider 요청도 하지 않는다.
- missing usage를 0으로 간주하지 않고, timeout을 실패로 간주해 Release하지 않는다.
- Idempotency replay는 최초 request의 권한과 current key ownership을 다시 확인하고 Provider credential/price identity를 노출하지 않는다.

## 테스트 계획

### 단위 테스트

- model token limit, dual/invalid output limit와 input byte upper bound
- per-million ceiling arithmetic, cached subtraction, overflow와 minimum margin
- usage missing/type/negative/inconsistent/upper-bound 검증
- error/body/header redaction과 native response serialization
- fingerprint stability와 idempotency conflict

### 통합 테스트

- PostgreSQL concurrent Reserve에서 Wallet 음수 방지
- same-key replay와 conflicting key의 Provider zero-call/이중 과금 방지
- input/cached/output 실제 usage Capture와 정확한 차액 Release
- cost quota/spend cap/margin 거부
- settlement crash 후 worker replay와 Ledger entry exactly-once
- timeout/connection/usage-invalid reservation hold와 manual-review convergence
- API Key tenant/model isolation과 price version race

### 호환성 및 장애 테스트

- OpenAI Python/JavaScript SDK native response와 usage parsing
- upstream 400/401/429/500, timeout, reset, truncated/oversized response
- restart 전후 reconciliation/idempotency replay
- Plan 036 BYOK, image billing, async Job과 management API 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAISDKsUseOnlyBaseURLAndKey
```

## 완료 조건

- [ ] input/cached/output token 가격과 model limit이 immutable version으로 관리됨
- [ ] 최대 비용 Reserve 전에는 Provider가 호출되지 않고 동시 요청에서도 Wallet이 음수가 되지 않음
- [ ] actual usage Capture와 예약 차액 Release가 정수 rounding 규칙과 일치함
- [ ] missing/invalid/excess usage와 timeout이 자동 Release되지 않고 reconciliation/manual review로 수렴함
- [ ] Idempotency replay가 Provider 재호출과 Ledger 중복을 방지함
- [ ] quota, spend cap, minimum margin과 model authorization이 dispatch 전에 적용됨
- [ ] raw prompt, credential, token/금액 identity가 응답·로그·telemetry에 노출되지 않음
- [ ] OpenAI Python/JavaScript SDK native 호환성이 유지됨
- [ ] 전체 unit/race/integration/장애 테스트가 통과함
- [ ] README와 Dashboard/Cloud/Conformance handoff 및 검증 증거가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Chat model allowlist를 제거해 신규 요청을 중단하고 기존 BYOK/이미지 route를 유지한다.
- 미정산 charge가 존재하면 worker를 먼저 drain하거나 reservation을 유지한 채 manual reconciliation한다.
- additive table은 즉시 삭제하지 않으며 Ledger와 audit evidence 보존 기간 후 별도 migration으로 정리한다.

## 후속 작업

- OpenAI Chat SSE streaming usage와 disconnect settlement
- OpenAI Responses API와 tool-specific usage
- Gemini/Anthropic native token usage adapters
- cross-provider LLM routing/fallback
