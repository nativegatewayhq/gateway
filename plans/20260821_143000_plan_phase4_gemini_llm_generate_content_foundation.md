---
id: gateway-20260821-042
title: Phase 4 Gemini Native LLM generateContent Foundation
status: completed
created_at: 2026-08-21T14:30:00+09:00
updated_at: 2026-08-21T15:40:00+09:00
owners:
  - gateway
initiative: phase-4-gemini-llm-generate-content-foundation
depends_on:
  - gateway-20260820-004
  - gateway-20260820-007
  - gateway-20260821-041
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 4 Gemini Native LLM generateContent Foundation

## 목적

공식 Google Gen AI Python/JavaScript SDK가 API key와 base URL만 변경하여 Gemini LLM `generateContent`를 호출하게 하고, 동일한 native endpoint를 사용하는 기존 Gemini 이미지 생성과 LLM operation을 모델 registry에서 명시적으로 분리한다.

## 배경

현재 Gateway는 `POST /v1beta/models/{model}:generateContent`를 native pass-through하지만 관리형 요청을 항상 `image.generate`로 분류한다. Gemini는 텍스트, 멀티모달 입력, function calling과 이미지 출력을 같은 `generateContent` wire contract로 표현하므로 request payload를 보고 operation을 추측하면 권한·가격·라우팅 경계가 흔들린다. 이 계획은 모델에 등록된 capability를 source of truth로 삼아 `chat.completions`와 `image.generate`를 분리하고, token settlement 전에는 관리형 LLM 요청을 fail closed한다.

참조 계약: [Gemini generateContent API](https://ai.google.dev/api/generate-content), [Google Gen AI SDK 문서](https://googleapis.github.io/python-genai/)

## 범위

- Gemini LLM용 `chat.completions` operation registry와 model capability
- 동일 `:generateContent` endpoint에서 model registry 기반 operation 선택
- 기존 Gemini 이미지 모델의 `image.generate` 동작과 과금 회귀 보존
- BYOK Gemini LLM request/response native byte pass-through
- `contents`, `systemInstruction`, `tools`, `toolConfig`, `generationConfig`, `safetySettings`, `cachedContent` 및 미래 JSON field 보존
- `x-goog-api-key`, query `key`, Authorization 입력을 service key로 인증하고 upstream Google credential로 교체
- API key model authorization, rate limit, network policy와 telemetry에 선택된 operation 적용
- LLM model allowlist/configuration과 ambiguous dual-operation model의 fail-closed startup validation
- token settlement 미지원 상태의 관리형 LLM pre-dispatch 거부
- Python/JavaScript 공식 SDK non-streaming conformance, README와 운영 계약

## 제외 범위

- `models.streamGenerateContent`와 `alt=sse`
- Gemini token 가격, reservation, usage extraction, Capture와 reconciliation
- cross-provider 변환, OpenAI-compatible Gemini 응답과 LLM fallback
- Gateway-side function/tool 실행
- Cached Content, Files, Batch, Live와 Interactions API
- 이미지 결과 저장을 LLM 응답에 적용하는 동작
- Gemini `models.list/get` facade

## 핵심 결정

### 1. Operation은 payload가 아니라 registry가 결정한다

- logical model은 정확히 하나의 Gemini `generateContent` operation으로 등록한다.
- LLM 모델은 `protocol=gemini`, `operation=chat.completions`, `media_type=json` capability를 가진다.
- 이미지 모델은 기존 `image.generate` capability를 유지한다.
- 같은 logical model에 두 operation이 동시에 등록되면 request 내용으로 추측하지 않고 설정 오류로 fail closed한다.
- operation 선택은 인증 후 body 해석과 Provider dispatch 전에 완료한다.

### 2. Native pass-through 경계

- 허용된 BYOK LLM 요청은 JSON body와 성공·오류 response bytes를 재직렬화하지 않는다.
- path model만 검증하고 configured provider model로 치환하며 query의 service credential은 upstream으로 전달하지 않는다.
- `Content-Type`, `Accept`, `User-Agent`, `x-goog-api-client`의 기존 안전한 전달 규칙을 유지한다.
- prompt, system instruction, tool argument/result, response candidate와 provider credential은 로그·metric·trace에 남기지 않는다.

### 3. 관리형 과금은 fail closed한다

- LLM capability로 선택된 요청에 billing-required mode가 적용되면 Google 호출 전에 명시적 `FAILED_PRECONDITION`을 반환한다.
- 이미지용 수량·해상도 가격을 LLM 요청에 적용하지 않는다.
- Wallet reserve, idempotency record, spend cap과 Provider health probe를 시작하지 않은 상태에서 종료한다.
- 후속 token settlement 계획이 operation별 immutable price와 `usageMetadata` 검증을 추가한 뒤에만 관리형 LLM을 활성화한다.

### 4. 호환성과 오류 계약

- Google의 native JSON response와 `google.rpc.Status` 계열 오류 body/status를 가능한 그대로 반환한다.
- unsupported stream path/query는 Provider dispatch 전에 명시적으로 거부한다.
- unknown model, capability ambiguity, disabled operation과 credential 부재는 서로 구분하되 내부 channel/credential identity를 노출하지 않는다.
- 기존 이미지 SDK fixture와 모든 OpenAI/async provider 경로는 영향을 받지 않아야 한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1beta/models/{model}:generateContent
Content-Type: application/json
x-goog-api-key: SERVICE_API_KEY
```

요청과 응답은 Google Gemini native JSON을 유지한다. Gateway 전용 field를 body에 삽입하지 않는다.

### 내부 인터페이스

- Gemini `generateContent` operation resolver
- Gemini LLM model registry/config parser
- operation-aware authorization, telemetry와 Provider execution metadata
- image 전용 billing pipeline과 LLM pass-through pipeline의 명시적 분기

### 데이터베이스 및 migration

- 이 foundation은 신규 영속 billing schema를 만들지 않는다.
- API key permission validation이 `gemini + chat.completions`를 허용하도록 additive constraint migration이 필요하면 추가한다.
- 기존 `gemini + image.generate` permission과 charge row는 변경하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

- `conformance`: Google Gen AI Python/JavaScript non-streaming text·system instruction·function declaration fixture
- `cloud`: Gemini LLM model allowlist와 billing-disabled rollout 설정
- `dashboard`: Gemini LLM capability/disabled managed billing 상태 표시

각 저장소는 같은 initiative로 자체 plan을 만들고 Gateway 내부 구현은 이 계획에서만 관리한다.

## 구현 순서

1. Gemini LLM operation/capability와 model configuration을 추가한다.
2. `generateContent` handler가 registry로 operation을 결정하도록 image 전용 가정을 격리한다.
3. BYOK native relay에 operation-aware auth, telemetry와 safe failure mapping을 적용한다.
4. billing-required LLM을 Provider dispatch 전 fail closed한다.
5. migration이 필요한 permission/quota validation을 additive 변경하고 이미지 회귀를 검증한다.
6. 단위·통합·공식 SDK conformance와 문서를 추가한다.

## 보안 및 과금 고려사항

- request/response body를 operation 판별, 로깅 또는 telemetry label 생성에 사용하지 않는다.
- service key가 query/header 어느 위치에 있어도 제거하고 configured Google key 하나만 upstream에 적용한다.
- ambiguous model은 permissive fallback하지 않는다.
- LLM 요청을 이미지 가격으로 reserve/capture하지 않으며 token 가격이 없을 때 관리형 호출을 무료 전달하지 않는다.
- Provider native error body를 전달할 때 credential-bearing header와 hop-by-hop header는 제거한다.

## 테스트 계획

### 단위 테스트

- LLM/image model operation 선택과 ambiguous/missing capability 거부
- operation-aware API key permission과 telemetry metadata
- arbitrary native JSON field와 binary-safe response byte 보존
- query/header service credential 제거와 로그 redaction
- LLM billing-required pre-dispatch failure 및 zero billing/provider calls
- 기존 Gemini 이미지 routing, pricing, storage와 idempotency 회귀

### 통합 테스트

- configured Gemini LLM model의 BYOK native round trip
- DB-backed API key allowlist에서 `gemini + chat.completions` 허용/거부
- LLM/image model이 동일 endpoint에서 독립 operation으로 분리됨
- billing-enabled deployment에서 이미지 요청만 기존 lifecycle을 수행함

### 호환성 및 장애 테스트

- Google Gen AI Python `models.generate_content`
- Google Gen AI JavaScript `models.generateContent`
- system instruction, multimodal parts와 function declarations의 native round trip
- upstream 400/401/429/500, timeout, cancellation과 oversized body/response
- OpenAI Chat/Responses, Gemini image, Replicate/fal 전체 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/gemini -run TestOfficialGeminiLLMGenerateContentSDKs
```

## 완료 조건

- [x] 공식 Google Gen AI Python/JavaScript SDK가 key/base URL 변경만으로 non-streaming LLM 요청에 성공함
- [x] Gemini image와 LLM operation이 payload 추측 없이 registry에서 분리됨
- [x] request/response native JSON bytes와 function calling field가 보존됨
- [x] API key permission, rate limit, network policy와 telemetry가 `gemini + chat.completions`를 사용함
- [x] 관리형 LLM 요청이 token 과금 없이 Provider로 전달되지 않음
- [x] 기존 Gemini 이미지 과금·저장·idempotency 동작이 회귀하지 않음
- [x] credential, prompt, output과 tool payload가 로그·telemetry에 노출되지 않음
- [x] 전체 unit/race/integration/공식 SDK 테스트가 통과함
- [x] README와 멀티레포 handoff가 갱신됨
- [x] 재현 가능한 검증 증거가 기록됨

## 검증 증거

- 구현 commit `47ed251`, PR [#52](https://github.com/nativegatewayhq/gateway/pull/52), squash merge `777db00`
- `GOCACHE=/private/tmp/nativegateway-go-cache make check`: gofmt, vet, 전체 race unit test와 모든 Gateway binary build 통과
- 전용 PostgreSQL `gateway_plan042`과 Redis에서 `make integration-test`: migration 000036, API key 권한, Gemini image와 전체 Gateway integration 통과
- `go test -tags=sdkconformance ./protocols/gemini -run TestOfficialGeminiLLMGenerateContentSDKs -count=1`: `google-genai` Python 2.19.0과 `@google/genai` JavaScript 공식 SDK 통과
- native request/response byte 보존, function declaration, operation-aware authorization과 managed pre-dispatch fail-closed 회귀 통과

## Rollback 계획

- Gemini LLM model allowlist를 비우고 LLM capability를 비활성화한다.
- `generateContent` handler를 기존 image-only operation resolver로 되돌리되 native image facade는 유지한다.
- additive permission migration은 호환성을 위해 남기고 신규 operation 사용만 중단한다.
- 이미 생성된 감사·rate-limit 기록은 삭제하지 않는다.

## 후속 작업

- Gemini `usageMetadata` 기반 token 가격·예약·Capture·reconciliation
- Gemini `streamGenerateContent` native SSE와 disconnect settlement
- Anthropic Messages non-streaming/streaming
- cross-provider LLM routing과 fallback
