# Contributing to Native AI Gateway

Native AI Gateway에 기여해 주셔서 감사합니다.

이 프로젝트는 **Plan First** 방식으로 개발한다. 기능 구현, 동작 변경, 리팩터링, 데이터 변경처럼 의미 있는 작업은 코드를 수정하기 전에 `plans/`에 실행 계획을 먼저 기록해야 한다.

## 가장 먼저 읽을 문서

기여를 시작하기 전에 다음 문서를 순서대로 확인한다.

1. [`plans/README.md`](./plans/README.md) — 계획 파일의 형식과 운영 규칙
2. 해당 작업과 관련된 기존 계획 — 선행 작업, 대체 관계, 미완료 조건 확인
3. 저장소의 `README.md`와 관련 설계 문서 — 실행 및 구현 규약

## Plan First 원칙

다음 변경은 구현 전에 계획 파일이 필요하다.

- 새로운 API 또는 사용자 기능
- Protocol, Operation, Provider 동작 변경
- 데이터베이스 schema 또는 migration
- 인증, 권한, 과금, Ledger 변경
- 라우팅, retry, fallback, timeout 변경
- 공개 인터페이스 또는 응답 형식 변경
- 보안 경계나 credential 처리 변경
- 여러 패키지 또는 여러 저장소에 영향을 주는 리팩터링
- 기존 계획의 범위나 설계를 대체하는 변경

다음 작업은 일반적으로 별도 계획 없이 진행할 수 있다.

- 오탈자와 깨진 링크 수정
- 동작을 바꾸지 않는 작은 문서 보완
- 기존 계획에 명시된 구현 작업
- 기존 완료 조건을 위한 테스트 추가
- formatter 또는 lint가 만드는 기계적인 수정

판단이 모호하면 작은 계획을 먼저 작성한다.

## 계획 생성 절차

1. `Asia/Seoul` 기준 생성 시각을 확인한다.
2. [`plans/TEMPLATE.md`](./plans/TEMPLATE.md)를 복사한다.
3. 다음 형식으로 파일명을 정한다.

   ```text
   YYYYMMDD_HHMMSS_<kind>_<slug>.md
   ```

4. 고유한 `id`와 관련 `initiative`를 입력한다.
5. 범위, 제외 범위, 구현 순서, 테스트, 완료 조건을 작성한다.
6. `plans/README.md`의 현재 계획 목록에 추가한다.
7. 계획을 검토하여 `accepted`로 전환한 후 구현을 시작한다.

계획 종류:

| kind | 용도 |
|---|---|
| `plan` | 새로운 구현 계획 |
| `change` | 승인된 계획의 범위나 접근 방식 변경 |
| `rollback` | 구현 또는 정책 철회 |
| `close` | 별도 종료 기록이 필요한 경우 |

## 계획 변경 규칙

승인되어 구현이 시작된 계획은 당시 의사결정의 기록이다.

- 상태, `updated_at`, 체크박스, 검증 증거는 갱신할 수 있다.
- 오탈자나 링크 오류처럼 의미를 바꾸지 않는 수정은 허용한다.
- 범위, 설계, 인터페이스, 완료 조건을 실질적으로 바꾸지 않는다.
- 실질적 변경은 새로운 `change` 계획을 만들고 `supersedes`로 연결한다.
- 구현을 철회할 때는 `rollback` 계획으로 이유와 영향 범위를 남긴다.

## 구현 절차

일반적인 작업 흐름은 다음과 같다.

```text
proposed plan
    ↓ review
accepted
    ↓ implementation begins
in_progress
    ↓ tests and evidence
completed
```

구현 중에는 다음 원칙을 지킨다.

- 계획의 범위 밖 변경을 함께 넣지 않는다.
- 기존 동작을 변경하면 회귀 테스트를 추가한다.
- 인증 정보와 API Key 원문을 코드, fixture, 로그에 남기지 않는다.
- Protocol 호환성은 내부 handler 테스트만으로 완료 처리하지 않는다.
- 과금 변경은 동시성, 멱등성, 실패, timeout, reconciliation을 검증한다.
- 코드와 문서가 다르면 코드만 고치지 말고 관련 계획 또는 문서를 함께 정리한다.

## 완료 처리

계획은 코드가 작성되었다는 이유만으로 완료되지 않는다.

다음 조건을 모두 만족해야 한다.

1. 계획의 완료 조건이 모두 충족되었다.
2. 관련 자동 테스트가 통과한다.
3. formatter, lint 또는 vet, build가 통과한다.
4. 공개 동작이 바뀌었다면 문서와 예제가 갱신되었다.
5. `검증 증거`에 commit, pull request, CI 또는 재현 가능한 명령을 기록했다.
6. 후속 작업이 있다면 새로운 계획이나 issue로 분리했다.
7. 계획 상태와 `plans/README.md`의 목록을 `completed`로 갱신했다.

## 멀티레포 작업

각 저장소는 자신의 변경만 계획하고 소유한다. 하나의 계획 파일로 여러 저장소의 내부 구현을 통제하지 않는다.

여러 저장소에 걸친 작업은 공통 `initiative`를 사용한다.

```yaml
initiative: phase-0-gemini-sdk-e2e
affected_repos:
  - gateway
  - conformance
```

예:

```text
gateway/plans/...plan_gemini_native_passthrough.md
conformance/plans/...plan_gemini_python_conformance.md
```

Gateway 계획에는 Gateway의 공개 계약과 인수 조건을 기록하고, Conformance 계획에는 공식 SDK 설치와 외부 호환 테스트를 기록한다. 저장소 사이에는 commit hash 대신 릴리스 버전이나 명시된 계약을 사용하는 것을 우선한다.

## Pull Request 기준

Pull Request에는 다음 정보를 포함한다.

- 관련 계획 ID와 링크
- 변경 목적과 범위
- 주요 설계 판단
- 실행한 테스트와 결과
- 보안 및 과금 영향
- 호환성 또는 migration 영향
- rollback 방법

관련 계획이 없는 예외 작업이라면 계획이 필요하지 않은 이유를 적는다.

## Provider Adapter 기여

외부 HTTP sidecar Adapter는 Gateway의 `internal/` package를 import하지 않고
동기 Adapter는 `plugin-sdk/runtime/v1`, 비동기 이미지 Adapter는
`plugin-sdk/async/v1`, 비동기 영상 Adapter는 `plugin-sdk/video/v1` 공개 wire package만 사용한다. 시작점은 각각
[`examples/plugin/go-sidecar-template`](./examples/plugin/go-sidecar-template)과
[`examples/plugin/go-async-sidecar-template`](./examples/plugin/go-async-sidecar-template),
[`examples/plugin/go-video-sidecar-template`](./examples/plugin/go-video-sidecar-template)이다.

Adapter 변경 PR과 외부 저장소 CI에서는 다음을 실행한다.

```text
GOWORK=off go test ./...
go run ./cmd/gateway-plugin-validator -manifest-dir /absolute/manifests
go run ./cmd/gateway-plugin-conformance ...
go run ./cmd/gateway-plugin-conformance -profile async-v1 ...
go run ./cmd/gateway-plugin-conformance -profile video-v1 ...
```

Conformance는 실제 유료 Provider 호출이 아닌 전용 test-mode contract를
사용해야 한다. endpoint, secret ref/value, prompt, raw request/response 또는
이미지를 report와 로그에 포함하면 안 된다. v1 필드 삭제나 의미 변경은
허용하지 않으며 additive optional field 또는 새 schema version으로 변경한다.
Async callback HMAC 키는 sidecar bearer와 분리하고 callback URL, capability,
Provider Job ref와 서명을 report나 로그에 기록하지 않는다.
기존 버전 폐기는 새 버전 공개와 migration 기간을 제공하는 별도 Plan으로만
진행한다.

공식 Adapter Registry 기여는 OCI digest, source commit, conformance report,
SBOM과 provenance descriptor를 함께 제출해야 한다. 제출자가 만든 서명이나
report만으로 official 상태를 부여하지 않고 Registry의 격리된 pipeline이
재검증한다. Gateway PR에는 private key, mutable image tag, remote download
동작 또는 sequence floor를 낮추는 rollback을 추가하지 않는다.

## 프로젝트 불변 조건

다음 조건을 위반하는 변경은 승인할 수 없다.

1. 동일한 논리 요청을 두 번 과금하지 않는다.
2. 동시 요청으로 가용 잔액이 음수가 되지 않는다.
3. Provider timeout을 실패로 단정하지 않는다.
4. 금전 변경은 append-only Ledger로 감사할 수 있어야 한다.
5. Provider credential과 서비스 API Key 원문을 로그에 남기지 않는다.
6. Fallback은 operation 의미와 과금 조건을 보존할 때만 수행한다.
7. 비동기 작업과 정산은 재시작 후 복구할 수 있어야 한다.
8. 중복 webhook과 polling은 멱등적으로 처리한다.
9. 가격과 라우팅 정책은 버전과 유효 시각을 가져야 한다.
10. Control Plane 장애가 무조건 Data Plane 장애로 전파되지 않아야 한다.

## 보안 문제

실제 credential, 취약점 악용 방법 또는 사용자 데이터를 공개 issue에 올리지 않는다. 보안 신고 채널이 확정되기 전에는 저장소 관리자에게 비공개로 전달한다.
