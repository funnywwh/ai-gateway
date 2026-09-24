#!/usr/bin/env bash
# Workspace-local Go environment.
#
# Why: in this sandbox $HOME is read-only and there is no C compiler, so
#   * GOPATH/GOMODCACHE/GOCACHE must live inside the workspace (writable), and
#   * the pre-populated read-only module cache is reused as a file:// proxy.
# Usage:  source scripts/goenv.sh && go build ./...
set -a
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/sdk/go/bin:$PATH"
# Some workspaces carry the SDK inside the checkout's gitignored .cache/ instead of
# $HOME/sdk (both exist in practice); prefer it only when it is actually there, so the
# host layout keeps working unchanged.
if [ -x "$ROOT/.cache/go/bin/go" ]; then
  export PATH="$ROOT/.cache/go/bin:$PATH"
fi
export GOPATH="$ROOT/.cache/gopath"
export GOMODCACHE="$ROOT/.cache/gomod"
export GOCACHE="$ROOT/.cache/gobuild"
export GOFLAGS=-mod=mod
export GOSUMDB=off
export GONOSUMDB='*'
export GOTOOLCHAIN=local
export CGO_ENABLED=0
export GOPROXY="file://$HOME/go/pkg/mod/cache/download,https://goproxy.cn"
set +a
