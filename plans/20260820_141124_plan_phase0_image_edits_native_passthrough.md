---
id: gateway-20260820-006
title: Phase 0 Image Edits Native Pass-through
status: completed
created_at: 2026-08-20T14:11:24+09:00
updated_at: 2026-08-20T14:19:48+09:00
owners:
  - gateway
initiative: phase-0-image-edits-native-e2e
depends_on:
  - gateway-20260820-005
supersedes: []
affected_repos:
  - gateway
  - conformance
---

# Phase 0 Image Edits Native Pass-through

## 목적

`POST /v1/images/edits`에서 OpenAI의 multipart 편집 요청과 xAI의 JSON 편집 요청을 각 Provider의 native wire format 그대로 안전하게 전달한다.

OpenAI 공식 SDK의 `images.edit()`는 service Key와 Gateway Base URL만 변경해 OpenAI Provider를 호출할 수 있어야 한다. xAI는 공식 문서가 OpenAI SDK multipart 편집을 지원하지 않는다고 명시하므로 JSON REST 요청만 지원하며, Gateway가 두 형식 사이를 암묵적으로 변환하지 않는다.

## 배경

`gateway-20260820-005`는 JSON 기반 `POST /v1/images/generations`와 OpenAI/xAI fixed-origin transport를 구현했다. 초기 릴리스의 나머지 이미지 공개 API는 편집이다.

2026-08-20 기준 공식 계약:

- OpenAI Images API: `https://platform.openai.com/docs/api-reference/images`
- xAI Image Editing: `https://docs.x.ai/developers/model-capabilities/images/editing`
- xAI Images REST API: `https://docs.x.ai/developers/rest-api-reference/inference/images`

xAI 공식 문서는 OpenAI SDK `images.edit()`이 multipart를 사용하지만 xAI API는 JSON을 요구하므로 호환되지 않는다고 명시한다. 이 차이는 Capability Registry 이전에도 명시적으로 보존해야 한다.

## 범위

- `POST /v1/images/edits`
- service API Key 인증
- OpenAI `multipart/form-data` native pass-through
- xAI `application/json` native pass-through
- media type과 exact model route의 일치 검증
- multipart 원본 body의 bounded disk spool 및 byte-preserving replay
- JSON 원본 body의 bounded memory pass-through
- OpenAI/xAI fixed-origin Bearer credential 적용
- Provider native success/error response pass-through
- timeout, cancellation, redirect 차단과 단일 시도
- 임시 파일 보안·삭제 및 동시 spool 제한
- OpenAI SDK 예제와 xAI 비호환 경계 문서화

## 제외 범위

- multipart↔JSON 변환
- xAI 편집을 OpenAI SDK `images.edit()`으로 호출하는 호환 계층
- Gateway가 image URL을 fetch하거나 Base64를 decode하는 기능
- mask/image 파일 내용 검사, 리사이즈 또는 MIME sniffing
- streaming partial image 응답
- Capability Registry, 동적 model mapping과 `/v1/models`
- retry, fallback, idempotency와 reconciliation
- Wallet/Ledger 및 과금
- managed storage와 CDN

## 설계 및 구현 순서

### 1. Route와 인증

- OpenAI protocol facade에 exact route `POST /v1/images/edits`를 추가한다.
- 기존 네 가지 service credential 위치를 허용하고 복수 위치를 거부한다.
- 인증 실패 시 body를 읽거나 임시 파일을 만들거나 Provider를 호출하지 않는다.
- 모든 Gateway 오류는 OpenAI-compatible envelope와 `X-Request-Id`를 사용한다.

### 2. Media type별 native 계약

- `multipart/form-data`는 OpenAI 편집 요청으로만 처리한다.
- `application/json`은 xAI 편집 요청으로만 처리한다.
- multipart body의 `model`은 exact OpenAI model route여야 한다.
- JSON body의 최상위 `model`은 exact xAI model route여야 한다.
- 알려진 model이어도 wire format과 Provider가 맞지 않으면 network 호출 전 `400 unsupported_media_type_for_model`로 거부한다.
- model 누락·중복·빈 값·미등록은 생성 endpoint와 같은 공개 오류 원칙을 따른다.

### 3. Multipart bounded spool

- multipart body를 re-encode하지 않는다. 원본 bytes를 mode `0600` 임시 파일에 복사한 뒤 동일 파일을 parsing과 upstream replay에 사용한다.
- 기본 body limit은 `64 MiB`, 운영 상한은 `256 MiB`로 한다.
- `Content-Length` 사전 검사와 `io.LimitReader` 실제 검사 모두 적용한다.
- parsing은 multipart boundary와 text field `model`만 검사한다. image, mask와 prompt 값은 로그나 오류에 포함하지 않는다.
- 파일 part를 메모리에 적재하지 않는다.
- 모든 성공·오류·panic·cancellation 경로에서 file descriptor를 닫고 임시 파일을 삭제한다.
- 동시 spool semaphore 기본값 8을 두고 초과 요청은 body 소비 전 `503 spool_capacity_exhausted`로 거부한다.
- 임시 파일명에 사용자 입력을 포함하지 않는다.

### 4. JSON bounded pass-through

- 기본/최대 body limit은 `64 MiB`로 한다.
- xAI가 URL, data URI, file ID와 다중 image object를 해석하도록 Gateway는 model 외 필드를 변환하지 않는다.
- Gateway는 JSON 내부 URL을 fetch하지 않는다.
- body는 model inspection 후 원본 byte sequence로 replay한다.

### 5. Provider transport 확장

- 기존 OpenAI Images transport에 operation path를 명시적으로 전달하되 허용 path enum만 받는다.
- 허용 path는 `/v1/images/generations`와 `/v1/images/edits` 두 개다.
- OpenAI origin은 `https://api.openai.com`, xAI origin은 `https://api.x.ai`로 고정한다.
- Content-Type boundary, Content-Length를 제외한 안전한 SDK header와 body를 전달한다. Go client가 실제 body 길이에 맞게 전송하도록 한다.
- 선택된 Provider Bearer credential만 적용하고 redirect는 따르지 않는다.
- timeout·cancel·connection error 분류와 response body close 계약은 생성 endpoint와 공유한다.

### 6. Native 응답과 관측성

- Provider status와 response bytes, `Content-Type`, `Retry-After`를 그대로 전달한다.
- URL, Base64, revised prompt, usage와 Provider 확장 필드를 변경하지 않는다.
- 로그는 request ID, protocol, operation=`image.edit`, provider, 검증된 model, status, duration과 failure category만 기록한다.
- credential, prompt, multipart filename, body, URL, Base64와 임시 경로를 기록하지 않는다.
- 요청당 upstream attempt는 최대 한 번이다.

## 인터페이스와 데이터 변경

### 공개 API

```text
POST /v1/images/edits
Content-Type: multipart/form-data  -> OpenAI only
Content-Type: application/json     -> xAI only
```

### 내부 인터페이스

```go
type ImageRequest struct {
    Operation   Operation
    ContentType string
    ContentLength int64
    Body        io.Reader
}
```

Operation은 허용된 generation/edit 값만 갖고 arbitrary upstream path를 허용하지 않는다.

### 데이터베이스 및 migration

없음.

### 다른 저장소에 제공하거나 요구하는 계약

`conformance`는 initiative `phase-0-image-edits-native-e2e`로 다음을 검증한다.

- OpenAI Python·JavaScript SDK multipart `images.edit()`
- 파일명, boundary, image와 mask part 보존
- xAI JSON REST fixture
- xAI OpenAI SDK edit 비호환이 compatibility matrix에 명시됨

## 보안 및 과금 고려사항

- 임시 파일은 전용 prefix, mode `0600`, bounded size와 보장된 삭제를 사용한다.
- body와 파일을 로그에 남기지 않으며 사용자 filename을 filesystem path로 사용하지 않는다.
- 동시 spool 제한으로 disk exhaustion을 제한한다.
- URL은 Provider에 전달만 하고 Gateway가 fetch하지 않아 SSRF surface를 추가하지 않는다.
- service credential은 제거하고 Provider-scoped Bearer만 fixed origin에 적용한다.
- retry/fallback을 금지해 편집 중복과 Provider 비용 중복을 방지한다.
- Wallet/Ledger가 없으므로 사용자 금전 transaction은 만들지 않는다.
- timeout 결과를 실패로 단정하거나 자동 재호출하지 않는다.

## 테스트 계획

### 단위 테스트

- exact route, method, 인증과 오류 envelope
- multipart boundary, model field 누락·중복·크기 제한
- multipart raw bytes와 Content-Type boundary 보존
- JSON model inspection과 원본 bytes 보존
- media type/model Provider mismatch
- spool semaphore 초과, temp file mode와 모든 경로 삭제
- fixed origin, edit path allowlist와 scoped credential
- redirect, timeout, cancellation, connection error와 response close
- 로그 redaction과 panic cleanup

### 통합 테스트

- PostgreSQL service Key → edit handler → OpenAI mock transport multipart 왕복
- PostgreSQL service Key → edit handler → xAI mock transport JSON 왕복
- image/mask bytes가 메모리 변환 없이 동일하게 전달됨
- Provider 400, 401, 429, 500 native response 전달
- body limit 전 Provider 호출 0회
- health, Gemini와 image generation 회귀 없음

### 호환성 및 장애 테스트

- OpenAI SDK multipart fixture
- xAI 공식 JSON fixture
- client disconnect, temp write/read failure와 truncated upstream response
- process panic 뒤 temp artifact 부재
- service/Provider credential, prompt, filename, body와 temp path 로그 부재

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

- [x] OpenAI multipart와 xAI JSON edit 요청이 exact model로 분리됨
- [x] OpenAI SDK multipart 요청이 Key와 Base URL 변경만으로 Gateway contract에 도달함
- [x] multipart와 JSON body가 Provider native 형식으로 보존됨
- [x] 임시 파일이 bounded·0600이며 모든 종료 경로에서 삭제됨
- [x] spool 동시성 제한과 body limit이 Provider 호출 전에 적용됨
- [x] Provider credential이 fixed origin 하나에만 전달됨
- [x] redirect, retry와 fallback이 수행되지 않음
- [x] native success/error response와 확장 필드가 보존됨
- [x] timeout과 cancellation을 확정 실패로 재시도하지 않음
- [x] credential, prompt, filename, body, URL과 temp path가 로그에 없음
- [x] 기존 API 회귀 없이 전체 race/integration/CI 통과
- [x] README와 Conformance 계약에 SDK 호환 경계가 기록됨
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 로컬 검증:
  - `make check`: formatter, vet, 전체 race test와 binary build 통과
  - `make integration-test`: PostgreSQL service Key를 사용하는 OpenAI multipart 및 xAI JSON edit 경로 통과
  - `git diff --check`: 통과
  - `go test -cover ./protocols/openai ./providers/openaiimages`: protocol 77.4%, transport 81.5%
- 보안 및 장애 검증:
  - multipart 원본 byte·boundary 보존, bounded spool, mode `0600`, Provider 실패 후 삭제
  - spool capacity와 fixed/chunked body 제한을 upstream 호출 전에 적용
  - exact model/media routing, fixed origin, scoped Bearer, redirect 차단과 edit path allowlist
  - xAI JSON URL·확장 usage와 Provider native success/error body 무변환 전달
- 구현 commit: [`27d98aa`](https://github.com/nativegatewayhq/gateway/commit/27d98aa)
- pull request: [#6](https://github.com/nativegatewayhq/gateway/pull/6)
- CI:
  - [`check`](https://github.com/nativegatewayhq/gateway/actions/runs/32335114852/job/96323058341): 통과
  - [`validate`](https://github.com/nativegatewayhq/gateway/actions/runs/32335114859/job/96323058606): 통과

## Rollback 계획

- `/v1/images/edits` route와 edit transport path를 제거해 generation-only release로 되돌린다.
- 신규 DB migration이 없으므로 schema rollback은 없다.
- rollback 시 신규 edit 요청을 중단하고 열린 request와 임시 파일 cleanup 완료를 확인한다.
- 이미 Provider에 전달된 timeout 요청은 자동 재호출하지 않는다.

## 후속 작업

1. Conformance 저장소의 OpenAI SDK multipart E2E와 xAI JSON fixture
2. 정식 Capability Registry와 `/v1/models`
3. Wallet/Ledger, idempotency와 edit 비용 정산
4. managed image storage와 CDN
5. routing과 fallback
