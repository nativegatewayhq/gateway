---
id: gateway-20260820-003
title: Phase 0 Provider Credential Security Boundary
status: in_progress
created_at: 2026-08-20T13:07:04+09:00
updated_at: 2026-08-20T13:12:55+09:00
owners:
  - gateway
initiative: phase-0-provider-credential-boundary
depends_on:
  - gateway-20260820-002
supersedes: []
affected_repos:
  - gateway
---

# Phase 0 Provider Credential Security Boundary

## 목적

고객이 Gateway에 제출한 서비스 API Key와 Gateway가 upstream에 사용하는 Provider credential을 구조적으로 분리하고, Google·OpenAI·xAI credential을 안전하게 로드하고 올바른 인증 위치에만 주입하는 공통 보안 경계를 만든다.

이 계획이 완료되면 후속 Gemini 및 OpenAI Images adapter는 inbound 요청의 credential을 전달하지 않고, 검증된 Provider ID에 해당하는 Gateway 소유 credential만 outbound 요청에 적용할 수 있다.

## 배경

`gateway-20260820-002`는 서비스 API Key를 hash-only로 저장하고 네 가지 inbound 인증 형식을 하나의 인증 주체로 통합했다. 다음 native pass-through 구현에서 inbound 요청을 그대로 복제하거나 header와 query를 무분별하게 전달하면 서비스 Key가 Provider에 노출되거나 한 Provider의 credential이 다른 Provider로 전송될 수 있다.

Phase 0에서는 Google, OpenAI, xAI별 단일 credential을 배포 환경의 secret injection으로 공급한다. 다중 credential, 암호화 저장, rotation control plane과 channel routing은 Phase 1 이후 별도 계획으로 확장한다. 이번 계획은 그 확장을 막지 않는 최소 인터페이스와 outbound 경계를 확립한다.

## 범위

- Google, OpenAI, xAI를 나타내는 검증된 Provider ID 타입
- 환경 변수로 주입된 Provider credential 로딩 및 시작 전 형식 검증
- credential 원문을 일반 config, 로그, 오류, JSON 또는 debug 문자열로 노출하지 않는 secret 타입
- Provider별 credential 조회를 담당하는 registry 인터페이스
- outbound 요청에서 inbound credential-bearing header와 query를 제거하는 sanitizer
- Provider별 올바른 upstream 인증 적용
  - Google: `x-goog-api-key`
  - OpenAI: `Authorization: Bearer`
  - xAI: `Authorization: Bearer`
- Provider ID와 credential의 scope가 일치하지 않으면 요청 전 실패
- credential 미설정과 잘못된 설정을 민감 정보 없는 typed error로 표현
- credential이 없는 Provider 때문에 health-only Gateway 시작이 실패하지 않는 optional 설정 정책
- 단위 테스트, redaction 회귀 테스트와 후속 adapter가 사용할 내부 계약 문서화

## 제외 범위

- 실제 Google, OpenAI 또는 xAI HTTP 호출
- Gemini `generateContent` 및 OpenAI Images protocol handler
- Provider endpoint URL과 region 선택
- Provider 응답 및 오류 변환
- credential을 PostgreSQL에 저장하거나 API로 관리하는 기능
- KMS, envelope encryption, Vault 또는 cloud secret manager client
- 하나의 Provider에 여러 credential을 등록하는 channel 모델
- credential rotation, health score, 지출 한도와 자동 fallback
- 사용자별 BYOK와 Provider credential 반환·조회 API
- Wallet, Ledger, 가격 계산과 과금

## 설계 및 구현 순서

### 1. Provider ID와 설정 계약

- 문자열을 직접 비교하지 않고 제한된 Provider ID 타입을 사용한다.
- 초기 허용값은 `google`, `openai`, `xai`로 한정한다.
- 알 수 없는 Provider ID는 credential 조회나 outbound 요청 생성 전에 거부한다.
- 배포 환경은 다음 secret 환경 변수를 사용할 수 있다.

| 환경 변수 | Provider | 필수 여부 |
|---|---|---:|
| `GATEWAY_GOOGLE_API_KEY` | Google | 해당 Provider 활성화 시 필수 |
| `GATEWAY_OPENAI_API_KEY` | OpenAI | 해당 Provider 활성화 시 필수 |
| `GATEWAY_XAI_API_KEY` | xAI | 해당 Provider 활성화 시 필수 |

- 아직 Provider adapter가 활성화되지 않았으므로 모든 credential은 optional이다.
- 값이 설정된 경우 공백, 빈 문자열, 제어 문자와 비정상적인 길이를 시작 시 거부한다.
- 설정 오류에는 환경 변수 이름과 실패 분류만 포함하고 원문, prefix 또는 길이를 포함하지 않는다.
- Provider credential은 기존 `config.Config`의 공개 문자열 field에 저장하지 않는다.

### 2. Secret과 registry 경계

- credential 원문을 일반 문자열로 반환하는 public method를 제공하지 않는다.
- secret 타입은 `fmt.Stringer`, JSON/Text marshaling 또는 구조화 로그 attribute를 구현하지 않는다.
- registry는 Provider ID로 scope가 지정된 credential handle만 반환한다.
- registry 초기화 이후 credential map은 불변으로 취급한다.
- credential이 없을 때는 `credential unavailable` typed error를 반환하고 Provider 호출을 시도하지 않는다.
- credential handle은 필요한 outbound 요청에 인증을 적용하는 최소 기능만 노출한다.
- 테스트용 registry는 production 환경 loader와 분리하되 같은 인터페이스를 사용한다.

예상 책임 경계:

```go
type ProviderID string

type Registry interface {
    Credential(provider ProviderID) (Credential, error)
}

type Credential interface {
    Apply(request *http.Request, provider ProviderID) error
}
```

구체 타입과 이름은 구현 시 Go 관례에 맞게 조정할 수 있지만, 원문 getter를 외부 패키지에 공개하지 않고 Provider scope를 적용 시점에 다시 확인하는 조건은 유지한다.

### 3. Outbound credential sanitizer

- 후속 adapter가 만든 outbound 요청은 credential 적용 전에 sanitizer를 통과한다.
- 다음 header는 대소문자와 관계없이 제거한다.
  - `Authorization`
  - `Proxy-Authorization`
  - `x-api-key`
  - `x-goog-api-key`
  - `Cookie`
- 다음 query parameter는 대소문자와 관계없이 제거한다.
  - `key`
  - `api_key`
  - `access_token`
  - `token`
- sanitizer는 입력 요청을 직접 재사용하지 않고 outbound 요청의 독립된 header와 URL 복사본에만 동작해야 한다.
- sanitizer 이후 선택된 Provider credential 하나만 주입한다.
- Google에는 `x-goog-api-key`, OpenAI와 xAI에는 Bearer authorization만 적용한다.
- credential 적용 후에도 URL 문자열이나 request dump를 로그에 출력하지 않는다.

### 4. 애플리케이션 조립

- `main`은 환경에서 credential registry를 초기화하되 원문을 애플리케이션 config나 로그 field로 전달하지 않는다.
- 설정된 Provider 목록만 비밀이 아닌 metadata로 노출할 수 있다. credential prefix, hash 또는 길이는 노출하지 않는다.
- credential이 하나도 없어도 health endpoint와 개발 서버는 시작할 수 있다.
- 후속 adapter가 registry를 명시적 dependency로 주입받을 수 있도록 app 조립 지점을 추가한다.
- readiness는 아직 활성 Provider가 없으므로 credential 존재 여부를 반영하지 않는다. Provider 활성화와 readiness 정책은 각 adapter 계획에서 정의한다.

### 5. 오류와 관측성

- unknown Provider, credential unavailable, malformed credential과 scope mismatch를 구분하는 내부 오류를 제공한다.
- 외부 HTTP 오류 매핑은 후속 Protocol 계획에서 네이티브 오류 형식에 맞춰 정의한다.
- 모든 오류 문자열은 credential 원문을 포함하지 않는다.
- panic, request completion, configuration failure 및 outbound 준비 실패 로그에서 Provider credential과 inbound 서비스 Key가 모두 제거되는지 회귀 테스트한다.
- Provider ID는 낮은 cardinality의 비밀이 아닌 metric/log attribute로 사용할 수 있다.

## 인터페이스와 데이터 변경

### 공개 API

새로운 공개 HTTP endpoint는 추가하지 않는다.

### 내부 인터페이스

- 제한된 Provider ID
- 불변 credential registry
- Provider scope가 지정된 credential handle
- outbound credential sanitizer 및 injector
- 후속 adapter 조립을 위한 registry dependency

후속 protocol/provider 구현은 inbound `http.Request`의 credential-bearing header 또는 query를 upstream 요청에 복사하지 않아야 한다. upstream 요청은 새로운 request로 생성한 뒤 sanitizer와 Provider credential injector를 순서대로 적용한다.

### 데이터베이스 및 migration

없음.

Phase 0 credential은 배포 환경의 secret injection으로 제공한다. 암호화된 Provider credential 저장, channel metadata와 rotation 이력은 Phase 1의 별도 migration 계획에서 다룬다.

### 다른 저장소에 제공하거나 요구하는 계약

없음.

후속 `conformance` 테스트는 Gateway 외부에서 Provider credential을 관찰할 수 없어야 하며, mock upstream을 통해 고객 서비스 Key가 전달되지 않고 Gateway Provider credential만 전달되는지를 검증한다. 해당 작업은 native pass-through initiative의 별도 계획으로 작성한다.

## 보안 및 과금 고려사항

- Provider credential 원문은 프로세스 메모리에는 존재하지만 저장소, 데이터베이스, 로그, trace, metric label과 오류에는 남기지 않는다.
- environment 기반 secret은 프로세스 실행 환경과 배포 플랫폼의 권한으로 보호해야 하며 README 예제에 실제 Key를 기록하지 않는다.
- inbound 서비스 Key와 Provider credential을 동일 타입이나 registry에 저장하지 않는다.
- arbitrary inbound header/query pass-through를 금지하고 제거 대상은 denylist 회귀 테스트로 고정한다.
- redirect를 따르는 HTTP client는 다른 host로 credential을 재전송할 수 있으므로 실제 client 정책은 Provider adapter 계획에서 redirect 금지 또는 동일 trusted origin 제한으로 정의한다.
- 이 계획은 upstream 호출을 수행하지 않으므로 SSRF host allowlist와 DNS 검증은 후속 Provider transport 계획에서 구현한다.
- 유료 요청이 없으므로 Reserve, Capture, Release, Refund 이벤트를 생성하지 않는다.
- credential 설정 성공만으로 Provider 활성화나 과금 가능 상태로 간주하지 않는다.

## 테스트 계획

### 단위 테스트

- 세 Provider ID parsing과 unknown 값 거부
- credential 환경 변수별 로딩 및 Provider scope 매핑
- 미설정 credential의 optional 처리
- 빈 값, 공백, 제어 문자와 길이 초과 credential 거부
- secret 타입의 formatting, JSON 및 오류 경로에 원문이 없는지 검증
- registry의 unknown Provider 및 unavailable credential 처리
- credential scope mismatch 거부
- Google, OpenAI, xAI별 정확한 인증 위치 적용
- 기존 credential-bearing header와 query 제거
- sanitizer가 입력 요청 header와 URL을 변경하지 않는지 검증

### 통합 테스트

- main 조립에서 credential 미설정 상태로 health endpoint 시작
- 각 Provider credential을 설정한 상태에서 registry 초기화
- malformed credential 설정 시 listener bind 전에 안전하게 실패
- mock outbound 요청에서 inbound 서비스 Key가 제거되고 선택된 Provider credential만 존재함을 검증
- request 처리 및 panic 로그에 두 종류의 credential이 없는지 검증

### 호환성 및 장애 테스트

- Gemini가 사용하는 query `key`와 `x-goog-api-key`가 Google outbound 요청에서 Gateway 소유 `x-goog-api-key` 하나로 교체됨
- OpenAI/xAI Bearer service Key가 선택된 Provider의 Bearer credential로 교체됨
- credential 없는 Provider 선택 시 network 요청 없이 실패
- Provider ID와 credential scope가 다르면 network 요청 없이 실패

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

- [x] Google, OpenAI, xAI credential이 제한된 Provider ID로 분리됨
- [x] credential 원문 getter 또는 일반 config 문자열 노출 없이 registry가 구성됨
- [x] credential 미설정 상태에서 Gateway health endpoint가 정상 동작함
- [x] 설정된 malformed credential이 listener 시작 전에 안전하게 거부됨
- [x] inbound 인증 header와 민감 query가 outbound 복사본에서 제거됨
- [x] Google, OpenAI, xAI별 정확한 인증 형식만 적용됨
- [x] Provider scope mismatch와 credential unavailable이 network 호출 전에 실패함
- [x] 입력 요청 객체가 sanitizer에 의해 변경되지 않음
- [x] 로그, 오류, JSON과 debug formatting에 credential 원문이 없음
- [x] 정상·오류·panic·동시성 테스트가 통과함
- [ ] formatter, vet, race test, build와 integration test가 CI에서 통과함
- [x] README에 secret 주입 방식과 로컬 개발 주의사항이 기록됨
- [ ] 검증 증거가 이 계획에 기록됨

## 검증 증거

- 로컬 검증:
  - `make check`: formatter, vet, race test 및 두 binary build 통과
  - `make integration-test`: PostgreSQL, API Key, Gateway process와 Provider credential 설정 경로 통과
  - `git diff --check`: 통과
- 보안 검증:
  - 대소문자가 다른 inbound 인증 header와 민감 query가 outbound 복사본에서 제거됨
  - Google은 `x-goog-api-key`, OpenAI와 xAI는 Provider별 Bearer credential만 적용됨
  - scope mismatch와 credential unavailable이 outbound request 반환 전에 실패함
  - 입력 요청의 header와 URL이 변경되지 않음
  - 일반 formatting, JSON, 설정 오류와 process log에 credential 원문이 포함되지 않음
- commit, pull request와 CI: 아직 게시 전

## Rollback 계획

- credential registry와 outbound sanitizer 조립을 제거하고 이전 인증 완료 release로 되돌린다.
- 데이터베이스 migration이 없으므로 schema rollback은 필요하지 않다.
- rollback 시 Provider 호출 기능도 아직 없으므로 upstream 작업이나 금전 정산은 발생하지 않는다.
- 배포 환경에 주입된 Provider credential은 별도로 폐기하거나 rotation하며 코드 rollback이 secret 삭제를 대신하지 않는다.

## 후속 작업

1. Gemini `generateContent` native pass-through 및 Google transport
2. OpenAI `/v1/images/generations` native pass-through 및 OpenAI/xAI transport
3. Provider mock server 계약
4. Conformance 저장소의 공식 SDK credential 교체 검증
5. Phase 1 encrypted Provider channel storage와 rotation
