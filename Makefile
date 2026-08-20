.PHONY: build check fmt fmt-check test vet

build:
	mkdir -p bin
	go build -o bin/gateway ./cmd/gateway

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

test:
	go test -race ./...

vet:
	go vet ./...

check: fmt-check vet test build
