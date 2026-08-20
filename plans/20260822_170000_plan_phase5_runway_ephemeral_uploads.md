---
id: gateway-20260822-052
title: Phase 5 Runway Native Ephemeral Uploads and Tenant Asset Binding
status: completed
created_at: 2026-08-22T17:00:00+09:00
updated_at: 2026-08-22T19:30:00+09:00
owners:
  - gateway
initiative: phase-5-runway-ephemeral-uploads
depends_on:
  - gateway-20260820-002
  - gateway-20260820-018
  - gateway-20260820-023
  - gateway-20260820-029
  - gateway-20260822-050
  - gateway-20260822-051
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 5 Runway Native Ephemeral Uploads and Tenant Asset Binding

## 목적

공식 Runway Python/JavaScript SDK가 API Key와 Base URL만 변경한 상태에서 `uploads.createEphemeral`을 사용할 수 있게 하고, 대용량 media byte는 Gateway 메모리나 디스크를 통과시키지 않은 채 Runway가 발급한 bounded direct-upload form으로 전송하며, 반환된 `runway://` 자산은 발급받은 tenant만 video 요청에 사용할 수 있게 한다.

## 배경

현재 Runway facade는 HTTPS URL과 작은 image data URI를 입력으로 허용하지만 공식 SDK의 `POST /v1/uploads` 계약은 지원하지 않는다. 이 계약은 먼저 filename과 `type=ephemeral`을 보내 signed `uploadUrl`, multipart `fields`, `runwayUri`를 받고, SDK가 media file을 해당 object upload URL로 직접 전송하는 2단계 흐름이다. 따라서 Gateway가 200MB payload를 reverse proxy하거나 메모리에 적재할 필요가 없다.

Runway 공식 문서 기준 ephemeral upload는 512 bytes 이상 200MB 이하이고 `runway://` URI는 24시간 유효하며 여러 요청에서 재사용할 수 있다. 실패한 object upload는 같은 form을 retry하지 않고 새 `/v1/uploads` 요청으로 시작해야 한다. `runway://`는 같은 관리형 Provider 계정 안에서 사용 가능한 bearer-like asset reference이므로, 멀티테넌트 Gateway가 단순 pass-through만 하면 다른 tenant에게 유출된 URI가 재사용될 수 있다.

참조 계약:

- [Runway ephemeral uploads](https://docs.dev.runwayml.com/assets/uploads/)
- [Runway API reference: POST /v1/uploads](https://docs.dev.runwayml.com/api/)
- [Runway input limits](https://docs.dev.runwayml.com/assets/inputs/)
- [Runway OpenAPI and official SDK source](https://github.com/runwayml/openapi)

## 범위

- Runway native `POST /v1/uploads` facade와 exact `X-Runway-Version: 2024-11-06`
- 공식 SDK multipart/file abstraction과 호환되는 upload-bootstrap request/response
- client request의 `filename`과 exact `type=ephemeral` validation
- 선택된 Runway channel credential로 fixed-origin bootstrap 호출
- upstream `uploadUrl`, multipart `fields`, `runwayUri`의 bounded schema validation
- media byte를 Gateway가 받지 않는 direct-upload architecture
- `runway://` URI의 SHA-256 digest, tenant/API Key ownership, channel, 발급·만료 시각 저장
- 관리형 및 공유 credential mode에서 video submit 전 URI ownership/expiry 검증
- 동일 tenant 내 24시간 범위의 URI 재사용
- bootstrap rate limit, timeout, response loss와 Provider 429/5xx의 native error mapping
- credential, signed URL/form field, raw URI와 filename의 log/metric/trace redaction
- readiness, 운영 문서, 공식 Python/JavaScript SDK conformance handoff

## 제외 범위

- Gateway가 media multipart body를 받아 Provider storage로 streaming proxy하는 경로
- signed object upload의 byte/content sniffing 또는 upload-completion callback
- Gateway-managed S3/R2 input asset와 영구 input library
- Provider output video 다운로드, 저장, CDN과 URL 교체
- 업로드 자체에 별도 Wallet 가격을 부과하는 기능
- expired URI 자동 재업로드
- Runway 외 Provider upload protocol 변환
- video-to-video, audio, avatar와 workflow endpoint

## 핵심 결정

### 1. Native 2단계 direct upload를 유지한다

- Gateway는 작은 JSON bootstrap만 받고 Provider의 signed multipart form을 SDK에 반환한다.
- SDK는 file stream을 `uploadUrl`로 직접 보내므로 Gateway의 request body, spool, 메모리와 egress를 사용하지 않는다.
- signed form의 expiry, 최대 크기와 object key는 Provider가 발급한 policy를 그대로 적용한다.
- Gateway는 object storage origin으로 redirect하거나 upload body를 중계하지 않는다.

### 2. `runway://`는 tenant-owned capability로 취급한다

- raw URI는 bootstrap 응답에서만 native wire로 반환하고 DB에는 keyed digest만 저장한다.
- asset binding은 organization/project/API Key, Runway channel, issued/expired time을 가진다.
- video request의 URL-capable top-level field에서 `runway://`가 발견되면 동일 tenant와 channel의 active digest가 아니면 Provider 호출 전에 거부한다.
- HTTPS/data URI의 기존 규칙은 유지하며 URI를 로그나 telemetry dimension으로 사용하지 않는다.

### 3. bootstrap 성공과 실제 upload 성공을 구분한다

- Gateway는 signed form 발급 성공만 알 수 있고 client-to-storage upload 성공 여부는 알 수 없다.
- binding 상태는 `ISSUED`이며 실제 task가 URI를 받아들인 사실을 upload completion으로 추정하지 않는다.
- object upload 실패는 원장이나 Job reconciliation 대상이 아니며 SDK/사용자는 새 bootstrap을 요청한다.
- bootstrap timeout/connection loss는 안전한 자동 retry나 fallback을 하지 않고 native unavailable로 반환한다.

### 4. 업로드는 video charge와 분리한다

- upload-bootstrap은 Wallet Reserve/Capture를 만들지 않는다.
- 후속 video submit만 Plan 051 가격으로 과금한다.
- bootstrap rate limit과 Provider channel availability는 적용하되 업로드 실패가 video credit 환불을 만들지 않는다.

## 설계 및 구현 순서

### 1. Native upload facade

- `POST /v1/uploads`를 Runway handler에 추가한다.
- service authentication, API Key network/model authorization, request rate limit과 exact version 검사를 재사용한다.
- content type은 JSON, body는 작은 상한으로 제한하고 duplicate/unknown security-sensitive field를 거부한다.

### 2. Provider bootstrap adapter

- fixed Runway origin에 Bearer credential과 allowlisted headers만 전송한다.
- redirect를 금지하고 timeout/connection loss/429/5xx를 기존 pre/post-dispatch 분류에 연결한다.
- response는 maximum bytes, HTTPS signed upload URL, bounded string map과 `runway://` 형식을 검증한다.

### 3. Asset binding persistence

- `runway_upload_assets`와 append-only audit event를 additive migration으로 추가한다.
- URI digest uniqueness는 channel 범위로 두고 tenant ownership과 24시간 expiry를 저장한다.
- raw URI, signed URL, multipart fields, filename과 file content는 저장하지 않는다.
- 동일 bootstrap response가 관측된 경우 단일 binding으로 멱등 수렴한다.

### 4. Video request authorization

- text/image video native request의 URL-capable input에서 `runway://`를 엄격히 추출한다.
- 관리형/shared channel에서는 owner, channel, expiry가 일치해야 dispatch한다.
- BYOK의 tenant-dedicated credential 정책을 명시하고, 공유 channel이면 동일 검사를 적용한다.
- DB unavailable/corrupt binding은 fail closed하며 Provider를 호출하지 않는다.

### 5. 운영 및 호환성

- upload bootstrap readiness와 bounded telemetry를 추가한다.
- Python sync/async와 JavaScript stream upload가 Gateway bootstrap 뒤 signed URL로 직접 전송되는지 검증한다.
- README에 512-byte/200MB, 24-hour expiry, no-retry form과 upload-byte bypass를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/uploads
Authorization: Bearer SERVICE_KEY
X-Runway-Version: 2024-11-06
Content-Type: application/json

{"filename":"input.mp4","type":"ephemeral"}
```

성공 응답은 Provider native `uploadUrl`, `fields`, `runwayUri` field를 유지한다. Gateway 내부 asset ID나 tenant metadata는 삽입하지 않는다.

### 내부 인터페이스

- `providers/runway.CreateEphemeralUpload(ctx, filename) -> UploadBootstrap`
- `internal/runwayassets.Bind(ctx, owner, channel, runwayURI, expiresAt)`
- `internal/runwayassets.Authorize(ctx, owner, channel, runwayURI, now)`
- protocol handler는 body 전체를 일반화하지 않고 URL-capable native field만 allowlist로 검사한다.

### 데이터베이스 및 migration

- `runway_upload_assets`: digest, organization/project/API Key, channel, issued/expires timestamps
- `runway_upload_asset_events`: append-only issued/authorized/expired audit category
- tenant/channel/digest active lookup index
- 기존 Job, charge와 BYOK row에는 nullable/additive 영향만 주고 downgrade 시 새 table은 구 binary가 무시한다.

### 다른 저장소에 제공하거나 요구하는 계약

- `conformance`: 공식 Python/JavaScript SDK의 file stream bootstrap, direct multipart upload, returned URI 사용과 cross-tenant rejection을 외부 HTTP에서 검증한다.
- `cloud`: fixed Runway credential channel과 upload rate-limit 정책을 배포하고 signed URL/form/URI를 log 또는 state output에 남기지 않는다.
- `dashboard`: upload content/filename/URI를 표시하지 않으며 필요 시 bounded issued/expired count만 사용한다.

## 보안 및 과금 고려사항

- inbound Authorization을 upstream에 전달하지 않고 선택된 credential로 교체한다.
- filename은 basename만 허용하고 path separator, control character, traversal, 과도한 Unicode/길이와 미지원 extension을 거부한다.
- signed `uploadUrl`과 form field는 secret-bearing capability로 취급해 로그, trace, Sentry와 DB에서 제외한다.
- Gateway는 upload URL을 fetch하지 않으므로 SSRF와 redirect surface를 만들지 않는다.
- raw `runway://`도 bearer-like capability이므로 digest 비교만 하고 다른 tenant/channel/expired URI는 native 400/403으로 fail closed한다.
- bootstrap에는 Wallet charge가 없고, video charge의 idempotency fingerprint는 URI 원문을 DB/telemetry에 저장하지 않은 채 기존 digest로 유지한다.
- Provider bootstrap timeout은 성공 여부가 불명확하더라도 금전 효과가 없으며 자동 retry하지 않는다.

## 테스트 계획

### 단위 테스트

- filename/type/version/content-type/body bound와 duplicate key validation
- fixed origin, credential/header replacement, redirect/timeout와 response size
- signed URL/fields/runway URI schema, expiry와 digest normalization
- URI ownership/channel/expiry authorization과 raw value redaction

### 통합 테스트

- asset binding append-only audit와 tenant/API Key isolation
- concurrent identical observed URI의 단일 binding
- DB failure, expired/cross-tenant URI의 video pre-dispatch rejection
- managed video charge가 unauthorized URI에서 생성되지 않음
- 기존 HTTPS/data URI, Runway task billing, Replicate/fal Job 회귀

### 호환성 및 장애 테스트

- Runway Python sync/async `uploads.create_ephemeral`
- Runway JavaScript `uploads.createEphemeral` with `fs.ReadStream`
- 512-byte minimum과 200MB policy를 signed direct upload가 적용하고 Gateway RSS/body reader가 file size에 비례하지 않음
- Provider 429/5xx, bootstrap response loss, malformed signed form과 expired URI

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/runway -count=1
```

## 완료 조건

- [x] 공식 Runway Python/JavaScript SDK가 Key와 Base URL만 변경해 ephemeral upload bootstrap을 사용함
- [x] media byte가 Gateway 메모리, 디스크 또는 egress를 통과하지 않음
- [x] 512 bytes–200MB와 24시간 expiry가 native signed-upload 계약으로 보존됨
- [x] `runway://`가 tenant/channel/expiry에 묶여 cross-tenant 재사용이 pre-dispatch 차단됨
- [x] signed URL/form, URI, filename, content와 credential이 persistence/telemetry/log에 노출되지 않음
- [x] bootstrap timeout/429/5xx와 failed direct upload가 자동 retry 또는 과금 효과를 만들지 않음
- [x] authorized URI를 사용한 managed video 요청이 Plan 051 정산 불변 조건을 유지함
- [x] 전체 unit/race/integration/SDK 회귀가 통과함
- [x] README, migration, 운영 runbook과 멀티레포 handoff가 갱신됨

## 검증 증거

- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과: format, vet, 전체 race unit test와 binary build.
- fresh PostgreSQL `gateway_plan052` 및 격리 Redis DB 14에서 전체 `make integration-test` 통과. migration 000001–000046과 기존 Billing/Job 회귀 포함.
- `GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/runway -count=1` 통과.
- 공식 Runway Python sync/async 및 JavaScript SDK가 Gateway bootstrap 뒤 별도 storage server로 multipart file stream을 직접 전송하고 반환 URI로 image-to-video를 생성함을 검증했다.
- tenant/API Key/channel digest binding, 24시간 expiry, cross-tenant 거부와 append-only history를 PostgreSQL 통합 테스트로 검증했다.

## Rollback 계획

- `/v1/uploads` route를 설정으로 비활성화해 신규 signed form 발급을 중단한다.
- 이미 발급된 signed form과 `runway://`는 Provider expiry까지 존재하므로 asset binding 검사는 유지한다.
- additive binding/event table은 감사와 cross-tenant 방어를 위해 삭제하지 않는다.
- 기존 HTTPS/data URI video 요청과 Plan 051 billing/worker는 영향 없이 계속 동작한다.

## 후속 작업

- managed video output download, S3/R2 storage와 CDN delivery
- Gateway-managed reusable input asset library
- video-to-video와 추가 Runway native task type
- cross-provider video routing/fallback
- Speech/Transcription native foundation과 audio billing
