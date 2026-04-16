# query-service Makefile
# Called directly or via parent aikey-data Makefile.

APP       := query-service
VERSION   ?= 0.1.0
PKG       := github.com/AiKeyLabs/pkg/buildinfo
GIT_REVISION  = $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")
GIT_DIRTY     = $(shell test -z "$$(git status --porcelain --untracked-files=normal 2>/dev/null)" && echo "" || echo "-dirty")
BUILD_ID     ?= $(shell head -c 2 /dev/urandom 2>/dev/null | xxd -p 2>/dev/null \
                  || powershell -NoProfile -C "'{0:x4}' -f (Get-Random -Max 65535)" 2>/dev/null \
                  || echo "0000")
BUILD_TIME    = $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                  || powershell -NoProfile -C "(Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')" 2>/dev/null \
                  || echo "unknown")
BUILD_DIR := bin
GO        := go

# Local dev: keep debug symbols for dlv/pprof
LDFLAGS_DEV  := -ldflags "\
  -X $(PKG).Version=$(VERSION) \
  -X $(PKG).Revision=$(GIT_REVISION)$(GIT_DIRTY) \
  -X $(PKG).BuildID=$(BUILD_ID) \
  -X $(PKG).BuildTime=$(BUILD_TIME)"

# Production: strip symbols, embed version+commit, static link (CGO_ENABLED=0)
LDFLAGS_PROD := -ldflags "-s -w \
  -X $(PKG).Version=$(VERSION) \
  -X $(PKG).Revision=$(GIT_REVISION)$(GIT_DIRTY) \
  -X $(PKG).BuildID=$(BUILD_ID) \
  -X $(PKG).BuildTime=$(BUILD_TIME)"

# Cross-compile targets
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build build-prod cross-compile run test lint tidy clean

## build: local dev build (debug symbols kept)
build:
	$(GO) build $(LDFLAGS_DEV) -o $(BUILD_DIR)/$(APP) ./cmd/...

## build-prod: production build (stripped, static, version+commit embedded)
build-prod:
	CGO_ENABLED=0 $(GO) build $(LDFLAGS_PROD) -o $(BUILD_DIR)/$(APP) ./cmd/...

## cross-compile: build for all target platforms
cross-compile:
	@mkdir -p $(BUILD_DIR)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		output="$(BUILD_DIR)/$(APP)-$${os}-$${arch}$${ext}"; \
		echo "  Building $$output ..."; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			$(GO) build $(LDFLAGS_PROD) -o $$output ./cmd/...; \
	done

## run: build and start (local dev)
run: build
	$(BUILD_DIR)/$(APP)

## test: run unit tests with race detector
test:
	$(GO) test -race -v ./internal/...

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## tidy: go mod tidy
tidy:
	$(GO) mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)
