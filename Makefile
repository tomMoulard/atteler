BINARY  := atteler
PKG     := ./cmd/atteler
MODULE  := github.com/tommoulard/atteler
GORELEASER_VERSION ?= v2.15.4
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS ?= -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build run test e2e e2e-live lint generate release-check release-snapshot clean

all: generate lint test build

## build: compile the binary
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

## run: build and run
run:
	go run $(PKG)

## test: run all tests
test:
	go test -race -count=1 ./...

## e2e: run black-box CLI end-to-end tests
e2e:
	go test -count=1 ./test/e2e

## e2e-live: run opt-in live LLM CLI tests (requires ATTELER_E2E_LIVE=1 and provider API keys)
e2e-live:
	ATTELER_E2E_LIVE=1 go test -count=1 -run TestLive -timeout=10m ./test/e2e

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
	rm -rf $(BINARY) dist
