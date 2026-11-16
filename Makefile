VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga/internal/cli.version=$(VERSION)

.PHONY: build test lint fmt

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga ./cmd/karga

test:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt
