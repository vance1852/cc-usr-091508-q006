GO ?= go
BIN := bin/samplechain

.PHONY: all build run test vet fmt clean

all: build

build:
	CGO_ENABLED=1 $(GO) build -o $(BIN) ./cmd/server

run: build
	SAMPLECHAIN_DB ?= samplechain.db
	SAMPLECHAIN_ADDR ?= :8080
	$(BIN)

test:
	CGO_ENABLED=1 $(GO) test ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
