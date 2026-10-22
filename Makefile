OS := $(shell go env GOOS)
ARCH := $(shell go env GOARCH)
# Minimum macOS the darwin libraries run on. It only stamps LC_BUILD_VERSION:
# the Go linker picks its own floor (12.0 on Go 1.26, 13.0 on Go 1.27+), so
# darwin builds are pinned to the go.mod toolchain to keep the real floor here.
MACOS_MIN ?= 12.0
# Empty when go.mod declares no toolchain; then the host toolchain is used.
GO_TOOLCHAIN ?= $(shell awk '/^toolchain /{print $$2}' go.mod)
DARWIN_ENV := $(if $(filter darwin,$(OS)),$(if $(GO_TOOLCHAIN),GOTOOLCHAIN=$(GO_TOOLCHAIN) ,),)
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
	@rm -f bin/harness-usage.$(EXT_$(OS))
	@set -e; \
	if [ "$(OS)/$(ARCH)" = "darwin/amd64" ]; then \
		toolchain=$$(bash scripts/darwin-amd64-go.sh "" "$(GO_TOOLCHAIN)") && \
		GOROOT="$$toolchain" GOTOOLCHAIN=local CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=$(MACOS_MIN) CGO_CFLAGS="-O2 -g -mmacosx-version-min=$(MACOS_MIN)" CGO_LDFLAGS="-mmacosx-version-min=$(MACOS_MIN)" CC="clang -arch x86_64" "$$toolchain/bin/go" build -tags bundledcollector -trimpath -buildmode=c-shared -o bin/harness-usage.$(EXT_$(OS)) ./cmd/plugin && \
		bash scripts/check-darwin.sh bin/harness-usage.$(EXT_$(OS)) amd64 $(MACOS_MIN); \
	elif [ "$(OS)" = "darwin" ]; then \
		$(DARWIN_ENV)CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=$(MACOS_MIN) CGO_CFLAGS="-O2 -g -mmacosx-version-min=$(MACOS_MIN)" CGO_LDFLAGS="-mmacosx-version-min=$(MACOS_MIN)" go build -tags bundledcollector -trimpath -buildmode=c-shared -o bin/harness-usage.$(EXT_$(OS)) ./cmd/plugin && \
		bash scripts/check-darwin.sh bin/harness-usage.$(EXT_$(OS)) $(ARCH) $(MACOS_MIN); \
	else \
		CGO_ENABLED=1 go build -tags bundledcollector -trimpath -buildmode=c-shared -o bin/harness-usage.$(EXT_$(OS)) ./cmd/plugin; \
	fi
	$(DARWIN_ENV)go build -tags bundledcollector -trimpath -o bin/harness-usage$(EXE_$(OS)) ./cmd/harness-usage

smoke: build
	go build -o bin/fakeccusage$(EXE_$(OS)) ./internal/usage/testdata/fakeccusage
	go run ./cmd/plugin-smoke -library bin/harness-usage.$(EXT_$(OS)) -binary bin/fakeccusage$(EXE_$(OS))
	go run ./cmd/plugin-smoke -library bin/harness-usage.$(EXT_$(OS)) -binary bundled
