---
id: gateway-20260820-001
title: Phase 0 Gateway Bootstrap
status: accepted
created_at: 2026-08-20T11:38:25+09:00
updated_at: 2026-08-20T12:10:00+09:00
owners:
  - gateway
initiative: phase-0-native-sdk-validation
depends_on: []
supersedes: []
affected_repos:
  - gateway
---

# Phase 0 Gateway Bootstrap

## 목적

후속 Protocol과 Provider 기능을 안전하게 추가할 수 있는 최소 Go Gateway 실행 기반을 만든다.

이 계획은 실제 공급자 이미지 생성까지 구현하지 않는다. HTTP 서버, 설정, 종료 처리, 상태 확인, 프로젝트 구조와 테스트 기반을 먼저 고정한다.

## 배경

Native AI Gateway의 첫 기술적 목표는 공식 Gemini 및 OpenAI SDK가 Base URL과 API Key만 변경하여 Gateway를 호출하는지 검증하는 것이다. 이 검증 전에 모든 후속 기능이 공통으로 사용할 실행 기반과 최소 운영 규약이 필요하다.

초기부터 과도한 프레임워크나 서비스 분리를 도입하지 않고 Go 모듈형 모놀리스로 시작한다.

## 범위

- Go module 초기화
- `cmd/gateway` 실행 진입점
- 환경 변수 기반 설정 로딩과 검증
- HTTP 서버와 routing 골격
- `/health/live` liveness endpoint
- `/health/ready` readiness endpoint
- 구조화 로그와 request ID
- graceful shutdown
- 기본 오류 응답 구조
- 단위 테스트 및 HTTP smoke test
- 로컬 실행 문서
- 기본 CI에서 수행할 검증 명령 정의

## 제외 범위

- 실제 Google, OpenAI, xAI 호출
- 서비스 API Key 인증
- PostgreSQL과 Redis 연결
- Wallet과 Ledger
- 가격 및 Routing Engine
- Provider credential 저장
- 요청/응답 본문 로깅
- Docker Compose
- 공식 SDK Conformance 테스트

제외된 항목은 각각 별도 계획으로 추가한다.

## 목표 디렉터리 구조

```text
gateway/
├─ cmd/
│  └─ gateway/
│     └─ main.go
├─ internal/
│  ├─ app/
│  ├─ config/
│  ├─ httpserver/
│  ├─ requestid/
│  └─ observability/
├─ plans/
├─ go.mod
├─ go.sum
├─ Makefile
├─ README.md
├─ LICENSE
└─ CONTRIBUTING.md
```

디렉터리는 실제 코드가 필요할 때만 생성한다. 빈 패키지를 미리 만들지 않는다.

## 설계 및 구현 순서

### 1. 프로젝트 메타데이터

- Go module 경로를 확정한다.
- 최소 지원 Go 버전을 명시한다.
- Apache-2.0 라이선스를 추가한다.
- 로컬 실행 및 테스트 방법을 README에 기록한다.
- Formatter, vet, test 명령을 Makefile 또는 동일한 task runner로 표준화한다.

### 2. 설정 계약

초기 설정은 환경 변수로 주입한다.

| 변수 | 필수 | 기본값 | 설명 |
|---|---:|---|---|
| `GATEWAY_HTTP_ADDR` | 아니요 | `:8080` | HTTP listen address |
| `GATEWAY_LOG_LEVEL` | 아니요 | `info` | 로그 수준 |
| `GATEWAY_SHUTDOWN_TIMEOUT` | 아니요 | `10s` | graceful shutdown 제한 |

규칙:

- 잘못된 설정은 서버 시작 전에 실패시킨다.
- Secret 값은 설정 오류와 로그에 출력하지 않는다.
- package global mutable config를 사용하지 않는다.
- 테스트에서 환경 변수 없이 config 객체를 직접 주입할 수 있어야 한다.

### 3. 애플리케이션 조립

- `main`은 설정 로딩, dependency 조립, signal 처리만 담당한다.
- HTTP handler와 서버 생명주기는 테스트 가능한 패키지로 분리한다.
- 라이브러리 패키지에서 `os.Exit` 또는 `log.Fatal`을 호출하지 않는다.
- 장기 실행 goroutine은 명시적인 context와 종료 경로를 가진다.

### 4. HTTP 기반

- liveness는 프로세스가 요청을 처리할 수 있는지만 반환한다.
- readiness는 필수 dependency 상태를 반영할 수 있는 확장 지점을 가진다.
- 알 수 없는 경로는 일관된 404 응답을 반환한다.
- panic recovery는 credential이나 본문을 노출하지 않는 500 응답을 반환한다.
- request ID가 없으면 생성하고 응답 헤더와 로그에 포함한다.
- 입력 request ID를 수용할 경우 길이와 문자 집합을 제한한다.

초기 오류 형식은 Gateway 자체 endpoint에만 적용한다. Provider-native endpoint의 오류 형식은 각 Protocol 계획에서 별도로 정의한다.

```json
{
  "error": {
    "code": "internal_error",
    "message": "internal server error",
    "request_id": "req_..."
  }
}
```

### 5. 관측성과 로그

- 구조화된 로그를 사용한다.
- 최소 필드: timestamp, level, message, request_id, method, route, status, duration.
- Authorization, API Key, query의 `key`, cookie는 출력하지 않는다.
- 요청 및 응답 본문은 기본적으로 기록하지 않는다.
- health endpoint 로그는 운영 시 sampling 또는 제외 가능하도록 구성한다.

### 6. 종료 처리

- SIGINT와 SIGTERM을 처리한다.
- 신규 연결 수락을 중지한다.
- 진행 중 요청은 설정된 timeout 내 종료를 기다린다.
- timeout을 초과하면 명확한 오류 로그를 남기고 종료한다.

## 인터페이스와 데이터 변경

### 공개 HTTP endpoint

```text
GET /health/live
GET /health/ready
```

성공 응답:

```json
{
  "status": "ok"
}
```

### 데이터베이스 변경

없음.

### 다른 저장소에 제공하는 계약

이 계획은 Conformance 저장소가 Gateway 프로세스의 준비 상태를 확인할 수 있도록 `/health/ready`를 제공한다. SDK endpoint 계약은 후속 Protocol 계획에서 정의한다.

## 보안 및 과금 고려사항

- 모든 request metadata는 신뢰할 수 없는 입력으로 취급한다.
- 오류 응답에는 stack trace, 내부 경로, 환경 변수 값을 포함하지 않는다.
- signal 및 shutdown 로그에도 전체 config를 출력하지 않는다.
- 이 단계에는 유료 요청이 없으므로 과금 이벤트를 생성하지 않는다.

## 테스트 계획

### 단위 테스트

- 기본 설정 로딩
- 잘못된 listen address 또는 duration 거부
- 환경 변수 override
- secret redaction 보조 로직
- request ID 생성 및 검증
- 오류 응답 직렬화

### HTTP 테스트

- `/health/live`가 200과 예상 JSON 반환
- `/health/ready`가 200과 예상 JSON 반환
- 알 수 없는 경로가 404 반환
- request ID가 응답에 포함됨
- panic 발생 시 민감 정보 없는 500 반환

### 프로세스 테스트

- 서버 시작과 health check 성공
- SIGTERM 이후 정상 종료
- 사용 중인 port 지정 시 빠르게 실패

### 필수 검증 명령

정확한 명령은 Go module 생성 후 README와 CI에서 고정한다. 최소한 다음 검증을 제공해야 한다.

```text
go fmt check
go vet ./...
go test ./...
go build ./cmd/gateway
```

## 완료 조건

- [ ] `gateway`가 독립 Go module로 초기화됨
- [ ] 단일 명령으로 Gateway를 로컬 실행할 수 있음
- [ ] liveness와 readiness endpoint가 명세대로 응답함
- [ ] 설정 오류가 서버 시작 전에 검출됨
- [ ] 각 요청에 request ID가 부여됨
- [ ] 구조화 로그에 인증 헤더와 query API Key가 포함되지 않음
- [ ] SIGINT와 SIGTERM에서 graceful shutdown이 동작함
- [ ] formatter, vet, unit test, build가 모두 통과함
- [ ] README에 실행 및 검증 방법이 기록됨
- [ ] CI에서 동일 검증을 자동 실행함

## 검증 증거

아직 구현 전이다. 완료 시 다음 정보를 기록한다.

- 구현 commit 또는 pull request
- CI 실행 링크 또는 로컬 검증 결과
- health endpoint smoke test 결과
- graceful shutdown 검증 결과

## 후속 작업

이 계획 완료 후 최소한 다음 계획을 순서대로 추가한다.

1. 서비스 API Key 인증과 redaction
2. Provider credential 설정 및 보안 경계
3. Gemini `generateContent` native pass-through
4. OpenAI `/v1/images/generations` native pass-through
5. Provider mock server 계약
6. Conformance 저장소의 공식 SDK 검증 계획
