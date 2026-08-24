---
id: gateway-20260824-058
title: Phase 5 OpenAI Audio Translations Native Multipart Foundation
status: in_progress
created_at: 2026-08-24T12:43:08+09:00
updated_at: 2026-08-24T12:50:19+09:00
owners:
  - gateway
initiative: phase-5-openai-audio-translations-foundation
depends_on:
  - gateway-20260820-003
  - gateway-20260820-007
  - gateway-20260820-018
  - gateway-20260820-019
  - gateway-20260820-020
  - gateway-20260820-028
  - gateway-20260823-056
  - gateway-20260824-057
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 OpenAI Audio Translations Native Multipart Foundation

## 목적

공식 OpenAI Python·JavaScript SDK가 API Key와 Base URL만 변경해 `POST /v1/audio/translations`를 호출할 수 있도록, translation 전용 operation·model capability·고정 Provider endpoint와 bounded multipart/native response 경계를 구현하고 기존 transcription 동작을 보존하는 BYOK foundation을 제공한다.

## 배경

Plan 056과 057로 transcription의 native multipart와 typed usage settlement가 완성됐지만 audio translation은 별도 SDK resource, URL, operation 의미와 capability를 가진다. 현재 공식 SDK 계약에서 translation은 audio를 English text로 변환하며 `file`, `model`이 필수이고 `prompt`, `response_format`, `temperature`를 선택적으로 받는다. 응답 형식은 `json`, `verbose_json`, `text`, `srt`, `vtt`이고 현재 SDK가 명시하는 모델은 `whisper-1`이다.

translations를 transcription route의 별칭으로 등록하면 모델 권한, rate-limit dimension, telemetry, Provider URL과 향후 과금 증거가 섞인다. 반대로 multipart parser와 spool을 통째로 복사하면 두 경로의 보안 제한과 cleanup 수정이 쉽게 어긋난다. 따라서 operation과 정책은 분리하고, 검증된 multipart/spool/relay primitive만 내부 공용 구성요소로 추출한다.

참조 계약:

- OpenAI Audio API `POST /v1/audio/translations`
- 공식 OpenAI Python SDK `audio.translations.create`
- 공식 OpenAI JavaScript SDK `audio.translations.create`
- `gateway-20260823-056` bounded transcription multipart foundation
- `gateway-20260824-057` transcription billing separation and content-free evidence boundary

## 범위

- OpenAI native `POST /v1/audio/translations`
- 공식 Python·JavaScript SDK Base URL 호환
- 독립 `audio.translation` operation과 translation model/capability registry
- `file`, `model`, 선택적 `prompt`, `response_format`, `temperature`의 bounded multipart validation
- 기존 memory/file spool, part/header/request/file/field/concurrency 제한의 안전한 공용화
- translation 전용 fixed OpenAI origin과 Provider credential replacement
- bounded native JSON, verbose JSON, text, SRT, VTT response pass-through
- API Key authentication, model authorization, network restriction, distributed rate limit
- Provider health gate, complete timeout, client cancellation과 no-redispatch fault boundary
- translation 모델의 `/v1/models` 노출과 billing-required fail-closed readiness
- content-free log/trace/metric와 temporary file cleanup
- unit, integration, official SDK conformance와 장애 테스트

## 제외 범위

- translation Wallet 예약, duration pricing, Capture·Release·Reconciliation
- translation streaming 또는 SSE
- `language`, timestamp granularity, diarization 등 transcription 전용 옵션
- 결과 언어 선택 또는 English 외 언어로의 Gateway-side 변환
- cross-provider translation routing/fallback
- audio 또는 번역 결과 영구 저장·CDN·response replay
- codec decode, MIME 신뢰 또는 Gateway-side duration 추정
- realtime/batch translation Job
- Cloud·Dashboard·Conformance 저장소 내부 구현

## 핵심 결정

### 1. translation은 독립 operation으로 식별한다

- operation은 `audio.translation`이며 `audio.transcription`의 alias가 아니다.
- route, capability, model permission, rate-limit, health와 telemetry dimension을 translation 전용으로 유지한다.
- 사용자가 요청한 logical model은 Provider model mapping 후에만 multipart `model` 값으로 교체한다.

### 2. wire primitive만 공유하고 operation 정책은 공유하지 않는다

- bounded multipart part parsing, secure spool, outbound multipart 작성과 native response relay를 package-local 공용 primitive로 추출할 수 있다.
- field allowlist, capability 검사, fixed endpoint, operation 이름, logging과 error code는 handler별 정책으로 유지한다.
- 공용화 전후 transcription의 request/response bytes, limits, cleanup과 fault behavior를 회귀 테스트로 고정한다.

### 3. 공식 translation 입력 계약을 fail closed한다

- 정확히 하나의 `file`과 `model`을 요구하고 중복·빈 값·비정상 part를 거부한다.
- 선택 필드는 `prompt`, `response_format`, `temperature`만 허용한다.
- `temperature`는 JSON number 문법이 아니라 multipart decimal text로 `0..1` 범위를 binary float 비교 없이 검증하고 원문을 보존한다.
- response format은 model capability와 교차 검사하며 translation에 없는 `stream`, `language`, `timestamp_granularities[]`는 Provider 호출 전에 거부한다.

### 4. BYOK만 활성화하고 관리형 과금은 별도 계획으로 둔다

- billing-disabled에서는 native translation을 제공한다.
- billing-required에서는 verified translation 가격과 usage evidence가 없으므로 설정 load/readiness/dispatch가 fail closed한다.
- verbose JSON의 `duration`을 별도 승인 없이 청구량으로 사용하지 않는다.

## 설계 및 구현 순서

### 1. Operation, registry와 configuration

- `audio.translation` 상수와 translation route/capability registry를 추가한다.
- translation model 목록, model mapping, response formats, prompt/temperature capability와 timeout/size/spool 설정을 로드한다.
- API Key permission constraint migration에 translation operation을 추가하고 기존 permission을 보존한다.
- billing-required와 enabled translation model 조합을 startup에서 명시적으로 거부한다.

### 2. Multipart 보안 primitive 공용화

- transcription 내부의 part/header/field 제한, memory-to-file spool과 outbound temporary multipart 생성을 동작 변경 없이 재사용 가능한 내부 구조로 분리한다.
- input/output temporary file 이름은 operation별 prefix만 사용하고 사용자 filename을 경로에 사용하지 않는다.
- partial parse, capacity rejection, Provider error, cancel, panic과 client write failure에서 모든 resource cleanup을 보장한다.

### 3. Translation facade와 Provider adapter

- translation handler가 인증, route resolution, model authorization, health gate 이후 multipart를 검증한다.
- Provider adapter는 오직 `https://api.openai.com/v1/audio/translations`로 전송하고 DB/env credential registry에서 Authorization을 재생성한다.
- inbound Authorization, Host, proxy, OpenAI organization/project와 routing headers를 폐기한다.
- Provider 응답을 받은 뒤 retry/fallback하지 않고 native status와 bounded body를 safe header allowlist와 함께 반환한다.

### 4. Models, observability와 SDK conformance

- `/v1/models`는 credential/channel/model authorization이 유효하고 BYOK mode에서 enabled인 translation model을 중복 없이 노출한다.
- log/trace/metric에는 request ID, protocol, operation, logical model, status, outcome, duration과 bounded byte count만 사용한다.
- 공식 Python·JavaScript SDK가 json과 text 계열 응답을 정확히 역직렬화하는지 검증한다.
- transcription SDK/fault suite를 함께 실행해 공용화 회귀가 없음을 증명한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/audio/translations
Content-Type: multipart/form-data; boundary=...
```

공식 SDK의 다음 호출 형태를 유지한다.

```python
client.audio.translations.create(
    model="whisper-1",
    file=("speech.wav", audio_bytes, "audio/wav"),
)
```

Gateway는 native `json`, `verbose_json`, `text`, `srt`, `vtt` 응답을 변환하지 않는다.

### 내부 인터페이스

- `operations/audio.Translation`
- `audio.TranslationModel{ID, ProviderModel, ChannelID, Capabilities}`
- `openai.TranslationRequest{ChannelID, ContentType, ContentLength, Accept, UserAgent, Body}`
- `openai.TranslationExecutor.Create(ctx, request)`
- package-local bounded multipart/spool/relay primitive

### 데이터베이스 및 migration

- API Key model permission의 operation constraint에 `audio.translation`을 추가한다.
- 가격, Wallet, charge, usage evidence와 reconciliation table은 추가하지 않는다.
- migration은 기존 permission row와 다른 operation을 변경하지 않으며 rollback 시 호환성을 위해 유지한다.

### 설정

- `GATEWAY_OPENAI_TRANSLATION_MODELS`
- `GATEWAY_OPENAI_TRANSLATION_MODEL_MAP`
- `GATEWAY_OPENAI_TRANSLATION_MODEL_CAPABILITIES_JSON`
- `GATEWAY_OPENAI_TRANSLATION_REQUEST_TIMEOUT`
- `GATEWAY_OPENAI_TRANSLATION_MAX_REQUEST_BODY_BYTES`
- `GATEWAY_OPENAI_TRANSLATION_MAX_FILE_BYTES`
- `GATEWAY_OPENAI_TRANSLATION_MAX_FIELD_BYTES`
- `GATEWAY_OPENAI_TRANSLATION_MAX_RESPONSE_BODY_BYTES`
- `GATEWAY_OPENAI_TRANSLATION_MAX_CONCURRENT_SPOOLS`

기본값은 transcription의 검증된 제한과 동일하게 시작하되 설정 validation과 runtime semaphore는 translation별로 독립한다.

### 다른 저장소에 제공하거나 요구하는 계약

- `cloud`: translation model/mapping/capability, BYOK-only mode와 upload/timeout 제한 배포
- `dashboard`: `audio.translation` capability와 BYOK-only 상태 표시; audio/prompt/result 비노출
- `conformance`: Python·JavaScript multipart json/verbose_json/text/SRT/VTT, invalid option, oversize, timeout/reset/cancel fixture

각 저장소는 `phase-5-openai-audio-translations-foundation` initiative의 독립 로컬 plan으로 구현한다.

## 보안 및 과금 고려사항

- request/file/field/part/header/response 크기와 concurrent spool을 독립 제한한다.
- filename의 절대 경로, traversal, NUL, CR/LF와 비정상 길이를 거부하고 경로 선택에 사용하지 않는다.
- audio MIME·확장자를 보안 신뢰값이나 비용 근거로 사용하지 않으며 URL fetch를 수행하지 않는다.
- service key와 Provider credential, OpenAI organization/project header가 서로의 trust boundary를 넘지 않게 한다.
- audio bytes, filename, prompt와 translated text를 DB, log, trace attribute 또는 metric label에 기록하지 않는다.
- billing-required dispatch를 사전에 거부하므로 이 계획은 Wallet·Ledger를 변경하지 않는다.
- Provider dispatch 이후 timeout/reset/cancel/panic은 재호출하지 않으며 후속 관리형 과금 전에는 금전 상태가 없다.

## 테스트 계획

### 단위 테스트

- missing/duplicate `file`·`model`, duplicate optional field와 unknown/transcription-only field
- prompt/temperature/response-format capability와 `temperature` decimal `0..1` 경계
- request/file/field/part/header/response limit 및 spool capacity
- Unicode filename, basename, traversal, NUL/CRLF와 content type validation
- memory/file spool 전환, outbound multipart integrity와 모든 cleanup 경로
- fixed Provider URL, credential replacement와 request/downstream safe headers
- JSON/verbose JSON/text/SRT/VTT native body 보존

### 통합 테스트

- migration 적용·반복과 기존 API Key permission 회귀
- Provider mock에서 logical/provider model mapping과 multipart file/field byte integrity
- translation/transcription concurrent spool과 설정 격리
- distributed rate limit, model/network authorization과 health gate
- Provider 400/401/429/5xx, timeout/reset/panic, response oversize와 client cancellation
- 공용화 이후 transcription multipart, cleanup, billing과 SDK 회귀

### 호환성 및 장애 테스트

- OpenAI Python `client.audio.translations.create(file=..., model=...)`
- OpenAI JavaScript `client.audio.translations.create({file, model})`
- json, verbose_json와 text/SRT/VTT SDK response typing
- Key와 Base URL 외 코드 변경 없음
- malformed multipart와 duplicate fields가 Provider dispatch 전에 거부됨
- timeout/reset/cancel/panic과 duplicate client retry가 자동 Provider 재호출을 만들지 않음

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [ ] 공식 Python·JavaScript SDK에서 Key와 Base URL만 변경해 audio translation이 성공함
- [ ] `audio.translation` operation, model registry, permission, rate limit과 telemetry가 transcription과 분리됨
- [ ] multipart가 bounded spool되고 file/model/optional field 및 모든 선언 제한이 fail closed됨
- [ ] temperature, response format과 translation 전용 capability가 dispatch 전에 검증됨
- [ ] Provider credential 교체와 fixed `/v1/audio/translations` origin이 보장됨
- [ ] native JSON/verbose JSON/text/SRT/VTT status·header·body가 보존됨
- [ ] timeout/reset/panic/cancel/client failure가 Provider 재호출을 만들지 않음
- [ ] 성공·실패·cancel의 input/outbound temporary file이 모두 제거됨
- [ ] audio/prompt/filename/result/credential이 DB·log·telemetry에 노출되지 않음
- [ ] billing-required translation enablement와 dispatch가 fail closed됨
- [ ] transcription의 native multipart, billing, fault와 SDK 회귀가 없음
- [ ] 전체 unit/race/integration/SDK 검사가 통과함
- [ ] README, migration과 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- translation model 설정과 route 등록을 제거해 신규 endpoint dispatch를 중단한다.
- API Key permission migration과 공용 multipart primitive는 backward-compatible이므로 유지한다.
- 문제가 공용화에 있으면 translation route를 비활성화한 뒤 transcription의 검증된 동작을 우선 복구하는 change/rollback plan을 작성한다.
- 이 계획은 Wallet·Ledger data를 만들지 않으므로 금전 reconciliation은 필요하지 않다.

## 후속 작업

- OpenAI Audio Translation duration pricing and settlement
- managed audio input storage and reusable asset references
- realtime transcription WebSocket and batch transcription Jobs
- cross-provider STT/translation routing and fallback
