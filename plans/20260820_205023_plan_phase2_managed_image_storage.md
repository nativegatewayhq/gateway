---
id: gateway-20260820-027
title: Phase 2 Managed Image Storage and CDN Delivery
status: completed
created_at: 2026-08-20T20:50:23+09:00
updated_at: 2026-08-20T21:19:24+09:00
owners:
  - gateway
initiative: phase-2-managed-image-storage
depends_on:
  - gateway-20260820-004
  - gateway-20260820-005
  - gateway-20260820-006
  - gateway-20260820-011
  - gateway-20260820-012
  - gateway-20260820-013
  - gateway-20260820-026
supersedes: []
affected_repos:
  - gateway
  - cloud
  - conformance
---

# Phase 2 Managed Image Storage and CDN Delivery

## 목적

OpenAI Images와 Gemini 이미지 응답에 포함된 Provider 임시 URL 또는 Base64 이미지 데이터를 Gateway 소유 S3-compatible object storage에 안전하게 보존하고, native response 형식을 유지한 채 안정적인 CDN URL로 교체할 수 있는 `provider`/`managed` 저장 모드를 제공한다.

## 배경

현재 Gateway는 Provider 성공 응답을 byte-for-byte 전달하므로 SDK 호환성은 높지만 Provider URL의 짧은 만료 시간과 Base64 응답의 큰 전송 비용을 그대로 고객에게 전달한다. 관리형 서비스는 생성 결과를 재접근 가능한 URL로 제공해야 한다. 반면 임의 Provider URL fetch는 SSRF, DNS rebinding, redirect, 압축 폭탄과 메모리 고갈 경계가 되고, Provider 성공 이후 storage 실패를 Provider 실패로 잘못 환불하면 이미 발생한 원가가 손실된다. 따라서 response 변환, 안전한 fetch, streaming upload, billing settlement와 idempotent replay의 순서를 하나의 명시적 계약으로 구현해야 한다.

## 범위

- `provider`와 `managed` image result storage mode
- S3-compatible object store와 public CDN URL 구성
- OpenAI `data[].url` 및 `data[].b64_json` 결과 수집과 native JSON 재작성
- Gemini `inlineData`/`inline_data` 이미지 part 수집과 native JSON 재작성
- allowlisted Provider origin만 허용하는 URL fetcher
- DNS/IP 검증, redirect 금지, content type와 byte 제한, streaming download/upload
- request/result 단위 deterministic object key와 idempotent put
- 저장 객체 metadata, content type, checksum과 수명 정책 계약
- Provider dispatch 성공 후 managed transform 결과를 billing response snapshot에 저장
- 저장 실패의 명시적 settlement/error/reconciliation 의미
- disabled/provider mode의 기존 byte-for-byte pass-through 보존
- readiness, bounded logs, 운영 설정과 Cloud/Conformance handoff

## 제외 범위

- 고객이 임의 URL을 입력하는 image edit source fetch
- signed upload URL과 고객 파일 업로드 API
- 영상·음성·LLM attachment 저장
- 이미지 변환, resize, moderation, EXIF 처리와 transcoding
- Dashboard asset browser와 사용자 삭제 API
- multi-region replication과 CDN purge control plane
- storage byte/egress 기반 고객 과금
- Provider API에 Gateway CDN URL을 입력하는 cross-provider conversion

## 설계 및 구현 순서

### 1. 저장 모드와 object store 계약

- `GATEWAY_IMAGE_STORAGE_MODE=provider|managed`를 추가하고 기본값은 `provider`로 둔다.
- managed mode는 bucket, endpoint/region, public CDN base URL, 최대 이미지 수·개별 byte·전체 byte, fetch/upload timeout을 요구한다.
- 내부 `ObjectStore.Put(ctx, key, contentType, reader, size, checksum)` 인터페이스는 body를 메모리에 전부 적재하지 않는다.
- object key는 protocol, charge/request identity, result index와 content digest로 구성하되 customer ID, prompt, Provider credential과 원본 URL을 포함하지 않는다.
- 동일 key의 재시도는 동일 객체를 반환하며 partial upload는 공개 URL로 노출하지 않는다.

### 2. 안전한 Provider URL 수집

- fetch 대상은 route에서 확정된 Provider의 고정 allowlist origin과 HTTPS만 허용한다.
- URL userinfo, fragment, non-default unsafe port, IP literal, localhost와 private/link-local/loopback/multicast/reserved 주소를 거부한다.
- DNS resolve 결과를 모두 검증하고 실제 dial 주소를 검증된 IP에 고정해 DNS rebinding을 막는다.
- redirect는 따르지 않으며 credential/cookie와 inbound header를 전달하지 않는다.
- response header와 bounded reader 양쪽에서 size를 제한하고 image MIME allowlist 및 magic-byte 일치를 검사한다.
- timeout/cancel 시 download와 multipart upload를 즉시 닫고 raw URL/error/body를 로그에 남기지 않는다.

### 3. Native response transform

- OpenAI 응답은 `data` 순서와 알려지지 않은 모든 필드를 보존하면서 `url` 또는 `b64_json` 결과만 managed CDN URL로 치환한다.
- Gemini 응답은 candidates/parts 순서와 unknown fields를 보존하면서 image MIME의 inline data만 CDN-backed `fileData`/Provider-compatible 결과 계약으로 치환한다.
- JSON 숫자와 unknown extension의 의미 손실을 막기 위해 bounded raw-message transformer를 사용한다.
- image가 없는 성공 응답은 그대로 전달하고, malformed Provider success JSON은 storage 변환 실패로 분류한다.
- 여러 이미지 중 하나라도 저장 실패하면 부분 CDN 응답을 고객에게 반환하지 않는다.

### 4. Billing, idempotency와 실패 의미

- Provider dispatch 결과는 기존 health observation에 정확히 한 번 반영하며 storage 결과는 Provider health score에 포함하지 않는다.
- known Provider success는 원가가 이미 발생했으므로 storage 실패만으로 Reserve를 Release/Refund하지 않는다.
- 변환 성공 후의 최종 native response snapshot을 Capture와 함께 저장해 terminal replay가 Provider와 storage를 다시 호출하지 않게 한다.
- Provider 성공과 storage 결과의 원자적 DB 확정이 불가능한 경계는 charge를 `RECONCILING`으로 유지하고 deterministic object key로 worker가 upload/settlement를 재개한다.
- 동일 request/charge의 concurrent retry는 한 번만 외부 fetch/upload하고 동일 snapshot을 반환한다.
- storage 실패 응답은 protocol-native bounded 502/503을 사용하고 bucket/key/upstream URL을 노출하지 않는다.

### 5. 운영, readiness와 수명 정책

- managed mode startup 시 설정을 검증하고 object store probe 실패 시 `/health/ready`를 unavailable로 만든다.
- probe는 destructive delete에 의존하지 않는 bounded head/list 또는 전용 canary object 계약을 사용한다.
- 로그는 request ID, protocol, Provider, bounded stage/category, result count와 byte bucket만 포함한다.
- 객체 보존 기간과 orphan cleanup은 tag/metadata를 통해 Cloud lifecycle policy가 집행할 수 있게 한다.
- provider mode는 object store client를 만들거나 readiness/fetch/transform 경로를 호출하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

기존 OpenAI/Gemini endpoint와 인증 방식은 변경하지 않는다. 초기 storage mode는 deployment 전역 설정으로 제공하고 고객이 request body로 bucket, object key, fetch URL 또는 storage credential을 지정할 수 없게 한다. managed mode의 OpenAI 결과는 기존 `url` 필드에 CDN URL을 반환한다. Gemini의 정확한 native wire representation은 공식 SDK conformance fixture로 고정한다.

### 내부 인터페이스

```go
type ObjectStore interface {
    Put(ctx context.Context, object Object, body io.Reader) (StoredObject, error)
    Ready(ctx context.Context) error
}

type ResultManager interface {
    Transform(ctx context.Context, route image.RoutingDecision, response ProviderResponse) (ManagedResponse, error)
}
```

`ProviderAssetFetcher`는 route Provider에서 파생된 origin policy만 받고 외부 request 값으로 allowlist를 확장하지 않는다. transformer는 final response bytes와 저장된 object reference 목록을 함께 반환한다.

### 데이터베이스 및 migration

새 append-only `image_assets` 테이블을 추가한다.

- immutable asset ID, charge ID, request/result index
- protocol, Provider와 channel snapshot ID
- object key, content type, byte length, SHA-256 checksum
- lifecycle state (`PENDING`, `AVAILABLE`, `FAILED`, `ORPHANED`)
- bounded failure category와 timestamps

object key와 `(charge_id, result_index)`는 unique이며 update 가능한 필드는 lifecycle state와 bounded failure category로 제한한다. 기존 charge/ledger row 의미를 변경하지 않으며 migration rollback은 새 binary가 읽지 않는 테이블을 남긴 채 provider mode로 전환한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 동일 initiative에서 R2/S3 bucket, least-privilege credential, CDN custom domain, CORS/cache/lifecycle 정책, readiness alert와 secret 배포를 소유한다. Conformance는 OpenAI Python/JavaScript와 Google Gen AI Python/JavaScript에서 managed URL wire shape, unknown-field 보존, replay 및 provider mode byte parity를 검증한다.

## 보안 및 과금 고려사항

- Provider URL은 신뢰 입력으로 간주하지 않으며 HTTPS origin·DNS·dial IP·redirect·크기·MIME를 모두 검증한다.
- S3 credential, signed endpoint, bucket 내부 이름과 object key는 client response/log에 노출하지 않는다. 공개 URL은 고정 CDN base 아래에서만 생성한다.
- inbound Authorization, API Key, cookies와 tracing baggage를 fetch/storage upstream으로 전달하지 않는다.
- Base64는 decoded byte 상한을 먼저 계산하고 streaming decoder로 처리한다.
- Provider 성공 후 storage 실패는 원가를 환불하지 않으며 unknown persistence 결과는 기존 reconciliation reserve를 유지한다.
- terminal idempotency replay는 network/storage side effect를 재실행하지 않는다.
- object metadata에 prompt, customer identity, raw Provider URL과 credential을 기록하지 않는다.

## 테스트 계획

### 단위 테스트

- provider/managed config validation과 secret redaction
- deterministic safe object key와 CDN URL join
- URL scheme/origin/port/userinfo/IP/DNS/redirect 거부
- MIME, magic byte, declared/stream byte limit과 Base64 limit
- OpenAI URL/Base64 다중 결과 및 unknown-field/order 보존
- Gemini inline image 결과와 non-image part 보존
- partial failure, cancel, timeout과 closed body/upload cleanup

### 통합 테스트

- MinIO 또는 S3-compatible mock에 streaming put과 checksum/metadata 검증
- PostgreSQL asset uniqueness와 concurrent idempotency
- Provider mock URL fetch에서 DNS rebinding/redirect/private IP 거부
- Provider 성공→upload→Capture→terminal replay 순서
- storage failure/timeout 후 non-refund와 reconciliation recovery
- managed-mode readiness failure/recovery, provider-mode 완전 비활성

### 호환성 및 장애 테스트

- OpenAI generation, JSON edit와 multipart edit의 managed response parity
- Gemini generateContent image/non-image mixed response parity
- Provider URL 만료 전 복사, truncated body와 slow stream
- storage 403/429/500, connection loss와 ambiguous upload completion
- Gateway 재시작 후 PENDING asset reconciliation
- 응답 write cancellation 후 billing/storage side effect 일관성

### 필수 검증 명령

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
go build ./cmd/gateway-quota
go build ./cmd/gateway-spend-cap
go build ./cmd/gateway-provider-credential
make integration-test
```

## 완료 조건

- [x] provider mode가 기존 native response bytes와 network behavior를 보존함
- [x] managed mode가 OpenAI URL/Base64와 Gemini inline image를 stable CDN URL로 반환함
- [x] URL fetch가 SSRF, DNS rebinding, redirect와 credential forwarding을 방지함
- [x] download/upload가 개별·전체 크기 제한 안에서 streaming으로 동작함
- [x] object key, put, asset row와 retry가 멱등적임
- [x] terminal replay가 Provider fetch/upload를 다시 호출하지 않음
- [x] Provider 성공 후 storage 실패가 잘못 환불되지 않음
- [x] ambiguous persistence가 reconciliation으로 복구됨
- [x] 부분 저장 결과가 client response로 노출되지 않음
- [x] managed required dependency 장애가 readiness를 낮추고 provider mode는 영향받지 않음
- [x] OpenAI 생성·편집과 Gemini native response 계약이 호환 테스트로 고정됨
- [x] storage credential, 내부 bucket/key, raw URL, prompt와 customer identity가 response/log/metadata에 노출되지 않음
- [x] migration forward/backward와 provider-mode rollback이 검증됨
- [x] README, Cloud와 Conformance handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- PR: `https://github.com/nativegatewayhq/gateway/pull/26`
- 계획: `0fa8898`
- S3-compatible store, configuration과 asset migrations: `2d57d3b`
- safe collector, native transformers와 protocol integration: `4067f56`, `9fe8e4e`
- distributed upload lease와 single-PUT integration: `7bac3da`
- storage reconciliation transform와 billing preservation: `987855c`
- readiness process test와 운영 문서: `2ec38d4`
- SSRF reserved-range, lease와 Provider fetch port hardening: `7efe210`, `a5266e6`
- `git diff --check` 통과
- `make check` 통과
- `TEST_DATABASE_URL='postgres://gateway:gateway-local@127.0.0.1:55433/gateway?sslmode=disable' TEST_REDIS_URL='redis://127.0.0.1:56379/0' make integration-test` 통과
- GitHub Plan policy `validate` 통과
- GitHub CI `check` 통과: `https://github.com/nativegatewayhq/gateway/actions/runs/32368052371`

## Rollback 계획

- `GATEWAY_IMAGE_STORAGE_MODE=provider`로 전환해 fetch, upload, transform과 storage readiness dependency를 즉시 우회한다.
- 새 binary는 이미 AVAILABLE인 object를 삭제하지 않으며 Cloud lifecycle policy가 보존/정리를 담당한다.
- migration은 기존 charge/ledger schema를 변경하지 않으므로 이전 binary가 `image_assets`를 무시하고 정상 실행된다.
- RECONCILING charge는 기존 billing worker 계약으로 보존하며 직접 SQL로 환불하거나 asset row를 삭제하지 않는다.

## 후속 작업

- customer/project별 storage mode와 retention policy
- signed/private delivery와 asset deletion API
- storage byte/egress 가격 및 quota
- 영상·음성 asset pipeline 재사용
- multi-region replication과 CDN purge control plane
- OpenTelemetry storage latency/byte/error exporter
