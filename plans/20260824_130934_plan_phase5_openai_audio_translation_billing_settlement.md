---
id: gateway-20260824-059
title: Phase 5 OpenAI Audio Translation Duration Pricing and Settlement
status: accepted
created_at: 2026-08-24T13:09:34+09:00
updated_at: 2026-08-24T13:09:34+09:00
owners:
  - gateway
initiative: phase-5-openai-audio-translation-billing-settlement
depends_on:
  - gateway-20260820-009
  - gateway-20260820-021
  - gateway-20260820-022
  - gateway-20260820-023
  - gateway-20260820-028
  - gateway-20260824-057
  - gateway-20260824-058
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 OpenAI Audio Translation Duration Pricing and Settlement

## 목적

OpenAI audio translation의 immutable 분당 가격과 검증된 최대 duration을 publication하고, Provider dispatch 전에 Wallet·cost quota·Provider spend cap을 예약한 뒤 native `verbose_json.duration` 증거로 exactly-once Capture·차액 Release·Reconciliation하는 관리형 정산 경로를 구현한다.

## 배경

Plan 058은 공식 SDK 호환 BYOK translation을 제공하지만 관리형 과금은 검증 가능한 청구량이 없어 fail closed한다. 현재 공식 translation SDK 계약에서 `verbose_json`만 입력 audio의 duration을 응답 필드로 제공하고, 기본 JSON·text·SRT·VTT에는 해당 수량이 없다. translation은 현재 `whisper-1`의 분당 가격 모델이므로 compressed file byte 수나 Gateway가 추정한 재생 시간을 비용 근거로 사용해서는 안 된다.

관리형 dispatch는 요청의 native response format을 몰래 변경하지 않는다. 사용자가 `verbose_json`을 요청하고 활성 duration price의 상한 안에서 검증된 duration을 반환하는 경우에만 자동 정산한다. 다른 응답 형식은 BYOK에서 계속 지원하되 billing-required에서는 Provider 호출 전에 명시적으로 거부한다.

참조 계약:

- OpenAI Audio Translation `verbose_json` response의 `duration`
- OpenAI Audio API translation per-minute pricing
- `gateway-20260820-009` append-only Wallet/Ledger
- `gateway-20260820-021` hierarchical cost quota
- `gateway-20260820-022` Provider channel spend cap
- `gateway-20260820-023` exact settlement and reconciliation
- `gateway-20260824-057` transcription typed duration settlement
- `gateway-20260824-058` native translation multipart foundation

## 범위

- immutable `audio_translation_prices`와 publication history
- 단일 `openai-translation-duration-v1` strategy와 extractor version
- 분당 USD ticks와 maximum duration milliseconds의 checked integer estimate/actual 계산
- translation charge identity, request fingerprint와 append-only events
- Provider dispatch 전 Wallet·hierarchical quota·channel spend cap 원자적 예약
- native `verbose_json.duration`의 duplicate-safe bounded decimal extraction
- valid duration의 exactly-once Capture와 예약 차액 Release
- known non-2xx Release, uncertain transport와 invalid/missing/over-bound duration Reconciliation
- idempotency key concurrent deduplication과 terminal redispatch 방지
- translated text/audio/prompt/filename을 저장하지 않는 content-free duration evidence
- price publication/estimate/inspect CLI, readiness, models visibility와 bounded worker
- 공식 Python·JavaScript SDK managed translation/fault conformance

## 제외 범위

- compressed audio byte 수, MIME, codec metadata 기반 duration 추정
- Gateway-side codec decode, ffprobe 또는 waveform inspection
- default JSON, text, SRT, VTT response의 관리형 과금
- Provider usage report를 개별 request에 임의 연결
- token pricing 또는 cross-provider translation routing/fallback
- translated result, audio 또는 Provider response body 저장·replay
- 관리형 audio asset storage/CDN
- realtime/batch translation Job
- Cloud·Dashboard·Conformance 저장소 내부 구현

## 핵심 결정

### 1. `verbose_json.duration`만 actual quantity로 인정한다

- extractor는 top-level JSON object의 정확히 하나인 finite decimal `duration`을 요구한다.
- decimal seconds는 binary float 없이 millisecond로 올림 정규화한다.
- duplicate key, string/null/bool, exponent, negative/zero, 과도한 소수 자릿수와 overflow를 거부한다.
- transcript text, language, segments와 other fields는 정산 입력으로 읽거나 저장하지 않는다.

### 2. 예약은 publication의 maximum duration으로 계산한다

- active price는 model/channel별 검증된 `maximum_duration_milliseconds`를 필수로 가진다.
- maximum cost/sale/margin을 checked integer ceiling arithmetic으로 계산한다.
- 실제 duration이 상한을 넘으면 추가 자동 인출하지 않고 reservation을 유지한 채 manual reconciliation으로 보낸다.

### 3. native response format을 과금 때문에 변형하지 않는다

- billing-required 요청은 명시적 `response_format=verbose_json`만 허용한다.
- 기본값 JSON이나 text/SRT/VTT는 Provider 호출 전에 `unsupported_billing_response_format`으로 거부한다.
- Gateway는 duration을 삽입하거나 response schema를 다시 인코딩하지 않고 settlement 성공 후 원본 bytes를 반환한다.

### 4. 불명확한 Provider 결과는 자동 환불하지 않는다

- 명시적 Provider non-2xx는 알려진 실패로 Release한다.
- timeout/reset/cancel/panic, response loss/oversize, invalid/missing/over-bound duration과 settlement commit 실패는 reservation을 유지하고 reconciliation한다.
- valid response와 duration이 확보된 뒤 client write가 실패해도 Capture는 유지한다.
- facade와 worker는 Provider audio translation을 재호출하지 않는다.

### 5. 번역 결과를 idempotency replay용으로 저장하지 않는다

- request fingerprint는 operation, route, canonical form option과 raw audio digest로 만들고 원문을 저장하지 않는다.
- terminal idempotency key 재사용은 translated result replay 대신 bounded conflict를 반환한다.
- evidence에는 duration milliseconds, response status, safe header projection, body SHA-256와 extractor version만 저장한다.

## 설계 및 구현 순서

### 1. Pricing domain과 publication CLI

- translation duration price/publication table과 append-only/interval guard migration을 추가한다.
- `USD_TICKS` per minute와 maximum duration milliseconds를 immutable하게 고정한다.
- margin, currency, interval overlap, overflow와 publication-key idempotency를 검증한다.
- `gateway-audio-price -operation translation`의 publish/estimate/inspect를 추가한다.

### 2. Charge와 원자적 reservation

- tenant/request/key/model/channel/price/maximum duration/cost/sale/fingerprint를 immutable charge로 저장한다.
- Wallet, quota와 spend cap을 기존 lock ordering으로 한 transaction에서 예약한다.
- organization+idempotency advisory lock과 fingerprint가 concurrent duplicate의 단일 charge/dispatch를 보장한다.

### 3. Duration extraction과 facade state machine

- bounded duplicate-aware JSON scanner가 top-level duration raw token만 추출한다.
- request capability/price/idempotency 확인과 reservation 뒤 Provider에 정확히 한 번 dispatch한다.
- valid duration은 actual cost/sale을 계산해 Capture하고 차액을 Release한다.
- known non-2xx는 Release하고 uncertain/invalid outcome은 content-free evidence와 함께 Reconciliation한다.

### 4. Reconciliation, readiness와 운영

- append-only duration evidence/event와 lease/backoff/manual task를 추가한다.
- complete valid evidence가 있는 task만 settlement를 재시도하고 Provider를 재호출하지 않는다.
- missing/invalid/over-bound evidence는 bounded attempts 후 manual review로 수렴한다.
- billing-required readiness와 `/v1/models`는 credential, channel, capability, active price와 reservation bound를 함께 검사한다.

## 인터페이스와 데이터 변경

### 공개 API

`POST /v1/audio/translations`의 native multipart와 response schema는 변경하지 않는다. Billing-required에서는 다음을 추가 요구한다.

```text
Idempotency-Key: <opaque key>
response_format=verbose_json
```

terminal key 재사용은 response replay 없이 원 charge 상태를 나타내는 bounded conflict를 반환한다.

### 내부 인터페이스

- `audiopricing.PublishTranslation/EstimateTranslation/CalculateTranslation`
- `audiobilling.BeginTranslation/CompleteTranslation/ReleaseTranslation/MarkTranslationReconciling`
- `TranslationDurationEvidence{SchemaVersion, DurationMilliseconds, Status, Headers, SHA256}`
- translation reconciliation worker/settler

### 데이터베이스 및 migration

- `audio_translation_prices`, `audio_translation_price_publications`
- `audio_translation_charges`
- `audio_translation_duration_evidence`
- `audio_translation_charge_events`
- `audio_translation_reconciliations`
- cost quota/spend-cap allocation의 translation charge type 확장

모든 price/publication/evidence/event는 update/delete를 거부하며 charge identity와 terminal amount는 immutable하다. translated text, segments, language, audio, prompt, filename과 response body 컬럼은 만들지 않는다.

### 설정

- global `GATEWAY_BILLING_MODE=required`에 translation settlement를 연결한다.
- active price와 maximum duration은 PostgreSQL publication으로 관리한다.
- translation reconciliation은 기존 audio worker interval/lease/backoff 설정을 재사용한다.

### 다른 저장소에 제공하거나 요구하는 계약

- `cloud`: verified channel/model duration price, maximum duration과 worker 설정 배포
- `dashboard`: duration, cost/sale/margin, charge/reconciliation 상태 표시; prompt/result/input metadata 비노출
- `conformance`: managed Python·JavaScript verbose JSON, duplicate/missing/over-bound duration, non-2xx와 disconnect no-redispatch fixture

각 저장소는 `phase-5-openai-audio-translation-billing-settlement` initiative로 독립 local plan을 소유한다.

## 보안 및 과금 고려사항

- audio, filename, prompt, language, segments와 translated text를 billing DB·log·trace·metric label에 기록하지 않는다.
- response digest는 결과 복구/replay 용도가 아니며 body는 저장하지 않는다.
- decimal duration과 금액 계산에 binary floating point를 사용하지 않는다.
- price, route, maximum duration과 request fingerprint를 dispatch 전에 고정한다.
- actual duration이 상한을 넘을 때 Wallet을 음수로 만들거나 후행 추가 과금하지 않는다.
- duplicate JSON key와 response content-type mismatch를 fail closed한다.
- management-disabled BYOK translation의 기존 native 동작을 보존한다.

## 테스트 계획

### 단위 테스트

- duration exact decimal, millisecond ceil, duplicate/missing/string/null/bool/exponent/negative/zero/overflow
- per-minute checked integer ceil, maximum estimate, margin과 overflow
- publication-key idempotency, interval overlap와 immutable mutation guard
- request fingerprint, response-format fail-closed와 terminal key conflict
- safe header projection, response digest와 content-free evidence

### 통합 테스트

- concurrent Begin의 단일 charge/Wallet·quota·spend-cap reservation
- actual Capture와 차액 Release의 exactly-once projection
- known failure Release와 uncertain/invalid/over-bound manual reconciliation
- migration 적용·반복, append-only guard와 tenant isolation
- worker lease recovery와 restart 후 convergence
- transcription/speech/image/chat/video billing 회귀

### 호환성 및 장애 테스트

- Python·JavaScript SDK managed `verbose_json` translation과 native text integrity
- default JSON/text/SRT/VTT managed pre-dispatch rejection과 BYOK 호환
- Provider 400/401/429/5xx, timeout/reset/cancel/panic, invalid/oversize JSON
- valid evidence 후 client disconnect와 settlement failure
- concurrent duplicate `Idempotency-Key`가 Provider·Ledger를 재실행하지 않음

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [ ] duration price와 maximum reservation bound가 immutable하게 publication됨
- [ ] native `verbose_json.duration`만 actual translation quantity와 비용으로 인정됨
- [ ] dispatch 전에 Wallet·quota·spend cap이 한 transaction에서 예약됨
- [ ] valid duration이 exactly-once Capture되고 예약 차액이 반환됨
- [ ] known non-2xx는 Release되고 uncertain/invalid/missing duration은 Reconciliation됨
- [ ] over-bound duration이 추가 인출 없이 manual review로 수렴함
- [ ] duplicate idempotency 요청이 Provider·Ledger를 재실행하지 않음
- [ ] non-verbose managed format은 pre-dispatch fail closed되고 BYOK 호환은 유지됨
- [ ] audio/prompt/filename/result/credential이 DB·log·telemetry에 노출되지 않음
- [ ] translation result body를 저장하거나 replay하지 않음
- [ ] readiness, models visibility, CLI와 worker가 active translation price를 반영함
- [ ] 전체 unit/race/integration/SDK 검사가 통과함
- [ ] README, migration과 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- active translation price publication 또는 billing-required translation model enablement를 제거해 신규 managed dispatch를 중단한다.
- worker는 기존 reservation이 captured/released/manual 상태로 수렴할 때까지 유지한다.
- append-only price, charge, duration evidence와 event는 감사 목적으로 보존한다.
- BYOK translation은 Plan 058 native 경로로 계속 제공한다.

## 후속 작업

- managed audio input storage and reusable asset references
- realtime transcription WebSocket and batch transcription Jobs
- cross-provider STT/translation routing and fallback
