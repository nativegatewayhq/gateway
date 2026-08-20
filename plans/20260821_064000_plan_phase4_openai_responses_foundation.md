---
id: gateway-20260821-039
title: Phase 4 OpenAI Responses Native Non-streaming Foundation
status: completed
created_at: 2026-08-21T06:40:00+09:00
updated_at: 2026-08-22T10:30:00+09:00
owners:
  - gateway
initiative: phase-4-openai-responses-foundation
depends_on:
  - gateway-20260821-036
  - gateway-20260821-037
  - gateway-20260821-038
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 4 OpenAI Responses Native Non-streaming Foundation

## 목적

공식 OpenAI Python/JavaScript SDK가 API Key와 Base URL만 변경해 non-streaming `POST /v1/responses`를 호출할 수 있도록, Responses request/response를 변환하지 않는 native protocol facade와 독립 operation capability를 제공한다.

## 배경

Chat Completions foundation과 token settlement는 완성됐지만 OpenAI의 최신 agent/tool workflow는 Responses API를 사용한다. Responses는 `input`, typed output items, built-in tools, reasoning, previous response identity와 별도 usage schema를 가지므로 Chat operation으로 가장하거나 Chat request로 변환하면 데이터 손실과 잘못된 과금이 발생한다. 먼저 BYOK native transport와 capability 경계를 확립하고, 저장·retrieval·streaming·usage 정산은 후속 plan에서 명시적으로 추가한다.

## 범위

- `POST /v1/responses` non-streaming native facade
- 공식 OpenAI Python/JavaScript SDK Base URL 호환
- Responses 전용 operation `responses.create`
- Responses model capability/allowlist와 `/v1/models` 병합
- service API Key, network policy, rate limit와 model authorization
- trusted OpenAI origin, Provider credential replacement와 redirect/retry 차단
- JSON request/response/error raw pass-through
- bounded request/response, timeout, cancellation과 header allowlist
- tool definitions, typed input/output items와 reasoning fields의 무변형 전달
- bounded telemetry와 credential/prompt redaction
- billing-required mode fail-closed

## 제외 범위

- `stream=true` Responses SSE
- Responses token usage 가격·Wallet·Ledger 정산
- `GET /v1/responses/{response_id}`, delete, cancel과 input-items 조회
- `store=true`, background mode와 Provider-side durable response lifecycle 지원 보장
- previous response replay와 Gateway-side conversation state
- built-in web/file/computer/code-interpreter tool의 별도 비용
- file upload, vector store와 Batch API
- cross-provider Responses 변환, routing과 fallback

## 설계 및 구현 순서

### 1. Operation과 capability 분리

- `operations/responses` registry를 Chat registry와 분리한다.
- exact logical model은 Provider, channel과 `responses.create` capability를 가진다.
- Chat model 설정을 암묵적으로 Responses에 재사용하지 않으며 별도 model allowlist를 요구한다.
- `/v1/models`는 이미지·Chat·Responses model을 중복 ID 없이 deterministic하게 병합한다.

### 2. Native request boundary

- top-level JSON object에서 exact `model`과 optional `stream`만 strict envelope parsing한다.
- `stream=true`는 dispatch 전에 native-shaped 명시적 unsupported 오류로 거부한다.
- unknown/future fields, `input`, `instructions`, `tools`, `tool_choice`, `reasoning`, `text`, metadata와 typed items는 decode/re-encode하지 않는다.
- request body와 media type은 Provider transport에 원본 bytes로 전달한다.

### 3. Authentication과 authorization

- 기존 service API Key authentication, IP/CIDR policy, project/model permission과 rate limit 순서를 재사용한다.
- authorization dimension은 `openai + responses.create + logical model`이다.
- Provider credential은 trusted outbound request에만 주입하고 inbound authorization/cookie/query secret을 제거한다.
- 현재 key ownership과 model authorization 실패는 Provider dispatch 전에 종료한다.

### 4. Provider transport

- 고정 `https://api.openai.com/v1/responses` origin만 호출한다.
- redirect와 automatic retry를 금지하고 timeout/cancel/connection 오류를 bounded native error로 매핑한다.
- `Content-Type`, `Accept`, safe `User-Agent`만 전달하며 Provider credential/error 원문은 로그하지 않는다.
- response는 bounded buffer 후 status, allowlisted headers와 body bytes를 그대로 반환한다.

### 5. Billing fail-closed

- `GATEWAY_BILLING_MODE=required`에서 Responses model 활성화는 configuration error로 차단한다.
- Wallet, Chat token price와 image charge를 우회해 무료 dispatch하는 경로를 만들지 않는다.
- 후속 Responses usage settlement plan이 accepted/implemented된 뒤에만 managed mode를 연다.

### 6. Observability와 문서

- protocol `openai`, operation `responses.create`, Provider와 bounded outcome만 기록한다.
- model, tenant, request/response/tool content, response ID, credential과 raw error는 metric label/log에 넣지 않는다.
- README에 지원 path, BYOK-only 상태, unsupported lifecycle/streaming과 공식 SDK 예제를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/responses
Content-Type: application/json
Authorization: Bearer SERVICE_API_KEY
```

### 내부 인터페이스

- `responses.Registry.Resolve(model)`
- `openai.ResponsesHandler`
- `openai.ResponsesExecutor.Create(ctx, request)`
- model listing의 Responses source 병합

### 설정

```text
GATEWAY_OPENAI_RESPONSES_MODELS
GATEWAY_OPENAI_RESPONSES_REQUEST_TIMEOUT
GATEWAY_OPENAI_RESPONSES_MAX_BODY_BYTES
```

### 데이터베이스

- service API Key model permission constraint에 `openai + responses.create`를 additive migration으로 추가한다.
- 가격, charge, usage와 conversation table은 이 plan에서 만들지 않는다.

## 보안 고려사항

- input, instructions, tool arguments/output, reasoning과 response content를 구조화 로그에 기록하지 않는다.
- response ID를 Gateway 관리 identity처럼 신뢰하거나 tenant 간 조회에 사용하지 않는다.
- unknown URL/file reference를 Gateway가 fetch하지 않아 SSRF 경계를 확장하지 않는다.
- response body 상한을 초과하면 부분 native payload를 반환하지 않는다.
- Provider redirects는 credential 탈취 방지를 위해 따르지 않는다.

## 테스트 계획

### 단위 테스트

- strict model/stream envelope, duplicate/trailing JSON과 future field preservation
- tool definitions, function arguments, reasoning/text options byte preservation
- auth/network/rate/model denial의 Provider zero-call
- timeout/cancel/credential unavailable/oversized response mapping
- response header와 log redaction

### 통합 테스트

- PostgreSQL tenant/model permission isolation
- billing-required configuration fail-closed
- `/v1/models` image/Chat/Responses deterministic merge와 deduplication
- Gateway restart와 Provider credential lifecycle 회귀

### 공식 SDK 및 회귀

- OpenAI Python `client.responses.create(...)`
- OpenAI JavaScript `client.responses.create(...)`
- typed output text와 function-call item 보존
- Chat non-stream/stream, image, Replicate/fal와 management API 회귀

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAIResponsesSDKs
```

## 완료 조건

- [x] 공식 OpenAI Python/JavaScript SDK가 Key와 Base URL만 변경해 `/v1/responses`를 호출함
- [x] request와 success/error response가 native bytes로 보존됨
- [x] typed input/output, tool과 reasoning future fields가 손실되지 않음
- [x] auth/network/rate/model/config failure가 Provider 호출 전에 종료됨
- [x] trusted origin과 Provider credential 격리가 유지되고 redirect/retry가 없음
- [x] billing-required mode에서 미정산 Responses dispatch가 불가능함
- [x] `/v1/models`가 Responses capability를 deterministic하게 노출함
- [x] prompt, tool content, credential, response ID와 raw error가 로그·telemetry에 없음
- [x] 전체 unit/race/integration/공식 SDK 회귀가 통과함
- [x] README와 Conformance/Cloud handoff가 갱신됨

## 검증 증거

- 구현 및 머지: `52e703f` (`feat: add OpenAI Responses foundation`, PR #46)
- `make check`
- `TEST_DATABASE_URL=... TEST_REDIS_URL=... make integration-test`
- `go test -tags=sdkconformance ./protocols/openai -run TestOfficialOpenAIResponsesSDKs`
- fresh PostgreSQL schema에서 migration 000001~000033 반복 적용
- raw tool/future field request와 typed output response byte-preservation tests
- billing-required configuration fail-closed 및 trusted Provider origin tests

## Rollback 계획

- Responses model allowlist를 제거해 route를 비활성화한다.
- additive permission constraint는 유지해 구 binary와 충돌하지 않게 한다.
- Chat, Images와 async Provider route는 영향 없이 유지한다.

## 후속 작업

- Responses token usage billing과 immutable price mapping
- Responses SSE streaming 및 disconnect settlement
- response retrieve/delete/cancel lifecycle와 tenant-safe identity
- built-in tool 별도 usage/cost settlement
- tool calling conformance와 policy controls
