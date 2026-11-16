VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga/internal/cli.version=$(VERSION)

.PHONY: build test test-integration lint fmt

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga ./cmd/karga

test:
	go test -race ./...

# Needs a reachable Docker daemon (DOCKER_HOST or the active docker context).
test-integration:
	go test -tags integration -race -count=1 ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt
