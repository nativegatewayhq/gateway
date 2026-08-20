---
id: gateway-20260820-012
title: Phase 1 Idempotency-Key and Native Response Replay
status: in_progress
created_at: 2026-08-20T15:33:09+09:00
updated_at: 2026-08-20T15:33:09+09:00
owners:
  - gateway
initiative: phase-1-idempotency-response-replay
depends_on:
  - gateway-20260820-011
supersedes: []
affected_repos:
  - gateway
  - conformance
---

# Phase 1 Idempotency-Key and Native Response Replay

## 목적

OpenAI/xAI 유료 이미지 요청에 organization-scoped `Idempotency-Key`를 지원한다. 동일 key와 동일 wire request의 동시·순차 재시도는 Provider 호출과 과금을 한 번만 수행하고, 완료된 native HTTP status·허용 header·body를 그대로 replay한다. 같은 key를 다른 요청에 사용하면 Provider 호출 전에 명시적으로 거부한다.

## 배경

`gateway-20260820-011`은 Gateway request ID를 사용해 Wallet lifecycle의 중복 금전 효과를 차단하지만, 완료된 request ID 재사용은 409를 반환하고 Provider 응답을 복구하지 못한다. Client가 timeout이나 응답 유실 후 안전하게 재시도하려면 고객이 제어하는 별도 idempotency identity, request fingerprint와 terminal response snapshot이 필요하다.

## 범위

- 선택적 `Idempotency-Key` request header
- organization-scoped key uniqueness
- exact wire request SHA-256 fingerprint
- protocol, operation, model, channel과 content type을 fingerprint에 포함
- in-flight duplicate의 deterministic conflict/pending 응답
- 완료된 성공 및 native Provider 오류 response replay
- executor/Gateway terminal 오류 snapshot과 replay
- response status, allowlisted headers, body와 SHA-256 저장
- response body의 명시적인 저장 상한
- charge settlement와 terminal response snapshot의 동일 transaction commit
- replay 시 인증·tenant active 상태 재검증 후 Provider/Wallet 미호출
- OpenAI JSON generation, xAI JSON edit, OpenAI multipart edit 지원
- SDK에서 header를 전달하지 않는 기존 호출의 one-shot 호환성 유지

## 제외 범위

- billing disabled mode의 전역 HTTP response cache
- GET, streaming, SSE와 비동기 Job replay
- Gemini idempotency
- S3/R2 response snapshot 저장
- TTL/retention과 idempotency key 삭제
- semantic JSON canonicalization: byte가 다르면 다른 fingerprint다
- timeout 이후 Provider-side job/status reconciliation
- 여러 Gateway region 사이 별도 cache invalidation

## 설계 및 구현 순서

### 1. Header 계약

- header 이름은 `Idempotency-Key`다.
- 1..200 byte의 visible ASCII를 허용하고 앞뒤 공백, control과 중복 header를 거부한다.
- billing required mode에서 header는 선택적이다. 생략하면 기존 Gateway request ID one-shot lifecycle을 사용한다.
- key 원문은 로그와 고객 오류 body에 기록하지 않는다.

### 2. Request fingerprint

- SHA-256 입력에는 version marker, protocol, operation, model, channel ID, normalized media type과 exact body bytes를 length-prefix로 결합한다.
- JSON whitespace, field order와 multipart boundary가 다르면 다른 request로 취급한다.
- prompt, URL, image bytes 자체는 DB나 로그에 저장하지 않고 digest만 charge에 저장한다.
- fingerprint 계산 후에도 원래 body bytes를 Provider에 그대로 전달한다.

### 3. Schema

- `image_request_charges`에 nullable idempotency key와 request fingerprint를 추가한다.
- idempotency key가 있으면 fingerprint가 반드시 존재하고 organization/key partial unique index를 적용한다.
- terminal response status, headers JSON, body `bytea`, body SHA-256와 저장 시각을 추가한다.
- CAPTURED/RELEASED terminal charge는 replay snapshot을 가져야 하고 기존 row 호환을 위해 migration 이전 charge는 예외 marker를 둔다.
- snapshot과 fingerprint identity는 append-only update guard에 포함한다.

### 4. Begin과 동시 요청

- Begin은 organization/key advisory lock 후 기존 charge를 조회한다.
- key가 처음이면 가격 견적과 Reserve를 기존 transaction에서 수행한다.
- 같은 key/fingerprint가 RESERVED 또는 RECONCILING이면 `idempotency_in_progress`를 반환하고 Provider를 재호출하지 않는다.
- 같은 key와 다른 fingerprint는 `idempotency_conflict`를 반환한다.
- terminal same fingerprint는 response snapshot을 반환하고 Reserve/Provider를 건너뛴다.

### 5. Response snapshot

- 허용 header는 `Content-Type`, `Retry-After`로 제한하고 canonical JSON으로 저장한다.
- hop-by-hop, cookie, auth와 Provider 내부 header는 저장하거나 replay하지 않는다.
- body는 `GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES` 상한 내에서 읽고 SHA-256을 검증한다.
- 기본 상한은 32 MiB, 허용 최대는 256 MiB다.
- 상한 초과·read 실패는 성공 여부를 임의 결정하지 않고 charge를 RECONCILING으로 표시해 503 fail-closed한다.

### 6. 원자적 terminal commit

- Provider 2xx는 Wallet Capture, charge CAPTURED와 native response snapshot을 같은 transaction에서 commit한다.
- Provider non-2xx와 executor/Gateway terminal 오류는 Release, charge RELEASED와 반환할 error snapshot을 같은 transaction에서 commit한다.
- commit 성공 후에만 최초 caller에게 snapshot을 쓴다.
- response 전달 실패는 terminal snapshot에 영향을 주지 않으며 retry가 replay한다.

### 7. Replay

- replay도 API key authentication과 active organization/project 검증을 먼저 수행한다.
- 저장 body hash가 불일치하면 corruption으로 fail closed하고 RECONCILING 분류 로그를 남긴다.
- status, allowlisted headers와 body bytes를 snapshot에서 복원한다.
- 새로운 Provider credential, price, Wallet balance와 무관하게 원래 terminal 결과를 반환한다.
- replay response에 `Idempotency-Replayed: true`를 추가한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
Idempotency-Key: client-generated-visible-ascii
Idempotency-Replayed: true
```

동일 key에 다른 request는 OpenAI error envelope의 `idempotency_conflict`, 처리 중 재시도는 `idempotency_in_progress`를 반환한다.

### 내부 인터페이스

```go
type BeginRequest struct {
    ...
    IdempotencyKey string
    RequestFingerprint [32]byte
}

type ResponseSnapshot struct {
    Status int
    Headers http.Header
    Body []byte
}

Begin(...) (Charge, Replay, error)
Complete(ctx, chargeID string, success bool, snapshot ResponseSnapshot) (Charge, error)
```

### 데이터베이스 및 migration

forward-only `000006_idempotency_response_replay.sql`. 기존 terminal charge는 `response_snapshot_version=0`, 신규 snapshot은 version 1로 구분한다. 기존 binary는 추가 column을 무시한다.

### 다른 저장소에 제공하거나 요구하는 계약

Conformance는 OpenAI Python/JavaScript와 raw HTTP에서 header 전달, 동시 duplicate, timeout 후 replay, conflict 및 multipart exact-byte fingerprint를 검증한다.

## 보안 및 과금 고려사항

- key scope는 인증된 organization에서만 결정하며 client tenant 식별자를 신뢰하지 않는다.
- key/fingerprint/response body를 로그에 남기지 않는다.
- replay 전에 tenant가 disabled됐으면 인증 단계에서 거부한다.
- response snapshot에는 credential, Set-Cookie와 임의 Provider header가 포함되지 않는다.
- body hash corruption은 Provider 재호출이나 재과금으로 자동 복구하지 않는다.
- snapshot size 제한은 DB 자원 고갈을 막고 초과 시 fail closed한다.

## 테스트 계획

### 단위 테스트

- header 누락/단일/중복과 visible ASCII 검증
- fingerprint field boundary 및 JSON/multipart exact byte 차이
- allowlisted response header canonicalization
- response body size와 hash 검증
- billing error→idempotency HTTP code mapping

### 통합 테스트

- 최초 2xx Capture+snapshot 후 동일 key replay
- native 4xx/5xx Release+snapshot replay
- executor error Release+Gateway error replay
- 동시 duplicate에서 Provider/Reserve/Capture 단일 effect
- 같은 key와 다른 body/model/operation conflict
- organization이 다르면 같은 key 허용
- terminal replay에서 Provider credential/price/Wallet 변경 무관
- snapshot/settlement transaction rollback 원자성
- corrupted snapshot fail-closed
- oversized response RECONCILING

### 호환성 및 프로세스 테스트

- header 없는 기존 official SDK 호출 회귀 없음
- OpenAI Python/JavaScript custom header replay fixture
- xAI JSON과 OpenAI multipart edit replay
- Gateway 재시작 후 PostgreSQL snapshot replay
- migration 반복·동시 실행

### 필수 검증 명령

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
make integration-test
```

## 완료 조건

- [ ] organization-scoped Idempotency-Key와 fingerprint schema가 존재함
- [ ] 동일 key/fingerprint 동시 요청이 Provider와 과금을 한 번만 수행함
- [ ] 동일 key의 다른 request가 Provider 호출 전에 conflict로 거부됨
- [ ] 성공/native 오류/Gateway terminal 오류가 원자적으로 snapshot됨
- [ ] 완료 retry가 status/header/body를 Provider 호출 없이 replay함
- [ ] replay가 active tenant 인증을 다시 요구함
- [ ] response body 크기와 hash corruption이 fail closed됨
- [ ] credential/cookie/비허용 header가 snapshot에 저장되지 않음
- [ ] header 없는 SDK 요청의 기존 동작이 유지됨
- [ ] JSON과 multipart body가 변경 없이 Provider에 전달됨
- [ ] 전체 race/integration/CI 통과
- [ ] README와 Conformance 계약이 기록됨
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 이전 binary로 rollback하되 idempotency column과 response snapshot은 유지한다.
- 신규 key 접수를 중단해도 terminal snapshot row를 삭제하거나 수정하지 않는다.
- snapshot 이상은 Provider 재호출 대신 forward reconciliation 도구로 조사한다.
- body retention 정책이 도입되기 전 production snapshot 삭제를 수행하지 않는다.

## 후속 작업

1. timeout/provider status reconciliation worker
2. Gemini 이미지 price selector와 billing/idempotency
3. response snapshot R2/S3 offload와 retention
4. priority/weighted/lowest-cost routing과 fallback
5. usage·cost 관리 API와 Dashboard
