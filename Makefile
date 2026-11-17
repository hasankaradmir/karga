VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga/internal/version.Version=$(VERSION)

.PHONY: build test test-integration test-e2e test-linux lint fmt

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga ./cmd/karga
	go build -ldflags "$(LDFLAGS)" -o bin/karga-server ./cmd/karga-server

test:
	go test -race ./...
	cd sdk && go test -race ./...

# Needs a reachable Docker daemon (DOCKER_HOST or the active docker context).
test-integration:
	go test -tags integration -race -count=1 ./...

# Builds the binary and drives it against Docker.
test-e2e:
	go test -tags e2e -count=1 ./tests/e2e/...

lint:
	golangci-lint run
	cd sdk && golangci-lint run --config ../.golangci.yml

fmt:
	golangci-lint fmt

# Runs TARGETS in a Linux VM (OrbStack or Lima): the gateway and release proxy are Linux-only.
TARGETS ?= test-integration test-e2e
test-linux:
	scripts/linux-vm.sh $(TARGETS)
