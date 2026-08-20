---
id: gateway-20260820-032
title: Phase 3 Replicate Signed Webhook Reconciliation
status: in_progress
created_at: 2026-08-20T23:12:25+09:00
updated_at: 2026-08-20T23:36:21+09:00
owners:
  - gateway
initiative: phase-3-replicate-signed-webhooks
depends_on:
  - gateway-20260820-009
  - gateway-20260820-011
  - gateway-20260820-013
  - gateway-20260820-023
  - gateway-20260820-029
  - gateway-20260820-030
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 3 Replicate Signed Webhook Reconciliation

## 목적

Replicate Prediction의 signed webhook을 Gateway가 안전하고 멱등적으로 수신하여 polling보다 빠르게 durable Job 상태와 과금 정산을 갱신하고, webhook·polling·cancel 경합이 하나의 terminal 결과로 수렴하게 한다.

## 배경

Plan 029와 Plan 030은 재시작 가능한 async Job, Replicate native Prediction facade와 polling worker를 제공했다. 현재 terminal 상태는 polling으로만 관측하므로 완료 반영이 poll interval만큼 늦고, Provider 호출량도 작업 수에 비례한다. Replicate는 Prediction 생성 시 callback URL과 event filter를 받고, webhook 본문을 계정 secret 기반 HMAC-SHA256으로 서명한다.

Gateway는 client가 제공한 webhook URL을 그대로 전달할 수 없다. 이는 SSRF, tenant 데이터 유출, credential·결과 exfiltration 경계를 만든다. 대신 Gateway가 생성한 HTTPS callback URL만 outbound submit에 주입하고, `webhook-id`, `webhook-timestamp`, `webhook-signature`와 exact raw body를 검증해야 한다. webhook은 신뢰 가능한 terminal observation일 뿐 별도 과금 경로가 아니며, 기존 polling 및 reconciliation과 동일한 CAS와 Ledger 불변 조건을 사용해야 한다.

## 범위

- Gateway-owned Replicate webhook callback URL 및 completed event filter 주입
- account-scoped Replicate webhook signing secret의 startup configuration과 rotation window
- raw request body 기반 HMAC-SHA256 signature, timestamp tolerance와 constant-time comparison
- opaque callback capability token, Job/provider/channel binding과 만료 정책
- delivery ID 기반 durable replay protection
- bounded Replicate Prediction webhook parsing, identity/status 검증과 snapshot sanitization
- webhook·polling·cancel·timeout reconciliation의 terminal CAS 통합
- terminal success Capture, known failure/cancel Release와 unknown reservation 유지
- webhook 처리 결과의 retry-safe HTTP status와 최소 telemetry
- polling fallback, webhook 비활성화 및 rolling deployment 호환
- mock Provider, 실제 PostgreSQL과 race/integration test
- Cloud/Conformance handoff 계약 및 운영 문서

## 제외 범위

- client가 지정한 webhook URL 또는 event filter pass-through
- Replicate 외 Provider의 webhook ingress
- fal ED25519/JWKS webhook 검증과 `fal_webhook` callback
- webhook secret의 Replicate API 자동 조회·회전
- webhook payload를 사용자 endpoint로 재전송하는 기능
- submit 외 Prediction API, streaming/SSE와 realtime event delivery
- managed result storage 변환과 async 결과 CDN 복제
- webhook만을 전제로 polling/reconciliation 제거
- non-terminal logs/output progress의 public snapshot 갱신
- 이미 생성된 webhook 없는 Prediction의 callback retrofit

## 설계 및 구현 순서

### 1. Callback와 configuration 경계

- webhook 기능은 명시적 mode(`disabled` 또는 `required`)로 시작하며 `required`는 HTTPS public base URL, callback-token secret과 하나 이상의 valid Replicate signing secret이 모두 있어야 startup한다.
- callback URL은 Gateway public base와 고정된 internal route, Gateway Job ID 및 opaque capability token으로만 생성한다.
- capability token은 충분한 entropy의 random value로 생성하고 digest만 저장하며 로그, metric, event와 public Prediction response에는 포함하지 않는다.
- Replicate submit payload에 Gateway callback과 terminal event filter를 server-side로 주입한다. inbound client webhook fields는 기존처럼 거부한다.
- webhook 설정 실패 시 Provider submit 전에 실패시켜 reservation을 Release하며 webhook을 요구한 채 callback 없는 Prediction을 만들지 않는다.

### 2. Signature와 ingress 검증

- ingress는 body를 제한 크기까지 raw bytes로 읽고 JSON decode 전에 `{webhook-id}.{webhook-timestamp}.{raw body}`를 서명 입력으로 만든다.
- configured `whsec_` secret의 base64 payload로 HMAC-SHA256을 계산하고 space-delimited `v1,<base64>` 후보 중 하나와 constant-time으로 비교한다.
- timestamp는 strict integer로 파싱하고 현재 시각 기준 기본 5분 tolerance 및 제한된 future skew를 적용한다.
- 필수 header 누락, malformed/oversized body, stale timestamp, unknown signature version, invalid capability token은 Provider detail 없이 거부한다.
- secret rotation은 active와 previous secret을 동시에 검증할 수 있게 하되 어느 secret이 일치했는지 관측 정보로 노출하지 않는다.

### 3. Durable binding과 replay protection

- Job submit 전에 `job_id`, provider, provider channel/credential scope, token digest, expiry를 durable binding으로 저장한다.
- webhook의 Prediction ID는 Job에 저장된 upstream Provider Job ID와 exact match해야 하며 Provider/channel binding도 일치해야 한다.
- `webhook-id`를 provider namespace와 함께 durable unique key로 기록하고 동일 delivery의 재전송은 이전 성공 acknowledgment를 재현한다.
- invalid signature/token/identity payload는 delivery로 기록하지 않아 올바른 Provider retry를 가로막지 않는다.
- 성공 acknowledgment는 observation 및 delivery 기록이 같은 transaction에서 commit된 뒤에만 반환한다.

### 4. Observation과 과금 수렴

- verified webhook payload를 Replicate adapter의 기존 response sanitizer와 status mapper를 재사용해 bounded Provider observation으로 변환한다.
- initial release는 configured completed event만 요청하며 terminal `succeeded`, `failed`, `canceled`만 상태 전이에 사용한다.
- webhook repository path는 worker lease를 요구하지 않지만 Job version/terminal CAS, Provider identity와 settlement intent 규칙을 그대로 적용한다.
- success는 sanitized final snapshot 저장 후 Capture하고 known failure/cancel은 Release한다. malformed terminal result 또는 DB 불확실성은 reservation을 유지하고 polling/reconciliation으로 넘긴다.
- webhook, poll과 cancel이 경합하면 최초의 유효 terminal CAS만 채택하고 duplicate Capture/Release를 만들지 않는다.

### 5. Retry, fallback과 운영

- signature·token·identity와 schema 오류는 retry 불가능한 4xx, transient DB/settlement 오류는 retry 가능한 5xx로 응답한다.
- 이미 정상 처리한 delivery와 이미 같은 terminal 상태인 유효 새 delivery는 2xx로 응답한다.
- webhook mode와 무관하게 polling worker를 유지하며 missed webhook, Provider retry 종료와 rolling deployment를 복구한다.
- ingress metric은 provider, outcome과 coarse error category만 기록하고 callback URL, delivery ID, Prediction ID와 payload는 기록하지 않는다.
- callback binding 및 delivery retention을 문서화하고 terminal Job 보존기간 이후 안전하게 정리할 수 있는 index를 둔다.

### 6. 호환성과 배포

- webhook-disabled 기본값은 기존 Replicate submit wire와 동작을 유지한다.
- webhook-required rollout은 새 binary와 additive migration 배포 후에만 활성화한다.
- official Replicate webhook header/signature fixture와 mock callback retry를 Conformance 계약으로 고정한다.
- Cloud는 public HTTPS ingress, signing secret rotation, callback-token secret과 webhook 실패율/backlog alert를 제공한다.

## 인터페이스와 데이터 변경

### 공개 API

서비스 사용자가 호출하는 API 변경은 없다. Provider 전용 ingress를 추가한다.

```text
POST /internal/webhooks/replicate/{gateway_job_id}/{capability_token}
```

이 route는 service API Key를 사용하지 않고 Replicate signature와 callback capability를 모두 요구한다. 응답은 최소 acknowledgment만 반환하며 Job 또는 과금 정보를 포함하지 않는다.

### 내부 인터페이스

- Replicate submit adapter에 Gateway-owned callback metadata를 전달하는 typed submit context를 추가한다.
- Job repository에 callback binding 생성, verified delivery 적용과 replay 조회 transaction을 추가한다.
- polling과 webhook이 함께 사용할 terminal observation validation/settlement path를 추출한다.
- signature verifier는 clock과 secret set을 주입할 수 있는 독립 component로 둔다.

### 데이터베이스 및 migration

- `async_job_webhook_bindings`: Job/provider/channel, token digest, expiry, created/disabled timestamps를 저장한다.
- `async_job_webhook_deliveries`: provider와 delivery ID unique key, Job ID, accepted outcome과 received timestamp를 저장한다.
- token 원문, raw body, signature와 Provider credential은 저장하지 않는다.
- schema는 additive하며 webhook-disabled 구 binary는 새 table을 무시한다. rollback 시 route와 injection을 끄고 table은 retention 기간까지 유지한다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 `phase-3-replicate-signed-webhooks` initiative로 공식 signature fixture, header 변형, retry와 duplicate delivery matrix를 소유한다.
- Cloud는 동일 initiative로 HTTPS public callback route, trusted proxy/Host 정책, signing-secret rotation과 secret injection을 소유한다.
- Gateway는 callback path, body/header limits, retry status, terminal projection과 mode별 startup 요구사항을 versioned handoff로 제공한다.

## 보안 및 과금 고려사항

- Provider signature만으로 Job을 선택하지 않고 unguessable callback capability와 stored Provider identity를 함께 검증한다.
- raw body를 signature 검증 전에 parse/re-encode하지 않으며 compressed body, ambiguous duplicate security header와 unsupported content encoding을 거부한다.
- callback URL은 outbound Replicate API에만 전달하고 client response, logs, traces와 metric labels에서 제거한다.
- trusted proxy 설정 없이 forwarded host/proto로 callback origin을 만들지 않는다.
- replay protection은 delivery 중복을 막고 terminal CAS 및 append-only Ledger unique constraint는 semantic 중복 정산을 막는다.
- webhook 도착 자체로 신규 Reserve를 만들지 않는다. 기존 reservation에 대해서만 Capture/Release한다.
- unknown, malformed, non-terminal과 identity mismatch는 임의 Release하지 않고 기존 polling/reconciliation이 확인하도록 둔다.

## 테스트 계획

### 단위 테스트

- exact raw body 서명, secret base64 decode, multiple `v1` signature와 constant-time verifier
- stale/future timestamp, malformed/duplicate header와 secret rotation
- callback capability 생성/digest/expiry 및 URL redaction
- Replicate terminal payload sanitization, Provider ID mismatch와 non-terminal rejection
- HTTP status/retry category와 body/content-encoding bounds

### 통합 테스트

- migration fresh/current upgrade와 binding/delivery unique constraints
- submit 전 binding 생성, outbound callback/event filter 주입과 client webhook 거부
- signed succeeded webhook→snapshot→Capture 및 failed/canceled→Release
- duplicate delivery, 다른 delivery의 동일 terminal event와 idempotent acknowledgment
- webhook/poll/cancel concurrent terminal CAS 및 Ledger exactly-once
- DB/settlement failure 후 Provider retry, process restart와 polling fallback
- expired token, wrong tenant/Job/Provider/channel/Prediction identity 격리

### 호환성 및 장애 테스트

- official Replicate signature example와 header casing/order 변형
- webhook response loss와 identical retry, out-of-order terminal event
- oversized/malformed JSON, stale signature, invalid HMAC과 timing-safe failure
- trusted public base validation, Host/forwarded-header injection과 callback leak regression
- existing Replicate Python/native Prediction, fal Queue와 OpenAI/Gemini regression

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

- [x] Gateway-owned callback과 completed event filter만 Replicate submit에 주입됨
- [x] official raw-body HMAC, timestamp와 signature rotation 계약이 검증됨
- [x] callback capability, Provider/channel/Prediction identity가 모두 검증됨
- [x] duplicate delivery가 observation과 Ledger 전이를 늘리지 않음
- [x] success Capture와 known failed/canceled Release가 정확히 한 번 수렴함
- [x] webhook, polling과 cancel terminal race가 하나의 durable 결과로 수렴함
- [x] invalid/unknown webhook이 reservation을 임의 해제하지 않음
- [x] transient failure는 Provider retry로 복구되고 polling fallback이 유지됨
- [x] callback secret, URL, signature, Provider ID와 payload가 노출되지 않음
- [x] webhook-disabled rolling rollback이 기존 Replicate 동작을 보존함
- [x] 기존 fal/OpenAI/Gemini protocol과 전체 race/integration test가 회귀하지 않음
- [x] 운영 문서와 Cloud/Conformance handoff가 갱신됨
- [ ] commit, PR과 최종 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `9ec9db5` (signed ingress, HMAC-keyed callback capability, additive migration, durable replay/CAS, runtime wiring, telemetry와 운영 문서).
- signature: exact raw body, `webhook-id`/`webhook-timestamp`/space-delimited `v1` signatures, active/previous `whsec_` secret, stale/future timestamp 및 duplicate header rejection을 단위 테스트로 검증함.
- callback 경계: HTTPS public base, server-owned `completed` filter, per-Job random token의 keyed digest만 저장, client webhook 거부와 public response/log/metric route redaction을 검증함.
- durable ingress: early delivery 503 retry, expired/wrong token, Provider/channel/Prediction mismatch, delivery replay와 append-only schema를 실제 PostgreSQL에서 검증함.
- 동시성: webhook과 leased polling의 terminal race가 단일 `OBSERVED` event 및 settlement intent로 수렴함을 race integration test로 검증함. 기존 cancel terminal CAS suite도 통과함.
- 과금: signed webhook `SUCCEEDED`의 Capture 1회 및 `FAILED`/`CANCELED`의 Release 1회를 실제 Wallet, Ledger와 settlement lease에서 검증함.
- 로컬 검증: `make check` 통과.
- 통합 검증: Compose PostgreSQL/Redis에서 migration, Job, billing, Replicate/fal/OpenAI/Gemini 회귀를 포함한 `make integration-test` 통과.
- PR 및 CI: PR 생성 후 기록 예정.

## Rollback 계획

- webhook mode를 `disabled`로 바꿔 새 Prediction의 callback 주입과 ingress 처리를 중단한다.
- 기존 Prediction은 polling worker로 계속 reconciliation하고 reservation을 임의 Release하지 않는다.
- additive binding/delivery table은 Provider retry와 감사 기간 동안 유지한 뒤 retention 정책으로 정리한다.
- secret 또는 callback ingress 장애 시 Provider channel을 비활성화하거나 webhook-required submit을 pre-dispatch에서 차단한다.

## 후속 작업

- fal ED25519/JWKS signed webhook ingress
- webhook secret 자동 조회, scheduled rotation과 control-plane 관리
- async terminal result managed storage/CDN delivery
- webhook binding/delivery retention worker와 관리자 감사 화면
