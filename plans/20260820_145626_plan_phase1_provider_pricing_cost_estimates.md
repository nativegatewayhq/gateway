---
id: gateway-20260820-010
title: Phase 1 Provider Pricing and Request Cost Estimates
status: completed
created_at: 2026-08-20T14:56:26+09:00
updated_at: 2026-08-20T15:07:29+09:00
owners:
  - gateway
initiative: phase-1-provider-pricing
depends_on:
  - gateway-20260820-007
  - gateway-20260820-009
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Provider Pricing and Request Cost Estimates

## 목적

Provider channel별 원가와 고객 판매가를 버전이 있는 가격표로 관리하고, 이미지 요청을 공급자 호출 전에 보수적으로 견적한다. 모든 금액은 Wallet과 동일한 `USD_TICKS` 정수로 계산하며 최소 마진을 위반하거나 가격이 불명확한 요청은 과금 실행 전에 차단한다.

## 배경

`gateway-20260820-007`은 모델과 capability를, `gateway-20260820-009`는 금액 예약과 정산 상태 머신을 제공한다. 두 경계를 연결하려면 먼저 어떤 Provider channel의 어떤 모델·작업·옵션이 얼마인지, 요청 전 최대 예약액을 어떻게 결정하는지 독립적으로 확립해야 한다. 가격표 없이 inference handler에서 임의 계산하면 가격 변경 이력, 재현 가능한 정산과 최소 마진 정책을 보장할 수 없다.

## 범위

- Provider channel registry와 활성 상태
- 모델·operation·가격 차원을 식별하는 versioned price records
- 원가와 판매가를 `USD_TICKS`로 저장
- `effective_from`/`effective_until` 가격 유효 구간
- 이미지 생성·편집의 요청 전 최대 비용 견적
- 모델, operation, 수량, size, quality 기반 exact price dimension
- 가격 선택 시점과 price record ID를 포함한 immutable estimate
- 판매가가 원가 및 최소 마진 조건을 만족하는지 검증
- 가격 없음, 만료, 모호한 중복, 지원하지 않는 옵션의 typed error
- overflow-safe 정수 곱셈과 단위 테스트
- PostgreSQL schema·조회·동시성 통합 테스트
- Cloud가 가격을 게시하기 위한 내부 repository 계약

## 제외 범위

- 공개 가격 관리 HTTP API와 Dashboard
- 결제, Deposit 또는 관리자 Adjustment
- inference handler의 Wallet Reserve/Capture/Release 연결
- Provider 응답에서 실제 usage를 추출하는 정산
- 동적 lowest-cost routing과 fallback
- 외부 가격 사이트 scraping과 자동 가격 수집
- 토큰, 영상 초, 음성 초 또는 storage byte 가격
- 할인 bucket, 쿠폰, 세금과 다중 통화

## 설계 및 구현 순서

### 1. Provider channel

- `provider_channels`는 channel ID, Provider, 표시명, 상태와 timestamps를 가진다.
- 동일 Provider에 여러 credential/channel을 추가할 수 있도록 모델과 channel을 분리한다.
- 가격 견적에는 active channel만 사용할 수 있다.
- credential 원문이나 secret reference는 가격 schema에 저장하지 않는다.

### 2. 가격 차원과 버전

- `provider_prices`는 channel, protocol, operation, model, size, quality, 단위 수량, 원가/판매가, 유효 구간을 가진다.
- wildcard fallback을 사용하지 않고 요청 옵션과 정확히 일치하는 가격만 선택한다.
- 옵션이 없는 차원은 빈 문자열이 아니라 명시적인 canonical 값 `default`로 저장한다.
- 게시된 row는 수정하지 않는다. 가격 변경은 새로운 row와 유효 구간으로 표현한다.
- 동일 selector의 유효 구간이 겹치지 않도록 PostgreSQL exclusion constraint 또는 transaction 검증으로 강제한다.

### 3. Estimate 입력

- 공통 입력은 Protocol, Operation, Model, Channel, Quantity, Size, Quality와 evaluation time이다.
- Quantity는 이미지 생성의 `n`이며 생략 시 1이다. 편집도 초기에는 출력 이미지 수 1을 기본으로 한다.
- Protocol parser가 Provider payload에서 필요한 가격 차원만 추출하는 계약을 제공하되 prompt, image bytes와 URL을 저장하지 않는다.
- 지원하지 않거나 상한을 넘는 수량과 canonicalize할 수 없는 옵션은 Provider 호출 전에 거부한다.

### 4. 정수 비용 계산

- 단가와 합계는 Wallet의 `ledger.Currency == USD_TICKS`를 공유한다.
- `estimated_cost = unit_cost * quantity`, `reserved_sale = unit_sale * quantity`로 계산한다.
- 곱셈 전 overflow를 검사하며 float와 암묵적 반올림을 금지한다.
- 반환 Estimate에는 price ID, channel ID, 평가 시각, quantity, estimated cost와 maximum sale을 포함한다.

### 5. 마진 정책

- 각 price record는 `unit_sale >= unit_cost`를 만족해야 한다.
- 전역 최소 마진은 basis points로 표현하고 `(sale-cost)/sale` 기준으로 검증한다.
- 정수 나눗셈 오차를 피하도록 교차 곱을 사용하고 overflow-safe 비교를 적용한다.
- 최소 마진 미달 channel은 `ErrMarginViolation`으로 견적 대상에서 제외한다.

### 6. 가격 게시 계약

- repository의 Publish는 selector와 유효 구간을 검증하고 append-only row를 생성한다.
- 과거 가격 row UPDATE/DELETE를 DB trigger로 거부한다.
- Cloud는 검증된 관리자 입력이나 가격 수집 결과를 Publish에 전달한다.
- 동일 publication key retry는 같은 record를 반환하고 다른 payload 재사용은 conflict다.

### 7. 감사와 보안

- 견적은 선택한 price ID로 재현할 수 있다.
- 로그에는 price/channel ID와 오류 분류만 남기며 prompt, 파일, credential과 전체 요청 body를 기록하지 않는다.
- 고객 응답에 Provider 원가와 내부 마진을 노출하는 공개 API는 이번 범위에 포함하지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

없음.

### 내부 인터페이스

```go
type Request struct {
    Protocol, Operation, Model, ChannelID string
    Quantity int64
    Size, Quality string
    At time.Time
}

type Estimate struct {
    PriceID, ChannelID, Currency string
    Quantity, EstimatedCost, MaximumSale int64
    EvaluatedAt time.Time
}

Estimate(ctx context.Context, request Request) (Estimate, error)
Publish(ctx context.Context, price Price, publicationKey string) (Price, error)
```

### 데이터베이스 및 migration

forward-only `000004_provider_pricing.sql`에 `provider_channels`, `provider_prices`, `price_publications`를 추가한다. 기존 binary는 새 table을 무시할 수 있다. 게시된 가격은 삭제 rollback하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 initiative `phase-1-provider-pricing`에서 인증·감사된 가격 게시 UI/API를 소유한다. Gateway는 Publish/Estimate domain 계약과 schema를 소유하며 Cloud가 DB row를 직접 수정하는 방식은 허용하지 않는다.

## 보안 및 과금 고려사항

- 가격 선택은 client가 보낸 Provider 원가나 판매가를 신뢰하지 않는다.
- 요청 시각을 임의로 과거로 지정하는 공개 경로를 만들지 않는다.
- 견적 이후 가격이 변경돼도 저장된 price ID와 금액을 후속 예약·정산이 사용한다.
- 가격 없음과 channel 비활성 오류는 credential 존재 여부를 노출하지 않는다.
- publication key와 selector는 길이·문자 제한을 적용하고 SQL parameter로만 사용한다.
- 모든 금액 계산은 overflow 시 fail closed한다.

## 테스트 계획

### 단위 테스트

- canonical selector와 default option 처리
- quantity 기본값·상한·음수 거부
- int64 곱셈 overflow
- minimum margin 경계와 1 basis point 미달
- typed error가 내부 가격이나 credential을 포함하지 않음

### 통합 테스트

- active channel의 현재 가격 exact lookup
- 미래·만료 가격 제외
- inactive channel 거부
- selector별 연속 가격 버전 전환
- 겹치는 유효 구간과 publication conflict 거부
- 동일 publication retry의 단일 row effect
- 게시 가격 UPDATE/DELETE 거부
- migration 반복·동시 실행

### 호환성 테스트

- OpenAI image generation의 `n`, `size`, `quality` 추출
- OpenAI multipart edit의 model/size/quality 추출 시 body와 파일 미보관
- xAI JSON image generation/edit selector 추출
- Gemini generateContent는 가격 dimension 계약이 확정되기 전 명시적 unavailable 처리

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

- [x] Provider channel과 append-only versioned 가격 schema가 존재함
- [x] 현재 시점의 active exact price만 결정적으로 선택됨
- [x] 원가·판매가·합계가 정수 USD_TICKS로만 계산됨
- [x] 수량 계산과 margin 비교에서 overflow가 차단됨
- [x] 최소 마진 미달 가격이 견적되지 않음
- [x] 동일 publication retry가 중복 가격을 만들지 않음
- [x] 유효 구간 중복과 publication 충돌이 거부됨
- [x] OpenAI/xAI 이미지 요청 가격 차원이 안전하게 추출됨
- [x] Estimate가 price/channel ID와 평가 시각을 보존함
- [x] 가격 row UPDATE/DELETE가 DB에서 거부됨
- [x] 전체 race/integration/CI 통과
- [x] README와 Cloud 게시 계약이 기록됨
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 계획 commit: `4a674cb771ec49053126f93b002f95911a9bbe6a`
- 구현 commit: `72500f236071f9ccb7c6665341d951e527718e80`
- Pull Request: `https://github.com/nativegatewayhq/gateway/pull/10`
- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과
- `GOCACHE=/private/tmp/gateway-go-cache TEST_DATABASE_URL=postgres://gateway:***@127.0.0.1:55433/gateway?sslmode=disable make integration-test` 통과
- PostgreSQL integration에서 현재·미래·만료·연속 가격, exact selector, margin, channel 상태, overflow, overlap exclusion, append-only trigger와 concurrent publication 검증 통과
- OpenAI/xAI JSON 및 OpenAI multipart selector 단위 테스트와 Gemini unavailable 계약 검증 통과
- GitHub Actions `check` 통과: `https://github.com/nativegatewayhq/gateway/actions/runs/32338177797/job/96331667298`
- GitHub Actions `validate` 통과: `https://github.com/nativegatewayhq/gateway/actions/runs/32338177795/job/96331665791`

## Rollback 계획

- 가격 견적 사용을 중단하고 이전 binary로 rollback하되 가격 이력 table은 유지한다.
- 게시된 row를 수정·삭제하지 않고 잘못된 가격의 유효 종료와 교정 가격을 forward publish한다.
- inference 과금 연결 전 계획이므로 기존 Provider pass-through 동작에는 영향을 주지 않는다.

## 후속 작업

1. inference request reserve/capture/release 연결
2. request idempotency와 timeout reconciliation
3. priority/weighted/lowest-cost routing
4. 가격 관리 API와 Dashboard
5. Provider 실제 usage 기반 정산
