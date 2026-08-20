---
id: gateway-20260820-023
title: Phase 1 Encrypted Provider Credential Control Plane
status: completed
created_at: 2026-08-20T18:58:24+09:00
updated_at: 2026-08-20T19:29:54+09:00
owners:
  - gateway
initiative: phase-1-provider-credential-control-plane
depends_on:
  - gateway-20260820-003
  - gateway-20260820-010
  - gateway-20260820-016
  - gateway-20260820-017
  - gateway-20260820-022
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Encrypted Provider Credential Control Plane

## 목적

Provider channel마다 하나 이상의 upstream credential version을 암호화해 저장하고 무중단으로 stage·activate·retire할 수 있게 한다. 라우팅이 선택한 channel에 고정된 active credential만 outbound 요청 직전에 복호화·주입하며, credential 원문이나 복호화 key가 데이터베이스·로그·오류·감사 이벤트·API에 노출되지 않게 한다.

## 배경

현재 Gateway는 Google, OpenAI, xAI별 환경변수 credential 하나를 시작 시 immutable registry에 적재한다. 그러나 channel, 가격, 지출 cap과 fallback은 이미 channel ID를 기준으로 동작하므로 Provider별 단일 credential은 복수 계약 계정, 사전 rotation, channel별 폐기와 독립 장애 격리를 표현하지 못한다. 배포 재시작에 의존하는 교체는 일부 인스턴스가 이전 secret을 계속 사용할 수 있고, credential과 channel 선택이 분리되어 잘못된 계정에 비용이 발생할 위험도 있다.

## 범위

- Provider channel별 encrypted credential version 저장
- application-level envelope encryption과 master key ID 분리
- staged, active, retired 상태와 원자적 activation
- channel/provider scope가 고정된 opaque credential handle
- 다중 Gateway 인스턴스에서 bounded-delay rotation 반영
- 기존 환경변수 credential의 명시적 bootstrap fallback
- stdin 기반 create/rotate/activate/retire 운영 CLI
- append-only actor/reason audit와 비밀 없는 metadata 조회
- outbound sanitizer 이후 선택 channel credential 하나만 주입
- credential unavailable/decrypt failure의 pre-dispatch fallback
- README와 Cloud secret provisioning handoff

## 제외 범위

- 고객 BYOK와 tenant 소유 credential
- credential 원문 조회, export 또는 복호화 API
- Provider OAuth refresh token과 interactive login
- 외부 KMS/Vault 제품별 adapter의 실제 구현
- 자동 rotation schedule과 Provider key 발급·폐기 API
- credential별 health score, rate limit 또는 spend cap
- Dashboard와 관리형 public admin API
- dispatch 이후 credential 교체 retry

## 설계 및 구현 순서

### 1. 암호화 경계와 key provider

- `MasterKeyProvider`는 key ID로 wrapping key를 조회하고 현재 write key ID를 제공한다.
- 초기 구현은 배포 secret manager가 주입한 32-byte master keyring을 사용하며 key 원문은 일반 `config.Config`, DB와 로그에 전달하지 않는다.
- credential마다 random 256-bit data key와 nonce를 생성해 AES-256-GCM으로 plaintext를 암호화한다.
- data key는 master key로 별도 nonce와 authenticated context를 사용해 wrap한다.
- authenticated context에는 schema version, credential ID, channel ID, provider와 credential version을 포함해 ciphertext row 교체 공격을 막는다.
- 저장 후와 outbound 적용 후 plaintext/data-key buffer는 가능한 범위에서 즉시 overwrite하고 public plaintext getter를 제공하지 않는다.
- unknown master key, authentication tag 실패와 malformed ciphertext는 secret-free typed error로 fail closed한다.

### 2. Credential version과 lifecycle

- credential row는 channel, provider snapshot, version, state, encrypted payload, wrapped data key, key ID와 생성 metadata를 가진다.
- 하나의 channel에는 active version을 최대 하나만 허용한다.
- `stage`는 현재 active를 건드리지 않고 새 version을 검증·암호화한다.
- `activate`는 channel row와 credential versions를 transaction에서 lock하고 대상 staged version을 active로, 이전 active를 retired로 바꾼다.
- retired credential은 신규 dispatch에 사용하지 않지만 감사·과금 증거를 위해 row를 삭제하지 않는다.
- 동일 lifecycle operation key 재시도는 idempotent하고 다른 대상/내용은 conflict다.
- Provider channel의 provider와 credential provider가 일치하지 않으면 저장·activation·resolve 전에 거부한다.

### 3. Channel-scoped resolve와 outbound 적용

- routing decision의 immutable `ChannelID`와 `Provider`를 resolver 입력으로 사용한다.
- resolver는 정확히 하나의 active DB credential을 반환하고 없으면 bootstrap 환경변수 fallback 정책을 평가한다.
- 환경변수 fallback은 built-in legacy channel에만 허용하고 DB active credential이 있으면 항상 DB version이 우선한다.
- opaque handle은 expected channel/provider가 일치할 때에만 sanitized outbound request에 인증을 적용한다.
- request URL, header dump, credential fingerprint·prefix·length와 ciphertext ID를 completion log에 기록하지 않는다.
- billing charge에는 기존 channel ID만 snapshot하며 credential ID/version은 저장하지 않아 secret inventory와 customer financial data를 분리한다.

### 4. Rotation 반영과 동시성

- 각 resolve는 PostgreSQL의 active version을 authoritative source로 사용한다. 최적화 cache를 도입하면 짧은 TTL과 explicit invalidation을 모두 제공한다.
- activation transaction commit 이후 새 resolve는 새 credential만 선택해야 한다.
- commit 전에 이미 resolve된 request는 기존 opaque handle로 단 한 번 dispatch할 수 있으며 activation이 post-dispatch retry를 유발하지 않는다.
- cache가 stale하거나 DB가 unavailable해도 이전 credential을 임의로 사용하지 않고 candidate를 credential-unavailable로 skip한다.
- 다중 process에서 동시 activation 시 channel lock과 lifecycle operation key로 하나만 결정되게 한다.

### 5. 운영·감사 인터페이스

- `gateway-provider-credential` CLI는 stage, activate, retire, list-metadata를 제공한다.
- secret은 bounded stdin으로만 받고 command argument, environment echo, shell history 또는 confirmation output에 포함하지 않는다.
- stage 출력은 credential ID, channel, provider, version과 state만 포함한다.
- audit event는 action, credential ID/version, channel ID, provider, actor, reason, operation key와 timestamp만 append한다.
- list는 plaintext, ciphertext, nonce, wrapped key, key ID, prefix, hash와 length를 반환하지 않는다.
- credential decrypt/apply failure log는 request ID, provider, channel ID와 bounded category만 포함한다.

## 인터페이스와 데이터 변경

### 공개 API

새 client API와 client-visible credential 오류는 없다. 사용할 credential이 없는 candidate는 기존 pre-dispatch fallback에 참여하고 모든 candidate가 실패하면 기존 OpenAI/Gemini provider-unavailable 응답을 반환한다.

### 내부 인터페이스

```go
type CredentialResolver interface {
    Resolve(context.Context, string, ProviderID) (Credential, error)
    ConfiguredChannels(context.Context, []ChannelRef) (map[string]bool, error)
}

type Credential interface {
    Apply(*http.Request, string, ProviderID) error
    Destroy()
}

type MasterKeyProvider interface {
    CurrentID(context.Context) (string, error)
    Key(context.Context, string) (OpaqueMasterKey, error)
}
```

`ConfiguredChannels`는 `/v1/models`와 routing preflight를 위해 secret 없는 availability snapshot만 반환한다. 실제 `Resolve`와 `Apply`가 최종 scope를 다시 검증한다.

### 데이터베이스 및 migration

- `provider_credentials`
- `provider_credential_lifecycle_operations`
- `provider_credential_events`
- active partial unique index on channel ID
- provider/channel ownership FK 또는 trigger validation
- credential ciphertext와 audit/event table은 append-preserving이며 plaintext column은 존재하지 않는다.
- migration은 additive이고 기존 환경변수 기반 legacy channel은 bootstrap fallback으로 유지된다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-1-provider-credential-control-plane` initiative에서 Gateway master keyring을 deployment secret manager로 주입하고 credential 원문을 stdin으로 CLI에 전달한다. Cloud는 key ID와 rotation 순서를 관리하지만 Gateway DB, manifest, Terraform state와 CI log에 master key 또는 Provider credential을 기록하지 않는다. 외부 KMS adapter는 동일 `MasterKeyProvider` 계약의 후속 계획으로 구현한다.

## 보안 및 과금 고려사항

- DB 단독 유출로 Provider credential을 복호화할 수 없어야 한다.
- master key 누락 또는 decrypt 실패는 fail closed하며 plaintext 오류를 wrapping하지 않는다.
- channel/provider scope mismatch는 Provider 호출과 Billing reserve 전에 차단한다.
- preflight와 resolve 사이 rotation race에서 Billing reserve가 이미 시작됐다면 Provider 미호출이 증명되는 credential failure만 release하며 다른 candidate로 post-reserve fallback하지 않는다.
- activation은 spend cap, price, charge와 channel identity를 변경하지 않는다.
- credential lifecycle row는 삭제하지 않아 어떤 channel version이 언제 활성화되었는지 비밀 없는 감사 증거를 유지한다.
- process crash, panic dump와 structured logging에서 opaque type이 plaintext/ciphertext를 formatting하지 않게 한다.

## 테스트 계획

### 단위 테스트

- envelope encrypt/decrypt round trip과 random nonce/data key
- AAD의 credential/channel/provider/version 변조 탐지
- wrong/unknown master key와 malformed ciphertext의 fail-closed error
- opaque formatting/JSON/error/panic path redaction과 buffer destroy
- channel/provider mismatch 및 Provider별 auth header 적용
- stdin limit, control character와 empty credential 검증
- legacy fallback precedence와 non-legacy channel 거부

### 통합 테스트

- migration constraints와 plaintext column 부재
- stage→activate→stage→activate→retire lifecycle 및 append-only audit
- 동시 activation과 idempotent operation replay/conflict
- process restart 후 DB credential decrypt와 dispatch
- 두 channel의 서로 다른 credential이 정확한 mock Provider에 적용됨
- rotation 직전/직후 concurrent resolve가 각 request에 한 credential만 적용함
- decrypt/DB unavailable candidate skip과 다음 channel fallback
- `/v1/models` availability가 active credential lifecycle을 반영함
- CLI stdin secret이 stdout/stderr/process log와 DB audit에 없음

### 호환성 및 장애 테스트

- DB credential이 없는 기존 환경변수 설치 회귀
- master key rotation 중 이전 key read/new key write
- PostgreSQL cancellation, cache invalidation loss와 Gateway 다중 인스턴스
- Provider timeout 이후 credential rotation이 reconciliation 또는 retry를 만들지 않음
- `go test -race`에서 credential handle/cache data race 없음

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

- [x] DB에 Provider credential plaintext column이나 plaintext audit가 없음
- [x] DB 단독 정보로 credential을 복호화할 수 없음
- [x] credential이 channel/provider에 정확히 scope됨
- [x] stage·activate·retire가 원자적이고 감사 가능하며 idempotent함
- [x] 다중 인스턴스 rotation에서 신규 request가 bounded delay 내 새 version을 사용함
- [x] 한 request가 credential 하나로 최대 한 번만 dispatch됨
- [x] credential failure가 pre-dispatch fallback과 과금 불변식을 유지함
- [x] legacy 환경변수 설치가 명시된 channel에서 호환됨
- [x] CLI, log, response, JSON과 오류에 credential·ciphertext·key metadata가 노출되지 않음
- [x] `/v1/models`와 routing availability가 active channel credential을 반영함
- [x] README와 Cloud handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `161664dae11b11c7a6d22ceb1fe13d7c36007c1b`
- PR: `https://github.com/nativegatewayhq/gateway/pull/22`
- 로컬 release gate: `GOCACHE=/private/tmp/nativegateway-go-cache make check` 통과
- 로컬 통합 검증: PostgreSQL `127.0.0.1:55433`, Redis `127.0.0.1:56379`를 사용한 `make integration-test` 통과
- 암호화 경계: random data key/nonce, master-key wrapping, ID/channel/provider/version AAD, malformed·wrong-key·row-scope 변조의 fail-closed 동작과 plaintext column 부재 검증
- lifecycle: stage/activate/retire, stable idempotency replay, conflicting replay, 이전 active 자동 retire audit, immutable ciphertext, append-only operation/event와 동시 activation을 실제 PostgreSQL에서 검증
- rotation: 이전 key read/현재 key write keyring, key 변경 후 lifecycle replay, process 재생성 후 resolve, rotation 동시 resolve가 완전한 구·신 credential 중 하나만 사용하고 commit 이후 즉시 신 version을 사용함을 검증
- Data Plane: OpenAI Images/Edits와 Gemini channel ID 전달, active credential 우선, built-in legacy fallback, DB/decrypt failure fail-closed, `/v1/models` filtering과 pre-reserve fallback 검증
- 과금·보안: post-reserve credential race가 정확히 한 번 release되고 다른 Provider를 호출하지 않음, stdin CLI와 response/log/format/JSON redaction 및 applied header 정리 검증
- GitHub Actions: `check` pass (`32359062681`), `validate` pass (`32359106054`)

## Rollback 계획

- DB active credential을 retire하고 built-in legacy channel의 환경변수 fallback을 복구한다.
- 이전 binary rollback 전 DB credential을 읽지 못하고 환경변수만 사용하게 됨을 운영자가 확인한다.
- credential, lifecycle operation과 audit row는 삭제하지 않는다.
- master keyring은 모든 in-use/retired ciphertext의 보존 기간이 끝나기 전에 제거하지 않는다.
- rotation 도중 시작된 request는 기존 post-dispatch no-fallback 및 reconciliation 규칙으로 종료한다.

## 후속 작업

1. external KMS/Vault `MasterKeyProvider`
2. Provider balance polling과 low-balance alert
3. credential/channel health score와 circuit breaker
4. credential 자동 rotation orchestration
5. 관리형 channel/credential Dashboard
