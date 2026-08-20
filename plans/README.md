# Gateway Plan Log

이 디렉터리는 Gateway 저장소의 구현 계획과 실행 이력을 시간순으로 관리한다.

일반적인 `PLAN.md`처럼 하나의 문서를 계속 덮어쓰지 않는다. 데이터베이스 마이그레이션과 유사하게 새 계획, 변경, 중단, 대체를 각각 새로운 파일로 추가한다. 이를 통해 특정 시점에 무엇을 왜 구현했는지 추적할 수 있다.

기여 절차와 리뷰 기준은 [`../CONTRIBUTING.md`](../CONTRIBUTING.md)를 따르고, 새 계획은 [`TEMPLATE.md`](./TEMPLATE.md)에서 시작한다.

## 운영 원칙

1. 계획 파일은 생성 시각을 접두사로 사용한다.
2. 파일명과 `id`는 한 번 사용한 뒤 재사용하지 않는다.
3. 승인되어 구현이 시작된 계획의 범위와 완료 조건은 원칙적으로 수정하지 않는다.
4. 계획 변경은 기존 파일을 덮어쓰지 않고 새로운 계획 파일을 추가한다.
5. 새 계획은 필요하면 `depends_on` 또는 `supersedes`로 기존 계획을 참조한다.
6. 구현 완료는 코드, 테스트, 문서 등 검증 가능한 증거로 판단한다.
7. 하나의 계획은 독립적으로 검증하고 완료할 수 있는 크기로 작성한다.
8. 여러 저장소에 걸친 작업은 각 저장소에 로컬 계획을 만들고 동일한 `initiative` 값으로 연결한다.

## 파일명 규칙

```text
YYYYMMDD_HHMMSS_<kind>_<slug>.md
```

시간대는 프로젝트 기본 시간대인 `Asia/Seoul`을 사용한다. 동일 초 충돌이 발생하면 더 늦게 생성되는 파일의 시각을 증가시킨다.

종류:

| kind | 의미 |
|---|---|
| `plan` | 신규 기능 또는 구현 작업 |
| `change` | 승인된 계획의 범위나 접근 방식 변경 |
| `rollback` | 구현 또는 정책의 명시적 철회 |
| `close` | 별도 증거를 포함한 계획 종료 기록이 필요할 때 |

예시:

```text
20260820_113825_plan_phase0_gateway_bootstrap.md
20260822_091500_change_replace_router_contract.md
20260825_170000_rollback_disable_unsafe_url_fetch.md
```

## 계획 메타데이터

모든 계획 파일은 다음 front matter로 시작한다.

```yaml
---
id: gateway-20260820-001
title: Phase 0 Gateway Bootstrap
status: proposed
created_at: 2026-08-20T11:38:25+09:00
updated_at: 2026-08-20T11:38:25+09:00
owners:
  - gateway
initiative: phase-0-native-sdk-validation
depends_on: []
supersedes: []
affected_repos:
  - gateway
---
```

허용 상태:

```text
proposed → accepted → in_progress → completed
                              ├──→ blocked
                              └──→ superseded
```

- `proposed`: 검토 전 초안
- `accepted`: 구현 대상으로 승인됨
- `in_progress`: 구현 또는 검증 진행 중
- `completed`: 모든 완료 조건과 검증 증거가 충족됨
- `blocked`: 외부 조건 때문에 진행할 수 없음
- `superseded`: 후속 계획이 이 계획을 대체함

상태와 검증 증거는 실행 이력에 해당하므로 해당 파일에서 갱신할 수 있다. 범위, 설계, 완료 조건을 실질적으로 변경해야 할 때는 새로운 `change` 계획을 추가한다.

## 필수 본문 구조

각 계획은 최소한 다음 항목을 포함한다.

```markdown
## 목적
## 배경
## 범위
## 제외 범위
## 설계 및 구현 순서
## 인터페이스와 데이터 변경
## 보안 및 과금 고려사항
## 테스트 계획
## 완료 조건
## 검증 증거
## 후속 작업
```

## 계획 작성 단위

좋은 계획은 다음 특성을 가진다.

- 단일한 사용자 또는 시스템 결과를 만든다.
- 선행 조건과 영향을 받는 저장소가 명확하다.
- 구현 순서가 코드 변경 단위로 나뉜다.
- 실패, 재시도, 멱등성, 보안 조건을 포함한다.
- 자동화 가능한 완료 조건을 가진다.
- 다음 계획 없이도 완료 여부를 판단할 수 있다.

큰 Phase 전체를 하나의 실행 계획으로 만들지 않는다. Phase는 루트 마스터 플랜에서 관리하고, 이 디렉터리에서는 다음 수준으로 나눈다.

```text
Phase 0
├─ Gateway bootstrap
├─ Service API key authentication
├─ Gemini native pass-through
├─ OpenAI image pass-through
├─ Credential redaction
└─ SDK conformance handoff
```

## 멀티레포 연결 규칙

여러 저장소가 함께 변경되는 기능은 저장소마다 독립 계획을 가진다.

예를 들어 Gemini SDK 검증은 다음처럼 연결한다.

```text
initiative: phase-0-gemini-sdk-e2e

gateway/plans/...plan_gemini_native_passthrough.md
conformance/plans/...plan_gemini_python_conformance.md
```

Gateway 계획은 Conformance 저장소의 내부 작업을 직접 소유하지 않는다. 대신 필요한 공개 API 계약과 인수 조건을 명시한다.

## 현재 계획 목록

| ID | 상태 | 계획 | Initiative |
|---|---|---|---|
| `gateway-20260820-001` | completed | [Phase 0 Gateway Bootstrap](./20260820_113825_plan_phase0_gateway_bootstrap.md) | `phase-0-native-sdk-validation` |
| `gateway-20260820-002` | completed | [Phase 0 Service API Key Authentication](./20260820_124731_plan_phase0_service_api_key_authentication.md) | `phase-0-service-api-key-auth` |
| `gateway-20260820-003` | completed | [Phase 0 Provider Credential Security Boundary](./20260820_130704_plan_phase0_provider_credential_security_boundary.md) | `phase-0-provider-credential-boundary` |
| `gateway-20260820-004` | completed | [Phase 0 Gemini generateContent Native Pass-through](./20260820_132444_plan_phase0_gemini_generate_content_native_passthrough.md) | `phase-0-gemini-sdk-e2e` |
| `gateway-20260820-005` | completed | [Phase 0 OpenAI Images Native Pass-through](./20260820_135352_plan_phase0_openai_images_native_passthrough.md) | `phase-0-openai-images-sdk-e2e` |
| `gateway-20260820-006` | completed | [Phase 0 Image Edits Native Pass-through](./20260820_141124_plan_phase0_image_edits_native_passthrough.md) | `phase-0-image-edits-native-e2e` |
| `gateway-20260820-007` | completed | [Phase 1 Capability Registry and Models API](./20260820_142222_plan_phase1_capability_registry_models_api.md) | `phase-1-capability-registry` |
| `gateway-20260820-008` | completed | [Phase 1 Tenant Ownership Foundation](./20260820_143201_plan_phase1_tenant_ownership_foundation.md) | `phase-1-tenant-ownership` |
| `gateway-20260820-009` | completed | [Phase 1 Wallet and Append-only Ledger Foundation](./20260820_144259_plan_phase1_wallet_ledger_foundation.md) | `phase-1-wallet-ledger` |
| `gateway-20260820-010` | completed | [Phase 1 Provider Pricing and Request Cost Estimates](./20260820_145626_plan_phase1_provider_pricing_cost_estimates.md) | `phase-1-provider-pricing` |
| `gateway-20260820-011` | completed | [Phase 1 Billable Image Request Lifecycle](./20260820_151115_plan_phase1_billable_image_request_lifecycle.md) | `phase-1-billable-image-lifecycle` |
| `gateway-20260820-012` | completed | [Phase 1 Idempotency-Key and Native Response Replay](./20260820_153309_plan_phase1_idempotency_native_response_replay.md) | `phase-1-idempotency-response-replay` |
| `gateway-20260820-013` | completed | [Phase 1 Timeout and Provider Reconciliation Worker](./20260820_155724_plan_phase1_timeout_provider_reconciliation_worker.md) | `phase-1-timeout-reconciliation` |
| `gateway-20260820-014` | completed | [Phase 1 Gemini Image Billing and Idempotency](./20260820_161342_plan_phase1_gemini_image_billing_idempotency.md) | `phase-1-gemini-image-billing` |
| `gateway-20260820-015` | completed | [Allow Gemini Protocol in Image Charge Schema](./20260820_162516_change_allow_gemini_image_charge_protocol.md) | `phase-1-gemini-image-billing` |
| `gateway-20260820-016` | completed | [Phase 1 Provider Channel Candidates and Priority Routing](./20260820_163602_plan_phase1_provider_channel_priority_routing.md) | `phase-1-provider-routing` |
| `gateway-20260820-017` | completed | [Phase 1 Pre-dispatch Candidate Fallback](./20260820_165240_plan_phase1_predispatch_candidate_fallback.md) | `phase-1-provider-routing` |
| `gateway-20260820-018` | completed | [Phase 1 API Key Distributed Rate Limiting](./20260820_171128_plan_phase1_api_key_distributed_rate_limiting.md) | `phase-1-api-key-rate-limiting` |
| `gateway-20260820-019` | completed | [Phase 1 API Key Model Authorization](./20260820_173449_plan_phase1_api_key_model_authorization.md) | `phase-1-api-key-authorization` |
| `gateway-20260820-020` | completed | [Phase 1 API Key Network Restrictions](./20260820_175231_plan_phase1_api_key_network_restrictions.md) | `phase-1-api-key-authorization` |
| `gateway-20260820-021` | completed | [Phase 1 Hierarchical Cost Quotas](./20260820_181108_plan_phase1_hierarchical_cost_quotas.md) | `phase-1-cost-quotas` |
| `gateway-20260820-022` | completed | [Phase 1 Provider Channel Spend Caps](./20260820_183744_plan_phase1_provider_channel_spend_caps.md) | `phase-1-provider-spend-controls` |
| `gateway-20260820-023` | completed | [Phase 1 Encrypted Provider Credential Control Plane](./20260820_185824_plan_phase1_provider_credential_control_plane.md) | `phase-1-provider-credential-control-plane` |
| `gateway-20260820-024` | completed | [Phase 2 Lowest-Cost Provider Routing](./20260820_193408_plan_phase2_lowest_cost_routing.md) | `phase-2-cost-routing` |
| `gateway-20260820-025` | completed | [Phase 2 Weighted Provider Routing](./20260820_195929_plan_phase2_weighted_provider_routing.md) | `phase-2-weighted-routing` |
| `gateway-20260820-026` | completed | [Phase 2 Provider Health Score and Circuit Breaker](./20260820_201810_plan_phase2_provider_health_circuit_breaker.md) | `phase-2-provider-health` |
| `gateway-20260820-027` | completed | [Phase 2 Managed Image Storage and CDN Delivery](./20260820_205023_plan_phase2_managed_image_storage.md) | `phase-2-managed-image-storage` |
| `gateway-20260820-028` | completed | [Phase 2 OpenTelemetry Tracing and Metrics](./20260820_212326_plan_phase2_opentelemetry_observability.md) | `phase-2-opentelemetry-observability` |
| `gateway-20260820-029` | completed | [Phase 3 Durable Asynchronous Job Foundation](./20260820_214540_plan_phase3_async_job_foundation.md) | `phase-3-async-job-foundation` |
| `gateway-20260820-030` | completed | [Phase 3 Replicate Native Predictions](./20260820_221427_plan_phase3_replicate_native_predictions.md) | `phase-3-replicate-native-predictions` |
| `gateway-20260820-031` | completed | [Phase 3 fal Native Queue](./20260820_223933_plan_phase3_fal_native_queue.md) | `phase-3-fal-native-queue` |
| `gateway-20260820-032` | completed | [Phase 3 Replicate Signed Webhook Reconciliation](./20260820_231225_plan_phase3_replicate_signed_webhooks.md) | `phase-3-replicate-signed-webhooks` |
| `gateway-20260820-033` | completed | [Phase 3 fal Signed Webhook Reconciliation](./20260820_235333_plan_phase3_fal_signed_webhooks.md) | `phase-3-fal-signed-webhooks` |
| `gateway-20260821-034` | completed | [Phase 3 Async Usage and Partial-output Settlement](./20260821_001646_plan_phase3_async_usage_partial_settlement.md) | `phase-3-async-usage-partial-settlement` |
| `gateway-20260821-035` | completed | [Phase 3 Async Job Management Read API](./20260821_005344_plan_phase3_async_job_management_read_api.md) | `phase-3-async-job-management-read-api` |
| `gateway-20260821-036` | completed | [Phase 4 OpenAI Chat Completions Non-streaming Foundation](./20260821_011141_plan_phase4_openai_chat_completions_foundation.md) | `phase-4-openai-chat-completions-foundation` |
| `gateway-20260821-037` | accepted | [Phase 4 OpenAI Chat Token Usage Billing and Settlement](./20260821_013812_plan_phase4_openai_chat_token_settlement.md) | `phase-4-openai-chat-token-settlement` |
| `gateway-20260821-038` | accepted | [Phase 4 OpenAI Chat SSE Streaming and Disconnect Settlement](./20260821_042500_plan_phase4_openai_chat_streaming_settlement.md) | `phase-4-openai-chat-streaming-settlement` |
| `gateway-20260821-039` | accepted | [Phase 4 OpenAI Responses Native Non-streaming Foundation](./20260821_064000_plan_phase4_openai_responses_foundation.md) | `phase-4-openai-responses-foundation` |
| `gateway-20260821-040` | completed | [Phase 4 OpenAI Responses Token Usage Billing and Settlement](./20260821_080000_plan_phase4_openai_responses_token_settlement.md) | `phase-4-openai-responses-token-settlement` |
| `gateway-20260821-041` | completed | [Phase 4 OpenAI Responses SSE Streaming and Disconnect Settlement](./20260821_123000_plan_phase4_openai_responses_streaming_settlement.md) | `phase-4-openai-responses-streaming-settlement` |
| `gateway-20260821-042` | completed | [Phase 4 Gemini Native LLM generateContent Foundation](./20260821_143000_plan_phase4_gemini_llm_generate_content_foundation.md) | `phase-4-gemini-llm-generate-content-foundation` |
| `gateway-20260821-043` | completed | [Phase 4 Gemini Token Usage Billing and Settlement](./20260821_160000_plan_phase4_gemini_token_usage_settlement.md) | `phase-4-gemini-token-usage-settlement` |
| `gateway-20260821-044` | completed | [Phase 4 Gemini Native SSE Streaming and Disconnect Settlement](./20260821_190000_plan_phase4_gemini_streaming_settlement.md) | `phase-4-gemini-streaming-settlement` |
| `gateway-20260821-045` | completed | [Phase 4 Anthropic Messages Native Non-streaming Foundation](./20260821_220000_plan_phase4_anthropic_messages_foundation.md) | `phase-4-anthropic-messages-foundation` |
| `gateway-20260821-046` | completed | [Phase 4 Anthropic Messages Token Usage Billing and Settlement](./20260821_233000_plan_phase4_anthropic_token_usage_settlement.md) | `phase-4-anthropic-token-usage-settlement` |
| `gateway-20260822-047` | completed | [Phase 4 Anthropic Messages Native SSE and Disconnect Settlement](./20260822_013000_plan_phase4_anthropic_streaming_settlement.md) | `phase-4-anthropic-messages-streaming-settlement` |
| `gateway-20260822-048` | completed | [Phase 4 OpenAI-protocol LLM Routing and Pre-dispatch Fallback](./20260822_040000_plan_phase4_openai_protocol_llm_routing_fallback.md) | `phase-4-openai-protocol-llm-routing-fallback` |
| `gateway-20260822-049` | in_progress | [Phase 4 OpenAI Responses Protocol-compatible Provider Routing](./20260822_070000_plan_phase4_openai_responses_provider_routing.md) | `phase-4-openai-responses-provider-routing` |

새 계획을 추가할 때 이 표에도 항목을 추가한다. 파일의 상세 내용과 충돌할 경우 개별 계획 파일의 메타데이터를 기준으로 한다.
