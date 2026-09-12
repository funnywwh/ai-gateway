#!/usr/bin/env bash
# 验证 M34「可交互 HTML5 界面」在本机实例上是否真的生效。
#
# 它验证的是**服务端那一半**：票据 scope、桥接脚本注入、CSP nonce、沙箱边界、登出即失效、
# 以及"同一个代码块重复预览不会拿到坏 URL"。这些都不需要模型，因此可以在你自己的部署上
# 反复运行，且全部走真实 HTTP（不碰数据库、不读进程内存）。
#
# 模型那一半（模型是否真的会输出表单）只能人工走查，见 docs/chat.md §4 与 docs/TODO.md 的 M34 小节。
#
# 用法：
#   GW_ADMIN_USER=admin GW_ADMIN_PASSWORD='你的密码' scripts/verify-m34.sh
#   BASE=http://127.0.0.1:8088 scripts/verify-m34.sh          # 换地址
#   KEEP=1 ... scripts/verify-m34.sh                          # 保留现场（排查用）
#
# 副作用（结束时自动清理）：一个 MCP 令牌 + 一个会话（含它的预览产物）。
# 不新建账户、不新建 API Key、不产生任何模型调用，因此不花钱。
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8088}"
USER_NAME="${GW_ADMIN_USER:-admin}"
PASSWORD="${GW_ADMIN_PASSWORD:-}"
KEEP="${KEEP:-0}"
JAR="$(mktemp -t m34jar.XXXXXX)"
trap 'rm -f "$JAR"' EXIT

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
info() { printf '       %s\n' "$*"; }
have() { case "$2" in *"$3"*) ok "$1";; *) bad "$1"; info "实际：$(printf '%s' "$2" | head -c 200)";; esac; }
lack() { case "$2" in *"$3"*) bad "$1";; *) ok "$1";; esac; }

command -v curl >/dev/null || { echo "需要 curl" >&2; exit 2; }
command -v python3 >/dev/null || { echo "需要 python3" >&2; exit 2; }

# get <json> <path> —— 从一段 JSON 里取一个值，取不到就输出空串。
# 第一版把 python 代码喂进了 stdin（`-c` 的代码在 argv 里，stdin 应该是管道来的 JSON），
# 于是每个字段都取成空字符串：会话 id 是空的，后面每一条请求都打在 /sessions//artifacts 上，
# 报了一堆"功能坏了"的 404。脚本本身有 bug 时最容易得出的结论就是"功能有 bug"。
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

echo "实例：$BASE"
printf '%s\n' "────────────────────────────────────────────────────────────"

# ── 0. 二进制是否是含 M34 的那一版 ────────────────────────────────────────────
echo "0) 新二进制是否在跑"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/admin/ui/js/pages/chat_ui.js")
[ "$CODE" = "200" ] && ok "控制台资源 chat_ui.js 存在（HTTP 200，M34 的新模块）" \
  || bad "chat_ui.js 返回 HTTP $CODE —— 8088 上跑的可能还是旧二进制，先 scripts/local-run.sh restart"

# ── 1. 登录 ──────────────────────────────────────────────────────────────────
echo
echo "1) 登录"
if [ -z "$PASSWORD" ]; then
  echo "  需要管理员密码：GW_ADMIN_PASSWORD='…' scripts/verify-m34.sh" >&2
  exit 2
fi
# 密码经环境变量传给 python，不写进命令行（命令行会出现在同机其他用户的 ps 里）。
LOGIN_BODY=$(USER_NAME="$USER_NAME" PASSWORD="$PASSWORD" python3 -c \
  'import json,os; print(json.dumps({"username":os.environ["USER_NAME"],"password":os.environ["PASSWORD"]}))')
LOGIN=$(curl -s -c "$JAR" -H 'Content-Type: application/json' -d "$LOGIN_BODY" "$BASE/admin/api/v1/auth/login")
unset LOGIN_BODY
have "登录成功" "$LOGIN" '"username"'
case "$LOGIN" in *'"username"'*) ;; *) exit 1;; esac

# ── 2. 找一个可计费的账户 + Key（只用现有的，不新建） ─────────────────────────
echo
echo "2) 找一个账户与它的 API Key（不新建、不花钱）"
KEYS=$(curl -s -b "$JAR" "$BASE/admin/api/v1/keys?limit=200")
ACCID=$(printf '%s' "$KEYS" | python3 -c '
import json,sys
rows = json.load(sys.stdin).get("data", [])
for row in rows:
    if row.get("status") == "active" and row.get("account_id"):
        print(row["account_id"]); break
')
KEYID=$(printf '%s' "$KEYS" | python3 -c '
import json,sys
rows = json.load(sys.stdin).get("data", [])
for row in rows:
    if row.get("status") == "active" and row.get("account_id"):
        print(row["id"]); break
')
if [ -z "$ACCID" ] || [ -z "$KEYID" ]; then
  bad "没有可用的 active Key；请先在控制台建一个账户和 Key"
  exit 1
fi
ok "使用账户 #$ACCID 与 Key #$KEYID"

# ── 3. 临时 MCP 令牌与会话 ───────────────────────────────────────────────────
echo
echo "3) 建临时会话（一个 scope=query 的令牌 + 一个会话）"
TOKRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"name\":\"m34-verify-$$\",\"account_id\":$ACCID,\"scope\":\"query\"}" \
  "$BASE/admin/api/v1/mcp-tokens")
TOKID=$(get "$TOKRESP" "id")
[ -n "$TOKID" ] && ok "临时令牌 #$TOKID（scope=query，只读）" || { bad "令牌签发失败"; exit 1; }

# 会话必须绑定一个「该 Key 真的能路由」的模型，所以先问服务端要候选列表（与手动新建会话
# 时下拉框里的那份数据同源），再拿第一个去建会话。
MODEL=$(get "$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/models?account_id=$ACCID&api_key_id=$KEYID")" "data.0.id")
if [ -z "$MODEL" ]; then
  bad "该 Key 没有可路由的模型；请先给它授权一个模型或换一个 Key"
  exit 1
fi
info "使用模型 $MODEL"
SESSRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"title\":\"M34 验证\",\"model\":\"$MODEL\",\"account_id\":$ACCID,\"api_key_id\":$KEYID,\"mcp_token_id\":$TOKID}" \
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

# ── 4. 登记一份"模型写的表单" ────────────────────────────────────────────────
echo
echo '4) 登记一份带表单的页面（模拟模型输出的 html 代码块）'
FORM='<!doctype html><html><head><title>账户信息</title></head><body><h2>请填写账户信息</h2><form><label>账户名 <input name="name" value="demo"></label><button type="submit">提交</button></form></body></html>'
# 页面正文经环境变量交给 python：直接拼进命令行会被 shell 与 JSON 两层转义搞坏，
# 而 JSON 生成交给 python 是最不容易出错的一步。
BODY=$(FORM="$FORM" python3 -c 'import json,os; print(json.dumps({"key":"verify:0","format":"html","title":"验证用表单","body":os.environ["FORM"]}))')
RO=$(curl -s -b "$JAR" -H 'Content-Type: application/json' -d "$BODY" "$BASE/admin/api/v1/chat/sessions/$SID/artifacts")
ROURL=$(get "$RO" "url")
ROTK=$(get "$RO" "ticket")
ROID=$(get "$RO" "id")
have "只读票据已签发" "$RO" '"bridge":false'

# 到这里为止只用了"今天就有"的能力。如果连一步都做不到，后面每一条都会跟着红，
# 而真正的原因只有一个：这个部署没有开交互预览。先说清楚，别让人对着 30 条红字猜。
if [ -z "$ROURL" ] || [ -z "$ROTK" ]; then
  echo
  bad "连只读预览都登记不了，后面的检查没有意义，先停下"
  info "服务端回答：$(printf '%s' "$RO" | head -c 200)"
  info "最常见的原因：该部署的 chat.ui_bridge_enabled=false（0) 的 chat_ui.js 能读到，说明二进制是新的，"
  info "但注册预览会被直接拒绝）。检查 config.yaml 的 chat 段，改成 true 后 scripts/local-run.sh restart。"
  exit 1
fi

# ── 5. 只读预览：与今天的行为逐字一致 ────────────────────────────────────────
echo
echo "5) 只读预览（未显式申请交互时，页面里不应该有任何通道）"
H=$(mktemp); B=$(curl -s -D "$H" "$BASE$ROURL?ticket=$ROTK")
lack "没有 X-Aigw-Bridge 头" "$(cat "$H")" 'X-Aigw-Bridge'
lack "没有注入桥接脚本" "$B" 'aigw-ui-bridge'
lack "CSP 里没有 nonce" "$(cat "$H")" 'nonce-'
have "模型原文逐字返回" "$B" '请填写账户信息'
have "沙箱仍是 allow-scripts" "$(cat "$H")" 'sandbox allow-scripts'
have "仍禁止联网" "$(cat "$H")" "connect-src 'none'"

echo
echo "6) 只读票据 + ?bridge=1：不能自己升权"
B2=$(curl -s "$BASE$ROURL?ticket=$ROTK&bridge=1")
lack "加参数没有换来桥接脚本" "$B2" 'aigw-ui-bridge'

# ── 7. 交互预览：注入 + nonce + 头 ───────────────────────────────────────────
echo
echo "7) 交互预览（控制台点「预览（可交互）」时走的就是这条）"
BRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' -d '{"bridge":true}' \
  "$BASE/admin/api/v1/chat/sessions/$SID/artifacts/$ROID/ticket")
BT=$(get "$BRESP" "ticket")
if [ -z "$BT" ]; then
  # 交互票据签不出来，只有一个原因：这个部署关了交互预览。后面每一条都会跟着红，
  # 所以停在这里把原因说清楚，而不是让人对着十几行 404 猜。
  bad "服务端拒绝了交互票据（后面的检查没有意义，先停下）"
  info "服务端回答：$(printf '%s' "$BRESP" | head -c 200)"
  info "检查 config.yaml 的 chat 段：ui_bridge_enabled 必须是 true，改完 scripts/local-run.sh restart"
  exit 1
fi
H3=$(mktemp); B3=$(curl -s -D "$H3" "$BASE$ROURL?ticket=$BT&bridge=1")
have "响应头 X-Aigw-Bridge: 1" "$(cat "$H3")" 'X-Aigw-Bridge: 1'
have "注入了 script#aigw-ui-bridge" "$B3" 'id="aigw-ui-bridge"'
have "脚本带每响应随机的 bridgeToken" "$B3" 'data-token='
TOKEN=$(printf '%s' "$B3" | python3 -c 'import re,sys; m=re.search(r"data-token=\"([^\"]+)\"",sys.stdin.read()); print(m.group(1) if m else "")')
NONCE=$(printf '%s' "$B3" | python3 -c 'import re,sys; m=re.search(r"nonce=\"([^\"]+)\"",sys.stdin.read()); print(m.group(1) if m else "")')
have "CSP 授权了这个 nonce" "$(cat "$H3")" "'nonce-$NONCE'"
have "保留了页面自己的内联脚本能力" "$(cat "$H3")" "'unsafe-inline'"
have "沙箱没有被放松（仍无 allow-forms/allow-same-origin）" "$(cat "$H3")" 'sandbox allow-scripts'
lack "没有 allow-forms" "$(cat "$H3")" 'allow-forms'
lack "没有 allow-same-origin" "$(cat "$H3")" 'allow-same-origin'
have "模型原文仍在" "$B3" '请填写账户信息'

echo
echo "8) 每个响应各有各的 token 与 nonce（重放同一个不成立）"
B4=$(curl -s "$BASE$ROURL?ticket=$BT&bridge=1")
T4=$(printf '%s' "$B4" | python3 -c 'import re,sys; m=re.search(r"data-token=\"([^\"]+)\"",sys.stdin.read()); print(m.group(1) if m else "")')
N4=$(printf '%s' "$B4" | python3 -c 'import re,sys; m=re.search(r"nonce=\"([^\"]+)\"",sys.stdin.read()); print(m.group(1) if m else "")')
[ "$TOKEN" != "$T4" ] && ok "两次的 token 不同" || bad "两次的 token 相同"
[ "$NONCE" != "$N4" ] && ok "两次的 nonce 不同" || bad "两次的 nonce 相同"

# ── 9. 重复预览同一代码块 ───────────────────────────────────────────────────
echo
echo "9) 同一个代码块重复预览（这里曾经会发出一个 404 的 URL）"
RO2=$(curl -s -b "$JAR" -H 'Content-Type: application/json' -d "$BODY" "$BASE/admin/api/v1/chat/sessions/$SID/artifacts")
ROID2=$(get "$RO2" "id")
[ "$ROID" = "$ROID2" ] && ok "复用同一行 id（$ROID）" || bad "同一 key 产生了两个 id：$ROID / $ROID2"
C=$(curl -s -o /dev/null -w '%{http_code}' "$BASE$ROURL?ticket=$ROTK")
[ "$C" = "200" ] && ok "第一次拿到的预览 URL 仍然可用" || bad "第一次的 URL 失效：HTTP $C"

# ── 10. 不可交互的情形 ──────────────────────────────────────────────────────
echo
echo "10) 不该有交互通道的情形"
SVG=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d '{"key":"verify:svg","format":"svg","bridge":true,"body":"<svg xmlns=\"http://www.w3.org/2000/svg\"/>"}' \
  "$BASE/admin/api/v1/chat/sessions/$SID/artifacts")
have "SVG 拒绝交互票据" "$SVG" 'only an html artifact can be interactive'

# ── 11. 登出即失效 ──────────────────────────────────────────────────────────
echo
echo "11) 登出后票据立即失效（收回权限的手段之一）"
C1=$(curl -s -o /dev/null -w '%{http_code}' "$BASE$ROURL?ticket=$BT&bridge=1")
[ "$C1" = "200" ] && ok "登出前：票据可读（票据本身就是授权）" || bad "登出前就读不到：HTTP $C1"
curl -s -b "$JAR" -H 'Content-Type: application/json' -d '{}' "$BASE/admin/api/v1/auth/logout" >/dev/null
C2=$(curl -s -o /dev/null -w '%{http_code}' "$BASE$ROURL?ticket=$BT&bridge=1")
[ "$C2" = "404" ] && ok "登出后：同一票据 404" || bad "登出后票据仍可用：HTTP $C2"

printf '%s\n' "────────────────────────────────────────────────────────────"
printf '服务端验证：\033[32m%d 通过\033[0m / \033[31m%d 失败\033[0m\n' "$PASS" "$FAIL"
cat <<'NEXT'

接下来是只能人工做的那一半（模型是否真的会输出表单）：

  1. 硬刷新 http://127.0.0.1:8088/admin/ui/#/chat
  2. 新建会话（选一个 admin scope 的 MCP 令牌，才有工具面）
  3. 提一个需要它先问你的需求，例如：
       帮我建一个账户，先问我要账户名和月限额，再创建
  4. 回答里应出现一个 html 代码块（表单）→ 点「预览（可交互）」
  5. 工具栏状态应从「等待页面握手…」变成「已连接」
  6. 在页面里填写 → 点提交按钮 → 工具栏计数 +1，状态变「模型正在处理…」
  7. 会话里出现一条新提问：开头是你填的字段说明，接着是一段 source=ui_event 的 JSON
  8. 「请求日志」页按该会话 id 过滤，应看到这条 client=console 的请求，正文为空

哪一步不对，照 docs/chat.md §9 的排障表对号入座。
NEXT
[ "$FAIL" = "0" ]
