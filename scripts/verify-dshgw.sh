#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
NODE=${DSHGW_NODE:-/home/winger/.local/node-v22.23.1-linux-x64/bin/node}
DSH_ROOT=${DSHGW_DSH_ROOT:-/home/winger/.local/dsh-0.1.2-rc.1}
TMP=$(mktemp -d /tmp/dshgw-verify.XXXXXX)
template_pid=
cleanup(){
  if [[ -n "$template_pid" ]] && kill -0 "$template_pid" 2>/dev/null; then
    kill "$template_pid" 2>/dev/null || true
    wait "$template_pid" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

bash -n "$ROOT/deploy/dshgw/prepare-template.sh"
cp "$ROOT/deploy/dshgw/config.example.yaml" "$TMP/config.yaml"
# The example names its data root relatively (./data/dshgw, M63) and its runtime
# installation absolutely. This check must not write into the repository's real data root,
# so both kinds of value are repointed at the temporary tree: the data root becomes
# $TMP/data, and the picker plugin is read from this checkout.
python3 - "$TMP/config.yaml" "$NODE" "$DSH_ROOT" "$TMP" "$ROOT" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
node = Path(sys.argv[2])
dsh = Path(sys.argv[3])
tmp = Path(sys.argv[4])
root = Path(sys.argv[5])
s = p.read_text()
replacements = {
    '/home/winger/.local/node-v22.23.1-linux-x64/bin/node': str(node),
    '/home/winger/.local/dsh-0.1.2-rc.1/lib/bin.js': str(dsh / 'lib/bin.js'),
    '/home/winger/.local/dsh-0.1.2-rc.1': str(dsh),
    './data/dshgw': str(tmp / 'data/dshgw'),
    './cmd/dshgw/plugin/picker-clamp.js': str(root / 'cmd/dshgw/plugin/picker-clamp.js'),
}
for old, new in sorted(replacements.items(), key=lambda item: len(item[0]), reverse=True):
    s = s.replace(old, new)
p.write_text(s)
PY
"$ROOT/bin/dshgw" --version
"$ROOT/bin/dshgw" --config "$TMP/config.yaml" contract dsh

# Build a disposable profile from the installed DSH and Node. The test-only
# mode is restricted by prepare-template.sh to /tmp destinations and skips
# only the root-owned chown; it never touches /opt or a production profile.
template_home="$TMP/template-home"
DSHGW_TEMPLATE_TEST_MODE=1 DSHGW_NODE="$NODE" DSHGW_DSH_ROOT="$DSH_ROOT" \
  DSHGW_TEMPLATE_HOME="$template_home" "$ROOT/deploy/dshgw/prepare-template.sh"
[[ -f "$template_home/profiles/web/package.json" ]] || { echo 'temporary template missing web profile' >&2; exit 1; }
[[ -f "$template_home/profiles/web/pnpm-lock.yaml" ]] || { echo 'temporary template missing pnpm lockfile' >&2; exit 1; }
"$NODE" - "$template_home/profiles/web/package.json" <<'NODECHECK'
const fs = require('node:fs');
const path = process.argv[2];
const profile = JSON.parse(fs.readFileSync(path));
if (profile.dependencies?.['dsh-browser-fs'] !== '0.2.0') throw new Error('temporary template browser-fs version mismatch');
if (!profile.dsh?.profile?.bundles?.includes('dsh-browser-fs')) throw new Error('temporary template browser-fs bundle missing');
NODECHECK

template_port=$(python3 - <<'PORTCHECK'
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
PORTCHECK
)
template_log="$TMP/template-dsh.log"
sanitize_log(){ sed -E 's/(token=)[A-Za-z0-9_-]+/\1[REDACTED]/g; s/(dsh-auth-)[A-Za-z0-9_-]+/\1[REDACTED]/g' "$1"; }
HOME="$TMP/template-work" DSH_HOME="$template_home" \
  "$NODE" "$DSH_ROOT/lib/bin.js" web --port "$template_port" --no-open >"$template_log" 2>&1 &
template_pid=$!
cookie_jar="$TMP/template.cookies"
startup_url=
static_status=
for _ in $(seq 1 120); do
  if [[ -z "$startup_url" ]]; then
    startup_url=$(sed -n 's/^dsh web: //p' "$template_log" | head -n 1 || true)
    if [[ -n "$startup_url" ]]; then
      exchange_status=$(curl --noproxy '*' --silent --show-error --output /dev/null --write-out '%{http_code}' \
        --max-time 5 --cookie-jar "$cookie_jar" "$startup_url" || true)
      [[ "$exchange_status" == 303 ]] || { sanitize_log "$template_log" >&2; echo "temporary DSH token exchange status: $exchange_status" >&2; exit 1; }
    fi
  fi
  static_status=$(curl --noproxy '*' --silent --output /dev/null --write-out '%{http_code}' \
    --max-time 2 --cookie "$cookie_jar" "http://127.0.0.1:$template_port/" || true)
  [[ "$static_status" == 200 ]] && break
  if ! kill -0 "$template_pid" 2>/dev/null; then
    sanitize_log "$template_log" >&2
    echo 'temporary DSH exited before static 200' >&2
    exit 1
  fi
  sleep 0.25
done
[[ "$static_status" == 200 ]] || { sanitize_log "$template_log" >&2; echo "temporary DSH static status: $static_status" >&2; exit 1; }
index_html="$TMP/template-index.html"
curl --noproxy '*' --silent --show-error --fail --max-time 5 --cookie "$cookie_jar" \
  "http://127.0.0.1:$template_port/" >"$index_html"
plugin_path=$(python3 - "$index_html" <<'PLUGINURL'
import html
import re
import sys
text = open(sys.argv[1], encoding='utf-8').read()
match = re.search(r'/plugins/\?\?[^" ]*dsh-browser-fs/client\.js[^" ]*', text)
if not match:
    raise SystemExit('advertised dsh-browser-fs client module URL is missing')
print(html.unescape(match.group(0)))
PLUGINURL
)
plugin_status=$(curl --noproxy '*' --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 5 --cookie "$cookie_jar" "http://127.0.0.1:$template_port$plugin_path" || true)
[[ "$plugin_status" == 200 ]] || { sanitize_log "$template_log" >&2; echo "temporary browser-fs client status: $plugin_status (want 200)" >&2; exit 1; }
plain_ws_status=$(curl --noproxy '*' --http1.1 --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 5 --cookie "$cookie_jar" "http://127.0.0.1:$template_port/browser-fs/ws" || true)
[[ "$plain_ws_status" == 426 ]] || { sanitize_log "$template_log" >&2; echo "temporary plain browser-fs WS status: $plain_ws_status (want 426)" >&2; exit 1; }
ws_headers="$TMP/template-ws.headers"
set +e
ws_status=$(curl --noproxy '*' --http1.1 --silent --show-error --dump-header "$ws_headers" --output /dev/null --write-out '%{http_code}' \
  --max-time 5 --cookie "$cookie_jar" -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  "http://127.0.0.1:$template_port/browser-fs/ws")
ws_rc=$?
set -e
[[ "$ws_status" == 101 && "$ws_rc" == 0 || "$ws_status" == 101 && "$ws_rc" == 28 ]] || {
  sanitize_log "$template_log" >&2
  echo "temporary upgraded browser-fs WS status: $ws_status (curl exit $ws_rc; want 101)" >&2
  exit 1
}
grep -Eq '^HTTP/[0-9.]+ 101 ' "$ws_headers" || { echo 'temporary upgraded browser-fs WS lacked an HTTP 101 response' >&2; exit 1; }
kill "$template_pid" 2>/dev/null || true
wait "$template_pid" 2>/dev/null || true
template_pid=

# Copy the prepared profile to a fresh home and prove startup does not invoke
# pnpm. The failing wrapper is deliberately first in PATH; a first-boot
# installation attempt creates the marker and fails the verification.
fresh_home="$TMP/fresh-home"
mkdir -p "$fresh_home"
cp -a "$template_home/profiles" "$fresh_home/"
pnpm_marker="$TMP/pnpm-called"
no_pnpm="$TMP/no-pnpm/bin"
mkdir -p "$no_pnpm"
cat >"$no_pnpm/pnpm" <<'PNPMFAIL'
#!/bin/sh
: "${DSHGW_PNPM_MARKER:?missing marker path}"
printf 'pnpm invoked\n' >"$DSHGW_PNPM_MARKER"
exit 97
PNPMFAIL
chmod 0755 "$no_pnpm/pnpm"
fresh_log="$TMP/fresh-dsh.log"
fresh_port=$(python3 - <<'PORTCHECK2'
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
PORTCHECK2
)
HOME="$TMP/fresh-work" DSH_HOME="$fresh_home" PATH="$no_pnpm:$(dirname "$NODE"):/usr/bin:/bin" \
  DSHGW_PNPM_MARKER="$pnpm_marker" "$NODE" "$DSH_ROOT/lib/bin.js" web --port "$fresh_port" --no-open >"$fresh_log" 2>&1 &
template_pid=$!
fresh_startup=
for _ in $(seq 1 120); do
  fresh_startup=$(sed -n 's/^dsh web: //p' "$fresh_log" | head -n 1 || true)
  [[ -n "$fresh_startup" ]] && break
  if ! kill -0 "$template_pid" 2>/dev/null; then
    sanitize_log "$fresh_log" >&2
    echo 'fresh DSH exited before startup' >&2
    exit 1
  fi
  sleep 0.25
done
[[ -n "$fresh_startup" ]] || { sanitize_log "$fresh_log" >&2; echo 'fresh DSH startup URL missing' >&2; exit 1; }
sleep 1
[[ ! -e "$pnpm_marker" ]] || { sanitize_log "$fresh_log" >&2; echo 'fresh DSH attempted first-boot pnpm installation' >&2; exit 1; }
fresh_cookie="$TMP/fresh.cookies"
fresh_exchange=$(curl --noproxy '*' --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 5 --cookie-jar "$fresh_cookie" "$fresh_startup" || true)
[[ "$fresh_exchange" == 303 ]] || { sanitize_log "$fresh_log" >&2; echo "fresh DSH token exchange status: $fresh_exchange" >&2; exit 1; }
kill "$template_pid" 2>/dev/null || true
wait "$template_pid" 2>/dev/null || true
template_pid=
echo "temporary template: browser-fs profile, static 200, client 200, plain WS 426, upgraded WS 101; fresh home made no pnpm call"
