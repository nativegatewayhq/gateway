---
id: gateway-20260820-014
title: Phase 1 Gemini Image Billing and Idempotency
status: completed
created_at: 2026-08-20T16:13:42+09:00
updated_at: 2026-08-20T16:33:11+09:00
owners:
  - gateway
initiative: phase-1-gemini-image-billing
depends_on:
  - gateway-20260820-007
  - gateway-20260820-010
  - gateway-20260820-011
  - gateway-20260820-012
  - gateway-20260820-013
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
---

# Phase 1 Gemini Image Billing and Idempotency

## 목적

공식 Google Gen AI SDK가 호출하는 Gemini `generateContent` 이미지 요청에 기존 native request/response 형식을 유지하면서 가격 견적, Wallet 예약·정산, `Idempotency-Key` replay와 timeout reconciliation을 적용한다. Capability Registry에 이미지 생성 모델로 등록된 요청만 과금해 동일 endpoint를 사용하는 일반 텍스트 요청의 오분류와 오과금을 막는다.

## 배경

Gateway는 Gemini native pass-through를 지원하지만 과금 lifecycle은 OpenAI/xAI 이미지 경로에만 연결되어 있다. `generateContent`는 이미지 전용 endpoint가 아니므로 model path와 capability를 함께 확인하지 않고 body 형태만으로 작업을 추론하면 LLM 요청을 이미지로 청구할 수 있다. Phase 1 이미지 MVP를 완성하려면 Google image model/channel을 registry에 명시하고 Gemini native body에서 가격 차원만 안전하게 추출한 뒤 이미 검증된 Billing, replay와 reconciliation 경계를 재사용해야 한다.

## 범위

- Capability Registry의 Google Provider 및 Gemini JSON image generation route
- `protocol + operation + model + capability` 기반 Gemini 이미지 요청 판별
- Gemini native request의 가격 selector 추출
- model path를 canonical model로 사용하고 body가 model identity를 덮어쓰지 못하게 함
- 초기 수량 `1`, 기본 및 명시적 aspect ratio/image size 가격 차원
- Billing required mode의 Estimate→Reserve→Google→Capture/Release
- `Idempotency-Key` 검증, wire request fingerprint와 native response replay
- Gemini native 성공·오류 envelope/header/body 보존
- response loss와 settlement failure의 known reconciliation observation
- timeout, connection loss와 panic의 unknown reconciliation observation
- billing disabled mode의 기존 raw pass-through 보존
- Google/Gemini handler 단위·PostgreSQL 통합·process 테스트
- README 및 Conformance/Cloud handoff

## 제외 범위

- 일반 Gemini text/chat 요청 과금
- `streamGenerateContent`와 SSE
- Gemini image edit 또는 multi-turn image editing의 별도 operation
- body semantic validation, prompt moderation과 safety policy 변환
- response를 파싱해 실제 생성 이미지 수를 사후 정산하는 가변 과금
- cross-provider protocol conversion과 fallback
- managed storage/CDN 업로드
- 공개 가격·Wallet·reconciliation 관리 API
- Google SDK 실제 credential을 사용하는 live test

## 설계 및 구현 순서

### 1. Google image capability route

- image `ModelRoute`가 Google Provider를 허용하고 Gemini JSON `image.generate` capability와 canonical channel ID를 가진다.
- Gemini handler는 path model을 registry에서 resolve한 경우에만 billable image flow로 진입한다.
- 등록되지 않은 model은 billing disabled에서는 기존 pass-through를 유지한다.
- billing required에서 비이미지 Gemini 요청은 이 이미지 과금 계획이 지원하지 않음을 native `FAILED_PRECONDITION`으로 fail closed하고 Provider를 호출하지 않는다. 관리형 서비스가 텍스트를 무과금 통과시키지 않게 한다.
- model not found와 capability mismatch는 client가 channel/provider를 선택할 수 없는 안정적인 native 오류로 변환한다.

### 2. Gemini 가격 selector

- model은 URL path에서만 가져오고 request JSON은 원본 bytes를 그대로 Provider에 전달한다.
- selector parser는 JSON object와 중복 key를 검증하고 가격 결정에 필요한 `generationConfig.imageConfig.aspectRatio`, `generationConfig.imageConfig.imageSize`만 읽는다.
- 누락 값은 각각 `default`로 canonicalize하며 초기 quantity는 항상 `1`이다.
- selector 값은 길이·문자 집합을 제한하고 number/object/null 등 예상하지 않은 타입을 가격 없음으로 처리한다.
- unknown request field와 contents는 저장·로그·변환하지 않는다.

### 3. Gemini billable orchestration

- 인증 성공 Principal의 organization/project, request ID, protocol `gemini`, operation `image.generate`, registry channel과 selector를 Billing `Begin`에 전달한다.
- Billing request validation을 Gemini protocol까지 확장하되 OpenAI/xAI identity와 기존 charge semantics는 변경하지 않는다.
- Reserve commit 이전에는 Google executor를 호출하지 않는다.
- Google 2xx는 Capture, native non-2xx는 Release 후 원본 response를 반환한다.
- credential unavailable처럼 Provider 미호출이 확실한 오류만 Release하고 native `UNAVAILABLE`을 반환한다.

### 4. Idempotency와 native replay

- 선택적 `Idempotency-Key`는 기존 visible ASCII/200-byte 계약을 재사용한다.
- fingerprint는 method, escaped path model, secret-bearing `key`를 제거한 canonical query, content type와 원본 body bytes를 포함한다.
- service API key 위치나 원문은 fingerprint, charge, snapshot과 로그에 포함하지 않는다.
- 같은 organization/key와 동일 fingerprint는 Provider 및 Wallet 변경 없이 Gemini response를 replay한다.
- 다른 request는 Gemini native `ALREADY_EXISTS` 또는 동등한 안정적 conflict envelope를 반환한다.
- replay snapshot allowlist에는 `Content-Type`, `Retry-After`, 안전한 `X-Goog-Request-Id`만 포함하고 인증 관련 header는 제외한다.

### 5. Reconciliation 분류

- Provider status와 complete response를 관측한 2xx/non-2xx settlement failure는 각각 known success/failure observation으로 저장한다.
- status 이후 body read 실패와 body 상한 초과는 known outcome과 안전한 Gemini `UNAVAILABLE` snapshot을 기록한다.
- Google timeout, connection loss, cancel 원인이 client cancellation으로 확정되지 않은 경우와 panic은 unknown으로 기록하며 예약금을 자동 Release하지 않는다.
- client가 Provider 호출 전 취소하면 Release하고, 호출 시작 이후 취소 결과가 불명확하면 unknown으로 보수적으로 처리한다.
- worker가 terminal resolve한 뒤 동일 key retry는 저장된 Gemini snapshot을 replay한다.

### 6. Handler 구조와 배포 mode

- unbilled native path와 billable path가 인증, bounded body read, executor header 구성과 response copy 규칙을 공유하게 한다.
- Billing dependency는 명시적 constructor로만 주입하며 nil은 disabled semantics를 유지한다.
- billing required process는 Gemini handler에도 동일 Billing service를 전달한다.
- Gateway log에는 protocol, operation, provider, 안전한 model, status와 category만 기록한다.

## 인터페이스와 데이터 변경

### 공개 API

기존 경로와 Google native schema를 유지한다.

```text
POST /v1beta/models/{model}:generateContent
Idempotency-Key: optional-visible-ascii-key
```

replay 성공 시 `Idempotency-Replayed: true`를 추가한다. Gateway 자체 과금 오류는 HTTP status와 Gemini `error.status`가 일치하는 native envelope로 반환한다.

### 내부 인터페이스

```go
type GeminiPricingSelector struct {
    Quantity int64
    AspectRatio string
    ImageSize string
}

NewBillableHandler(logger, authenticator, registry, executor, maxBodyBytes, billing)
```

기존 `billing.BeginRequest`는 `Protocol=gemini`, `Operation=image.generate`를 허용한다. response snapshot과 reconciliation interface는 새 별도 금전 구현 없이 기존 서비스를 재사용한다.

### 데이터베이스 및 migration

새 migration은 원칙적으로 없다. 기존 provider channel, price, image charge, response snapshot과 reconciliation schema에 Gemini row를 저장한다. 구현 중 schema 변경이 필요해지면 이 계획을 수정하지 않고 별도 `change` 계획으로 승인받는다.

### 다른 저장소에 제공하거나 요구하는 계약

- Conformance는 initiative `phase-1-gemini-image-billing`으로 Google Gen AI Python/JavaScript SDK의 Base URL+Key, optional idempotency replay와 native error 호환 테스트를 소유한다.
- Cloud는 Google image channel과 `protocol=gemini`, `operation=image.generate`, exact model/aspect ratio/image size 가격을 Gateway 시작 전 publish하고 Wallet을 funding한다.
- Cloud는 등록되지 않은 Gemini text model을 이미지 가격으로 publish하지 않는다.

## 보안 및 과금 고려사항

- `key` query와 API-key header는 fingerprint 전에 제거하며 plaintext를 어느 테이블에도 저장하지 않는다.
- prompt, contents, inline image bytes와 전체 request body는 charge/reconciliation/log에 저장하지 않는다.
- request body는 fingerprint 계산 중에만 bounded memory에 유지하고 Provider에는 동일 bytes를 전달한다.
- Provider 호출은 Reserve commit 후 한 번만 수행한다.
- Gemini endpoint의 modality ambiguity는 Registry allowlist로 해결하고 client body만으로 과금 operation을 결정하지 않는다.
- known success를 Release하거나 known failure를 Capture하지 않는다.
- unknown outcome은 worker 정책에 따라 reservation을 유지하며 자동 환불하지 않는다.
- replay와 concurrent retry는 동일 Wallet/Ledger effect 하나만 허용한다.

## 테스트 계획

### 단위 테스트

- Gemini selector default/aspect ratio/image size와 duplicate/type/size 오류
- Google image route와 unsupported/non-image model 판별
- query credential 제거 및 fingerprint 안정성
- billing 오류의 Gemini native status 매핑
- 2xx/non-2xx/timeout/connection/cancel/panic outcome 분류
- billing disabled raw byte pass-through 회귀

### 통합 테스트

- 충분한 잔액의 Gemini Reserve→Google 2xx→Capture와 native response
- Google 4xx/5xx→Release와 native error replay
- price 없음, insufficient funds와 unsupported model에서 upstream 미호출
- 같은 key retry의 단일 reserve/capture와 response replay
- 다른 body/model/options key reuse conflict
- response loss known success/failure의 reconciliation resolve
- timeout unknown의 reservation 유지와 retry pending/manual review
- tenant/project 비활성화와 channel/price 변경이 기존 charge replay에 영향 없음
- query/header credential이 charge, snapshot, reconciliation과 log에 존재하지 않음

### 호환성 및 장애 테스트

- Google Gen AI Python/JavaScript SDK가 Base URL과 Key만 변경한 mock E2E handoff
- Gateway 재시작 후 pending reconciliation 처리와 terminal replay
- concurrent identical request의 Provider 단일 호출 또는 stable in-progress conflict
- DB failure, worker failure와 Provider timeout에서 이중 과금 없음
- OpenAI/xAI billable image 및 Gemini unbilled pass-through 회귀 없음

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

- [x] Google image model/channel/capability가 Registry에서 resolve됨
- [x] Gemini selector가 path model과 canonical 가격 차원을 안전하게 추출함
- [x] 등록된 Gemini image request만 이미지 과금 lifecycle로 진입함
- [x] billing required의 Reserve→Capture/Release가 Wallet/Ledger와 일치함
- [x] Reserve 이전 실패에서 Google Provider가 호출되지 않음
- [x] native Gemini 성공·오류 response가 정산 이후 보존됨
- [x] Idempotency-Key 동일 retry가 response를 replay하고 이중 과금하지 않음
- [x] credential 위치 차이가 fingerprint나 저장 데이터에 secret을 남기지 않음
- [x] known/unknown outcome이 reconciliation 불변 조건대로 처리됨
- [x] billing disabled mode와 OpenAI/xAI 경로 회귀가 없음
- [x] 전체 race/integration/CI 통과
- [x] README와 Conformance/Cloud 계약이 기록됨
- [x] commit, PR과 CI 증거가 기록됨

## 검증 증거

- 계획 commit: `3e1ad3ad0cdd47666f5020a315f35114a2c9623d`
- schema change 계획 commit: `535fbc8`
- 구현 commit: `a08a1ad2c825a21f7a4460720032febf5175097c`
- Pull Request: [#14](https://github.com/nativegatewayhq/gateway/pull/14)
- CI: [check run 32344425839](https://github.com/nativegatewayhq/gateway/actions/runs/32344425839) 및 [Plan policy run 32344447958](https://github.com/nativegatewayhq/gateway/actions/runs/32344447958) 통과
- 로컬 `make check` 통과
- `TEST_DATABASE_URL=... make integration-test` 통과
- PostgreSQL integration에서 default 및 16:9/2K exact price, Capture, native 429 Release/replay, timeout UNKNOWN reservation, response-loss known-success resolve/replay와 protocol identity immutability를 검증함
- 단위/race test에서 protocol namespace 격리, duplicate/type selector 거부, credential-location-independent fingerprint, panic/cancel/settlement outcome 분류와 billing-disabled pass-through 회귀를 검증함

## Rollback 계획

- managed traffic에서 Gemini image route를 중단하고 이전 binary로 rollback하되 charge, Wallet, Ledger와 reconciliation row를 유지한다.
- billing required를 disabled로 내려 유료 요청을 무과금 통과시키지 않는다.
- 이미 terminal 처리된 row를 수정·삭제하지 않고 필요한 금전 보정은 검토된 append-only entry 후속 절차로 수행한다.
- schema migration이 없으므로 이전 binary는 Gemini protocol charge를 읽지 않고 기존 OpenAI/xAI lifecycle을 계속 처리할 수 있다.

## 후속 작업

1. Conformance 저장소의 Google Gen AI Python/JavaScript billed-image SDK plan
2. Cloud 저장소의 Gemini price publication과 manual reconciliation plan
3. priority/weighted/lowest-cost routing과 fallback
4. 관리형 image object storage와 CDN
5. 사용량·원가·판매가 조회 API와 Dashboard
