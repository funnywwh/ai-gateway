#!/usr/bin/env bash
# End-to-end load test against a freshly started gateway.
#
# The environment has no hey/wrk/ab, so the generator is cmd/loadgen: standard
# library only, and it reports rps plus latency percentiles.
#
# Usage: scripts/load.sh [concurrency] [duration] [stream]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source scripts/goenv.sh

CONCURRENCY="${1:-32}"
DURATION="${2:-10s}"
STREAM="${3:-false}"
PORT="${PORT:-18099}"
WORK="${WORK:-.cache/load}"

mkdir -p "$WORK"
cat > "$WORK/config.yaml" <<YAML
server:
  listen: ":$PORT"
database:
  path: "./$WORK/aigw.db"
plugins:
  dir: "./$WORK/plugins"
  state_dir: "./$WORK/plugin-state"
auth:
  default_grant: all
bootstrap:
  mode: off
  admin:
    username: admin
    password: "load-admin-password"
credentials_key: "load-credentials-key"
billing:
  default_markup_bp: 10000
  writer_batch_size: 256
  writer_flush_interval_ms: 20
log:
  level: warn
YAML

rm -f "$WORK/aigw.db" "$WORK/cookies.txt"
# Both binaries are built into $WORK, never over bin/: this script's gateway carries the
# console as source (it is a hand build, so `main.uiAssets` keeps its "source" default),
# and writing that over the deployable bin/aigw was a silent way to put readable console
# assets on :8088. See docs/design/m54-console-asset-shape.md.
go build -o "$WORK/aigw" ./cmd/aigw
go build -o "$WORK/loadgen" ./cmd/loadgen

"$WORK/aigw" --config "$WORK/config.yaml" > "$WORK/server.log" 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT

for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" > /dev/null 2>&1; then break; fi
  sleep 0.25
done

api() { curl -sS -b "$WORK/cookies.txt" "$@"; }
api -c "$WORK/cookies.txt" -o /dev/null -X POST "http://127.0.0.1:$PORT/admin/api/v1/auth/login" \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"load-admin-password"}'
api -o /dev/null -X POST "http://127.0.0.1:$PORT/admin/api/v1/accounts" \
  -H 'Content-Type: application/json' -d '{"name":"load","billing_mode":"prepaid"}'
PROVIDER_ID=$(api -X POST "http://127.0.0.1:$PORT/admin/api/v1/providers" \
  -H 'Content-Type: application/json' -d '{"name":"echo","kind":"testecho"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
api -o /dev/null -X POST "http://127.0.0.1:$PORT/admin/api/v1/providers/$PROVIDER_ID/models" \
  -H 'Content-Type: application/json' \
  -d '{"public_model":"echo","upstream_model":"echo","capabilities":{"stream":true},"pricing_rules":{"rules":[{"id":"cost","order":10,"when":{},"rates":{"input":100000,"output":2000000}}]}}'
api -o /dev/null -X POST "http://127.0.0.1:$PORT/admin/api/v1/models" \
  -H 'Content-Type: application/json' -d '{"public_name":"echo","sale_pricing":{"basis":"cost_follow","markup_bp":15000}}'
api -o /dev/null -X POST "http://127.0.0.1:$PORT/admin/api/v1/routes" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"echo\",\"provider_id\":$PROVIDER_ID,\"upstream_model\":\"echo\"}"
KEY=$(api -X POST "http://127.0.0.1:$PORT/admin/api/v1/keys" \
  -H 'Content-Type: application/json' -d '{"name":"load","account":"load"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])')

# Fund the account so requests are admitted.
python3 - "$WORK/aigw.db" <<'PY'
import sqlite3, sys, time
conn = sqlite3.connect(sys.argv[1])
conn.execute("INSERT INTO ledger_entries(account_id, kind, amount_micros, balance_after_micros, idem_key, created_at) VALUES(1,'topup',1000000000,1000000000,'load:topup',?)", (int(time.time()),))
conn.execute('UPDATE accounts SET balance_micros = 1000000000 WHERE id = 1')
conn.commit()
PY

echo "--- load: concurrency=$CONCURRENCY duration=$DURATION stream=$STREAM"
"$WORK/loadgen" -url "http://127.0.0.1:$PORT/v1/responses" -key "$KEY" -model echo \
  -concurrency "$CONCURRENCY" -duration "$DURATION" -stream "$STREAM" -input "ping"

echo '--- billing writer'
api "http://127.0.0.1:$PORT/admin/api/v1/billing/status"
echo
echo '--- invariants after the run'
api "http://127.0.0.1:$PORT/admin/api/v1/billing/invariants" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("ok=", d["ok"], "balance_mismatch=", len(d["balance_sum_mismatch"]), "charge_mismatch=", len(d["charge_mismatch"]), "negative=", len(d["negative_balance"]))'
echo '--- usage rows'
python3 - "$WORK/aigw.db" <<'PY'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
rows = conn.execute('SELECT COUNT(*), COALESCE(SUM(charge_micros),0), COALESCE(SUM(cost_micros),0) FROM usage_records').fetchone()
print('usage rows=%d charge=%d cost=%d' % rows)
print('ledger charge entries=%d' % conn.execute("SELECT COUNT(*) FROM ledger_entries WHERE kind='charge'").fetchone()[0])
PY
