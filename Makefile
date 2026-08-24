.PHONY: build check fmt fmt-check integration-test public-sdk-test test vet

build:
	mkdir -p bin
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/gateway-key ./cmd/gateway-key
	go build -o bin/gateway-quota ./cmd/gateway-quota
	go build -o bin/gateway-spend-cap ./cmd/gateway-spend-cap
	go build -o bin/gateway-provider-credential ./cmd/gateway-provider-credential
	go build -o bin/gateway-chat-price ./cmd/gateway-chat-price
	go build -o bin/gateway-video-price ./cmd/gateway-video-price
	go build -o bin/gateway-audio-price ./cmd/gateway-audio-price
	go build -o bin/gateway-plugin-validator ./cmd/gateway-plugin-validator
	go build -o bin/gateway-plugin-conformance ./cmd/gateway-plugin-conformance
	go build -o bin/gateway-plugin-registry ./cmd/gateway-plugin-registry
	go build -o bin/gateway-plugin-mock ./cmd/gateway-plugin-mock

fmt:
	gofmt -w $$(find cmd internal operations plugin-sdk protocols providers -name '*.go' -type f)

fmt-check:
	test -z "$$(gofmt -l cmd internal operations plugin-sdk protocols providers)"

test:
	go test -race ./...

public-sdk-test:
	cd examples/plugin/go-sidecar-template && GOWORK=off go test ./...
	cd examples/plugin/go-async-sidecar-template && GOWORK=off go test ./...
	cd examples/plugin/go-video-sidecar-template && GOWORK=off go test ./...
	test -z "$$(cd examples/plugin/go-sidecar-template && GOWORK=off go list -deps ./... | grep 'github.com/nativegatewayhq/gateway/internal/' || true)"
	test -z "$$(cd examples/plugin/go-sidecar-template && GOWORK=off go list -deps github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1 | grep 'github.com/nativegatewayhq/gateway/internal/' || true)"
	test -z "$$(cd examples/plugin/go-async-sidecar-template && GOWORK=off go list -deps ./... | grep 'github.com/nativegatewayhq/gateway/internal/' || true)"
	test -z "$$(cd examples/plugin/go-video-sidecar-template && GOWORK=off go list -deps ./... | grep 'github.com/nativegatewayhq/gateway/internal/' || true)"

integration-test:
	@if [ -z "$$TEST_DATABASE_URL" ]; then echo "TEST_DATABASE_URL is required"; exit 1; fi
	@if [ -z "$$TEST_REDIS_URL" ]; then echo "TEST_REDIS_URL is required"; exit 1; fi
	TEST_DATABASE_URL="$$TEST_DATABASE_URL" TEST_REDIS_URL="$$TEST_REDIS_URL" go test -race -count=1 -tags=integration ./internal/database ./internal/apikey ./internal/audioassets ./internal/speechstorage ./internal/audiobilling ./internal/audiopricing ./internal/audioreconciliation ./internal/billing ./internal/chatbilling ./internal/chatpricing ./internal/chatreconciliation ./internal/costquota ./internal/imagestorage ./internal/jobs ./internal/ledger ./internal/plugins ./internal/pricing ./internal/providercredentials ./internal/providerhealth ./internal/ratelimit ./internal/reconciliation ./internal/runwayassets ./internal/spendcap ./internal/videostorage ./protocols/anthropic ./protocols/fal ./protocols/gemini ./protocols/openai ./protocols/replicate ./protocols/runway ./providers/anthropic ./providers/fal ./providers/google ./providers/openai ./providers/plugin ./providers/replicate ./providers/runway ./providers/xai ./cmd/gateway-audio-price ./cmd/gateway-chat-price ./cmd/gateway-video-price ./cmd/gateway-key ./cmd/gateway-quota ./cmd/gateway-spend-cap ./cmd/gateway-provider-credential ./cmd/gateway-plugin-validator ./cmd/gateway-plugin-mock ./cmd/gateway

vet:
	go vet ./...

check: fmt-check vet test public-sdk-test build
