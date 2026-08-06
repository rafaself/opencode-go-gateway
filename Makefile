GO ?= go
BIN ?= bin/opencode-gateway
OUTPUT_DIR ?= dist
PREFIX ?= $(HOME)/.local
INSTALL_DIR ?= $(PREFIX)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf 'dev')
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf 'unknown')
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS ?= -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: fmt vet test contract integration race fuzz-smoke live-scenarios-test build install check release-check package package-self-test

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 ./...

contract:
	$(GO) test -count=1 ./internal/capture ./internal/codex ./internal/server

integration:
	$(GO) test -count=1 ./internal/server

race:
	$(GO) test -race ./...

FUZZTIME ?= 1s

fuzz-smoke:
	FUZZTIME=$(FUZZTIME) ./scripts/fuzz-smoke.sh

live-scenarios-test:
	./scripts/live-smoke-test.sh
	./scripts/live-scenarios-test.sh

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/opencode-gateway

install:
	GO="$(GO)" PREFIX="$(PREFIX)" INSTALL_DIR="$(INSTALL_DIR)" ./scripts/install.sh

check: fmt vet test race build
	git diff --check

release-check: fmt vet test contract integration race build fuzz-smoke
	git diff --check

package:
	GO="$(GO)" VERSION="$(VERSION)" COMMIT="$(COMMIT)" OUTPUT_DIR="$(OUTPUT_DIR)" ./scripts/package-release.sh

package-self-test:
	GO="$(GO)" VERSION="$(VERSION)" COMMIT="$(COMMIT)" OUTPUT_DIR="$(OUTPUT_DIR)" ./scripts/package-release.sh --self-test
