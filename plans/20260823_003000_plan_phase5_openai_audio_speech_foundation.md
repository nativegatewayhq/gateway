---
id: gateway-20260823-054
title: Phase 5 OpenAI Audio Speech Native Foundation
status: accepted
created_at: 2026-08-23T00:30:00+09:00
updated_at: 2026-08-23T00:30:00+09:00
owners:
  - gateway
initiative: phase-5-openai-audio-speech-foundation
depends_on:
  - gateway-20260820-003
  - gateway-20260820-007
  - gateway-20260820-016
  - gateway-20260820-028
  - gateway-20260822-053
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 5 OpenAI Audio Speech Native Foundation

## 목적

공식 OpenAI Python·JavaScript SDK가 API Key와 Base URL만 변경해 `POST /v1/audio/speech`를 호출하고, Gateway가 요청을 bounded native pass-through로 OpenAI에 전달해 음성 바이트를 스트리밍 반환하는 최초 Audio Operation 기반을 구축한다.

## 배경

Phase 5의 영상 기반과 관리형 결과 저장은 완료됐다. 다음 로드맵 항목은 Audio Speech와 Transcription이며, 먼저 JSON 입력과 binary 출력인 Speech를 독립 계획으로 고정한다. Speech 응답은 JSON snapshot이 아니라 장시간 전송될 수 있는 audio stream이므로 기존 image 및 chat execution 경계를 그대로 재사용할 수 없다.

현재 공식 OpenAI Audio API는 `POST /v1/audio/speech`에서 text, model, voice와 선택적 출력 형식을 받아 생성된 audio content를 반환한다. 이 계획은 현재 SDK wire 형식을 그대로 유지하며 모델·voice 목록을 Gateway가 임의로 재정의하지 않는다.

참조 계약:

- [OpenAI Audio API reference](https://platform.openai.com/docs/api-reference/audio)
- `gateway-20260820-003` Provider credential security boundary
- `gateway-20260820-007` capability registry
- `gateway-20260820-016` Provider channel routing
- `gateway-20260820-028` bounded telemetry

## 범위

- `audio.speech` Operation과 modality별 capability registry
- OpenAI native `POST /v1/audio/speech` facade
- 공식 SDK가 생성하는 JSON request 및 binary response contract
- bounded request body, duplicate JSON key, model/input/voice 기본 구조 검증
- selected OpenAI channel credential 주입과 고정 upstream origin/path
- hop-by-hop·credential header 제거 및 native safe response header allowlist
- response body를 전체 메모리에 적재하지 않는 downstream streaming
- upstream status/error body의 bounded native 전달
- client disconnect, upstream timeout, short write와 provider panic 분류
- API Key model authorization, rate limit, request ID, readiness와 bounded telemetry
- provider mode만 제공하는 최초 foundation; Speech 과금은 후속 계획으로 분리
- Python·JavaScript 공식 OpenAI SDK conformance fixture

## 제외 범위

- `/v1/audio/transcriptions`, `/v1/audio/translations`
- Realtime Audio, WebSocket, WebRTC와 duplex streaming
- Speech token/character/second 기반 Wallet 예약·Capture·환불
- 생성 음성의 S3/R2 저장, CDN, range request와 download authorization
- custom voice 생성·consent·목록 API
- cross-provider TTS 변환과 fallback
- response audio transcoding, waveform 검사와 duration 추출
- dashboard UI 및 cloud infrastructure 구현

## 핵심 결정

### 1. OpenAI native wire를 보존한다

- 공개 경로와 SDK method는 `POST /v1/audio/speech` 그대로 유지한다.
- Gateway는 model, voice, input, response format 등 알려진 필드를 다른 공통 schema로 변환하지 않는다.
- 알 수 없는 확장 필드는 size/security 경계 안에서 pass-through하여 SDK·Provider 진화를 막지 않는다.
- Provider error status와 bounded JSON/text body도 민감 header만 제거하고 가능한 한 native로 반환한다.

### 2. Audio response는 streaming 전용 실행 경계다

- upstream response를 `[]byte`나 Billing response snapshot으로 완전히 버퍼링하지 않는다.
- header 검증 후 `io.CopyBuffer` 계열로 bounded chunk streaming하고 flush 가능한 writer를 지원한다.
- maximum total response bytes, idle/complete timeout과 허용 MIME을 적용해 무제한 stream을 차단한다.
- downstream write 실패나 cancellation은 재시도·fallback하지 않는다. 일부 audio를 보낸 뒤 다른 Provider 응답으로 교체할 수 없기 때문이다.

### 3. fallback은 response commit 전까지만 허용한다

- 최초 foundation은 OpenAI Provider의 fixed/priority channel 선택을 사용한다.
- credential 누락, circuit-open 등 dispatch 전 실패만 다음 channel 후보로 이동할 수 있다.
- upstream 요청을 전송했거나 response header/body를 받은 뒤에는 자동 재호출하지 않아 중복 생성과 비용 불확실성을 만들지 않는다.

### 4. Content-Type과 공개 header를 fail closed 검증한다

- 성공 응답은 설정된 audio MIME allowlist 또는 SDK가 지원하는 명시적 binary format만 허용한다.
- `Content-Type`, bounded `Content-Length`, `Content-Disposition`, `Cache-Control`만 필요한 범위에서 전달한다.
- `Set-Cookie`, upstream request ID, server, credential·signed URL 성격 header와 hop-by-hop header는 제거한다.
- body 크기가 명시된 상한을 초과하거나 MIME이 일치하지 않으면 downstream commit 전에 거부한다.

## 설계 및 구현 순서

### 1. Audio Operation과 capability

- `operations/audio`에 Speech operation, model capability와 registry를 추가한다.
- protocol, operation, model과 provider/channel 조합을 검증한다.
- `/v1/models` projection에서 Audio capability를 기존 image/video/LLM 모델과 충돌 없이 노출한다.

### 2. OpenAI Speech Provider adapter

- 기존 credential registry와 selected channel을 사용해 고정 OpenAI endpoint로 요청한다.
- public request credential/header를 제거하고 Provider credential만 outbound request에 주입한다.
- request/response body, input text, voice와 audio content를 log·metric·trace에 기록하지 않는다.

### 3. Native facade와 safe parsing

- method/path/content-type/body limit를 먼저 검증한다.
- duplicate key를 거부하는 bounded JSON scanner로 model, input, voice를 구조 검증한다.
- API Key model permission과 distributed rate limit을 Provider dispatch 전에 적용한다.

### 4. Streaming response bridge

- upstream header/status를 검증한 다음 response를 한 번만 commit한다.
- bounded buffer로 audio bytes를 streaming하고 cancellation을 upstream context로 전파한다.
- streaming 시작 전 Provider 오류만 bounded native error response로 반환한다.

### 5. 운영·호환성

- timeout/body-size/MIME 설정과 provider enablement를 fail-closed load한다.
- protocol/operation/provider/stage/outcome만 telemetry dimension으로 사용한다.
- 공식 Python·JavaScript SDK에서 Base URL과 Key만 바꾼 Speech create 호출을 검증한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/audio/speech
Authorization: Bearer SERVICE_KEY
Content-Type: application/json
```

성공 시 OpenAI native audio bytes와 안전한 content headers를 반환한다. Gateway 전용 envelope나 Job ID를 추가하지 않는다.

### 내부 인터페이스

- `audio.Operation = audio.speech`
- `audio.Registry.Resolve(protocol, operation, model)`
- `openai.AudioSpeechExecutor.Execute(ctx, request) -> streaming response`
- streaming writer는 response commit 여부와 전송 byte 수를 bounded result로 반환한다.

### 데이터베이스 및 migration

최초 foundation은 신규 과금 원장을 만들지 않는다. 필요 시 capability/model permission constraint와 Provider channel seed만 additive migration으로 확장한다. Speech 과금 schema와 response evidence는 후속 계획이 소유한다.

### 다른 저장소 계약

- `conformance`: 공식 OpenAI Python·JavaScript Speech create, binary integrity, error와 disconnect fixture
- `cloud`: OpenAI Audio enablement, timeout/body limits와 secret-managed credential 배포
- `dashboard`: 후속 과금 계획 전에는 Audio 사용량을 추정 비용으로 표시하지 않음

## 보안 및 과금 고려사항

- 사용자 input text, voice/custom voice ID와 생성 audio는 민감 content로 취급하고 observability에 남기지 않는다.
- public Base URL, Host, Authorization과 forwarding headers가 upstream origin·credential 선택에 영향을 주지 못한다.
- unknown-length 또는 chunked audio는 streamed byte counter로 상한을 강제한다.
- downstream에 한 byte라도 전달한 뒤에는 retry/fallback하지 않는다.
- 과금이 없는 foundation에서는 Billing required 모드에서 Speech를 활성화하지 않거나 명시적 unbilled 개발 정책으로만 허용한다.
- Provider 호출 성공 여부가 불명확한 timeout은 후속 Billing 계획 전까지 metric/error category로만 기록한다.

## 테스트 계획

### 단위 테스트

- method/path/content-type, duplicate key, model/input/voice와 request size 검증
- credential/header redaction과 고정 origin/path
- success/error MIME, Content-Length, oversized/unknown-length stream
- streaming copy, flush, cancellation, short write와 commit 후 retry 금지
- model authorization 및 disabled/unbilled policy

### 통합 테스트

- mock OpenAI server의 binary chunk streaming과 SHA-256 일치
- slow body, mid-stream disconnect, timeout, 401/429/5xx와 invalid MIME
- rate limit/model permission/credential channel 경계
- 기존 OpenAI image/chat/responses와 Runway route 회귀

### SDK 호환성 테스트

- OpenAI Python `client.audio.speech.create(...)`
- OpenAI JavaScript `openai.audio.speech.create(...)`
- Base URL과 API Key 이외 SDK 코드 변경 없음
- SDK streaming/download helper가 native headers와 binary body를 처리함

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [ ] 공식 OpenAI Python·JavaScript SDK가 Key와 Base URL만 변경해 Speech를 호출함
- [ ] 성공 audio가 전체 메모리 buffering 없이 byte-for-byte streaming됨
- [ ] request/response size, timeout, MIME와 header 경계가 fail closed함
- [ ] credential, input text, voice/custom voice ID와 audio content가 노출되지 않음
- [ ] response commit 뒤 retry/fallback이 발생하지 않음
- [ ] disabled/Billing-required 정책이 무과금 Provider 호출을 방지함
- [ ] 기존 OpenAI image/chat/responses와 Runway 동작이 회귀하지 않음
- [ ] 전체 unit/race/integration/SDK 검사가 통과함
- [ ] README와 conformance/cloud/dashboard handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Audio Speech enable flag를 비활성화해 신규 route 등록과 dispatch를 중단한다.
- additive capability/permission migration은 유지해 downgrade 혼합 배포의 schema 충돌을 방지한다.
- 기존 image, LLM, async video route와 Provider credential은 영향 없이 계속 동작한다.

## 후속 작업

- OpenAI Audio Speech usage pricing, Wallet reservation과 disconnect reconciliation
- OpenAI Audio Transcriptions native multipart foundation
- Audio Transcription usage settlement
- managed audio storage/CDN 및 range delivery
- additional TTS Provider adapter와 protocol-compatible routing
- OpenAI Audio Translations와 Realtime Audio
