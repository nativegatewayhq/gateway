---
id: gateway-20260820-009
title: Phase 1 Wallet and Append-only Ledger Foundation
status: completed
created_at: 2026-08-20T14:42:59+09:00
updated_at: 2026-08-20T14:53:51+09:00
owners:
  - gateway
initiative: phase-1-wallet-ledger
depends_on:
  - gateway-20260820-008
supersedes: []
affected_repos:
  - gateway
  - cloud
---

# Phase 1 Wallet and Append-only Ledger Foundation

## 목적

조직별 선불 Wallet과 append-only Ledger를 만들고 Deposit, Reserve, Capture, Release, Refund를 PostgreSQL transaction으로 원자적·멱등적으로 처리한다. 동시 요청에서도 available balance가 음수가 되지 않고 projection을 ledger delta 합계로 감사할 수 있어야 한다.

## 배경

`gateway-20260820-008`에서 모든 요청을 organization/project에 귀속할 기반이 생겼다. Provider 호출에 과금을 연결하기 전에 금전 상태 머신과 동시성 불변 조건을 독립적으로 확립해야 한다. 단순 balance update만 사용하면 예약·실패·timeout과 retry를 감사하거나 복구할 수 없다.

## 범위

- organization wallet projection
- append-only ledger entries
- reservation aggregate
- Deposit, Reserve, Capture, Release, Refund
- 정수 `USD_TICKS` 통화 단위 (`1 USD = 10,000,000,000 ticks`)
- organization별 operation key 멱등성
- PostgreSQL row lock 기반 동시성 제어
- insufficient funds와 invalid transition typed errors
- ledger update/delete 방지 database trigger
- projection과 ledger delta reconciliation query/test
- domain service 단위·통합·동시성 테스트

## 제외 범위

- 결제 processor, 세금, invoice와 실제 현금 입금
- HTTP wallet 관리 API와 Dashboard
- Provider 가격표, 판매가, margin과 estimate
- inference handler의 자동 reserve/capture/release
- timeout reconciliation과 Provider usage 조회
- 다중 통화와 환율
- credit expiration, promotional bucket과 overdraft
- 관리자 adjustment와 chargeback

## 설계 및 구현 순서

### 1. 금액과 통화

- 금액은 `int64` 정수 ticks로만 표현하고 float를 금지한다.
- 초기 통화는 `USD_TICKS` 하나이며 1 USD는 10^10 ticks다.
- 모든 public domain operation은 양수 금액만 허용한다. Capture는 0을 허용해 전액 release 결과를 표현한다.
- 합산 overflow를 application과 database constraint에서 거부한다.

### 2. Schema

- `organization_wallets`: organization_id/currency PK, available, reserved, version, timestamps.
- `wallet_reservations`: id, organization_id, project_id, request_id, maximum, captured, refunded, state, timestamps.
- `ledger_entries`: id, organization/project/reservation, type, delta_available, delta_reserved, operation_key, metadata 최소값, created_at.
- wallet balance는 음수일 수 없고 reservation 금액 관계는 `0 <= captured <= maximum`, `0 <= refunded <= captured`를 강제한다.
- organization 내 request_id와 operation_key는 unique다.
- ledger row UPDATE/DELETE를 trigger로 거부한다.
- foreign key는 cascade delete하지 않는다.

### 3. Deposit

- active organization wallet을 없으면 생성한다.
- operation key가 처음이면 available을 증가시키고 Deposit ledger entry를 append한다.
- 같은 organization/key/동일 command retry는 기존 결과를 반환한다.
- 같은 key를 다른 type/amount에 재사용하면 idempotency conflict다.
- 실제 결제 검증은 Cloud 후속 계획이 수행한다.

### 4. Reserve

- organization wallet row를 `FOR UPDATE`로 잠근다.
- project가 요청 organization에 속하고 active인지 같은 transaction에서 검증한다.
- available < maximum이면 어떤 row도 변경하지 않고 insufficient funds를 반환한다.
- 성공 시 available 감소, reserved 증가, reservation PENDING 생성, Reserve ledger append를 원자적으로 수행한다.
- organization/request_id 및 operation key retry는 같은 reservation을 반환한다.

### 5. Capture와 Release

- reservation과 wallet을 일관된 순서로 잠근다.
- Capture actual은 maximum 이하여야 한다.
- reserved에서 maximum 전체를 제거하고 `maximum-actual`을 available로 반환한다.
- actual > 0이면 Capture entry, 차액 > 0이면 Release entry를 각각 append한다.
- reservation은 CAPTURED가 되고 captured 금액을 보관한다.
- 명시적 Release는 PENDING reservation 전체를 available로 돌리고 RELEASED로 전이한다.
- 완료 상태의 동일 operation retry는 같은 결과, 다른 operation은 invalid transition이다.

### 6. Refund

- CAPTURED reservation만 refund 가능하다.
- 누적 refund가 captured를 넘을 수 없다.
- available을 증가시키고 Refund entry를 append하며 reservation refunded를 증가시킨다.
- 부분 refund와 전액 refund를 지원하고 operation key retry를 멱등 처리한다.

### 7. 감사와 관측성

- wallet projection의 available/reserved는 해당 organization ledger delta 합과 일치해야 한다.
- 모든 command는 transaction commit 이후 결과만 반환한다.
- 로그에는 organization/project/request/reservation 원문을 기본적으로 넣지 않고 operation category와 성공/실패만 기록한다.
- operation key와 ledger metadata에 credential, prompt 또는 Provider payload를 넣지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

없음.

### 내부 인터페이스

```go
Deposit(ctx, organizationID, amount, operationKey) (Balance, error)
Reserve(ctx, organizationID, projectID, requestID, maximum, operationKey) (Reservation, error)
Capture(ctx, reservationID, actual, operationKey) (Reservation, Balance, error)
Release(ctx, reservationID, operationKey) (Reservation, Balance, error)
Refund(ctx, reservationID, amount, operationKey) (Reservation, Balance, error)
```

### 데이터베이스 및 migration

forward-only `000003_wallet_ledger.sql`. 이전 binary는 새 테이블을 무시할 수 있다. 금전 data가 생성된 뒤 schema 삭제 rollback은 금지한다.

### 다른 저장소에 제공하거나 요구하는 계약

Cloud는 initiative `phase-1-wallet-ledger`에서 검증된 payment event만 Deposit command로 전달하는 후속 계약을 소유한다.

## 보안 및 과금 고려사항

- 모든 command는 organization/project ownership을 DB에서 다시 검증한다.
- SQL parameter만 사용하고 operation key를 로그에 남기지 않는다.
- ledger는 update/delete 불가이며 correction은 후속 반대 entry로만 수행한다.
- insufficient funds와 idempotency conflict는 내부 balance나 다른 tenant 존재 여부를 노출하지 않는다.
- timeout 시 transaction 결과가 불명확하면 같은 operation key로 retry해 결과를 확인한다.
- 동일 논리 command는 operation key로 한 번만 금전 효과를 낸다.

## 테스트 계획

### 단위 테스트

- amount validation, overflow와 state transition
- typed error 분류와 secret-free message
- 동일/충돌 operation key semantics

### 통합 테스트

- Deposit→Reserve→Capture+차액 Release→부분/전액 Refund 전체 lifecycle
- Deposit→Reserve→Release failure lifecycle
- insufficient funds에서 row 변화 없음
- 동일 command 동시 retry의 단일 ledger effect
- 잔액보다 큰 복수 Reserve 경쟁에서 available 음수 방지
- 잘못된 tenant/project와 disabled tenant 거부
- ledger UPDATE/DELETE database 거부
- projection과 ledger delta 합 일치
- transaction rollback과 connection failure

### 호환성 및 장애 테스트

- migration 반복·동시 실행
- operation commit 후 client 응답 유실을 동일 key retry로 복구
- deadlock 없이 일관된 lock order
- int64 boundary와 overflow

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

- [x] wallet, reservation과 append-only ledger schema가 존재함
- [x] 금액이 정수 USD_TICKS로만 처리됨
- [x] Deposit/Reserve/Capture/Release/Refund lifecycle이 원자적임
- [x] 동일 operation retry가 이중 금전 효과를 만들지 않음
- [x] operation key 충돌이 명시적으로 거부됨
- [x] 동시 Reserve에서 available이 음수가 되지 않음
- [x] capture 차액과 실패 release가 정확히 available로 반환됨
- [x] refund가 captured 금액을 초과하지 않음
- [x] ledger update/delete가 DB에서 거부됨
- [x] projection이 ledger delta 합과 일치함
- [x] tenant ownership과 active status가 검증됨
- [x] 전체 race/integration/CI 통과
- [x] README와 Cloud 후속 계약이 기록됨
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 구현 commit: `f0d26aee72c5661065ffccdf7d9450aa35448ce7`
- Pull Request: `https://github.com/nativegatewayhq/gateway/pull/9`
- `GOCACHE=/private/tmp/gateway-go-cache make check` 통과
- `GOCACHE=/private/tmp/gateway-go-cache TEST_DATABASE_URL=postgres://gateway:***@127.0.0.1:55433/gateway?sslmode=disable make integration-test` 통과
- PostgreSQL integration에서 lifecycle, release, insufficient funds, tenant 상태, append-only trigger, projection reconciliation과 concurrent retry/reserve 검증 통과
- GitHub Actions `check` 통과: `https://github.com/nativegatewayhq/gateway/actions/runs/32337272693/job/96329142976`
- GitHub Actions `validate` 통과: `https://github.com/nativegatewayhq/gateway/actions/runs/32337272674/job/96329142984`

## Rollback 계획

- 이전 binary로 rollback하되 wallet/ledger schema와 data는 유지한다.
- 금전 table을 drop하거나 ledger row를 수정하지 않는다.
- 신규 command 수락을 중단한 뒤 in-flight transaction 종료를 확인한다.
- correction은 forward fix와 compensating entry로 수행한다.

## 후속 작업

1. Provider pricing과 request cost estimate
2. inference request reserve/capture/release 연결
3. request idempotency와 timeout reconciliation
4. wallet management API와 Dashboard
5. payment-backed Deposit와 adjustment
