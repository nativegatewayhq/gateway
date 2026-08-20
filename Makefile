.PHONY: build check fmt fmt-check integration-test test vet

build:
	mkdir -p bin
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/gateway-key ./cmd/gateway-key
	go build -o bin/gateway-quota ./cmd/gateway-quota
	go build -o bin/gateway-spend-cap ./cmd/gateway-spend-cap
	go build -o bin/gateway-provider-credential ./cmd/gateway-provider-credential

fmt:
	gofmt -w $$(find cmd internal operations protocols providers -name '*.go' -type f)

fmt-check:
	test -z "$$(gofmt -l cmd internal operations protocols providers)"

test:
	go test -race ./...

integration-test:
	@if [ -z "$$TEST_DATABASE_URL" ]; then echo "TEST_DATABASE_URL is required"; exit 1; fi
	@if [ -z "$$TEST_REDIS_URL" ]; then echo "TEST_REDIS_URL is required"; exit 1; fi
	TEST_DATABASE_URL="$$TEST_DATABASE_URL" TEST_REDIS_URL="$$TEST_REDIS_URL" go test -race -count=1 -tags=integration ./internal/database ./internal/apikey ./internal/billing ./internal/costquota ./internal/ledger ./internal/pricing ./internal/providercredentials ./internal/ratelimit ./internal/reconciliation ./internal/spendcap ./protocols/gemini ./protocols/openai ./providers/google ./providers/openai ./providers/xai ./cmd/gateway-key ./cmd/gateway-quota ./cmd/gateway-spend-cap ./cmd/gateway-provider-credential ./cmd/gateway

vet:
	go vet ./...

check: fmt-check vet test build
