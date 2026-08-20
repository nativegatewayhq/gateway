---
id: gateway-20260820-008
title: Phase 1 Tenant Ownership Foundation
status: in_progress
created_at: 2026-08-20T14:32:01+09:00
updated_at: 2026-08-20T14:32:01+09:00
owners:
  - gateway
initiative: phase-1-tenant-ownership
depends_on:
  - gateway-20260820-007
supersedes: []
affected_repos:
  - gateway
  - dashboard
---

# Phase 1 Tenant Ownership Foundation

## 목적

사용자, 조직, 조직 membership과 프로젝트의 영속 도메인을 추가하고 모든 service API Key를 정확히 하나의 프로젝트와 조직에 귀속시킨다. 인증 성공 principal은 API Key ID뿐 아니라 project와 organization ID를 포함해 후속 Wallet, 권한, rate limit의 안전한 tenant 경계가 된다.

## 배경

현재 `service_api_keys`는 소유자 없이 전역으로 존재한다. Phase 1의 Wallet, 사용량, 모델 권한과 비용 한도를 구현하려면 먼저 모든 요청을 안정적인 project/organization에 귀속해야 한다. 기존 개발 Key를 깨뜨리지 않으면서 NOT NULL 소유권으로 migration해야 한다.

## 범위

- users, organizations, organization_memberships, projects schema
- service_api_keys의 NOT NULL project foreign key
- 기존 Key를 deterministic legacy organization/project로 backfill
- API Key principal에 project/organization ID 포함
- API Key 생성 시 project ID 필수 저장
- `gateway-key` CLI의 `-project-id` 옵션과 legacy development 기본값
- active organization/project만 인증 허용
- tenant 삭제 대신 status 기반 비활성화
- migration transaction, foreign key와 tenant lookup integration test
- README의 self-hosted bootstrap tenant 문서

## 제외 범위

- 사용자 로그인, password, OAuth/OIDC와 session
- tenant CRUD HTTP API와 Dashboard UI
- invitation과 membership 관리 workflow
- RBAC 권한 검사
- 사용자별 API Key 직접 소유
- model allowlist, IP 제한과 환경 구분
- Wallet/Ledger, billing account와 결제
- tenant별 rate limit, usage API와 audit log
- hard delete 및 GDPR workflow

## 설계 및 구현 순서

### 1. Tenant schema migration

- 새 migration은 다음 테이블을 만든다.
  - `users`: id, external_subject, email, display_name, status, timestamps
  - `organizations`: id, name, slug, status, timestamps
  - `organization_memberships`: organization_id, user_id, role, status, timestamps
  - `projects`: id, organization_id, name, slug, environment, status, timestamps
- ID는 application-generated opaque text이고 길이·prefix check를 둔다.
- status는 `active|disabled`, membership role은 `owner|admin|member`, environment는 `development|production`으로 제한한다.
- organization slug와 organization 내 project slug는 case-normalized unique다.
- 금전 및 감사 이력 보존을 위해 cascade delete를 사용하지 않는다.

### 2. 기존 Key migration

- deterministic `org_legacy`와 `project_legacy`를 idempotently 생성한다.
- `service_api_keys.project_id`를 nullable로 추가하고 모든 기존 row를 `project_legacy`로 backfill한 뒤 NOT NULL과 FK를 적용한다.
- migration 완료 후 project 없는 Key가 존재할 수 없다.
- 기존 digest, prefix, status, expiry와 ID는 변경하지 않아 기존 plaintext Key가 계속 인증된다.
- rollback은 destructive down migration 대신 application rollback 호환성을 유지한다. 이전 binary는 추가 column을 무시할 수 있다.

### 3. Tenant principal

- `apikey.Principal`은 `APIKeyID`, `ProjectID`, `OrganizationID`를 포함한다.
- 인증 query는 Key→project→organization을 join한다.
- Key, project 또는 organization 중 하나라도 disabled이면 unauthorized다.
- tenant 식별자는 검증된 DB 값만 context와 로그-safe metadata로 사용할 수 있다.
- service Key 원문, digest와 사용자 email은 principal에 포함하지 않는다.

### 4. Key 생성 계약

- `apikey.Record`에 `ProjectID`를 추가하고 store insert에서 필수 사용한다.
- 존재하지 않거나 disabled project에는 Key를 만들 수 없다.
- CLI에 `-project-id`를 추가하고 기본값은 migration이 생성한 `project_legacy`로 두어 기존 self-hosted workflow를 유지한다.
- CLI 출력은 plaintext Key를 한 번만 출력하며 tenant 상세나 DB 오류를 노출하지 않는다.
- 향후 CRUD API가 같은 store 계약을 재사용할 수 있게 project validation error를 typed classification으로 둔다.

### 5. 애플리케이션과 문서

- request handler 공개 응답은 tenant ID를 추가로 노출하지 않는다.
- 인증 로그에도 이 계획에서는 tenant ID를 추가하지 않아 cardinality와 정보 노출을 피한다.
- README에 bootstrap tenant, CLI project option, status disable 효과를 문서화한다.
- Dashboard 저장소에는 동일 initiative로 CRUD/UI 후속 계약을 남긴다.

## 인터페이스와 데이터 변경

### 공개 API

기존 native inference API의 request/response 변경 없음.

CLI:

```text
gateway-key -name NAME [-project-id project_legacy] [-expires-at RFC3339]
```

### 내부 인터페이스

```go
type Principal struct {
    APIKeyID      string
    ProjectID     string
    OrganizationID string
}
```

### 데이터베이스 및 migration

forward-only `000002_tenant_ownership.sql`을 추가한다. migration은 기존 data를 보존하며 이전 binary가 추가 column을 무시할 수 있어 rolling rollback이 가능하다. schema 자체 제거는 금지한다.

### 다른 저장소에 제공하거나 요구하는 계약

Dashboard는 initiative `phase-1-tenant-ownership`에서 향후 tenant CRUD API 계약을 소비한다. 이 계획은 Dashboard 내부 구현을 소유하지 않는다.

## 보안 및 과금 고려사항

- 모든 Key에 NOT NULL project/organization chain을 강제해 cross-tenant orphan을 막는다.
- tenant status는 인증 시 같은 query snapshot에서 검사한다.
- email은 case-insensitive unique normalization을 적용하되 로그에 남기지 않는다.
- foreign key는 cascade delete 없이 감사 가능성을 보존한다.
- 아직 Wallet/Ledger transaction은 만들지 않지만 후속 원장의 organization/project foreign key 기반이 된다.
- tenant ID는 secret은 아니지만 공개 오류에는 포함하지 않는다.

## 테스트 계획

### 단위 테스트

- principal tenant field 전달
- CLI project option parsing과 안전한 오류
- Record validation과 missing project classification

### 통합 테스트

- 빈 DB migration과 반복 migration
- 기존 `000001` Key 생성 후 `000002` backfill 및 동일 plaintext 인증
- 신규 project Key 생성과 tenant principal
- disabled Key/project/organization 인증 거부
- 존재하지 않는 project Key 생성 거부
- foreign key와 unique tenant invariant
- concurrent migration advisory lock 회귀

### 호환성 및 장애 테스트

- 이전 schema에서 forward migration
- migration transaction 중 오류 rollback
- DB unavailable 인증 오류 redaction
- 기존 health와 native protocol 전체 회귀

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

- [ ] users, organizations, memberships와 projects schema가 존재함
- [ ] 모든 service API Key가 NOT NULL project에 귀속됨
- [ ] 기존 Key가 손실 없이 legacy project로 backfill되고 계속 인증됨
- [ ] principal이 project와 organization ID를 포함함
- [ ] disabled Key/project/organization이 인증되지 않음
- [ ] 신규 Key가 존재하는 active project에만 생성됨
- [ ] CLI가 project를 선택하고 plaintext Key를 한 번만 출력함
- [ ] tenant foreign key와 unique invariant가 검증됨
- [ ] migration이 반복·동시 실행에 안전함
- [ ] 공개 오류와 로그에 Key, email 또는 불필요한 tenant 정보가 없음
- [ ] 전체 race/integration/CI 통과
- [ ] README와 Dashboard 후속 계약이 기록됨
- [ ] commit, PR과 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 이전 binary는 추가 tenant column을 무시하고 기존 Key를 계속 읽을 수 있어야 한다.
- tenant schema와 backfill data는 삭제하지 않는다.
- 신규 tenant-aware Key 생성만 중단하고 schema는 forward-fix한다.
- 운영 rollback 전 신규 project에만 존재하는 Key의 접근 영향을 확인한다.

## 후속 작업

1. tenant CRUD management API와 Dashboard
2. organization Wallet과 append-only Ledger
3. project별 model 권한, 한도와 environment
4. tenant audit log와 SSO/OIDC
