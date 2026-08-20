---
id: gateway-YYYYMMDD-NNN
title: Short Plan Title
status: proposed
created_at: YYYY-MM-DDTHH:MM:SS+09:00
updated_at: YYYY-MM-DDTHH:MM:SS+09:00
owners:
  - gateway
initiative: initiative-name
depends_on: []
supersedes: []
affected_repos:
  - gateway
---

# Short Plan Title

> 이 파일을 복사한 뒤 안내 문구를 제거하고 작성한다. 승인된 계획의 범위와 설계를 변경해야 한다면 기존 파일을 수정하지 말고 새로운 `change` 계획을 만든다.

## 목적

이 계획이 완료되었을 때 만들어지는 하나의 명확한 결과를 작성한다.

## 배경

문제, 사용자 요구, 기존 제약과 이 작업이 지금 필요한 이유를 작성한다.

## 범위

- 구현할 항목
- 변경할 동작
- 제공할 계약

## 제외 범위

- 이 계획에서 구현하지 않을 인접 기능
- 별도 계획으로 분리할 항목

## 설계 및 구현 순서

### 1. 첫 번째 작업 단위

- 구현 내용
- 실패 및 경계 조건
- 선행 조건

### 2. 두 번째 작업 단위

- 구현 내용
- 실패 및 경계 조건
- 선행 조건

## 인터페이스와 데이터 변경

### 공개 API

없으면 `없음`이라고 작성한다.

### 내부 인터페이스

없으면 `없음`이라고 작성한다.

### 데이터베이스 및 migration

없으면 `없음`이라고 작성한다. 변경이 있다면 forward/backward compatibility와 rollback 방식을 포함한다.

### 다른 저장소에 제공하거나 요구하는 계약

없으면 `없음`이라고 작성한다. 멀티레포 작업은 상대 저장소의 계획 ID 또는 initiative를 기록한다.

## 보안 및 과금 고려사항

- credential 및 개인정보 영향
- 인증과 권한 영향
- SSRF, 파일, webhook 등 입력 경계
- Reserve, Capture, Release, Refund 영향
- 멱등성, timeout, reconciliation 영향

영향이 없다면 그 이유를 작성한다.

## 테스트 계획

### 단위 테스트

- 검증 항목

### 통합 테스트

- 검증 항목

### 호환성 및 장애 테스트

- 검증 항목

### 필수 검증 명령

```text
실제로 실행할 명령
```

## 완료 조건

- [ ] 사용자 또는 시스템 결과가 검증됨
- [ ] 정상 경로 테스트 통과
- [ ] 오류, timeout, retry 또는 동시성 경로 테스트 통과
- [ ] 보안 및 과금 불변 조건 검증
- [ ] 문서와 예제 갱신
- [ ] 검증 증거 기록

## 검증 증거

구현 전에는 `아직 구현 전`으로 둔다. 완료 시 다음을 기록한다.

- commit 또는 pull request
- CI 실행
- 테스트 명령과 결과
- 필요한 수동 검증 결과

## Rollback 계획

문제가 발생했을 때 안전하게 이전 동작으로 돌아가는 방법을 작성한다.

## 후속 작업

- 이 계획에서 의도적으로 분리한 작업
- 새 계획 또는 issue가 필요한 항목
