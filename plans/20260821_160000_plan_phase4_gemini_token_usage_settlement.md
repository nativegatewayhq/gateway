---
id: gateway-20260821-043
title: Phase 4 Gemini Token Usage Billing and Settlement
status: accepted
created_at: 2026-08-21T16:00:00+09:00
updated_at: 2026-08-21T16:00:00+09:00
owners:
  - gateway
initiative: phase-4-gemini-token-usage-settlement
depends_on:
  - gateway-20260821-037
  - gateway-20260821-042
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - cloud
  - conformance
---

# Phase 4 Gemini Token Usage Billing and Settlement

## 목적

Gemini native non-streaming LLM 요청의 최대 비용을 Provider 호출 전에 예약하고, Google `usageMetadata`의 prompt·cached·tool-use·candidate·thought token을 strict하게 검증해 실제 비용을 exactly-once Capture하며 불명확한 결과는 durable reconciliation으로 보존한다.

## 배경

Plan 042는 공식 Google Gen AI SDK 호환 BYOK 경로와 `gemini + chat.completions` operation을 만들었지만, 관리형 모드에서는 이미지 가격 오적용과 무료 dispatch를 막기 위해 LLM을 fail closed한다. Gemini usage는 OpenAI usage와 field 및 사고 token 의미가 다르므로 OpenAI extractor를 재사용하면 안 된다. 가격 저장소를 공유하더라도 protocol과 usage schema는 immutable charge identity에 포함해야 한다.

참조 계약: [Gemini GenerateContentResponse와 UsageMetadata](https://ai.google.dev/api/generate-content#generatecontentresponse), [Google Gen AI SDK 문서](https://googleapis.github.io/python-genai/)

## 범위

- `protocol=gemini`, `operation=chat.completions` 전용 effective-dated token 가격
- model별 maximum input/output token capability
- `generationConfig.maxOutputTokens` strict managed 요청 계약
- native request byte 기반 보수적 input reservation 상한
- Wallet, hierarchical quota와 Google channel spend cap 원자적 Reserve
- Idempotency-Key fingerprint와 bounded native response replay
- `usageMetadata` strict extraction 및 schema versioning
- prompt/cached/tool-use input과 candidate/thought output actual usage 계산
- valid 2xx usage Capture와 예약 차액 Release
- confirmed non-2xx Release
- timeout, connection loss, body loss, missing/invalid/excess usage와 settlement failure reconciliation
- minimum margin, credential, model authorization와 Provider health pre-dispatch enforcement
- 가격 발행 CLI, telemetry, README와 공식 SDK conformance

## 제외 범위

- `streamGenerateContent`와 SSE disconnect settlement
- cross-provider 변환·routing·fallback
- modality별 이미지/audio token과 media-second 별도 가격
- Grounding, Search, Code Execution 등 tool별 별도 공급자 요금
- Cached Content API 생성/조회 lifecycle
- Gateway tokenizer를 이용한 exact preflight count
- Dashboard/Cloud/Conformance 저장소 내부 구현

## 핵심 결정

### 1. 가격과 capability

- token price key는 protocol, operation, channel, logical model과 effective interval을 포함한다.
- input, cached input, output cost/sale per million을 정수 micro-currency로 저장하고 checked ceiling arithmetic을 사용한다.
- cached token은 prompt token에 포함된 것으로 취급해 `prompt-cached`와 cached quantity에 각 단가를 적용한다.
- tool-use prompt token은 input rate, thought token은 output rate를 적용한다.
- sale은 cost 이상이고 configured minimum margin을 충족해야 한다.

### 2. Reservation bound

- 관리형 요청은 `generationConfig.maxOutputTokens`를 정확히 하나의 positive integer로 제공하고 model maximum 이하이어야 한다.
- input upper bound는 credential을 제거한 native request body byte length이며 prompt/tool body를 저장하거나 tokenize하지 않는다.
- maximum input과 requested output에 immutable price를 적용해 Wallet/quota/spend cap을 하나의 transaction에서 예약한다.
- price/capability/credential/health 실패 시 Provider dispatch와 charge side effect는 없다.

### 3. Native usage evidence

- 2xx response는 bounded buffer에 보관하고 JSON duplicate key를 포함한 strict schema로 `usageMetadata`를 읽는다.
- `promptTokenCount`, `candidatesTokenCount`, `totalTokenCount`는 필수 non-negative integer다.
- `cachedContentTokenCount`, `toolUsePromptTokenCount`, `thoughtsTokenCount`는 optional non-negative integer이며 cached는 prompt 이하이어야 한다.
- billable input은 prompt와 tool-use prompt, billable output은 candidates와 thoughts로 계산한다.
- `totalTokenCount`와 schema component 관계가 Google contract와 일치하지 않거나 actual usage가 model/reservation 상한을 넘으면 Capture/Release하지 않는다.
- evidence에는 counts, immutable price version, response digest와 `gemini-usage-v1`만 저장하고 prompt/output은 저장하지 않는다.

### 4. Settlement와 idempotency

- valid usage만 actual cost/sale로 exactly-once Capture하고 예약 차액을 Release한다.
- confirmed non-2xx는 비용 없음 정책으로 전액 Release하고 native snapshot을 replay한다.
- same key/same request의 terminal 결과는 Provider와 Ledger 호출 없이 replay한다. body/model/operation 변경은 conflict다.
- timeout, reset, truncated/oversized body, usage missing/invalid/excess와 DB settlement 실패는 예약을 유지한다.
- complete snapshot과 valid usage가 있는 DB 실패만 worker가 재정산하며 그 외에는 bounded attempts 후 manual review로 수렴한다.

### 5. Native 응답과 운영 정보

- Google response/error bytes를 바꾸지 않고 Gateway 가격·잔액·charge identity를 응답에 넣지 않는다.
- telemetry는 bounded protocol/operation/outcome/schema validity와 billing transition만 기록한다.
- model, tenant, token 수, 금액, response ID, prompt/tool/output과 idempotency key는 metric label에 넣지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1beta/models/{model}:generateContent
x-goog-api-key: SERVICE_API_KEY
Idempotency-Key: optional caller-generated key
```

관리형 요청은 `generationConfig.maxOutputTokens`를 포함한다. wire response는 Gemini native JSON이다.

### 내부 인터페이스

- protocol-aware token price publication과 estimate
- Gemini billing `Begin`, `Replay`, `CompleteUsage`, `MarkReconciling`
- strict `usageMetadata` extractor와 `gemini-usage-v1` evidence
- operation/protocol-aware reconciliation task

### 데이터베이스 및 migration

- 기존 token price/charge schema를 protocol-aware additive migration으로 확장하거나 Gemini 전용 append-only table을 추가한다.
- 기존 OpenAI Chat/Responses price, charge, usage와 reconciliation row를 구 binary가 계속 읽을 수 있어야 한다.
- unique key와 Ledger operation key에 protocol을 포함해 OpenAI와 Gemini collision을 막는다.
- cost quota constraint에 `gemini + chat.completions`를 추가한다.

### 멀티레포 handoff

- `dashboard`: Gemini usage component, reserved/captured sale와 manual-review 표시
- `cloud`: Gemini model limit/price publication, reconciliation backlog와 exposure alert
- `conformance`: Python/JavaScript usage variants, idempotency와 timeout fixture

각 저장소는 같은 initiative로 독립 plan을 가진다.

## 구현 순서

1. protocol-aware immutable token price와 Gemini model limit configuration을 추가한다.
2. Gemini request bound parser와 atomic Reserve/idempotency를 연결한다.
3. strict `usageMetadata` extractor와 actual amount 계산을 구현한다.
4. Capture/Release/reconciliation과 worker exactly-once 복구를 추가한다.
5. CLI, telemetry, migration, README와 공식 SDK fixture를 갱신한다.
6. Wallet/과금/timeout/전체 protocol 회귀를 검증한다.

## 보안 및 과금 고려사항

- missing usage를 0으로 처리하거나 timeout을 무료 실패로 처리하지 않는다.
- Provider가 반환한 token count도 type, 관계, model limit과 reservation 상한을 검증한 뒤에만 신뢰한다.
- arithmetic overflow, fractional undercharge와 cached token 이중 차감을 차단한다.
- actual sale/cost가 reserved maximum을 넘으면 자동 Capture하지 않는다.
- prompt, system instruction, tools, candidates, thought content와 provider credential은 저장·로그하지 않는다.

## 테스트 계획

### 단위 테스트

- `maxOutputTokens` missing/duplicate/type/range와 request byte bound
- protocol-aware price lookup, margin, ceiling, overflow와 collision
- usage required/optional field, duplicate/type/negative/inconsistent/excess 검증
- cached subtraction, tool-use input, thought output와 total relationship
- fingerprint stability, native response preservation와 redaction

### 통합 테스트

- concurrent Reserve에서 Wallet 음수 방지와 quota/spend-cap atomicity
- Gemini actual usage Capture/차액 Release와 Ledger exactly-once
- same-key replay/conflict와 Provider zero redispatch
- price version race 및 OpenAI Chat/Responses operation isolation
- timeout/invalid usage hold, worker retry와 manual-review convergence
- migration repeatability와 API key tenant/model permission

### 호환성 및 장애 테스트

- Google Gen AI Python/JavaScript managed non-streaming response/usage
- upstream 400/401/429/500, timeout/reset/truncated/oversized response
- Gemini image/BYOK LLM, OpenAI Chat/Responses, Replicate/fal 전체 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/gemini -run TestOfficialGeminiLLMGenerateContentSDKs
```

## 완료 조건

- [ ] Gemini 전용 immutable token price와 model limit이 관리됨
- [ ] Provider dispatch 전에 Wallet/quota/spend cap 최대 비용이 원자적으로 예약됨
- [ ] strict native usage가 exact integer 금액으로 Capture되고 차액이 반환됨
- [ ] cached/tool-use/thought token이 명시된 가격 규칙으로 중복 없이 계산됨
- [ ] confirmed non-2xx만 Release되고 timeout/missing/invalid usage는 reservation을 유지함
- [ ] replay와 reconciliation이 Provider/Ledger 중복 없이 exactly-once 수렴함
- [ ] OpenAI token 과금과 operation/price/idempotency identity가 격리됨
- [ ] credential과 prompt/tool/thought/output/내부 금액이 응답·로그·telemetry에 노출되지 않음
- [ ] 공식 SDK와 전체 unit/race/integration/장애 회귀가 통과함
- [ ] README와 멀티레포 handoff 및 검증 증거가 갱신됨

## 검증 증거

구현 PR에서 commit, CI run, fresh DB migration, 필수 명령과 결과를 기록한다.

## Rollback 계획

- billing-required deployment에서 Gemini LLM model/price publication을 비활성화해 Plan 042의 pre-dispatch fail-closed 상태로 복귀한다.
- 미정산 charge는 worker를 drain하거나 reservation을 유지한 채 manual review한다.
- additive price/charge/usage/Ledger evidence는 감사 보존 기간 동안 삭제하지 않는다.
- BYOK Gemini LLM과 기존 Gemini image route는 유지한다.

## 후속 작업

- Gemini native `streamGenerateContent` SSE와 disconnect settlement
- Anthropic Messages non-streaming token settlement
- cross-provider LLM routing과 fallback
