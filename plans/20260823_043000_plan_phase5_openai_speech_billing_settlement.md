---
id: gateway-20260823-055
title: Phase 5 OpenAI Speech Character Pricing and Settlement
status: completed
created_at: 2026-08-23T04:30:00+09:00
updated_at: 2026-08-23T06:20:00+09:00
owners:
  - gateway
initiative: phase-5-openai-speech-billing-settlement
depends_on:
  - gateway-20260820-009
  - gateway-20260820-021
  - gateway-20260820-022
  - gateway-20260820-023
  - gateway-20260820-028
  - gateway-20260823-054
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 OpenAI Speech Character Pricing and Settlement

## 목적

OpenAI Speech 요청 중 Provider 계약이 input character 단위로 확정된 모델을 대상으로 immutable 가격, Wallet 예약, streaming 결과 확정과 exactly-once Capture/Release/Reconciliation을 구현하여 Billing-required 관리형 서비스에서 안전하게 활성화한다.

## 배경

Plan 054는 BYOK native Speech와 bounded binary streaming을 제공하지만 Billing-required 모드에서는 Speech 모델을 fail closed한다. Speech API 성공 응답에는 일반적인 JSON `usage` 객체가 없고, 모델 세대에 따라 과금 단위가 input character, text/audio token 또는 출력 시간으로 다를 수 있다. 압축 audio byte 수를 token이나 duration으로 간주하면 원가가 틀리므로 하나의 추정식을 모든 모델에 적용하지 않는다.

이 계획은 첫 관리형 과금 범위를 `input_character` 가격 계약이 운영자가 검증한 모델로 제한한다. Unicode scalar count를 immutable request evidence로 사용하고, 다른 가격 단위 모델은 publication과 dispatch를 거부한다. 추후 Provider가 verified usage를 제공하거나 audio duration/token extractor가 구현되면 별도 strategy version으로 확장한다.

참조 계약:

- [OpenAI Audio API reference](https://platform.openai.com/docs/api-reference/audio)
- `gateway-20260820-009` append-only Wallet/Ledger
- `gateway-20260820-021` hierarchical cost quota
- `gateway-20260820-022` Provider spend cap
- `gateway-20260823-054` native Speech streaming foundation

## 범위

- immutable `audio_speech_prices`와 publication history
- `input_character` 단위 cost/sale rate 및 strategy/extractor version
- exact Unicode scalar quantity, bounded multiplication과 margin 검증
- organization Wallet, cost quota와 channel spend-cap 예약
- Speech charge identity, idempotency fingerprint와 append-only events
- Provider dispatch 전 최대 비용 예약
- non-2xx known failure Release 및 success stream 완료 후 Capture
- Provider timeout/reset/panic, invalid MIME/length, oversize와 client disconnect reconciliation
- streaming 중 byte content를 저장하지 않는 terminal digest/byte-count evidence
- retry 없이 idempotency state 조회 및 duplicate request 방지
- price publication CLI와 요청별 cost/sale/margin 조회 기반
- bounded billing/stream telemetry와 reconciliation worker
- 공식 SDK success/error/disconnect 회귀

## 제외 범위

- text/audio token, generated-second 또는 request-flat Speech 가격 strategy
- 압축 audio duration·codec parsing과 transcription
- Speech binary response replay 및 completed audio 저장
- managed audio S3/R2/CDN
- cross-provider TTS routing/fallback
- streaming response 재전송과 resumable download
- dashboard UI와 cloud deployment 구현

## 핵심 결정

### 1. 가격 단위는 publication에 명시하고 추측하지 않는다

- 최초 허용 strategy는 `openai-speech-input-character-v1` 하나다.
- quantity는 JSON `input`의 Unicode scalar count이며 UTF-8 byte 수나 grapheme 수와 혼용하지 않는다.
- cost/sale은 million-character rate를 정수 `USD_TICKS`로 ceil 계산한다.
- model/provider 계약이 token 또는 duration 기반이면 price publication과 Billing-required 활성화를 거부한다.

### 2. binary stream 완료가 Capture 경계다

- Provider 2xx header만으로 Capture하지 않는다.
- MIME, declared/actual length와 response byte bound를 만족하고 EOF까지 downstream에 기록했을 때 Capture한다.
- client write failure는 Provider가 생성에 성공했을 수 있으므로 Release하지 않고 reconciliation으로 보낸다.
- Provider의 명시적 non-2xx는 native bounded error를 snapshot하고 Release한다.

### 3. audio content 대신 content-free terminal evidence를 저장한다

- success evidence는 status, safe headers, streamed byte count와 SHA-256 digest만 가진다.
- input text, voice/custom voice ID, audio bytes와 Provider credential은 DB/log/telemetry에 저장하지 않는다.
- digest는 settlement identity 검증용이며 사용자가 audio를 재다운로드하는 수단이 아니다.

### 4. Speech idempotency는 redispatch 방지가 목적이다

- 완료 audio를 저장하지 않으므로 같은 `Idempotency-Key`로 binary를 replay하지 않는다.
- in-flight 또는 terminal key 재사용은 원래 charge 상태를 확인한 뒤 conflict를 반환하며 Provider를 다시 호출하지 않는다.
- 다른 fingerprint의 key 재사용은 명시적 conflict다.
- durable replay는 managed audio storage 후속 계획에서만 지원한다.

## 설계 및 구현 순서

### 1. Pricing domain과 CLI

- model/channel/strategy별 effective range를 가진 immutable price schema를 추가한다.
- million-character cost/sale, currency, publication key와 margin을 검증한다.
- `gateway-audio-price` publish/estimate 명령을 제공한다.

### 2. Speech charge와 reservation

- tenant/request/key/model/channel/price/quantity/cost/sale/fingerprint 상태를 immutable charge로 저장한다.
- Wallet, quota와 spend cap을 동일 transaction ordering으로 예약한다.
- concurrent identical request가 하나의 reservation/Provider dispatch만 생성하도록 advisory lock을 사용한다.

### 3. Streaming settlement evidence

- Plan 054 relay에 SHA-256와 exact byte count를 추가한다.
- complete EOF는 `CAPTURED`, known non-2xx는 `RELEASED`로 exactly once 수렴한다.
- commit 후 오류와 uncertain Provider outcome은 reservation을 유지한 `RECONCILING` 상태로 기록한다.

### 4. Reconciliation과 운영

- append-only task/event와 lease worker를 추가한다.
- terminal evidence가 충분한 task만 Capture/Release하고 불충분한 결과는 bounded retry 후 manual review한다.
- charge 조회와 bounded metric으로 reserved/reconciling/manual 비율을 관측한다.

## 인터페이스와 데이터 변경

### 공개 API

`POST /v1/audio/speech` native wire는 변경하지 않는다. Billing-required 모드에서는 `Idempotency-Key`를 지원하지만 성공 body에 Gateway metadata를 삽입하지 않는다. terminal key 재사용은 replay 불가 conflict로 응답한다.

### 내부 인터페이스

- `audiopricing.Publish/Estimate(input_character)`
- `audiobilling.Begin/Complete/Release/MarkReconciling`
- `speechStreamResult{Bytes, SHA256, Complete, WriteFailure}`
- `audiocharge.Repository`의 durable state/event/reconciliation lease

### 데이터베이스 및 migration

- `audio_speech_prices`, `audio_speech_price_publications`
- `audio_speech_charges`, immutable identity와 terminal evidence
- `audio_speech_charge_events`, append-only transition audit
- `audio_speech_reconciliations`, lease/backoff/manual state
- cost quota와 spend-cap allocation의 charge type 확장

### 다른 저장소 계약

- `cloud`: verified character-priced model publications, margin과 worker 설정 배포
- `dashboard`: Speech charge quantity/unit/cost/sale/margin 및 reconciliation 상태 표시; content 비노출
- `conformance`: official SDK success, non-2xx, disconnect, duplicate key와 no-redispatch 검증

## 보안 및 과금 고려사항

- input character quantity만 저장하고 원문 input은 저장하지 않는다.
- request fingerprint는 canonical operation/model/voice/options와 raw body digest를 사용하되 원문을 복원할 수 없게 한다.
- arithmetic은 overflow-safe integer ceil이며 floating point를 사용하지 않는다.
- price, route와 quantity는 Provider dispatch 전에 charge에 고정한다.
- stream write failure나 response loss는 고객 성공 전달 실패와 Provider 원가 발생 가능성을 동시에 보존해 자동 Release하지 않는다.
- unsupported pricing strategy는 운영자 override 없이 fail closed한다.

## 테스트 계획

### 단위 테스트

- Unicode scalar count, combining/emoji/NUL과 maximum input
- million-unit ceil, overflow, margin과 effective interval
- idempotency fingerprint와 terminal replay conflict
- streaming digest/count, short read, oversize, write failure 분류
- known failure Release와 uncertain reconciliation

### 통합 테스트

- concurrent Begin의 단일 reservation/charge
- Capture/Release/reconciliation의 Wallet·quota·spend-cap exactly once
- crash before/after Provider response, stream EOF, charge update와 Ledger commit
- append-only price/charge/event 및 tenant isolation
- 기존 image/chat/video billing migration 회귀

### SDK 및 장애 테스트

- Python·JavaScript native Speech success and binary integrity
- duplicate `Idempotency-Key`가 Provider를 재호출하지 않음
- Provider 400/401/429/5xx, timeout/reset/panic와 client disconnect
- missing/invalid MIME, declared-length mismatch와 body limit

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1
```

## 완료 조건

- [x] verified input-character price만 publish·dispatch할 수 있음
- [x] Unicode quantity와 integer cost/sale/margin이 정확하고 immutable함
- [x] Provider dispatch 전에 Wallet·quota·spend cap이 원자적으로 예약됨
- [x] complete audio stream만 exactly-once Capture됨
- [x] known non-2xx는 Release되고 uncertain/commit 후 실패는 reservation을 유지함
- [x] duplicate idempotency request가 Provider·Ledger를 재실행하지 않음
- [x] input/voice/audio/credential이 DB·log·telemetry에 노출되지 않음
- [x] 전체 unit/race/integration/SDK 검사가 통과함
- [x] README, CLI와 멀티레포 handoff가 갱신됨

## 검증 증거

- `GOCACHE=/private/tmp/gateway-go-cache make check`
- `TEST_DATABASE_URL=... TEST_REDIS_URL=... make integration-test`
- `GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai -count=1`
- 격리 DB `gateway_plan055`, Redis DB 12에서 migration 49 및 동시 Begin/Capture/Release/Reconciliation 검증
- `gateway-audio-price`, immutable character price, audio charge/event/reconciliation schema와 native Speech billing wiring 구현

## Rollback 계획

- Billing-required 환경의 Speech model enablement를 제거해 신규 charge와 dispatch를 중단한다.
- reconciliation worker는 기존 reservation이 terminal/manual 상태로 수렴할 때까지 유지한다.
- append-only 가격, charge, event와 digest evidence는 감사 목적으로 보존한다.
- BYOK Speech는 Billing-disabled 환경에서 계속 사용할 수 있다.

## 후속 작업

- OpenAI Audio Transcriptions native multipart foundation
- token/duration-priced Speech strategy와 verified usage extractor
- managed Speech audio storage, replay와 CDN/range delivery
- cross-provider TTS routing and fallback
