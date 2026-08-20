---
id: gateway-20260820-007
title: Phase 1 Capability Registry and Models API
status: completed
created_at: 2026-08-20T14:22:22+09:00
updated_at: 2026-08-20T14:29:30+09:00
owners:
  - gateway
initiative: phase-1-capability-registry
depends_on:
  - gateway-20260820-006
supersedes: []
affected_repos:
  - gateway
  - conformance
---

# Phase 1 Capability Registry and Models API

## 목적

모델·Provider·operation·native wire capability를 하나의 불변 레지스트리에서 관리하고, 서비스 Key로 보호된 OpenAI-compatible `GET /v1/models`에서 현재 실행 가능한 image model 목록을 결정적으로 반환한다.

기존 generation/edit handler의 model routing도 같은 레지스트리를 사용해 protocol, operation, model, capability가 불일치한 요청을 Provider 호출 전에 거부한다.

## 배경

`gateway-20260820-005`와 `006`은 정확한 model ID를 Provider에 매핑했지만 registry가 image package의 단순 map에 머물러 있다. 생성과 편집, JSON과 multipart capability를 명시적으로 표현하지 않으면 새 모델을 추가할 때 지원하지 않는 operation으로 라우팅될 수 있다.

2026-08-20 기준 공식 계약:

- OpenAI Models API: `https://platform.openai.com/docs/api-reference/models`
- xAI Models API: `https://docs.x.ai/developers/rest-api-reference/inference/models`

두 API의 최소 list 응답은 `object: "list"`와 `data` 내 `id`, `object: "model"`, `created`, `owned_by`를 공유한다. Gateway는 upstream 목록을 요청마다 합치지 않고 자신의 검증된 capability snapshot을 반환한다.

## 범위

- immutable Capability Registry
- model ID, Provider, owner, created timestamp
- 지원 operation: `image.generate`, `image.edit`
- operation별 inbound media type capability
- exact model lookup과 deterministic list
- 중복 model, 잘못된 Provider, operation 없는 model과 모순 capability의 시작 전 거부
- generation/edit handler가 registry operation lookup 사용
- service Key 인증이 적용된 `GET /v1/models`
- configured Provider credential이 있는 실행 가능 모델만 목록화
- OpenAI-compatible list response와 Gateway 오류 envelope
- README compatibility 표와 registry 추가 지침

## 제외 범위

- upstream `/v1/models` 실시간 proxy 또는 merge
- `GET /v1/models/{model}`
- database-backed 동적 registry와 Control Plane snapshot
- alias, 사용자별 model 권한, project policy와 region
- 가격, 판매가, margin과 유효 시각
- Provider channel, health score와 routing weight
- LLM, video, audio 및 embedding capability
- Wallet/Ledger, rate limit과 fallback

## 설계 및 구현 순서

### 1. Capability domain

- `operations` 또는 독립 core package에 다음 불변 값을 정의한다.
  - `OperationImageGenerate`
  - `OperationImageEdit`
  - JSON 및 multipart media capability
- model record는 `ID`, `Provider`, `Owner`, `Created`, operation별 capability를 가진다.
- public getter는 복사본을 반환해 생성 후 내부 map/slice가 변경되지 않게 한다.
- model ID는 길이·문자 집합을 검증하고 exact match만 허용한다.
- Provider는 기존 제한된 `providercredentials.ProviderID`를 사용한다.

### 2. Registry validation

- 중복 model ID, 빈 owner, 음수 created, operation 없음, 지원하지 않는 Provider를 시작 전 거부한다.
- 같은 operation/media capability 중복과 빈 media set을 거부한다.
- image generation은 JSON만, OpenAI edit은 multipart, xAI edit은 JSON으로 초기 manifest를 고정한다.
- list 결과는 model ID 오름차순으로 고정해 응답과 테스트를 재현 가능하게 한다.

### 3. 기존 routing 전환

- generation handler는 `image.generate + application/json` capability를 조회한다.
- edit handler는 `image.edit + 실제 media type` capability를 조회한다.
- model은 존재하지만 operation/media가 맞지 않으면 명시적인 unsupported 오류를 반환한다.
- model 미등록은 기존 `model_not_found`를 유지한다.
- handler별 임시 Provider 규칙과 media별 hard-coded Provider 비교를 제거한다.
- Provider credential 존재 여부로 요청 model을 다른 Provider에 fallback하지 않는다.

### 4. Models API

- exact route `GET /v1/models`를 추가한다.
- service API Key 인증은 기존 네 가지 위치를 지원하되 OpenAI SDK 기본 Bearer 형식을 문서화한다.
- 성공 응답은 다음 최소 native 형식이다.

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-image-1",
      "object": "model",
      "created": 0,
      "owned_by": "openai"
    }
  ]
}
```

- `created`는 manifest의 검증된 안정 값이며 현재 시각을 사용하지 않는다.
- 현재 process에 credential이 configured된 Provider의 model만 반환한다.
- credential 원문이나 비밀 metadata를 반환하지 않는다.
- 빈 목록도 `200`과 `data: []`를 반환한다.
- method가 GET이 아니면 `405`와 `Allow: GET`을 반환한다.

### 5. 조립과 관측성

- main이 registry 하나를 생성해 generation, edit, models handler에 공유한다.
- registry manifest 오류는 listener bind 전에 process 시작을 실패시킨다.
- models log는 request ID, protocol, operation=`models.list`, status, duration과 반환 개수만 포함한다.
- Key, raw query와 model 목록 전체를 로그 field로 남기지 않는다.

## 인터페이스와 데이터 변경

### 공개 API

```text
GET /v1/models
```

### 내부 인터페이스

```go
type Registry interface {
    Resolve(model string, operation Operation, mediaType MediaType) (Model, error)
    List() []Model
}
```

### 데이터베이스 및 migration

없음. 정적 manifest는 후속 versioned Control Plane snapshot으로 교체 가능하게 소비 인터페이스를 유지한다.

### 다른 저장소에 제공하거나 요구하는 계약

`conformance`는 initiative `phase-1-capability-registry`로 OpenAI Python·JavaScript SDK `models.list()`와 credential별 빈/활성 목록을 검증한다.

## 보안 및 과금 고려사항

- models endpoint도 service Key 인증을 요구해 활성 Provider metadata의 무인증 열람을 막는다.
- Provider credential은 configured 여부만 내부 filtering에 사용하며 원문·prefix·hash를 반환하거나 기록하지 않는다.
- registry 입력은 trusted startup manifest이며 사용자 URL이나 path를 생성하지 않는다.
- capability 불일치를 upstream 전에 거부해 예기치 않은 Provider 비용을 막는다.
- 이 계획에는 금전 transaction이 없으며 Wallet/Ledger를 변경하지 않는다.

## 테스트 계획

### 단위 테스트

- registry validation, immutability, exact lookup과 stable ordering
- operation/media capability resolve
- generation/edit handler routing 회귀
- models 인증, method, 빈 목록, 활성 Provider filtering과 response schema
- credential 및 query log redaction

### 통합 테스트

- PostgreSQL service Key로 `/v1/models` 호출
- OpenAI만, xAI만, 모두, credential 없음 조합
- 반환 model로 generation/edit handler route 성공
- health와 기존 native API 회귀 없음

### 호환성 및 장애 테스트

- OpenAI Python/JavaScript SDK models list fixture
- authentication store 장애 시 `503`
- malformed/ambiguous service credential
- registry 초기화 오류가 network bind 전에 실패

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

- [x] model/provider/operation/media capability가 하나의 immutable registry에 있음
- [x] 잘못되거나 모순된 registry가 시작 전에 거부됨
- [x] generation과 edit handler가 같은 registry로 routing함
- [x] `/v1/models`가 service Key로 보호됨
- [x] configured Provider의 실행 가능 model만 stable OpenAI schema로 반환됨
- [x] credential 없음과 빈 registry가 안전한 빈 목록을 반환함
- [x] credential과 내부 capability가 응답·로그에 노출되지 않음
- [x] 기존 native request/response 의미가 회귀하지 않음
- [x] 단위·통합·race 테스트와 CI가 통과함
- [x] README와 Conformance 계약이 갱신됨
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 로컬 검증:
  - `make check`: formatter, vet, 전체 race test와 binary build 통과
  - `make integration-test`: PostgreSQL service Key로 image generation과 `/v1/models` 호출 통과
  - `git diff --check`: 통과
  - `go test -cover ./operations/image ./protocols/openai`: registry 89.4%, OpenAI protocol 79.8%
- 동작 및 보안 검증:
  - manifest 중복·잘못된 Provider·빈 operation·모순 media capability 시작 전 거부
  - registry copy-on-read 불변성과 model ID stable ordering
  - generation/edit의 operation/media resolve 및 불일치 upstream 전 거부
  - OpenAI/xAI credential 조합별 model filtering, credential 없는 빈 목록과 native schema
  - service Key 인증과 로그에 credential·model 목록이 포함되지 않는 최소 관측성
- 구현 commit: [`9f5d4fd`](https://github.com/nativegatewayhq/gateway/commit/9f5d4fd)
- pull request: [#7](https://github.com/nativegatewayhq/gateway/pull/7)
- CI:
  - [`check`](https://github.com/nativegatewayhq/gateway/actions/runs/32335703138/job/96324752622): 통과
  - [`validate`](https://github.com/nativegatewayhq/gateway/actions/runs/32335703091/job/96324752731): 통과

## Rollback 계획

- `/v1/models` route를 제거하고 generation/edit handler를 직전 exact model map으로 되돌린다.
- DB migration이 없어 schema rollback은 없다.
- rollback은 공개 model 목록 기능만 제거하며 기존 Provider 요청을 재시도하지 않는다.

## 후속 작업

1. database-backed model/capability snapshot과 version
2. Provider Channel 및 model alias mapping
3. 사용자·project별 model 권한
4. 가격과 margin metadata
5. rate limit과 fallback
