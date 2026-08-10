# PacaPulse development commands.
#
# `make verify` is the gate: it runs exactly what CI runs for a pull request,
# minus the container build.

GO ?= go
GOLANGCI_LINT ?= golangci-lint
DOCKER ?= docker
# Node validates the browser beacon: it runs the asset's test suite and
# measures its Brotli size, which Go's standard library cannot. It is a
# development tool only and never enters the binary or the image.
NODE ?= node
NODE_MAJOR := 24
BEACON_TESTS := 'internal/beacon/jstest/*.test.mjs'

BINARY := pacapulse
PACKAGE := github.com/mtrenker/pacapulse
OUTPUT := bin/$(BINARY)

# VERSION is the build identity stamped into the binary and the image; CI passes
# the commit SHA that the published image is tagged with.
VERSION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)
IMAGE ?= pacapulse:dev

LDFLAGS := -s -w -X $(PACKAGE)/internal/version.version=$(VERSION)

# Local run defaults. Override on the command line, e.g.
# `make run PACAPULSE_LOG_LEVEL=debug`.
PACAPULSE_BASE_URL ?= http://localhost:8080
PACAPULSE_ADDR ?= :8080
PACAPULSE_DATA_DIR ?= ./data
PACAPULSE_LOG_LEVEL ?= info
PACAPULSE_LOG_FORMAT ?= text

.DEFAULT_GOAL := help
.PHONY: help fmt fmt-check vet lint test test-race test-beacon cover build run image smoke verify clean

help: ## List the available commands
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

fmt: ## Format the Go sources in place
	gofmt -w -s .

fmt-check: ## Fail if any Go source is not gofmt-clean
	@unformatted=$$(gofmt -l -s .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not formatted; run 'make fmt':"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet: ## Run the standard Go static checks
	$(GO) vet ./...

lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

test: ## Run the tests
	$(GO) test ./...

test-race: ## Run the tests under the race detector
	$(GO) test -race ./...

test-beacon: ## Run the browser beacon's Node tests and size gate
	@version=$$($(NODE) --version 2>/dev/null); \
	case "$$version" in \
		v$(NODE_MAJOR).*) ;; \
		"") echo "Node $(NODE_MAJOR) is required for the beacon tests; see docs/development.md"; exit 1 ;; \
		*) echo "Node $(NODE_MAJOR) is required for the beacon tests, found $$version; see docs/development.md"; exit 1 ;; \
	esac
	$(NODE) --test $(BEACON_TESTS)

cover: ## Run the tests and report coverage per package
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

build: ## Build the service binary into bin/
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(OUTPUT) ./cmd/$(BINARY)

run: ## Run the service locally
	PACAPULSE_BASE_URL='$(PACAPULSE_BASE_URL)' \
	PACAPULSE_ADDR='$(PACAPULSE_ADDR)' \
	PACAPULSE_DATA_DIR='$(PACAPULSE_DATA_DIR)' \
	PACAPULSE_LOG_LEVEL='$(PACAPULSE_LOG_LEVEL)' \
	PACAPULSE_LOG_FORMAT='$(PACAPULSE_LOG_FORMAT)' \
	$(GO) run ./cmd/$(BINARY)

image: ## Build the production container image
	$(DOCKER) build --build-arg VERSION=$(VERSION) --tag $(IMAGE) .

smoke: ## Build the image and smoke-test it as a container
	IMAGE=$(IMAGE) scripts/smoke-test.sh

verify: fmt-check vet lint test-race test-beacon ## Run every check CI runs for a pull request

clean: ## Remove build output
	rm -rf bin coverage.out
