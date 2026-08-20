---
id: gateway-20260820-020
title: Phase 1 API Key Network Restrictions
status: completed
created_at: 2026-08-20T17:52:31+09:00
updated_at: 2026-08-20T19:02:00+09:00
owners:
  - gateway
initiative: phase-1-api-key-authorization
depends_on:
  - gateway-20260820-018
  - gateway-20260820-019
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 API Key Network Restrictions

## 목적

서비스 API Key에 IPv4/IPv6 CIDR allowlist를 설정하고, direct peer 또는 명시적으로 신뢰한 reverse proxy chain에서 도출한 client IP를 기준으로 모든 native API 요청을 fail closed로 제한한다. 위조된 forwarding header는 권한 우회에 사용할 수 없어야 한다.

## 배경

API Key는 bearer secret이므로 유출 시 모델 allowlist와 rate limit 범위 안에서 다른 네트워크에서도 사용할 수 있다. 관리형 Gateway는 CDN/load balancer 뒤에서 실행되지만 현재 request source identity 계약이 없다. 무조건 `X-Forwarded-For`나 vendor header를 신뢰하면 client가 allowlisted IP를 직접 위조할 수 있으므로 Key 정책과 함께 trusted proxy 경계를 먼저 확립해야 한다.

## 범위

- API Key network access mode `all` 또는 `allowlist`
- normalized IPv4/IPv6 prefix 저장과 canonicalization
- 기존 Key와 기본 CLI는 network `all`로 backward compatible
- 반복 가능한 `gateway-key --allow-cidr`와 Key/prefix 원자 생성
- direct peer IP의 strict parsing
- operator-configured trusted proxy CIDR 목록
- trusted proxy에서만 RFC 7239 `Forwarded` 또는 `X-Forwarded-For` chain 해석
- right-to-left trusted hop 제거 후 최초 untrusted client 선택
- 인증 후 rate-limit token 소비 전 network authorization
- OpenAI/Gemini native 403과 `/v1/models` 포함 전 경로 적용
- liveness/readiness와 Provider outbound에는 client IP 정책 미적용
- secret-free network denial logs와 README/Cloud handoff

## 제외 범위

- Cloudflare/AWS/GCP 공개 proxy range 자동 다운로드
- GeoIP, ASN, 국가와 VPN 탐지
- hostname/DNS 기반 allowlist
- denylist, port, protocol과 time-of-day 규칙
- per-model CIDR 정책
- PROXY protocol listener
- Key 관리 REST API와 Dashboard UI

## 설계 및 구현 순서

### 1. CIDR 정책 데이터 모델

- `service_api_keys.network_access_mode`은 `all` 기본값과 `allowlist`만 허용한다.
- `service_api_key_network_prefixes`는 PostgreSQL `cidr`과 canonical text를 Key FK 아래 저장한다.
- IPv4-mapped IPv6는 canonical IPv4로 정규화하고 host bits는 prefix network로 mask한다.
- 중복/포함 관계를 제거해 bounded immutable prefix snapshot을 만든다.
- Key와 prefix rows는 한 transaction에서 생성하며 Key 삭제 시 cascade한다.

### 2. Client IP resolver

- request `RemoteAddr`의 host/port를 strict parse하며 Unix/빈/비정상 peer는 typed unavailable로 처리한다.
- direct peer가 trusted proxy set에 없으면 forwarding headers를 전부 무시하고 peer를 client IP로 사용한다.
- trusted peer일 때 `Forwarded`와 `X-Forwarded-For` 중 하나만 허용하고 둘 다 있으면 ambiguous error로 fail closed한다.
- chain을 right-to-left로 검사해 trusted proxy hop을 제거하고 첫 untrusted 주소를 client로 선택한다.
- header element 수, 전체 bytes와 trusted hops를 제한해 parsing DoS를 방지한다.
- obfuscated/unknown `Forwarded for=` identifier, zone ID, port ambiguity와 invalid IP는 fail closed한다.

### 3. Request context와 enforcement 순서

- request ID 이후, protocol handler 이전 middleware가 resolved client IP를 context에 저장한다.
- health routes는 resolver 실패와 무관하게 운영 가능해야 하므로 API route에만 적용하거나 health에서 결과를 사용하지 않는다.
- 인증된 Principal의 network policy를 client IP에 적용한 후 rate limiter를 호출한다.
- network denial은 rate-limit token을 소비하지 않으며 model parse, replay, Billing과 Provider에 도달하지 않는다.
- resolver unavailable/ambiguous는 restricted Key에서만 403으로 fail closed하고 unrestricted Key는 direct peer 결과로 정상 동작한다.

### 4. Native response와 관측성

- OpenAI는 403 `permission_error`/`network_not_allowed`, Gemini는 403 `PERMISSION_DENIED`를 반환한다.
- response는 allowlist prefix, trusted proxy topology와 forwarding header 원문을 노출하지 않는다.
- denial log는 request ID, API Key ID, project ID, normalized client IP와 category만 포함한다.
- raw Key, full forwarding chain, Provider credential과 request body는 기록하지 않는다.

### 5. 설정과 운영

- `GATEWAY_TRUSTED_PROXY_CIDRS`는 comma-separated canonical CIDR이며 unset이면 어떤 forwarding header도 신뢰하지 않는다.
- invalid/overlapping/excessive 설정은 값 자체를 로그하지 않고 startup config error로 종료한다.
- Cloud는 실제 ingress egress address만 trusted proxy set으로 주입하고 public CDN range 변경을 별도 IaC에서 관리한다.
- 로컬 direct mode, 단일 proxy와 multi-hop 예제를 문서화한다.

## 인터페이스와 데이터 변경

### 공개 API

새 endpoint는 없다. restricted Key에 native 403 `network_not_allowed` 동작이 추가된다.

### 내부 인터페이스

```go
type ClientIPResolver interface {
    Resolve(*http.Request) (netip.Addr, error)
}

type NetworkPolicy struct {
    Mode     AccessMode
    Prefixes []netip.Prefix
}

func (principal Principal) AuthorizeNetwork(clientIP netip.Addr) bool
```

### 데이터베이스 및 migration

- `service_api_keys.network_access_mode text NOT NULL DEFAULT 'all' CHECK (...)`
- `service_api_key_network_prefixes(api_key_id, prefix cidr, created_at)`
- `(api_key_id, prefix)` PK와 `ON DELETE CASCADE`
- 기존 Key는 `all`로 backfill된다.
- allowlist Key 생성 후 이전 binary rollback은 네트워크 제한을 우회하므로 해당 Key를 먼저 disable해야 한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-1-api-key-authorization` initiative로 ingress topology의 실제 trusted proxy CIDR만 Gateway secret/config에 제공하고 Key CIDR 정책을 canonical prefix로 provision한다. Proxy range 자동 갱신과 deployment rollout은 Cloud 소유이며 Gateway는 외부 range를 다운로드하지 않는다.

## 보안 및 과금 고려사항

- untrusted peer의 forwarding header는 절대 client identity로 사용하지 않는다.
- network denial은 rate-limit, body spool/parse, replay, Quote/Reserve, Wallet/Ledger와 Provider 전에 발생한다.
- unrestricted Key는 header ambiguity 때문에 불필요하게 차단되지 않지만 restricted Key는 resolver 불확실성에서 fail closed한다.
- IPv4/IPv6 표현 차이를 canonicalization해 textual bypass를 막는다.
- network failure는 candidate-specific 상태가 아니므로 routing fallback하지 않는다.
- trusted proxy 설정과 Key prefix 정책은 client 요청으로 변경할 수 없다.

## 테스트 계획

### 단위 테스트

- IPv4/IPv6/mapped address와 CIDR canonicalization, dedupe, containment
- direct peer mode에서 spoofed headers 무시
- trusted single/multi-hop right-to-left resolution
- mixed `Forwarded`/XFF, malformed, unknown, zone ID와 excessive chain fail-closed
- all/allowlist network authorization matrix
- native OpenAI/Gemini 403와 unrestricted regression
- denial 시 rate limiter/body/Billing/replay/executor 미호출
- config validation/redaction과 log redaction

### 통합 테스트

- migration existing Key network default-all
- Key/prefix atomic create, authentication snapshot과 cascade
- CLI repeated CIDR round trip과 plaintext 비저장
- real HTTP listener direct peer allow/deny
- trusted reverse proxy header allow/deny 및 spoof resistance
- Gateway restart 후 policy 즉시 반영
- corrupt empty allowlist와 invalid DB policy authentication fail-closed

### 호환성 및 장애 테스트

- SDK 요청은 forwarding header 없이 direct peer에서 기존과 동일하게 동작
- load balancer가 header를 누락/중복/변조할 때 restricted Key만 fail closed
- rate limiting, model authorization, idempotency와 reconciliation 회귀
- `go test -race`에서 resolver와 immutable prefix snapshot data race 없음

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

- [x] 기존 Key가 network default-all로 동일하게 동작함
- [x] restricted Key가 canonical IPv4/IPv6 prefix만 허용함
- [x] untrusted peer의 spoofed forwarding header가 무시됨
- [x] trusted multi-hop chain이 right-to-left 규칙으로 안전하게 해석됨
- [x] ambiguous/malformed resolver state가 restricted Key에서 fail closed함
- [x] network denial은 rate-limit, Billing/Wallet/Ledger/replay와 Provider effect가 없음
- [x] native OpenAI/Gemini 403과 models route가 일관되게 적용됨
- [x] Key/prefix 생성 원자성, snapshot과 cascade가 검증됨
- [x] credential/header chain/trusted topology가 log와 response에 노출되지 않음
- [x] README와 Cloud handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `0b05f0d38ce92abfdc547e3f39dcfa283e898070`
- PR: `https://github.com/nativegatewayhq/gateway/pull/19`
- 로컬 검증: `GOCACHE=/private/tmp/nativegateway-go-cache make check`
- 통합 검증: PostgreSQL `127.0.0.1:55433`, Redis `127.0.0.1:56379`를 사용한 `make integration-test`
- GitHub Actions: `check` pass (`32352173199`), `validate` pass (`32352173077`)

## Rollback 계획

- network allowlist Key를 disable한 뒤 이전 binary로 rollback한다.
- 긴급 정책 완화는 affected Key를 명시적으로 `all`로 변경하고 감사한다.
- prefix table과 mode column은 보존해 재배포 시 정책을 복구한다.
- denial에는 금전 row가 없으므로 compensation이 필요하지 않다.

## 후속 작업

1. Cloud provider proxy range IaC 자동 갱신
2. Key 관리 REST API와 network policy audit log
3. organization/project/model 비용 quota
4. Geo/ASN 정책은 별도 privacy/security review 후 검토
5. PROXY protocol listener가 필요한 배포용 별도 adapter
