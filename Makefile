# PacaPulse development commands.
#
# `make verify` is the gate: it runs exactly what CI runs for a pull request,
# minus the container build.

GO ?= go
GOLANGCI_LINT ?= golangci-lint
DOCKER ?= docker

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
PACAPULSE_LOG_LEVEL ?= info
PACAPULSE_LOG_FORMAT ?= text

.DEFAULT_GOAL := help
.PHONY: help fmt fmt-check vet lint test test-race cover build run image smoke verify clean

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

cover: ## Run the tests and report coverage per package
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

build: ## Build the service binary into bin/
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(OUTPUT) ./cmd/$(BINARY)

run: ## Run the service locally
	PACAPULSE_BASE_URL='$(PACAPULSE_BASE_URL)' \
	PACAPULSE_ADDR='$(PACAPULSE_ADDR)' \
	PACAPULSE_LOG_LEVEL='$(PACAPULSE_LOG_LEVEL)' \
	PACAPULSE_LOG_FORMAT='$(PACAPULSE_LOG_FORMAT)' \
	$(GO) run ./cmd/$(BINARY)

image: ## Build the production container image
	$(DOCKER) build --build-arg VERSION=$(VERSION) --tag $(IMAGE) .

smoke: ## Build the image and smoke-test it as a container
	IMAGE=$(IMAGE) scripts/smoke-test.sh

verify: fmt-check vet lint test-race ## Run every check CI runs for a pull request

clean: ## Remove build output
	rm -rf bin coverage.out
