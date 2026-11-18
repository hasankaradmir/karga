VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga-cli/internal/version.Version=$(VERSION)

.PHONY: build dist test lint fmt

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga ./cmd/karga

# dist builds the release archives without a release: the names and contents goreleaser gives them,
# and checksums.txt, so scripts/install.sh can install them from file://.
DIST_TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
dist:
	rm -rf dist && mkdir -p dist
	set -e; for t in $(DIST_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; name=karga_$(VERSION:v%=%)_$${os}_$${arch}; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o dist/$$name/karga ./cmd/karga; \
		COPYFILE_DISABLE=1 tar --no-xattrs -czf dist/$$name.tar.gz -C dist/$$name karga -C $(CURDIR) README.md USAGE.md CHANGELOG.md LICENSE; rm -rf dist/$$name; \
	done
	cd dist && if command -v sha256sum >/dev/null; then sha256sum *.tar.gz; else shasum -a 256 *.tar.gz; fi > checksums.txt
	@echo "install: KARGA_VERSION=$(VERSION) KARGA_DOWNLOAD_URL=file://$(CURDIR)/dist KARGA_SKIP_SIGNATURE=1 sh scripts/install.sh"

test:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt
