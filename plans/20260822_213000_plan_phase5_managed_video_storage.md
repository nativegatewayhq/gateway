---
id: gateway-20260822-053
title: Phase 5 Managed Video Output Storage and CDN Delivery
status: completed
created_at: 2026-08-22T21:30:00+09:00
updated_at: 2026-08-22T23:30:00+09:00
owners:
  - gateway
initiative: phase-5-managed-video-storage
depends_on:
  - gateway-20260820-027
  - gateway-20260820-029
  - gateway-20260821-035
  - gateway-20260822-050
  - gateway-20260822-051
  - gateway-20260822-052
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 Managed Video Output Storage and CDN Delivery

## 목적

Runway terminal task의 만료되는 Provider video output을 bounded streaming으로 검증·다운로드하여 S3/R2에 내구적으로 저장하고, 원본 native task response의 `output` URL만 Gateway CDN URL로 교체한 뒤 과금 정산과 사용자 조회를 exactly-once로 완료한다.

## 배경

Plan 050–052는 native Runway task, credit settlement와 ephemeral input upload를 제공한다. 그러나 Runway output URL은 접근 후 약 24–48시간 내 만료되므로 관리형 서비스가 그대로 반환하면 성공한 유료 결과를 다시 내려받을 수 없다. 기존 `internal/imagestorage`는 Provider URL/Base64 이미지를 임시 파일로 bounded 수집하고 S3/R2에 저장하지만, synchronous response와 image 전용 schema/object key를 전제로 한다.

영상 결과는 asynchronous Job terminal observation과 Billing settlement 사이에서 저장되어야 한다. Provider 성공을 관측했더라도 저장이 실패하면 native success snapshot을 사용자에게 확정하거나 charge를 Capture해서는 안 된다. 반대로 object upload와 DB 상태 확정 사이에 프로세스가 종료되어도 같은 object key와 durable lease로 재시작 후 수렴해야 한다.

참조 계약:

- [Runway output formats and expiry](https://docs.dev.runwayml.com/assets/outputs/)
- `gateway-20260820-027` managed image storage 불변 조건
- `gateway-20260820-029` durable Job observation/settlement state machine
- `gateway-20260822-051` terminal Provider credit settlement

## 범위

- video 전용 managed storage configuration과 provider/managed mode
- Runway terminal `SUCCEEDED.output[]`의 bounded native schema 추출
- allowlisted Runway output origin, DNS/IP/redirect/content-type/content-length 검증
- response body 전체를 메모리에 적재하지 않는 임시 파일 streaming download
- per-video 및 request 전체 byte limit, 다운로드·upload timeout과 동시성 bound
- SHA-256, content type, byte length 기반 immutable object identity
- S3/R2 multipart-compatible streaming upload와 CDN URL 생성
- durable `video_assets` state, lease, retry, orphan recovery와 append-only events
- Job terminal Provider snapshot과 managed client snapshot 분리
- storage 완료 후 native `output` URL만 CDN URL로 교체
- storage success 뒤 verified Runway credits Capture, 저장 실패 시 reservation 유지/reconciliation
- duplicate poll, worker 경쟁, restart와 object-put response loss의 exactly-once 수렴
- 관리 Job API의 managed result projection
- readiness, OpenTelemetry, 운영 runbook과 lifecycle/retention 문서

## 제외 범위

- Provider가 실패·취소한 task의 output 저장
- live video streaming, transcoding, thumbnail, codec 변환과 metadata extraction
- 사용자 입력 asset의 영구 저장
- CDN signed URL/token 발급과 tenant별 download authorization
- object lifecycle 삭제 worker 및 장기 archive tier
- Runway 외 video Provider와 cross-provider output normalization
- audio output 저장
- 대시보드 UI와 cloud bucket provisioning 구현

## 핵심 결정

### 1. Storage는 terminal observation과 Billing Capture 사이의 durable 단계다

- Provider `SUCCEEDED` snapshot과 verified `cost.credits`를 먼저 immutable evidence로 저장한다.
- Job settlement claim은 managed mode에서 video storage state가 `AVAILABLE`일 때만 Billing Capture를 호출한다.
- fetch/upload/DB 확정 실패는 Job을 `RECONCILING`으로 유지하고 charge reservation을 임의 Release하지 않는다.
- Provider task를 다시 생성하거나 다른 Provider로 fallback하지 않는다.

### 2. Provider evidence와 client snapshot을 분리한다

- 원본 output URL을 포함한 Provider snapshot은 size-bounded internal recovery evidence이며 공개 관리 API에서 숨긴다.
- client snapshot은 `id`, status, cost, failure 등 native field를 유지하고 `output[]` URL만 CDN URL로 바꾼다.
- output URL, object key, CDN URL, Provider task ID를 log/metric/trace attribute로 기록하지 않는다.

### 3. 다운로드는 fail-closed SSRF 경계다

- Runway channel에 versioned allowlist로 등록된 HTTPS origin만 fetch한다.
- hostname은 DNS resolve 후 public IP만 허용하고 연결 시 해당 IP에 pinning하며 redirect를 따르지 않는다.
- `video/*` allowlist, positive bounded `Content-Length`와 streamed byte count를 모두 검증한다.
- compressed transfer, unknown length와 limit 초과는 저장 실패로 분류한다.

### 4. 재실행 가능한 object identity를 사용한다

- object key는 `videos/runway/<charge-or-job-id>/<index>-<sha256>.<extension>`으로 결정한다.
- 동일 content/object key put은 overwrite-safe 또는 conditional idempotent semantics를 사용한다.
- object upload 성공 후 DB 확정 실패 시 동일 key를 재검사·재사용하고 orphan audit를 남긴다.
- 동일 Job/index에 다른 digest가 관측되면 Provider evidence conflict로 manual review한다.

## 설계 및 구현 순서

### 1. Video storage domain과 설정

- image storage의 safe collector/S3 primitives를 modality-neutral하게 추출하거나 video 전용 wrapper로 재사용한다.
- `GATEWAY_VIDEO_STORAGE_MODE`, bucket/endpoint/region/CDN credential, byte/time/concurrency/origin 설정을 추가한다.
- managed mode의 필수 설정과 readiness를 startup에서 fail closed 검증한다.

### 2. Durable asset schema

- `video_assets`, lease/state/evidence와 append-only `video_asset_events` migration을 추가한다.
- Job ID/result index, charge, Provider/channel, object identity와 bounded metadata를 저장한다.
- raw Provider/CDN URL과 content는 저장하지 않는다.

### 3. Safe streaming collection

- terminal Runway snapshot에서 output string array를 최대 개수까지 검증한다.
- allowlisted origin과 pinned DNS transport로 temporary file에 streamed download한다.
- content type, content length, actual bytes, SHA-256와 extension을 일관되게 검증한다.
- temporary file은 성공·실패·취소·panic 모두에서 제거한다.

### 4. S3/R2 persistence와 native rewrite

- deterministic object key로 streaming Put/multipart upload한다.
- durable asset를 `AVAILABLE`로 확정한 뒤 client snapshot의 output URL만 CDN URL로 바꾼다.
- 모든 output이 available일 때 단일 managed Job snapshot을 확정한다.

### 5. Job settlement orchestration

- async worker에 `terminal observed → storage claim → managed snapshot → billing settlement` 단계를 추가한다.
- lease expiry, duplicate poll, object put response loss와 worker crash를 멱등적으로 회복한다.
- storage failure/retry exhaustion은 reservation을 유지한 manual reconciliation으로 수렴한다.

### 6. 관리·관측·호환성

- native Runway retrieve가 저장 완료 전 processing/reconciling, 완료 후 succeeded+CDN output을 반환한다.
- 관리 API는 bounded CDN result만 노출하고 Provider URL/object key는 숨긴다.
- storage transition metric은 bounded protocol/stage/outcome만 사용한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 Runway `GET /v1/tasks/{gateway-job-id}` 형식을 유지한다. managed mode에서 성공 응답의 `output` 배열만 Gateway CDN HTTPS URL을 포함한다. provider mode는 기존 Provider URL을 유지한다.

### 내부 인터페이스

- `videostorage.Manager.Transform(ctx, TerminalInput) -> ManagedSnapshot`
- `videostorage.Repository.Begin/Claim/MarkAvailable/Release`
- Job settlement worker가 optional `TerminalResultManager`를 storage-before-capture hook으로 호출한다.
- collector와 object store는 `io.Reader`/seekable temporary file을 사용하며 byte slice를 요구하지 않는다.

### 데이터베이스 및 migration

- `video_assets`: Job/charge/result identity, digest, content type/length, object key, state와 lease
- `video_asset_events`: claimed, available, retry, orphan/manual categories의 append-only audit
- async Job에 managed snapshot version/hash 또는 별도 bounded snapshot table
- additive migration이며 provider mode와 기존 image/Replicate/fal Job은 nullable/new table을 무시한다.

### 다른 저장소에 제공하거나 요구하는 계약

- `cloud`: S3/R2 credential, Runway output origin allowlist, CDN origin과 lifecycle policy를 secret manager/IaC로 배포한다.
- `dashboard`: managed CDN result와 storage state만 표시하고 Provider URL/object key는 노출하지 않는다.
- `conformance`: Provider output fixture, large streaming body, restart/duplicate worker와 native SDK retrieve 결과를 외부 HTTP에서 검증한다.

## 보안 및 과금 고려사항

- Provider output fetch는 arbitrary user URL fetch가 아니며 selected Provider/channel allowlist에만 제한한다.
- DNS rebinding, private/link-local/metadata IP, redirect, userinfo, fragment, invalid port와 oversized/unknown body를 거부한다.
- S3 credential, signed Provider URL, object key, CDN URL과 content를 telemetry/log/error에 포함하지 않는다.
- `SUCCEEDED`라도 storage가 완료되기 전에는 Capture하지 않으며, storage 실패를 Provider 무료 실패로 오인해 Release하지 않는다.
- 동일 Job/index의 중복 작업은 하나의 asset lease와 object identity로 수렴한다.
- client cancellation은 이미 시작된 bounded persistence/reconciliation을 임의 중단해 원장과 object 상태를 분리하지 않는다.

## 테스트 계획

### 단위 테스트

- Runway terminal output schema, count, URL/origin/content-type/length validation
- DNS public-IP pinning, redirect/timeout/short-read/oversize와 temp cleanup
- object key, SHA-256, extension, CDN rewrite와 raw URL 비노출
- storage-before-capture state classification

### 통합 테스트

- PostgreSQL asset lease/event/immutable identity와 restart recovery
- concurrent workers의 단일 fetch/put/Capture
- put success/response loss/DB failure 후 exactly-once convergence
- storage failure가 reservation과 Job reconciliation을 유지함
- existing image storage, Runway billing, Replicate/fal Job migration 회귀

### 호환성 및 장애 테스트

- 공식 Runway Python/JavaScript task retrieve가 managed CDN output을 parse함
- multi-output, maximum-size streamed body와 Gateway memory bound
- Provider URL expiry, DNS change, 3xx, 404/429/5xx, slow body와 S3 timeout
- process crash at fetch, put, DB mark-available, snapshot commit and Billing Capture boundaries

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/runway -count=1
```

## 완료 조건

- [x] managed mode의 Runway success output이 만료되는 Provider URL 대신 안정적인 CDN URL을 반환함
- [x] video download/upload가 전체 파일을 메모리에 적재하지 않고 설정된 byte/concurrency bound를 지킴
- [x] SSRF, redirect, DNS rebinding, content-type/length와 timeout 경계가 fail closed함
- [x] duplicate poll/worker와 crash 재시도가 단일 deterministic object, managed snapshot 및 settlement로 수렴함
- [x] storage 완료 전 Capture되지 않고 실패·불명확 결과가 reservation/reconciliation을 유지함
- [x] Provider URL, object key, content, credential과 signed URL이 public API/telemetry/log에 노출되지 않음
- [x] provider mode와 기존 image/Replicate/fal 동작이 회귀하지 않음
- [x] 전체 unit/race/integration/SDK 회귀가 통과함
- [x] README, migration, 운영 설정과 멀티레포 handoff가 갱신됨

## 검증 증거

- `internal/videostorage`: exact-origin/public-IP-pinned collector, MIME signature 및 byte bound, temporary-file streaming, deterministic S3/R2 persistence와 native Runway output rewrite
- migration `000047`: immutable `video_assets`, append-only asset events, per-Job managed-result policy와 별도 managed response snapshot
- async worker: managed snapshot 저장을 Billing Capture보다 먼저 실행하고 lease-aware snapshot replay로 재시도 수렴
- Runway facade와 management projection: 저장 전 Provider URL을 숨기고 저장 완료 후 CDN URL만 공개
- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과
- Plan 전용 PostgreSQL DB와 격리 Redis DB 14에서 `make integration-test` 통과
- `GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/runway -count=1` 통과

## Rollback 계획

- `GATEWAY_VIDEO_STORAGE_MODE=provider`로 신규 managed persistence를 중단한다.
- 이미 storage/settlement 중인 Job은 durable worker가 drain하도록 유지하고 reservation을 임의 해제하지 않는다.
- additive asset/snapshot schema와 저장된 object는 감사와 recovery를 위해 보존한다.
- Runway native task, input uploads와 Billing settlement는 provider output mode에서 계속 동작한다.

## 후속 작업

- managed video object lifecycle deletion과 tenant download authorization
- video thumbnail/transcoding pipeline
- 추가 Runway video operation과 Provider adapter
- Speech/Transcription native foundation 및 audio storage/billing
- cross-provider video routing/fallback
