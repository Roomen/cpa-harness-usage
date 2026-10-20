OS := $(shell go env GOOS)
ARCH := $(shell go env GOARCH)
export GOCACHE ?= $(CURDIR)/.cache/go-build
export GOMODCACHE ?= $(CURDIR)/.cache/go-mod
export npm_config_cache ?= $(CURDIR)/.cache/npm
EXT_darwin := dylib
EXT_linux := so
EXT_windows := dll
EXE_windows := .exe

.PHONY: test build deps collector collector-test smoke

deps: node_modules/.harness-usage-lock

node_modules/.harness-usage-lock: package.json package-lock.json
	npm ci --ignore-scripts --no-audit --no-fund
	@touch $@

collector: deps
	GOOS=$(OS) GOARCH=$(ARCH) node scripts/prepare-collector.mjs

collector-test: collector
	go test -tags 'bundledcollector collectorintegration' ./internal/usage

test:
	go vet ./...
	go test -race ./...

build: collector
	@mkdir -p bin
	@if [ "$(OS)/$(ARCH)" = "darwin/amd64" ]; then \
		if [ "$(EXPERIMENTAL_DARWIN_AMD64)" != "1" ]; then echo "darwin/amd64 is single-plugin experimental only; see README.md"; exit 1; fi; \
		toolchain=$$(bash scripts/darwin-amd64-go.sh); \
		GOROOT="$$toolchain" GOTOOLCHAIN=local CGO_ENABLED=1 "$$toolchain/bin/go" build -tags bundledcollector -trimpath -buildmode=c-shared -o bin/harness-usage.$(EXT_$(OS)) ./cmd/plugin; \
	else \
		CGO_ENABLED=1 go build -tags bundledcollector -trimpath -buildmode=c-shared -o bin/harness-usage.$(EXT_$(OS)) ./cmd/plugin; \
	fi
	go build -tags bundledcollector -trimpath -o bin/harness-usage$(EXE_$(OS)) ./cmd/harness-usage

smoke: build
	go build -o bin/fakeccusage$(EXE_$(OS)) ./internal/usage/testdata/fakeccusage
	go run ./cmd/plugin-smoke -library bin/harness-usage.$(EXT_$(OS)) -binary bin/fakeccusage$(EXE_$(OS))
	go run ./cmd/plugin-smoke -library bin/harness-usage.$(EXT_$(OS)) -binary bundled
