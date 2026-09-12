#!/usr/bin/env bash
# 验证 M34「可交互 HTML5 界面」在本机实例上是否真的生效。
#
# 它验证的是**服务端那一半**：票据 scope、桥接脚本注入、通道的**结构性**鉴权（不带任何凭证）、
# CSP 保留 'unsafe-inline' 且不加 nonce、沙箱边界、登出即失效，以及"同一个代码块重复预览不会
# 拿到坏 URL"。这些都不需要模型，因此可以在你自己的部署上反复运行，且全部走真实 HTTP
# （不碰数据库、不读进程内存）。
#
# 模型那一半（模型是否真的会输出表单）只能人工走查，见 docs/chat.md §4 与 docs/TODO.md 的 M34 小节。
#
# 用法：
#   GW_ADMIN_PASSWORD='你的密码' scripts/verify-m34.sh
#   BASE=http://127.0.0.1:8099 GW_ADMIN_PASSWORD='…' scripts/verify-m34.sh
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
have() { case "$2" in *"$3"*) ok "$1";; *) bad "$1"; info "实际：$(printf '%s' "$2" | head -c 220)";; esac; }
lack() { case "$2" in *"$3"*) bad "$1";; *) ok "$1";; esac; }

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

rand_hex() { python3 -c 'import secrets; print(secrets.token_hex(16))'; }
grab() { python3 -c 'import re,sys; m=re.search(sys.argv[1], sys.stdin.read()); print(m.group(1) if m else "")' "$1"; }

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
# 密码经环境变量交给 python，不写进命令行（命令行会出现在同机其他用户的 ps 里）。
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
for row in json.load(sys.stdin).get("data", []):
    if row.get("status") == "active" and row.get("account_id"):
        print(row["account_id"]); break
')
KEYID=$(printf '%s' "$KEYS" | python3 -c '
import json,sys
for row in json.load(sys.stdin).get("data", []):
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
echo "3) 建临时会话（一个 scope=query 的只读令牌 + 一个会话）"
TOKRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"name\":\"m34-verify-$$-$(rand_hex | head -c 8)\",\"account_id\":$ACCID,\"scope\":\"query\"}" \
  "$BASE/admin/api/v1/mcp-tokens")
TOKID=$(get "$TOKRESP" "id")
[ -n "$TOKID" ] && ok "临时令牌 #$TOKID（scope=query，只读）" || { bad "令牌签发失败"; exit 1; }

# 会话必须绑定一个「该 Key 真的能路由」的模型，所以先问服务端要候选列表（与手动新建会话
# 时下拉框里的那份数据同源）。
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

# ── 4. 登记两份页面：一份交互、一份只读 ──────────────────────────────────────
echo
echo '4) 登记页面（模拟模型输出的 html 代码块）'
FORM='<!doctype html><html><head><title>账户信息</title></head><body><h2>请填写账户信息</h2><form><label>账户名 <input name="name" value="demo"></label><button type="submit">提交</button></form></body></html>'

# 交互预览不带任何凭证：交出的 MessagePort 只能被加载该文档的 window 收到，注入脚本只在
# 自己是顶层文档时启动，宿主只接受来自那个 frame 的 hello——三条合起来等价于凭证。
BODY=$(FORM="$FORM" python3 -c 'import json,os; print(json.dumps(
  {"key":"verify:0","format":"html","title":"验证用表单","bridge":True,"body":os.environ["FORM"]}))')
RO=$(curl -s -b "$JAR" -H 'Content-Type: application/json' -d "$BODY" "$BASE/admin/api/v1/chat/sessions/$SID/artifacts")
ROURL=$(get "$RO" "url"); ROTK=$(get "$RO" "ticket"); ROID=$(get "$RO" "id")


# 同一份正文再登记一份只读副本（不同 key），用来验证"没申请交互就没有通道"。
RO_ONLY_BODY=$(FORM="$FORM" python3 -c 'import json,os; print(json.dumps(
  {"key":"verify:readonly","format":"html","title":"只读副本","body":os.environ["FORM"]}))')
RO_ONLY=$(curl -s -b "$JAR" -H 'Content-Type: application/json' -d "$RO_ONLY_BODY" \
  "$BASE/admin/api/v1/chat/sessions/$SID/artifacts")
ROURL_ONLY=$(get "$RO_ONLY" "url"); ROTK_ONLY=$(get "$RO_ONLY" "ticket")
# 断言用解析后的字段，不用字符串匹配：服务端的 JSON 是紧凑格式（{"bridge":false}），
# 而 python 生成的是带空格的（{"bridge": false}），按空格去比会得到假失败。
[ "$(get "$RO" "bridge")" = "True" ] && ok "交互产物已登记（bridge=true）" || bad "交互登记的 bridge 不是 true"
[ "$(get "$RO_ONLY" "bridge")" = "False" ] && ok "只读产物已登记（bridge=false）" || bad "只读登记的 bridge 不是 false"

if [ -z "$ROURL" ] || [ -z "$ROTK" ] || [ -z "$ROURL_ONLY" ]; then
  echo
  bad "连预览都登记不了，后面的检查没有意义，先停下"
  info "服务端回答：$(printf '%s' "$RO" | head -c 200)"
  exit 1
fi

# ── 5. 只读预览：不该有任何通道 ──────────────────────────────────────────────
echo
echo "5) 只读预览（未显式申请交互时，页面里不应该有任何通道）"
H=$(mktemp); B=$(curl -s -D "$H" "$BASE$ROURL_ONLY?ticket=$ROTK_ONLY")
lack "没有 X-Aigw-Bridge 头" "$(cat "$H")" 'X-Aigw-Bridge'
lack "没有注入桥接脚本" "$B" 'aigw-ui-bridge'
lack "CSP 里没有 nonce" "$(cat "$H")" 'nonce-'
have "模型原文逐字返回" "$B" '请填写账户信息'
have "沙箱仍是 allow-scripts" "$(cat "$H")" 'sandbox allow-scripts'
have "仍禁止联网" "$(cat "$H")" "connect-src 'none'"

echo
echo "6) 只读票据 + ?bridge=1：不能自己升权"
B2=$(curl -s "$BASE$ROURL_ONLY?ticket=$ROTK_ONLY&bridge=1")
lack "加参数没有换来桥接脚本" "$B2" 'aigw-ui-bridge'

# ── 7. 交互预览 ─────────────────────────────────────────────────────────────
echo
echo "7) 交互预览（控制台点「预览（可交互）」时走的就是这条）"
if [ -z "$ROTK" ]; then
  bad "服务端没有签发交互票据（后面的检查没有意义，先停下）"
  info "服务端回答：$(printf '%s' "$RO" | head -c 200)"
  info "检查 config.yaml 的 chat 段：ui_bridge_enabled 必须是 true，改完 scripts/local-run.sh restart"
  exit 1
fi
# 页面 URL 必须带上凭证：注入脚本从**自己的 location** 读它，那是唯一别的文档看不到的地方。
PREVIEW_URL="$BASE$ROURL?ticket=$ROTK&bridge=1"
H3=$(mktemp); B3=$(curl -s -D "$H3" "$PREVIEW_URL")
have "响应头 X-Aigw-Bridge: 1" "$(cat "$H3")" 'X-Aigw-Bridge: 1'
have "注入了 script#aigw-ui-bridge" "$B3" 'id="aigw-ui-bridge"'
lack "注入标签不带任何凭证（没有 data-token）" "$B3" 'data-token'
have "注入脚本只在自己是顶层文档时启动" "$B3" 'window.top'
have "CSP 保留 'unsafe-inline'（模型页面自己的内联脚本必须还能跑）" "$(cat "$H3")" "'unsafe-inline'"
lack "CSP 里没有 nonce（有它会忽略 'unsafe-inline'，连带拦掉页面自己的脚本）" "$(cat "$H3")" 'nonce-'
# 模型页面自己的内联脚本与事件处理器必须原样留在响应里
have "模型页面的内联脚本仍在" "$B3" '<form' 
have "沙箱没有被放松（仍无 allow-forms/allow-same-origin）" "$(cat "$H3")" 'sandbox allow-scripts'
lack "没有 allow-forms" "$(cat "$H3")" 'allow-forms'
lack "没有 allow-same-origin" "$(cat "$H3")" 'allow-same-origin'
have "模型原文仍在" "$B3" '请填写账户信息'

echo
echo "8) 通道是结构性鉴权：响应里没有秘密，也不需要每响应的例外"
B4=$(curl -s "$PREVIEW_URL")
[ "$B3" = "$B4" ] && ok "两次响应的正文逐字相同（没有每响应的随机秘密）" \
  || bad "两次响应不一致——有东西在每次响应里变化"
lack "正文里没有 aigw_token/bridge_token" "$B4" 'aigw_token'
# 沙箱与策略的边界：这几条是"没被放松"的证据，逐条摆出来而不是笼统说"安全"。
have "沙箱仍是 allow-scripts" "$(cat "$H3")" 'sandbox allow-scripts'
lack "没有 allow-forms（表单提交由脚本拦截，不是导航）" "$(cat "$H3")" 'allow-forms'
lack "没有 allow-same-origin（页面读不到控制台的登录态）" "$(cat "$H3")" 'allow-same-origin'
lack "没有 allow-popups" "$(cat "$H3")" 'allow-popups'

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
C1=$(curl -s -o /dev/null -w '%{http_code}' "$PREVIEW_URL")
[ "$C1" = "200" ] && ok "登出前：票据可读（票据本身就是授权）" || bad "登出前就读不到：HTTP $C1"
curl -s -b "$JAR" -H 'Content-Type: application/json' -d '{}' "$BASE/admin/api/v1/auth/logout" >/dev/null
C2=$(curl -s -o /dev/null -w '%{http_code}' "$PREVIEW_URL")
[ "$C2" = "404" ] && ok "登出后：同一票据 404" || bad "登出后票据仍可用：HTTP $C2"

printf '%s\n' "────────────────────────────────────────────────────────────"
if [ "$FAIL" = "0" ]; then
  printf '服务端验证：\033[32m%d 通过\033[0m / %d 失败\n' "$PASS" "$FAIL"
else
  printf '服务端验证：%d 通过 / \033[31m%d 失败\033[0m\n' "$PASS" "$FAIL"
fi
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

哪一步不对，照 docs/chat.md §10 的排障表对号入座。
NEXT
[ "$FAIL" = "0" ]
