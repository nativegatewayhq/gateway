.PHONY: build check fmt fmt-check integration-test test vet

build:
	mkdir -p bin
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/gateway-key ./cmd/gateway-key

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

test:
	go test -race ./...

integration-test:
	@if [ -z "$$TEST_DATABASE_URL" ]; then echo "TEST_DATABASE_URL is required"; exit 1; fi
	TEST_DATABASE_URL="$$TEST_DATABASE_URL" go test -race -count=1 -tags=integration ./internal/database ./internal/apikey ./cmd/gateway-key ./cmd/gateway

vet:
	go vet ./...

check: fmt-check vet test build
