SHELL := /bin/bash
GOENV := source scripts/goenv.sh &&
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build test vet fmt tidy run clean verify smoke plugin-example

all: build

build:
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/aigw ./cmd/aigw

test:
	@$(GOENV) go test ./...

vet:
	@$(GOENV) go vet ./...

fmt:
	@$(GOENV) gofmt -l -w cmd internal pkg examples 2>/dev/null || true

tidy:
	@$(GOENV) go mod tidy

verify: vet test build

run: build
	@./bin/aigw --config config.yaml

plugin-example:
	@mkdir -p bin
	@$(GOENV) go build -trimpath -o bin/aigw-provider-replay ./examples/provider-replay

smoke:
	@bash scripts/smoke.sh

clean:
	@rm -rf bin
