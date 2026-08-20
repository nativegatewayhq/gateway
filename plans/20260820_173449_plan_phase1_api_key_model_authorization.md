---
id: gateway-20260820-019
title: Phase 1 API Key Model Authorization
status: completed
created_at: 2026-08-20T17:34:49+09:00
updated_at: 2026-08-20T17:49:14+09:00
owners:
  - gateway
initiative: phase-1-api-key-authorization
depends_on:
  - gateway-20260820-007
  - gateway-20260820-008
  - gateway-20260820-018
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 API Key Model Authorization

## 목적

서비스 API Key에 logical `protocol + operation + model` allowlist를 설정하고 모든 Gemini/OpenAI image 경로에서 현재 권한을 fail closed로 집행한다. 권한 없는 모델은 Provider candidate, 가격, Wallet, idempotency replay와 upstream에 도달하지 않으며 `/v1/models`에도 노출되지 않는다.

## 배경

현재 활성 API Key는 프로젝트의 모든 Gateway 모델을 사용할 수 있다. 프로젝트가 개발·운영 Key를 나누거나 특정 고객에게 일부 모델만 제공하려 해도 Provider 비용과 데이터 정책 경계를 강제할 수 없다. capability registry와 logical routing이 안정화되었으므로 Provider-native 이름이 아니라 client-visible logical identity에 권한을 결합해야 한다.

## 범위

- API Key model access mode `all` 또는 `allowlist`
- normalized API Key permission row: protocol, operation, logical model
- 기존 Key와 기본 CLI 생성은 `all`로 backward compatible
- `gateway-key` 반복 가능 permission flag와 원자적 Key/permission 생성
- 인증 Principal에 immutable permission snapshot 포함
- OpenAI image generation/edit, Gemini generateContent 권한 검사
- `/v1/models` credential/capability 결과와 Key permission 교집합
- 권한 거부 native protocol envelope
- 권한 검사를 candidate/Quote/Begin/idempotency replay/Provider보다 먼저 수행
- rate limiting과 권한 검사의 고정 순서
- secret-free authorization logs와 bounded permission validation

## 제외 범위

- IP/CIDR allowlist와 trusted proxy 해석
- 조직·프로젝트 role 및 사용자 Dashboard 인증
- Provider/channel 직접 권한
- wildcard, prefix, regex, deny rule과 조건식
- 요청량·비용·기간 quota
- 동적 관리 REST API와 정책 cache invalidation
- LLM tool/feature-level 권한

## 설계 및 구현 순서

### 1. Permission 데이터 모델

- `service_api_keys.model_access_mode`은 `all` 기본값과 `allowlist`만 허용한다.
- `service_api_key_model_permissions`는 Key FK와 canonical protocol/operation/model을 복합 PK로 저장한다.
- 허용 protocol/operation 조합은 현재 image operation registry와 동일하게 검증한다.
- model은 trim된 1–200 byte logical identity이며 wildcard 문자를 특별 취급하지 않는다.
- Key 삭제 시 permission은 cascade하고 permission row는 credential/digest를 포함하지 않는다.

### 2. 생성 및 인증 snapshot

- Key domain record에 access mode와 중복 제거·정렬된 permission 목록을 추가한다.
- CLI는 `--allow-model protocol:operation:model`을 반복 입력받는다. 하나 이상이면 allowlist, 없으면 all이다.
- Key row와 permission rows는 한 PostgreSQL transaction에서 생성해 부분 정책 Key가 활성화되지 않게 한다.
- 인증 query는 permissions를 함께 읽어 Principal에 immutable snapshot으로 반환하며 request마다 추가 DB query를 만들지 않는다.
- 비정상 mode, 빈 allowlist와 DB-corrupt permission은 인증 unavailable로 fail closed한다.

### 3. Authorization boundary

- API Key 인증과 rate-limit token 소비 후 logical model이 검증되는 즉시 `Authorize(protocol, operation, model)`을 호출한다.
- Gemini는 path model, OpenAI JSON은 parsed body model, multipart edit은 bounded spool에서 읽은 logical model을 사용한다.
- Provider model rewrite와 candidate enumeration 전에 검사하며 provider-native 이름으로 재검사하지 않는다.
- allowlist 거부는 OpenAI 403 `permission_error`와 Gemini 403 `PERMISSION_DENIED`로 반환한다.
- rate-limit token은 권한 거부 요청에도 하나 소비해 유출 Key의 model probing을 제한한다.

### 4. Idempotency와 models list

- 현재 Key 권한을 terminal replay lookup보다 먼저 검사한다. 정책에서 모델이 제거되면 과거 성공 snapshot도 반환하지 않는다.
- 권한 변경은 기존 charge/ledger/response snapshot을 삭제하거나 수정하지 않는다.
- `/v1/models`는 capability와 configured Provider가 있어도 Principal이 generation 또는 edit capability 어느 것도 허용하지 않으면 모델을 제외한다.
- 동일 logical 모델의 capability별 권한이 다르면 허용된 capability가 하나 이상일 때 모델을 한 번만 표시한다.

### 5. 관측성과 문서

- 로그는 request ID, API Key ID, project ID, protocol, operation, logical model과 `denied` category만 포함한다.
- raw Key, digest, prompt, multipart filename과 Provider model은 authorization log에서 제외한다.
- CLI 예제, default-all 위험, 최소 권한 운영 방법과 Cloud provisioning handoff를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

새 endpoint는 없다. 기존 경로에 native 403 응답이 추가된다.

### 내부 인터페이스

```go
type ModelPermission struct {
    Protocol  string
    Operation string
    Model     string
}

func (principal Principal) AuthorizeModel(protocol, operation, model string) bool
```

### 데이터베이스 및 migration

- `service_api_keys.model_access_mode text NOT NULL DEFAULT 'all' CHECK (...)`
- `service_api_key_model_permissions(api_key_id, protocol, operation, model, created_at)`
- 복합 PK와 `ON DELETE CASCADE` FK
- 기존 row는 `all`로 backfill되어 동작이 바뀌지 않는다.
- 새 binary를 먼저 배포한 뒤 allowlist Key를 생성한다. 이전 binary는 mode를 집행하지 않으므로 allowlist 생성 이후 binary rollback은 보안상 금지한다.
- rollback이 필요하면 모든 allowlist Key를 disable한 뒤 이전 binary로 전환한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-1-api-key-authorization` initiative로 Key 생성 UI/API에서 canonical logical permission을 전달하고 allowlist Key가 존재할 때 구버전 Gateway로 rollback하지 않아야 한다. Dashboard는 Provider-native model이 아닌 Gateway registry의 logical model/capability를 선택지로 사용한다.

## 보안 및 과금 고려사항

- 권한은 현재 Key 기준이며 동일 프로젝트의 다른 Key 권한과 합치지 않는다.
- denial은 Quote, Reserve, charge/ledger, replay와 Provider 전에 발생한다.
- 권한 row는 client request로 수정할 수 없고 CLI/control-plane provisioning만 신뢰한다.
- rate limiter가 먼저 실행되어 denied model probing도 제한된다.
- model authorization failure는 candidate-specific 상태가 아니므로 fallback하지 않는다.
- permission 변경은 진행 중인 이미 Reserve된 요청을 취소하지 않으며 새 요청과 replay부터 적용한다.

## 테스트 계획

### 단위 테스트

- permission parse/canonicalize/deduplicate/sort와 상한
- all/allowlist authorization matrix
- protocol/operation/model exact match와 wildcard 미지원
- OpenAI JSON, multipart와 Gemini native 403
- denial 시 candidate/Billing/replay/executor 미호출
- denied request도 rate-limit token을 정확히 하나 소비
- `/v1/models` capability별 교집합과 stable ordering
- corrupt/empty allowlist fail-closed 및 log redaction

### 통합 테스트

- migration existing Key default-all
- Key와 permission 원자 생성 및 cascade delete
- CLI repeated flag round trip과 plaintext 비저장
- generation/edit/Gemini allow/deny with PostgreSQL Principal
- terminal replay 생성 후 permission 제거 시 403과 financial/upstream 무변경
- concurrent authentication의 immutable permission snapshot

### 호환성 및 장애 테스트

- 기존 unlimited/all Key의 SDK 동작 회귀
- unknown logical model은 기존 404/invalid-model 의미를 유지하고 권한 존재 여부를 누설하지 않도록 오류 순서를 고정
- database connection loss는 authorization bypass 없이 authentication unavailable
- Gateway restart 후 DB permission 즉시 반영
- `go test -race`에서 permission slice mutation/data race 없음

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

- [x] 기존 Key가 default-all로 동일하게 동작함
- [x] allowlist Key가 exact logical protocol/operation/model만 허용함
- [x] Key와 permission 생성이 원자적이고 삭제 cascade가 검증됨
- [x] generation/edit/Gemini denial이 native 403으로 반환됨
- [x] denial은 candidate, replay, Billing, Wallet/Ledger와 Provider effect가 없음
- [x] `/v1/models`가 현재 Key permission과 dispatch availability 교집합만 표시함
- [x] rate limit 후 authorization 순서와 denied token 소비가 고정됨
- [x] raw credential과 Provider model이 authorization 관측성에 노출되지 않음
- [x] 기존 idempotency/fallback/reconciliation 불변 조건이 유지됨
- [x] README와 Cloud handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현: `86fecee` (`feat: enforce API key model authorization`)
- Pull Request: https://github.com/nativegatewayhq/gateway/pull/18
- 로컬 검증: `make check` 통과
- PostgreSQL·Redis 통합 검증: `make integration-test` 통과
- migration/정책 검증: 기존 Key default-all, Key/permission 원자 생성, cascade, corrupt empty allowlist fail-closed 통과
- protocol 검증: OpenAI generation/edit, Gemini 403와 `/v1/models` permission 교집합 통과
- replay 검증: 성공 snapshot 생성 후 permission 철회 시 replay/Provider/금전 변경 없이 403 통과
- GitHub Actions: `check` 및 `validate` 통과
- Cloud handoff: control plane은 Provider-native 이름이 아닌 canonical logical protocol/operation/model을 전달하고 allowlist Key가 존재하면 구버전 Gateway로 rollback하지 않는다.

## Rollback 계획

- allowlist Key를 모두 disable한 후 이전 binary로 rollback한다.
- 새 binary 내 긴급 rollback은 authorization wrapper를 제거하지 않고 affected permission만 `all`로 명시 변경한다.
- permission table과 mode column은 nullable이 아닌 additive schema로 남겨 data 손실을 피한다.
- denial에는 금전 row가 없으므로 compensation이 필요하지 않다.

## 후속 작업

1. trusted proxy 기반 API Key IP/CIDR allowlist
2. organization/project/model 비용 quota
3. Key 관리 REST API와 audit log
4. Dashboard 권한 편집 UI
5. LLM capability/tool authorization
