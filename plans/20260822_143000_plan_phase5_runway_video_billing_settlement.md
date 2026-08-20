---
id: gateway-20260822-051
title: Phase 5 Runway Video Credit Billing and Settlement
status: accepted
created_at: 2026-08-22T14:30:00+09:00
updated_at: 2026-08-22T14:30:00+09:00
owners:
  - gateway
initiative: phase-5-runway-video-billing-settlement
depends_on:
  - gateway-20260820-005
  - gateway-20260820-007
  - gateway-20260820-029
  - gateway-20260821-034
  - gateway-20260822-050
supersedes: []
affected_repos:
  - gateway
  - cloud
  - dashboard
  - conformance
---

# Phase 5 Runway Video Credit Billing and Settlement

## 목적

Runway native video task를 관리형 선불 크레딧 모드에서도 안전하게 사용할 수 있도록 요청 전 최대 판매가를 예약하고, terminal task가 보고한 실제 Runway credit 비용을 검증해 Capture 또는 Release하며, 불명확한 결과는 자동 추정하지 않고 reconciliation으로 보낸다.

## 배경

`gateway-20260822-050`은 공식 Runway SDK 호환, durable Job ID, polling/cancel과 BYOK dispatch를 제공하지만 billing-required mode는 미정산 task 생성을 막기 위해 submit 전에 fail closed한다. Phase 5를 실제 관리형 서비스로 확장하려면 장시간 영상 작업에도 기존 원장 불변 조건을 유지해야 한다.

Runway의 현재 공개 가격은 모델, 초당 길이, 해상도, audio 여부와 최소 과금에 따라 달라지며 자주 확장된다. terminal task wire는 최종 `cost.credits`를 제공하고 pending/running wire는 `estimatedCost.credits`를 제공한다. Moderation으로 실패한 task도 과금될 수 있으므로 `FAILED`를 무조건 전액 환불하면 안 된다. 따라서 요청 시점에는 immutable price snapshot으로 보수적인 최대 credits를 예약하고, terminal Provider evidence의 실제 credits를 기준으로 정산한다.

참조 계약:

- [Runway API pricing](https://docs.dev.runwayml.com/guides/pricing/)
- [Runway task response contract](https://github.com/runwayml/openapi)
- [Runway moderation billing](https://docs.dev.runwayml.com/api-details/moderation/)
- [Runway usage tiers](https://docs.dev.runwayml.com/usage/tiers/)

## 범위

- `video_credit_prices`와 immutable publication/version 모델
- logical model, Provider model, native task kind, duration, ratio/resolution, audio 조건 기반 최대 Runway credit estimator
- Runway credit 원가와 Gateway microcredit 판매가의 명시적 정수 환산
- `video_request_charges` 또는 modality-neutral async charge 확장을 통한 Reserve/Capture/Release
- Job 생성과 charge 예약의 동일 transaction 또는 복구 가능한 단일 operation identity
- terminal `cost.credits`의 bounded decimal parsing과 Provider evidence 저장
- `SUCCEEDED`, 유료 `FAILED`, 무료 실패, `CANCELLED`별 실제 비용 정산
- actual cost가 estimate 이하이면 차액 Release, estimate 초과이면 자동 초과 인출 금지와 manual reconciliation
- poll/cancel/submit response loss 이후 terminal 재조회와 exactly-once settlement
- 가격 유효기간, 최소 마진, Provider channel spend cap과 요청별 원가·판매가·마진 기록
- SDK native response를 변경하지 않는 route-independent idempotent replay
- 관리형 모드 활성화, readiness, telemetry, 관리 API용 bounded charge projection과 운영 문서

## 제외 범위

- Runway 가격을 문서에서 런타임 scraping하는 기능
- Model Router, video-to-video, upscale, avatar와 workflow 가격
- BYOK task에 대한 Gateway wallet 과금
- Provider 조직 잔액 자동 충전 또는 결제 수단 관리
- 대용량 upload proxy
- managed video download/storage/CDN
- cross-provider video routing과 fallback
- 대시보드 UI 구현 및 cloud 가격 배포 구현

## 핵심 결정

### 1. 가격은 versioned snapshot이며 요청이 참조한 버전은 불변이다

- 런타임은 외부 가격 문서를 호출하지 않는다.
- 각 가격 행은 Provider model, task kind, resolution class, audio flag, credits-per-second, fixed credits, minimum credits와 유효 기간을 가진다.
- duration과 ratio는 native body에서 정수/enum으로 추출하며 지원되지 않거나 가격이 없는 조합은 Provider 호출 전에 거부한다.
- 예약 원가·판매가·margin과 price publication ID를 charge 및 Job route evidence에 저장한다.

### 2. Provider credit과 Gateway money 단위를 분리한다

- Provider `credits`는 별도 정수 단위로 저장한다.
- 원가 환산율과 판매 정책을 동일 price snapshot에 포함하고 모든 계산은 checked integer arithmetic을 사용한다.
- 부동소수점으로 wallet 금액을 계산하지 않는다.
- Provider가 fractional credits를 반환할 수 있으므로 decimal 문자열을 bounded fixed-point 단위로 정규화한다.

### 3. 실패 상태가 아니라 실제 cost evidence가 환불을 결정한다

- `SUCCEEDED`와 moderation을 포함한 `FAILED` 모두 terminal `cost.credits`가 있으면 해당 실제 원가에 맞춰 Capture한다.
- terminal 응답에서 cost가 명시적으로 0이면 전액 Release한다.
- `CANCELLED`도 `cost.credits`를 우선하며 상태만 보고 0으로 가정하지 않는다.
- cost 누락, 음수, estimate 초과, schema 불일치는 charge를 예약 상태로 유지하고 manual reconciliation을 생성한다.

### 4. 외부 native wire와 내부 billing evidence를 분리한다

- `cost`, `estimatedCost`, output과 failure native body는 SDK에 그대로 투영한다.
- 원장 ID, 판매가, margin, publication ID와 내부 reconciliation 원인은 Provider response에 삽입하지 않는다.
- prompt, input URL, output URL과 Provider task ID는 billing/telemetry dimension에 기록하지 않는다.

## 설계 및 구현 순서

### 1. 가격 schema와 publication

- video price, publication, active interval과 append-only update guard migration을 추가한다.
- model/task kind/resolution/audio별 단일 활성 가격과 겹치는 기간 방지 constraint를 둔다.
- 관리 CLI가 validate-only와 publish를 지원하고 actor/reason/operation key를 감사한다.

### 2. 요청 비용 추정

- native top-level model, duration, ratio, audio를 content-free estimate evidence로 추출한다.
- model capability와 price capability를 교차 검증한다.
- fixed-point Provider credits, 원가, 판매가, 최소 마진과 overflow를 검증한다.

### 3. 예약과 Job 원자성

- API key 권한, idempotency fingerprint, price와 channel availability 확인 후 잔액을 예약한다.
- charge와 Job이 동일 request/idempotency identity로 exactly once 생성되도록 transaction 경계를 확장한다.
- 예약 성공 후 Job 생성 실패, 프로세스 종료와 duplicate submit을 reconciliation 가능한 상태로 남긴다.

### 4. terminal usage 추출과 정산

- Runway task의 `cost.credits`, status, Gateway/Provider identity를 검증해 immutable evidence로 저장한다.
- actual sale 계산 후 Capture/Release를 단일 ledger operation으로 처리한다.
- terminal snapshot 저장과 settlement claim은 재시작 및 중복 poll에서 멱등적이어야 한다.

### 5. 취소·불명확 결과 reconciliation

- DELETE timeout/404, submit response loss와 maximum poll exhaustion에서 wallet을 임의 환불하지 않는다.
- Provider task ID가 있으면 terminal 재조회하며 없으면 manual review로 수렴한다.
- estimate 초과 또는 price/evidence 불일치는 자동 추가 인출 없이 manual review와 운영 metric을 남긴다.

### 6. 관리 및 호환성

- `/v1/models`에는 가격과 credential이 모두 준비된 관리형 모델만 노출한다.
- 관리 Job/charge projection은 bounded 금액·상태만 제공한다.
- 공식 SDK native response와 idempotent replay bytes가 BYOK와 동일함을 검증한다.

## 인터페이스와 데이터 변경

### 설정

- `GATEWAY_RUNWAY_BILLING_MODE` 또는 기존 global billing mode에 연결되는 explicit enable policy
- `GATEWAY_RUNWAY_CREDIT_COST_MICROCREDITS`
- `GATEWAY_RUNWAY_MINIMUM_MARGIN_BPS`는 global minimum보다 낮을 수 없음
- 가격 자체는 환경변수 JSON이 아니라 PostgreSQL publication으로 관리한다.

### 내부 인터페이스

- `operations/video.Estimate(request, route, price) -> VideoEstimate`
- `providers/runway` terminal observation에 verified Provider credit usage
- async Job settlement가 image output quantity 외에 video Provider-credit dimension을 타입 안전하게 처리
- Job/charge 생성용 원자적 repository operation 또는 durable outbox 계약

### 데이터베이스 및 migration

- append-only `video_credit_prices`, `video_credit_price_publications`
- video charge/evidence 또는 기존 charge schema의 additive modality/dimension 확장
- Job에 estimate/actual Provider credit와 price publication identity
- terminal evidence uniqueness와 charge settlement state constraint
- 기존 BYOK Job과 Replicate/fal 정산은 null/additive column을 무시하고 계속 동작해야 한다.

### 멀티레포 계약

- `cloud`: 공개 가격과 별개인 조달 원가·판매 정책을 versioned publication으로 배포하며 secret 값을 공개 저장소에 넣지 않는다.
- `dashboard`: reserved/captured/released/manual 상태와 판매 금액만 표시하고 prompt, URI, output과 Provider task ID를 표시하지 않는다.
- `conformance`: 실제 HTTP 경계에서 동시 idempotency, charged failure, cancel cost, timeout/restart와 native SDK response를 검증한다.

## 보안 및 과금 고려사항

- 가격 publication은 승인된 actor와 operation key로만 변경하며 과거 요청의 가격을 소급 변경하지 않는다.
- request body에서 과금에 필요한 필드만 추출하고 prompt/media URI를 evidence나 로그에 저장하지 않는다.
- int64 overflow, fractional credit truncation, 음수/NaN/과도한 cost와 estimate 초과를 fail closed한다.
- balance row lock과 append-only ledger로 동시 요청에서도 잔액이 음수가 되지 않게 한다.
- 동일 Idempotency-Key의 body, route 또는 price identity가 달라지면 409이며 새 charge/task를 만들지 않는다.
- Provider timeout과 process crash는 성공/무료 실패로 추정하지 않고 예약을 유지해 reconciliation한다.
- Provider `FAILED`는 무료 실패가 아니며 verified `cost.credits` 없이 자동 Release하지 않는다.

## 테스트 계획

### 단위 테스트

- duration/ratio/audio와 model별 exact price selection
- fixed/minimum/per-second 및 fractional credit 계산, rounding와 overflow
- cost evidence의 succeeded/failed/cancelled/zero/missing/invalid/over-estimate 분류
- native response와 prompt/URI 비노출
- price active interval, margin과 publication validation

### 통합 테스트

- concurrent same-key submit의 단일 Reserve/Job/Provider task
- 잔액 부족과 price/margin/spend-cap 거부 시 pre-dispatch 보장
- terminal success, charged moderation failure, zero-cost cancel의 Capture/Release
- duplicate poll/delete와 worker restart의 exactly-once ledger
- submit/poll/cancel timeout, missing cost와 estimate 초과의 reconciliation
- migration upgrade와 기존 Replicate/fal async settlement 회귀

### SDK 및 장애 테스트

- 공식 Runway Python sync/async와 JavaScript SDK의 관리형 submit/retrieve/delete
- native `estimatedCost`/`cost`와 Gateway Job ID wire 보존
- Provider 429/5xx, response loss, malformed decimal, DB conflict와 worker crash
- BYOK mode 무과금 회귀

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/runway -count=1
```

## 완료 조건

- [ ] 관리형 Runway task가 Provider dispatch 전에 최대 판매가를 정확히 예약함
- [ ] 동시성과 idempotency 충돌에서도 단일 charge, Job과 Provider task만 생성됨
- [ ] terminal actual `cost.credits`로 성공·유료 실패·취소를 정확히 Capture/Release함
- [ ] 비용 누락·초과·불명확 timeout은 자동 환불/추가 인출 없이 reconciliation됨
- [ ] 가격 유효기간, 최소 마진과 channel spend cap이 pre-dispatch 적용됨
- [ ] BYOK와 공식 Runway SDK native wire 호환성이 유지됨
- [ ] prompt, input/output URL, Provider task ID와 credential이 billing/telemetry에 노출되지 않음
- [ ] 전체 unit/race/integration/SDK 회귀가 통과함
- [ ] README, migration, 운영 runbook과 멀티레포 handoff가 갱신됨

## 검증 증거

아직 구현 전.

## Rollback 계획

- 관리형 Runway billing enable policy를 비활성화해 신규 유료 submit을 즉시 fail closed한다.
- 이미 예약된 charge와 실행 중 Job은 worker/reconciliation이 terminal cost evidence를 확인할 때까지 유지한다.
- 가격 publication은 삭제하지 않고 새 종료 시각 또는 대체 publication으로 비활성화한다.
- additive schema와 과거 ledger/evidence는 감사 및 복구를 위해 보존한다.

## 후속 작업

- Runway bounded streaming upload와 ephemeral `runway://` asset proxy
- managed video download, object storage와 CDN
- Model Router와 추가 video operation 가격
- cross-provider video routing/fallback
- Speech/Transcription native foundation과 audio billing
