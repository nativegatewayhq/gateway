---
id: gateway-20260824-060
title: Phase 5 Managed Audio Input Assets and Reusable References
status: completed
created_at: 2026-08-24T13:35:00+09:00
updated_at: 2026-08-24T14:18:00+09:00
owners:
  - gateway
initiative: phase-5-managed-audio-input-assets
depends_on:
  - gateway-20260820-006
  - gateway-20260820-019
  - gateway-20260824-056
  - gateway-20260824-057
  - gateway-20260824-058
  - gateway-20260824-059
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 Managed Audio Input Assets and Reusable References

## 목적

사용자가 동일한 audio를 반복 업로드하지 않고 tenant-scoped opaque asset reference로 OpenAI transcription과 translation 요청에 재사용할 수 있도록 private S3/R2 저장소, lifecycle API와 dispatch-time materialization을 구현한다. 기존 공식 SDK의 native multipart `file` 경로는 그대로 유지한다.

## 배경

현재 transcription과 translation은 요청마다 audio bytes를 Gateway의 bounded multipart spool에 올린 뒤 Provider로 다시 전송한다. 이 방식은 공식 SDK와 호환되지만 동일 입력을 여러 모델·옵션 또는 재시도 작업에서 사용할 때 ingress와 임시 디스크 비용이 반복된다. Plan 057과 059의 billing evidence는 의도적으로 audio나 filename을 저장하지 않으므로 reusable content는 과금 원장이 아닌 별도의 tenant-scoped asset domain에서 관리해야 한다.

OpenAI native audio endpoints는 multipart file을 요구하며 Provider file reference를 표준 입력으로 제공하지 않는다. 따라서 Gateway는 native `file`을 변경하지 않고 별도의 additive extension을 제공한다. reference 사용자는 Gateway REST 또는 향후 얇은 helper를 사용하고, 공식 SDK 사용자는 기존 file upload를 계속 사용할 수 있다.

## 범위

- authenticated `POST /v1/audio/assets`, `GET /v1/audio/assets/{id}`, `DELETE /v1/audio/assets/{id}` lifecycle
- opaque `audasset_...` reference와 organization/project/API-key ownership snapshot
- private S3/R2 object storage, server-side encryption option과 tenant-partitioned object key
- bounded streaming multipart ingestion, exact byte limit, MIME allowlist, SHA-256와 idempotent create
- `X-Native-Gateway-Audio-Asset` request extension으로 transcription/translation asset 선택
- native `file`과 asset reference의 exactly-one validation 및 pre-dispatch authorization
- available asset을 bounded local spool로 materialize한 뒤 기존 fixed-origin Provider adapter에 file part로 전송
- lifecycle expiry, logical deletion, reference lease와 orphan object cleanup worker
- object-store readiness, storage telemetry, configuration와 Docker Compose documentation
- Python·JavaScript REST/helper fixture와 기존 official SDK file-upload regression

## 제외 범위

- public CDN URL 또는 unauthenticated audio delivery
- Provider에 customer-controlled URL을 전달하는 방식
- cross-organization/project asset sharing
- global content deduplication 또는 content hash lookup API
- browser direct/multipart presigned upload
- audio playback, waveform, codec decode, duration 추정 또는 transcoding
- antivirus/content moderation 결과의 의미론적 보장
- Provider Files API emulation
- billing evidence나 idempotency response에 audio bytes 저장
- speech output의 durable managed delivery
- Cloud·Dashboard·Conformance 저장소 내부 구현

## 핵심 결정

### 1. native file path와 reference extension을 분리한다

- 기존 multipart `file` 요청은 byte-for-byte 호환 경로를 유지한다.
- reference 요청은 `X-Native-Gateway-Audio-Asset: audasset_...`을 사용하고 multipart에 `file`을 포함하지 않는다.
- 둘 다 있거나 둘 다 없으면 Provider 호출과 Wallet reservation 전에 거부한다.
- extension header는 Provider에 전달하지 않고 materialized bytes를 native `file` part로 재구성한다.

### 2. asset은 bearer token이 아니라 tenant-owned identifier다

- 무작위 128-bit ID만으로 접근을 허용하지 않고 인증 principal의 organization/project/API-key snapshot을 매번 대조한다.
- 다른 tenant의 유효한 ID, deleted/expired/pending asset은 동일한 bounded not-found 응답으로 fail closed한다.
- asset metadata/listing은 content digest나 object key를 공개하지 않는다.

### 3. private storage와 원장은 분리한다

- object는 public-read나 CDN 없이 private bucket에 저장한다.
- billing charge에는 기존 fingerprint와 asset ID digest만 결합하고 audio, filename, object key를 저장하지 않는다.
- asset DB에는 storage에 필요한 MIME, byte length, digest, object key와 lifecycle state만 저장하며 prompt/transcript/translation 결과를 저장하지 않는다.

### 4. 재사용은 dispatch 횟수를 줄이지 않는다

- asset reference는 ingress/storage 재사용 기능이며 각 API 요청은 별도의 idempotency key, reservation, Provider dispatch와 settlement를 가진다.
- same request idempotency key는 기존 billing state machine이 중복 dispatch를 막는다.
- asset을 읽은 뒤 timeout/reset이 발생해도 facade는 Provider를 재호출하지 않는다.

### 5. 삭제와 cleanup은 race-safe하다

- dispatch는 짧은 DB lease를 획득한 뒤 object를 materialize한다.
- DELETE는 logical `DELETING`으로 전환하고 활성 lease가 없을 때 worker가 object를 삭제한다.
- object deletion 실패는 재시도하며 DB row를 먼저 지우지 않는다.
- retention expiry도 동일한 state machine을 사용하고 audit event는 append-only로 보존한다.

## 설계 및 구현 순서

### 1. Asset domain과 migration

- `audio_input_assets`, append-only `audio_input_asset_events`, dispatch lease와 create idempotency publication을 추가한다.
- 상태는 `UPLOADING`, `AVAILABLE`, `DELETING`, `DELETED`, `FAILED`로 제한한다.
- identity, digest, object key, ownership과 terminal state mutation을 DB trigger로 보호한다.

### 2. Storage ingestion과 lifecycle API

- 독립 request/file/field/concurrency limit 아래 multipart file을 bounded spool에 수집한다.
- allowlisted `audio/*` MIME과 magic sniffing 일치, positive length, maximum bytes를 검증한다.
- organization-scoped deterministic object key와 random asset ID를 생성하고 S3 conditional put/reuse를 수행한다.
- create idempotency key의 same fingerprint는 같은 asset을, 다른 fingerprint는 conflict를 반환한다.

### 3. Transcription/translation reference dispatch

- facade가 header reference와 file의 exactly-one 조건을 먼저 확인한다.
- authenticated owner, `AVAILABLE`, expiry와 lease를 확인한 뒤 private object를 bounded spool에 내려받는다.
- stored canonical filename과 MIME으로 기존 Provider multipart builder를 사용한다.
- request fingerprint에는 asset ID, immutable digest와 operation options를 포함해 content substitution을 막는다.

### 4. Cleanup, readiness와 운영

- expired/deleting/failed asset을 leased worker가 object 삭제 후 terminalize한다.
- startup과 `/health/ready`가 managed audio mode의 DB와 object-store 접근을 확인한다.
- telemetry는 stage/outcome만 기록하고 tenant, asset ID, object key, digest, MIME과 bytes를 label에 넣지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST   /v1/audio/assets
GET    /v1/audio/assets/{asset_id}
DELETE /v1/audio/assets/{asset_id}
```

Create는 multipart `file` 하나와 `Idempotency-Key`를 요구하고 다음 bounded JSON을 반환한다.

```json
{
  "id": "audasset_...",
  "object": "audio.asset",
  "bytes": 123456,
  "content_type": "audio/wav",
  "status": "available",
  "created_at": 1787546100,
  "expires_at": 1788150900
}
```

Reference dispatch:

```text
POST /v1/audio/transcriptions
X-Native-Gateway-Audio-Asset: audasset_...

POST /v1/audio/translations
X-Native-Gateway-Audio-Asset: audasset_...
```

기존 multipart options는 유지하되 `file` part만 생략한다. BYOK와 billing-required 모두 asset ownership 검증을 적용하며 billing-required의 Idempotency와 usage/duration 규칙은 그대로 유지한다.

### 내부 인터페이스

- `audioassets.Repository.Begin/Complete/Acquire/Release/Delete/ClaimCleanup`
- `audioassets.ObjectStore.Put/Get/Delete/Ready`
- `audioassets.Service.Create/Resolve/Delete/RunCleanup`
- `AudioAssetResolver.Materialize(principal, assetID) -> bounded spool metadata`

### 데이터베이스 및 migration

- `audio_input_assets`
- `audio_input_asset_publications`
- `audio_input_asset_leases`
- `audio_input_asset_events`
- ownership, lifecycle, expiry와 cleanup 인덱스

audio bytes, prompt, transcript, translation result, Provider credential과 response body 컬럼은 만들지 않는다.

### 설정

- `GATEWAY_AUDIO_INPUT_STORAGE_MODE=disabled|managed`
- `GATEWAY_AUDIO_INPUT_STORAGE_ENDPOINT`, `REGION`, `BUCKET`, `ACCESS_KEY_ID`, `SECRET_ACCESS_KEY`
- `GATEWAY_AUDIO_INPUT_STORAGE_MAX_BYTES`, `MAX_CONCURRENT_UPLOADS`, `UPLOAD_TIMEOUT`, `DOWNLOAD_TIMEOUT`
- `GATEWAY_AUDIO_INPUT_STORAGE_RETENTION`, `CLEANUP_INTERVAL`, `CLEANUP_LEASE`
- `GATEWAY_AUDIO_INPUT_STORAGE_ALLOWED_CONTENT_TYPES`

### 다른 저장소에 제공하거나 요구하는 계약

- `cloud`: private bucket, encryption/lifecycle policy, least-privilege secret와 cleanup worker 설정
- `dashboard`: tenant-scoped asset metadata/list/delete UI; object key/digest/audio preview 비노출
- `conformance`: cross-tenant denial, create idempotency, native file regression, reference dispatch, delete/expiry race와 S3 fault fixtures

각 저장소는 `phase-5-managed-audio-input-assets` initiative로 독립 local plan을 소유한다.

## 보안 및 과금 고려사항

- bucket은 private이며 S3 credential, signed request, object key와 audio content를 log/trace/metric에 넣지 않는다.
- endpoint는 configured S3/R2 origin만 사용하며 customer URL fetch를 지원하지 않아 SSRF 경계를 단순화한다.
- content type은 request header만 신뢰하지 않고 bounded sniffing과 allowlist가 일치해야 한다.
- digest 기반 global dedup을 하지 않아 tenant 간 content-existence side channel을 만들지 않는다.
- object key는 organization partition과 random asset identity를 포함하며 응답에 노출하지 않는다.
- asset upload 자체는 Wallet charge를 만들지 않는다. storage quota/billing은 후속 계획으로 남기고 per-project count/bytes hard limit으로 abuse를 차단한다.
- Provider dispatch 전에 asset authorization/materialization과 managed billing reservation이 모두 완료되어야 한다.
- 삭제된 asset으로 새 dispatch는 불가능하지만 이미 lease를 얻은 request의 settlement는 정상 완료한다.

## 테스트 계획

### 단위 테스트

- asset ID/object key, ownership, expiry와 state transition
- multipart file count, byte/MIME/magic/field/concurrency limit
- create/reference fingerprint와 idempotency conflict
- file/reference exactly-one validation과 header stripping
- response projection에서 object key/digest 비노출

### 통합 테스트

- PostgreSQL concurrent create 단일 asset/publication과 append-only event
- S3 put/get/delete fault, conditional reuse와 cleanup lease recovery
- cross-tenant/project/API-key read/delete/reference denial
- delete와 active dispatch lease race, expiry convergence와 restart recovery
- managed billing reservation 전에 missing/deleted asset이 fail closed됨
- asset reference의 transcription token settlement와 translation duration settlement

### 호환성 및 장애 테스트

- 기존 Python·JavaScript official SDK multipart file regression
- Python·JavaScript REST/helper create→transcribe/translate reference flow
- DB/S3 unavailable, partial upload, corrupt/truncated object와 content mismatch
- Provider timeout/reset/panic 후 no-redispatch와 asset lease release
- concurrent idempotency/reference 요청의 단일 Provider/Ledger effect

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [x] private S3/R2 asset create/get/delete가 tenant와 API-key 범위에서 동작함
- [x] bounded ingestion이 크기·MIME·magic·동시성 제한을 fail closed함
- [x] create idempotency가 동일 content를 재업로드/중복 기록하지 않음
- [x] native multipart file과 asset reference가 exactly-one으로 검증됨
- [x] available reference가 transcription/translation native file part로 안전하게 materialize됨
- [x] 다른 tenant, deleted, expired, pending asset이 Provider·Ledger 전에 거부됨
- [x] dispatch fingerprint가 immutable asset digest를 결합해 substitution을 차단함
- [x] delete/expiry와 active dispatch race가 lease로 안전하게 수렴함
- [x] cleanup restart/lease recovery와 object deletion retry가 동작함
- [x] audio/object key/digest/credential이 응답·log·telemetry·billing DB에 노출되지 않음
- [x] 기존 official SDK file upload와 managed settlement 회귀 검사가 통과함
- [x] 전체 unit/race/integration/SDK 검사가 통과함
- [x] README, migration, Docker Compose와 멀티레포 handoff가 갱신됨

## 검증 증거

- `GOCACHE=/private/tmp/gateway-go-cache make check`
- 신규 `gateway_plan060` PostgreSQL DB와 Redis를 사용한 전체 integration suite. 공유 Redis DB 잔존 키로 실패한 기존 rate-limit package는 빈 DB 13에서 격리 재실행하여 통과했다.
- 실제 private MinIO bucket에 대한 conditional put/get/delete/readiness round trip
- `go test -race -count=1 -tags=sdkconformance ./protocols/openai -run TestPythonAndJavaScriptReusableAudioAssetFlow`
- concurrent publication, cross-tenant/API-key ownership, append-only event, active lease/delete/cleanup 수렴 integration tests
- response projection, MIME/magic, exactly-one file/reference와 transcription/translation native materialization unit tests

## Rollback 계획

- `GATEWAY_AUDIO_INPUT_STORAGE_MODE=disabled`로 신규 create/reference와 cleanup claim을 중단한다.
- 기존 native multipart file 요청은 storage 설정과 무관하게 계속 동작한다.
- 이미 `DELETING`인 object cleanup은 별도 maintenance 실행으로 수렴시키고 DB event를 보존한다.
- private objects와 asset rows는 운영 retention 절차에 따라 삭제하며 migration을 역삭제하지 않는다.

## 후속 작업

- browser direct multipart/presigned audio upload
- storage quota, retention tier와 storage billing
- realtime transcription WebSocket and batch transcription Jobs
- cross-provider STT/translation routing and fallback
