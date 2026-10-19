OS := $(shell go env GOOS)
ARCH := $(shell go env GOARCH)
EXT_darwin := dylib
EXT_linux := so
EXT_windows := dll

.PHONY: test build deps

deps:
	npm ci --ignore-scripts --no-audit --no-fund

test:
	go vet ./...
	go test -race ./...

build:
	@if [ "$(OS)/$(ARCH)" = "darwin/amd64" ]; then echo "darwin/amd64 requires a patched Go TLS toolchain; see AGENTS.md"; exit 1; fi
	@mkdir -p bin
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o bin/harness-usage.$(EXT_$(OS)) ./cmd/plugin
	go build -trimpath -o bin/harness-usage ./cmd/harness-usage
