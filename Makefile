SHELL := /bin/bash
GOENV := source scripts/goenv.sh &&
# The release version lives in VERSION at the repository root, not in a git tag: a tag is
# an index a release can forget to create, while this file travels with the commit that
# declares the release. A build with no VERSION file reports 0.0.0 rather than a commit
# prefix — reading "0.0.0" an operator knows nothing was released, whereas "56df9b5" looks
# like a version and compares with nothing.
VERSION ?= $(shell cat VERSION 2>/dev/null | tr -d '[:space:]')
REVISION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.revision=$(REVISION) -X main.date=$(DATE)

.PHONY: all build test vet fmt tidy run clean verify smoke plugin-example load ui-check ui-base version-check

all: build

# The version is a published fact, so a typo in it is a release-blocking error rather
# than something to paper over with a fallback: "1.2" and "v1.2.3" both fail here.
version-check:
	@v="$(VERSION)"; \
	if [ -z "$$v" ]; then \
		echo "version-check: VERSION file is missing or empty; write a.b.c into it" >&2; exit 1; \
	fi; \
	if ! printf '%s' "$$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "version-check: VERSION must be a.b.c (got '$$v')" >&2; exit 1; \
	fi

build: version-check
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

# The console derives its mount prefix from its own module URL, so one build serves both
# `/admin/ui/` and `/aigw/admin/ui/`. That derivation is a pure function, which is why it
# can be pinned by node instead of a browser (no node here means skip, like test-race).
ui-base:
	@if command -v node >/dev/null 2>&1; then \
		node scripts/ui-base-test.mjs ; \
		node scripts/ui-badge-test.mjs ; \
		node internal/webui/tests/requests_test.mjs || exit $$? ; \
		node internal/webui/tests/tags_binding_test.mjs || exit $$? ; \
	else \
		echo "skip: node is not available (the derivation is still covered by make ui-check)" ; \
	fi

# ui-base rides along because it needs no browser and checks the two console facts that are
# invisible to `go test`: where the console thinks it is mounted, and what its build badge says.
verify: vet test ui-base build

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
