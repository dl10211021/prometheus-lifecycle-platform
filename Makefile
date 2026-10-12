.PHONY: build unit vet test-local test-closure test-remote test-all
.NOTPARALLEL: test-all

GO ?= go
GOFLAGS ?= -mod=vendor
GO_TEST_TIMEOUT ?= 20m
BINARY ?= promlifecycle-server
export GOFLAGS

build:
	$(GO) build -trimpath -o $(BINARY) ./cmd/server

unit:
	$(GO) test -timeout $(GO_TEST_TIMEOUT) ./...

vet:
	$(GO) vet ./...

test-local: test-closure

test-closure: build
	PROMLIFECYCLE_ALLOW_PRIVATE_FETCH=1 SERVER_BIN=./$(BINARY) RUN_PRODUCT_CLOSURE=1 bash test-scripts/local-smoke.sh

test-remote:
	bash test-scripts/remote-tests.sh

test-all: unit vet test-local test-remote
