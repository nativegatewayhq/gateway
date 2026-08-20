---
id: gateway-20260821-035
title: Phase 3 Async Job Management Read API
status: in_progress
created_at: 2026-08-21T00:53:44+09:00
updated_at: 2026-08-21T00:53:44+09:00
owners:
  - gateway
initiative: phase-3-async-job-management-read-api
depends_on:
  - gateway-20260820-002
  - gateway-20260820-008
  - gateway-20260820-012
  - gateway-20260820-028
  - gateway-20260820-029
  - gateway-20260820-030
  - gateway-20260820-031
  - gateway-20260820-032
  - gateway-20260820-033
  - gateway-20260821-034
supersedes: []
affected_repos:
  - gateway
  - dashboard
  - conformance
  - cloud
---

# Phase 3 Async Job Management Read API

## 목적

인증된 프로젝트 사용자가 자신의 Replicate/fal 비동기 Job을 안정적으로 목록·검색·상세 조회하고 상태 전이, usage와 고객 청구 결과를 payload/Provider secret 노출 없이 감사할 수 있는 versioned management read API를 제공한다.

## 배경

Plan 029~034는 durable Job lifecycle, native Provider facade, signed webhook과 usage-aware settlement를 완성했다. 현재 사용자는 native Prediction/Queue ID를 알고 있을 때만 개별 결과를 조회할 수 있으며, 최근 작업 목록, 실패 원인, manual review 여부, 예약·확정 판매액과 상태 전이 이력을 한 화면에서 확인할 계약이 없다.

Phase 3의 남은 원래 범위인 비동기 작업 관리자 화면은 Dashboard 저장소가 구현하지만, Gateway가 먼저 tenant-safe pagination, stable DTO와 감사 event 계약을 소유해야 한다. 이 계획은 read-only data plane을 제공한다. Refund, Adjustment, 강제 상태 변경과 삭제는 별도 권한·감사 설계가 필요하므로 포함하지 않는다.

## 범위

- Gateway-owned versioned management namespace
- 프로젝트/API Key 범위 비동기 Job 목록과 상세 조회
- protocol, terminal/non-terminal status, settlement state, model과 생성 시간 필터
- `(created_at,id)` 기반 deterministic keyset pagination과 opaque cursor
- bounded page size, stable ordering과 snapshot-safe duplicate/skip 정책
- payload-free Job summary, result availability와 failure category
- estimated/actual output usage, reconciliation reason와 고객 판매액 상태
- bounded Job event timeline과 source/category projection
- request/API Key model authorization의 defense-in-depth filtering
- tenant isolation, cursor/query binding과 enumeration-resistant errors
- 관리 화면용 OpenAPI/JSON examples 및 Dashboard/Cloud handoff
- query latency·outcome telemetry와 필요한 PostgreSQL indexes

## 제외 범위

- Provider raw request/response body, prompt, image URL 또는 webhook payload 목록 반환
- Provider Job ID, credential, channel 원가, unit cost, margin과 내부 reservation ID 노출
- 결과 바이너리 다운로드 프록시와 signed object URL 재발급
- Job cancel API 변경; native cancel endpoint는 기존 계약 유지
- manual Capture/Release/Refund/Adjustment와 상태 강제 변경
- 조직 전체 또는 cross-project operator 조회
- retention, archival, hard delete와 legal hold
- 실시간 SSE/WebSocket 상태 feed
- Dashboard UI 자체 구현
- full-text prompt 검색, arbitrary sort와 offset pagination

## 설계 및 구현 순서

### 1. Public management DTO

- namespace는 native Provider route와 분리된 `GET /gateway/v1/jobs`와 `GET /gateway/v1/jobs/{job_id}`를 사용한다.
- summary는 Gateway Job ID, protocol, operation, model, status, settlement state, failure category, result availability, estimated/actual quantity와 created/updated/completed timestamp만 포함한다.
- billing projection은 currency, reserved sale, captured sale와 charge state만 포함하고 estimated Provider cost, actual cost, price/channel/reservation ID는 제외한다.
- detail은 summary와 bounded event timeline을 포함하되 native response body/header와 Provider identity를 반환하지 않는다.
- optional field와 enum은 additive evolution을 허용하고 response schema version을 고정한다.

### 2. Tenant-safe repository query

- 모든 query는 인증 principal의 organization, project와 API Key scope를 SQL predicate에 포함한다.
- API Key는 자신이 생성한 Job만 기본 조회한다. 향후 project-admin credential은 별도 plan 전에는 다른 Key의 Job을 조회하지 않는다.
- model authorization이 제거된 Key는 과거 Job metadata도 기본 목록에서 제외하고 direct detail은 not-found로 응답한다.
- list는 `created_at DESC,id DESC` 고정 순서와 `(created_at,id)<cursor` keyset을 사용한다.
- page size 기본 25, 최대 100이며 response는 items와 optional next cursor를 반환한다.
- event는 Job version 오름차순, 최대 256개로 제한하고 schema invariant상 초과하면 truncated marker를 제공한다.

### 3. Cursor와 filter contract

- cursor는 version, last created timestamp, last Job ID와 canonical filter fingerprint를 담은 base64url payload다.
- deployment의 32-byte cursor secret으로 HMAC-SHA256을 적용하고 원문 secret은 저장·로그하지 않는다.
- cursor는 organization/project/API Key identity와 filter fingerprint에 bind해 다른 tenant, Key 또는 query에서 재사용할 수 없게 한다.
- malformed, unknown version, invalid signature, 미래 timestamp와 길이 초과 cursor는 동일한 invalid-query 400으로 처리한다.
- status/filter 값은 allowlist enum과 exact model ID만 허용하며 SQL identifier나 raw order expression으로 사용하지 않는다.

### 4. Billing과 usage projection

- Job과 optional charge/usage evidence를 단일 bounded read query 또는 명시적 batch query로 조회해 N+1을 방지한다.
- legacy Job은 estimated quantity `1`, actual quantity 미표시와 `legacy` usage mode로 projection한다.
- usage-aware Job은 estimate, verified actual과 reconciliation reason을 반환하되 extractor/provenance internal version은 노출하지 않는다.
- `MANUAL_REVIEW`는 reason enum만 반환하고 raw Provider 오류나 Ledger detail을 포함하지 않는다.
- charge가 없는 BYOK/self-hosted Job은 billing object를 생략한다.
- DB row 조합이 불가능한 상태면 partial DTO를 추측하지 않고 503과 bounded telemetry를 반환한다.

### 5. HTTP authentication와 native-route isolation

- 기존 service API Key extractor/authenticator, network restriction, distributed rate limit을 그대로 적용한다.
- route는 Replicate/fal wildcard보다 먼저 mount하고 Provider SDK가 우연히 호출할 수 있는 native namespace를 사용하지 않는다.
- list/detail 모두 GET만 허용하고 body, unsupported query, duplicate parameter와 content-bearing header를 거부한다.
- 존재하지 않음, 다른 tenant, 다른 API Key와 model authorization 거부는 모두 동일 404 envelope로 응답한다.
- error response에는 cursor 원문, SQL/driver detail, tenant/key ID 또는 filter fingerprint를 포함하지 않는다.

### 6. Index, telemetry와 rollout

- owner keyset query를 위한 `(organization_id,project_id,api_key_id,created_at DESC,id DESC)` index를 additive migration으로 추가한다.
- status/settlement filter가 실제 query plan에서 같은 owner prefix를 유지하도록 partial/composite index를 검토하고 `EXPLAIN` evidence를 기록한다.
- telemetry는 protocol `gateway`, operation `jobs.list|jobs.get`, outcome, bounded filter class와 page-size bucket만 기록한다.
- Job ID, model, cursor, tenant/API Key, failure text와 금액은 metric label/trace event에 넣지 않는다.
- default rollout은 route 활성화이며 기존 native facade wire와 worker behavior를 변경하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

```text
GET /gateway/v1/jobs?protocol=replicate&status=SUCCEEDED&settlement_state=SETTLED&model=owner/model:version&limit=25&cursor=...
GET /gateway/v1/jobs/{gateway_job_id}
```

목록 response 예시:

```json
{
  "object": "list",
  "data": [
    {
      "id": "job_...",
      "protocol": "replicate",
      "operation": "image.generate",
      "model": "owner/model:version",
      "status": "SUCCEEDED",
      "settlement_state": "SETTLED",
      "result_available": true,
      "usage": {"mode":"verified","estimated_quantity":3,"actual_quantity":2,"unit":"image"},
      "billing": {"currency":"USD_MICRO","reserved_sale":300,"captured_sale":200,"state":"CAPTURED"},
      "created_at": "2026-08-21T00:00:00Z",
      "updated_at": "2026-08-21T00:01:00Z"
    }
  ],
  "next_cursor": "..."
}
```

### 내부 인터페이스

- `jobs.ManagementRepository.List`와 `GetDetail` typed query contract
- tenant/model authorization-aware management handler
- cursor codec with injected secret and clock
- public summary/detail/event DTO independent from native Provider snapshots

### 데이터베이스 및 migration

- owner keyset pagination index를 추가한다.
- 기존 async Job, event, usage와 charge row는 수정하지 않는다.
- migration은 additive index-only이며 구 binary와 rolling rollback에 안전하다.
- retention/delete trigger 변경은 하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

- Dashboard는 `phase-3-async-job-management-read-api` initiative에서 목록 filter, detail drawer, usage/billing/manual-review 표시를 구현한다.
- Conformance는 tenant isolation, cursor replay/tamper, pagination stability와 payload/원가 비노출 fixture를 소유한다.
- Cloud는 cursor secret injection/rotation, query latency/error alert와 index rollout을 소유한다.
- Gateway는 JSON schema, error/status contract와 cursor rotation 절차를 versioned handoff로 제공한다.

## 보안 및 과금 고려사항

- management API는 read-only이며 Reserve/Capture/Release/Refund를 생성하지 않는다.
- 모든 SQL은 bound parameter와 tenant triple predicate를 사용하고 cursor/filter 원문을 identifier에 삽입하지 않는다.
- cursor HMAC은 tenant와 canonical filters에 bind하며 active/previous secret 두 개를 지원해 무중단 rotation한다.
- DTO allowlist로 raw snapshot, Provider URL/ID, prompt, credential, 원가와 내부 Ledger identity를 구조적으로 제외한다.
- API Key model 권한과 network/rate-limit 정책을 native request와 동일하게 적용한다.
- pagination 중 새 Job이 생성돼도 첫 page cursor 이전의 고정 keyset만 탐색해 duplicate를 만들지 않는다.
- management read 오류는 실행/정산 상태를 변경하지 않으며 manual-review reservation을 자동 Release하지 않는다.

## 테스트 계획

### 단위 테스트

- filter enum, duplicate parameter, limit와 exact model validation
- cursor encode/decode, tenant/filter binding, tamper, version과 active/previous secret rotation
- summary/detail DTO의 legacy/verified/manual usage와 optional billing projection
- payload/header/Provider ID/원가/secret 비노출 serialization
- route precedence, method/auth/network/rate-limit과 native error envelope 격리

### 통합 테스트

- 실제 PostgreSQL에서 mixed tenant/project/API Key의 list/detail 격리
- created-at tie를 포함한 keyset pagination의 no duplicate/no skip
- protocol/status/settlement/model 조합 filter와 maximum page size
- legacy, partial-success, manual-review, BYOK Job의 usage/billing projection
- event timeline ordering, bound/truncated marker와 immutable source/category projection
- index 존재 및 representative owner query의 `EXPLAIN` plan 확인

### 호환성 및 장애 테스트

- concurrent Job insert/terminal update 중 pagination 안정성
- invalid/tampered/old-secret cursor와 rotation
- DB timeout/connection failure의 secret-safe 503
- existing Replicate/fal SDK routes, signed webhook, worker와 OpenAI/Gemini regression

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

- [ ] 인증된 API Key가 자신의 Job만 deterministic pagination으로 조회함
- [ ] protocol/status/settlement/model filter와 cursor query binding이 검증됨
- [ ] detail이 bounded event timeline과 usage/customer billing projection을 제공함
- [ ] raw snapshot/prompt/URL/Provider identity/credential/원가/margin이 노출되지 않음
- [ ] 다른 tenant/project/API Key/model 권한의 Job이 동일 404로 은닉됨
- [ ] cursor tamper와 active/previous secret rotation이 안전하게 처리됨
- [ ] concurrent insert/update 중 page duplicate/skip 정책이 검증됨
- [ ] owner keyset index와 representative query plan이 검증됨
- [ ] management read가 Wallet/Ledger/Job 상태를 변경하지 않음
- [ ] 기존 native SDK, webhook, worker와 전체 integration test가 회귀하지 않음
- [ ] README/OpenAPI examples와 Dashboard/Cloud/Conformance handoff가 갱신됨
- [ ] commit, PR과 최종 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- management route publication을 끄거나 이전 binary로 rollback한다. native Provider route와 worker는 영향을 받지 않는다.
- additive index는 유지해도 쓰기 동작에 영향을 주지 않으며 필요 시 별도 non-transactional maintenance plan에서 제거한다.
- cursor secret rotation 문제 시 previous secret을 다시 active로 승격하되 로그에 원문을 남기지 않는다.
- rollback 중 Job, event, usage, charge와 Ledger row는 수정하거나 삭제하지 않는다.

## 후속 작업

- Dashboard Job 목록/detail/manual-review UX
- project-admin/organization operator 역할과 cross-key 조회
- manual reconciliation, Refund/Adjustment write workflow와 감사 승인
- terminal Job/result/webhook evidence retention, archival과 legal hold
- live SSE 상태 feed와 aggregate usage/cost analytics
