BINARY  := atteler
PKG     := ./cmd/atteler
MODULE  := github.com/tommoulard/atteler
GORELEASER_VERSION ?= v2.15.4
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

.PHONY: all build run test lint generate release-check release-snapshot clean

all: generate lint test build

## build: compile the binary
build:
	go build -o $(BINARY) $(PKG)

## run: build and run
run:
	go run $(PKG)

## test: run all tests
test:
	go test -race -count=1 ./...

## lint: run golangci-lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 run ./...

## generate: run go generate
generate:
	go generate ./...

## release-check: validate the GoReleaser configuration
release-check:
	$(GORELEASER) check

## release-snapshot: build local release artifacts without publishing
release-snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish

## clean: remove build artifacts
clean:
	rm -rf $(BINARY) dist
