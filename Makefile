BINARY  := atteler
PKG     := ./cmd/atteler
SYMPHONY_BINARY := symphony
SYMPHONY_PKG := ./cmd/symphony
MODULE  := github.com/tommoulard/atteler
GORELEASER_VERSION ?= v2.15.4
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS ?= -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build build-symphony run run-symphony test e2e e2e-live lint generate release-check release-snapshot clean

all: generate lint test build

## build: compile the binary
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

## build-symphony: compile the standalone Symphony service binary
build-symphony:
	go build -o $(SYMPHONY_BINARY) $(SYMPHONY_PKG)

## run: build and run
run:
	go run $(PKG)

## run-symphony: run the standalone Symphony service command
run-symphony:
	go run $(SYMPHONY_PKG)

TESTFLAGS ?=
TESTPACKAGE ?= ./...

## test: run Go tests; override TESTPACKAGE/TESTFLAGS for focused runs
test:
	go test -race -count=1 $(value TESTFLAGS) $(TESTPACKAGE)

## e2e: run black-box CLI end-to-end tests without live-provider calls
e2e:
	ATTELER_E2E_LIVE= go test -count=1 $(value TESTFLAGS) ./test/e2e

## e2e-live: run opt-in live LLM CLI tests (requires ATTELER_E2E_LIVE=1 and provider API keys)
e2e-live:
	ATTELER_E2E_LIVE=1 go test -count=1 -run TestLive -timeout=10m $(value TESTFLAGS) ./test/e2e

## lint: run golangci-lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 run ./...

## generate: run go generate
generate:
	go generate -x ./...

## release-check: validate the GoReleaser configuration
release-check:
	$(GORELEASER) check

## release-snapshot: build local release artifacts without publishing
release-snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish

## clean: remove build artifacts
clean:
	rm -rf $(BINARY) $(SYMPHONY_BINARY) dist
