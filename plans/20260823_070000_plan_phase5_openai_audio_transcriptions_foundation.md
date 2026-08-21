---
id: gateway-20260823-056
title: Phase 5 OpenAI Audio Transcriptions Native Multipart Foundation
status: accepted
created_at: 2026-08-23T07:00:00+09:00
updated_at: 2026-08-23T07:00:00+09:00
owners:
  - gateway
initiative: phase-5-openai-audio-transcriptions-foundation
depends_on:
  - gateway-20260820-004
  - gateway-20260820-011
  - gateway-20260820-028
  - gateway-20260823-054
  - gateway-20260823-055
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 OpenAI Audio Transcriptions Native Multipart Foundation

## 목적

공식 OpenAI Python·JavaScript SDK가 API Key와 Base URL만 변경해 `POST /v1/audio/transcriptions`를 호출할 수 있도록, bounded multipart parsing, 고정 OpenAI origin, credential 교체, native JSON 응답과 장애 경계를 갖춘 BYOK transcription foundation을 구현한다.

## 배경

Speech는 JSON 입력과 binary 출력이지만 transcription은 대용량 audio file을 multipart로 입력하고 JSON 또는 text/SSE 계열 결과를 반환한다. 기존 image edit multipart 구현을 그대로 복사하면 이미지 전용 필드·크기·응답 가정이 섞이고, request 전체를 메모리에 적재하면 Phase 5의 대용량 파일 원칙을 위반한다.

첫 단계는 OpenAI Provider의 native pass-through만 지원한다. 모델별 가격 단위가 audio duration, input/output token 등으로 다르므로 billing-required 활성화는 verified usage settlement 후속 계획 전까지 fail closed한다.

참조 계약:

- OpenAI Audio API `POST /v1/audio/transcriptions`
- `gateway-20260820-004` bounded multipart image edit 기반
- `gateway-20260820-011` provider credential control plane
- `gateway-20260823-054` OpenAI Audio Speech transport 경계

## 범위

- OpenAI native `POST /v1/audio/transcriptions`
- 공식 Python·JavaScript SDK Base URL 호환
- streaming multipart parser와 bounded temporary spool
- 정확히 하나의 `file`, `model` 및 선택적 native form fields 전달
- logical model registry, model authorization, API Key rate limit
- OpenAI channel credential replacement와 fixed upstream origin
- bounded native JSON/text response pass-through
- 지원 모델에 한한 transcription streaming/SSE capability 선언과 relay
- complete request/response 및 stream idle timeout
- client cancel, Provider timeout/reset/panic 분류
- safe response header allowlist와 credential/content 비노출 로그
- upload concurrency semaphore, request/file/field/response별 제한
- unit, integration, official SDK conformance와 fault tests

## 제외 범위

- Billing-required transcription dispatch와 Wallet settlement
- duration/token usage 가격 publication 및 extraction
- `/v1/audio/translations`
- cross-provider STT routing/fallback
- diarization normalization 또는 transcript schema 변환
- audio 파일 영구 저장, CDN, 재사용
- realtime transcription WebSocket
- batch transcription Job API

## 핵심 결정

### 1. multipart wire를 native로 보존하되 신뢰 경계에서 재구성한다

- inbound boundary와 각 part를 streaming으로 검사한다.
- client multipart body를 Provider에 byte-for-byte blind proxy하지 않는다.
- 허용된 field와 file part만 새 multipart writer로 구성해 Provider로 전달한다.
- `Authorization`, filename header, content type과 boundary는 Gateway가 다시 생성한다.
- unknown native text field는 bounded unique key/value로 보존하되 credential·routing field 주입은 거부한다.

### 2. audio file은 메모리 전체 적재 없이 bounded spool한다

- request total, file bytes, field count, field bytes와 concurrent spool 수를 독립 제한한다.
- 임계치 이하 memory spool과 그 이상 secure temporary file을 사용한다.
- 임시 파일은 성공·실패·panic·cancel 모든 경로에서 닫고 제거한다.
- filename은 basename 정규화 후 upstream metadata로만 사용하고 로컬 경로 결정에 사용하지 않는다.

### 3. native response와 streaming은 capability로 분리한다

- 일반 응답은 bounded JSON 또는 documented text MIME만 전달한다.
- `stream=true` 또는 streaming response format은 model capability가 명시된 경우만 허용한다.
- SSE는 event bytes를 변환하지 않고 idle timeout과 response byte bound만 적용한다.
- Provider response를 받은 뒤에는 retry/fallback하지 않는다.

### 4. billing-required는 구현 전까지 fail closed한다

- transcription 모델은 Billing-disabled BYOK에서만 활성화한다.
- compressed byte size를 duration/token 비용으로 간주하지 않는다.
- 후속 plan에서 verified Provider usage/duration strategy와 reservation upper bound가 완성된 뒤 제한을 제거한다.

## 설계 및 구현 순서

### 1. Operation과 configuration

- `audio.transcription` operation과 fixed OpenAI model registry를 추가한다.
- model capability에 streaming, response formats, language/prompt/timestamp support를 선언한다.
- request total, file, field, response, spool concurrency, complete/idle timeout 설정을 추가한다.
- Billing-required + enabled transcription model 조합을 configuration load에서 거부한다.

### 2. Multipart intake

- bounded reader와 multipart part count 제한을 적용한다.
- `file`과 `model`의 누락·중복·빈 값, duplicate top-level fields를 거부한다.
- filename, part MIME, text field key/value를 검증한다.
- audio MIME와 확장자를 보안 신뢰값으로 사용하지 않고 크기와 내용 전송만 수행한다.

### 3. Provider adapter와 facade

- 고정 `https://api.openai.com/v1/audio/transcriptions` endpoint를 사용한다.
- service credential과 hop-by-hop/client routing headers를 폐기한다.
- 재구성된 multipart body를 streaming upload하고 context cancellation을 전달한다.
- native status/body와 safe content/retry headers만 반환한다.

### 4. Streaming, observability와 cleanup

- non-stream response는 response limit 내에서 native bytes를 전달한다.
- SSE는 bounded frame-independent relay, idle timeout, client disconnect 분류를 제공한다.
- 로그/trace에는 request ID, operation, model, status, duration, byte count와 분류만 기록한다.
- filename, audio bytes, prompt, credential과 transcript content를 기록하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/audio/transcriptions
Content-Type: multipart/form-data; boundary=...
```

공식 SDK의 `audio.transcriptions.create(...)` request와 native response를 유지한다.

### 내부 인터페이스

- `operations/audio.Transcription`
- `audio.TranscriptionModel{Capabilities}`
- `openai.TranscriptionRequest{ChannelID, ContentType, ContentLength, Body}`
- `openai.TranscriptionExecutor.Create(ctx, request)`
- `transcriptionSpool{Open, Size, Cleanup}`

### 데이터베이스

- transcription 모델 권한을 위한 API Key permission operation constraint migration
- BYOK foundation이므로 가격, charge, ledger schema는 추가하지 않는다.

### 설정

- `GATEWAY_OPENAI_TRANSCRIPTION_MODELS`
- `GATEWAY_OPENAI_TRANSCRIPTION_MODEL_CAPABILITIES_JSON`
- `GATEWAY_OPENAI_TRANSCRIPTION_REQUEST_TIMEOUT`
- `GATEWAY_OPENAI_TRANSCRIPTION_STREAM_IDLE_TIMEOUT`
- `GATEWAY_OPENAI_TRANSCRIPTION_MAX_REQUEST_BODY_BYTES`
- `GATEWAY_OPENAI_TRANSCRIPTION_MAX_FILE_BYTES`
- `GATEWAY_OPENAI_TRANSCRIPTION_MAX_FIELD_BYTES`
- `GATEWAY_OPENAI_TRANSCRIPTION_MAX_RESPONSE_BODY_BYTES`
- `GATEWAY_OPENAI_TRANSCRIPTION_MAX_CONCURRENT_SPOOLS`

## 다른 저장소 계약

- `cloud`: Provider credential, model/capability와 upload/timeout 제한 배포
- `dashboard`: 모델 capability와 BYOK-only 상태 표시; audio/transcript content 비노출
- `conformance`: Python·JavaScript SDK multipart, JSON/text/SSE, cancel/oversize fixtures

## 보안 및 운영 고려사항

- multipart boundary, header count/length, field count와 duplicate field를 제한한다.
- filename의 절대 경로, `..`, NUL, CR/LF와 비정상 길이를 거부한다.
- temporary spool은 권한이 제한된 OS 임시 디렉터리에 생성하고 항상 제거한다.
- audio 내용 sniffing으로 원격 URL fetch를 수행하지 않아 SSRF 경로를 만들지 않는다.
- inbound Authorization, OpenAI organization/project, Host와 proxy headers는 upstream에 전달하지 않는다.
- transcript, prompt, audio, filename과 Provider response body는 log/metric label에 포함하지 않는다.
- client disconnect와 Provider 불확실 결과는 재시도하지 않는다.

## 테스트 계획

### 단위 테스트

- multipart missing/duplicate file/model과 duplicate unknown field
- total/file/field/part/header limit
- Unicode filename, traversal, NUL/CRLF와 basename 처리
- memory/file spool 전환과 cleanup
- capability별 response format/streaming 허용
- safe upstream/downstream header와 fixed URL
- bounded JSON/text/SSE relay, idle timeout과 write failure

### 통합 테스트

- PostgreSQL model permission migration과 기존 permission 회귀
- Provider mock으로 multipart field/file byte integrity 검증
- concurrent spool limit과 cancel cleanup
- Provider 400/401/429/5xx, timeout/reset/panic
- Gateway restart/handler cancellation 후 임시 파일 잔존 없음

### SDK 호환성 테스트

- OpenAI Python `client.audio.transcriptions.create(file=..., model=...)`
- OpenAI JavaScript `client.audio.transcriptions.create({file, model})`
- JSON, text response format과 capability-enabled SSE

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [ ] 공식 Python·JavaScript SDK에서 Key와 Base URL만 바꿔 transcription 성공
- [ ] multipart가 전체 메모리 적재 없이 bounded spool됨
- [ ] file/model/field duplicate와 모든 선언 제한이 fail closed됨
- [ ] Provider credential 교체와 fixed OpenAI origin이 보장됨
- [ ] native JSON/text 및 capability-enabled SSE 응답이 보존됨
- [ ] timeout/reset/panic/client disconnect가 Provider 재호출을 만들지 않음
- [ ] 모든 임시 파일이 성공·실패·cancel에서 제거됨
- [ ] audio/prompt/filename/transcript/credential이 로그와 telemetry에 노출되지 않음
- [ ] Billing-required transcription enablement가 fail closed됨
- [ ] 전체 unit/race/integration/SDK 검사가 통과함
- [ ] README와 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- transcription model 설정을 제거해 route 등록을 중단한다.
- API Key permission migration은 호환성을 위해 유지한다.
- Provider adapter와 facade는 호출 경로가 비활성화되면 실행되지 않는다.
- Speech 및 기존 image/chat/video 경로는 영향을 받지 않는다.

## 후속 작업

- OpenAI Audio Transcriptions duration/token pricing and settlement
- OpenAI Audio Translations native multipart foundation
- managed audio input storage and reusable asset references
- realtime transcription WebSocket and batch transcription Jobs
- cross-provider STT routing and fallback
