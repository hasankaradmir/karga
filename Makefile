VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga/internal/cli.version=$(VERSION)

.PHONY: build test test-integration test-e2e lint fmt

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga ./cmd/karga

test:
	go test -race ./...

# Needs a reachable Docker daemon (DOCKER_HOST or the active docker context).
test-integration:
	go test -tags integration -race -count=1 ./...

# Builds the binary and drives it against Docker.
test-e2e:
	go test -tags e2e -count=1 ./tests/e2e/...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt
