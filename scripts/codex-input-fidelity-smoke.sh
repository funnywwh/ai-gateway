#!/usr/bin/env bash
# Acceptance for the codex input dialect (M45): run the real gateway and the real codex plugin
# binary against a fake subscription backend and read what the backend was actually sent.
#
# Why a script and not just unit tests: the 2026-09-14 production failure
# (`Missing required parameter: 'input[N].summary'`, then `Item with id 'rs_…' not found`) lived
# in the JSON the *plugin process* builds after the *host* re-encoded the client's items — three
# hops (client → gateway → plugin frame → upstream body) that each have their own struct. Unit
# tests pin each hop; this pins the chain, using the two binaries that get deployed.
#
#   scripts/codex-input-fidelity-smoke.sh
#
# No network, no credentials, no model cost: the upstream is a local stub that answers the
# subscription backend's SSE shape.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

GW_PORT="${GW_PORT:-18991}"
UP_PORT="${UP_PORT:-18992}"
WORK="$(mktemp -d)"
fail=0

cleanup() { kill "${GW:-0}" "${UP:-0}" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

[ -x ./bin/aigw ] || { echo "build first: make build" >&2; exit 2; }
[ -x ./bin/aigw-provider-codex ] || { echo "build first: make plugin-example" >&2; exit 2; }

mkdir -p "$WORK/plugins" "$WORK/state"
cp ./bin/aigw-provider-codex "$WORK/plugins/aigw-provider-codex"

# The fake backend records every /responses body, then answers with a reasoning item that has an
# empty summary and an encrypted blob — the shape the real backend returns for a stateless
# (store=false) request that asked for reasoning.encrypted_content.
cat >"$WORK/upstream.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = sys.argv[1]
E = "gAAAA_stub_encrypted_content"


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length') or 0)
        raw = self.rfile.read(length)
        with open(LOG, 'a') as handle:
            handle.write(raw.decode('utf-8', 'replace') + "\n")
        # The event order the real subscription backend uses: an item is streamed as deltas and
        # then stated again in its finished form, which is where the id and the encrypted blob
        # live. The finished reasoning item deliberately keeps an empty summary array.
        reasoning = {"type": "reasoning", "id": "rs_stub", "summary": [],
                     "content": [], "encrypted_content": E, "status": "completed"}
        message = {"type": "message", "id": "msg_stub", "role": "assistant", "status": "completed",
                   "content": [{"type": "output_text", "text": "ok", "annotations": []}]}
        frames = [
            ("response.created", {"type": "response.created",
                                  "response": {"id": "resp_stub", "status": "in_progress", "output": []}}),
            ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0,
                                            "item": {"type": "reasoning", "id": "rs_stub",
                                                     "summary": [], "status": "in_progress"}}),
            ("response.reasoning_summary_part.added", {"type": "response.reasoning_summary_part.added",
                                                       "item_id": "rs_stub", "output_index": 0,
                                                       "summary_index": 0,
                                                       "part": {"type": "summary_text", "text": ""}}),
            ("response.reasoning_summary_text.delta", {"type": "response.reasoning_summary_text.delta",
                                                       "item_id": "rs_stub", "output_index": 0,
                                                       "summary_index": 0, "delta": "weighing the "}),
            ("response.reasoning_summary_text.delta", {"type": "response.reasoning_summary_text.delta",
                                                       "item_id": "rs_stub", "output_index": 0,
                                                       "summary_index": 0, "delta": "options"}),
            ("response.reasoning_summary_text.done", {"type": "response.reasoning_summary_text.done",
                                                      "item_id": "rs_stub", "output_index": 0,
                                                      "summary_index": 0, "text": "weighing the options"}),
            ("response.reasoning_summary_part.done", {"type": "response.reasoning_summary_part.done",
                                                      "item_id": "rs_stub", "output_index": 0,
                                                      "summary_index": 0,
                                                      "part": {"type": "summary_text",
                                                               "text": "weighing the options"}}),
            ("response.output_item.done", {"type": "response.output_item.done",
                                           "output_index": 0, "item": reasoning}),
            ("response.output_item.added", {"type": "response.output_item.added", "output_index": 1,
                                            "item": {"type": "message", "id": "msg_stub",
                                                     "role": "assistant", "status": "in_progress",
                                                     "content": []}}),
            ("response.output_text.delta", {"type": "response.output_text.delta", "item_id": "msg_stub",
                                            "output_index": 1, "delta": "ok"}),
            ("response.output_text.done", {"type": "response.output_text.done", "item_id": "msg_stub",
                                           "output_index": 1, "text": "ok"}),
            ("response.output_item.done", {"type": "response.output_item.done", "output_index": 1,
                                           "item": message}),
            ("response.completed", {"type": "response.completed",
                                    "response": {"id": "resp_stub", "status": "completed",
                                                 "output": [reasoning, message],
                                                 "usage": {"input_tokens": 5, "output_tokens": 3,
                                                           "total_tokens": 8}}}),
        ]
        payload = "".join("event: %s\ndata: %s\n\n" % (name, json.dumps(data)) for name, data in frames).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

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
    password: "codex-fidelity-only"
  accounts:
    - name: verify
      billing_mode: postpaid
      credit_limit_micros: 1000000000
  api_keys:
    - name: vk
      key: "sk-codex-fidelity"
      account: verify
      grants:
        models: ["*"]
        providers: ["*"]
  providers:
    - name: codex
      kind: plugin:provider-codex
      enabled: true
      config:
        base_url: "http://127.0.0.1:$UP_PORT"
        store: false
      models:
        - public: codex-test
          upstream: gpt-5-codex
          context_window: 200000
          max_output_tokens: 32000
          capabilities: {stream: true, tools: true, reasoning: true}
  models:
    - public_name: codex-test
  routes:
    - model: codex-test
      provider: codex
      upstream_model: gpt-5-codex
      priority: 10
      weight: 100
backup:
  enabled: false
log:
  level: warn
YAML

# The plugin takes an access token; a static one keeps the smoke off the login path.
HTTP_PROXY= HTTPS_PROXY= http_proxy= https_proxy= NO_PROXY=127.0.0.1,localhost \
  ./bin/aigw --config "$WORK/config.yaml" >"$WORK/gateway.log" 2>&1 &
GW=$!
sleep 2

ADMIN="http://127.0.0.1:$GW_PORT"
COOKIE="$WORK/cookie.txt"
login=$(curl -s -c "$COOKIE" -X POST "$ADMIN/admin/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"codex-fidelity-only"}')
provider_id=$(curl -s -b "$COOKIE" "$ADMIN/admin/api/v1/providers" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])')
curl -s -b "$COOKIE" -X PATCH "$ADMIN/admin/api/v1/providers/$provider_id" -H 'Content-Type: application/json' \
  -d '{"credentials":{"access_token":"stub-token"}}' >/dev/null
sleep 1

# The client's request: a reasoning item from an earlier turn, replayed with the empty summary
# array codex sends, plus the include that makes a stateless replay possible.
cat >"$WORK/request.json" <<'JSON'
{"model":"codex-test","stream":true,"store":false,
 "include":["reasoning.encrypted_content"],
 "reasoning":{"effort":"high","summary":"auto"},
 "input":[
   {"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
   {"type":"reasoning","id":"rs_prev","summary":[],"content":[{"type":"reasoning_text","text":"prior"}],
    "status":"completed","encrypted_content":"gAAAA_prev_blob"},
   {"type":"function_call","call_id":"call_1","name":"noop","arguments":""},
   {"type":"function_call_output","call_id":"call_1","output":""},
   {"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}
JSON

curl -sN -X POST "$ADMIN/v1/responses" -H 'Authorization: Bearer sk-codex-fidelity' \
  -H 'Content-Type: application/json' --data-binary "@$WORK/request.json" >"$WORK/response.sse"
echo "$login" >/dev/null

echo "== 1. what the fake backend was sent (the client's shapes must survive three hops) =="
python3 - "$WORK/upstream-bodies.jsonl" <<'PY'
import json, sys

bodies = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
if not bodies:
    print("   [FAIL] the backend was never called"); raise SystemExit(1)
body = bodies[-1]
fail = 0

def check(ok, what, got):
    global fail
    print(("   ok   " if ok else "   [FAIL] ") + what + ("" if ok else " -> %r" % (got,)))
    if not ok:
        fail = 1

check(body.get("include") == ["reasoning.encrypted_content"],
      "include reaches the upstream", body.get("include"))
reasoning = next((i for i in body["input"] if i.get("type") == "reasoning"), None)
check(reasoning is not None, "the reasoning item was forwarded", body["input"])
if reasoning is not None:
    check("summary" in reasoning, "the empty summary key survives", sorted(reasoning))
    check(reasoning.get("summary") == [], "summary is the empty array the client sent", reasoning.get("summary"))
    check(reasoning.get("encrypted_content") == "gAAAA_prev_blob",
          "encrypted_content is forwarded verbatim", reasoning.get("encrypted_content"))
    check("status" not in reasoning, "the output-only status is dropped", sorted(reasoning))
    check("content" not in reasoning, "the forbidden reasoning content array is dropped", sorted(reasoning))
call = next((i for i in body["input"] if i.get("type") == "function_call"), None)
check(call is not None and "arguments" in call, "an explicitly empty arguments survives", call)
out = next((i for i in body["input"] if i.get("type") == "function_call_output"), None)
check(out is not None and "output" in out, "an explicitly empty tool output survives", out)
msg = next(i for i in body["input"] if i.get("type") == "message")
check("summary" not in msg, "a user message does not grow a summary key", sorted(msg))
raise SystemExit(fail)
PY
[ $? -eq 0 ] || fail=1

echo "== 2. what the client got back (the identity and blob it needs for the next turn) =="
python3 - "$WORK/response.sse" <<'PY'
import json, sys

def events(path):
    for block in open(path).read().split("\n\n"):
        if not block.strip():
            continue
        names = [l[7:] for l in block.split("\n") if l.startswith("event: ")]
        if not names:
            continue  # a bare data line (or padding) carries no event name
        data = [l[6:] for l in block.split("\n") if l.startswith("data: ")]
        yield names[0], json.loads(data[0]) if data else {}

fail = 0

def check(ok, what, got=None):
    global fail
    print(("   ok   " if ok else "   [FAIL] ") + what + ("" if ok else " -> %r" % (got,)))
    if not ok:
        fail = 1

stream = list(events(sys.argv[1]))
completed = [d for name, d in stream if name == "response.completed"]
if not completed:
    print("   [FAIL] no response.completed in the stream")
    raise SystemExit(1)
response = completed[0]["response"]
items = response.get("output", [])
reasoning = [i for i in items if i.get("type") == "reasoning"]

check(response.get("status") == "completed", "response status is completed", response.get("status"))
check(len(reasoning) == 1, "exactly one reasoning item (deltas + finished item are one item)",
      [(i.get("type"), i.get("id")) for i in items])
if reasoning:
    item = reasoning[0]
    check(item.get("id") == "rs_stub", "the upstream's own item id reaches the client", item.get("id"))
    check(item.get("encrypted_content") == "gAAAA_stub_encrypted_content",
          "encrypted_content reaches the client", sorted(item))
message = [i for i in items if i.get("type") == "message"]
check(bool(message) and message[0].get("id") == "msg_stub", "the message keeps the upstream's id too",
      [i.get("id") for i in items])

# Streaming must survive the fix: the chain of thought still arrives as deltas, under the
# upstream's id, and the item finishes exactly once.
deltas = [d for name, d in stream if name == "response.reasoning_summary_text.delta"]
check(len(deltas) == 2, "the chain of thought is still streamed as deltas", len(deltas))
if deltas:
    check(deltas[0].get("item_id") == "rs_stub", "deltas carry the upstream's item id",
          deltas[0].get("item_id"))
done_ids = [d.get("item", {}).get("id") for name, d in stream if name == "response.output_item.done"]
check(done_ids.count("rs_stub") == 1, "the reasoning item finishes exactly once", done_ids)
raise SystemExit(fail)
PY
[ $? -eq 0 ] || fail=1

# A client that did not ask for the blob must not receive it either: forwarding include is not
# the same as inventing the field.
echo "== 3. a request without include gets no encrypted_content =="
python3 - "$WORK/request.json" "$WORK/request-noinclude.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
body.pop("include")
json.dump(body, open(sys.argv[2], "w"))
PY
: >"$WORK/upstream-bodies.jsonl"
curl -sN -X POST "$ADMIN/v1/responses" -H 'Authorization: Bearer sk-codex-fidelity' \
  -H 'Content-Type: application/json' --data-binary "@$WORK/request-noinclude.json" >"$WORK/response2.sse"
python3 - "$WORK/upstream-bodies.jsonl" "$WORK/response2.sse" <<'PY'
import json, sys

fail = 0
bodies = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
if not bodies or "include" in bodies[-1]:
    print("   [FAIL] include was sent for a client that never asked for it: %r" % (bodies[-1] if bodies else None))
    fail = 1
else:
    print("   ok   include stays absent when the client did not ask")
raise SystemExit(fail)
PY
[ $? -eq 0 ] || fail=1

if [ "$fail" != 0 ]; then
  echo "FAILED (gateway log: $WORK/gateway.log)"
  exit 1
fi
echo "PASS: codex input dialect survives the client -> gateway -> plugin -> upstream chain"
