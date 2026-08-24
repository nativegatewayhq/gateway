---
id: gateway-20260824-063
title: Phase 6 Public Plugin Adapter SDK, Conformance Kit, and Template
status: completed
created_at: 2026-08-24T15:52:01+09:00
updated_at: 2026-08-24T16:25:01+09:00
owners:
  - gateway
initiative: phase-6-plugin-adapter-sdk-conformance
depends_on:
  - gateway-20260824-062
supersedes: []
affected_repos:
  - gateway
  - conformance
  - cloud
  - dashboard
---

# Phase 6 Public Plugin Adapter SDK, Conformance Kit, and Template

## 목적

Provider Plugin foundation의 private runtime 구현을 외부 Adapter 개발자가 안정적으로 소비할 수 있는 versioned public SDK로 승격하고, core 수정 없이 만든 HTTP sidecar가 보안·호환성·usage 계약을 만족하는지 black-box로 판정하는 Conformance Kit와 최소 Adapter template을 제공한다.

## 배경

Plan 062는 strict Provider Manifest v1, 고정 origin transport, OpenAI/Gemini image projection, immutable channel evidence와 mock sidecar를 end-to-end로 증명했다. 그러나 execute envelope와 validator가 `internal/plugins`에 있어 외부 Go module이 import할 수 없고, mock 성공 경로 외에 독립 Adapter를 판정하는 reusable runner·versioned fixture corpus·machine-readable report가 없다.

최초 로드맵 Phase 6의 완료 조건은 외부 개발자가 코어를 수정하지 않고 Adapter를 구현하며 CI에서 보안·호환성·비용 계산 검사를 자동 수행하는 것이다. 따라서 다음 단계는 marketplace나 새 modality보다 공개 계약과 검증 도구를 먼저 고정한다.

## 범위

- `plugin-sdk/runtime/v1` public canonical execute/health types와 strict codec
- schema version, bounded field, identity, result/error exclusivity, usage와 image media validation
- Gateway runtime과 mock server를 public SDK codec으로 이관해 단일 wire 구현 유지
- `plugin-sdk/conformance/v1` black-box HTTP sidecar runner
- manifest+endpoint+secret ref를 입력으로 하는 `gateway-plugin-conformance` CLI
- deterministic test request, success, declared output, identity, auth, header, timeout와 malformed behavior 검사
- valid/invalid Provider Manifest 및 sidecar response fixture corpus
- JSON machine-readable report schema, stable check ID와 exit code
- Go HTTP sidecar Adapter template와 build/test workflow
- manifest 기반 Markdown capability 문서 생성
- SDK compatibility range와 contract change policy
- contributor guide, Makefile, CI, examples와 멀티레포 handoff 갱신

## 제외 범위

- remote manifest/adapter 다운로드와 arbitrary command 실행
- Adapter process lifecycle 관리, container build/push 또는 deployment
- official Registry 등록, 서명, transparency log와 marketplace UI
- gRPC, mTLS, WASM, shared library와 in-process plugin
- asynchronous Job/webhook, streaming, LLM/video/audio와 image edit
- Provider credential 발급, 가격 publication 자동 승인과 Wallet 접근
- conformance runner가 실제 유료 upstream Provider 호출을 요구하는 방식
- Dashboard/Cloud/Conformance 저장소 내부 구현

## 핵심 결정

### 1. Public SDK는 wire 계약만 소유한다

- `plugin-sdk/runtime/v1`는 transport-neutral value, strict JSON codec와 validator만 제공한다.
- Gateway auth, DB, routing, billing, storage와 telemetry 타입을 import하거나 노출하지 않는다.
- Adapter가 Gateway 내부 package를 import할 필요가 없어야 한다.
- v1 field 삭제·의미 변경은 금지하고 새 capability는 backward-compatible optional field 또는 새 schema version으로 추가한다.

### 2. 한 개의 strict codec을 Gateway와 Adapter가 함께 사용한다

- duplicate/unknown field, trailing JSON, oversized body와 ambiguous success/error를 fail closed한다.
- request identity와 response echo 검증은 Gateway transport와 conformance runner가 같은 public validator를 사용한다.
- Base64 image는 허용 MIME과 magic bytes를 일치시키고 URL은 syntax만 SDK에서 검사한다. exact origin·DNS·SSRF 검증은 Gateway operator/runtime 경계가 계속 소유한다.
- usage는 요청 최대치 이하의 정수 image count이고 result 개수와 일치해야 한다.

### 3. Conformance는 black-box이며 실제 Provider 비용을 발생시키지 않는다

- runner는 operator가 이미 실행한 sidecar의 exact loopback HTTP 또는 configured HTTPS endpoint만 호출한다.
- deterministic prompt와 declared model로 health 및 execute 계약을 검사한다.
- secret은 env/file ref로만 읽고 CLI argument, report, stdout/stderr와 failure detail에 기록하지 않는다.
- success corpus와 함께 invalid auth, wrong method/path, oversized body, duplicate identity와 canceled request에 대한 bounded behavior를 검사한다.
- sidecar가 반환한 image bytes는 report에 저장하지 않고 digest·size·MIME만 bounded evidence로 남긴다.

### 4. Report는 CI와 Registry의 장래 입력 계약이다

- report는 schema version, manifest identity/digest, SDK version, check ID, outcome, bounded category와 duration만 포함한다.
- endpoint, secret ref/value, prompt, raw request/response, image/URL과 process environment는 포함하지 않는다.
- check ID와 exit code는 release 내에서 안정적으로 유지해 외부 CI가 fail closed할 수 있게 한다.
- future official Adapter Registry는 이 report를 신뢰하지 않고 별도 signed build provenance와 함께 재실행한다.

### 5. Template은 복제 가능한 최소 reference implementation이다

- 별도 Go module로 `plugin-sdk/runtime/v1` public package만 import한다.
- fixed health/execute path, dedicated bearer validation, bounded server timeouts와 graceful shutdown을 포함한다.
- prompt/image를 log하지 않고 success 및 stable error envelope를 반환한다.
- generated artifact를 commit하지 않고 local build/test와 conformance 실행 명령을 제공한다.

## 설계 및 구현 순서

### 1. Public runtime SDK v1

- execute request/response, image result, usage와 plugin error types를 `plugin-sdk/runtime/v1`로 이동한다.
- strict bounded `DecodeRequest`, `DecodeResponse`, `ValidateRequest`, `ValidateResponse`와 canonical encode를 제공한다.
- manifest duplicate-key scanner를 common public JSON boundary로 재사용하되 package dependency cycle을 만들지 않는다.
- internal client, provider executor와 mock server를 public codec으로 이관한다.

### 2. Fixture corpus와 compatibility policy

- valid manifest/request/response와 unknown/duplicate/oversize/identity/usage/MIME invalid fixtures를 추가한다.
- corpus index는 expected outcome과 stable category를 선언한다.
- public packages의 Go API compile test와 wire golden digest를 제공한다.
- compatibility와 deprecation policy를 문서화한다.

### 3. Black-box conformance runner

- trusted manifest와 runtime endpoint/secret mapping을 bind한다.
- health, auth isolation, execute success, declared protocol/model/output, error envelope와 cancellation check를 순차 실행한다.
- proxy와 redirect를 차단하고 request/response/time/concurrency를 제한한다.
- expected failure probe는 sidecar state를 변경하거나 upstream 유료 call을 요구하지 않도록 contract-level invalid request만 사용한다.

### 4. CLI와 report

- `cmd/gateway-plugin-conformance`가 manifest file/directory, plugin ID, endpoint ref mapping과 secret env/file mapping을 받는다.
- human summary와 `--json` report를 지원하고 secret-safe generic error만 stderr에 쓴다.
- pass/fail/configuration을 stable exit code로 구분한다.
- report decoder test가 unknown/secret-like field와 raw content 부재를 확인한다.

### 5. Adapter template와 documentation

- isolated template module, manifest, server, tests와 CI example을 추가한다.
- manifest에서 capability table과 configuration reference Markdown을 deterministic 생성한다.
- root README, CONTRIBUTING, Makefile와 Compose example을 public SDK/conformance 흐름으로 갱신한다.
- `conformance`, `cloud`, `dashboard`가 복사해야 할 versioned artifact와 금지 데이터를 handoff에 명시한다.

## Public 인터페이스

### Runtime SDK 예시

```go
request, err := runtimev1.DecodeRequest(reader, maximumBytes)
if err != nil {
    runtimev1.WriteError(writer, requestIdentity, runtimev1.InvalidRequest("invalid request"))
    return
}

response := runtimev1.Success(request.Identity(), runtimev1.Result{
    Images: []runtimev1.Image{{MIMEType: "image/png", Base64: encoded}},
    Usage: runtimev1.Usage{Images: 1},
})
err = runtimev1.EncodeResponse(writer, response)
```

### Conformance report 개요

```json
{
  "schema_version": "nativegateway.plugin-conformance/v1",
  "plugin_id": "provider.example",
  "plugin_version": "1.0.0",
  "manifest_digest": "...",
  "sdk_version": "runtime/v1",
  "outcome": "pass",
  "checks": [
    {"id": "health.authenticated", "outcome": "pass", "duration_ms": 3}
  ]
}
```

## 보안 및 과금 고려사항

- public codec은 어떤 Authorization, service key, Provider credential, cookie, forwarding header 또는 tenant identity field도 정의하지 않는다.
- runner bearer secret은 mutable byte buffer로 취급하고 report/log/error에 format하지 않는다.
- endpoint는 customer input이 아니라 CI/operator trust input이며 proxy·redirect·userinfo·query·fragment를 거부한다.
- conformance image는 최대 byte limit과 MIME magic을 검증한 뒤 즉시 폐기하고 report에는 digest·size도 check 내부 판정에만 사용한다.
- manifest digest와 response identity가 다르면 결과 내용을 사용하지 않고 실패한다.
- usage conformance는 count/maximum 일치만 검증하며 plugin-supplied price/currency를 허용하지 않는다.
- 실제 managed price, reserve/capture/release와 minimum margin은 Gateway integration suite가 계속 검증한다.

## 테스트 계획

### 단위 테스트

- strict public request/response codec, duplicate/unknown/trailing/oversize JSON
- canonical identity, error/result exclusivity, usage bound와 MIME magic
- public API compile test와 wire golden digest
- report schema, stable check ID, sorting과 secret/raw-content absence
- CLI option/ref validation과 exit code

### 통합 테스트

- existing Gateway client와 mock이 public SDK codec을 통해 동일 wire 유지
- template sidecar health/execute/error/cancel behavior
- conformance pass와 각 fixture failure category
- redirect/proxy/timeout/reset/oversize 및 concurrency bound
- manifest endpoint/secret ref mismatch와 compatibility failure

### 호환성 테스트

- Plan 062 OpenAI/Gemini official SDK plugin tests가 wire 변경 없이 통과
- external temporary Go module이 public SDK와 template를 import/build/test
- JSON report를 별도 consumer가 strict decode
- existing manifest digest와 plugin channel snapshot이 변경되지 않음

### 필수 검증 명령

```text
GOCACHE=/private/tmp/gateway-go-cache make check
GOCACHE=/private/tmp/gateway-go-cache GOFLAGS=-p=1 TEST_DATABASE_URL='postgres://...' TEST_REDIS_URL='redis://...' make integration-test
GOCACHE=/private/tmp/gateway-go-cache go test -tags=sdkconformance ./protocols/openai ./protocols/gemini -count=1
GOWORK=off go test ./plugin-sdk/...
go run ./cmd/gateway-plugin-conformance ...
```

## 완료 조건

- [x] 외부 module이 `plugin-sdk/runtime/v1`만으로 sidecar wire를 구현함
- [x] Gateway, mock과 template이 동일 public strict codec을 사용함
- [x] valid/invalid fixture corpus와 stable category가 제공됨
- [x] black-box conformance runner가 health/auth/execute/identity/output/usage/failure 경계를 검증함
- [x] machine-readable report가 secret·endpoint·prompt·raw image 없이 deterministic 생성됨
- [x] CLI가 env/file secret ref, bounded options와 stable exit code를 제공함
- [x] standalone Go Adapter template가 build/test/conformance를 통과함
- [x] 기존 manifest digest, immutable channel evidence와 native SDK 호환성이 유지됨
- [x] 전체 unit/race/integration/SDK/public-module 검사가 통과함
- [x] README, CONTRIBUTING, Makefile, examples와 멀티레포 handoff가 갱신됨

## 검증 증거

- 구현 Pull Request: [#95](https://github.com/nativegatewayhq/gateway/pull/95), 구현 commit `b2818bb`.
- `GOCACHE=/private/tmp/nativegateway-go-cache make check` 통과: fmt, vet, 전체 race unit test, standalone public module dependency 검사와 모든 binary build.
- fresh PostgreSQL `gateway_plan063` 및 Redis DB 15에서 `GOFLAGS=-p=1 make integration-test` 통과: plugin channel snapshot, billing/idempotency와 전체 Gateway integration suite 유지.
- `go test -tags=sdkconformance ./protocols/openai ./protocols/gemini -count=1` 통과: 기존 공식 OpenAI/Gemini SDK plugin wire 호환 유지.
- 실행 중인 `gateway-plugin-mock`과 standalone `go-sidecar-template` 각각에 `gateway-plugin-conformance` 실행: 10개 health/auth/wire/success/error/cancel check 모두 pass.
- `GOWORK=off go test ./...` 및 dependency audit 통과: template이 `plugin-sdk/runtime/v1`만 사용하고 Gateway `internal/` package를 import하지 않음.
- versioned fixture corpus, strict report decode, secret/raw-content 부재, env/file ref와 capability Markdown의 unit/race test 통과.

## Rollback 계획

- Gateway runtime은 public SDK 이관 전과 동일한 v1 wire를 유지하므로 새 conformance CLI/template 배포만 중단할 수 있다.
- public package에 오류가 있으면 새 Adapter release를 차단하고 Gateway의 기존 manifest/plugin runtime을 `GATEWAY_PLUGIN_MODE=disabled`로 비활성화한다.
- 이미 저장된 manifest digest와 `plugin_channel_snapshots`는 수정·삭제하지 않는다.
- report와 fixture schema는 versioned additive artifact이므로 rollback 시에도 기존 v1 consumer를 깨뜨리지 않는다.

## 후속 작업

- signed official Adapter Registry, build provenance와 compatibility matrix
- async Job/webhook plugin SDK and conformance
- streaming LLM/audio plugin SDK and conformance
- gRPC/mTLS sidecar transport
- controlled runtime reload and health rollout
