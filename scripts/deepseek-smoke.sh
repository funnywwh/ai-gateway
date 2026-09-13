#!/usr/bin/env bash
# M17 acceptance: one DeepSeek provider config, exercised end to end.
#
#   scripts/deepseek-smoke.sh            # offline: a fake DeepSeek upstream checks the
#                                        # request shape, thinking switches, streaming
#                                        # order, usage dimensions and error mapping
#   DEEPSEEK_API_KEY=sk-... scripts/deepseek-smoke.sh --live
#                                        # the same checks against api.deepseek.com
#
# Either run starts a real gateway (bin/aigw) with a generated config in a temp
# directory, so nothing in ./data or ./config.yaml is touched.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LIVE=0
[ "${1:-}" = "--live" ] && LIVE=1

GATEWAY_PORT="${GATEWAY_PORT:-18099}"
UPSTREAM_PORT="${UPSTREAM_PORT:-18098}"
API_KEY="sk-gw-deepseek-smoke-0001"
WORK="$(mktemp -d)"
FAKE_PID=""
GW_PID=""

cleanup() {
  [ -n "$GW_PID" ] && kill "$GW_PID" 2>/dev/null || true
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

BASE_URL="http://127.0.0.1:${UPSTREAM_PORT}/v1"
if [ "$LIVE" = "1" ]; then
  [ -n "${DEEPSEEK_API_KEY:-}" ] || fail "--live needs DEEPSEEK_API_KEY"
  BASE_URL="https://api.deepseek.com/v1"
else
  python3 - "$UPSTREAM_PORT" >"$WORK/upstream.log" 2>&1 <<'PY' &
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])


class Fake(BaseHTTPRequestHandler):
    """A DeepSeek-shaped /chat/completions upstream."""

    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.endswith("/models"):
            self._json(200, {"object": "list", "data": [{"id": "deepseek-flash"}]})
        else:
            self._json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            req = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": {"message": "invalid json"}})

        prompt = json.dumps(req)
        if "boom" in prompt:
            return self._json(400, {"error": {"message": "Invalid Format"}})
        if "poor" in prompt:
            return self._json(402, {"error": {"message": "Insufficient Balance", "code": 402}})
        if "busy" in prompt:
            self.send_response(429)
            self.send_header("Retry-After", "60")
            body = json.dumps({"error": {"message": "Rate Limit Reached"}}).encode()
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        thinking = (req.get("thinking") or {}).get("type", "<absent>")
        reasoning = req.get("reasoning_effort") or "<absent>"
        tools = "tools" in req
        replay = "reasoning_content" in json.dumps(req.get("messages", []))
        assistants = sum(1 for m in req.get("messages", []) if m.get("role") == "assistant")
        keyed = sum(1 for m in req.get("messages", [])
                    if m.get("role") == "assistant" and "reasoning_content" in m)
        print("REQUEST thinking=%s effort=%s tools=%s replay=%s assistant=%d keyed=%d"
              % (thinking, reasoning, tools, replay, assistants, keyed), flush=True)

        # DeepSeek's thinking-mode validator, reproduced: unless thinking is explicitly
        # disabled, EVERY assistant message must carry the reasoning_content key (an
        # empty value is accepted, a missing key is a 400). The fake used to only report
        # the shape, so a request the real upstream rejects still looked green here.
        if thinking != "disabled":
            for m in req.get("messages", []):
                if m.get("role") == "assistant" and "reasoning_content" not in m:
                    return self._json(400, {"error": {"message":
                        "The `reasoning_content` in the thinking mode must be passed "
                        "back to the API."}})

        if not req.get("stream"):
            message = {
                "role": "assistant",
                "content": "4",
                "reasoning_content": "two plus two is four",
            }
            finish = "stop"
            if tools:
                # Thinking + tools is the combination whose reasoning must come back.
                message = {
                    "role": "assistant",
                    "content": "let me check",
                    "reasoning_content": "I need the weather tool for this",
                    "tool_calls": [{
                        "id": "call_smoke_1",
                        "type": "function",
                        "function": {"name": "weather", "arguments": "{\"city\":\"hz\"}"},
                    }],
                }
                finish = "tool_calls"
            self._json(200, {
                "id": "chatcmpl-fake",
                "choices": [{"index": 0, "message": message, "finish_reason": finish}],
                "usage": {
                    "prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
                    "prompt_cache_hit_tokens": 80, "prompt_cache_miss_tokens": 20,
                    "completion_tokens_details": {"reasoning_tokens": 5},
                },
            })
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        def chunk(payload):
            self.wfile.write(("data: " + json.dumps(payload) + "\n\n").encode())
            self.wfile.flush()

        for piece in ["thinking...", " answer"]:
            chunk({"choices": [{"index": 0, "delta": {"reasoning_content": piece}}]})
        for piece in ["four", "!"]:
            chunk({"choices": [{"index": 0, "delta": {"content": piece}}]})
        chunk({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
               "usage": {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19,
                         "prompt_cache_hit_tokens": 4, "prompt_cache_miss_tokens": 8,
                         "completion_tokens_details": {"reasoning_tokens": 3}}})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


ThreadingHTTPServer(("127.0.0.1", PORT), Fake).serve_forever()
PY
  FAKE_PID=$!
fi

cat >"$WORK/config.yaml" <<YAML
server:
  listen: "127.0.0.1:${GATEWAY_PORT}"
  secret_key: "smoke-session-signing-key"
database:
  path: "${WORK}/aigw.db"
plugins:
  dir: "${WORK}/plugins"
  state_dir: "${WORK}/plugin-state"
credentials_key: "0123456789abcdef0123456789abcdef"
log:
  level: warn
billing:
  prepaid_enforce: false
recording:
  record_input: metadata
mcp:
  enabled: false
backup:
  enabled: false
bootstrap:
  mode: upsert
  admin:
    username: admin
    password: "smoke-admin-password"
  accounts:
    - name: smoke
      billing_mode: postpaid
      credit_limit_usd: 100
  api_keys:
    - name: smoke
      key: "${API_KEY}"
      account: smoke
  providers:
    - name: deepseek
      kind: openai-chat
      enabled: true
      priority: 10
      weight: 100
      config:
        base_url: "${BASE_URL}"
        thinking:
          mode: auto
          style: deepseek
          replay_reasoning_content: true
        # NOTE: response_format=json_object makes DeepSeek reject prompts that do
        # not mention json, so the checks below keep the default (text).
        default_max_output_tokens: 8192
        timeout_s: 60
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
YAML

source scripts/goenv.sh
echo "== build"
go build -o "$WORK/aigw" ./cmd/aigw || fail "build failed"

echo "== start gateway (${BASE_URL})"
"$WORK/aigw" --config "$WORK/config.yaml" >"$WORK/gateway.log" 2>&1 &
GW_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:${GATEWAY_PORT}/v1/models" -H "Authorization: Bearer ${API_KEY}" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "http://127.0.0.1:${GATEWAY_PORT}/v1/models" -H "Authorization: Bearer ${API_KEY}" >/dev/null \
  || { cat "$WORK/gateway.log"; fail "gateway did not become ready"; }

ask() { # ask <input> <extra json fields>
  curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"model\":\"deepseek-flash\",\"input\":$1$2}"
}

# The live key travels through the credential channel (sealed at rest), never
# through the config file: the provider config is stored as plain JSON.
if [ "$LIVE" = "1" ]; then
  echo "== store the API key as provider credentials"
  curl -fsS -c "$WORK/cookies" -X POST "http://127.0.0.1:${GATEWAY_PORT}/admin/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"admin\",\"password\":\"smoke-admin-password\"}" >/dev/null
  PROVIDER_ID="$(curl -fsS -b "$WORK/cookies" "http://127.0.0.1:${GATEWAY_PORT}/admin/api/v1/providers" |
    python3 -c 'import json,sys; print([p["id"] for p in json.load(sys.stdin)["data"] if p["name"]=="deepseek"][0])')"
  python3 -c 'import json,sys; print(json.dumps({"credentials":{"api_key":sys.argv[1]}}))' "$DEEPSEEK_API_KEY" >"$WORK/creds.json"
  curl -fsS -b "$WORK/cookies" -X PATCH "http://127.0.0.1:${GATEWAY_PORT}/admin/api/v1/providers/${PROVIDER_ID}" \
    -H "Content-Type: application/json" -d @"$WORK/creds.json" >/dev/null
  rm -f "$WORK/creds.json"
  health="$(curl -fsS -b "$WORK/cookies" -X POST "http://127.0.0.1:${GATEWAY_PORT}/admin/api/v1/providers/${PROVIDER_ID}/test" \
    -H "Content-Type: application/json" -d '{"mode":"health"}')"
  echo "$health" | grep -q '"ok":true' || fail "live health probe failed: $health"
  pass "live health probe ok (credentials accepted)"
fi

echo "== non-streaming, reasoning kept and metered"
if [ "$LIVE" = "1" ]; then
  OUT="$(ask '"Say hi in one word."' ',"reasoning":{"effort":"low"}')" \
    || { echo "--- gateway log ---"; tail -25 "$WORK/gateway.log"; fail "live request failed"; }
  echo "$OUT" | grep -q '"output"' || fail "live response has no output"
  echo "$OUT" | grep -q '"reasoning"' || fail "live response lost its reasoning item"
  pass "live non-streaming response carries a reasoning item"
  echo "$OUT" | grep -q '"input_tokens"' || fail "live response has no usage"
  pass "live usage: $(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("usage"))')"

  echo "== live tool turn, continued with previous_response_id"
  TOOLS='"tools":[{"type":"function","name":"get_weather","description":"Get the weather of a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":"auto"'
  OUT="$(curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
        -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
        -d "{\"model\":\"deepseek-flash\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":\"Weather in Hangzhou? Use the tool.\"}],$TOOLS,\"reasoning\":{\"effort\":\"low\"},\"store\":true}")" \
    || fail "live tool request failed"
  rid="$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  echo "$OUT" | grep -q '"function_call"' || fail "live tool turn produced no function_call: $OUT"
  pass "live tool turn stored as $rid (with its chain of thought)"

  # The upstream rejects this request with 400 when the previous turn's
  # reasoning_content is not replayed verbatim.
  CONT="$(curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"model\":\"deepseek-flash\",\"previous_response_id\":\"${rid}\",\"input\":[{\"type\":\"function_call_output\",\"call_id\":\"$(echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); print([i["call_id"] for i in d["output"] if i["type"]=="function_call"][0])')\",\"output\":\"Cloudy 7~13C\"}],$TOOLS,\"reasoning\":{\"effort\":\"low\"}}")" \
    || fail "live continuation failed (the upstream rejects a continuation that drops reasoning_content)"
  echo "$CONT" | grep -q '"output_text"' || fail "live continuation produced no answer: $CONT"
  pass "live continuation accepted the replayed chain of thought"
else
  OUT="$(ask '"2+2?"' ',"reasoning":{"effort":"low"}')" || fail "non-streaming request failed"
  echo "$OUT" | grep -q '"reasoning"' || fail "reasoning item missing from the response"
  pass "reasoning item present"
  echo "$OUT" | grep -q '"reasoning_summary_text.delta"' && pass "reasoning summary events emitted" || true
  usage="$(echo "$OUT" | python3 -c 'import json,sys; u=json.load(sys.stdin)["usage"]; print(u["input_tokens_details"]["cached_tokens"], u["output_tokens_details"]["reasoning_tokens"])')"
  [ "$usage" = "80 5" ] || fail "usage details = '$usage', want cache=80 reasoning=5"
  pass "usage details: cached=80 reasoning=5"

  echo "== thinking switch reaches the upstream"
  OUT="$(ask '"2+2?"' ',"reasoning":{"effort":"none"}')" || fail "effort=none request failed"
  grep -q "REQUEST thinking=disabled effort=<absent>" "$WORK/upstream.log" \
    || { cat "$WORK/upstream.log"; fail "effort=none must switch thinking off"; }
  pass "effort=none → thinking.type=disabled, no stale effort"
  OUT="$(ask '"2+2?"' ',"reasoning":{"effort":"high"}')" || fail "effort=high request failed"
  grep -q "REQUEST thinking=enabled effort=high" "$WORK/upstream.log" \
    || { cat "$WORK/upstream.log"; fail "effort=high must switch thinking on"; }
  pass "effort=high → thinking.type=enabled + reasoning_effort=high"
  # Silence is not "off": the upstream's own default decides (DeepSeek defaults to
  # thinking on). Sending type=disabled here stripped the chain of thought from every
  # client that does not speak the reasoning field — DSH pointed at the gateway sends
  # none — and the downgraded model stopped tasks half-finished.
  ask '"2+2?"' '' >/dev/null || fail "silent request failed"
  grep -q "REQUEST thinking=<absent> effort=<absent>" "$WORK/upstream.log" \
    || { cat "$WORK/upstream.log"; fail "a silent client must not get an explicit thinking switch"; }
  pass "no reasoning field → no thinking field (upstream default decides)"

  echo "== tool turn replays the chain of thought on continuation"
  TOOLS='"tools":[{"type":"function","name":"weather","description":"d","parameters":{"type":"object"}}],"tool_choice":"auto"'
  OUT="$(curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
        -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
        -d "{\"model\":\"deepseek-flash\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":\"weather in hz?\"}],$TOOLS,\"reasoning\":{\"effort\":\"low\"},\"store\":true}")" \
    || fail "tool request failed"
  rid="$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  pass "first tool turn stored as $rid"
  # The continuation answers the tool call, which is the only real shape: an
  # unanswered tool_call is pruned (the upstream rejects a call with no tool
  # message behind it), and with the call gone there is no chain of thought to
  # replay. Sending a bare user message here tested a request no client sends.
  curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"model\":\"deepseek-flash\",\"previous_response_id\":\"${rid}\",\"input\":[{\"type\":\"function_call_output\",\"call_id\":\"call_smoke_1\",\"output\":\"Cloudy 7~13C\"}],$TOOLS,\"reasoning\":{\"effort\":\"low\"}}" \
    >/dev/null || fail "continuation request failed"
  grep -q "replay=True" "$WORK/upstream.log" \
    || { cat "$WORK/upstream.log"; fail "the continuation must replay reasoning_content"; }
  pass "continuation replays reasoning_content (upstream saw it)"
  # A client that never sends reasoning items (DSH pointed at this gateway does not)
  # must still get its tool turn through: the upstream demands the reasoning_content
  # KEY on assistant messages that carry tool_calls, and takes an empty value.
  curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"model\":\"deepseek-flash\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":\"weather in hz?\"},{\"type\":\"function_call\",\"call_id\":\"call_smoke_2\",\"name\":\"weather\",\"arguments\":\"{}\"},{\"type\":\"function_call_output\",\"call_id\":\"call_smoke_2\",\"output\":\"cloudy\"}],$TOOLS}" \
    >/dev/null || fail "a tool turn without client reasoning items must still be accepted"
  pass "tool turn without reasoning items carries the empty reasoning_content key"
  # A turn that announces itself in text before calling a tool is ONE assistant turn for
  # the upstream: sent as two chat messages, the thinking-mode validator rejects it even
  # with the key on the tool-calling message (the text that announces the call has none).
  # DSH produces exactly this shape whenever the model writes a sentence before acting.
  curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"model\":\"deepseek-flash\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":\"weather in hz?\"},{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"let me check:\"},{\"type\":\"function_call\",\"call_id\":\"call_smoke_3\",\"name\":\"weather\",\"arguments\":\"{}\"},{\"type\":\"function_call_output\",\"call_id\":\"call_smoke_3\",\"output\":\"cloudy\"}],$TOOLS}" \
    >/dev/null || fail "a text-then-call turn must still be accepted"
  grep -q "replay=True assistant=1" "$WORK/upstream.log" \
    || { cat "$WORK/upstream.log"; fail "the announcing text must be folded into one assistant message"; }
  pass "text before a tool call folds into one assistant turn (assistant=1, key present)"
  # The assistant narrating what a tool returned belongs to the same turn, and the fold
  # cannot reach it (the tool result sits between them): with the earlier tool-call-only
  # rule that message travelled WITHOUT the key and DeepSeek rejected the whole request.
  # Two assistant messages, both keyed.
  curl -fsS -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d "{\"model\":\"deepseek-flash\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":\"read it\"},{\"type\":\"function_call\",\"call_id\":\"call_smoke_4\",\"name\":\"weather\",\"arguments\":\"{}\"},{\"type\":\"function_call_output\",\"call_id\":\"call_smoke_4\",\"output\":\"42\"},{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"the file has 42 lines\"}],$TOOLS}" \
    >/dev/null || fail "a narrative after a tool result must still be accepted"
  grep -q "replay=True assistant=2 keyed=2" "$WORK/upstream.log" \
    || { cat "$WORK/upstream.log"; fail "every assistant message of a tool conversation must carry the key"; }
  pass "assistant text after a tool result carries the key too (assistant=2 keyed=2)"

  echo "== error mapping"
  code="$(curl -s -o "$WORK/err.json" -w '%{http_code}' -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d '{"model":"deepseek-flash","input":"boom"}')"
  [ "$code" = "500" ] || fail "a fatal upstream 400 must surface as 500, got $code"
  grep -q 'upstream_400' "$WORK/err.json" || fail "upstream code lost: $(cat "$WORK/err.json")"
  grep -q 'Invalid Format' "$WORK/err.json" || fail "upstream message lost: $(cat "$WORK/err.json")"
  pass "upstream 400 → fatal 500, code and message preserved"
  code="$(curl -s -o "$WORK/err402.json" -w '%{http_code}' -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
    -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
    -d '{"model":"deepseek-flash","input":"poor"}')"
  grep -q 'Insufficient Balance' "$WORK/err402.json" || fail "402 message lost: $(cat "$WORK/err402.json")"
  pass "upstream 402 → quota mapping (HTTP $code, message preserved)"
fi

echo "== streaming (reasoning before text)"
STREAM="$(curl -fsS -N -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/responses" \
  -H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-flash","input":"2+2?","stream":true,"reasoning":{"effort":"low"}}')" \
  || fail "streaming request failed"
REASON_LINE="$(echo "$STREAM" | grep -n 'reasoning_summary_text.delta' | head -1 | cut -d: -f1)"
TEXT_LINE="$(echo "$STREAM" | grep -n 'output_text.delta' | head -1 | cut -d: -f1)"
[ -n "$REASON_LINE" ] || fail "no reasoning delta in the stream"
[ -n "$TEXT_LINE" ] || fail "no text delta in the stream"
[ "$REASON_LINE" -lt "$TEXT_LINE" ] \
  || fail "reasoning must precede text (reasoning at line $REASON_LINE, text at $TEXT_LINE)"
pass "stream order: reasoning (line $REASON_LINE) before text (line $TEXT_LINE)"
echo "$STREAM" | grep -q 'response.completed' || fail "stream did not complete"
pass "stream completed with usage: $(echo "$STREAM" | grep -o '"output_tokens":[0-9]*' | tail -1)"

echo
echo "all checks passed"
