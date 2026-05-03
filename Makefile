SHELL := /bin/bash
BIN   := bin/autoscaler
PKG   := ./...

# Prefer a manually-installed Go SDK over Debian's go-1.24 + auto-toolchain.
# Debian's system Go currently lags behind go.mod's `go 1.25` directive, and
# Go's auto-downloaded toolchains ship without `covdata`, which breaks
# `go test -coverprofile=...`. A real SDK from go.dev/dl provides a full
# toolset. Override at invocation time with `make GO=/path/to/go test`.
ifneq ($(wildcard $(HOME)/sdk/go/bin/go),)
GO     := $(HOME)/sdk/go/bin/go
GOROOT := $(HOME)/sdk/go
export GOROOT
export PATH := $(HOME)/sdk/go/bin:$(PATH)
else
GO     := go
endif

# Force the configured SDK; never auto-download a stripped toolchain.
export GOTOOLCHAIN := local

# Default target prints the available recipes instead of running one — that way
# `make` with no argument cannot trigger an unintended action.
.DEFAULT_GOAL := help

.PHONY: help all build test cover lint tidy run-local image clean

help:
	@echo "Targets:"
	@echo "  build       Compile the autoscaler binary into $(BIN)"
	@echo "  test        Run unit tests with -race"
	@echo "  cover       Run tests with coverage report (writes coverage.html)"
	@echo "  lint        Run golangci-lint (falls back to go vet)"
	@echo "  tidy        go mod tidy"
	@echo "  run-local   Source .env and run the binary against \$$KUBECONFIG"
	@echo "  image       docker build -t kamatera-autoscaler:dev ."
	@echo "  clean       Remove bin/ and coverage artifacts"
	@echo "  all         lint + test + build"

all: lint test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/autoscaler

test:
	$(GO) test -race -coverprofile=coverage.out $(PKG)

cover: test
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html"

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found — falling back to go vet"; \
		$(GO) vet $(PKG); \
	fi

tidy:
	$(GO) mod tidy

# Loads variables from a local .env (gitignored) and runs the binary against your kubeconfig.
# .env must define at least KAMATERA_API_CLIENT_ID, KAMATERA_API_SECRET,
# AUTOSCALER_JOIN_TOKEN, SSH_PUB_KEY. Optionally AUTOSCALER_CONFIG and KUBECONFIG.
run-local:
	@if [ ! -f .env ]; then echo "no .env found — copy .env.example to .env first"; exit 1; fi
	set -a; . ./.env; set +a; $(GO) run ./cmd/autoscaler

image:
	docker build -t kamatera-autoscaler:dev .

clean:
	rm -rf bin coverage.out coverage.html
