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

.PHONY: all build build-src ui-dist test vet fmt tidy run clean verify smoke plugin-example load ui-check ui-base version-check dshgw-build gwproxy-build dshgw-test dshgw-node-test dshgw-node-e2e dshgw-node-deploy-e2e dshgw-verify dshgw-sandbox-test dshgw-supervised-test dshgw-ssh-integration dshgw-ssh-e2e dshgw-browser-e2e dshgw-browser-reload-e2e

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

# The optional single-domain front proxy (bin/gwproxy): one listener, path prefixes.
gwproxy-build: version-check
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gwproxy ./cmd/gwproxy

dshgw-build: version-check
	@mkdir -p bin
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dshgw ./cmd/dshgw

# The multi-machine unit surface (M77). It is a subset of dshgw-test, kept as its own target
# because it is the loop used while working on node behaviour: the protocol, the node record
# store, the control plane's client, the node agent's listener, and the configuration and
# registry fields that carry a tenant's placement.
dshgw-node-test:
	@$(GOENV) go test ./internal/dshgw/nodeproto ./internal/dshgw/nodestore ./internal/dshgw/nodeclient \
		./internal/dshgw/nodeserve ./internal/dshgw/nodeops ./internal/dshgw/nodeplane ./internal/dshgw/nodeup \
		./internal/dshgw/nodedep ./internal/dshgw/nodeaudit ./internal/dshgw/audit ./internal/dshgw/nodeitest \
		./internal/dshgw/tenancy \
		./internal/dshgw/proxy ./internal/dshgw/config \
		./internal/dshgw/registry ./cmd/dshgw

# The multi-machine acceptance (M77): one control plane, one worker node and a real tenant, run as
# two processes on one machine (two addresses, two state roots) with a stub aigw. It needs the
# gateway host — bubblewrap, the dsh runtime, Node — and inside a tenant sandbox it skips itself
# with the reason, exactly like dshgw-sandbox-test. `--passthrough-bwrap` swaps the sandbox for a
# stand-in so the protocol and process path can still be exercised where namespaces are forbidden.
dshgw-node-e2e: dshgw-build
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" PYTHONDONTWRITEBYTECODE=1 \
		python3 scripts/dshgw_node_e2e.py

# The SSH one-click deploy acceptance (M77): register a node, deploy it over a real ssh connection
# to loopback (fingerprint gate, payload upload, unit, start, verify), create a tenant on it and
# serve it through the control plane, upgrade it, rotate its token and purge it. The target is this
# machine reached over ssh — one host, two roles — and everything the deploy writes lives under
# .cache/node-deploy-e2e (a path both this shell and the ssh session see).
dshgw-node-deploy-e2e: dshgw-build
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" PYTHONDONTWRITEBYTECODE=1 \
		python3 scripts/dshgw_node_deploy_e2e.py $(DSHGW_NODE_DEPLOY_E2E_ARGS)

dshgw-test:
	@$(GOENV) DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" go test ./internal/dshgw/... ./cmd/dshgw ./internal/arch
	@test -x "$(DSHGW_NODE)" || { echo "dshgw-test: Node missing: $(DSHGW_NODE)" >&2; exit 1; }
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" cmd/dshgw/plugin/picker-clamp.test.mjs
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" cmd/dshgw/plugin/ssh-workspace/ssh-workspace.test.mjs
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" cmd/dshgw/plugin/ssh-workspace/client.test.mjs
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" cmd/dshgw/plugin/account-card/client.test.mjs
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" internal/dshgw/tenancy/settings_schema.test.mjs
# The three tenant-side plugins every account gets (M75). web-tty needs the anchor the runner gives
# a real worker, because that is where its node-pty comes from — without it the host test would be
# testing a plugin that cannot resolve its PTY, which is not the plugin a tenant runs.
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" DSHGW_DSH_ANCHOR="$(DSHGW_DSH_ROOT)/package.json" \
		"$(DSHGW_NODE)" --test cmd/dshgw/plugin/web-tty/test/*.test.mjs
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" --test cmd/dshgw/plugin/workspace-files/test/*.test.mjs
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" --test cmd/dshgw/plugin/git-diff/test/*.test.mjs
	@PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_dshgw_migration_plan.py
	@PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_decommission_legacy_plan.py
	@PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_ssh_config_adopt.py
	@PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_dshgw_ssh_identity.py

# The ssh-workspace acceptance (M64): a real sshfs mount over loopback, a write through the
# mount landing on the remote side, and a clean detach. It needs sshfs, a non-interactive
# loopback ssh, and the ssh server in the same mount namespace — so it runs on the gateway
# host, never inside the development sandbox (where it skips itself).
dshgw-ssh-integration:
	@$(GOENV) DSHGW_SSH_INTEGRATION=1 go test ./internal/dshgw/sshworkspace -run '^TestIntegration' -count=1 -v

# The ssh workspace end-to-end acceptance (M64): a throwaway dshgw in its own state directory
# and port band, a real sshfs mount created from an account's mailbox request, the proof that
# the mount is visible INSIDE that account's sandbox, and a clean teardown. Needs the gateway
# host: bwrap, sshfs, and a non-interactive loopback ssh.
dshgw-ssh-e2e:
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_BIN_JS="$(DSHGW_DSH_ROOT)/lib/bin.js" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" \
		PYTHONDONTWRITEBYTECODE=1 python3 scripts/ssh_workspace_e2e.py

# The logout acceptance (M76): "dsh 点击退出按钮后，强制 umount 使用挂载文件系统，最后强制退出 dsh".
# A throwaway dshgw with both mount kinds attached at once — a real browser-directory FUSE mount
# (driven by a stand-in for the browser side of the poll protocol, so no Chromium is needed) bound
# into the account's sandbox, and a real sshfs mount — then POST /dshgw/logout/ (the sidebar's 退出):
# both mounts leave the kernel table, the dsh's worker port closes, the audit records the detach and
# the verified stop, and the next sign-in puts the ssh workspace back at the same path. Needs the
# gateway host: /dev/fuse, fusermount3, bwrap, sshfs, a non-interactive loopback ssh, and a prepared
# dsh template. The busy/unmount-EBUSY half of the force ladder is pinned by the Go test
# TestRealFUSEForceUnmountTakesABusyMount (BROWSERWORKSPACE_FUSE_TEST=1).
dshgw-logout-e2e: dshgw-build
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_BIN_JS="$(DSHGW_DSH_ROOT)/lib/bin.js" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" \
		PYTHONDONTWRITEBYTECODE=1 python3 scripts/dshgw_logout_teardown_e2e.py --dshgw bin/dshgw

# The bwrap isolation mode's real acceptance: the tenant profile runs under the
# host's own bubblewrap, and a real dsh web worker starts inside it and answers
# the gateway's unauthenticated /api probe with 401. Both tests skip themselves
# where an unprivileged user namespace is unavailable, so this target is safe to
# call from any checkout, but it is only meaningful on the deployment host.
dshgw-sandbox-test:
	@$(GOENV) DSHGW_NODE="$(DSHGW_NODE)" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" go test ./internal/dshgw/sandbox -run '^TestStaging' -count=1 -v

# The supervised shape's host acceptance (M58): aigw starts its dshgw child, that
# child runs a bubblewrap tenant worker, and the tenant answers the /api probe with
# 401 — all as the invoking account, on its own ports and work directory, so it
# never touches a running deployment. It skips itself when the host lacks bwrap or
# a staged dsh runtime, and needs bin/aigw next to bin/dshgw.
# Both binaries are prerequisites: this acceptance runs the real aigw, and a stale
# bin/aigw would silently test an older tree (which is exactly how a missing
# worker_limits passthrough once went unnoticed here).
dshgw-supervised-test: build dshgw-build
	@if [ ! -x bin/aigw ]; then echo 'SKIP: bin/aigw missing (run make build first)'; exit 0; fi
	@PYTHONDONTWRITEBYTECODE=1 python3 scripts/dshgw_supervised_e2e.py \
	  --aigw-bin bin/aigw --dshgw-bin bin/dshgw \
	  --node "$(DSHGW_NODE)" --dsh-root "$(DSHGW_DSH_ROOT)"

dshgw-verify: dshgw-test dshgw-build
	@$(MAKE) --no-print-directory dshgw-sandbox-test
	@$(MAKE) --no-print-directory dshgw-supervised-test
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
		node internal/webui/tests/org_person_list_test.mjs || exit $$? ; \
		node internal/webui/tests/tenant_name_test.mjs || exit $$? ; \
		node internal/webui/tests/dshgw_nodes_wiring_test.mjs || exit $$? ; \
		node --experimental-vm-modules internal/webui/tests/org_assign_test.mjs || exit $$? ; \
		node --experimental-vm-modules internal/webui/tests/keys_feishu_test.mjs || exit $$? ; \
		node --experimental-vm-modules internal/webui/tests/org_feishu_test.mjs || exit $$? ; \
		node --experimental-vm-modules internal/webui/tests/account_feishu_test.mjs || exit $$? ; \
		node --experimental-vm-modules internal/webui/tests/dshgw_nodes_test.mjs || exit $$? ; \
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

# Browser-backed filesystem and tenant integration, without requiring a FUSE host.
.PHONY: dshgw-browser-test
dshgw-browser-test:
	@$(GOENV) go test ./internal/dshgw/browserworkspace ./internal/dshgw/browsermount ./internal/dshgw/config ./internal/dshgw/tenancy ./internal/dshgw/sandbox ./internal/dshgw/proxy ./internal/dshgwsup ./cmd/aigw ./cmd/dshgw

# The browser workspace acceptance (M65): a throwaway dshgw in its own state directory and
# port band, a real Chromium whose directory chooser is replaced by a real OPFS handle, a real
# FUSE mount, real I/O in both directions, and the proof that the mount is visible INSIDE the
# account's bubblewrap sandbox. Needs the gateway host: /dev/fuse, bwrap, fusermount3,
# Chromium, the installed dsh runtime and a prepared dsh template.
dshgw-browser-e2e: dshgw-build
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_BIN_JS="$(DSHGW_DSH_ROOT)/lib/bin.js" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" \
		PYTHONDONTWRITEBYTECODE=1 python3 scripts/browser_workspace_mount_e2e.py --dshgw bin/dshgw

# What happens to that mount when the page that owns it goes away or its transport breaks:
# an injected failure of this plugin's own endpoints (recovered in place, no reload and no
# click), a real reload, and a real closed-tab-then-new-tab — each measured from the host,
# from inside the sandbox and from the page, and each pinned to the expected outcome:
#   control alive · reconnect alive · reload dead (until the click) · restore alive · reopen alive
dshgw-browser-reload-e2e: dshgw-build
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_BIN_JS="$(DSHGW_DSH_ROOT)/lib/bin.js" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" \
		PYTHONDONTWRITEBYTECODE=1 python3 scripts/browser_workspace_reload_e2e.py --dshgw bin/dshgw \
		--expect alive --expect-reconnect alive --expect-reload dead --expect-restore alive --expect-reopen alive
	@test -x "$(DSHGW_NODE)" || { echo "dshgw-browser-test: Node missing: $(DSHGW_NODE)" >&2; exit 1; }
	@DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" "$(DSHGW_NODE)" --test cmd/dshgw/plugin/browser-workspace/*.test.mjs

# SEVERAL directories at once, and the folder list that manages them: two local directories
# mounted side by side with independent I/O, one disconnected while the other keeps serving
# (host and sandbox), the disconnected one reconnected at the SAME stable path and the SAME
# workspace id, then deleted (its mount point and workspace entry released), and the survivor
# restored by one click after a page reload.
dshgw-browser-multi-e2e: dshgw-build
	@DSHGW_NODE="$(DSHGW_NODE)" DSHGW_BIN_JS="$(DSHGW_DSH_ROOT)/lib/bin.js" DSHGW_DSH_ROOT="$(DSHGW_DSH_ROOT)" \
		PYTHONDONTWRITEBYTECODE=1 python3 scripts/browser_workspace_multi_e2e.py --dshgw bin/dshgw
