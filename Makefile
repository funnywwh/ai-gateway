SHELL := /bin/bash
GOENV := source scripts/goenv.sh &&
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build test vet fmt tidy run clean verify smoke plugin-example load ui-check

all: build

build:
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/aigw ./cmd/aigw

test:
	@$(GOENV) go test ./...

# The race detector requires cgo and a C toolchain. This sandbox has neither
# (CGO_ENABLED=0, no gcc), so test-race explains itself instead of failing hard.
test-race:
	@if command -v gcc >/dev/null 2>&1 || command -v cc >/dev/null 2>&1; then \
		$(GOENV) CGO_ENABLED=1 go test -race ./... ; \
	else \
		echo "skip: -race needs cgo and a C compiler (not available here); run 'make test' instead" ; \
	fi

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
	@$(GOENV) go build -trimpath -o bin/aigw-provider-codex ./examples/provider-codex

smoke:
	@bash scripts/smoke.sh

# The console has no JS test runner here (no node/npm), so it is exercised in a real
# headless browser against captured API fixtures. Skips itself when firefox or python3
# is missing, exactly like test-race does without a C compiler.
ui-check:
	@bash scripts/ui-harness/run.sh

clean:
	@rm -rf bin
