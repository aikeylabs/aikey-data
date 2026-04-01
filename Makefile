# ==============================================================================
# aikey-data — top-level Makefile
#
# Orchestrates collector-service and query-service.
# Delegates build/test/lint to sub-service Makefiles.
#
# Local dev:    make build          (debug symbols, current platform)
# Production:   make build-prod     (stripped, static, version+commit)
# Release:      make release        (cross-compile + bundle + checksums)
#
# Release bundles follow the naming convention from:
#   阶段2-MVP-版本发布与一键安装方案.md §8–§9
#
# Bundle name:  aikey-data_${VERSION}_${OS}_${ARCH}.tar.gz
# Bundle structure:
#   bundle/VERSION
#   bundle/bin/collector-service[.exe]
#   bundle/bin/query-service[.exe]
#   bundle/config/default/collector-service.env
#   bundle/config/default/query-service.env
#   bundle/migrations/*.sql
# ==============================================================================

VERSION     ?= 0.1.0
SERVICES    := collector-service query-service
DIST_DIR    := dist
RELEASE_DIR := $(DIST_DIR)/release
BUNDLE_TMP  := $(DIST_DIR)/bundle

# Cross-compile target platforms
PLATFORMS   := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

# Portable SHA-256: prefer sha256sum (Linux/coreutils), fall back to shasum (macOS)
SHASUM := $(shell command -v sha256sum 2>/dev/null || echo "shasum -a 256")

.PHONY: build build-prod cross-compile release release-current \
        test lint tidy clean help _bundle \
        dev-up dev-down dev-restart dev-restart-all dev-logs dev-ps

# ==============================================================================
# Local development
# ==============================================================================

## build: local dev build of all services (debug symbols kept)
build:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc build VERSION=$(VERSION); done

## build-prod: production build (stripped, static, version+commit)
build-prod:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc build-prod VERSION=$(VERSION); done

## test: run unit tests for all services
test:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc test; done

## lint: run golangci-lint for all services
lint:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc lint; done

## tidy: go mod tidy for all services
tidy:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc tidy; done

## clean: remove all build artifacts
clean:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc clean; done
	rm -rf $(DIST_DIR)

# ==============================================================================
# Docker Compose (local dev environment)
# ==============================================================================

## dev-up: start PostgreSQL + both services (docker compose)
dev-up:
	docker compose up --build -d
	@echo ""
	@echo "Services started:"
	@echo "  collector-service: http://localhost:27300/health"
	@echo "  query-service:     http://localhost:27301/health"

## dev-down: stop all containers
dev-down:
	docker compose down

## dev-restart: rebuild and restart both services (postgres keeps running)
dev-restart:
	docker compose up --build -d --no-deps collector-service query-service
	@echo "Restarted:"
	@echo "  collector-service: http://localhost:27300/health"
	@echo "  query-service:     http://localhost:27301/health"
	docker compose logs -f collector-service query-service

## dev-restart-all: full teardown + rebuild (including postgres)
dev-restart-all:
	docker compose down
	docker compose up --build -d
	@echo "Restarted all."
	docker compose logs -f collector-service query-service

## dev-logs: follow logs of both services
dev-logs:
	docker compose logs -f collector-service query-service

## dev-ps: show running containers
dev-ps:
	docker compose ps

# ==============================================================================
# Cross-compilation
# ==============================================================================

## cross-compile: build all services for all target platforms
cross-compile:
	@for svc in $(SERVICES); do $(MAKE) -C $$svc cross-compile VERSION=$(VERSION); done

# ==============================================================================
# Release packaging
# ==============================================================================

## release: cross-compile + package per-platform bundles + checksums
release: cross-compile
	@echo "==> Packaging release bundles v$(VERSION)..."
	@mkdir -p $(RELEASE_DIR)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		$(MAKE) _bundle OS=$$os ARCH=$$arch; \
	done
	@echo "==> Generating checksums..."
	@cd $(RELEASE_DIR) && $(SHASUM) *.tar.gz > checksums.txt
	@echo ""
	@echo "Release artifacts ($(RELEASE_DIR)/):"
	@ls -lh $(RELEASE_DIR)/
	@echo ""
	@echo "Checksums:"
	@cat $(RELEASE_DIR)/checksums.txt

## release-current: build + package bundle for current platform only
release-current:
	@$(MAKE) build-prod VERSION=$(VERSION)
	$(eval _OS   := $(shell go env GOOS))
	$(eval _ARCH := $(shell go env GOARCH))
	$(eval _EXT  := $(if $(filter windows,$(_OS)),.exe,))
	$(eval _NAME := aikey-data_$(VERSION)_$(_OS)_$(_ARCH))
	@echo "==> Packaging $(_NAME)..."
	@mkdir -p $(RELEASE_DIR)
	@rm -rf $(BUNDLE_TMP)/$(_NAME)
	@mkdir -p $(BUNDLE_TMP)/$(_NAME)/bin $(BUNDLE_TMP)/$(_NAME)/config/default $(BUNDLE_TMP)/$(_NAME)/migrations
	@echo "$(VERSION)" > $(BUNDLE_TMP)/$(_NAME)/VERSION
	@cp collector-service/bin/collector-service$(_EXT) $(BUNDLE_TMP)/$(_NAME)/bin/
	@cp query-service/bin/query-service$(_EXT)         $(BUNDLE_TMP)/$(_NAME)/bin/
	@cp collector-service/migrations/*.sql              $(BUNDLE_TMP)/$(_NAME)/migrations/
	@cp collector-service/.env.example                  $(BUNDLE_TMP)/$(_NAME)/config/default/collector-service.env
	@cp query-service/.env.example                      $(BUNDLE_TMP)/$(_NAME)/config/default/query-service.env
	@cd $(BUNDLE_TMP) && tar czf ../../$(RELEASE_DIR)/$(_NAME).tar.gz $(_NAME)/
	@rm -rf $(BUNDLE_TMP)/$(_NAME)
	@cd $(RELEASE_DIR) && $(SHASUM) $(_NAME).tar.gz > checksums.txt
	@echo "==> Done: $(RELEASE_DIR)/$(_NAME).tar.gz"
	@cat $(RELEASE_DIR)/checksums.txt

# Internal: package a single platform bundle (called by release target)
.PHONY: _bundle
_bundle:
	$(eval _EXT  := $(if $(filter windows,$(OS)),.exe,))
	$(eval _NAME := aikey-data_$(VERSION)_$(OS)_$(ARCH))
	@rm -rf $(BUNDLE_TMP)/$(_NAME)
	@mkdir -p $(BUNDLE_TMP)/$(_NAME)/bin $(BUNDLE_TMP)/$(_NAME)/config/default $(BUNDLE_TMP)/$(_NAME)/migrations
	@echo "$(VERSION)" > $(BUNDLE_TMP)/$(_NAME)/VERSION
	@cp collector-service/bin/collector-service-$(OS)-$(ARCH)$(_EXT)  $(BUNDLE_TMP)/$(_NAME)/bin/collector-service$(_EXT)
	@cp query-service/bin/query-service-$(OS)-$(ARCH)$(_EXT)          $(BUNDLE_TMP)/$(_NAME)/bin/query-service$(_EXT)
	@cp collector-service/migrations/*.sql                             $(BUNDLE_TMP)/$(_NAME)/migrations/
	@cp collector-service/.env.example                                 $(BUNDLE_TMP)/$(_NAME)/config/default/collector-service.env
	@cp query-service/.env.example                                     $(BUNDLE_TMP)/$(_NAME)/config/default/query-service.env
	@cd $(BUNDLE_TMP) && tar czf ../../$(RELEASE_DIR)/$(_NAME).tar.gz $(_NAME)/
	@rm -rf $(BUNDLE_TMP)/$(_NAME)
	@echo "    $(_NAME).tar.gz"

# ==============================================================================
# Help
# ==============================================================================

## help: list all available targets
help:
	@echo ""
	@echo "aikey-data Makefile (v$(VERSION))"
	@echo ""
	@echo "Local development (native):"
	@echo "  make build              Build all services (debug symbols, current platform)"
	@echo "  make build-prod         Production build (stripped, static, version+commit)"
	@echo "  make test               Run all unit tests with race detector"
	@echo "  make lint               Run golangci-lint on all services"
	@echo "  make tidy               go mod tidy for all services"
	@echo "  make clean              Remove all build artifacts"
	@echo ""
	@echo "Local development (Docker Compose):"
	@echo "  make dev-up             Start PostgreSQL + both services"
	@echo "  make dev-down           Stop all containers"
	@echo "  make dev-restart        Rebuild + restart services (postgres keeps running)"
	@echo "  make dev-restart-all    Full teardown + rebuild all containers"
	@echo "  make dev-logs           Follow service logs"
	@echo "  make dev-ps             Show running containers"
	@echo ""
	@echo "Release:"
	@echo "  make cross-compile              Cross-compile for all platforms"
	@echo "  make release                    Cross-compile + bundle + checksums"
	@echo "  make release-current            Bundle for current platform only"
	@echo ""
	@echo "  Bundles: $(RELEASE_DIR)/aikey-data_VERSION_OS_ARCH.tar.gz"
	@echo "  Checksums: $(RELEASE_DIR)/checksums.txt"
	@echo ""
	@echo "Variables:"
	@echo "  VERSION=$(VERSION)              Release version (default: 0.1.0)"
	@echo ""
	@echo "Platforms: $(PLATFORMS)"
	@echo ""
	@echo "Examples:"
	@echo "  make dev-up                             # docker: start everything"
	@echo "  make dev-restart                        # docker: rebuild services only"
	@echo "  make build                              # native: local dev build"
	@echo "  make test                               # native: run all tests"
	@echo "  make release VERSION=0.2.0              # full release for all platforms"
	@echo "  make release-current VERSION=0.2.0      # release for current platform only"
	@echo ""
