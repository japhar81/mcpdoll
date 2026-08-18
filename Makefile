# MCPDoll — enterprise MCP gateway.
#
# `make help` lists every target. The three you need day to day:
#   make dev    — bring the whole local stack up
#   make test   — offline suites (unit + integration); must be green on every commit
#   make parity — enforce the tri-surface law (every API op has a CLI cmd + UI route)

SHELL := /bin/bash
GO ?= go
GOBIN ?= $(shell $(GO) env GOPATH)/bin
BIN := bin

# Packages under test, excluding generated code.
GOPKGS := $(shell $(GO) list ./... 2>/dev/null | grep -v '/internal/proto')

.DEFAULT_GOAL := help

## help: list available targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

# ---------------------------------------------------------------- build ----

## build: compile all three binaries into ./bin
build: $(BIN)/mcpdoll-dp $(BIN)/mcpdoll-cp $(BIN)/mcpdoll

$(BIN)/%: FORCE
	@mkdir -p $(BIN)
	$(GO) build -o $@ ./cmd/$*

FORCE:

## clean: remove build output
clean:
	rm -rf $(BIN) coverage.txt coverage.html

# ----------------------------------------------------------------- test ----

## test: offline Go suites (unit + integration). Green on every commit.
test:
	$(GO) test ./... -count=1

## test-race: the same suites under the race detector
test-race:
	$(GO) test ./... -count=1 -race

## test-cover: run tests and report per-package coverage
test-cover:
	$(GO) test ./... -count=1 -coverprofile=coverage.txt -covermode=atomic
	$(GO) tool cover -func=coverage.txt | tail -1

## test-conformance: drive the edge with a real MCP client from the Go SDK
test-conformance:
	$(GO) test ./internal/dataplane/edge/... -count=1 -run Conformance -v

## test-all: everything, including the suites that need Docker and a browser
test-all: test test-conformance test-web test-e2e

## test-web: console unit tests
test-web:
	cd web && npm test --silent

## test-e2e: Playwright against the running stack
test-e2e:
	cd web && npx playwright test

# ------------------------------------------------------------- generate ----

## generate: regenerate code from api/openapi.yaml and the .proto files
generate: generate-go generate-ts

generate-go:
	$(GO) generate ./...

generate-ts:
	cd web && npm run generate

## verify-generated: fail if generated code is stale (CI gate)
verify-generated: generate
	@git diff --exit-code || { \
	  echo ""; \
	  echo "Generated code is stale. Run 'make generate' and commit the result."; \
	  exit 1; \
	}

# --------------------------------------------------------------- parity ----

## parity: enforce the tri-surface law — every operation has a CLI cmd + UI route
parity: $(BIN)/mcpdoll
	$(GO) run ./tools/paritycheck \
	  -openapi api/openapi.yaml \
	  -cli-bin $(BIN)/mcpdoll \
	  -routes web/src/routes.gen.ts

# ----------------------------------------------------------------- lint ----

## lint: vet + formatting check
lint: fmt-check
	$(GO) vet ./...

## fmt: rewrite Go sources with gofmt
fmt:
	gofmt -w $(shell find . -name '*.go' -not -path './internal/proto/*')

fmt-check:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './internal/proto/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# ------------------------------------------------------------------ dev ----

## dev: bring up Postgres, Redis, LGTM, control plane, data plane, console, fixtures
dev:
	./deploy/dev-up.sh

## dev-down: tear the local stack down and delete its volumes
dev-down:
	./deploy/dev-down.sh

## obs: open the local Grafana
obs:
	@echo "Grafana → http://localhost:3300  (folder: MCPDoll)"
	@command -v open >/dev/null 2>&1 && open http://localhost:3300 || true

.PHONY: help build clean test test-race test-cover test-conformance test-all \
        test-web test-e2e generate generate-go generate-ts verify-generated \
        parity lint fmt fmt-check dev dev-down obs
