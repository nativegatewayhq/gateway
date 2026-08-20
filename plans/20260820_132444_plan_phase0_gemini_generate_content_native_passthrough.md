---
id: gateway-20260820-004
title: Phase 0 Gemini generateContent Native Pass-through
status: completed
created_at: 2026-08-20T13:24:44+09:00
updated_at: 2026-08-20T13:51:33+09:00
owners:
  - gateway
initiative: phase-0-gemini-sdk-e2e
depends_on:
  - gateway-20260820-003
supersedes: []
affected_repos:
  - gateway
  - conformance
---

# Phase 0 Gemini generateContent Native Pass-through

## 목적

Google Gen AI 공식 SDK가 서비스 API Key와 Gateway Base URL만 사용해 Gemini Developer API의 non-streaming `models.generateContent`를 호출할 수 있도록 Gemini native protocol facade와 Google transport를 구현한다.

Gateway는 inbound 서비스 Key를 검증하고 Google Provider credential로 교체하지만, 성공 및 Provider 오류의 JSON payload는 변환하지 않고 최대한 그대로 전달한다. 이 계획의 핵심 결과는 이미지 출력을 포함한 `generateContent` 요청이 Google native 의미를 유지한 채 안전하게 왕복하는 것이다.

## 배경

`gateway-20260820-002`는 SDK별 inbound credential 형식을 서비스 API Key 인증으로 통합했고, `gateway-20260820-003`은 inbound credential 제거와 Provider-scoped upstream credential 주입 경계를 만들었다. 이제 실제 Provider 호출을 처음 연결해 공식 SDK가 Gateway를 API endpoint로 사용할 수 있는지 검증해야 한다.

Google Gen AI SDK는 기본적으로 beta API endpoint를 사용하며 `models.generate_content` 호출을 제공한다. Google Gemini Developer API의 REST 계약은 모델 resource에 대한 `generateContent` 요청과 native `GenerateContentResponse`/Google error payload를 정의한다. 이 계획은 2026-08-20에 확인한 다음 공식 자료를 기준으로 한다.

- Google Gemini API `generateContent`: `https://ai.google.dev/api/generate-content`
- Google Gen AI Python SDK: `https://github.com/googleapis/python-genai`

SDK 자체를 설치해 검증하는 작업은 `conformance` 저장소가 소유한다. Gateway 저장소에서는 SDK가 생성하는 HTTP 계약과 mock upstream 왕복을 검증한다.

## 범위

- Gemini native route
  - `POST /v1beta/models/{model}:generateContent`
- 기존 네 가지 서비스 API Key 형식을 통한 route 보호
- Gemini protocol 전용 Gateway 오류 envelope
- Google `generativelanguage.googleapis.com` trusted upstream transport
- Google Provider credential의 `x-goog-api-key` 주입
- inbound service credential, cookie, proxy 및 hop-by-hop header 제거
- Gemini 요청 body의 byte-preserving bounded pass-through
- 허용된 query parameter 보존과 credential query 제거
- Google 응답 status, JSON body와 호환성 header pass-through
- request timeout, client cancellation 및 upstream 연결 오류 처리
- redirect를 따르지 않는 정책
- 자동 retry와 fallback을 수행하지 않는 단일 시도 정책
- mock Google upstream 기반 handler·transport 통합 테스트
- Python/JavaScript Conformance 저장소가 사용할 공개 HTTP 계약 문서화

## 제외 범위

- `streamGenerateContent`와 SSE
- Gemini Live API, WebSocket 및 bidi streaming
- `models.list`, file upload, batch, cache와 token count API
- OpenAI Images protocol과 xAI/OpenAI transport
- 다른 Provider로 Gemini 요청을 변환하는 cross-provider routing
- 요청/응답 JSON schema 변환 또는 필드 filtering
- model alias, capability registry와 동적 model mapping
- Wallet, Ledger, 가격 추정, reserve/capture/refund
- retry, fallback, circuit breaker와 reconciliation
- managed image storage, Base64 추출, S3/R2 및 CDN
- 공식 SDK 설치와 실제 Google credential을 사용하는 live test

`streamGenerateContent`, 과금과 managed storage는 각각 후속 계획으로 분리한다.

## 설계 및 구현 순서

### 1. Gemini protocol route와 handler 조립

- `protocols/gemini` 또는 현재 모듈 구조에 맞는 독립 package를 추가한다.
- route는 정확히 `POST /v1beta/models/{model}:generateContent`만 처리한다.
- 다른 method는 Go router의 method semantics를 따르되 Gemini-compatible 오류 body를 반환한다.
- `{model}`은 단일 path segment여야 하며 빈 값, slash, 제어 문자, 비정상적인 길이와 잘못된 percent encoding을 거부한다.
- handler는 다음 dependency를 명시적으로 주입받는다.
  - 서비스 API Key authenticator
  - Google Provider credential registry
  - Google transport/executor
  - request timeout과 최대 body 크기
- health endpoint는 기존처럼 인증 없이 유지한다.
- Gateway-owned 404는 기존 형식을 유지하고, Gemini route에 도달한 뒤 발생한 오류만 Gemini envelope를 사용한다.

### 2. 서비스 API Key 인증 연결

- `main`은 기존 PostgreSQL API Key store와 authentication service를 조립한다.
- Gemini route는 Provider 요청을 만들기 전에 서비스 API Key를 인증한다.
- 다음 입력 위치를 기존 공통 extractor로 지원한다.
  - `Authorization: Bearer SERVICE_KEY`
  - `x-api-key: SERVICE_KEY`
  - `x-goog-api-key: SERVICE_KEY`
  - `?key=SERVICE_KEY`
- 인증에 사용한 header와 query는 upstream으로 전달하지 않는다.
- 인증 실패 시 body를 읽거나 Google network 요청을 시작하지 않는다.
- authenticated principal에는 API Key ID만 유지하고 원문 Key를 handler context 밖으로 전달하지 않는다.

### 3. Gemini 요청 계약

- `Content-Type`은 JSON media type만 허용하고 charset parameter는 안전하게 parsing한다.
- 요청 body는 JSON을 구조체로 decode/re-encode하지 않고 원본 byte sequence를 보존한다.
- body가 비었거나 malformed JSON이어도 Gateway에서 JSON 의미를 검증하지 않는다. body 크기와 media type만 검증하고 native 오류 판단은 Google에 맡긴다.
- `GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES`의 기본값은 inline image 편집 요청을 고려해 `33554432`(`32 MiB`)로 두며, 운영자는 양의 정수 범위에서 더 낮게 설정할 수 있다.
- 명시된 `Content-Length`가 제한을 넘으면 upstream 호출 전에 거부한다.
- chunked body도 제한을 초과할 수 없도록 bounded read를 적용한다.
- 제한 내 body는 단일 요청의 replay나 retry를 위해 보관하지 않는다. 이 계획은 retry하지 않으며 메모리 사용 상한을 명확히 유지한다.
- `Content-Type`, `Accept`, `User-Agent`와 공식 SDK 식별용 `x-goog-api-client`만 upstream header allowlist에 포함한다.
- `Authorization`, API Key 계열, Cookie, Proxy authorization, forwarding header와 hop-by-hop header는 전달하지 않는다.

### 4. Trusted Google upstream 요청 생성

- production upstream origin은 compile-time 또는 검증된 내부 기본값 `https://generativelanguage.googleapis.com`으로 고정한다.
- incoming scheme, host, userinfo, fragment와 `Host` field를 upstream URL에 재사용하지 않는다.
- path는 검증된 model segment로 `v1beta/models/{model}:generateContent`를 새로 구성한다.
- inbound query에서 `key`, `api_key`, `access_token`, `token`을 제거하고, native 동작에 필요한 비민감 query만 보존한다.
- outbound request의 `RequestURI`와 `Host` override를 비우고 trusted origin의 TLS hostname 검증을 사용한다.
- `gateway-20260820-003`의 Provider credential boundary를 통해 Google credential을 `x-goog-api-key` 하나로 적용한다.
- test에서는 package-private constructor 또는 injected transport로 mock origin을 사용한다. production 환경 변수로 임의 origin을 허용하지 않는다.
- HTTP redirect는 status와 location에 관계없이 따르지 않아 Provider credential이 다른 origin으로 전송되지 않게 한다.

### 5. Timeout, cancellation과 단일 시도

- `GATEWAY_GOOGLE_REQUEST_TIMEOUT`을 추가하고 기본값은 `2m`, 허용 범위는 양수부터 `10m`까지로 제한한다.
- client disconnect와 server shutdown context가 Google 요청으로 전파되어야 한다.
- connect, TLS handshake, response header timeout을 무한대로 두지 않는다.
- `generateContent`는 생성 작업을 포함할 수 있으므로 Gateway는 timeout, connection reset 또는 5xx에 자동 retry하지 않는다.
- 이 단계에는 Provider job ID와 reconciliation이 없으므로 timeout은 upstream 결과가 불명확한 상태로 기록하고 `504 DEADLINE_EXCEEDED`를 반환한다.
- client cancellation 이후 응답 writer에 추가 body를 쓰거나 성공으로 기록하지 않는다.
- transport는 매 요청 response body를 정확히 한 번 닫고 connection reuse를 방해하지 않아야 한다.

### 6. Native 응답 pass-through

- Google이 응답을 반환한 경우 HTTP status와 body byte sequence를 변환하지 않는다.
- 최소 응답 header allowlist:
  - `Content-Type`
  - `Content-Length`는 실제 전달 방식과 일치할 때만 사용
  - `Retry-After`
  - Google request/trace 식별 header 중 credential이 아닌 검토된 항목
- `Set-Cookie`, hop-by-hop header, upstream server 내부 정보와 인증 관련 header는 제거한다.
- 이미지 part의 inline Base64, MIME type, finish reason, safety rating과 usage metadata를 decode하거나 변경하지 않는다.
- Google 4xx/5xx error body도 native body 그대로 전달해 공식 SDK의 `APIError` parsing을 보존한다.
- upstream이 status를 보내기 전에 실패한 경우에만 Gateway가 Gemini 오류 envelope를 생성한다.
- response streaming copy 중 연결이 끊기면 이미 전달한 status를 다른 오류로 덮어쓰지 않고 관측성 event만 남긴다.

### 7. Gemini 전용 Gateway 오류 형식

Gateway가 생성하는 pre-upstream 오류는 Google RPC 스타일과 SDK parsing에 호환되는 최소 envelope를 사용한다.

```json
{
  "error": {
    "code": 401,
    "message": "authentication required",
    "status": "UNAUTHENTICATED"
  }
}
```

초기 mapping:

| 조건 | HTTP | status |
|---|---:|---|
| credential 누락·잘못됨·만료 | 401 | `UNAUTHENTICATED` |
| 복수 credential 위치·잘못된 model·media type·body limit | 400 또는 413 | `INVALID_ARGUMENT` 또는 `RESOURCE_EXHAUSTED` |
| Google credential 미설정 | 503 | `UNAVAILABLE` |
| upstream 연결 실패 | 502 | `UNAVAILABLE` |
| Gateway timeout | 504 | `DEADLINE_EXCEEDED` |
| 내부 panic | 500 | `INTERNAL` |

- 외부 오류는 API Key 존재 여부, Google credential, upstream URL, 내부 package와 stack trace를 포함하지 않는다.
- 모든 응답에는 기존 `X-Request-Id`를 유지한다.

### 8. 관측성

- 최소 기록: request ID, protocol=`gemini`, operation=`generateContent`, provider=`google`, model, status, duration과 실패 category.
- 서비스 API Key, Google API Key, raw query, request/response body, inline image와 Google error message는 기록하지 않는다.
- model은 길이와 문자 validation 후에만 log attribute로 사용한다.
- metric label에 request ID, model 전체 문자열, 오류 message 또는 credential을 사용하지 않는다.
- upstream request count를 검증해 retry가 없음을 테스트한다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1beta/models/{model}:generateContent
```

예시 SDK 계약:

```python
from google import genai
from google.genai import types

client = genai.Client(
    api_key="SERVICE_API_KEY",
    http_options=types.HttpOptions(
        base_url="https://gemini.api.example.com"
    ),
)

response = client.models.generate_content(
    model="gemini-image-model",
    contents="고양이 우주비행사를 그려줘",
    config=types.GenerateContentConfig(response_modalities=["IMAGE"]),
)
```

정확한 SDK 생성 path는 Conformance 계획에서 고정한다. SDK 버전에 따라 custom Base URL resource scope 설정이 필요하면 Gateway 공개 path를 변경하기보다 사용 예제와 호환 matrix에 명시한다.

### 내부 인터페이스

책임 경계는 다음과 같다.

```go
type GoogleExecutor interface {
    GenerateContent(ctx context.Context, request GenerateContentRequest) (*http.Response, error)
}

type GenerateContentRequest struct {
    Model       string
    Query       url.Values
    ContentType string
    Body        io.Reader
}
```

- protocol handler는 인증과 Gemini 오류 mapping을 소유한다.
- Google executor는 trusted URL, safe header, Provider credential, timeout과 HTTP 실행을 소유한다.
- request/response JSON schema를 공통 Operation model로 변환하지 않는다.

### 데이터베이스 및 migration

없음.

기존 `service_api_keys`와 Google environment credential을 사용한다. 요청 이력, 과금과 usage 저장은 별도 계획에서 추가한다.

### 다른 저장소에 제공하거나 요구하는 계약

`conformance` 저장소는 동일한 initiative `phase-0-gemini-sdk-e2e`로 별도 계획을 작성한다.

Gateway가 제공하는 계약:

- `POST /v1beta/models/{model}:generateContent`
- 서비스 Key를 받는 Gemini SDK 인증 위치
- native Google success/error response pass-through
- `X-Request-Id`

Conformance 저장소가 검증할 항목:

- Google Gen AI Python 및 JavaScript SDK 버전 고정
- Base URL과 Key만 변경한 sync non-streaming 호출
- image response part decoding
- native error parsing
- SDK 업데이트에 따른 path 및 header drift

## 보안 및 과금 고려사항

- inbound 서비스 Key는 Google에 전달하지 않고 Google credential은 사용자 응답과 로그에 노출하지 않는다.
- trusted upstream origin을 고정하고 redirect를 차단해 SSRF와 credential exfiltration을 방지한다.
- inbound header allowlist를 사용해 forwarding spoof, cookie와 proxy credential 전파를 막는다.
- body limit와 timeout으로 Base64 이미지 및 느린 client 자원 고갈을 제한한다.
- 응답 body는 native pass-through하므로 사용자에게 전달될 Google payload 외 별도 저장이나 로그를 만들지 않는다.
- 자동 retry를 금지해 하나의 논리 요청이 중복 생성·중복 Provider 비용을 발생시키지 않게 한다.
- 이 단계에는 Wallet/Ledger가 없으므로 금전 transaction을 만들지 않는다. Provider 비용이 발생하는 공개 운영 배포는 과금 계획 완료 전 제한해야 한다.
- timeout을 확정 실패로 간주하거나 자동 재호출하지 않는다. 과금 도입 전에도 ambiguous outcome으로 관측한다.

## 테스트 계획

### 단위 테스트

- route와 model segment validation
- Gemini Gateway 오류 envelope 및 status mapping
- JSON media type과 body 크기 제한
- 네 가지 서비스 credential 위치 인증
- 인증 실패 시 body 미소비 및 executor 미호출
- query credential 제거와 safe query 보존
- safe request header allowlist
- trusted upstream URL 구성과 Host/Userinfo 제거
- Google credential만 `x-goog-api-key`에 적용
- response header allowlist
- timeout 및 transport error classification

### 통합 테스트

- PostgreSQL에 생성한 서비스 Key로 mock Google upstream 호출
- request path, query, content type과 body byte sequence 보존
- inbound 서비스 Key가 mock upstream 어디에도 존재하지 않음
- Google Provider credential만 upstream에 존재함
- image inline data를 포함한 success JSON의 byte-preserving 왕복
- Google 400, 401, 429와 5xx status/body pass-through
- `Retry-After` 보존과 `Set-Cookie` 제거
- redirect 응답을 따라가지 않음
- timeout, client cancellation, connection reset과 truncated response
- oversized fixed-length 및 chunked body 거부
- upstream request count가 항상 한 번 이하임
- health endpoint는 계속 인증 없이 동작함

### 호환성 및 장애 테스트

- Python/JavaScript SDK가 생성하는 대표 request fixture
- API version이 다른 path는 명시적인 미지원 오류 또는 404
- Google credential 미설정 시 network 요청 없는 `503`
- PostgreSQL 인증 store 장애 시 Google 호출 없는 `503`
- Gateway 재시작 후 동일 API Key로 재호출 가능
- response copy 중 client disconnect가 goroutine·connection leak을 만들지 않음
- 로그에 service Key, Google Key, prompt와 inline image가 없음

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

- [x] `POST /v1beta/models/{model}:generateContent`가 서비스 API Key로 보호됨
- [x] 네 가지 inbound 인증 형식이 Google credential 하나로 안전하게 교체됨
- [x] trusted Google origin 외 다른 host로 credential을 전송할 수 없음
- [x] redirect가 차단되고 자동 retry가 수행되지 않음
- [x] 요청 path, safe query, content type과 body가 native 의미를 보존함
- [x] 이미지 inline data를 포함한 Google success response가 변환 없이 전달됨
- [x] Google native error status와 body가 변환 없이 전달됨
- [x] Gateway-generated 오류가 Gemini SDK에서 해석 가능한 envelope를 사용함
- [x] body limit, timeout, cancellation과 upstream 연결 실패가 안전하게 처리됨
- [x] 서비스 Key, Google credential, prompt와 image data가 로그에 없음
- [x] health endpoint와 기존 API Key 기능이 회귀 없이 동작함
- [x] 단위·통합·race 테스트가 통과함
- [x] formatter, vet, build와 integration test가 CI에서 통과함
- [x] README에 Gemini SDK Base URL 설정 예제와 지원 범위가 기록됨
- [x] Conformance 저장소에 필요한 공개 계약과 후속 initiative가 기록됨
- [x] 검증 증거가 이 계획에 기록됨

## 검증 증거

- 로컬 검증:
  - `make check`: formatter, vet, 전체 race test와 두 binary build 통과
  - `make integration-test`: PostgreSQL 서비스 Key → Gemini handler → fixed Google origin → mock transport → native image response 경로 통과
  - `git diff --check`: 통과
  - `go test -cover ./protocols/gemini ./providers/google`: Gemini 86.2%, Google transport 93.2%
- 프로토콜 및 장애 검증:
  - 네 가지 서비스 credential 위치, body 크기, media type, model과 method 검증
  - 요청 path, safe query, content type, malformed JSON body의 byte-preserving 전달
  - image inline data 및 Google 400·401·429·500 native body/status 전달
  - redirect 차단, timeout, caller cancellation, connection reset, truncated response와 단일 시도 검증
- 보안 검증:
  - trusted origin `https://generativelanguage.googleapis.com` 고정
  - service Key query/header 제거 및 Google `x-goog-api-key`만 적용
  - service Key, Google credential, prompt와 inline image가 로그 및 Gateway 오류에 없음
- 구현 commit: [`35fd90a`](https://github.com/nativegatewayhq/gateway/commit/35fd90a)
- pull request: [#4](https://github.com/nativegatewayhq/gateway/pull/4)
- CI:
  - [`check`](https://github.com/nativegatewayhq/gateway/actions/runs/32333317842/job/96317968351): 통과
  - [`validate`](https://github.com/nativegatewayhq/gateway/actions/runs/32333317825/job/96317969553): 통과

## Rollback 계획

- Gemini route 등록과 Google executor 조립을 제거하고 Provider credential boundary release로 되돌린다.
- 데이터베이스 migration이 없으므로 schema rollback은 필요하지 않다.
- rollback 전에 진행 중 request를 종료하고 신규 Gemini 요청 수락을 중단한다.
- 이미 Google에 전달된 timeout 요청의 결과를 재호출로 상쇄하지 않는다.
- 공개 운영에서 Provider 비용이 발생했다면 외부 Provider usage를 별도로 확인하고 자동 환불을 가정하지 않는다.

## 후속 작업

1. `conformance` 저장소의 Google Gen AI Python/JavaScript SDK E2E
2. OpenAI `/v1/images/generations` native pass-through 및 OpenAI/xAI transport
3. Gemini `streamGenerateContent`와 SSE
4. Capability Registry와 image model metadata
5. Wallet/Ledger 및 요청별 Provider 비용 정산
6. managed image storage와 CDN
