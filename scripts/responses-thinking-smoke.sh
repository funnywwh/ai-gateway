#!/usr/bin/env bash
# Acceptance: does a DeepSeek-shaped /responses upstream's chain of thought reach a streaming client?
#
# Why a script and not just a unit test: the provider test pins which upstream frames turn into
# reasoning events, while this runs the real binary end to end and reads the SSE a client
# (DSH, Codex) actually receives. The defect it guards against was silent — the non-streaming
# body carried the reasoning item, so only streaming clients lost the thinking, and the
# provider's own unit test stayed green because it never named the dialect DeepSeek uses
# (response.reasoning_text.delta).
#
#   scripts/responses-thinking-smoke.sh
#
# No network, no API key, no model cost: the upstream is a local stub that speaks DeepSeek's
# /responses stream, reasoning deltas first.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

GW_PORT="${GW_PORT:-18201}"
UP_PORT="${UP_PORT:-18202}"
WORK="$(mktemp -d)"
fail=0

cleanup() { kill "${GW:-0}" "${UP:-0}" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

[ -x ./bin/aigw ] || { echo "build first: make build" >&2; exit 2; }

# The fake upstream mirrors the event order DeepSeek uses on /responses: the reasoning item
# opens first, its text streams as response.reasoning_text.delta (with item_id + output_index),
# and only then does the answer's message item start. It also records every request body so the
# assertions below can read what the gateway sent upstream.
cat >"$WORK/upstream.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = sys.argv[1]
PORT = int(sys.argv[2])

REASONING = ["先看", "天气"]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _frame(self, name, payload):
        body = json.dumps(payload).encode()
        self.wfile.write(b"event: " + name.encode() + b"\n")
        self.wfile.write(b"data: " + body + b"\n\n")
        self.wfile.flush()

    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "deepseek-flash"}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        with open(LOG, "a") as handle:
            handle.write(raw.decode("utf-8", "replace") + "\n")
        request = json.loads(raw or b"{}")

        if not request.get("stream"):
            # Same content, non-streaming: one reasoning item then the message item.
            body = json.dumps({
                "id": "resp_stub_1", "object": "response", "status": "completed",
                "model": request.get("model", "deepseek-flash"),
                "output": [
                    {"type": "reasoning", "id": "rs_upstream", "status": "completed",
                     "content": [{"type": "reasoning_text", "text": "".join(REASONING)}]},
                    {"type": "message", "id": "msg_upstream", "role": "assistant",
                     "status": "completed",
                     "content": [{"type": "output_text", "text": "4"}]},
                ],
                "usage": {"input_tokens": 5, "output_tokens": 7, "total_tokens": 12},
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        self._frame("response.created", {"type": "response.created", "response": {
            "id": "resp_stub_1", "object": "response", "status": "in_progress",
            "model": request.get("model", "deepseek-flash"), "output": []}})
        self._frame("response.output_item.added", {
            "type": "response.output_item.added", "output_index": 0,
            "item": {"type": "reasoning", "id": "rs_upstream", "status": "in_progress"}})
        for i, chunk in enumerate(REASONING):
            self._frame("response.reasoning_text.delta", {
                "type": "response.reasoning_text.delta", "item_id": "rs_upstream",
                "output_index": 0, "content_index": 0, "delta": chunk})
        self._frame("response.reasoning_text.done", {
            "type": "response.reasoning_text.done", "item_id": "rs_upstream",
            "output_index": 0, "content_index": 0, "text": "".join(REASONING)})
        self._frame("response.output_item.done", {
            "type": "response.output_item.done", "output_index": 0,
            "item": {"type": "reasoning", "id": "rs_upstream", "status": "completed",
                     "content": [{"type": "reasoning_text", "text": "".join(REASONING)}]}})
        self._frame("response.output_item.added", {
            "type": "response.output_item.added", "output_index": 1,
            "item": {"type": "message", "id": "msg_upstream", "role": "assistant",
                     "status": "in_progress"}})
        self._frame("response.output_text.delta", {
            "type": "response.output_text.delta", "item_id": "msg_upstream",
            "output_index": 1, "content_index": 0, "delta": "4"})
        self._frame("response.output_text.done", {
            "type": "response.output_text.done", "item_id": "msg_upstream",
            "output_index": 1, "content_index": 0, "text": "4"})
        self._frame("response.output_item.done", {
            "type": "response.output_item.done", "output_index": 1,
            "item": {"type": "message", "id": "msg_upstream", "role": "assistant",
                     "status": "completed",
                     "content": [{"type": "output_text", "text": "4"}]}})
        self._frame("response.completed", {"type": "response.completed", "response": {
            "id": "resp_stub_1", "object": "response", "status": "completed",
            "model": request.get("model", "deepseek-flash"),
            "output": [], "usage": {"input_tokens": 5, "output_tokens": 7, "total_tokens": 12,
                                    "output_tokens_details": {"reasoning_tokens": 4}}}})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PY
python3 "$WORK/upstream.py" "$WORK/upstream-bodies.jsonl" "$UP_PORT" &
UP=$!
sleep 1

cat >"$WORK/config.yaml" <<YAML
server:
  listen: "127.0.0.1:$GW_PORT"
  secret_key: "0000000000000000000000000000000000000000000000000000000000000003"
database:
  path: "$WORK/aigw.db"
credentials_key: "0000000000000000000000000000000000000000000000000000000000000004"
plugins:
  dir: "$WORK/plugins"
  state_dir: "$WORK/state"
bootstrap:
  mode: upsert
  admin:
    username: admin
    password: "responses-thinking-smoke-only"
  accounts:
    - name: verify
      billing_mode: postpaid
      credit_limit_usd: 1000
  api_keys:
    - name: vk
      key: "sk-responses-thinking-smoke"
      account: verify
  providers:
    - name: deepseek
      kind: openai-responses
      enabled: true
      config:
        base_url: "http://127.0.0.1:$UP_PORT"
        timeout_s: 30
      models:
        - public: deepseek-flash
          upstream: deepseek-flash
          context_window: 1000000
          max_output_tokens: 65536
          capabilities: {stream: true, tools: true, reasoning: true}
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

AUTH='Authorization: Bearer sk-responses-thinking-smoke'
URL="http://127.0.0.1:$GW_PORT/v1/responses"

echo "== A. streaming request (the shape DSH and Codex use) =="
code="$(curl -sN -o "$WORK/stream.sse" -w '%{http_code}' -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-flash","input":"2+2?","stream":true}' "$URL")"
echo "   HTTP $code"
[ "$code" = "200" ] || { echo "   [FAIL] want 200"; fail=1; }

echo "== B. non-streaming request (regression: the body already carried the item) =="
code="$(curl -s -o "$WORK/body.json" -w '%{http_code}' -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-flash","input":"2+2?","stream":false}' "$URL")"
echo "   HTTP $code"
[ "$code" = "200" ] || { echo "   [FAIL] want 200"; fail=1; }

echo
echo "== what the streaming client saw =="
python3 - "$WORK/stream.sse" "$WORK/body.json" <<'PY' || fail=1
import json, sys

frames = []
for block in open(sys.argv[1], encoding="utf-8").read().split("\n\n"):
    name = data = None
    for line in block.splitlines():
        if line.startswith("event: "):
            name = line[len("event: "):]
        elif line.startswith("data: "):
            data = line[len("data: "):]
    if name and data and data != "[DONE]":
        frames.append((name, json.loads(data)))

for name, payload in frames:
    print("  %-45s %s" % (name, json.dumps(payload)[:110]))

problems = []
reasoning = [p for n, p in frames if n == "response.reasoning_summary_text.delta"]
if not reasoning:
    problems.append("the client received no reasoning delta: DeepSeek's "
                    "response.reasoning_text.delta did not reach the stream")
else:
    text = "".join(p.get("delta", "") for p in reasoning)
    if text != "先看天气":
        problems.append("reasoning text = %r, want the whole chain of thought"
                        % (text,))
    # The gateway synthesizes the reasoning item when the first delta arrives, so the client
    # must see it added before any of it: pi-ai drops a delta whose output_index has no slot.
    order = [i for i, (n, p) in enumerate(frames)
             if n == "response.output_item.added" and (p.get("item") or {}).get("type") == "reasoning"]
    if not order:
        problems.append("no reasoning output_item.added reached the client")
    elif order[0] > frames.index(("response.reasoning_summary_text.delta", reasoning[0])):
        problems.append("the reasoning item was added after its own deltas")
    # Identity: the item the client sees is the upstream's own (M47), not a gateway-made id.
    ids = {p.get("item_id") for p in reasoning} | {
        (p.get("item") or {}).get("id") for n, p in frames
        if n == "response.output_item.added" and (p.get("item") or {}).get("type") == "reasoning"}
    if ids != {"rs_upstream"}:
        problems.append("reasoning item ids = %s, want the upstream's rs_upstream" % (sorted(ids),))

texts = [p for n, p in frames if n == "response.output_text.delta"]
if "".join(p.get("delta", "") for p in texts) != "4":
    problems.append("the answer text did not survive alongside the thinking")

if not any(n == "response.completed" for n, _ in frames):
    problems.append("the stream did not end as response.completed")

# Ordering across the whole stream: thinking first, answer second (that is how a client renders
# the two blocks, and how the upstream sent them).
first_reasoning = next((i for i, (n, _) in enumerate(frames)
                        if n == "response.reasoning_summary_text.delta"), None)
first_text = next((i for i, (n, _) in enumerate(frames) if n == "response.output_text.delta"), None)
if first_reasoning is not None and first_text is not None and first_text < first_reasoning:
    problems.append("the answer started before the chain of thought")

body = json.load(open(sys.argv[2], encoding="utf-8"))
items = body.get("output") or []
kinds = [item.get("type") for item in items]
if kinds != ["reasoning", "message"]:
    problems.append("non-streaming output = %s, want [reasoning, message]" % (kinds,))
else:
    parts = (items[0].get("content") or [{}])[0].get("text", "")
    if "先看" not in parts:
        problems.append("non-streaming reasoning lost its text: %r" % (parts,))

for message in problems:
    print("  [FAIL] " + message)
sys.exit(1 if problems else 0)
PY

if [ "$fail" = "0" ]; then
  echo
  echo "ok: DeepSeek's response.reasoning_text.delta reaches a streaming client as reasoning"
  echo "    events (item added first, upstream id kept, thinking before the answer)"
else
  echo
  echo "responses thinking smoke FAILED"
  echo "--- gateway log ---"; tail -20 "$WORK/gateway.log"
fi
exit $fail
