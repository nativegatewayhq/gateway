---
id: gateway-20260824-061
title: Phase 5 Managed Speech Output Storage and Authorized Delivery
status: completed
created_at: 2026-08-24T14:19:33+09:00
updated_at: 2026-08-24T14:52:05+09:00
owners:
  - gateway
initiative: phase-5-managed-speech-output-delivery
depends_on:
  - gateway-20260820-006
  - gateway-20260820-019
  - gateway-20260820-027
  - gateway-20260820-028
  - gateway-20260823-054
  - gateway-20260823-055
  - gateway-20260824-060
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 Managed Speech Output Storage and Authorized Delivery

## 목적

OpenAI native Speech 요청과 binary streaming 응답을 그대로 유지하면서, 명시적으로 managed delivery를 요청한 결과를 private S3/R2에 내구 저장하고 tenant-scoped API로 안전하게 조회·다운로드할 수 있게 한다. 저장 완료 여부와 무관하게 Provider 호출 및 Wallet 정산이 중복되지 않는 durable state machine을 구축한다.

## 배경

Plan 054와 055는 공식 OpenAI SDK의 `POST /v1/audio/speech` 호환, bounded binary streaming과 character 기반 과금 정산을 제공한다. 현재 생성 음성은 요청 연결에서 한 번 전송되고 종료되므로 client disconnect, 장기 보관 또는 반복 다운로드가 필요한 관리형 서비스에는 충분하지 않다.

Plan 053의 managed video output은 비동기 Provider URL을 수집한 후 CDN URL로 바꾸지만 Speech는 동기 binary stream이다. Gateway가 downstream과 object storage로 동시에 복제할 때 느린 client가 storage를 막거나, client disconnect가 이미 과금된 결과를 유실시키거나, object upload 실패가 Provider 재호출을 유발해서는 안 된다. 또한 기본 official SDK 응답을 JSON envelope로 바꾸면 안 되므로 durable delivery는 additive opt-in 계약이어야 한다.

## 범위

- `X-Native-Gateway-Delivery: managed` opt-in과 기본 `stream` mode 유지
- Provider audio stream을 bounded private spool에 수집하면서 downstream으로 native binary 전달
- MIME, declared/actual length, maximum bytes와 SHA-256 검증
- deterministic private S3/R2 object persistence와 conditional idempotent put
- tenant/project/API-key ownership을 갖는 opaque `speechasset_...` identity
- append-only asset events, lease, retry와 orphan cleanup 상태
- `GET /v1/audio/speech/assets/{id}` metadata와 authenticated binary content endpoint
- single-range `Range`/`HEAD`, safe content headers와 bounded streaming download
- original Speech billing charge와 asset publication의 immutable 결합
- client disconnect 후 background persistence 및 existing settlement/reconciliation 연계
- duplicate idempotency, process restart, put response loss와 DB failure의 수렴
- readiness, bounded telemetry, retention cleanup과 Docker Compose/운영 문서
- Python·JavaScript official SDK 기본 mode 회귀와 REST managed-delivery fixture

## 제외 범위

- 기본 `/v1/audio/speech` 응답을 JSON 또는 Job protocol로 변경
- unauthenticated public CDN URL과 permanent bearer download URL
- arbitrary customer URL upload/fetch 또는 SSRF 가능한 redirect
- transcoding, waveform, loudness normalization, codec metadata 추출
- custom voice 저장·consent·복제
- browser presigned upload/download와 multi-range 응답
- storage 용량 기반 Wallet billing 및 archive tier
- 다른 TTS Provider routing/fallback
- Realtime Audio/WebSocket/WebRTC
- Cloud·Dashboard·Conformance 저장소 내부 구현

## 핵심 결정

### 1. Native streaming이 기본이며 managed delivery는 additive opt-in이다

- header가 없거나 `stream`이면 기존 content type과 binary body를 그대로 반환한다.
- `managed`여도 성공 응답 body는 native audio bytes이며 Gateway JSON envelope로 바꾸지 않는다.
- 생성된 asset ID는 body commit 전에 `X-Native-Gateway-Speech-Asset` response header로 제공한다.
- malformed/unsupported delivery mode는 Provider 호출과 Wallet reservation 전에 거부한다.

### 2. Provider stream은 한 번만 읽고 bounded spool을 복제 경계로 사용한다

- Provider body를 maximum byte 제한 아래 private temporary file에 기록하면서 downstream에 전달한다.
- 전체 audio를 `[]byte`로 적재하지 않으며 spool 파일은 `0600`과 설정된 concurrency cap을 사용한다.
- client write 실패가 발생하면 Provider stream 수집은 bounded background context로 계속해 이미 생성된 결과의 저장·정산을 수렴시킨다.
- Provider body read 실패나 byte/MIME 불일치는 asset을 실패로 만들고 기존 ambiguous billing reconciliation 규칙을 따른다. Provider를 재호출하지 않는다.

### 3. Asset은 Speech idempotency와 charge에 결합된 tenant-owned resource다

- managed request의 idempotency publication이 asset ID, request fingerprint, charge ID와 immutable하게 결합된다.
- 동일 key/fingerprint는 기존 completed asset 또는 진행 상태를 재사용하고 새 Provider 호출을 만들지 않는다.
- 다른 fingerprint는 기존 Speech billing idempotency 규칙과 동일하게 conflict 처리한다.
- asset ID만 아는 다른 organization/project/API key는 존재 여부를 구분할 수 없는 not-found를 받는다.

### 4. 저장과 과금은 독립 상태지만 동일 요청으로 수렴한다

- verified Provider usage가 있으면 기존 규칙으로 Capture하며 object upload 실패를 무료 Provider 실패로 간주하지 않는다.
- audio 완료 후 storage만 실패하면 charge를 되돌리거나 Provider를 재호출하지 않고 asset을 `RECONCILING`에 둔다.
- storage가 완료되기 전 asset download는 fail closed하고 metadata는 bounded processing 상태만 노출한다.
- upload 성공 후 DB 응답 유실은 deterministic object key와 HEAD 검증으로 재사용한다.

### 5. 다운로드는 인증된 Gateway origin에서만 제공한다

- bucket/object URL과 S3 credential은 client에 노출하지 않는다.
- metadata/content 요청마다 organization/project/API-key snapshot을 검증한다.
- content endpoint는 `GET`, `HEAD`와 단일 byte range만 지원하고 Content-Type, Content-Length, Content-Range, ETag, Cache-Control만 안전하게 생성한다.
- telemetry에는 asset ID, object key, digest, MIME, byte count 또는 Range 값을 label로 기록하지 않는다.

## 설계 및 구현 순서

### 1. Speech output asset domain과 migration

- `speech_output_assets`, publication, lease와 append-only event table을 추가한다.
- 상태를 `CAPTURING`, `PERSISTING`, `AVAILABLE`, `RECONCILING`, `DELETING`, `DELETED`, `FAILED`로 제한한다.
- owner, charge/request identity, digest, object key와 terminal transition을 DB trigger로 보호한다.

### 2. Managed response capture

- Speech facade가 delivery header를 pre-dispatch 검증하고 asset/publication을 billing request와 결합한다.
- Provider response의 safe MIME/length를 검증한 뒤 private spool과 downstream으로 bounded copy한다.
- downstream disconnect 뒤에는 제한된 detached context에서 Provider read, digest와 storage 작업을 마친다.
- panic, timeout, short read와 spool exhaustion은 bounded category로 기록하고 no-redispatch를 보장한다.

### 3. Private object persistence

- `audio/speech/<organization>/<asset-id>/<digest>.<extension>` deterministic key를 사용한다.
- S3/R2 conditional put, configured SSE와 HEAD identity 검증을 제공한다.
- capture 완료 spool을 seekable stream으로 upload하고 DB의 available transition 뒤에만 content API를 연다.
- cleanup worker가 expired/failed/orphaned asset을 lease로 claim해 object와 spool 잔재를 정리한다.

### 4. Metadata와 authorized content API

- metadata는 opaque ID, status, bytes, content type, timestamps만 반환한다.
- content는 private object를 fixed configured origin에서 가져와 bounded streaming하고 client-controlled URL을 사용하지 않는다.
- HEAD와 single range semantics, invalid/unsatisfiable range, cancellation과 object truncation을 fail closed한다.

### 5. Reconciliation과 운영

- Speech charge reconciliation과 asset persistence reconciliation이 동일 dispatch evidence를 공유하되 원장을 상호 수정하지 않게 한다.
- startup/readiness가 managed speech mode에서 DB와 bucket 접근을 확인한다.
- storage worker metric은 bounded `protocol/stage/outcome`만 사용한다.
- provider mode rollback, retention, orphan 검사와 manual recovery 절차를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

Managed Speech 생성:

```text
POST /v1/audio/speech
Authorization: Bearer SERVICE_KEY
Idempotency-Key: ...
X-Native-Gateway-Delivery: managed
```

성공 응답은 기존 audio bytes를 유지하고 다음 header만 추가한다.

```text
X-Native-Gateway-Speech-Asset: speechasset_...
```

Asset API:

```text
GET  /v1/audio/speech/assets/{asset_id}
GET  /v1/audio/speech/assets/{asset_id}/content
HEAD /v1/audio/speech/assets/{asset_id}/content
DELETE /v1/audio/speech/assets/{asset_id}
```

metadata 응답은 bounded JSON이며 object key, digest, charge ID와 Provider identity를 노출하지 않는다.

### 내부 인터페이스

- `speechstorage.Repository.BeginCapture/MarkCaptured/ClaimPersist/MarkAvailable/RequestDelete`
- `speechstorage.ObjectStore.Put/OpenRange/Delete/Ready`
- `speechstorage.Manager.Begin/Complete/Fail/Persist/Resolve/OpenContent`
- Speech streaming bridge는 downstream 전송 결과와 Provider capture 결과를 별도로 반환한다.

### 데이터베이스 및 migration

- `speech_output_assets`
- `speech_output_asset_publications`
- `speech_output_asset_leases`
- `speech_output_asset_events`
- Speech charge/request와의 immutable foreign-key identity 및 lifecycle/expiry index

audio bytes, request input, voice, prompt, object response body와 Provider credential 컬럼은 만들지 않는다.

### 설정

- `GATEWAY_SPEECH_OUTPUT_STORAGE_MODE=disabled|managed`
- S3/R2 endpoint, region, bucket, access key와 optional server-side encryption
- maximum bytes, concurrent captures, spool directory, capture/upload/download timeout
- retention, cleanup interval/lease와 authorized-download cache policy

### 다른 저장소에 제공하거나 요구하는 계약

- `cloud`: private bucket, encryption/lifecycle, least-privilege credential와 cleanup worker 설정
- `dashboard`: metadata/status/delete UI만 제공하고 audio preview는 별도 명시적 권한 정책 전까지 제외
- `conformance`: official SDK native regression, managed header, disconnect persistence, tenant denial와 Range/download fixtures

각 저장소는 `phase-5-managed-speech-output-delivery` initiative로 독립 local plan을 소유한다.

## 보안 및 과금 고려사항

- input text, voice, 생성 audio, digest, object key, asset/charge ID와 credentials를 log/trace/metric label에 넣지 않는다.
- private bucket은 public-read를 금지하고 Gateway content endpoint만 인증 경계가 된다.
- managed capture가 이미 시작된 뒤 client disconnect가 발생해도 Provider 재호출·reservation 자동 Release를 하지 않는다.
- object storage 실패는 Provider usage evidence를 무효화하지 않으며 storage와 charge reconciliation을 분리한다.
- Range parsing은 overflow, suffix/open-ended 범위와 multi-range를 명시적으로 제한한다.
- response header는 CRLF와 filename injection 여지가 없는 Gateway 생성 상수만 사용한다.
- retention expiry와 DELETE는 active content/capture lease가 끝난 뒤 object를 삭제한다.

## 테스트 계획

### 단위 테스트

- delivery header와 mode별 native response/header projection
- MIME, content length, streamed byte limit, digest와 temp cleanup
- downstream disconnect 뒤 capture continuation 및 no-redispatch
- deterministic object key/extension, conditional reuse와 metadata 비노출
- GET/HEAD/single Range, invalid/multi/unsatisfiable Range
- exactly-once idempotency와 request/charge/asset fingerprint conflict

### 통합 테스트

- PostgreSQL concurrent publication, immutable identity, append-only event와 lease recovery
- actual MinIO put/head/range/delete와 upload response-loss reuse
- client disconnect, restart와 concurrent persistence worker의 단일 object effect
- tenant/project/API-key metadata/content/delete denial
- storage failure 중 Speech Capture/Release/Reconciliation 원장 불변성
- retention/delete와 active download lease race

### 호환성 및 장애 테스트

- 기존 OpenAI Python·JavaScript Speech SDK native binary 회귀
- Python·JavaScript REST managed create→metadata→range download
- Provider slow/short/oversized/corrupt body, S3 timeout/5xx와 DB failure
- process crash at capture, object put, mark-available와 charge settlement 경계
- audio bytes와 민감 identity의 API/log/telemetry/billing DB 부재 감사

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [x] 기본 Speech 요청이 기존 official SDK native binary contract를 그대로 유지함
- [x] managed 요청이 동일 binary와 opaque asset response header를 제공함
- [x] Provider stream이 전체 메모리 적재 없이 downstream과 private spool로 bounded capture됨
- [x] client disconnect와 process restart 뒤 storage/settlement가 Provider 재호출 없이 수렴함
- [x] private S3/R2 conditional persistence와 tenant-scoped metadata/content/delete가 동작함
- [x] authenticated GET/HEAD/single-range download가 safe header와 byte bound를 지킴
- [x] duplicate request/worker가 단일 asset/object/Provider/Ledger effect로 수렴함
- [x] storage 실패가 verified Provider charge를 잘못 Release하거나 이중 Capture하지 않음
- [x] delete/retention과 active capture/download race가 lease로 안전하게 수렴함
- [x] audio/input/voice/object key/digest/credential이 API·log·telemetry·billing DB에 노출되지 않음
- [x] 전체 unit/race/integration/SDK 검사가 통과함
- [x] README, migration, Docker Compose와 멀티레포 handoff가 갱신됨

## 검증 증거

- migration `000055`: tenant/API-key ownership, immutable charge/request/content identity, lifecycle state, lease와 append-only events
- `internal/speechstorage`: bounded mode-0600 capture, downstream disconnect continuation, deterministic conditional S3 persistence, put-response-loss recovery, authenticated read lease와 retention cleanup
- OpenAI facade: opt-in managed header, native binary response와 opaque asset header, completed object replay without Provider/Ledger duplication
- Speech asset API: bounded private metadata, logical delete와 authenticated GET/HEAD/single-range content delivery
- S3 conditional reuse 시 existing object의 exact length와 SHA-256를 재검증하도록 공통 private audio store를 강화함
- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과
- 격리 PostgreSQL `gateway_plan061`, 빈 Redis DB와 실제 MinIO를 사용한 전체 `make integration-test` 통과
- actual MinIO capture/open/delete round trip와 PERSISTING/RECONCILING recovery, active-download/delete race integration tests 통과
- official OpenAI Python·JavaScript Speech 회귀와 Python·Node managed delivery SDK-conformance tests 통과
- 정적 schema/telemetry 감사에서 input, voice, audio body, object key/digest와 credential의 공개·고카디널리티 노출이 없음

## Rollback 계획

- `GATEWAY_SPEECH_OUTPUT_STORAGE_MODE=disabled`로 신규 managed delivery 요청을 fail closed하고 기본 native Speech streaming은 유지한다.
- 이미 capture/persist 중인 asset worker는 drain하여 object/charge 상태를 수렴시킨다.
- additive schema와 append-only event는 감사 및 recovery를 위해 보존한다.
- 저장된 private objects는 retention/maintenance 절차로 삭제하며 DB migration을 역삭제하지 않는다.

## 후속 작업

- speech storage quota, retention tier와 storage billing
- browser-authorized signed download 또는 short-lived CDN token
- additional TTS Provider routing and fallback
- realtime transcription WebSocket과 batch transcription Jobs
- audio transcoding, waveform와 playback metadata pipeline
