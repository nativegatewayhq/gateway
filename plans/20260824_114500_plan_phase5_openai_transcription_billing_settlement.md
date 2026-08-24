---
id: gateway-20260824-057
title: Phase 5 OpenAI Transcription Duration and Token Pricing Settlement
status: completed
created_at: 2026-08-24T11:45:00+09:00
updated_at: 2026-08-24T12:32:22+09:00
owners:
  - gateway
initiative: phase-5-openai-transcription-billing-settlement
depends_on:
  - gateway-20260820-009
  - gateway-20260820-021
  - gateway-20260820-022
  - gateway-20260820-023
  - gateway-20260820-028
  - gateway-20260823-055
  - gateway-20260823-056
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 OpenAI Transcription Duration and Token Pricing Settlement

## 목적

OpenAI transcription 모델의 typed Provider usage를 기준으로 duration 또는 input/output token 가격을 immutable하게 publication하고, 요청 전 Wallet·quota·Provider spend cap을 예약한 뒤 native JSON/SSE 응답의 검증된 usage로 exactly-once Capture·Release·Reconciliation하는 관리형 과금 경로를 구현한다.

## 배경

Plan 056은 공식 OpenAI SDK와 호환되는 native multipart transcription을 제공하지만 Billing-required 환경에서는 비용 단위가 검증되지 않아 모델 활성화를 거부한다. OpenAI transcription 응답은 모델에 따라 `usage.type=tokens`와 `usage.type=duration` 중 하나를 반환한다. token usage는 input/output과 audio/text input 세부량을 포함하고, duration usage는 audio seconds를 제공한다. 현재 공개 가격도 모델별 input/output token 가격과 분당 가격이 공존한다.

압축 파일 byte 수, MIME, codec metadata 또는 Gateway가 추정한 재생 시간을 Provider 청구량으로 사용하지 않는다. 가격 publication은 기대 usage variant와 보수적인 최대량을 고정하고, 실제 정산은 Provider response에 들어 있는 typed usage만 신뢰한다. usage가 없는 text/SRT/VTT 응답은 BYOK에서 계속 지원하지만 Billing-required dispatch는 거부한다.

참조 계약:

- [OpenAI Audio transcription response and typed usage](https://platform.openai.com/docs/api-reference/audio/verbose-json-object)
- [OpenAI transcription pricing](https://platform.openai.com/pricing)
- `gateway-20260820-009` append-only Wallet/Ledger
- `gateway-20260820-021` hierarchical cost quota
- `gateway-20260820-022` Provider spend cap
- `gateway-20260820-023` exact usage settlement and reconciliation
- `gateway-20260823-056` native transcription multipart foundation

## 범위

- immutable `audio_transcription_prices`와 publication history
- `token` 및 `duration` pricing strategy와 extractor version
- input/output token 또는 duration millisecond의 checked integer estimate/actual 계산
- publication별 maximum input/output tokens 또는 maximum duration reservation bound
- model/channel/usage variant/response format/capability 교차 검증
- transcription charge identity, request fingerprint와 append-only events
- Provider dispatch 전 Wallet·cost quota·channel spend cap 원자적 예약
- native JSON 및 capability-enabled SSE의 strict typed usage extraction
- verified 2xx usage의 Capture와 예약 차액 Release
- known non-2xx Release, uncertain outcome와 invalid/missing usage Reconciliation
- idempotency key concurrent deduplication과 terminal redispatch 방지
- transcript/audio/prompt/filename을 저장하지 않는 content-free usage evidence
- 가격 publication/estimate CLI, readiness, bounded telemetry와 worker
- 공식 Python·JavaScript SDK billing/fault conformance

## 제외 범위

- compressed audio byte 수 기반 duration/token 추정
- Gateway-side codec decode, ffprobe 또는 waveform duration 계산
- usage가 없는 text, SRT, VTT response의 관리형 과금
- Provider 조직 usage report를 개별 request usage로 임의 연결
- `/v1/audio/translations`
- cross-provider STT routing/fallback
- audio 또는 transcript 영구 저장과 response replay
- realtime transcription WebSocket과 batch transcription Job
- Dashboard UI 및 Cloud 배포 구현

## 핵심 결정

### 1. 가격 strategy와 Provider usage variant를 정확히 일치시킨다

- 허용 strategy는 `openai-transcription-token-v1`과 `openai-transcription-duration-v1`이다.
- token strategy는 `usage.type=tokens`의 nonnegative integer `input_tokens`, `output_tokens`, `total_tokens`와 `input_token_details.audio_tokens/text_tokens`를 검증한다.
- duration strategy는 `usage.type=duration`의 bounded decimal seconds를 정수 millisecond로 올림 정규화한다.
- publication strategy와 실제 usage type이 다르면 자동 환산하지 않고 reconciliation한다.

### 2. 예약량은 파일에서 추정하지 않고 publication의 검증된 상한이다

- token publication은 maximum input/output tokens, duration publication은 maximum duration milliseconds를 필수로 가진다.
- capability와 Provider 계약에서 검증한 상한만 publication할 수 있다.
- 최대 원가·판매가·margin을 checked integer arithmetic으로 계산해 Provider 호출 전에 예약한다.
- 실제 usage가 상한을 넘으면 추가 자동 인출하지 않고 reservation을 유지한 채 manual reconciliation으로 보낸다.

### 3. usage를 보존하는 native 응답만 관리형으로 허용한다

- non-stream Billing-required 요청은 usage object를 반환하는 JSON 계열 response format만 허용한다.
- SSE는 terminal usage event의 정확한 단일 출현과 `[DONE]`/terminal event를 검증할 수 있는 capability와 extractor가 있을 때만 허용한다.
- text/SRT/VTT 또는 usage 없는 JSON/SSE는 Provider 호출 전에 거부하거나, 이미 호출된 경우 reservation을 임의 환불하지 않고 reconciliation한다.
- Gateway는 usage를 응답에 삽입하거나 native transcript schema를 변형하지 않는다.

### 4. transcript 없는 content-free evidence만 원장에 남긴다

- usage type, integer quantities, response status, safe header projection, body/stream SHA-256와 extractor version만 저장한다.
- response digest는 transcript 복구나 replay 용도가 아니며 body는 DB에 저장하지 않는다.
- audio bytes, filename, prompt, language hints, transcript, segment와 Provider credential/request ID를 charge·event·log·metric label에 기록하지 않는다.

### 5. 불명확한 Provider 원가는 자동 환불하지 않는다

- Provider의 명시적 non-2xx는 dispatch 결과가 알려진 실패이므로 Release한다.
- timeout/reset/panic, response loss, invalid/missing usage, client disconnect 전 terminal usage 미관측과 settlement commit 실패는 예약을 유지하고 reconciliation한다.
- complete valid usage가 확보된 뒤 client write가 실패해도 Provider 원가는 확정되므로 Capture 결과를 유지한다.
- facade와 reconciliation worker 모두 Provider를 재호출하지 않는다.

## 설계 및 구현 순서

### 1. Pricing domain과 publication CLI

- strategy별 typed price row, publication, effective interval과 append-only guard migration을 추가한다.
- token rate는 million-token, duration rate는 minute 단위 USD ticks로 저장하고 모든 계산은 ceil한다.
- 단일 model/channel/strategy 활성 가격과 기간 겹침 방지, 최소 마진, currency와 overflow를 검증한다.
- `gateway-audio-price`에 transcription publish/estimate/inspect 명령을 추가한다.

### 2. Charge와 reservation

- tenant/request/key/model/channel/price/strategy/maximum quantities/cost/sale/fingerprint를 immutable charge identity로 저장한다.
- Wallet, hierarchical quota와 Provider spend cap을 기존 ordering으로 한 transaction에서 예약한다.
- advisory lock과 idempotency fingerprint로 concurrent duplicate가 하나의 charge와 Provider dispatch만 만들게 한다.

### 3. JSON/SSE usage extraction

- bounded JSON parser가 duplicate key, exponent/fraction token quantity, negative/overflow, total mismatch와 detail mismatch를 거부한다.
- duration seconds는 float 없이 bounded decimal parser로 millisecond ceil 변환한다.
- SSE parser는 transcript event bytes를 그대로 relay하면서 terminal usage의 단일성과 complete terminal marker를 검증한다.
- usage body 자체를 보존하지 않고 typed evidence와 전체 response digest만 정산 서비스에 전달한다.

### 4. Facade settlement state machine

- authorization/capability/price/idempotency 확인과 reservation 이후 한 번만 Provider에 dispatch한다.
- complete valid usage는 actual cost/sale/margin을 계산해 Capture하고 예약 차액을 Release한다.
- known non-2xx는 native bounded response를 반환하면서 Release한다.
- invalid/oversize/truncated response, uncertain transport와 commit-after-dispatch 오류는 Reconciliation으로 전환한다.

### 5. Reconciliation과 운영

- append-only evidence/event와 lease/backoff/manual state를 추가한다.
- complete typed usage evidence가 있는 task만 settlement를 재시도한다.
- usage가 없거나 상한 초과·strategy mismatch인 task는 bounded retry 후 manual review로 수렴한다.
- readiness는 enabled model의 credential, capability, active price와 reservation bound를 함께 검사한다.

## 인터페이스와 데이터 변경

### 공개 API

`POST /v1/audio/transcriptions` native request/response는 변경하지 않는다. Billing-required 환경에서는 `Idempotency-Key`를 지원하고 usage를 보존하는 JSON/SSE 조합만 dispatch한다. terminal key 재사용은 transcript replay를 제공하지 않으며 원래 charge 상태를 나타내는 bounded conflict를 반환한다.

### 내부 인터페이스

- `audiopricing.PublishTranscription/EstimateTranscription`
- `audiopricing.TranscriptionUsage{Type, InputTokens, AudioInputTokens, TextInputTokens, OutputTokens, DurationMilliseconds}`
- `audiobilling.BeginTranscription/CompleteTranscription/ReleaseTranscription/MarkTranscriptionReconciling`
- `transcriptionUsageExtractor`의 JSON/SSE variant
- content-free `TranscriptionUsageEvidence{SchemaVersion, Usage, Status, Headers, SHA256}`

### 데이터베이스 및 migration

- `audio_transcription_prices`, `audio_transcription_price_publications`
- `audio_transcription_charges`, immutable reservation/actual amount와 state
- `audio_transcription_usage_evidence`, transcript 없는 typed usage와 digest
- `audio_transcription_charge_events`, append-only transition audit
- `audio_transcription_reconciliations`, lease/backoff/manual state
- cost quota와 spend-cap allocation의 transcription charge type 확장

### 설정

- global `GATEWAY_BILLING_MODE=required`에 transcription settlement를 연결한다.
- price와 reservation bound는 환경변수 JSON이 아니라 PostgreSQL publication으로 관리한다.
- worker interval/lease/backoff는 기존 audio reconciliation 설정을 재사용하거나 명시적 transcription 설정으로 분리한다.

### 다른 저장소 계약

- `cloud`: verified model/channel strategy, active publication, upper bound와 reconciliation worker 설정 배포
- `dashboard`: strategy, actual quantity, cost/sale/margin, charge/reconciliation 상태 표시; transcript와 input metadata 비노출
- `conformance`: Python·JavaScript JSON/SSE usage, duration/token variant, duplicate key, invalid/missing usage와 disconnect no-redispatch fixture

## 보안 및 과금 고려사항

- multipart audio와 transcript body는 billing DB 또는 로그에 복제하지 않는다.
- fingerprint는 canonical model/options와 raw request digest로 만들되 원문을 복구할 수 없게 한다.
- JSON duplicate field와 SSE duplicate terminal usage를 fail closed한다.
- decimal duration과 금액 계산에 binary floating point를 사용하지 않는다.
- price, usage strategy, route와 reservation upper bound는 dispatch 전에 고정한다.
- actual usage가 reservation을 넘을 때 Wallet을 음수로 만들거나 후행 추가 과금하지 않는다.
- response format을 몰래 JSON으로 바꾸지 않으며 usage가 없는 native format은 관리형에서 명시적으로 거부한다.

## 테스트 계획

### 단위 테스트

- token usage exact parse, detail/total mismatch, duplicate/missing/negative/fraction/overflow
- decimal seconds의 millisecond ceil, 최대 자릿수와 overflow
- million-token/minute integer ceil, margin과 maximum reservation
- strategy/usage/response format/capability mismatch
- JSON/SSE digest, terminal usage 단일성, truncation과 response bound
- idempotency fingerprint와 terminal replay conflict

### 통합 테스트

- concurrent Begin의 단일 charge/reservation/dispatch
- Capture/Release/Reconciliation의 Wallet·quota·spend cap exactly once
- migration append-only guard, tenant isolation과 active interval exclusion
- crash before/after Provider response, usage evidence와 Ledger commit
- missing usage, upper-bound excess와 strategy mismatch의 manual convergence
- 기존 Speech/image/chat/video billing 회귀

### SDK 및 장애 테스트

- Python·JavaScript native JSON token/duration usage와 transcript integrity
- capability-enabled SSE terminal usage와 client disconnect
- Provider 400/401/429/5xx, timeout/reset/panic, invalid JSON/SSE와 response oversize
- duplicate `Idempotency-Key`가 Provider와 Ledger를 재실행하지 않음
- text/SRT/VTT가 BYOK에서는 유지되고 Billing-required에서는 dispatch 전에 거부됨

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [x] token/duration strategy와 reservation upper bound가 immutable하게 publication됨
- [x] typed Provider usage만 actual quantity와 비용으로 인정됨
- [x] dispatch 전에 Wallet·quota·spend cap이 원자적으로 예약됨
- [x] valid JSON/SSE usage가 exactly-once Capture되고 차액이 반환됨
- [x] known non-2xx는 Release되고 uncertain/invalid/missing usage는 Reconciliation됨
- [x] actual usage 상한 초과가 자동 추가 인출 없이 manual review로 수렴함
- [x] duplicate idempotency request가 Provider·Ledger를 재실행하지 않음
- [x] audio/prompt/filename/transcript/credential이 DB·log·telemetry에 노출되지 않음
- [x] text/SRT/VTT의 관리형 dispatch가 fail closed되고 BYOK 호환은 유지됨
- [x] 전체 unit/race/integration/SDK 검사가 통과함
- [x] README, CLI, migration과 멀티레포 handoff가 갱신됨

## 검증 증거

- 구현 commit: `c3f9651` (`feat: settle OpenAI transcription usage billing`)
- Pull Request: [#83](https://github.com/nativegatewayhq/gateway/pull/83)
- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과: formatter, vet, 전체 race test와 모든 Gateway CLI build를 검증했다.
- `GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://gateway:gateway-local@127.0.0.1:55433/gateway_plan057?sslmode=disable' TEST_REDIS_URL='redis://127.0.0.1:56379/13' make integration-test` 통과: migration 51, 동시 예약·정산, Wallet·quota·spend cap, append-only guard, reconciliation/manual convergence와 기존 과금 회귀를 검증했다.
- `GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1` 통과: 공식 OpenAI Python 3.3.1 및 JavaScript 7.5.0 SDK의 native token/duration transcription 응답과 transcript 보존을 검증했다.
- JSON/SSE fault test에서 duplicate/missing/invalid usage, terminal 이전·이후 disconnect, timeout/reset/cancel/panic과 duplicate idempotency no-redispatch를 검증했다.
- migration과 저장 경로 감사에서 content-free typed usage, safe header projection, response SHA-256만 보존되고 audio/prompt/filename/transcript/credential 원문 컬럼이나 로그 필드가 없음을 확인했다.

## Rollback 계획

- Billing-required transcription model enablement 또는 active price publication을 제거해 신규 charge와 dispatch를 중단한다.
- reconciliation worker는 기존 reservation이 terminal/manual 상태로 수렴할 때까지 유지한다.
- append-only price, charge, event와 content-free usage evidence는 감사 목적으로 보존한다.
- migration의 permission과 charge type 확장은 호환성을 위해 유지한다.
- Billing-disabled BYOK transcription은 Plan 056 native 경로로 계속 제공한다.

## 후속 작업

- OpenAI Audio Translations native multipart foundation
- managed audio input storage and reusable asset references
- realtime transcription WebSocket and batch transcription Jobs
- cross-provider STT routing and fallback
