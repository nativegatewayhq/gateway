---
id: gateway-20260821-045
title: Phase 4 Anthropic Messages Native Non-streaming Foundation
status: proposed
created_at: 2026-08-21T22:00:00+09:00
updated_at: 2026-08-21T22:00:00+09:00
owners:
  - gateway
initiative: phase-4-anthropic-messages-foundation
depends_on:
  - gateway-20260820-003
  - gateway-20260820-018
  - gateway-20260820-019
  - gateway-20260820-020
  - gateway-20260820-023
  - gateway-20260821-042
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 4 Anthropic Messages Native Non-streaming Foundation

## 목적

공식 Anthropic Python·TypeScript SDK가 API key와 base URL만 변경해 `POST /v1/messages`를 호출하도록 Anthropic native facade와 trusted Provider transport를 추가하고, credential·인증·모델 권한·body/header 보안 경계를 확립한다.

## 배경

Gateway는 OpenAI와 Gemini native LLM protocol을 지원하지만 Anthropic Messages route는 아직 없다. Anthropic SDK는 `x-api-key`, `anthropic-version`, optional `anthropic-beta`와 `/v1/messages` wire contract를 사용한다. token reservation과 usage settlement를 연결하기 전에 native request/response 보존, service credential 교체, exact model authorization과 managed-mode fail-closed 경계를 독립적으로 검증해야 한다.

참조 계약: [Anthropic Messages API](https://platform.claude.com/docs/en/api/messages), [Anthropic 공식 SDK](https://platform.claude.com/docs/en/cli-sdks-libraries/overview)

## 범위

- `POST /v1/messages` Anthropic native non-streaming facade
- official Python sync/async 및 TypeScript SDK의 API key/base URL 호환
- `x-api-key` 또는 `Authorization: Bearer` service credential 인증 후 Provider key로 교체
- `anthropic-version`, bounded `anthropic-beta`, content type, user-agent와 native body 전달
- strict top-level `model`과 `stream` envelope 검사
- exact Anthropic model registry와 API Key model authorization
- fixed trusted `https://api.anthropic.com` origin, redirect 거부와 single dispatch
- timeout, cancellation, connection failure와 Provider status의 native 오류 전달
- safe response header allowlist, bounded request/response relay와 credential redaction
- Provider health, rate limit, network authorization, telemetry와 README
- billing-required mode에서 managed Anthropic Messages pre-dispatch fail closed

## 제외 범위

- token price, Wallet/quota/spend-cap reservation과 usage settlement
- `stream=true`, native SSE와 disconnect settlement
- prompt caching·cache creation/read token 가격
- extended thinking, server tools와 tool별 별도 가격
- Message Batches, token counting, Files와 Models API
- AWS Bedrock, Google Vertex AI와 cross-provider conversion
- routing, fallback, idempotent response replay와 response transformation
- Dashboard/Cloud/Conformance 저장소 내부 구현

## 핵심 결정

### 1. Native protocol contract

- route는 정확히 `POST /v1/messages`이며 JSON body를 재직렬화하지 않는다.
- top-level `model`은 정확히 하나의 bounded string이고 `stream`은 absent 또는 exactly one `false`여야 한다.
- system, messages, content blocks, tools, tool choice, metadata와 future fields는 Gateway가 해석하거나 변경하지 않는다.
- Anthropic native status/body를 보존하고 Gateway 내부 metadata를 response에 삽입하지 않는다.

### 2. 인증과 version headers

- inbound `x-api-key`와 Bearer credential은 service key로만 해석하며 ambiguous credential은 거부한다.
- outbound request에서 inbound key, authorization, cookie와 sensitive query를 제거한 뒤 Anthropic channel credential만 `x-api-key`에 적용한다.
- `anthropic-version`은 정확히 하나의 bounded printable value가 필수다.
- `anthropic-beta`는 bounded size/count의 printable header만 pass-through하며 credential처럼 로그하지 않는다.

### 3. Model과 managed boundary

- `GATEWAY_ANTHROPIC_MESSAGES_MODELS` exact allowlist만 Messages operation으로 분류한다.
- API Key authorization dimension은 `protocol=anthropic`, `operation=messages.create`, exact model이다.
- billing-disabled BYOK mode만 Provider dispatch를 허용한다.
- billing-required mode는 body read, Provider health claim, credential lookup과 dispatch 전에 native `FAILED_PRECONDITION` 계열 오류로 fail closed한다.

### 4. Trusted transport와 errors

- production origin은 compile-time `https://api.anthropic.com`; client-controlled origin, redirect와 path joining을 허용하지 않는다.
- request timeout은 response header까지 bounded하며 non-stream body relay에도 전체 response limit을 적용한다.
- Gateway는 Provider request를 자동 retry하지 않는다. 공식 SDK retry는 native status contract에 따라 client가 결정한다.
- credential unavailable은 503, timeout은 504, cancellation은 499, connection failure는 502의 Anthropic-shaped Gateway error로 반환한다.

### 5. 보안과 observability

- service/provider key, authorization, cookie, prompt, system, tool arguments/results와 native response content를 로그·metric·trace attribute에 넣지 않는다.
- safe response headers는 content type, request ID, retry-after와 documented bounded rate-limit headers로 제한한다.
- logs와 telemetry는 protocol, operation, bounded model, status class, provider outcome과 duration만 사용한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/messages
Content-Type: application/json
x-api-key: SERVICE_API_KEY
anthropic-version: 2023-06-01
```

응답은 Anthropic native Message 또는 native error JSON이다.

### 내부 인터페이스

- `operations/anthropic` exact Messages registry
- `protocols/anthropic` authenticator, handler와 native error writer
- `providers/anthropic` trusted Messages executor
- `providercredentials.Anthropic` provider/channel scope
- config와 main/httpserver wiring

### 데이터베이스 및 migration

- schema migration은 없다.
- 기존 encrypted provider credential control plane의 provider allowlist가 DB constraint를 사용한다면 additive migration으로 `anthropic`을 추가한다.
- 기존 provider rows와 binary read path를 변경하거나 삭제하지 않는다.

### 멀티레포 handoff

- `conformance`: Anthropic Python sync/async와 TypeScript native response/error fixture
- `cloud`: Anthropic credential secret, exact model allowlist, request/body timeout 설정
- `dashboard`: Anthropic channel/model readiness 표시 계약

각 저장소는 동일 initiative의 독립 plan에서 내부 구현을 관리한다.

## 구현 순서

1. Anthropic provider ID, configuration과 exact model registry를 추가한다.
2. strict route/envelope/version validation과 service-key authentication을 구현한다.
3. trusted Provider executor, credential replacement와 native response relay를 연결한다.
4. model/network/rate-limit/health/telemetry와 managed fail-closed 경계를 연결한다.
5. README, unit/integration 및 공식 Python/TypeScript SDK conformance를 추가한다.
6. 전체 protocol·credential·security 회귀를 검증한다.

## 보안 및 과금 고려사항

- managed mode에서 가격·예약 없이 Anthropic 요청을 dispatch하지 않는다.
- inbound service key를 Provider로 보내거나 Provider key를 client response/log에 노출하지 않는다.
- duplicate/ambiguous auth와 version header, CR/LF, oversized header/body를 pre-dispatch 거부한다.
- timeout과 connection loss를 과금 가능한 known failure로 해석하지 않으며 이 계획에서는 charge를 생성하지 않는다.
- user-controlled URL, redirect, proxy destination과 response header injection을 허용하지 않는다.

## 테스트 계획

### 단위 테스트

- method/path/content-type, model/stream duplicate·type·range
- x-api-key/Bearer/ambiguous credential와 version/beta header validation
- exact model authorization, billing-required pre-dispatch fail closed
- trusted origin/path, redirect 거부, sensitive header/query 제거와 provider key replacement
- native success/error body 및 safe response header 보존
- timeout/cancel/connection mapping, body bounds와 log redaction

### 통합 테스트

- stored service key tenant/network/model authorization 후 single Provider dispatch
- encrypted Anthropic channel credential lookup과 rotation boundary
- rate-limit/health denial의 zero body read·zero dispatch
- Provider 400/401/429/500 native response와 no Gateway retry
- existing OpenAI/Gemini/image/job routes 회귀

### 호환성 테스트

- official Anthropic Python sync/async `messages.create`
- official Anthropic TypeScript `messages.create`
- text, image, tool-use와 future content block의 native typed decoding
- API key와 base URL만 변경한 success/error behavior

### 필수 검증 명령

```text
make check
make integration-test
go test -tags=sdkconformance ./protocols/anthropic -run TestOfficialAnthropicMessagesSDKs
```

## 완료 조건

- [ ] 공식 Anthropic Python/TypeScript SDK가 key/base URL 변경만으로 native Message를 소비함
- [ ] `/v1/messages` request/response와 text/tool/future content block이 변형 없이 전달됨
- [ ] service credential이 Anthropic channel key로 교체되고 client/log에 Provider key가 노출되지 않음
- [ ] version/beta header, exact model과 API Key authorization이 pre-dispatch 검증됨
- [ ] billing-required mode가 body/credential/Provider side effect 전에 fail closed함
- [ ] trusted origin, redirect 거부, single dispatch와 timeout/cancel/error mapping이 검증됨
- [ ] network/rate-limit/health denial이 Provider 호출과 secret-bearing body read를 발생시키지 않음
- [ ] native error status/body와 safe response headers가 유지됨
- [ ] 전체 unit/race/integration/공식 SDK/security 회귀가 통과함
- [ ] README, multi-repo handoff와 재현 가능한 검증 증거가 갱신됨

## 검증 증거

구현 PR에서 commit, CI run, 공식 SDK 버전, 필수 명령과 결과를 기록한다.

## Rollback 계획

- Anthropic model allowlist를 비워 route를 pre-dispatch unsupported 상태로 되돌린다.
- provider credential을 비활성화하고 health/channel readiness에서 제거한다.
- additive provider enum migration과 encrypted audit rows는 보존한다.
- 기존 OpenAI/Gemini/image/job routes는 유지한다.

## 후속 작업

- Anthropic Messages non-streaming token usage billing and settlement
- Anthropic Messages native SSE and disconnect settlement
- prompt cache creation/read token pricing
- cross-provider LLM routing and fallback
