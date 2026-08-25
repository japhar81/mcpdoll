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

## test-all: everything, including the suites that need Docker
test-all: test test-conformance test-web

## test-web: console typecheck and unit tests
test-web: web-deps
	cd web && npm run typecheck && npm test --silent

# ------------------------------------------------------------ web deps ----

# Every target below that runs `npm` depends on this.
#
# Without it the failure is `sh: vite: command not found`, which points at
# nothing: the last person to hit it went looking for a misconfigured port in
# vite.config.ts, because the error named a missing binary rather than a
# missing install. The build system assumed a step it never performed and never
# checked.
#
# The stamp file, not the directory, is the target: npm rewrites paths inside
# node_modules without changing the directory's own mtime, so a directory
# target would go stale silently and never reinstall.
WEB_DEPS := web/node_modules/.mcpdoll-installed

## web-deps: install the console's dependencies if the lockfile is newer
web-deps: $(WEB_DEPS)

$(WEB_DEPS): web/package.json web/package-lock.json
	cd web && npm ci
	@mkdir -p $(dir $@) && touch $@

# ------------------------------------------------------------- generate ----

## generate: regenerate code from api/openapi.yaml and the .proto files
generate: generate-go generate-ts

generate-go:
	./proto/generate.sh

# The console's route manifest is generated from its router, so it cannot
# describe a route that does not exist. See tools/paritycheck.
generate-ts: web-deps
	$(GO) run ./tools/gents
	cd web && npm run routes

## verify-generated: fail if generated code is stale (CI gate)
verify-generated: generate
	@git diff --exit-code || { \
	  echo ""; \
	  echo "Generated code is stale. Run 'make generate' and commit the result."; \
	  exit 1; \
	}

# --------------------------------------------------------------- parity ----

## console: build the console into web/dist
console: web-deps
	cd web && npm run build

## parity: enforce the tri-surface law — every operation has a CLI cmd + UI route
#
# Depends on generate-ts so the manifest is never checked stale: a route added
# without regenerating would otherwise pass here and 404 in the browser.
parity: $(BIN)/mcpdoll generate-ts
	$(GO) run ./tools/paritycheck \
	  -openapi api/openapi.yaml \
	  -cli-bin $(BIN)/mcpdoll \
	  -routes web/src/routes.gen.ts

# ----------------------------------------------------------------- lint ----

## lint: vet + formatting check + console typecheck
lint: fmt-check web-deps
	$(GO) vet ./...
	cd web && npm run typecheck

## fmt: rewrite Go sources with gofmt
fmt:
	gofmt -w $(shell find . -name '*.go' -not -path './internal/proto/*')

fmt-check:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './internal/proto/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# ------------------------------------------------------------------ dev ----

## dev: run the stack as host processes — fastest edit-to-running loop for Go
dev:
	./deploy/dev-up.sh

## dev-down: stop the host stack, keeping the database
dev-down:
	./deploy/dev-down.sh

# --------------------------------------------------------------- docker ----
#
# `make dev` and `make up` are the same stack two ways, and they bind the same
# ports — run one or the other. Host processes rebuild faster; containers are
# what a deployment actually looks like, and are the only way to find out that
# a config file hardcoded localhost.

COMPOSE := docker compose -f deploy/docker-compose.yml

## up: build the images and bring the whole stack up in Docker, waiting for health
up:
	@./deploy/docker-up.sh

## down: stop the Docker stack, keeping its volumes (the key and the database survive)
down:
	$(COMPOSE) down --remove-orphans

## down-hard: stop and wipe everything — signing key, tenants, users, and keys
down-hard:
	$(COMPOSE) down --remove-orphans --volumes

## ps: what the Docker stack is doing
ps:
	@$(COMPOSE) ps --format 'table {{.Name}}\t{{.Status}}\t{{.Ports}}'

## logs: follow the Docker stack's logs (SERVICE=dataplane to narrow)
logs:
	$(COMPOSE) logs -f --tail=100 $(SERVICE)

## restart: rebuild and recreate one service (SERVICE=dataplane)
restart:
	$(COMPOSE) up -d --build --force-recreate $(SERVICE)

## obs: open the local Grafana
obs:
	@echo "Grafana → http://localhost:3300  (folder: MCPDoll)"
	@command -v open >/dev/null 2>&1 && open http://localhost:3300 || true

.PHONY: help build clean test test-race test-cover test-conformance test-all \
        test-web test-e2e generate generate-go generate-ts verify-generated \
        parity lint fmt fmt-check dev dev-down obs web-deps \
        up down down-hard ps logs restart
