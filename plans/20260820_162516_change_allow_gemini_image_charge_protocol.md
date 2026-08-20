---
id: gateway-20260820-015
title: Allow Gemini Protocol in Image Charge Schema
status: completed
created_at: 2026-08-20T16:25:16+09:00
updated_at: 2026-08-20T16:33:11+09:00
owners:
  - gateway
initiative: phase-1-gemini-image-billing
depends_on:
  - gateway-20260820-014
supersedes: []
affected_repos:
  - gateway
---

# Allow Gemini Protocol in Image Charge Schema

## 목적

기존 OpenAI 전용 `image_request_charges.protocol` 제약을 OpenAI와 Gemini 이미지 charge를 모두 허용하도록 forward-only 확장해 `gateway-20260820-014`의 Gemini billable lifecycle을 영속화한다.

## 배경

Gateway의 Billing service와 pricing domain은 Gemini protocol을 지원하도록 확장할 수 있지만 migration `000005`가 charge protocol을 `openai`로 제한한다. PostgreSQL 통합 테스트가 Gemini Reserve 이후 charge insert를 거부했으며, 계획 #14는 schema 변경 발견 시 별도 change plan을 요구한다.

## 범위

- `image_request_charges.protocol` CHECK 제약의 명시적 이름 부여 및 `openai|gemini` 확장
- 기존 OpenAI row와 index/foreign key/immutability trigger 보존
- fresh migration과 기존 database upgrade 검증
- database required-schema test 갱신

## 제외 범위

- Anthropic, Replicate, fal protocol 허용
- operation/state/금액 schema 변경
- 기존 charge row rewrite
- migration rollback 또는 destructive constraint 축소

## 설계 및 구현 순서

### 1. 기존 제약 식별

- PostgreSQL이 자동 생성한 `image_request_charges_protocol_check`를 명시적으로 drop한다.
- 예상 제약이 없거나 다른 이름인 비지원 수동 schema는 migration 실패로 드러내 silent drift를 허용하지 않는다.

### 2. 확장 제약 추가

- 동일 column에 `CHECK (protocol IN ('openai','gemini'))`를 추가한다.
- 기존 row는 모두 새 제약을 만족하므로 table rewrite나 data migration이 없다.
- charge identity immutability trigger는 protocol 변경을 계속 거부한다.

## 인터페이스와 데이터 변경

### 공개 API

없음.

### 내부 인터페이스

Billing `BeginRequest.Protocol`의 허용 집합과 DB 제약을 일치시킨다.

### 데이터베이스 및 migration

forward-only `000008_allow_gemini_image_charge_protocol.sql`. 이전 binary는 Gemini row를 생성하지 않으며 기존 OpenAI row를 정상 처리한다. downgrade 시 제약을 되돌리거나 Gemini row를 삭제하지 않는다.

### 다른 저장소에 제공하거나 요구하는 계약

없음. Cloud와 Conformance 계약은 계획 #14가 소유한다.

## 보안 및 과금 고려사항

- protocol 허용만 확장하며 client가 protocol을 직접 지정하는 공개 API는 추가하지 않는다.
- handler가 path/capability에서 결정한 canonical `gemini`만 Billing에 전달한다.
- Wallet/Ledger, tenant FK와 append-only trigger는 변경하지 않는다.

## 테스트 계획

### 단위 테스트

- Billing validation의 OpenAI/Gemini 허용과 기타 protocol 거부

### 통합 테스트

- fresh schema에서 Gemini charge insert 및 settlement
- 기존 OpenAI charge lifecycle 회귀
- protocol identity update 거부
- migration 반복/concurrent 실행

### 호환성 및 장애 테스트

- 이전 migration 상태에서 `000008` upgrade
- 전체 Gateway integration suite

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

- [x] forward-only migration이 OpenAI와 Gemini protocol만 허용함
- [x] 기존 OpenAI row와 lifecycle이 보존됨
- [x] Gemini charge lifecycle이 PostgreSQL에서 동작함
- [x] protocol identity의 append-only 불변 조건이 유지됨
- [x] fresh/upgrade migration과 전체 CI가 통과함
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- change plan commit: `535fbc8`
- 구현 commit: `a08a1ad2c825a21f7a4460720032febf5175097c`
- Pull Request: [#14](https://github.com/nativegatewayhq/gateway/pull/14)
- CI: [check run 32344425839](https://github.com/nativegatewayhq/gateway/actions/runs/32344425839) 및 Plan policy 통과
- `make check`와 전체 PostgreSQL `make integration-test` 통과
- fresh schema Gemini lifecycle, migration 반복·동시 실행, constraint definition과 protocol identity update 거부를 검증함

## Rollback 계획

- 이전 binary로 rollback하되 확장된 CHECK와 Gemini charge row를 유지한다.
- constraint를 축소하거나 row를 삭제하지 않는다.
- 문제가 있는 Gemini route는 traffic/config에서 중단하고 charge는 reconciliation 절차로 처리한다.

## 후속 작업

- `gateway-20260820-014`의 Gemini billable lifecycle 완료
