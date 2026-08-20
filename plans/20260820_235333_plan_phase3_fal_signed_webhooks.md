---
id: gateway-20260820-033
title: Phase 3 fal Signed Webhook Reconciliation
status: in_progress
created_at: 2026-08-20T23:53:33+09:00
updated_at: 2026-08-20T23:53:33+09:00
owners:
  - gateway
initiative: phase-3-fal-signed-webhooks
depends_on:
  - gateway-20260820-009
  - gateway-20260820-011
  - gateway-20260820-013
  - gateway-20260820-023
  - gateway-20260820-029
  - gateway-20260820-031
  - gateway-20260820-032
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 3 fal Signed Webhook Reconciliation

## 목적

fal Queue 완료 webhook을 Gateway-owned callback으로 안전하게 수신하고 ED25519/JWKS 검증, durable replay protection과 terminal CAS를 거쳐 polling·cancel·과금 정산과 정확히 한 번 수렴하게 한다.

## 배경

Plan 031은 fal native Queue submit/status/result/cancel과 durable polling을, Plan 032는 Provider-owned signed callback을 async Job에 연결하는 첫 Replicate 구현을 제공했다. fal은 Replicate의 shared-secret HMAC과 달리 public JWKS의 ED25519 key로 webhook을 검증한다. 공식 계약은 `fal_webhook` submit query, `X-Fal-Webhook-Request-Id`, `X-Fal-Webhook-User-Id`, Unix timestamp와 hex signature를 사용한다.

검증 message는 request ID, user ID, timestamp와 raw body의 SHA-256 hex digest를 newline으로 연결한다. 공개 key는 고정된 `https://rest.fal.ai/.well-known/jwks.json`에서 조회하고 24시간을 넘지 않게 cache해야 한다. Gateway는 client webhook을 그대로 전달하지 않고 자체 callback capability만 주입하며, webhook envelope의 upstream request ID가 stored Provider Job ID와 일치할 때만 기존 Job/settlement path에 terminal observation을 적용해야 한다.

## 범위

- fal submit의 Gateway-owned `fal_webhook` query 주입
- Provider 전용 `POST /internal/webhooks/fal/{gateway_job_id}/{capability_token}` ingress
- required fal webhook headers, timestamp와 raw-body digest message 구성
- hex ED25519 signature와 base64url OKP/Ed25519 JWKS key 검증
- fixed-origin JWKS fetch, bounded response, redirect rejection, cache와 refresh
- Plan 032 callback capability binding/delivery schema의 fal 확장
- per-Job random capability의 deployment-secret HMAC digest, expiry와 Provider/channel/model identity
- webhook envelope의 request ID/status/payload/error validation과 native result projection
- delivery replay, webhook·poll·result-fetch·cancel terminal race의 CAS 수렴
- success Capture, known failure/cancel Release와 unknown reservation 유지
- retry-safe HTTP status, polling fallback, bounded telemetry와 운영 문서
- 실제 PostgreSQL integration, mock JWKS/fal Provider와 regression test

## 제외 범위

- client-supplied `fal_webhook` 또는 `webhook_url` pass-through
- IP allowlist 자동 동기화와 fal Platform Meta API
- JWKS를 client request가 지정한 URL에서 조회하는 기능
- Replicate webhook 계약 변경
- non-terminal webhook, progress logs와 streaming event
- webhook payload를 사용자 endpoint로 forwarding
- managed image/video result storage와 CDN 변환
- synchronous `fal.run`, realtime/WebSocket과 file upload
- submit 이후 cross-provider fallback
- JWKS control-plane UI와 manual key upload

## 설계 및 구현 순서

### 1. Callback와 shared Job contract 일반화

- Plan 032 binding/delivery provider constraint를 `replicate|fal`로 additive migration에서 확장한다.
- generic Job webhook configuration은 provider별 public callback route, callback HMAC secret과 TTL을 지원한다.
- fal submit payload는 callback metadata를 typed field로 받고 adapter만 fixed Queue URL에 escaped `fal_webhook` query를 추가한다.
- inbound client query/body의 webhook field는 기존처럼 거부하며 Gateway callback은 public submit/result response에 포함하지 않는다.
- callback binding 생성 실패는 upstream submit 전에 known failure로 처리해 reservation을 Release 대상으로 만든다.

### 2. Fixed-origin JWKS cache

- JWKS URL은 기본 `https://rest.fal.ai/.well-known/jwks.json`이고 startup config에서 HTTPS origin과 exact path를 검증한다. loopback override는 test에서만 허용한다.
- fetch client는 redirect, proxy credential forwarding과 automatic retry를 끄고 timeout 및 64 KiB 이하 body limit를 둔다.
- JWKS는 `kty=OKP`, `crv=Ed25519`, bounded `kid`, 정확히 32-byte base64url `x`만 허용하고 duplicate `kid`와 빈 key set을 거부한다.
- 성공 cache TTL은 HTTP cache header와 configured maximum 중 더 짧은 값으로 제한하고 절대 24시간을 넘지 않는다.
- unknown `kid` 또는 signature mismatch 시 refresh cooldown을 지키며 한 번 refresh한 뒤 재검증한다. refresh stampede는 single-flight로 합친다.
- 아직 valid한 cached key가 있으면 transient refresh failure 시 stale이 아닌 cache로 계속 검증하고, valid key가 없으면 503으로 Provider retry를 요청한다.

### 3. ED25519 ingress 검증

- body를 제한 크기까지 raw bytes로 읽고 JSON decode 전에 SHA-256 lowercase hex digest를 계산한다.
- required header 네 개는 각각 정확히 하나만 허용하고 CR/LF, 과도한 길이와 ambiguous duplicate를 거부한다.
- timestamp는 strict base-10 Unix seconds이며 기본 ±5분 tolerance를 적용한다.
- message는 `{request_id}\n{user_id}\n{timestamp}\n{sha256_hex(raw_body)}` UTF-8 bytes로 정확히 구성한다.
- signature는 정확히 64-byte가 되는 hex만 허용하고 cached ED25519 public keys 중 하나로 검증한다.
- signature/JWKS 검증 완료 전 callback token, Job, Ledger DB lookup을 수행하지 않는다.

### 4. Envelope와 Provider identity

- signed body는 bounded JSON object이며 `request_id`, `gateway_request_id`, `status`와 terminal payload/error만 허용한다.
- envelope `request_id` 또는 공식 retry semantics에 따른 canonical queue identity가 stored upstream Provider Job ID와 exact match해야 한다.
- `X-Fal-Webhook-Request-Id`와 body identity의 공식 관계를 검증 fixture로 고정하고 서로 바꿔 tenant Job을 선택할 수 없게 한다.
- path Job ID, capability digest, provider/channel/model과 Provider Job ID가 모두 일치해야 한다.
- success `status=OK`는 `payload`만 fal native result snapshot으로 저장하고 failure/cancel은 official status/error envelope를 bounded projection한다.
- raw upstream control URL, Provider credential, user ID와 Provider Job ID는 public snapshot에 저장하지 않는다.

### 5. Durable replay와 정산 수렴

- fal delivery identity는 signed webhook request ID와 Provider namespace의 durable unique key로 기록한다.
- delivery insert, terminal observation, attempt terminalization과 settlement intent를 하나의 transaction에서 commit한 뒤 2xx를 반환한다.
- identical retry와 이미 같은 terminal snapshot은 2xx, identity/schema 오류는 4xx, submit-confirm race와 DB/JWKS transient failure는 5xx로 응답한다.
- webhook과 status poll/result fetch/cancel이 경합하면 최초 유효 terminal CAS만 채택하고 stale worker lease는 실패한다.
- success는 Capture, known failure/cancel은 Release하고 malformed/unknown observation은 reservation과 polling을 유지한다.
- polling worker는 webhook mode와 관계없이 계속 실행해 missed delivery, JWKS 장애와 rollback을 복구한다.

### 6. Configuration, telemetry와 rollout

- mode는 `disabled|required`이며 required는 fal enabled, HTTPS public base, 32-byte callback secret, JWKS URL/timeout/cache 설정을 startup에서 검증한다.
- telemetry는 protocol `fal`, stage `webhook`, bounded Job status/outcome만 기록하고 path token, header, request/user ID, signature와 payload를 label/event에 넣지 않는다.
- access/trace route는 token을 template으로 redaction하고 fal wildcard route보다 먼저 mount한다.
- webhook-disabled 기본값은 Plan 031 wire를 유지하며 additive migration 이후 required mode를 활성화한다.
- README와 Cloud/Conformance handoff에 JWKS egress, cache/refresh alert, callback secret rotation과 rollback을 기록한다.

## 인터페이스와 데이터 변경

### 공개 API

서비스 사용자가 호출하는 fal Queue API는 변경하지 않는다. Provider 전용 ingress를 추가한다.

```text
POST /internal/webhooks/fal/{gateway_job_id}/{capability_token}
```

service API Key 대신 valid fal signature, timestamp와 per-Job capability를 모두 요구한다. 응답에는 Job, user ID, Provider request ID 또는 결과를 포함하지 않는다.

### 내부 인터페이스

- generic `jobs.WebhookConfig`와 repository binding/delivery를 fal Provider까지 확장한다.
- fal `SubmitPayload`가 Gateway callback을 받는 `WebhookPayload` 계약을 구현한다.
- fal adapter가 webhook envelope를 existing native result/status sanitizer로 변환하는 typed method를 제공한다.
- `FalJWKSVerifier`는 clock, HTTP client와 cache를 주입할 수 있으며 concurrent refresh를 직렬화한다.

### 데이터베이스 및 migration

- webhook binding/delivery provider check constraint를 `replicate`, `fal`로 확장한다.
- 기존 Replicate binding/delivery row와 unique key는 변경하지 않는다.
- fal callback token 원문, raw body, signature, user ID와 JWKS body는 저장하지 않는다.
- migration은 additive/constraint replacement이며 구 binary는 fal row를 읽지 않는다. rollback 시 fal injection/route만 끄고 rows는 유지한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 `phase-3-fal-signed-webhooks` initiative로 official fal JavaScript/Python callback submit wire, ED25519 fixture, envelope/retry matrix를 소유한다.
- Cloud는 fixed JWKS egress, public HTTPS callback ingress, callback-secret injection/rotation, cache refresh와 signature failure alert를 소유한다.
- Gateway는 callback route, required headers, JWKS cache bounds, retry status와 native terminal projection을 versioned handoff로 제공한다.

## 보안 및 과금 고려사항

- JWKS fetch는 fixed exact URL만 사용하며 inbound header, `kid`, body URL과 callback query를 fetch target에 반영하지 않는다.
- ED25519 검증은 raw body digest와 모든 identity/timestamp header를 bind하고 body parse보다 먼저 수행한다.
- per-Job capability keyed digest와 Provider/channel/model/Job ID 검증으로 valid fal signature의 cross-tenant confused-deputy 사용을 막는다.
- callback URL과 secret-bearing path는 logs, traces, metrics, events, snapshots와 client response에서 제거한다.
- webhook은 신규 Reserve를 만들지 않고 기존 Job charge에 terminal settlement intent만 만든다.
- duplicate delivery와 semantic duplicate는 delivery unique key, Job CAS, settlement lease와 Ledger operation key의 다중 경계로 이중 과금을 막는다.
- JWKS unavailable, early callback, malformed payload와 unknown status는 임의 Release하지 않고 Provider retry 및 polling reconciliation에 남긴다.

## 테스트 계획

### 단위 테스트

- JWKS OKP/Ed25519 parsing, base64url/key length, duplicate/unknown kid와 body bound
- raw body digest message, hex signature, timestamp/duplicate header와 ED25519 verification
- cache TTL≤24h, cache-control bound, refresh cooldown/single-flight와 stale-invalid failure
- fal callback query injection, escaping, client webhook rejection와 URL redaction
- success/failure/cancel envelope mapping 및 Provider/user identity removal

### 통합 테스트

- migration fresh/current upgrade와 existing Replicate row compatibility
- fal submit binding→Gateway `fal_webhook` query→Provider Job confirmation
- signed OK webhook→durable result→Capture 및 failure/cancel→Release
- identical delivery, different delivery same terminal과 replay acknowledgment
- early webhook 503 retry, expired/wrong capability와 Provider/channel/model/request mismatch
- webhook/status/result-fetch/cancel concurrent CAS와 Ledger exactly-once
- restart/missed webhook/JWKS failure에서 polling fallback

### 호환성 및 장애 테스트

- official fal JavaScript/Python SDK submit callback wire와 official ED25519 fixture
- JWKS 200/304, rotation, timeout, 404/429/500, malformed/oversized body와 redirect
- stale/future timestamp, invalid hex, wrong raw-body hash, duplicate header와 unknown key
- callback response loss, retry, out-of-order terminal status와 DB transaction conflict
- existing fal polling, Replicate webhook, OpenAI/Gemini protocol regression

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

- [ ] Gateway-owned `fal_webhook`만 submit되고 client webhook은 거부됨
- [ ] official header/message/hex ED25519 계약과 ±5분 timestamp가 검증됨
- [ ] fixed-origin JWKS cache/rotation/refresh가 24시간 bound와 장애 정책을 지킴
- [ ] callback capability와 Provider/channel/model/request identity가 모두 검증됨
- [ ] success native result와 known failure/cancel이 durable하게 projection됨
- [ ] duplicate delivery와 semantic replay가 observation/Ledger 전이를 늘리지 않음
- [ ] webhook/poll/result/cancel race가 하나의 terminal 상태로 수렴함
- [ ] success Capture와 failure/cancel Release가 정확히 한 번 수행됨
- [ ] invalid/unknown/JWKS unavailable 경로가 reservation을 임의 해제하지 않음
- [ ] callback/JWKS/identity secret이 logs, telemetry와 public snapshot에 노출되지 않음
- [ ] webhook-disabled rollback과 polling fallback이 유지됨
- [ ] 기존 Replicate webhook 및 모든 protocol/race/integration test가 회귀하지 않음
- [ ] README와 Cloud/Conformance handoff가 갱신됨
- [ ] commit, PR과 최종 CI 증거가 기록됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- fal webhook mode를 `disabled`로 전환해 신규 callback query 주입과 ingress를 끈다.
- 기존 fal Job은 status/result polling으로 계속 reconciliation하며 terminal 확인 전 reservation을 Release하지 않는다.
- JWKS cache와 additive fal binding/delivery row는 보존하되 더 이상 ingress에서 사용하지 않는다.
- signature/JWKS 장애율이 임계치를 넘으면 required submit을 pre-dispatch 차단하거나 fal channel을 비활성화한다.

## 후속 작업

- webhook IP allowlist의 Platform Meta API 자동 동기화
- JWKS/cache control-plane 상태 및 webhook delivery 관리자 화면
- async image/video/audio terminal result의 managed storage/CDN
- callback binding/delivery retention과 archival worker
