---
id: gateway-20260821-036
title: Phase 4 OpenAI Chat Completions Non-streaming Foundation
status: accepted
created_at: 2026-08-21T01:11:41+09:00
updated_at: 2026-08-21T01:11:41+09:00
owners:
  - gateway
initiative: phase-4-openai-chat-completions-foundation
depends_on:
  - gateway-20260820-002
  - gateway-20260820-003
  - gateway-20260820-007
  - gateway-20260820-018
  - gateway-20260820-019
  - gateway-20260820-020
  - gateway-20260820-023
  - gateway-20260820-026
  - gateway-20260820-028
  - gateway-20260821-035
supersedes: []
affected_repos:
  - gateway
  - conformance
---

# Phase 4 OpenAI Chat Completions Non-streaming Foundation

## 목적

공식 OpenAI Python/JavaScript SDK가 API Key와 Base URL만 변경해 비스트리밍 `POST /v1/chat/completions`를 호출할 수 있도록, 이미지 Operation과 분리된 Chat capability 및 OpenAI native pass-through 경계를 제공한다.

## 배경

Phase 0~3은 이미지와 비동기 Provider 경로, 인증·라우팅·과금·관측성 기반을 완성했다. 최초 로드맵의 다음 단계는 LLM 지원이다. Chat Completions, Responses, Gemini, Anthropic, SSE, tool calling과 token 과금을 한 번에 구현하면 native wire 호환성과 정산 실패 영역이 결합된다. 첫 계획에서는 가장 널리 검증 가능한 OpenAI Chat Completions의 비스트리밍 native path를 수립하고, 후속 streaming·billing 계획이 재사용할 typed boundary를 만든다.

## 범위

- `POST /v1/chat/completions` OpenAI native facade
- OpenAI 공식 SDK가 보내는 JSON 요청의 bounded native pass-through
- Chat 전용 operation request/result와 capability registry
- 명시적으로 설정된 OpenAI chat model allowlist 및 `/v1/models` 합성 노출
- 기존 service API key 인증, network restriction, rate limit과 model authorization 재사용
- OpenAI provider credential control plane 및 provider health gate 재사용
- upstream status/header/body의 안전한 native response 전달
- request ID, protocol/operation, status class와 provider outcome telemetry
- Python/JavaScript Conformance 저장소가 사용할 공개 계약 및 fixture handoff

## 제외 범위

- `stream=true`, SSE relay와 연결 취소 정산
- OpenAI Responses API
- Gemini `generateContent` LLM 의미와 Anthropic Messages
- tool call 의미 변환 또는 cross-provider protocol conversion
- token usage 가격 계산, Wallet Reserve/Capture/Release와 fallback
- structured output schema 검증, audio/image attachment upload와 files API
- prompt caching, batch, fine-tuning과 embeddings
- 대화 내용 저장, prompt logging 또는 moderation

## 설계 및 구현 순서

### 1. Chat operation과 capability 분리

- `operations/chat`에 Chat 전용 operation, media/content capability와 model registry contract를 정의한다.
- image registry와 Chat registry를 하나의 거대 요청 DTO로 합치지 않는다.
- model ID는 exact allowlist로 로드하고 protocol `openai`, operation `chat.completions` 권한을 별도로 검사한다.
- `/v1/models`는 이미지와 Chat model을 중복 없이 합성하되 기존 이미지 응답 계약을 깨지 않는다.

### 2. Native request facade

- exact route `POST /v1/chat/completions`만 추가하고 다른 `/v1/chat/*` path는 수락하지 않는다.
- content type은 JSON으로 제한하고 compressed body, chunked upload, maximum body bytes와 trailing JSON을 명시적으로 처리한다.
- 최소 envelope에서 `model`과 `stream`만 검사하며 unknown native fields와 message/tool payload는 변형하지 않고 upstream으로 전달한다.
- `stream` 누락 또는 `false`만 허용하고 `true`는 Provider 호출 전 bounded native error로 거부한다.

### 3. OpenAI provider adapter

- credential registry에서 선택한 OpenAI channel credential로 Authorization을 교체한다.
- 고정된 HTTPS origin과 escaped path로만 요청해 SSRF와 path confusion을 방지한다.
- hop-by-hop, credential, cookie와 internal tracing header를 양방향 allowlist에서 제외한다.
- body 크기, timeout과 close/drain behavior를 제한하고 Provider credential이나 prompt를 오류/로그에 포함하지 않는다.

### 4. Error, cancellation과 observability

- 인증·권한·rate limit 오류는 기존 OpenAI native envelope를 유지한다.
- upstream 4xx/5xx body는 크기 제한 아래 native로 전달하되 credential-bearing header는 제거한다.
- client cancellation은 upstream context에 즉시 전파하고 retry/fallback을 수행하지 않는다.
- telemetry cardinality는 `openai`, `chat.completions`, bounded route/status/outcome만 사용하며 model, prompt, tool arguments와 API Key를 label에 넣지 않는다.

### 5. Configuration, docs와 conformance handoff

- chat route는 명시적 model allowlist가 있을 때만 활성화하고 빈 설정에서 기존 배포 동작을 유지한다.
- body limit과 timeout은 bounded configuration으로 제공한다.
- README에 SDK Base URL/Key 변경 예제, non-streaming 제한과 model 권한 설정을 기록한다.
- Conformance에 Python/JavaScript official SDK request/response, unknown-field preservation, error와 cancellation fixture 계약을 전달한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/chat/completions
Authorization: Bearer SERVICE_API_KEY
Content-Type: application/json
```

요청과 응답은 OpenAI Chat Completions native JSON wire를 보존한다. 이 계획에서는 `stream: true`를 지원하지 않는다.

### 내부 인터페이스

- `operations/chat` model/capability registry
- OpenAI Chat native facade와 bounded executor interface
- 기존 OpenAI models handler가 image/chat model source를 합성하는 read contract

### 데이터베이스 및 migration

없음. Model allowlist와 provider channel은 기존 configuration/control-plane 계약을 사용한다. Token 가격과 charge schema 변경은 후속 계획에서 additive migration으로 수행한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 initiative `phase-4-openai-chat-completions-foundation`으로 OpenAI Python/JavaScript SDK 비스트리밍 검증 계획을 소유한다.
- Gateway는 route, auth, native error, supported field와 maximum payload 계약을 제공한다.
- Dashboard/Cloud 변경은 이 계획에 필요하지 않으며 Chat 가격과 usage가 도입될 때 별도 계획으로 연결한다.

## 보안 및 과금 고려사항

- service/provider API Key 원문, Authorization, prompt, messages와 tool arguments를 로그·trace·metric에 기록하지 않는다.
- API Key의 organization/project/key identity, network, rate limit과 exact model permission을 Provider 호출 전에 검증한다.
- request URL은 operator-configured OpenAI HTTPS origin 외 주소를 사용할 수 없고 사용자 JSON의 URL은 Gateway가 fetch하지 않는다.
- 이 계획은 BYOK native execution foundation이며 Wallet/Ledger를 변경하지 않는다. 관리형 유료 Chat 활성화는 token usage 과금 계획 전에는 허용하지 않는다.
- retry/fallback은 중복 LLM 실행과 비용 위험 때문에 제외하며 timeout을 성공/실패로 추측해 정산하지 않는다.

## 테스트 계획

### 단위 테스트

- method, content type, body limit, malformed/trailing JSON과 `stream:true` 거부
- model extraction, exact allowlist와 API Key model authorization
- unknown request field/message/tool JSON byte preservation
- header allowlist/redaction과 OpenAI native error envelope
- telemetry route/operation cardinality와 secret/prompt 비노출

### 통합 테스트

- service API key PostgreSQL authentication과 OpenAI credential control-plane selection
- network/rate-limit/model policy 거부 시 Provider zero-call
- mock OpenAI upstream의 success, 400, 401, 429, 500, oversized response와 timeout
- `/v1/models` image/chat 합성, stable ordering과 duplicate elimination

### 호환성 및 장애 테스트

- OpenAI Python/JavaScript SDK에서 Base URL/Key만 변경한 비스트리밍 request
- client cancellation 시 upstream context cancellation
- Provider connection reset와 timeout에서 bounded 502/504 및 credential 비노출
- 기존 image, Gemini, Replicate/fal facade와 management API 회귀

### 필수 검증 명령

```text
make check
make integration-test
```

## 완료 조건

- [ ] 공식 OpenAI Python/JavaScript SDK의 비스트리밍 Chat Completions가 Base URL/Key 변경만으로 동작함
- [ ] `stream:true`와 미지원 route가 Provider 호출 전에 명시적으로 거부됨
- [ ] Chat model capability와 API Key exact model authorization이 image operation과 분리됨
- [ ] native request/response와 unknown JSON field가 불필요하게 변환되지 않음
- [ ] service/provider credential, prompt와 tool arguments가 응답·로그·telemetry에 노출되지 않음
- [ ] timeout/cancellation/oversized body와 upstream 오류가 bounded하게 처리됨
- [ ] `/v1/models`가 기존 image 계약을 깨지 않고 Chat model을 합성함
- [ ] Wallet/Ledger mutation과 unsafe retry/fallback이 발생하지 않음
- [ ] 전체 unit/race/integration test와 기존 protocol 회귀 검사가 통과함
- [ ] README와 Conformance handoff가 갱신되고 검증 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Chat model allowlist를 제거하면 route를 비활성화하고 기존 image/async route만 유지한다.
- 구현 commit은 schema migration이 없으므로 handler mount, chat registry와 adapter를 함께 revert할 수 있다.
- rollback 중에도 provider credential, API Key와 기존 `/v1/models` image 항목은 변경하지 않는다.

## 후속 작업

- OpenAI Chat token usage 기반 가격·Reserve/Capture/Release
- SSE streaming relay, disconnect settlement와 usage reconciliation
- OpenAI Responses API
- Gemini LLM `generateContent`와 Anthropic Messages
- tool calling conformance와 cross-provider LLM fallback
