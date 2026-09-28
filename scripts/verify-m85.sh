#!/usr/bin/env bash
# 验证 M85「智能问答把整段会话历史都发给模型」在本机实例上是否真的生效。
#
# 它验证的是**行为**，不是配置：第 1 轮在会话开头埋一个 4 位数的「针」，然后灌 N 轮填充提问，
# 最后问「第一条消息里我让你记住的数字是多少」。
#   - 旧默认（历史窗口 40 条消息 / 256 KiB）在 N=21 之后早就把第 1 轮丢掉了，模型答不出来；
#   - 新默认（0 = 不限制）整段回放，针还在请求里，模型答得出。
# 同时断言：整段流里**没有**「更早的 N 轮对话没有随本次请求发送」提示；会话里确实有 ≥ 44 条消息
# （即超过旧窗口，不然这次验证不成立）；最后一轮的 input_tokens 不小于前面各轮 output 之和（旁证）。
#
# 用法：
#   RUN_TURNS=1 GW_ADMIN_PASSWORD='管理员密码' scripts/verify-m85.sh
#   BASE=http://127.0.0.1:8088/aigw RUN_TURNS=1 GW_ADMIN_PASSWORD='…' scripts/verify-m85.sh   # gw-b 这类带 base_path 的实例
#   RUN_TURNS=1 N=5 … scripts/verify-m85.sh     # 只灌 5 轮（快速冒烟；注意 5 轮证不了「超过旧窗口」）
#   KEEP=1 … scripts/verify-m85.sh              # 保留现场（排查用）
#
# 副作用：一个 scope=query 的临时 MCP 令牌 + 一个会话 + **N+2 次真实计费的模型请求**（默认 23 次，
# 都很短）。不跑 RUN_TURNS=1 时只做登录与版本核对，不产生任何模型调用，因此不花钱。
# 结束时自动删除会话与令牌。
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8088}"
USER_NAME="${GW_ADMIN_USER:-admin}"
PASSWORD="${GW_ADMIN_PASSWORD:-}"
RUN_TURNS="${RUN_TURNS:-0}"
FILL="${N:-21}"
KEEP="${KEEP:-0}"
JAR="$(mktemp -t m85jar.XXXXXX)"
trap 'rm -f "$JAR"' EXIT

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  \033[33mskip\033[0m %s\n' "$*"; }
info() { printf '       %s\n' "$*"; }

command -v curl >/dev/null || { echo "需要 curl" >&2; exit 2; }
command -v python3 >/dev/null || { echo "需要 python3" >&2; exit 2; }

# get <json> <path> —— 从一段 JSON 里取一个值（支持 data.0.id 这类路径），取不到就输出空串。
get() {
  python3 -c '
import json, sys
raw, path = sys.argv[1], sys.argv[2]
try:
    value = json.loads(raw)
except Exception:
    print(""); raise SystemExit
for key in path.split("."):
    if not key:
        continue
    if isinstance(value, list):
        value = value[int(key)] if len(value) > int(key) else None
    elif isinstance(value, dict):
        value = value.get(key)
    else:
        value = None
    if value is None:
        break
print("" if value is None else value)
' "$1" "$2" 2>/dev/null
}

# turn_body <turn_id> <content> —— 生成轮次请求体（内容由 python 转义，中文与引号都安全）。
turn_body() {
  TURN_ID="$1" CONTENT="$2" python3 -c \
    'import json,os; print(json.dumps({"turn_id":os.environ["TURN_ID"],"content":os.environ["CONTENT"]}))'
}

# sse_fields —— 从 stdin 的 SSE 里取出「输入 token / 输出 token / 提示 / 正文」，制表符分隔。
# 事件形状见 internal/httpapi/chat.go 的 chatEventJSON（键是 snake_case，值可能带换行，这里压平）。
sse_fields() {
  python3 -c '
import json, sys
text, notices, inp, out = [], [], 0, 0
for frame in sys.stdin.read().split("\n\n"):
    lines = [l for l in frame.strip().split("\n") if l]
    if not lines or not lines[0].startswith("event: "):
        continue
    name = lines[0][7:]
    data = "\n".join(l[6:] for l in lines[1:] if l.startswith("data: "))
    if not data:
        continue
    try:
        ev = json.loads(data)
    except Exception:
        continue
    if name == "text":
        text.append(ev.get("delta") or "")
    elif name == "notice":
        notices.append(ev.get("notice") or "")
    elif name == "usage":
        usage = ev.get("usage") or {}
        inp += usage.get("input_tokens") or 0
        out += usage.get("output_tokens") or 0
flat = lambda s: " ".join(s.split())
print("%d\t%d\t%s\t%s" % (inp, out, flat(" | ".join(notices)), flat("".join(text))))
'
}

rand_hex() { python3 -c 'import secrets; print(secrets.token_hex(16))'; }

echo "实例：$BASE"
printf '%s\n' "────────────────────────────────────────────────────────────"

# ── 1. 登录 ──────────────────────────────────────────────────────────────────
echo "1) 登录"
if [ -z "$PASSWORD" ]; then
  echo "  需要管理员密码：GW_ADMIN_PASSWORD='…' scripts/verify-m85.sh" >&2
  exit 2
fi
LOGIN_BODY=$(USER_NAME="$USER_NAME" PASSWORD="$PASSWORD" python3 -c \
  'import json,os; print(json.dumps({"username":os.environ["USER_NAME"],"password":os.environ["PASSWORD"]}))')
LOGIN=$(curl -s -c "$JAR" -H 'Content-Type: application/json' -d "$LOGIN_BODY" "$BASE/admin/api/v1/auth/login")
unset LOGIN_BODY
case "$LOGIN" in
  *'"username"'*) ok "登录成功";;
  *) bad "登录失败"; info "实际：$(printf '%s' "$LOGIN" | head -c 200)"; exit 1;;
esac

# ── 2. 这台跑的是含 M85 的版本吗 ─────────────────────────────────────────────
echo
echo "2) 版本核对（整段回放是 4.4.0 起的行为）"
VERSION_JSON=$(curl -s -m 10 "$BASE/version")
VER=$(get "$VERSION_JSON" "version")
REV=$(get "$VERSION_JSON" "revision")
if [ -z "$VER" ]; then
  bad "拿不到 $BASE/version"
  info "实际：$(printf '%s' "$VERSION_JSON" | head -c 200)"
  exit 1
fi
info "线上：$VER / $REV"
if python3 -c 'import sys; parts=sys.argv[1].split(".")[:2]; raise SystemExit(0 if tuple(int(p) for p in parts) >= (4,4) else 1)' "$VER"; then
  ok "版本 ≥ 4.4.0：整段回放应当生效"
else
  bad "版本 $VER 早于 4.4.0：这台跑的还是没有 M85 的二进制（先发版/换二进制再验）"
  exit 1
fi

# ── 3. 找一个能路由模型的账户 + Key（只用现有的，不新建） ────────────────────
echo
echo "3) 找一个能路由模型的账户与 API Key（不新建）"
# ACCOUNT_ID / KEY_ID / MODEL 可以用环境变量指定：生产实例上「第一个能用的 Key」往往是客户的，
# 那样会把这次验证记到客户头上（虽然只有几分钱）。指定了就只核对它能不能路由模型。
ACCID="${ACCOUNT_ID:-}"; KEYID="${KEY_ID:-}"; MODEL="${MODEL:-}"
if [ -z "$ACCID" ] || [ -z "$KEYID" ]; then
  KEYS=$(curl -s -b "$JAR" "$BASE/admin/api/v1/keys?limit=200")
  while read -r acc key; do
    [ -n "$acc" ] || continue
    candidate=$(get "$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/models?account_id=$acc&api_key_id=$key")" "data.0.id")
    if [ -n "$candidate" ]; then
      ACCID="$acc"; KEYID="$key"; MODEL="$candidate"
      break
    fi
  done < <(printf '%s' "$KEYS" | python3 -c '
import json,sys
for row in json.load(sys.stdin).get("data", []):
    if row.get("status") == "active" and row.get("account_id"):
        print(row["account_id"], row["id"])
')
fi
if [ -z "$ACCID" ] || [ -z "$KEYID" ]; then
  bad "没有任何 active Key 能列出可路由的模型；请先给某个账号授权一个模型，或用 ACCOUNT_ID/KEY_ID 指定"
  exit 1
fi
# 针测试要跑 20+ 轮，挑一个便宜的名字（deepseek-flash / *mini* / *luna* 之类）；没有就用第一个。
if [ -z "$MODEL" ]; then
  CHEAP=$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/models?account_id=$ACCID&api_key_id=$KEYID" | python3 -c '
import json,sys
ids=[m.get("id","") for m in json.load(sys.stdin).get("data", [])]
wanted=("flash","mini","luna","terra","cheap")
for name in ids:
    if any(w in name for w in wanted):
        print(name); raise SystemExit
print(ids[0] if ids else "")
')
  MODEL="$CHEAP"
fi
if [ -z "$MODEL" ]; then
  bad "账户 #$ACCID / Key #$KEYID 列不出任何可路由的模型"
  exit 1
fi
ok "使用账户 #$ACCID 与 Key #$KEYID（模型 $MODEL）"

# ── 4. 临时令牌与会话 ───────────────────────────────────────────────────────
echo
echo "4) 建临时会话（一个 scope=query 的只读令牌 + 一个会话）"
TOKRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"name\":\"m85-verify-$$-$(rand_hex | head -c 8)\",\"account_id\":$ACCID,\"scope\":\"query\"}" \
  "$BASE/admin/api/v1/mcp-tokens")
TOKID=$(get "$TOKRESP" "id")
[ -n "$TOKID" ] && ok "临时令牌 #$TOKID（scope=query，只读）" || { bad "令牌签发失败"; exit 1; }
SESSRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"title\":\"M85 针测试\",\"model\":\"$MODEL\",\"account_id\":$ACCID,\"api_key_id\":$KEYID,\"mcp_token_id\":$TOKID}" \
  "$BASE/admin/api/v1/chat/sessions")
SID=$(get "$SESSRESP" "id")
[ -n "$SID" ] && ok "临时会话 $SID" || { bad "会话创建失败"; info "$(printf '%s' "$SESSRESP" | head -c 200)"; exit 1; }

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo; echo "KEEP=1：保留现场 —— 会话 $SID、令牌 #$TOKID"
    return
  fi
  curl -s -b "$JAR" -X DELETE "$BASE/admin/api/v1/chat/sessions/$SID" >/dev/null
  curl -s -b "$JAR" -X DELETE "$BASE/admin/api/v1/mcp-tokens/$TOKID" >/dev/null
  echo; echo "已清理：会话 $SID 与令牌 #$TOKID"
}
trap 'cleanup; rm -f "$JAR"' EXIT

if [ "$RUN_TURNS" != "1" ]; then
  echo
  skip "没有 RUN_TURNS=1：只做了登录与版本核对，未发起任何模型请求（也就没花钱）"
  printf '%s\n' "────────────────────────────────────────────────────────────"
  printf '通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIP"
  [ "$FAIL" = "0" ] || exit 1
  exit 0
fi

NEEDLE=$(python3 -c 'import secrets; print(secrets.randbelow(9000) + 1000)')
echo
echo "5) 针测试：第 1 轮埋数字 $NEEDLE，再灌 $FILL 轮填充，最后问它"
TOTAL_IN=0; TOTAL_OUT=0; DROPPED_NOTE=0; PREV_OUT=0
turns=0

# run_turn <content> —— 发一轮，回填 IN_TOK / OUT_TOK / NOTICES / TEXT（制表符分隔取字段）。
run_turn() {
  local body reply fields
  body=$(turn_body "m85-$(rand_hex | head -c 12)" "$1")
  reply=$(curl -s -N -m 180 -b "$JAR" -H 'Content-Type: application/json' -d "$body" \
    "$BASE/admin/api/v1/chat/sessions/$SID/turns")
  fields=$(printf '%s' "$reply" | sse_fields)
  IN_TOK=$(printf '%s' "$fields" | cut -f1)
  OUT_TOK=$(printf '%s' "$fields" | cut -f2)
  NOTICES=$(printf '%s' "$fields" | cut -f3)
  TEXT=$(printf '%s' "$fields" | cut -f4)
  case "$reply" in
    *'"type":"error"'*)
      bad "这一轮的流里有错误事件"; info "$(printf '%s' "$reply" | tail -c 300)";;
  esac
}

# 第 1 轮：针
run_turn "请记住数字 $NEEDLE，之后我会问你。现在只回一句「收到」。"
turns=$((turns+1)); TOTAL_IN=$((TOTAL_IN+IN_TOK)); TOTAL_OUT=$((TOTAL_OUT+OUT_TOK)); PREV_OUT=$((PREV_OUT+OUT_TOK))
case "$NOTICES" in *更早的*) DROPPED_NOTE=1;; esac
if [ -n "$TEXT" ]; then ok "第 1 轮完成（输入 $IN_TOK / 输出 $OUT_TOK token）"; else skip "第 1 轮没有正文（$NOTICES）"; fi

# 填充轮：内容各不相同，避免上游把同一份请求当缓存命中而看不出输入增长
i=1
while [ "$i" -le "$FILL" ]; do
  run_turn "填充第 $i 轮：只回「好」。"
  turns=$((turns+1)); TOTAL_IN=$((TOTAL_IN+IN_TOK)); TOTAL_OUT=$((TOTAL_OUT+OUT_TOK)); PREV_OUT=$((PREV_OUT+OUT_TOK))
  case "$NOTICES" in *更早的*) DROPPED_NOTE=1;; esac
  [ -z "$TEXT" ] && info "第 $((i+1)) 轮没有正文（$NOTICES）"
  i=$((i+1))
done
FILLER_OUT=$PREV_OUT
ok "完成 $turns 轮（累计输入 $TOTAL_IN / 输出 $TOTAL_OUT token）"

DETAIL=$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions/$SID")
COUNT=$(printf '%s' "$DETAIL" | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("messages") or []))')
if [ -n "$COUNT" ] && [ "$COUNT" -gt 40 ]; then
  ok "会话里已存 $COUNT 条消息 —— 超过旧默认窗口（40 条），这次验证成立"
else
  skip "会话里只有 $COUNT 条消息（未超过旧窗口 40 条）：本次 N=$FILL 证不了「不再丢历史」，要 N≥21"
fi

run_turn "我第一条消息里让你记住的数字是多少？只回数字。"
FINAL_IN=$IN_TOK
case "$TEXT" in
  *"$NEEDLE"*) ok "模型答出了第 1 条消息里的数字 $NEEDLE —— 整段历史确实在请求里";;
  *) bad "回答里没有 $NEEDLE：会话开头的消息没有发给模型，或模型没照做"; info "回答片段：$(printf '%s' "$TEXT" | tail -c 200)";;
esac
case "$NOTICES" in *更早的*) DROPPED_NOTE=1;; esac
if [ "$DROPPED_NOTE" = "0" ]; then
  ok "全程没有出现「更早的 N 轮对话没有随本次请求发送」"
else
  bad "出现了「更早的…」提示：这台部署的历史窗口不是 0（改 config 里的 chat.max_history_* 再验）"
fi
if [ "$FINAL_IN" -ge "$FILLER_OUT" ]; then
  ok "最后一轮输入 $FINAL_IN token ≥ 前面各轮输出合计 $FILLER_OUT（旁证：前面产出都被回放了）"
else
  info "最后一轮输入 $FINAL_IN < 前面输出合计 $FILLER_OUT —— 只作旁证，不作为失败（分词与缓存会让两者不等）"
fi

echo
printf '%s\n' "────────────────────────────────────────────────────────────"
printf '通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIP"
[ "$FAIL" = "0" ] || exit 1
