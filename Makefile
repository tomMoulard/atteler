BINARY  := atteler
PKG     := ./cmd/atteler
MODULE  := github.com/tommoulard/atteler

.PHONY: all build run test lint generate clean

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

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
