---
id: gateway-20260820-002
title: Phase 0 Service API Key Authentication
status: accepted
created_at: 2026-08-20T12:47:31+09:00
updated_at: 2026-08-20T12:53:00+09:00
owners:
  - gateway
initiative: phase-0-service-api-key-auth
depends_on:
  - gateway-20260820-001
supersedes: []
affected_repos:
  - gateway
---

# Phase 0 Service API Key Authentication

## 목적

공식 Provider SDK가 전달하는 서로 다른 credential 형식을 하나의 서비스 API Key 인증 경계로 통합하고, Key 원문이 데이터베이스와 로그에 남지 않는 재사용 가능한 Gateway 인증 기반을 만든다.

이 계획이 완료되면 후속 Gemini 및 OpenAI native protocol handler가 동일한 인증 middleware와 인증 주체 정보를 사용할 수 있다.

## 배경

Gateway bootstrap은 HTTP 서버, request ID, 구조화 로그와 redaction의 기본 골격을 제공하지만 아직 유료 Provider endpoint를 보호할 인증 기능이 없다. Gemini, OpenAI, Anthropic 계열 SDK는 서로 다른 header 또는 query parameter에 credential을 전달하므로 Protocol handler마다 인증을 개별 구현하면 동작 차이와 credential 유출 위험이 커진다.

Provider pass-through보다 먼저 서비스 Key와 Provider credential의 경계를 분리해야 한다. 이 단계에서는 서비스 Key 인증만 구현하고, 실제 Provider credential 저장과 upstream 교체는 후속 계획에서 다룬다.

## 범위

- PostgreSQL 연결 설정과 connection pool 생명주기
- 순서가 보장되는 SQL migration 실행 기반
- 서비스 API Key metadata와 단방향 hash 저장 schema
- 고엔트로피 서비스 API Key 생성 및 1회 표시 CLI
- 다음 credential 입력 형식의 공통 추출
  - `Authorization: Bearer SERVICE_KEY`
  - `x-api-key: SERVICE_KEY`
  - `x-goog-api-key: SERVICE_KEY`
  - query parameter `?key=SERVICE_KEY`
- Key hash 조회, 활성 상태 및 만료 검증
- 인증 결과를 request context에 전달하는 HTTP middleware
- 인증 오류의 일관된 Gateway 내부 표현
- header, query, 오류 및 구조화 로그의 credential redaction 강화
- PostgreSQL 통합 테스트와 인증 middleware 테스트
- 운영 및 로컬 개발용 Key 생성·설정 문서

## 제외 범위

- Google, OpenAI, xAI 등 Provider credential 저장
- upstream Provider 호출과 credential 교체
- Gemini 또는 OpenAI native endpoint
- 사용자, 조직, 프로젝트 및 역할 기반 권한 모델
- 모델별 권한, IP allowlist, 사용량 및 비용 한도
- Wallet, Ledger, 가격 계산과 과금
- API Key 조회·폐기용 HTTP 관리 API와 관리자 화면
- Redis rate limit
- Key 원문 복구 또는 재표시

제외된 기능은 Provider credential, native pass-through 및 Phase 1 관리 기능 계획으로 분리한다.

## 설계 및 구현 순서

### 1. PostgreSQL 및 migration 기반

- `GATEWAY_DATABASE_URL` 설정을 추가하고 secret-bearing 설정으로 취급한다.
- 애플리케이션 시작 시 PostgreSQL connection pool을 만들고 종료 시 명시적으로 닫는다.
- readiness가 데이터베이스 연결 상태를 반영하도록 확장한다.
- SQL migration 파일은 저장소에 순서대로 보관하고 적용 이력을 데이터베이스에서 추적한다.
- 동일 migration의 반복 실행은 안전해야 하며, migration 실패 시 Gateway는 요청을 받기 전에 종료한다.
- migration 도구와 실행 방식은 로컬, CI, 배포 환경에서 동일하게 재현 가능해야 한다.

### 2. 서비스 API Key 저장 모델

- Key 원문은 암호학적으로 안전한 난수로 생성한다.
- 사용자에게 전달할 Key는 서비스 식별 prefix와 충분한 entropy를 가진 opaque 문자열로 정의한다.
- 데이터베이스에는 원문 대신 deterministic SHA-256 digest와 표시용 비밀이 아닌 짧은 prefix만 저장한다.
- Key 조회는 전체 테이블 scan이나 plaintext 비교 없이 digest의 unique index로 수행한다.
- 레코드는 최소한 ID, 이름, hash, 표시용 prefix, 상태, 만료 시각, 생성 시각을 가진다.
- 생성 CLI는 성공 시 원문을 정확히 한 번 출력하며, 실패 로그나 구조화 로그에 원문을 포함하지 않는다.
- 비활성 또는 만료된 Key는 인증할 수 없다.

초기 schema에는 아직 존재하지 않는 사용자·프로젝트 foreign key를 만들지 않는다. Phase 1에서 소유권 모델을 추가할 수 있도록 Key ID와 인증 주체를 내부 타입으로 분리한다.

### 3. Credential 추출 규칙

- 지원되는 네 위치에서 credential 후보를 수집한 뒤 하나의 공통 타입으로 정규화한다.
- 한 요청에 둘 이상의 credential 위치가 사용되면 값의 동일 여부와 관계없이 ambiguous credential 오류로 거부한다.
- `Authorization`은 정확한 Bearer scheme만 허용하고 빈 값, 제어 문자, 비정상적으로 긴 값과 중복 header를 거부한다.
- query parameter `key`가 중복되면 거부한다.
- credential 값은 오류 객체, metric label, trace attribute 또는 로그 field에 포함하지 않는다.

### 4. 인증 서비스와 middleware

- HTTP 입력 해석, Key 저장소, hash 계산과 정책 검증을 분리한다.
- 저장소는 context cancellation을 준수하고 데이터베이스 내부 오류와 미인증 결과를 구분한다.
- 유효한 Key는 불변 인증 주체를 request context에 저장한다.
- downstream handler는 원문 Key가 아니라 Key ID와 필요한 최소 metadata만 읽는다.
- `/health/live`와 `/health/ready`는 인증 대상에서 제외한다.
- 후속 Protocol route가 명시적으로 middleware를 적용할 수 있도록 route 조립 지점을 제공한다.

### 5. 오류와 redaction

- credential 누락, 형식 오류, 알려지지 않은 Key, 비활성 Key와 만료 Key는 외부에서 구분되지 않는 `401 Unauthorized`로 응답한다.
- ambiguous credential은 `400 Bad Request`로 처리하되 입력값은 반환하지 않는다.
- 데이터베이스 장애는 credential 실패로 오인하지 않고 민감 정보 없는 `503 Service Unavailable`로 처리한다.
- 인증 응답에는 기존 request ID를 포함한다.
- 현재 access log가 header와 query string을 기록하지 않는다는 조건을 회귀 테스트로 고정한다.
- PostgreSQL DSN, API Key 원문 및 hash는 로그와 오류 문자열에 출력하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

새로운 Provider API endpoint는 추가하지 않는다.

후속 native endpoint에 적용할 인증 입력 계약은 다음과 같다.

```text
Authorization: Bearer <service-key>
x-api-key: <service-key>
x-goog-api-key: <service-key>
?key=<service-key>
```

한 요청에서는 위 위치 중 정확히 하나만 사용할 수 있다. Health endpoint는 인증 없이 유지한다.

### 내부 인터페이스

구체적인 이름은 구현 중 Go 관례에 맞게 조정할 수 있으나 다음 책임 경계는 유지한다.

```go
type APIKeyStore interface {
    FindActiveByDigest(ctx context.Context, digest [32]byte, now time.Time) (Principal, error)
}

type Principal struct {
    APIKeyID string
}
```

- 원문 credential은 추출과 digest 계산 범위를 벗어나 장기 보관하지 않는다.
- `Principal`에는 downstream 동작에 필요한 최소 식별자만 포함한다.

### 데이터베이스 및 migration

최초 migration은 서비스 API Key 테이블과 migration 이력 관리를 위한 구조를 추가한다. 예시 논리 schema는 다음과 같다.

```text
service_api_keys
├─ id
├─ name
├─ key_digest (unique)
├─ key_prefix
├─ status
├─ expires_at (nullable)
├─ created_at
└─ updated_at
```

- migration은 forward-only로 운영한다.
- 배포 rollback 시 기존 migration을 제거하지 않고, 애플리케이션을 이전 버전으로 되돌려도 추가 테이블이 무해하게 남도록 한다.
- hash algorithm 변경을 위해 저장 형식 또는 algorithm version을 확장할 수 있어야 한다.

### 다른 저장소에 제공하거나 요구하는 계약

후속 `conformance` 저장소는 Provider SDK의 credential 위치별로 Gateway 인증 성공과 실패를 검증한다. 이 계획에서는 다른 저장소를 변경하지 않으며, SDK E2E 작업은 별도 계획과 공통 initiative로 연결한다.

## 보안 및 과금 고려사항

- 서비스 API Key 원문은 생성 시 한 번만 표시하고 영구 저장하지 않는다.
- digest와 표시용 prefix도 secret-adjacent 데이터로 취급하여 일반 요청 로그에 남기지 않는다.
- Key 비교와 실패 응답으로 Key 존재 여부를 추측하기 어렵게 동일한 외부 오류를 사용한다.
- 요청 크기 제한 이전에 과도하게 긴 credential을 제한해 메모리 및 hash 남용을 방지한다.
- query credential은 URL에 노출될 위험이 있으므로 Gateway 로그는 raw URL과 query string을 기록하지 않는다.
- 데이터베이스 DSN은 config validation 및 readiness 오류에 포함하지 않는다.
- 이 단계에는 유료 Provider 호출이 없으므로 Reserve, Capture, Release, Refund 이벤트를 만들지 않는다.
- 아직 Idempotency와 과금 처리가 없으므로 인증 성공만으로 사용량 또는 금액을 변경하지 않는다.

## 테스트 계획

### 단위 테스트

- 네 가지 지원 위치에서 credential 추출
- credential 누락과 malformed Bearer 처리
- 중복 header, 중복 query 및 복수 위치 사용 거부
- 길이 제한과 제어 문자 거부
- API Key 생성 형식과 entropy source 오류 처리
- SHA-256 digest의 결정성과 원문 비보존
- 활성, 비활성 및 만료 Key 정책
- 오류 응답에 request ID 포함
- 로그 및 오류 redaction

### 통합 테스트

- PostgreSQL migration 최초 적용과 반복 적용
- API Key 생성 CLI가 hash만 저장하고 원문을 한 번만 반환
- 저장된 digest로 인증 성공
- 잘못된, 비활성 및 만료 Key 인증 실패
- 데이터베이스 장애 시 `503` 및 readiness 실패
- context cancellation과 connection pool 종료
- 동시 인증 요청에서 일관된 결과와 race 부재

### 호환성 및 장애 테스트

- 각 공식 SDK가 사용하는 인증 위치를 재현한 HTTP 요청 검증
- credential을 포함한 요청에서 access log, 오류, panic recovery에 원문이 없는지 검증
- PostgreSQL timeout과 connection loss가 미인증 응답으로 축소되지 않는지 검증
- health endpoint가 데이터베이스 상태에 맞는 readiness를 반환하고 인증은 요구하지 않는지 검증

### 필수 검증 명령

구현 시 Makefile과 CI에서 PostgreSQL 통합 테스트 환경을 동일하게 제공한다.

```text
make fmt-check
go vet ./...
go test -race ./...
go build ./cmd/gateway
go build ./cmd/gateway-key
make integration-test
```

## 완료 조건

- [ ] PostgreSQL 설정, 연결, 종료와 migration이 재현 가능하게 동작함
- [ ] API Key 생성 결과의 원문이 데이터베이스에 저장되지 않음
- [ ] 생성된 Key 원문이 성공 응답 외 로그와 오류에 노출되지 않음
- [ ] 네 가지 인증 형식이 하나의 인증 주체로 정상 처리됨
- [ ] 누락, malformed, unknown, 비활성 및 만료 Key가 안전하게 거부됨
- [ ] 복수 credential 위치와 중복 값이 명시적으로 거부됨
- [ ] 데이터베이스 장애가 민감 정보 없는 `503`과 readiness 실패로 표현됨
- [ ] health endpoint가 인증 없이 유지됨
- [ ] 동시성과 redaction을 포함한 단위·통합 테스트가 통과함
- [ ] formatter, vet, race test, build와 integration test가 CI에서 통과함
- [ ] README에 PostgreSQL 실행, migration 및 개발용 Key 생성 절차가 기록됨
- [ ] 검증 증거가 이 계획에 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 애플리케이션은 이전 bootstrap release로 되돌릴 수 있다.
- 추가된 테이블과 migration 이력은 데이터 손실을 피하기 위해 자동 삭제하지 않는다.
- 아직 Provider route에 과금 요청이 없으므로 rollback 중 금전 정산은 발생하지 않는다.
- Key 원문은 복구할 수 없으므로 인증 기능 재활성화 시 필요하면 새 Key를 발급한다.

## 후속 작업

1. Provider credential 설정 및 보안 경계
2. Gemini `generateContent` native pass-through
3. OpenAI `/v1/images/generations` native pass-through
4. Provider mock server 계약
5. Conformance 저장소의 공식 SDK 인증 호환성 검증
