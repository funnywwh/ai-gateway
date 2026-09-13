#!/usr/bin/env bash
# Acceptance for the response_format semantics (M39): does a client's text.format reach the
# upstream, and does a request *without* one stay free of the field?
#
# Why a script and not just a unit test: the unit test pins the body the provider builds, while
# this runs the real binary against a fake DeepSeek that records what it was actually sent. The
# defect it guards against (config.response_format forcing JSON mode onto every request) took a
# whole provider offline in production while every package test stayed green.
#
#   scripts/format-smoke.sh
#
# No network, no API key, no model cost: the upstream is a local stub.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

GW_PORT="${GW_PORT:-18191}"
UP_PORT="${UP_PORT:-18192}"
WORK="$(mktemp -d)"
fail=0

cleanup() { kill "${GW:-0}" "${UP:-0}" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

[ -x ./bin/aigw ] || { echo "build first: make build" >&2; exit 2; }

# The fake upstream records every /chat/completions body and answers with a minimal
# completion, so the assertions below read what DeepSeek would have received.
cat >"$WORK/upstream.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = sys.argv[1]


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length') or 0)
        raw = self.rfile.read(length)
        with open(LOG, 'a') as handle:
            handle.write(raw.decode('utf-8', 'replace') + "\n")
        body = json.dumps({
            "id": "chatcmpl-1", "object": "chat.completion", "created": 1,
            "model": "deepseek-flash",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
        }).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.send_response(404)
        self.end_headers()

    def log_message(self, *args):
        pass


HTTPServer(('127.0.0.1', int(sys.argv[2])), Handler).serve_forever()
PY
python3 "$WORK/upstream.py" "$WORK/upstream-bodies.jsonl" "$UP_PORT" &
UP=$!
sleep 1

# `response_format: json_object` is the line that broke production. It is kept here on purpose:
# it must no longer be able to force JSON mode onto a request that did not ask for it.
cat >"$WORK/config.yaml" <<YAML
server:
  listen: "127.0.0.1:$GW_PORT"
  secret_key: "0000000000000000000000000000000000000000000000000000000000000001"
database:
  path: "$WORK/aigw.db"
credentials_key: "0000000000000000000000000000000000000000000000000000000000000002"
plugins:
  dir: "$WORK/plugins"
  state_dir: "$WORK/state"
bootstrap:
  mode: upsert
  admin:
    username: admin
    password: "format-smoke-only"
  accounts:
    - name: verify
      billing_mode: postpaid
      credit_limit_usd: 1000
  api_keys:
    - name: vk
      key: "sk-format-smoke"
      account: verify
  providers:
    - name: deepseek
      kind: openai-chat
      enabled: true
      config:
        base_url: "http://127.0.0.1:$UP_PORT"
        response_format: json_object
        default_max_output_tokens: 8192
      models:
        - public: deepseek-flash
          upstream: deepseek-flash
          context_window: 1000000
          max_output_tokens: 65536
          capabilities: {stream: true, tools: true, reasoning: true, json_object: true}
  models:
    - public_name: deepseek-flash
  routes:
    - model: deepseek-flash
      provider: deepseek
      upstream_model: deepseek-flash
      priority: 10
      weight: 100
backup:
  enabled: false
log:
  level: warn
YAML

./bin/aigw --config "$WORK/config.yaml" >"$WORK/gateway.log" 2>&1 &
GW=$!
sleep 2

call() {
  curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer sk-format-smoke' \
    -H 'Content-Type: application/json' -d "$1" "http://127.0.0.1:$GW_PORT/v1/responses"
}

echo "== A. ordinary request, no text.format (the shape that failed in production) =="
A="$(call '{"model":"deepseek-flash","input":"hello","stream":false}')"
echo "   HTTP $A"
[ "$A" = "200" ] || { echo "   [FAIL] want 200"; fail=1; }

echo "== B. text.format = json_object =="
B="$(call '{"model":"deepseek-flash","input":"give me JSON","text":{"format":{"type":"json_object"}},"stream":false}')"
echo "   HTTP $B"
[ "$B" = "200" ] || { echo "   [FAIL] want 200"; fail=1; }

echo "== C. an invalid text.format.type is refused while parsing =="
C="$(call '{"model":"deepseek-flash","input":"hi","text":{"format":{"type":"yaml"}},"stream":false}')"
echo "   HTTP $C"
[ "$C" = "400" ] || { echo "   [FAIL] want 400"; fail=1; }

echo
echo "== what the upstream actually received =="
python3 - "$WORK/upstream-bodies.jsonl" <<'PY' || fail=1
import json, sys

rows = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
for i, body in enumerate(rows, 1):
    print("  #%d model=%s keys=%s" % (i, body.get("model"), ",".join(sorted(body.keys()))))
    print("      response_format = %s" % json.dumps(body.get("response_format")))

problems = []
if len(rows) != 2:
    problems.append("expected 2 upstream calls, saw %d" % len(rows))
# #1 is the ordinary request: the field must be absent. This is the regression's core.
if rows and "response_format" in rows[0]:
    problems.append("the ordinary request carried response_format: "
                    + json.dumps(rows[0].get("response_format")))
# #2 asked for json_object, so it must be there.
if len(rows) > 1 and (rows[1].get("response_format") or {}).get("type") != "json_object":
    problems.append("the json_object request lost its response_format: "
                    + json.dumps(rows[1].get("response_format")))
# Nothing may invent a level the client did not ask for (the configured ceiling is not a request).
if any((body.get("response_format") or {}).get("type") == "json_schema" for body in rows):
    problems.append("a json_schema level was invented for a request that did not ask for one")
# The invalid level is rejected before the upstream, so there must be no third call.
if len(rows) > 2:
    problems.append("an invalid text.format reached the upstream")
for message in problems:
    print("  [FAIL] " + message)
sys.exit(1 if problems else 0)
PY

if [ "$fail" = "0" ]; then
  echo
  echo "ok: an ordinary request carries no response_format; json_object travels when asked;"
  echo "    an invalid level is refused before the upstream"
else
  echo
  echo "format smoke FAILED"
fi
exit $fail
