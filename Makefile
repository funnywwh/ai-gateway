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

# The console is embedded minified and pre-compressed in a release build. `ui-dist` writes a
# compressed mirror of internal/webui/static plus gzip sidecars plus an overlay file, and
# `go build -overlay` then embeds all of it — so the shipped binary carries stripped,
# renamed code and can answer a client that asks for gzip, while the working tree keeps the
# readable source that the tests and `make ui-check` read. Everything generated lives under
# .cache/, which is ignored by git. See docs/design/m50-frontend-minify.md and
# docs/design/m55-console-transfer-compression.md.
UIDIST ?= $(CURDIR)/.cache/ui-dist
UI_OVERLAY ?= $(UIDIST)/overlay.json

.PHONY: all build build-src ui-dist test vet fmt tidy run clean verify smoke plugin-example load ui-check ui-base version-check dshgw-build dshgw-test dshgw-verify dshgw-sandbox-test

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

# ui-dist compresses the console assets and writes the overlay that makes the Go compiler
# embed the compressed copy instead of the source.
ui-dist:
	@mkdir -p $(UIDIST)
	@$(GOENV) go build -trimpath -o $(UIDIST)/minifyui ./cmd/minifyui
	@$(UIDIST)/minifyui -src internal/webui/static -out $(UIDIST)/static -overlay $(UI_OVERLAY)

# build is the release shape: the console goes in minified and pre-compressed. The -X flags
# and the -overlay are deliberately on the same line — they are two halves of one fact, and a
# build that passed one without the other would report a shape it does not carry. See
# docs/design/m54-console-asset-shape.md and docs/design/m55-console-transfer-compression.md.
build: version-check ui-dist
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS) -X main.uiAssets=minified -X main.uiEncoding=gzip" -overlay $(UI_OVERLAY) -o bin/aigw ./cmd/aigw

# build-src is the same binary with the assets as written: for debugging the console in a
# form a human can read, or for checking that `ui-dist` changed nothing but the bytes. It
# writes bin/aigw-src, NOT bin/aigw: the debug build must not be able to replace the
# artifact that scripts/local-run.sh serves, because "I only wanted to look at the
# console" followed by a restart is exactly how readable source reached :8088 before
# (M50 §6). It says which shape it is out loud, because that must never be a guess.
build-src: version-check
	@echo "ui: source assets (not minified); wrote bin/aigw-src — bin/aigw is untouched"
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS) -X main.uiAssets=source -X main.uiEncoding=identity" -o bin/aigw-src ./cmd/aigw

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

# dshgw is an independently deployable binary. These targets are deliberately
# not prerequisites of the existing aigw verify/release path.
DSHGW_NODE ?= /home/winger/.local/node-v22.23.1-linux-x64/bin/node
DSHGW_DSH_ROOT ?= /home/winger/.local/dsh-0.1.2-rc.1

dshgw-build: version-check
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dshgw ./cmd/dshgw

dshgw-test:
	@$(GOENV) DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" go test ./internal/dshgw/... ./cmd/dshgw ./internal/arch
	@test -x "$(DSHGW_NODE)" || { echo "dshgw-test: Node missing: $(DSHGW_NODE)" >&2; exit 1; }
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" cmd/dshgw/plugin/picker-clamp.test.mjs

# The bwrap isolation mode's real acceptance: the tenant profile runs under the
# host's own bubblewrap, and a real dsh web worker starts inside it and answers
# the gateway's unauthenticated /api probe with 401. Both tests skip themselves
# where an unprivileged user namespace is unavailable, so this target is safe to
# call from any checkout, but it is only meaningful on the deployment host.
dshgw-sandbox-test:
	@$(GOENV) DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" go test ./internal/dshgw/sandbox -run '^TestStaging' -count=1 -v

dshgw-verify: dshgw-test dshgw-build
	@$(MAKE) --no-print-directory dshgw-sandbox-test
	@$(GOENV) go vet ./internal/dshgw/... ./cmd/dshgw ./internal/arch
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" bash scripts/verify-dshgw.sh

# The console derives its mount prefix from its own module URL, so one build serves both
# `/admin/ui/` and `/aigw/admin/ui/`. That derivation is a pure function, which is why it
# can be pinned by node instead of a browser (no node here means skip, like test-race).
ui-base:
	@if command -v node >/dev/null 2>&1; then \
		node scripts/ui-base-test.mjs ; \
		node scripts/ui-badge-test.mjs ; \
		node internal/webui/tests/requests_test.mjs || exit $$? ; \
		node internal/webui/tests/tags_binding_test.mjs || exit $$? ; \
		node internal/webui/tests/org_tree_test.mjs || exit $$? ; \
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
	@rm -rf bin $(UIDIST)
