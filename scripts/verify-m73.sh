#!/usr/bin/env bash
# 验证 M73「控制台智能问答的联网能力」在本机实例上是否真的生效。
#
# 它验证的是**服务端那一半**：部署级开关与会话级开关是否都被网关承认、`web_access` 是否真的
# 落库（读回来一致）、部署没启用时是否明确拒绝（而不是静默存下一个没用的标志）、会话负载里
# 是否带上 UI 需要的部署事实，以及联网开关有没有把搜索密钥漏进接口。全部走真实 HTTP，
# 不碰数据库、不读进程内存。
#
# 它**不**验证模型行为（模型是否真的会去搜、拿到网页后是否引用来源）：那需要真实模型与外部
# 搜索额度，只能人工走查，见 docs/chat.md §12 与 docs/TODO.md 的 M73 小节。
#
# 用法：
#   GW_ADMIN_PASSWORD='你的密码' scripts/verify-m73.sh
#   BASE=http://127.0.0.1:8099 GW_ADMIN_PASSWORD='…' scripts/verify-m73.sh
#   RUN_TURN=1 ... scripts/verify-m73.sh    # 额外跑一轮真实提问（**会计费**，需要联网后端可用）
#   KEEP=1 ... scripts/verify-m73.sh        # 保留现场（排查用）
#
# 副作用（结束时自动清理）：一个会话 + 一个 MCP 令牌。默认不产生任何模型调用，因此不花钱。
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8088}"
USER_NAME="${GW_ADMIN_USER:-admin}"
PASSWORD="${GW_ADMIN_PASSWORD:-}"
RUN_TURN="${RUN_TURN:-0}"
KEEP="${KEEP:-0}"
JAR="$(mktemp -t m73jar.XXXXXX)"
trap 'rm -f "$JAR"' EXIT

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  \033[33mskip\033[0m %s\n' "$*"; }
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

echo "实例：$BASE"
printf '%s\n' "────────────────────────────────────────────────────────────"

# ── 1. 登录 ──────────────────────────────────────────────────────────────────
echo "1) 登录"
if [ -z "$PASSWORD" ]; then
  echo "  需要管理员密码：GW_ADMIN_PASSWORD='…' scripts/verify-m73.sh" >&2
  exit 2
fi
LOGIN_BODY=$(USER_NAME="$USER_NAME" PASSWORD="$PASSWORD" python3 -c \
  'import json,os; print(json.dumps({"username":os.environ["USER_NAME"],"password":os.environ["PASSWORD"]}))')
LOGIN=$(curl -s -c "$JAR" -H 'Content-Type: application/json' -d "$LOGIN_BODY" "$BASE/admin/api/v1/auth/login")
unset LOGIN_BODY
have "登录成功" "$LOGIN" '"username"'
case "$LOGIN" in *'"username"'*) ;; *) exit 1;; esac

# ── 2. 二进制是否是含 M73 的那一版 + 部署是否启用了联网 ──────────────────────
echo
echo "2) 这个部署有没有 M73 的联网能力"
LIST=$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions?limit=1")
AVAILABLE=$(get "$LIST" "web_access_available")
PROVIDER=$(get "$LIST" "web_access_provider")
if printf '%s' "$LIST" | grep -q 'web_access_available'; then
  ok "会话列表带上了部署事实 web_access_available（M73 的新字段）"
else
  bad "会话列表里没有 web_access_available —— 8088 上跑的可能还是旧二进制，先 scripts/local-run.sh restart"
  info "实际：$(printf '%s' "$LIST" | head -c 200)"
  exit 1
fi
if [ "$AVAILABLE" = "True" ]; then
  ok "本部署已启用控制台联网，后端 provider=$PROVIDER"
  case "$PROVIDER" in
    searxng|bocha|tavily|bing) ok "provider 是已知的四种后端之一";;
    *) bad "provider 取值异常：$PROVIDER（应为 searxng / bocha / tavily / bing）";;
  esac
else
  skip '本部署没有启用联网（chat.web_access.enabled=false）：只验证「关掉时明确拒绝」，其余检查跳过'
fi

# ── 3. 找一个真的能路由模型的账户 + Key（只用现有的，不新建） ────────────────
echo
echo "3) 找一个能路由模型的账户与 API Key（不新建、不花钱）"
#
# 「第一个 active Key」是不够的：本机的 admin 账户里，排在前面的 Key 属于没有模型授权的账号
# （/chat/models 返回空数组），照第一个挑会在第 4 步直接放弃。这里逐个试，直到有 Key 能列出
# 模型——这只读地翻一遍 /chat/models，不创建任何东西。
KEYS=$(curl -s -b "$JAR" "$BASE/admin/api/v1/keys?limit=200")
ACCID=""; KEYID=""; MODEL=""
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
if [ -z "$MODEL" ]; then
  bad "没有任何 active Key 能列出可路由的模型；请先给某个账号授权一个模型"
  exit 1
fi
ok "使用账户 #$ACCID 与 Key #$KEYID（模型 $MODEL）"

# ── 4. 临时令牌与会话 ───────────────────────────────────────────────────────
echo
echo "4) 建临时会话（一个 scope=query 的只读令牌 + 一个会话）"
TOKRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"name\":\"m73-verify-$$-$(rand_hex | head -c 8)\",\"account_id\":$ACCID,\"scope\":\"query\"}" \
  "$BASE/admin/api/v1/mcp-tokens")
TOKID=$(get "$TOKRESP" "id")
[ -n "$TOKID" ] && ok "临时令牌 #$TOKID（scope=query，只读）" || { bad "令牌签发失败"; exit 1; }
info "使用模型 $MODEL"
SESSRESP=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"title\":\"M73 验证\",\"model\":\"$MODEL\",\"account_id\":$ACCID,\"api_key_id\":$KEYID,\"mcp_token_id\":$TOKID}" \
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

# ── 5. 新建的会话默认关闭联网 ────────────────────────────────────────────────
echo
echo "5) 新建会话的联网开关默认关闭"
[ "$(get "$SESSRESP" "web_access")" = "False" ] && ok "新建会话 web_access=false" \
  || bad "新建会话的 web_access 不是 false（默认应该是关闭）"

# ── 6. 开关的往返：打开 → 落库 → 关掉 ───────────────────────────────────────
echo
echo "6) 联网开关的往返（这正是控制台角标点击时发的那个请求）"
PATCH_ON=$(curl -s -b "$JAR" -X PATCH -H 'Content-Type: application/json' \
  -d '{"web_access":true}' "$BASE/admin/api/v1/chat/sessions/$SID")
if [ "$AVAILABLE" = "True" ]; then
  [ "$(get "$PATCH_ON" "web_access")" = "True" ] && ok "PATCH web_access=true 被接受" \
    || { bad "PATCH web_access=true 没有被接受（部署已启用联网，本应成功）"; info "服务端回答：$(printf '%s' "$PATCH_ON" | head -c 200)"; }
  # 读回来：会话负载里的值必须来自数据库，而不是回显请求。
  READBACK=$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions/$SID")
  [ "$(get "$READBACK" "web_access")" = "True" ] && ok "重新读取仍是 true（说明真的落库了）" \
    || bad "重新读取不是 true —— 开关没有持久化"
  # 只改标题不该顺手把开关关掉（PATCH 的"未给即不改"语义）。
  curl -s -b "$JAR" -X PATCH -H 'Content-Type: application/json' \
    -d '{"title":"M73 验证 改名"}' "$BASE/admin/api/v1/chat/sessions/$SID" >/dev/null
  AFTER_TITLE=$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions/$SID")
  [ "$(get "$AFTER_TITLE" "web_access")" = "True" ] && ok "只改标题不会关掉联网（未给即不改）" \
    || bad "只改标题把联网开关改掉了"
  PATCH_OFF=$(curl -s -b "$JAR" -X PATCH -H 'Content-Type: application/json' \
    -d '{"web_access":false}' "$BASE/admin/api/v1/chat/sessions/$SID")
  [ "$(get "$PATCH_OFF" "web_access")" = "False" ] && ok "PATCH web_access=false 生效" \
    || bad "无法关闭联网开关"
else
  # 部署没启用：必须明确拒绝，而不是存下一个做不到的承诺。
  HAVE_ERR=$(printf '%s' "$PATCH_ON" | grep -c 'web_access')
  [ "$HAVE_ERR" != "0" ] && ok "部署未启用时，开启请求被拒绝并点名了那个开关" \
    || { bad "部署未启用时，开启请求应当被明确拒绝"; info "服务端回答：$(printf '%s' "$PATCH_ON" | head -c 200)"; }
  [ "$(get "$PATCH_ON" "web_access")" = "False" ] && ok "会话仍然是 web_access=false" \
    || bad "被拒绝的请求却把开关写成了 true"
fi

# ── 7. 别人的会话改不了（归属隔离没有被这次改动破坏） ────────────────────────
echo
echo "7) 归属隔离"
FOREIGN=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X PATCH -H 'Content-Type: application/json' \
  -d '{"web_access":true}' "$BASE/admin/api/v1/chat/sessions/chat_does_not_exist")
[ "$FOREIGN" = "404" ] && ok "不存在的会话返回 404（而不是 500 或 200）" \
  || bad "不存在的会话返回 HTTP $FOREIGN，应为 404"

# ── 8. 响应里不能出现搜索密钥 ────────────────────────────────────────────────
echo
echo "8) 搜索后端的密钥不出现在任何会话响应里"
if [ -n "${GW_CHAT_WEB_API_KEY:-}" ]; then
  lack "会话负载里没有密钥明文" "$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions/$SID")" "$GW_CHAT_WEB_API_KEY"
  lack "会话列表里没有密钥明文" "$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions?limit=5")" "$GW_CHAT_WEB_API_KEY"
else
  skip "未提供 GW_CHAT_WEB_API_KEY（把部署里配置的那个值传进来才能真正验证这一条）"
fi

# ── 9. 可选：真实跑一轮（会计费） ────────────────────────────────────────────
echo
if [ "$RUN_TURN" = "1" ]; then
  echo "9) 真实提一轮（RUN_TURN=1：会产生模型调用费用，并消耗搜索额度）"
  if [ "$AVAILABLE" != "True" ]; then
    skip "本部署没有启用联网，跑这一轮也没有意义"
  else
    curl -s -b "$JAR" -X PATCH -H 'Content-Type: application/json' \
      -d '{"web_access":true}' "$BASE/admin/api/v1/chat/sessions/$SID" >/dev/null
    TURN=$(curl -s -b "$JAR" -H 'Content-Type: application/json' \
      -d "{\"turn_id\":\"m73-$(rand_hex | head -c 12)\",\"content\":\"请联网搜索 deepseek 官方 API 文档里的模型价格页，给出链接和输入价格。\"}" \
      "$BASE/admin/api/v1/chat/sessions/$SID/turns")
    # 用 case 而不是 `printf | grep -q`：本脚本开了 pipefail，grep -q 命中后立刻退出会让 printf
    # 收到 SIGPIPE（退出码 141），于是"找到了"被判成"没找到"——这两个断言第一次跑就是被这个坑
    # 误报成失败的，而当时这一轮其实搜了也抓了。
    used=""
    case "$TURN" in *'"name":"web_search"'*) used="$used web_search";; esac
    case "$TURN" in *'"name":"web_fetch"'*) used="$used web_fetch";; esac
    if [ -n "$used" ]; then
      ok "这一轮真的用了联网工具：$used"
    else
      bad "这一轮没有任何联网工具调用（模型没去搜，或工具没进工具面）"
      info "回答片段：$(printf '%s' "$TURN" | tail -c 300)"
    fi
    case "$TURN" in
      *'https://'*|*'http://'*) ok "回答里出现了链接";;
      *) bad "回答里没有链接 —— 引用规则可能没进提示";;
    esac
    CALLS=$(curl -s -b "$JAR" "$BASE/admin/api/v1/chat/sessions/$SID")
    case "$CALLS" in
      *'"name":"web_search"'*|*'"name":"web_fetch"'*)
        ok "工具调用已记入会话记录（控制台会把它画成工具卡片）";;
      *) bad "会话记录里没有联网工具调用条目";;
    esac
  fi
else
  skip "RUN_TURN=1 才会跑真实提问（会计费并消耗搜索额度）"
fi

echo
printf '%s\n' "────────────────────────────────────────────────────────────"
printf '通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIP"
[ "$FAIL" = "0" ] || exit 1
