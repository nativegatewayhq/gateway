---
id: gateway-20260820-028
title: Phase 2 OpenTelemetry Tracing and Metrics
status: completed
created_at: 2026-08-20T21:23:26+09:00
updated_at: 2026-08-20T21:41:47+09:00
owners:
  - gateway
initiative: phase-2-opentelemetry-observability
depends_on:
  - gateway-20260820-011
  - gateway-20260820-013
  - gateway-20260820-016
  - gateway-20260820-026
  - gateway-20260820-027
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 2 OpenTelemetry Tracing and Metrics

## 목적

Gateway의 native HTTP 요청부터 인증, 라우팅, Provider dispatch, 과금 정산, managed storage와 reconciliation까지 이어지는 bounded OpenTelemetry traces와 metrics를 OTLP로 내보내 운영자가 지연·실패·비용 처리 상태를 상관 분석할 수 있게 한다.

## 배경

현재 Gateway는 request ID를 포함한 구조화 로그와 readiness만 제공한다. Provider fallback, circuit 상태, Wallet settlement, storage upload 또는 reconciliation 지연이 발생해도 여러 인스턴스의 단일 요청 경로와 시스템 추세를 연결하기 어렵다. 반대로 무분별한 telemetry는 prompt, API Key, credential, customer identity와 비용 정보를 외부 collector로 유출하거나 model/channel/request ID label로 metric cardinality를 폭증시킬 수 있다. 따라서 instrumentation보다 먼저 안정적인 attribute schema, exporter failure 의미와 수명 주기를 고정해야 한다.

## 범위

- OpenTelemetry SDK와 global하지 않은 process-owned provider 수명 주기
- OTLP HTTP/protobuf trace 및 metric export
- disabled/optional/required startup 설정과 bounded shutdown flush
- W3C `traceparent`/`tracestate` inbound context extraction
- native HTTP server request spans와 RED metrics
- authentication/rate-limit/model authorization outcome metrics
- routing policy, fallback depth와 circuit eligibility metrics
- Provider dispatch span, duration, status class와 bounded outcome
- billing Begin/Complete/reconciliation state metrics
- managed image fetch/upload/result transform metrics
- reconciliation worker cycle/task outcome metrics
- request ID와 trace/span ID structured-log correlation
- stable low-cardinality semantic attribute registry
- in-memory exporter 기반 deterministic tests와 Cloud dashboard/alert handoff

## 제외 범위

- prompt, request/response body, URL query, headers 또는 file telemetry
- API Key ID, organization/project/customer ID와 idempotency key attributes
- raw model name, Provider channel ID, candidate ID 또는 object key metric labels
- 고객별 비용·마진·Wallet balance metrics
- upstream Provider 요청으로 trace headers 자동 주입
- tail sampling collector, Grafana dashboard와 alert IaC 구현
- public metrics endpoint와 Prometheus pull exporter
- continuous profiling과 log exporter
- LLM token/streaming metrics

## 설계 및 구현 순서

### 1. Telemetry configuration과 lifecycle

- `GATEWAY_TELEMETRY_MODE=disabled|optional|required`를 추가하고 기본값은 `disabled`로 둔다.
- OTLP HTTP endpoint, authorization header의 secret reference, service name/version, deployment environment, sample ratio, export interval·timeout와 shutdown timeout을 검증한다.
- endpoint는 production HTTPS 또는 loopback HTTP만 허용하며 설정 오류에 secret 값을 포함하지 않는다.
- optional mode exporter 전송 장애는 request/readiness를 실패시키지 않고 bounded local warning/counter만 남긴다.
- required mode는 startup exporter construction 오류만 fail-fast하며 실행 중 collector 장애로 Gateway traffic을 fail closed하지 않는다.
- process shutdown은 새 request를 중단한 뒤 설정된 timeout 안에서 metric flush와 trace shutdown을 수행한다.

### 2. Attribute schema와 cardinality budget

- 공통 attributes는 protocol, operation, route policy, Provider, status class, bounded outcome/error category와 replay 여부로 제한한다.
- model은 metric label에서 제외하고 sampled span에서 registry가 허용한 logical model만 선택적으로 기록한다.
- channel/candidate/request/charge/API Key/customer/object identity는 metric label에 사용하지 않는다.
- fallback depth, image count와 byte size는 bounded numeric value/histogram으로 기록한다.
- error text, raw status body, URL, SQL, Redis key와 credential은 attribute/event에 기록하지 않는다.
- 계측 API는 문자열 자유 입력 대신 enum/typed record를 받아 cardinality가 코드 리뷰에서 드러나게 한다.

### 3. HTTP와 native protocol instrumentation

- middleware가 유효한 W3C remote parent를 추출하고 Gateway server span을 시작한다.
- route template만 span name/attribute로 사용하고 실제 model path, query와 API Key를 제외한다.
- request duration, active requests, response status class와 response byte bucket을 protocol/operation별로 기록한다.
- client cancellation은 server error와 분리하고 panic은 기존 native error/billing 처리를 보존한 채 span status만 error로 종료한다.
- request ID는 log correlation용으로 span attribute에만 허용하고 metric label에서는 금지한다.

### 4. Routing과 Provider instrumentation

- fixed/priority/lowest-cost/weighted selection은 policy, candidate count, bounded fallback depth와 선택 결과를 기록한다.
- health OPEN/probe busy, price/margin/spend-cap/credential exclusion은 bounded rejection category counter를 증가시킨다.
- 실제 dispatch만 Provider client span과 duration/outcome observation을 만든다.
- status는 `2xx`, `4xx`, `429`, `5xx`, `timeout`, `connection`, `canceled`로 제한하고 response/body/raw error는 기록하지 않는다.
- Billing Begin 후 Provider 실패의 non-fallback 계약과 health observation 횟수는 telemetry 때문에 변경하지 않는다.

### 5. Billing, storage와 worker metrics

- billing은 reserve/begin, capture, release, reconciling과 replay transition count/duration을 outcome별로 기록하되 금액은 기록하지 않는다.
- managed storage는 source kind, fetch/upload/transform 단계, bounded result count/byte histogram과 category만 기록한다.
- asset ID, URL, bucket, object key, MIME parameter와 checksum은 제외한다.
- reconciliation worker는 claimed/resolved/retried/manual count, cycle duration과 backlog state gauge를 기록한다.
- telemetry 호출 실패나 panic은 Wallet/Ledger, asset state, worker lease와 client response에 영향을 주지 않는다.

### 6. Log correlation과 운영 handoff

- logger handler가 active span context에서 trace ID와 span ID를 자동 추가하되 sampled 여부와 trace flags 외 baggage는 기록하지 않는다.
- 기존 bounded structured fields와 redaction 테스트를 유지한다.
- README에 collector 설정, optional/required 의미, attribute 금지 목록과 shutdown 동작을 문서화한다.
- Cloud는 동일 initiative에서 OTLP collector, TLS/authorization secret, retention, sampling, dashboards와 alerts를 소유한다.

## 인터페이스와 데이터 변경

### 공개 API

native OpenAI/Gemini endpoint와 response body는 변경하지 않는다. 유효한 W3C trace context는 inbound header에서 읽지만 response에는 vendor-specific tracing detail을 추가하지 않는다. 기존 `X-Request-Id` 동작은 유지한다.

### 내부 인터페이스

```go
type Recorder interface {
    HTTP(context.Context, HTTPRecord)
    Route(context.Context, RouteRecord)
    Provider(context.Context, ProviderRecord)
    Billing(context.Context, BillingRecord)
    Storage(context.Context, StorageRecord)
    Reconciliation(context.Context, ReconciliationRecord)
}
```

disabled mode는 allocation이 제한된 no-op recorder를 사용한다. SDK-backed recorder는 typed record를 stable attributes와 instruments로 변환한다. Handler와 domain service는 SDK global state를 직접 참조하지 않고 recorder를 주입받는다.

### 데이터베이스 및 migration

PostgreSQL migration은 없다. metrics gauge를 위해 기존 reconciliation/asset state를 bounded aggregate query로 읽을 수 있지만 telemetry가 원장 row를 수정하거나 별도 durable telemetry outbox를 만들지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 `phase-2-opentelemetry-observability` initiative에서 OTLP collector endpoint, TLS/authorization, resource attributes, retention과 cardinality alerts를 배포한다. Gateway는 documented metric/instrument/attribute 이름과 단위를 semver 호환 계약으로 제공한다.

## 보안 및 과금 고려사항

- trace propagation은 `traceparent`와 `tracestate`만 읽고 baggage를 무시한다.
- OTLP authorization 값과 endpoint query는 secret으로 취급해 response/log에 포함하지 않는다.
- prompt, native body, file, raw URL/error, Provider credential, service API Key와 tenant identity를 span event/attribute에 기록하지 않는다.
- telemetry exporter는 Provider credential transport와 분리하며 upstream Provider로 inbound trace headers를 전달하지 않는다.
- 계측 실패는 Reserve/Capture/Release/Reconciliation 순서와 트랜잭션 결과를 변경하지 않는다.
- amount, balance, quota/spend-cap limit와 margin은 telemetry에 기록하지 않는다.
- request ID/trace ID는 metric label이 아니며 logs/span correlation에만 사용한다.

## 테스트 계획

### 단위 테스트

- disabled/optional/required config와 secret redaction
- endpoint TLS/loopback, ratio, interval/timeout bounds
- stable attribute allowlist와 forbidden-field absence
- trace context extraction, invalid parent와 baggage 무시
- logger trace/span correlation과 no-context parity
- typed outcome/status/category mapping
- no-op recorder non-interference

### 통합 테스트

- in-memory trace/metric exporter에서 HTTP→route→Provider→billing span parentage
- OpenAI generation/JSON+multipart edit와 Gemini operation parity
- fallback/circuit/timeout/cancel/replay bounded attributes
- storage fetch/upload/reconciliation transform instruments
- concurrent requests에서 active gauge와 counter 정확성
- exporter failure가 native response, billing과 readiness를 변경하지 않음
- graceful shutdown force flush 성공과 timeout bound

### 호환성 및 장애 테스트

- malformed `traceparent`, oversized tracing headers와 hostile baggage
- collector 401/429/500, connection failure와 slow export
- Provider timeout/panic, Redis/PostgreSQL/storage 장애 시 span 종료
- telemetry disabled mode의 기존 wire/log behavior 회귀 없음
- metric label cardinality snapshot 검증

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

- [x] disabled mode가 기존 native wire, readiness와 log behavior를 보존함
- [x] optional/required OTLP lifecycle과 bounded shutdown이 검증됨
- [x] OpenAI/Gemini server span과 RED metrics가 정확한 protocol/operation을 사용함
- [x] routing, Provider, billing, storage와 reconciliation 단계가 동일 trace에서 상관됨
- [x] 실제 Provider dispatch만 client span/duration을 생성함
- [x] replay가 Provider/storage span을 만들지 않고 replay metric만 기록함
- [x] exporter 장애가 client response, Wallet/Ledger, asset와 worker lease를 변경하지 않음
- [x] inbound W3C context를 추출하고 baggage/upstream injection을 차단함
- [x] metric labels가 stable low-cardinality allowlist만 사용함
- [x] prompt/body/header/query/raw URL/error, credentials, tenant identity와 금액이 telemetry에 없음
- [x] logs가 active trace/span ID와 상관되며 secret redaction을 유지함
- [x] shutdown force flush가 timeout 안에서 완료됨
- [x] README와 Cloud handoff가 갱신됨
- [x] 전체 race/integration/CI 통과
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현: process-owned OTLP HTTP/protobuf trace·metric runtime, W3C-only propagation, bounded typed recorder, trace-aware structured logger를 추가했다.
- 계측: OpenAI/Gemini HTTP, 인증·권한, routing rejection/selection, 실제 Provider dispatch, Billing transition, managed storage와 reconciliation worker를 연결했다.
- 보안: metric attribute allowlist가 임의 문자열을 `unknown`으로 축약하고 body/query/header/baggage/credential/tenant identity를 export하지 않는 테스트를 추가했다.
- 로컬 검증: `make check` 통과.
- 통합 검증: Compose PostgreSQL/Redis에서 `make integration-test` 통과.
- PR: https://github.com/nativegatewayhq/gateway/pull/27

## Rollback 계획

- `GATEWAY_TELEMETRY_MODE=disabled`로 recorder/exporter를 no-op으로 전환한다.
- PostgreSQL migration과 durable state가 없으므로 이전 binary로 즉시 rollback할 수 있다.
- collector 장애 시 traffic을 유지하고 exporter queue는 bounded shutdown 이후 폐기한다.
- telemetry를 제거해도 request ID structured logging, readiness와 billing/storage reconciliation은 계속 동작한다.

## 후속 작업

- managed Cloud Grafana dashboards와 SLO alerts
- tail sampling과 exemplars
- customer-safe usage analytics pipeline
- LLM token/streaming 및 async job telemetry
- continuous profiling
