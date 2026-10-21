GO ?= go
BIN_DIR := bin
BIN := $(BIN_DIR)/herdr-agent-time
COVERAGE := coverage.out

.DEFAULT_GOAL := build

.PHONY: build test test-race cover fmt fmt-check vet lint check ci clean

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -o $(BIN) .

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=$(COVERAGE) ./...
	$(GO) tool cover -func=$(COVERAGE)

fmt:
	gofmt -w .

fmt-check:
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "gofmt required for:"; echo "$$files"; exit 1; fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

check: fmt-check vet test

ci: check test-race build lint

clean:
	rm -f $(BIN) $(COVERAGE)
