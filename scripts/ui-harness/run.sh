#!/usr/bin/env bash
# Console behaviour harness for the administration UI.
#
# This environment has no node/npm (recorded in docs/TODO.md), so the management
# console cannot be exercised by a JS test runner. This script drives a real browser
# instead: it serves the embedded static assets plus a harness page whose fetch() is
# stubbed with captured API fixtures, renders the page, performs the interactions a
# human would, and reads the assertions back over HTTP.
#
# Usage:
#   scripts/ui-harness/run.sh                     # all views, fixtures.json
#   scripts/ui-harness/run.sh --views docs detail # a subset
#   scripts/ui-harness/run.sh --fixtures x.json   # your own captured fixtures
#   scripts/ui-harness/run.sh --refresh           # re-capture fixtures from :8088
#
# See scripts/ui-harness/README.md for why it is built this way.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="${UI_HARNESS_WORK:-$ROOT/.cache/ui-harness}"
PORT="${UI_HARNESS_PORT:-8097}"
VIEWS="docs detail capacity models create plugin plugin-cached currency keys requests paging chat noSkills skills form bridge brand tree org org-readonly org-accounts"
FIXTURES="$ROOT/scripts/ui-harness/fixtures.json"
REFRESH=0

while [ $# -gt 0 ]; do
  case "$1" in
    --views) VIEWS="$2"; shift 2 ;;
    --fixtures) FIXTURES="$2"; shift 2 ;;
    --refresh) REFRESH=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v python3 >/dev/null 2>&1 || { echo "skip: python3 is not available"; exit 0; }

FIREFOX=""
# Order matters: on this host /usr/bin/firefox is the snap wrapper, which refuses to start
# with a read-only $HOME ("cannot create user data directory") and then loads no page at
# all — the run looks like "every view failed" with an empty server log. The snap's real
# binary works once HOME and XDG_RUNTIME_DIR point somewhere writable, which is arranged
# below.
for candidate in /snap/firefox/current/usr/lib/firefox/firefox /usr/lib/firefox/firefox firefox; do
  if command -v "$candidate" >/dev/null 2>&1; then FIREFOX="$candidate"; break; fi
done
[ -n "$FIREFOX" ] || { echo "skip: no firefox binary found (set one on PATH to run the UI harness)"; exit 0; }

if [ "$REFRESH" = 1 ]; then
  command -v curl >/dev/null 2>&1 || { echo "--refresh needs curl" >&2; exit 2; }
  echo "refreshing fixtures from ${GW_BASE:-http://127.0.0.1:8088} (needs an admin session cookie)"
  python3 "$ROOT/scripts/ui-harness/capture.py" || exit 1
fi

[ -f "$FIXTURES" ] || { echo "fixtures not found: $FIXTURES" >&2; exit 2; }

rm -rf "$WORK"
mkdir -p "$WORK/site" "$WORK/home" "$WORK/run" "$WORK/profiles"
chmod 700 "$WORK/run"
cp -r "$ROOT/internal/webui/static/." "$WORK/site/"

# One harness page per area under test: providers.page.html carries the provider and
# plugin views, currency.page.html the multi-currency money rendering, keys.page.html the
# key editor and the request log. All embed the same fixtures, so a view only has to name
# the page that renders it.
render_page() {
  python3 "$ROOT/scripts/ui-harness/render_page.py" "$1" "$FIXTURES" "$2"
}
render_page "$ROOT/scripts/ui-harness/providers.page.html" "$WORK/site/harness.html"
render_page "$ROOT/scripts/ui-harness/currency.page.html" "$WORK/site/currency.html"
render_page "$ROOT/scripts/ui-harness/keys.page.html" "$WORK/site/keys.html"
render_page "$ROOT/scripts/ui-harness/paging.page.html" "$WORK/site/paging.html"
render_page "$ROOT/scripts/ui-harness/chat.page.html" "$WORK/site/chat.html"
render_page "$ROOT/scripts/ui-harness/bridge_syntax.page.html" "$WORK/site/bridge_syntax.html"
render_page "$ROOT/scripts/ui-harness/brand.page.html" "$WORK/site/brand.html"
render_page "$ROOT/scripts/ui-harness/tree.page.html" "$WORK/site/tree.html"
render_page "$ROOT/scripts/ui-harness/org.page.html" "$WORK/site/org.html"

page_for_view() {
  case "$1" in
    currency) echo "currency.html" ;;
    keys|requests) echo "keys.html" ;;
    paging) echo "paging.html" ;;
    # noSkills is the same chat page with an empty skill library (the page reads it off the
    # hash): the refusal path needs a page that was empty from its first load.
    chat|noSkills|skills|form) echo "chat.html" ;;
    # Not a console view: the browser's syntax check on the script the server injects into an
    # interactive preview. It shares the fixtures mechanism, so it rides along with the others.
    bridge) echo "bridge_syntax.html" ;;
    # Not a console view either: the sidebar brand with its build badge, rendered straight
    # from js/brand.js against a stubbed version lookup and a stubbed fetch.
    brand) echo "brand.html" ;;
    # The reusable tree control on its own (it needs no API at all), and the organization page
    # with its two placements plus the account page's organization column and filter.
    tree) echo "tree.html" ;;
    org|org-readonly|org-accounts) echo "org.html" ;;
    *) echo "harness.html" ;;
  esac
}

# The static server is scripts/ui-harness/server.py rather than `python3 -m http.server`,
# because it answers every asset with no-store: the console's own `max-age=300` is right in
# production and wrong here, where it makes the browser run the previous version of a module
# that was just edited.
cp "$ROOT/scripts/ui-harness/server.py" "$WORK/site/server.py"

# A readiness sentinel, so "the server is up" means "this run's directory is being served".
# Asking for harness.html is not enough on its own: curl without -f treats an error page as
# success, so a listener left over from an earlier run — answering 404 out of a directory this
# run already deleted — would pass the check and every view would then fail with "no report".
READY_TOKEN="ready-$$-${RANDOM}"
printf '%s' "$READY_TOKEN" >"$WORK/site/harness-ready.txt"

# server_answers reports whether anything is listening on $PORT right now. curl exits 7 when the
# connection is refused, which is the only signal available without ss/lsof in this image.
server_answers() { curl -s -o /dev/null -m 1 "http://127.0.0.1:$PORT/harness-ready.txt" 2>/dev/null; }

# stop_server kills the server AND waits for the port to be released.
#
# Two things here are load-bearing. The pid must be the server's own (see the exec below), and
# the wait matters because the next run binds the same port immediately: returning while the old
# process still holds it makes the next invocation die with "Address already in use" — which is
# exactly how `make ui-check` twice in a row used to fail.
SERVER_PID=""
stop_server() {
  [ -n "$SERVER_PID" ] || return 0
  kill "$SERVER_PID" 2>/dev/null
  for _ in $(seq 1 50); do
    server_answers || break
    sleep 0.1
  done
  kill -9 "$SERVER_PID" 2>/dev/null
  SERVER_PID=""
}
# Cleanup on every exit path: an interrupted run (^C, a timeout, a failed assertion) must not
# leave a listener behind for the next one. A signal exits and lets the EXIT trap do the work,
# rather than resuming the script from wherever it was interrupted.
trap stop_server EXIT
trap 'exit 130' INT TERM

# A listener left by an interrupted or older run would make the bind below fail. Report it as
# what it is instead of printing a Python traceback the reader has to decode.
if server_answers; then
  echo "port $PORT is already in use by another process; refusing to start a second harness server." >&2
  echo "stop it first (for example: pkill -f 'server.py $PORT'), or run with UI_HARNESS_PORT=<port>." >&2
  exit 2
fi

# The bridge script an interactive preview runs is generated by Go code, so it cannot be
# captured from a running instance: it lives in the harness fixtures as a synthetic entry (the
# same convention capture.py documents for the plugin scenarios). What keeps that entry honest
# is TestUIBridgeScriptDumpForTheHarness in internal/httpapi, which rewrites it from
# uiBridgeScript() on every `go test` — so the browser view below always parses exactly what the
# server would serve, and a syntax error introduced while editing that string fails the harness
# instead of shipping.
#
# `exec` is what makes `$!` the server's own pid: without it the shell forks a subshell for the
# `cd && python3` list, `$!` names that short-lived subshell, and the kill below hits a dead pid
# while the real server stays alive holding the port.
(cd "$WORK/site" && exec python3 server.py "$PORT") >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
echo "$SERVER_PID" >"$WORK/server.pid"

ready=0
for _ in $(seq 1 50); do
  if body=$(curl -fsS -m 2 "http://127.0.0.1:$PORT/harness-ready.txt" 2>/dev/null) && [ "$body" = "$READY_TOKEN" ]; then
    ready=1
    break
  fi
  # If the server died on startup (a busy port, a Python error) there is nothing to wait for.
  kill -0 "$SERVER_PID" 2>/dev/null || break
  sleep 0.1
done
if [ "$ready" != 1 ]; then
  echo "the harness server did not start; see $WORK/server.log" >&2
  tail -5 "$WORK/server.log" >&2 2>/dev/null
  exit 1
fi

for view in $VIEWS; do
  profile="$WORK/profiles/$view"
  mkdir -p "$profile"
  HOME="$WORK/home" XDG_RUNTIME_DIR="$WORK/run" MOZ_HEADLESS=1 \
    timeout 90 "$FIREFOX" --headless --no-remote --profile "$profile" \
    --window-size=1500,2400 --screenshot "$WORK/$view.png" \
    "http://127.0.0.1:$PORT/$(page_for_view "$view")#$view" >/dev/null 2>&1
  echo "ran: $view"
done

# Stop the server before reading its log, and wait for the port to be released so a caller that
# runs this twice in a row (or a CI loop) does not race the next bind.
stop_server

python3 - "$WORK/server.log" "$VIEWS" <<'PY'
import json, sys, urllib.parse
log, views = sys.argv[1], sys.argv[2].split()
reports = {}
for line in open(log, encoding='utf-8', errors='replace'):
    if '/report?data=' not in line:
        continue
    raw = line.split('/report?data=', 1)[1].split(' ', 1)[0]
    try:
        data = json.loads(urllib.parse.unquote(raw))
    except Exception:
        continue
    if data.get('stage') in ('done', 'threw'):
        reports[data.get('view')] = data

failed = []
for view in views:
    data = reports.get(view)
    if data is None:
        print(f"[FAIL] {view}: no report (the page was torn down before the assertions ran)")
        failed.append(view)
        continue
    checks = data.get('checks', {})
    bad = [k for k, v in checks.items() if v is False or (isinstance(v, (int, float)) and v <= 0)]
    # A view may declare `strictChecks: true` to mean "every entry in checks is a VERDICT".
    # Under that flag anything that is not a boolean or a number fails, because a truthy string
    # (an assertion helper returning 'got […] want […]', say) is otherwise counted as a pass —
    # which is how one such helper silently passed here.
    #
    # The flag is opt-in rather than global because several views deliberately use checks as an
    # evidence scratchpad (models.rowActions is the list of buttons it found, bridge.parseError
    # is '' when the script parsed, chat.uiApplyThrew is '' when nothing threw) and carry their
    # verdict in a separate boolean. Those views predate this rule; changing their meaning is a
    # separate cleanup, not something a new rule should do behind the scenes.
    strict = data.get('strictChecks') is True
    for key, value in checks.items():
        if key in ('sample', 'fieldRows'):
            continue
        if not isinstance(value, (bool, int, float)):
            if strict:
                bad.append(key)
                print(f"        {key}: 检查项不是布尔/数值（{type(value).__name__}），按失败处理：{str(value)[:120]}")
            else:
                print(f"        {key}: 非判定项（{type(value).__name__}），仅供参考：{str(value)[:120]}")
    # `threwLate` means the view's script threw after collecting some checks; the harness catches
    # it so the partial report survives, but a throw must still fail the view — otherwise a
    # broken helper silently skips half the assertions and the run reports success.
    if checks.get('threwLate'):
        bad.append('threwLate')
    # `bad` decides, not the page's own `ok`: the page can only judge the checks it managed to
    # record, while `bad` also carries a late throw (a helper that broke mid-view).
    state = 'ok' if (data.get('ok') and not bad) else 'FAIL'
    print(f"[{state}] {view}: {len(checks)} checks" + (f", failed={bad}" if bad else ""))
    if data.get('thrown'):
        print(f"        thrown: {data['thrown'].splitlines()[0]}")
    if data.get('errors'):
        print(f"        page errors: {data['errors']}")
    for key in ('fieldRows', 'sample'):
        if key in checks:
            print(f"        {key}: {checks[key]}")
    if not data.get('ok') or bad:
        failed.append(view)

if failed:
    print(f"\n{len(failed)} view(s) failed: {', '.join(failed)}")
    sys.exit(1)
print("\nall views passed")
PY
status=$?
echo "artifacts: $WORK (screenshots are not a reliable signal — see README)"
exit $status
