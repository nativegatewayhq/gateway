---
id: gateway-20260820-018
title: Phase 1 API Key Distributed Rate Limiting
status: completed
created_at: 2026-08-20T17:11:28+09:00
updated_at: 2026-08-20T17:31:32+09:00
owners:
  - gateway
initiative: phase-1-api-key-rate-limiting
depends_on:
  - gateway-20260820-008
  - gateway-20260820-011
  - gateway-20260820-017
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 API Key Distributed Rate Limiting

## 목적

인증된 서비스 API Key마다 관리자가 설정한 요청률과 burst를 모든 Gateway 인스턴스에서 일관되게 제한한다. 제한 검사는 Provider 선택, 가격 조회, Wallet Reserve보다 먼저 수행하며, 초과 요청에는 native protocol 오류와 표준 rate-limit metadata를 반환한다.

## 배경

현재 API Key는 tenant ownership, 활성 상태와 만료일을 검증하지만 사용량 급증을 제어하지 않는다. 이 상태에서는 유출된 Key나 잘못된 client loop가 Provider 지출, Wallet contention과 DB 부하를 빠르게 증가시킬 수 있다. Phase 1 이미지 API를 운영하려면 단일 프로세스 메모리가 아닌 Redis 기반의 인스턴스 공통 제한과 명시적인 장애 정책이 필요하다.

## 범위

- API Key별 requests-per-minute과 burst 정책 저장
- 인증 결과에 secret-free rate-limit policy 포함
- Redis Lua 기반 atomic token bucket 또는 동등한 단일-key 원자 알고리즘
- OpenAI image generation/edit, Gemini generateContent와 `/v1/models` 적용
- 인증 직후, body spool/parse와 Billing Replay/Quote/Begin 전에 요청 token 소비
- OpenAI/Gemini native 429 envelope
- `Retry-After`, `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` 응답
- Redis 연결, timeout, key namespace와 TTL 설정
- disabled/unlimited policy와 안전한 설정 상한
- Docker Compose Redis와 운영 문서
- 허용/거부/Redis 장애의 secret-free structured metric/log

## 제외 범위

- 비용·token·image quantity 기반 quota
- 조직/프로젝트/model/provider별 복합 제한
- 동시 실행 수 제한과 장시간 Job semaphore
- 사용자별 IP allowlist
- 관리형 Dashboard와 공개 Key 관리 API
- Redis Cluster multi-key transaction
- global Provider spend cap
- 429 이후 다른 Provider fallback

## 설계 및 구현 순서

### 1. Policy 데이터 계약

- `service_api_keys`에 nullable `requests_per_minute`과 `burst`를 추가한다.
- 두 값이 모두 `NULL`이면 제한 없음으로 해석해 기존 Key 동작을 보존한다.
- 제한을 켤 때 RPM과 burst는 함께 필요하며 `1 <= burst <= requests_per_minute` 및 운영 상한을 DB와 애플리케이션에서 검증한다.
- 인증 query가 policy를 `Principal`에 포함해 request path에서 추가 PostgreSQL 조회를 만들지 않게 한다.
- `gateway-key` 생성 명령에 opt-in flag를 추가하고 출력에는 raw Key 외 정책 요약만 표시한다.

### 2. Redis rate limiter

- limiter 입력은 API Key ID, RPM, burst이며 raw Key/digest/prompt를 받지 않는다.
- Redis key는 versioned namespace와 API Key ID로 구성하고 TTL을 idle bucket 정리에 사용한다.
- Redis server time과 Lua script 하나로 refill, consume, remaining, retry/reset을 원자 계산한다.
- 숫자 overflow, clock regression과 malformed stored state는 허용으로 우회하지 않고 typed error를 반환한다.
- Redis operation에는 짧은 전용 timeout을 사용하고 request cancellation을 존중한다.

### 3. Middleware와 protocol response

- 인증 성공 직후 limiter를 한 번 호출하고 Principal을 context에 유지해 handler별 재인증/중복 token 소비를 방지한다.
- auth failure와 method/path validation처럼 token을 소비하지 않아야 할 경계를 명시하고 middleware 순서를 테스트한다.
- 제한 초과는 OpenAI `rate_limit_error`/`rate_limit_exceeded`, Gemini `RESOURCE_EXHAUSTED` envelope로 매핑한다.
- 허용 및 거부 응답에 정수 기반 rate-limit headers를 설정하고 내부 Redis key나 policy DB ID는 노출하지 않는다.
- `/healthz`는 Redis와 무관하게 liveness를 유지하고 `/readyz`는 rate limiting required mode에서 Redis 상태를 반영한다.

### 4. 장애 정책과 설정

- 관리형/required mode는 Redis 오류와 timeout에서 fail closed `503`으로 처리해 Provider·Wallet을 보호한다.
- self-hosted optional mode는 limiter 자체를 명시적으로 비활성화할 수 있지만, 활성화된 limiter가 장애일 때 암묵적으로 fail open하지 않는다.
- Redis URL과 timeout 설정을 validation/redaction 대상에 추가한다.
- startup에서 required mode인데 Redis 설정 또는 ping이 실패하면 준비 완료 상태가 되지 않는다.

### 5. 관측성과 운영

- metric은 protocol, operation, outcome(`allowed`, `limited`, `unavailable`)만 사용해 cardinality를 제한한다.
- log에는 request ID, API Key ID, project ID, outcome, retry-after만 기록하며 raw Key와 Redis script/state는 제외한다.
- Compose에 Redis healthcheck를 추가하고 로컬 실행 및 정책 생성 예제를 문서화한다.
- Cloud에는 Redis 연결과 API Key policy provisioning 계약을 handoff한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 endpoint만 사용한다. 제한 응답에 다음 headers가 추가된다.

```text
Retry-After
X-RateLimit-Limit
X-RateLimit-Remaining
X-RateLimit-Reset
```

### 내부 인터페이스

```go
type RateLimitPolicy struct {
    RequestsPerMinute int64
    Burst             int64
}

type RateLimiter interface {
    Allow(ctx context.Context, apiKeyID string, policy RateLimitPolicy) (Decision, error)
}
```

`Decision`은 allowed, limit, remaining, reset time과 retry-after를 포함하며 credential이나 client 입력 body를 포함하지 않는다.

### 데이터베이스 및 migration

- `service_api_keys.requests_per_minute BIGINT NULL`
- `service_api_keys.burst BIGINT NULL`
- 두 값의 동시 NULL/동시 non-NULL, 양수, 상호 범위를 보장하는 CHECK constraint
- 기존 row는 NULL이므로 migration 직후 동작이 바뀌지 않는다.
- 이전 binary는 추가 nullable column을 무시할 수 있어 rolling deploy가 가능하다.
- rollback은 enforcement를 비활성화한 뒤 column/constraint를 후속 migration에서 제거한다.

Redis bucket은 파생된 일시 상태이며 영구 migration이나 복구 대상이 아니다. 정책 변경 시 version/policy fingerprint가 달라진 bucket namespace를 사용해 이전 state와 섞이지 않게 한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-1-api-key-rate-limiting` initiative로 managed Redis, secret injection, required-mode 설정과 API Key policy provisioning을 제공해야 한다. Gateway는 공개 Key 관리 API가 생기기 전까지 DB/CLI 생성 계약과 readiness 신호를 제공한다.

## 보안 및 과금 고려사항

- Redis 식별자에는 raw API Key와 digest를 사용하지 않고 비밀이 아닌 Key ID만 사용한다.
- limiter는 Quote, Reserve와 Provider 호출 전에 실행되어 거부 요청의 금전 및 upstream effect가 없어야 한다.
- 제한된 요청은 idempotency charge/replay lookup에도 도달하지 않는다. 이는 Key별 요청률 방어를 우선하며 replay도 요청 token 하나를 소비한다는 명시적 정책이다.
- Redis 장애에서 fail closed해 무제한 Provider 지출을 방지한다.
- client가 rate-limit headers나 API Key policy를 요청 값으로 덮어쓸 수 없다.
- 429는 candidate-specific failure가 아니므로 routing fallback을 시작하지 않는다.

## 테스트 계획

### 단위 테스트

- policy validation, unlimited/limited Principal mapping
- deterministic refill/consume, burst, TTL, retry/reset rounding
- native OpenAI/Gemini 429 및 503 envelope와 headers
- auth 실패·잘못된 method/path의 token 미소비
- generation/edit/Gemini/models 요청당 정확히 한 token 소비
- rate-limit 거부 시 parser/spool/Billing/executor 미호출
- config error redaction과 metric label cardinality

### 통합 테스트

- PostgreSQL migration과 기존 Key backward compatibility
- `gateway-key` policy flag 생성 및 인증 결과
- 실제 Redis에서 다중 goroutine/다중 limiter instance atomic limit
- bucket TTL과 policy 변경 namespace 격리
- Redis timeout/connection loss required-mode fail closed
- 제한 요청에서 Wallet/Ledger/charge/upstream effect 없음
- 허용 요청의 기존 idempotency, fallback과 reconciliation 회귀

### 호환성 및 장애 테스트

- OpenAI/Gemini native SDK가 429 body와 headers를 정상 수신
- Gateway 재시작 후 Redis bucket state 유지
- Redis 재시작 시 fail-closed 구간 후 정상 복구
- PostgreSQL은 정상이나 Redis만 장애인 readiness 동작
- `go test -race`에서 limiter와 policy cache data race 없음

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

- [x] 기존 Key는 migration 후 제한 없이 동일하게 동작함
- [x] 정책이 있는 Key는 모든 인스턴스에서 atomic RPM/burst 제한을 공유함
- [x] 요청당 token 소비 경계와 middleware 순서가 고정됨
- [x] 429 native envelope와 rate-limit headers가 protocol별로 정확함
- [x] 거부 요청은 body spool, Billing, Wallet, Ledger와 Provider effect가 없음
- [x] Redis 장애 required mode가 fail closed하고 readiness에 반영됨
- [x] raw API Key와 고카디널리티 식별자가 Redis/log/metric에 노출되지 않음
- [x] 기존 idempotency, fallback, reconciliation 불변 조건이 유지됨
- [x] Compose와 Cloud handoff 문서가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현: `cd00c79` (`feat: add distributed API key rate limiting`)
- Pull Request: https://github.com/nativegatewayhq/gateway/pull/17
- 로컬 검증: `make check` 통과
- PostgreSQL·Redis 통합 검증: `make integration-test` 통과
- process 검증: 저장된 Key 정책 429, Gateway 재시작 간 Redis bucket 유지, Redis 장애 시 live 200/ready 503 통과
- Redis 검증: 다중 instance atomic burst, TTL, policy namespace 격리, malformed/future state fail-closed 통과
- GitHub Actions: `check` 및 `validate` 통과
- Cloud handoff: managed deployment는 Redis 8 endpoint를 secret으로 주입하고 `GATEWAY_RATE_LIMIT_MODE=required`를 설정하며 Key 생성 시 RPM/burst를 함께 provision해야 한다.

## Rollback 계획

- enforcement 설정을 비활성화해 기존 unlimited 동작으로 되돌린다.
- nullable policy column은 이전 binary와 호환되므로 즉시 제거하지 않는다.
- Redis bucket은 파생 상태이므로 삭제하지 않아도 TTL로 만료되며, 필요하면 해당 version namespace만 제거한다.
- 이미 거부된 요청은 금전 row가 없으므로 compensation이 필요하지 않다.

## 후속 작업

1. 프로젝트/조직/model별 hierarchical quota
2. 비용 및 Provider spend cap
3. 동시 image/video Job semaphore
4. 관리형 Dashboard와 Key policy API
5. IP allowlist와 감사 로그
