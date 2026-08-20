---
id: gateway-20260820-005
title: Phase 0 OpenAI Images Native Pass-through
status: completed
created_at: 2026-08-20T13:53:52+09:00
updated_at: 2026-08-20T14:08:30+09:00
owners:
  - gateway
initiative: phase-0-openai-images-sdk-e2e
depends_on:
  - gateway-20260820-004
supersedes: []
affected_repos:
  - gateway
  - conformance
---

# Phase 0 OpenAI Images Native Pass-through

## 목적

OpenAI 공식 Python·JavaScript SDK가 서비스 API Key와 Gateway Base URL만 사용해 `images.generate`를 호출하고, 요청 모델에 따라 OpenAI 또는 xAI의 native image generation endpoint로 안전하게 전달되도록 한다.

Gateway는 서비스 Key를 검증하고 선택된 Provider의 credential로 교체한다. 요청과 Provider 응답은 OpenAI Images JSON 계약을 유지하며, Phase 0에서는 변환·재시도·fallback 없이 한 Provider에 한 번만 전달한다.

## 배경

`gateway-20260820-003`에서 Google·OpenAI·xAI credential 경계를 확립했고, `gateway-20260820-004`에서 Gemini native facade와 fixed-origin Google transport를 구현했다. 다음 기술 검증은 동일한 OpenAI Images protocol을 제공하는 OpenAI와 xAI를 공식 OpenAI SDK 하나로 호출할 수 있는지 확인하는 것이다.

이 계획은 2026-08-20에 확인한 다음 공식 계약을 기준으로 한다.

- OpenAI Images API reference: `https://platform.openai.com/docs/api-reference/images`
- OpenAI API authentication: `https://platform.openai.com/docs/api-reference/introduction`
- xAI Images REST API: `https://docs.x.ai/developers/rest-api-reference/inference/images`
- xAI OpenAI SDK quickstart: `https://docs.x.ai/developers/quickstart`

xAI는 `POST https://api.x.ai/v1/images/generations`와 OpenAI SDK의 `images.generate` 사용법을 공식 문서화한다. 두 Provider가 같은 외부 protocol을 사용하더라도 credential, origin, 지원 model과 응답 확장 필드는 Provider별로 격리한다.

SDK 설치 및 실제 SDK 버전별 검증은 `conformance` 저장소가 소유한다. Gateway 저장소는 SDK가 생성하는 HTTP 계약과 mock upstream 왕복을 검증한다.

## 범위

- OpenAI Images native route `POST /v1/images/generations`
- 기존 네 가지 서비스 API Key 형식을 통한 route 보호
- 요청 JSON의 bounded, byte-preserving pass-through
- 요청의 최상위 `model`만 제한적으로 읽는 deterministic Provider 선택
- 명시적 image model route registry
- OpenAI `https://api.openai.com` fixed-origin transport
- xAI `https://api.x.ai` fixed-origin transport
- 선택된 Provider credential의 Bearer 주입
- OpenAI-compatible Gateway 오류 envelope
- Provider status, JSON body와 허용 응답 header pass-through
- timeout, cancellation, redirect 차단 및 단일 시도 정책
- OpenAI/xAI mock upstream 기반 단위·통합 테스트
- OpenAI SDK용 Base URL 예제와 지원 범위 문서화

## 제외 범위

- `POST /v1/images/edits`와 multipart upload
- image variation 및 Responses API image tool
- streaming image generation과 partial image SSE
- `/v1/models`
- Gemini와 OpenAI Images 사이의 cross-protocol 변환
- 자동 model discovery와 완전한 Capability Registry
- 가격·속도·성공률 기반 routing, weighted routing과 fallback
- retry, circuit breaker와 timeout reconciliation
- Wallet, Ledger, reserve/capture/refund와 usage 원가 정산
- idempotency 저장소
- managed image storage, Provider URL 복사, S3/R2 및 CDN
- 공식 SDK 설치와 실제 Provider credential을 사용하는 live test

편집, 과금, fallback 및 managed storage는 각각 후속 계획으로 분리한다.

## 설계 및 구현 순서

### 1. OpenAI Images route와 handler 조립

- `protocols/openai`에 image generation handler를 추가한다.
- route는 정확히 `POST /v1/images/generations`만 처리한다.
- handler dependency는 서비스 API Key authenticator, image model route registry, Provider별 executor, timeout과 최대 body 크기로 제한한다.
- 인증, media type, body 제한과 model routing이 성공한 뒤에만 Provider network 요청을 시작한다.
- health endpoint와 기존 Gemini route는 회귀 없이 유지한다.
- route에 도달한 Gateway 오류는 OpenAI-compatible envelope를 사용하고, Gateway 공통 404는 기존 계약을 유지한다.

### 2. 서비스 API Key 인증과 inbound 정리

- 기존 공통 extractor의 다음 위치를 그대로 지원한다.
  - `Authorization: Bearer SERVICE_KEY`
  - `x-api-key: SERVICE_KEY`
  - `x-goog-api-key: SERVICE_KEY`
  - `?key=SERVICE_KEY`
- 인증에 사용한 모든 header와 query credential은 upstream으로 전달하지 않는다.
- 인증 실패 시 body parsing과 Provider credential 조회 및 network 호출을 수행하지 않는다.
- authenticated principal에는 Key ID만 유지하고 원문 service Key는 요청 수명 밖으로 전달하지 않는다.
- Cookie, Proxy authorization, forwarding header와 hop-by-hop header를 제거한다.

### 3. 요청 계약과 bounded model inspection

- `Content-Type`은 JSON media type만 허용하며 charset parameter를 안전하게 parsing한다.
- `GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES`를 추가하고 기본값과 최대값을 `1048576`(`1 MiB`)로 둔다.
- 명시된 `Content-Length`와 chunked body 모두 동일한 제한을 적용한다.
- body는 한 번 bounded read한 원본 byte slice를 Provider에 그대로 보낸다.
- Provider 선택을 위해 최상위 JSON object의 `model` 문자열만 읽는다. 나머지 필드는 validation, 기본값 삽입, 제거 또는 re-encoding하지 않는다.
- 빈 body, malformed JSON, 최상위 object가 아닌 JSON, model 누락·빈 문자열·중복 model key는 upstream 호출 전 `400 invalid_request_error`로 거부한다.
- prompt와 Base64 data는 로그, metric, 오류 message에 포함하지 않는다.

### 4. 명시적 model route registry

- 초기 `ImageModelRegistry`는 정확한 model ID를 하나의 Provider ID에 매핑한다.
- prefix, substring, credential 존재 여부나 임의 fallback으로 Provider를 추측하지 않는다.
- 초기 검증 fixture에는 최소 다음 Provider별 model을 등록한다.
  - OpenAI: 구현 시점 공식 Images API가 지원하는 GPT Image model ID
  - xAI: 구현 시점 공식 Images API가 지원하는 Grok Imagine image model ID
- 실제 model ID는 구현 시 공식 문서를 다시 확인해 테스트 fixture와 README compatibility 표에 고정한다.
- 알 수 없는 model은 network 요청 없이 `404 model_not_found`로 반환한다.
- 동일 model ID를 여러 Provider에 등록하면 애플리케이션 시작을 실패시킨다.
- Phase 0 registry는 코드 또는 검증된 정적 설정으로 구성하며 database와 동적 routing은 후속 Capability Registry 계획이 대체한다.

### 5. Provider별 trusted transport

- production origin을 다음 값으로 고정한다.
  - OpenAI: `https://api.openai.com`
  - xAI: `https://api.x.ai`
- inbound scheme, host, userinfo, fragment, `Host`와 `RequestURI`를 upstream URL에 재사용하지 않는다.
- outbound path는 두 Provider 모두 `/v1/images/generations`로 새로 구성한다.
- incoming query는 native endpoint에 필요하지 않으므로 전달하지 않는다.
- `Content-Type`, `Accept`, 검토된 공식 SDK 식별 header만 allowlist로 전달한다.
- `gateway-20260820-003`의 credential boundary로 선택된 Provider의 `Authorization: Bearer` 하나만 적용한다.
- OpenAI organization/project header는 고객 credential scope와 결합되므로 Phase 0에서 전달하지 않는다.
- test 전용 injected transport는 허용하지만 production 환경 변수로 임의 origin을 받지 않는다.
- 모든 3xx redirect를 따르지 않아 Bearer credential의 origin 이탈을 막는다.

### 6. Timeout, cancellation과 실행 의미

- `GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT`을 추가하고 기본값 `2m`, 최대값 `10m`로 제한한다.
- caller disconnect와 server shutdown context를 upstream request 및 response body copy까지 전파한다.
- connect, TLS handshake와 response header에 유한 timeout을 둔다.
- timeout, connection reset, 429 또는 5xx에도 자동 retry나 다른 Provider fallback을 수행하지 않는다.
- Provider가 status를 보내기 전 timeout은 결과 불명 가능성이 있으므로 Gateway `504`를 반환하고 failure category를 기록한다.
- response body는 모든 성공·오류·copy failure 경로에서 정확히 한 번 닫는다.
- 이미 downstream status를 쓴 뒤 copy가 실패하면 새 JSON 오류를 덧붙이지 않는다.

### 7. Native 응답과 오류 pass-through

- Provider가 응답한 HTTP status와 body byte sequence는 변환하지 않는다.
- xAI의 `usage` 또는 비용 확장 필드도 제거하지 않는다.
- 최소 응답 header allowlist는 `Content-Type`, `Retry-After`, 검토된 request ID header다.
- 실제 전송과 불일치할 수 있는 upstream `Content-Length`, `Set-Cookie`, hop-by-hop 및 인증 관련 header는 제거한다.
- Provider 4xx/5xx body도 그대로 전달해 OpenAI SDK의 오류 parsing을 보존한다.
- Provider 응답을 받기 전에 발생한 Gateway 오류만 다음 최소 형식을 사용한다.

```json
{
  "error": {
    "message": "authentication required",
    "type": "invalid_request_error",
    "param": null,
    "code": "authentication_required"
  }
}
```

초기 mapping:

| 조건 | HTTP | type/code |
|---|---:|---|
| service credential 누락·오류 | 401 | `invalid_request_error/authentication_required` |
| media type·JSON·body limit 오류 | 400 또는 413 | `invalid_request_error` |
| model 미등록 | 404 | `invalid_request_error/model_not_found` |
| Provider credential 미설정 | 503 | `server_error/provider_unavailable` |
| upstream 연결 실패 | 502 | `server_error/upstream_unavailable` |
| Gateway timeout | 504 | `server_error/upstream_timeout` |
| 내부 panic | 500 | `server_error/internal_error` |

- 모든 Gateway 응답에 `X-Request-Id`를 유지한다.
- 외부 오류에는 credential 존재 여부, upstream URL, stack trace와 내부 package명을 포함하지 않는다.

### 8. 관측성과 문서

- 최소 기록: request ID, protocol=`openai`, operation=`image.generate`, provider, 검증된 model, status, duration, failure category.
- API Key, Authorization, raw query, prompt, request/response body, image URL과 Base64 image를 기록하지 않는다.
- upstream attempt counter로 요청당 선택된 Provider 호출이 최대 한 번임을 검증한다.
- README에 OpenAI Python·JavaScript SDK의 `base_url`과 서비스 Key 설정 예제를 추가한다.
- compatibility 표에 검증된 SDK 버전, model, Provider와 `response_format` 동작을 기록할 자리를 둔다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/images/generations
```

Python SDK 계약:

```python
from openai import OpenAI

client = OpenAI(
    api_key="SERVICE_API_KEY",
    base_url="https://openai.api.example.com/v1",
)

response = client.images.generate(
    model="REGISTERED_IMAGE_MODEL",
    prompt="고양이 우주비행사를 그려줘",
)
```

`base_url`이 `/v1`을 포함하는지 여부는 공식 SDK가 생성하는 실제 path를 Conformance 계획에서 고정하고 README 예제와 일치시킨다.

### 내부 인터페이스

```go
type ImageModelRegistry interface {
    ProviderForImageModel(model string) (providercredentials.ProviderID, error)
}

type ImageGenerationExecutor interface {
    Generate(ctx context.Context, req ImageGenerationRequest) (*http.Response, error)
}

type ImageGenerationRequest struct {
    Body        io.Reader
    ContentType string
    Headers     http.Header
}
```

- protocol handler는 인증, bounded body, model lookup과 OpenAI 오류 mapping을 소유한다.
- Provider executor는 fixed origin, safe header, Provider credential, timeout과 HTTP 실행을 소유한다.
- JSON을 공통 Image Operation model로 변환하지 않는다.

### 데이터베이스 및 migration

없음.

기존 `service_api_keys`와 `GATEWAY_OPENAI_API_KEY`·`GATEWAY_XAI_API_KEY`를 사용한다. model registry, 요청 원장과 과금 데이터의 영속화는 후속 계획에서 추가한다.

### 다른 저장소에 제공하거나 요구하는 계약

`conformance` 저장소는 동일한 initiative `phase-0-openai-images-sdk-e2e`로 별도 계획을 작성한다.

Gateway가 제공하는 계약:

- `POST /v1/images/generations`
- OpenAI SDK Bearer 위치에 입력한 service Key 인증
- 명시적 model→OpenAI/xAI 선택
- Provider native success/error response pass-through
- `X-Request-Id`

Conformance 저장소가 검증할 항목:

- OpenAI Python·JavaScript SDK 버전 고정
- Base URL과 Key만 변경한 `images.generate` 호출
- URL 및 Base64 response parsing
- OpenAI/xAI native error parsing
- SDK 업데이트에 따른 path, header와 request field drift

## 보안 및 과금 고려사항

- inbound service Key는 Provider에 전달하지 않고 선택된 Provider credential만 주입한다.
- model route는 사용자 입력 host나 URL을 만들지 않으며 trusted Provider enum만 반환한다.
- JSON 내부 URL을 Gateway가 fetch하지 않으므로 이 계획에서 SSRF surface를 추가하지 않는다.
- 요청 body와 Provider 응답은 민감한 prompt·image를 포함할 수 있으므로 기록하지 않는다.
- Bearer credential은 redirect, proxy header와 사용자 선택 origin을 통해 외부 host로 나갈 수 없다.
- 자동 retry/fallback을 금지해 생성 성공 유실 시 중복 Provider 비용이 발생하지 않게 한다.
- 이 계획에는 Wallet/Ledger가 없으므로 사용자 잔액을 차감하지 않는다. Provider 비용은 운영 측 usage에서 별도 확인한다.
- idempotency와 reconciliation이 추가되기 전에는 timeout 요청을 자동 재실행하지 않는다.

## 테스트 계획

### 단위 테스트

- route method와 exact path semantics
- 네 가지 service credential 위치 및 충돌
- JSON content type, fixed/chunked body limit
- malformed JSON, duplicate/missing/unknown model
- 정확한 model→Provider mapping과 중복 registry 거부
- OpenAI/xAI fixed origin, path와 Bearer credential
- unsafe inbound header·query 제거와 safe header 보존
- redirect 차단과 Provider credential scope mismatch
- OpenAI-compatible Gateway 오류 envelope
- timeout, caller cancellation, network error와 panic recovery
- prompt, Key와 image data의 log redaction

### 통합 테스트

- PostgreSQL service Key → OpenAI Images handler → 선택된 executor → mock upstream 왕복
- OpenAI와 xAI 각각의 native success response byte 보존
- URL, `b64_json`, revised prompt 및 xAI usage 확장 필드 보존
- Provider 400, 401, 429, 500 status/body pass-through
- `Retry-After`와 request ID 보존, `Set-Cookie` 제거
- inbound service Key가 mock upstream 어디에도 존재하지 않음
- 선택되지 않은 Provider credential과 executor가 사용되지 않음
- oversized body는 Provider 호출 전에 거부됨
- health와 Gemini route 회귀 없음

### 호환성 및 장애 테스트

- Python·JavaScript OpenAI SDK 대표 request fixture
- OpenAI와 xAI 공식 문서의 request/response fixture
- Provider credential 미설정 및 authentication store 장애
- connection reset, response header timeout과 truncated response
- client disconnect 후 goroutine·connection leak 없음
- 모든 실패 조건에서 upstream attempt가 0 또는 1
- 로그에 service Key, Provider Key, prompt, URL 및 Base64 image 없음

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

- [x] `POST /v1/images/generations`가 service Key로 보호됨
- [x] OpenAI SDK 요청이 등록 model에 따라 OpenAI 또는 xAI로 전달됨
- [x] Provider 선택이 명시적이고 모호하거나 미등록인 model은 호출 전에 거부됨
- [x] OpenAI/xAI credential이 선택된 fixed origin에만 전달됨
- [x] redirect가 차단되고 retry·fallback이 수행되지 않음
- [x] 요청 body와 Provider success/error 응답이 native 의미를 보존함
- [x] URL, Base64 image와 Provider 확장 필드가 손실되지 않음
- [x] body limit, timeout, cancellation과 연결 실패가 안전하게 처리됨
- [x] service Key, Provider credential, prompt와 image가 로그 및 오류에 없음
- [x] 기존 health, API Key와 Gemini 기능이 회귀 없이 동작함
- [x] 단위·통합·race 테스트 및 CI가 통과함
- [x] README에 SDK Base URL 예제와 정확한 지원 model 범위가 기록됨
- [x] Conformance 저장소에 필요한 공개 계약이 기록됨
- [x] commit, pull request와 CI 증거가 이 계획에 기록됨

## 검증 증거

- 로컬 검증:
  - `make check`: formatter, vet, 전체 race test와 두 binary build 통과
  - `make integration-test`: PostgreSQL service Key → OpenAI Images handler → fixed OpenAI origin → mock transport → native image response 경로 통과
  - `git diff --check`: 통과
  - `go test -cover ./operations/image ./protocols/openai ./providers/openaiimages`: registry 85.0%, protocol 80.6%, transport 80.4%
- 프로토콜 및 장애 검증:
  - 네 가지 service credential 위치, JSON media type, fixed/chunked body limit와 model 중복·누락·미등록 검증
  - OpenAI/xAI exact model routing과 선택되지 않은 executor 미호출
  - URL, `b64_json`, revised prompt, usage 및 xAI 비용 확장 필드의 native 전달
  - Provider native 오류, timeout, caller cancellation, connection error, redirect 차단과 단일 시도 검증
- 보안 검증:
  - OpenAI `https://api.openai.com`, xAI `https://api.x.ai` origin 고정
  - service credential 제거와 선택된 Provider Bearer credential만 적용
  - service Key, Provider credential, prompt와 image data가 로그 및 Gateway 오류에 없음
- 구현 commit: [`0c29ea5`](https://github.com/nativegatewayhq/gateway/commit/0c29ea5)
- pull request: [#5](https://github.com/nativegatewayhq/gateway/pull/5)
- CI:
  - [`check`](https://github.com/nativegatewayhq/gateway/actions/runs/32334378056/job/96320934120): 통과
  - [`validate`](https://github.com/nativegatewayhq/gateway/actions/runs/32334378054/job/96320934302): 통과

## Rollback 계획

- OpenAI Images route 등록, image model registry와 OpenAI/xAI executor 조립을 제거하고 Gemini release로 되돌린다.
- 데이터베이스 migration이 없으므로 schema rollback은 필요하지 않다.
- rollback 전에 신규 image 요청을 중단하고 진행 중 요청의 client connection을 종료한다.
- 이미 Provider에 전달된 timeout 요청을 자동 재호출하거나 상쇄하지 않는다.
- 발생 가능한 Provider 비용은 Provider usage에서 별도 확인한다.

## 후속 작업

1. `conformance` 저장소의 OpenAI Python/JavaScript SDK E2E
2. `POST /v1/images/edits` multipart native pass-through
3. `/v1/models`와 정식 Capability Registry
4. idempotency, Wallet/Ledger 및 Provider 비용 정산
5. priority·weighted·lowest-cost routing과 fallback
6. managed image storage와 CDN
7. image streaming과 partial image SSE
