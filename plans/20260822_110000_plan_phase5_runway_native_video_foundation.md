---
id: gateway-20260822-050
title: Phase 5 Runway Native Video Generation Foundation
status: accepted
created_at: 2026-08-22T11:00:00+09:00
updated_at: 2026-08-22T11:00:00+09:00
owners:
  - gateway
initiative: phase-5-runway-native-video-foundation
depends_on:
  - gateway-20260820-029
  - gateway-20260820-035
  - gateway-20260820-023
  - gateway-20260820-028
  - gateway-20260822-049
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 5 Runway Native Video Generation Foundation

## 목적

공식 Runway Python/JavaScript SDK가 API Key와 Base URL만 변경해 Gateway에서 text-to-video와 image-to-video task를 생성·조회·취소할 수 있도록, Runway native wire를 기존 durable Job 코어에 연결한다.

## 배경

Phase 3은 Gateway Job ID와 Provider Job ID를 분리하고 재시작 가능한 polling/cancel 상태 머신을 구축했다. Phase 5의 첫 단계는 새로운 공통 비디오 JSON을 외부 계약으로 만들지 않고 Runway native task protocol을 보존하면서 `video.generate` operation을 도입하는 것이다.

현재 Runway 공식 계약은 Bearer 인증과 `X-Runway-Version: 2024-11-06`, 비동기 task 생성, `GET /v1/tasks/{id}` 조회, `DELETE /v1/tasks/{id}` 취소/삭제를 사용한다. 공식 SDK는 custom `base_url`/`baseURL`을 지원한다. task polling은 같은 task에 5초보다 자주 갱신될 것으로 기대하면 안 되며, 결과 URL은 접근 후 약 24~48시간 내 만료되므로 후속 managed storage가 필요하다.

참조 계약:

- [Runway API reference](https://docs.dev.runwayml.com/api/)
- [Runway official SDK behavior](https://docs.dev.runwayml.com/api-details/sdks/)
- [Runway input constraints](https://docs.dev.runwayml.com/assets/inputs/)
- [Runway output lifetime](https://docs.dev.runwayml.com/assets/outputs/)
- [Runway ephemeral uploads](https://docs.dev.runwayml.com/assets/uploads/)
- [Runway Python SDK](https://github.com/runwayml/sdk-python)

## 범위

- `operations/video`의 `video.generate` operation과 text/image input capability
- Runway native facade:
  - `POST /v1/text_to_video`
  - `POST /v1/image_to_video`
  - `GET /v1/tasks/{id}`
  - `DELETE /v1/tasks/{id}`
- service API Key를 Runway fixed-origin Bearer credential로 교체
- exact `X-Runway-Version: 2024-11-06` validation/forwarding
- logical model에서 Runway provider model로 top-level `model`만 byte-preserving rewrite
- Gateway Job ID를 native `id`로 반환하고 Provider task ID를 비공개 저장
- durable submit, poll lease, status transition, cancel intent와 restart recovery
- Runway PENDING/THROTTLED/RUNNING/SUCCEEDED/FAILED/CANCELED 상태의 공통 Job 매핑
- submit `Idempotency-Key`의 route-independent fingerprint와 exactly-once Provider task 생성
- tenant/API Key ownership이 적용된 native task 조회·취소
- bounded native task snapshot과 failure code 보존
- HTTPS/data URI image input validation과 request/response limits
- Provider URL 반환 모드와 만료 경고 문서화
- `/v1/models`, telemetry, readiness, official SDK conformance

## 제외 범위

- billing-required mode의 video dispatch, credit 예약·Capture·Release
- Runway credit usage 조회 또는 task별 원가 계산
- multipart/ephemeral upload proxy와 200MB streaming upload
- video-to-video, upscale, avatar, workflow, model-router endpoint
- 결과 파일 다운로드, managed object storage와 CDN URL 교체
- Runway webhook 또는 callback
- Runway 이외 Provider로의 변환·routing·fallback
- task 결과 삭제와 cancel 의미를 분리한 별도 관리 API
- SDK의 `waitForTaskOutput` 내부 polling 정책 변경

## 핵심 결정

### 1. Native facade와 내부 operation 분리

- 외부 request/response는 Runway native field와 status를 유지한다.
- 내부 operation은 `video.generate`이며 capability는 `text_to_video` 또는 `image_to_video`다.
- Gateway는 공통 Video request를 외부에 노출하지 않는다.
- submit에서 top-level `model`만 Provider model로 치환하고 unknown native fields/order를 보존한다.

### 2. Durable identity와 ownership

- client-visible task `id`는 Gateway Job ID다.
- Provider task ID는 Job route evidence에 암호화 대상 credential과 분리해 저장하고 응답·로그·metric에 노출하지 않는다.
- retrieve/cancel은 조직·프로젝트 소유권을 확인한 뒤 저장된 Provider/channel/task를 사용한다.
- 다른 tenant의 Job은 존재 여부가 드러나지 않도록 native 404로 응답한다.

### 3. Polling과 cancellation

- worker polling은 최소 5초 간격, jitter와 bounded exponential backoff를 사용한다.
- concurrent request polling과 worker polling은 기존 lease로 단일 Provider poll만 허용한다.
- DELETE는 cancel intent를 먼저 durable하게 기록한 후 Provider DELETE를 호출한다.
- Provider 204와 이미 aborted/deleted인 404는 idempotent cancel 성공으로 수렴한다.
- timeout/connection loss 뒤 task를 실패로 단정하지 않고 `RECONCILING`으로 유지한다.

### 4. Billing boundary

- 이 foundation은 BYOK에서만 submit을 허용한다.
- billing-required mode는 Provider dispatch와 Job 생성 전에 native error로 fail closed한다.
- 후속 plan이 Runway 모델·duration·resolution 가격과 cancel/partial-output 정산을 정의하기 전에는 관리형 credit을 소비하지 않는다.

### 5. Input/output safety

- image input HTTPS URL은 domain hostname만 허용하고 IP literal, userinfo, fragment와 redirect 의존 URL을 거부한다.
- 허용된 image data URI는 content type, decoded-size와 전체 body bound를 검증한다.
- Gateway는 이 plan에서 외부 URL을 직접 fetch하지 않는다.
- Provider output URL은 native provider mode로 반환하지만 ephemeral임을 문서화하고 telemetry/log에 기록하지 않는다.

## 설계 및 구현 순서

1. `Runway` Provider ID, credential environment와 fixed-origin executor를 추가한다.
2. `operations/video` registry에 logical model, input mode, ratio/duration capability를 추가한다.
3. native submit envelope 검증과 top-level model lexical rewrite를 구현한다.
4. Runway submit response를 durable Job 생성 transaction과 결합한다.
5. Runway poll/cancel adapter와 status/error mapping을 기존 Job worker에 연결한다.
6. tenant-owned GET/DELETE facade와 native snapshot projection을 구현한다.
7. idempotency concurrency, unknown outcome와 restart recovery를 검증한다.
8. `/v1/models`, config, README, telemetry와 SDK conformance를 갱신한다.

## 인터페이스와 데이터 변경

### 설정

- `GATEWAY_RUNWAY_API_KEY`
- `GATEWAY_RUNWAY_MODELS`
- `GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON`
- `GATEWAY_RUNWAY_REQUEST_TIMEOUT`
- `GATEWAY_RUNWAY_MAX_BODY_BYTES`
- `GATEWAY_RUNWAY_POLL_INTERVAL` (최소 5초)

Provider origin은 `https://api.dev.runwayml.com`로 고정하며 설정으로 URL을 받지 않는다.

### 데이터베이스

- Provider enum/check constraint에 `runway`를 additive migration으로 추가한다.
- Job operation/capability constraint에 `video.generate`와 Runway task kind를 추가한다.
- native submit snapshot에는 Gateway task ID만 client-facing 형태로 저장한다.
- Provider task ID, route, API version과 model은 immutable Job identity로 보존한다.

### 공개 동작

- submit 성공은 Runway native `{ "id": "<gateway-job-id>" }`를 반환한다.
- GET은 native Runway task status와 bounded output/failure fields를 반환하되 `id`는 Gateway Job ID다.
- DELETE 성공은 native 204를 반환한다.
- `X-Runway-Version` 누락/불일치, unsupported model/input, ownership 실패는 Provider dispatch 전에 native error로 종료한다.

## 멀티레포 계약

- `conformance`: Runway Python sync/async와 JavaScript SDK의 submit/retrieve/delete, polling, idempotency와 restart fixture를 외부 HTTP로 검증한다.
- `cloud`: Runway credential/channel과 versioned model capability manifest를 배포하며 secret이나 Provider task ID를 state/log에 기록하지 않는다.
- `dashboard`: Gateway Job ID, native status, bounded failure category만 표시하고 prompt/input URL/Provider output URL은 기본 telemetry에 포함하지 않는다.

## 보안 및 과금 고려사항

- inbound Authorization을 제거한 뒤 선택된 Runway credential만 적용한다.
- `X-Runway-Version`은 exact allowlist로 제한하고 임의 upstream header를 전달하지 않는다.
- prompt, image URI/data, output URL, Provider task ID와 failure detail을 log/metric/trace label로 기록하지 않는다.
- task lookup은 client ID를 곧바로 Provider path에 넣지 않고 tenant-owned Job lookup을 먼저 수행한다.
- submit timeout은 task 미생성을 의미하지 않으므로 자동 retry/fallback하지 않는다.
- billing-required mode는 미정산 video task를 만들지 않는다.

## 테스트 계획

### 단위 테스트

- model/capability registry와 text/image mode exact matching
- native body validation, duplicate model, URL/data URI bound와 lexical model rewrite
- fixed origin/path/method/version/credential/redirect/timeout
- Runway↔Job status와 failure mapping
- native snapshot ID projection과 secret/content-safe telemetry

### 통합 테스트

- concurrent same-key submit이 하나의 Job/Provider task만 생성
- submit response loss/timeout이 retry 없이 reconciliation으로 수렴
- poll lease, 5초 minimum interval, jitter/backoff와 restart recovery
- duplicate GET/DELETE와 Provider 204/404 cancel idempotency
- cross-tenant GET/DELETE native 404 및 Provider 미호출
- migration upgrade, immutable identity와 billing-required pre-dispatch rejection

### SDK 및 회귀 테스트

- official Runway Python sync/async and JavaScript SDK base URL/API Key replacement
- `image_to_video.create`, `text_to_video.create`, `tasks.retrieve`, `tasks.delete`
- existing Replicate/fal Job, OpenAI/Gemini/Anthropic and image regression
- `make check`, fresh PostgreSQL/Redis integration and SDK conformance

## 완료 조건

- [ ] 공식 Runway Python/JavaScript SDK가 Key와 Base URL만 변경해 native task API를 사용함
- [ ] text-to-video와 image-to-video가 exact capability/model로 dispatch됨
- [ ] Gateway/Provider task identity와 tenant ownership이 분리·보존됨
- [ ] submit idempotency와 polling/cancel이 재시작·동시성 환경에서 exactly-once로 수렴함
- [ ] timeout/response loss가 실패나 자동 retry로 잘못 처리되지 않음
- [ ] 대용량/악성 URI와 secret/content가 request boundary 및 telemetry에서 차단됨
- [ ] billing-required mode에서 미정산 Provider task가 생성되지 않음
- [ ] native status/output/error wire와 204/404 cancel 의미가 SDK 호환됨
- [ ] 전체 unit/race/integration/SDK 회귀가 통과함
- [ ] README, migration, multi-repo handoff와 검증 증거가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- Runway model 설정과 credential channel을 제거해 신규 submit을 중단한다.
- 이미 생성된 Job worker는 poll/cancel을 계속 수행해 terminal 상태로 drain한다.
- Provider task identity와 event/Job row는 감사 및 recovery를 위해 삭제하지 않는다.
- additive Provider/operation schema는 구 binary가 무시하도록 유지한다.

## 후속 작업

- Runway video price, reservation, usage/cancel/partial-output settlement
- Runway ephemeral upload의 bounded streaming proxy
- managed video download, object storage and CDN
- video-to-video and additional Runway task types
- cross-provider video routing and capability translation
- audio Speech/Transcription foundation
