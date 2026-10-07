#!/usr/bin/env bash
# 给运行中的 ai-gateway 实例的 autodl-api 供应商补齐一批模型：供应商模型行（含成本价）、
# 对客模型目录项、模型路由 —— 三层齐全才可路由（缺 provider_models 只在 router/explain 里
# 以 not_mapped 现身，不报错）。
#
#   scripts/autodl-models.sh                       # 干跑：只打印将要写入的三层与自检
#   scripts/autodl-models.sh --apply               # 写入 + 读回核对 + 路由解释 + 计价试算
#   GW_BASE=http://127.0.0.1:8088 scripts/autodl-models.sh --apply
#
# 环境变量：GW_BASE（默认 http://127.0.0.1:8088）、GW_ADMIN_USER / GW_ADMIN_PASS
#          （默认取自 $CONFIG 的 bootstrap.admin）、CONFIG（默认 仓库根/config.yaml，
#           脚本在部署根下时自动改用同目录或上一级的 config.yaml）、
#          PROVIDER_NAME（默认 autodl-api）。
#
# ── 为什么单独一个脚本（而不是改 add-provider-models.py / official-pricing.sh）──
#   那两个脚本的价格真值来自 official-pricing.sh 的**官方价表**（OpenAI / DeepSeek 官方页），
#   并在取不到价时整批中止。而这里的两个模型是 AutoDL 转售的第三方模型（智谱 GLM、
#   腾讯混元），**没有可引用的官方页价目表**——价格的真值只有 AutoDL 自己公布的那张表
#   （控制台模型详情页 / GET /v1/models 的 member_discount）。硬塞进官方价表会让那个脚本
#   的白名单语义（「没有官方价 → 保持原样」）失去意义，所以价与模型表都留在本文件一处。
#
# ── 价格来源（2026-10-07 抓取，用户截图 + 实测核对）─────────────────────────
#   AutoDL 模型详情页（用户提供截图）给出「原价 / 会员价」两列：
#     GLM-5.3-flash   输入 ￥0.560 / 缓存命中 ￥0.161 / 输出 ￥1.960（会员 7 折；
#                     原价 ￥0.800 / ￥0.230 / ￥2.800，页面标注「会员7折」）
#     hy4-preview     输入 ￥6.000 / 缓存命中 ￥0.300 / 输出 ￥18.000（无折扣）
#   折扣率不是转述：GET /v1/models 每个模型带 member_discount，实测
#     GLM-5.3-flash 0.7、hy4-preview 1、DeepSeek-V4.1-Flash 0.5
#   与截图逐项吻合（0.800×0.7=0.560、0.230×0.7=0.161、2.800×0.7=1.960）。
#   ⚠ 取折扣率只能读**列表**端点：GET /v1/models/{id} 的 member_discount 恒为 0
#     （实测 GLM 与 hy4 都回 0，而同一模型的列表项回 0.7 / 1）—— 拿详情端点当真是错的。
#   ⚠ 折扣是**账号状态**（会员到期就没了），所以本脚本把折后价当**常量**写死，
#     只把折扣率当**写入前的一次自检**：上游改折扣会当场红，而不是默默按旧价计费。
#
# ── 币种与汇率 ───────────────────────────────────────────────────────────────
#   与 scripts/autodl-pricing.sh 同口径：规则集币种 CNY，账本币种保持 USD（M22：账本永远
#   单一币种），靠 billing.fx_rates.CNY 入账。汇率不是本脚本的发明，它必须已经在实例上，
#   且必须是 autodl-pricing.sh 确认过的那个值（150000 = 1 元 = 0.150000 美元）——不一致
#   就报错停下：汇率改的是实收金额，静默按别的汇率计费是最坏的结果。
#
# ── 单位与算法 ───────────────────────────────────────────────────────────────
#   费率单位是「微单位 / 百万 token」（pricing.RateScale = 1e6）：￥0.560/百万 = 560000。
#   折后价一律由「原价 × 折扣分子 ÷ 折扣分母」**用整数算出**（禁止手抄折后价、禁止浮点），
#   除不尽即报错退出 —— 与 autodl-pricing.sh 的五折同一套纪律。
#
# ── 一处刻意的留白：hy4-preview 的能力申报为空 ───────────────────────────────
#   实测（2026-10-07）该模型*任何*请求都回 402 `401008 The free trial quota for the service
#   has been exhausted and postpaid billing is not enabled`——chat/completions、/responses、
#   /messages 三条路都是，连 `thinking:{type:"bogus"}` 这种必被参数校验拒绝的请求也还是 402，
#   说明**参数校验不可达**，能力一个都测不出来。网关把空 capabilities 读作「未知 → 不拦任何
#   请求」，`GET /v1/models` 于是只披露 text —— 这是「没测过」唯一诚实的表示。等上游开通后
#   付费、能真发出一次请求，再按实测把 capabilities / max_output_tokens / context_window 补上。
#   context_window 是可公布的事实（页面 1024K），所以照写；max_output_tokens 不写（0 = 未公布）。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
GW_BASE="${GW_BASE:-http://127.0.0.1:8088}"
PROVIDER_NAME="${PROVIDER_NAME:-autodl-api}"
APPLY=0
[ "${1:-}" = "--apply" ] && APPLY=1

CONFIG="${CONFIG:-$ROOT/config.yaml}"
# 部署形态（脚本与 config.yaml 同目录，如 /media/.../aigw/scripts/autodl-models.sh）里上面那个
# 默认值会算到别的目录：找不到就改用「与脚本同目录或上一级的 config.yaml」。
if [ ! -f "$CONFIG" ] && [ -f "$SCRIPT_DIR/config.yaml" ]; then
  CONFIG="$SCRIPT_DIR/config.yaml"
fi
if [ ! -f "$CONFIG" ] && [ -f "$SCRIPT_DIR/../config.yaml" ]; then
  CONFIG="$SCRIPT_DIR/../config.yaml"
fi
if [ -z "${GW_ADMIN_USER:-}" ] && [ -f "$CONFIG" ]; then
  GW_ADMIN_USER="$(awk '/^  admin:/{f=1} f&&/username:/{print $2; exit}' "$CONFIG" 2>/dev/null | tr -d '"' || true)"
fi
if [ -z "${GW_ADMIN_PASS:-}" ] && [ -f "$CONFIG" ]; then
  GW_ADMIN_PASS="$(awk '/^  admin:/{f=1} f&&/password:/{print $2; exit}' "$CONFIG" 2>/dev/null | tr -d '"' || true)"
fi
GW_ADMIN_USER="${GW_ADMIN_USER:-}"
GW_ADMIN_PASS="${GW_ADMIN_PASS:-}"
[ -n "$GW_ADMIN_USER" ] && [ -n "$GW_ADMIN_PASS" ] || { echo "缺少管理员凭据（GW_ADMIN_USER/GW_ADMIN_PASS 或 $CONFIG 的 bootstrap.admin）" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
COOKIE="$WORK/cookie.txt"
PLAN="$WORK/plan.json"
IMPORT_PLAN="$WORK/import.json"

# ── 模型表：模型 id / 上下文 / 能力 / 原价（元/百万 token） / 折扣 / 折扣来源 ──────
# 原价与折扣都写成整数（微元与分子/分母），折后价在 python 里用整数算出。
# capabilities 留空 = 未实测（见文件头「hy4-preview 的能力申报为空」那段）。
python3 - "$PLAN" <<'PY'
import json, sys

PLAN_OUT = sys.argv[1]

# 原价（微元 / 百万 token；1e6 微元 = ￥1）：GLM 取详情页「原价」列，hy4 无折扣即原价。
MODELS = [
    {
        "public": "GLM-5.3-flash",
        "upstream": "GLM-5.3-flash",
        "context_window": 1000000,
        # 四项能力全部实测（2026-10-07，直连 www.autodl.art/api/v1）：
        #   stream / tools / image（data URI 图片）/ json_object（response_format）
        # 都真发过请求拿到 200；reasoning 见下面「推理档位」那条。
        "capabilities": {"stream": True, "tools": True, "reasoning": True,
                         "image": True, "json_object": True},
        # 输出上限按上游公布的 1000K 上下文不写死（未实测真实上限）：留 0 = 未公布。
        "max_output_tokens": 0,
        "list_price": {"input_cache_hit": 230_000, "input_cache_miss": 800_000,
                       "output": 2_800_000},
        "discount": (7, 10),
        "note": "会员 7 折（AutoDL 详情页标注「会员7折」）；实测流式/工具/图片/JSON 四项能力全通，"
                "reasoning_effort 只认 low/high/max（none/minimal/medium/xhigh 一律 400 "
                "param=reasoning_effort），且拒绝 {\"thinking\":{\"type\":\"disabled\"}}（该模型始终思考）",
    },
    {
        "public": "hy4-preview",
        "upstream": "hy4-preview",
        "context_window": 1024000,
        # 留空 = 未实测：任何请求都 402（见文件头），连参数校验都到不了。
        "capabilities": {},
        "max_output_tokens": 0,
        "list_price": {"input_cache_hit": 300_000, "input_cache_miss": 6_000_000,
                       "output": 18_000_000},
        "discount": (1, 1),
        "note": "无折扣；2026-10-07 起任何请求都是 402 401008（腾讯侧免费额度用尽、未开后付费），"
                "能力未实测，故 capabilities 留空 = 未公布",
    },
]

def discounted(price, disc, label):
    num, den = disc
    out = {}
    for dim, micros in price.items():
        assert micros % den == 0, f"{label}/{dim}：{micros} 不能被 {den} 整除，别用四舍五入掩盖口径"
        out[dim] = micros * num // den
    return out

def yuan(micros):
    return micros / 1_000_000

plan = {}
for m in MODELS:
    rates = discounted(m["list_price"], m["discount"], m["public"])
    num, den = m["discount"]
    # 单条 catch-all：这两个模型在 AutoDL 上没有分时价（不像 DeepSeek 的高峰/空闲两档），
    # 所以只需要一条规则。id 带折扣与来源，控制台上看得见这条规则是怎么来的。
    rule_id = m["public"].lower().replace("_", "-") + "-member-fixed"
    title = (f"AutoDL {m['public']} 成本价（人民币）：￥{yuan(rates['input_cache_hit']):g} 缓存命中 / "
             f"￥{yuan(rates['input_cache_miss']):g} 未命中 / ￥{yuan(rates['output']):g} 输出 每百万 tokens"
             f"（原价 ￥{yuan(m['list_price']['input_cache_hit']):g} / "
             f"￥{yuan(m['list_price']['input_cache_miss']):g} / "
             f"￥{yuan(m['list_price']['output']):g} 的 {num}/{den} 折扣）。"
             f"{m['note']}。AutoDL 无分时价，故单条 catch-all；币种 CNY，入账靠 billing.fx_rates.CNY")
    plan[m["public"]] = {
        "upstream": m["upstream"],
        "context_window": m["context_window"],
        "max_output_tokens": m["max_output_tokens"],
        "capabilities": m["capabilities"],
        "pricing_rules": {"currency": "CNY", "rules": [
            {"id": rule_id, "title": title, "order": 100, "when": {}, "rates": rates},
        ]},
    }

json.dump(plan, open(PLAN_OUT, "w"), ensure_ascii=False, indent=2)

print("将补齐的模型（费率单位 微元/百万 token，1e6 = ￥1；规则集币种 CNY）：")
for name, row in plan.items():
    r = row["pricing_rules"]["rules"][0]["rates"]
    caps = ",".join(sorted(row["capabilities"])) or "（未申报 = 未知）"
    print(f"  {name:16s} ctx={row['context_window']:<8d} max_out={row['max_output_tokens'] or '未公布':<8} "
          f"caps={caps}")
    print(f"      hit=￥{yuan(r['input_cache_hit']):<8g} miss=￥{yuan(r['input_cache_miss']):<8g} "
          f"out=￥{yuan(r['output']):<8g} 规则={row['pricing_rules']['rules'][0]['id']}")

# ── 自检：口径错了在这里就红，而不是等账单 ──────────────────────────────────
for name, row in plan.items():
    rules = row["pricing_rules"]["rules"]
    assert row["pricing_rules"]["currency"] == "CNY", f"{name}：规则集币种必须是 CNY"
    assert len(rules) == 1 and rules[0]["when"] == {}, f"{name}：必须是单条 catch-all（这两个模型无分时价）"
    r = rules[0]["rates"]
    assert r["input_cache_hit"] < r["input_cache_miss"], f"{name}：缓存命中价必须低于未命中价"
    for dim, micros in r.items():
        assert micros > 0, f"{name}/{dim}：折后价为 0，会变成免费"
    assert rules[0]["order"] > 0, f"{name}：规则 order 必须为正"
json.dump(plan, open(PLAN_OUT, "w"), ensure_ascii=False, indent=2)
print("\n自检通过：两条规则都是单条 catch-all、币种 CNY、命中价 < 未命中价、费率非 0。")
PY

# 干跑也要走完「登录（只读）→ 汇率前提 → 现况对照」这一段：只有把目标实例的真实状态打印
# 出来，干跑才算预览。写入留在 --apply 里。
code="$(curl -s -m 10 -c "$COOKIE" -o "$WORK/login.json" -w '%{http_code}' \
  -X POST "$GW_BASE/admin/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$GW_ADMIN_USER" "$GW_ADMIN_PASS")")"
[ "$code" = "200" ] || { echo "登录失败 HTTP $code: $(cat "$WORK/login.json")" >&2; exit 1; }
echo "已登录 $GW_BASE ($GW_ADMIN_USER)"

# ── 汇率前提：CNY 不在表里则写规则会被 400；汇率不等于 150000 则实收与确认口径不符 ──
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/billing/currency" -o "$WORK/currency.json"
python3 - "$WORK/currency.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
ledger = d["ledger_currency"]
rates = {c["code"]: c["rate_micros"] for c in d.get("currencies", [])}
print(f"账本币种 {ledger}；现有汇率表 {rates or '（空）'}")
if ledger != "USD":
    raise SystemExit(f"账本币种是 {ledger}，本脚本按「账本 USD + CNY 规则集」设计，请先确认口径")
if "CNY" not in rates:
    raise SystemExit("billing.fx_rates 里没有 CNY：先按 scripts/autodl-pricing.sh 的口径写入 "
                     "{\"CNY\":150000}（本脚本不代写汇率——它改的是实收金额）")
if rates["CNY"] != 150000:
    raise SystemExit(f"实例上的 billing.fx_rates.CNY = {rates['CNY']}，不是确认口径 150000"
                     "（1 元 = 0.15 美元）。汇率改的是实收金额，所以这里停下："
                     "确认后用 PUT /admin/api/v1/settings/billing.fx_rates 改。")
print("汇率已就位：CNY = 150000（1 元 = 0.150000 美元）")
PY

# 供应商 id 与已有行都按名字查，不写死：写错目标比不写更糟。
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers" -o "$WORK/providers.json"
PROVIDER_ID="$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
name=sys.argv[2]
for p in d["data"]:
    if p["name"]==name: print(p["id"]); break
else: raise SystemExit(f"providers 列表里没有 {name}")
' "$WORK/providers.json" "$PROVIDER_NAME")"
echo "供应商 $PROVIDER_NAME id=$PROVIDER_ID"

curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models?limit=200" -o "$WORK/pm.json"
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/models?limit=200" -o "$WORK/models.json"
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/routes?limit=200" -o "$WORK/routes.json"

# ── 折扣自检（可选）：直接问上游「这些模型的 member_discount 还是不是写入时那个」──────
# 为什么要查：折扣是**账号状态**（会员到期就没了），而折后价是写死在脚本里的常量。上游改了
# 折扣而我们不知道，就会按旧价计费 —— 那是静默的账目偏差。这里把折扣率当成一次自检。
#
# 为什么是可选：上游 key 在网关的凭据通道里（AES-GCM 加密落库、管理面永不回显明文），
# 脚本拿不到它。所以只有在调用方显式给了 key 时才能真查；没给就**明说跳过了**，不装作查过。
# 上游 base_url 不写在本文件里（仓库里不落真实域名），改从实例上那个供应商的 config 读 ——
# 这样查的就是网关真正在用的那个端点。
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID" -o "$WORK/provider.json"
UPSTREAM_BASE="$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print(((d.get("config") or {}).get("base_url") or "").rstrip("/"))
' "$WORK/provider.json")"
if [ -n "${AUTODL_API_KEY:-}" ] && [ -n "$UPSTREAM_BASE" ]; then
  code="$(curl -s -m 20 -o "$WORK/upstream-models.json" -w '%{http_code}' \
    "$UPSTREAM_BASE/models" -H "Authorization: Bearer $AUTODL_API_KEY")"
  if [ "$code" != "200" ]; then
    echo "折扣自检：跳过（上游 $UPSTREAM_BASE/models 回 HTTP $code，不是 200）" >&2
  else
    python3 - "$WORK/upstream-models.json" "$PLAN" <<'PY'
import json, sys
upstream = {m["id"]: m for m in json.load(open(sys.argv[1])).get("data", [])}
plan = json.load(open(sys.argv[2]))
# 脚本里那条折扣（分子/分母）从规则 title 里取不回来，所以这里独立重列一次：
# 它与上面的价格表必须同时改，改一处而漏另一处就会当场红 —— 这正是要的效果。
EXPECTED = {"GLM-5.3-flash": (7, 10), "hy4-preview": (1, 1)}
bad = 0
for name, (num, den) in EXPECTED.items():
    row = upstream.get(name)
    if row is None:
        print(f"  跳过 {name}：上游 /models 里没有这个 id（可能已下架）", file=sys.stderr)
        continue
    got = row.get("member_discount")
    want = num / den
    # 上游给的是浮点（0.7 / 1），用「四舍五入到 4 位小数」比，避免 0.7000000000000001 这类噪声。
    ok = got is not None and abs(float(got) - want) < 1e-4
    print(f"  {'ok  ' if ok else 'FAIL'} {name}: 上游 member_discount={got}（脚本按 {num}/{den}={want:g} 计价）")
    if not ok:
        bad += 1
print("  注：折扣只能读**列表**端点；GET /models/{id} 的 member_discount 恒为 0，拿它当真是错的。")
if bad:
    raise SystemExit(f"{bad} 个模型的折扣与脚本里的价不符：折扣是账号状态，"
                     "先去控制台确认当前价目表，再同步改本文件的价格表与 EXPECTED")
PY
  fi
else
  echo "折扣自检：跳过（没给 AUTODL_API_KEY；上游 key 在网关凭据通道里，脚本读不到）"
fi

# ── 决策：逐模型算出「要建 / 要更新 / 已一致」，并判掉不可自动覆盖的情况 ──────────
# 与 add-provider-models.py 同一条纪律：已有行的 upstream_model 与要补的不一致就整批中止，
# 绝不覆盖（那可能是别人手工映射到别处的行）。
python3 - "$PLAN" "$WORK/pm.json" "$WORK/models.json" "$WORK/routes.json" "$WORK/plan-actions.json" "$PROVIDER_NAME" <<'PY'
import json, sys

plan = json.load(open(sys.argv[1]))
pms = json.load(open(sys.argv[2]))["data"]
models = json.load(open(sys.argv[3]))["data"]
routes = json.load(open(sys.argv[4]))["data"]
out_path = sys.argv[5]
provider = sys.argv[6]

by_public = {row["public_model"]: row for row in pms}
by_name = {row["public_name"]: row for row in models}
route_keys = {(row["model"], row["provider"]): row for row in routes}

actions = []
fatals = []
for name, want in plan.items():
    act = {"public": name, "pm": "create", "pm_id": 0, "model": "create", "model_id": 0,
           "route": "create", "route_id": 0, "changes": []}
    row = by_public.get(name)
    if row is not None:
        act["pm_id"] = row["id"]
        upstream = (row.get("upstream_model") or "")
        if upstream and upstream != want["upstream"]:
            fatals.append(f"供应商模型行 {row['id']} 的 upstream_model={upstream!r}，"
                          f"与要补的 {want['upstream']!r} 不一致：不覆盖，请人工确认后再跑")
        else:
            if not upstream:
                act["changes"].append("补 upstream")
            if row.get("pricing_rules") != want["pricing_rules"]:
                act["changes"].append("重写成本价")
            if (row.get("capabilities") or {}) != want["capabilities"]:
                act["changes"].append("改能力申报")
            if row.get("context_window") != want["context_window"]:
                act["changes"].append(f"改上下文 {row.get('context_window')}→{want['context_window']}")
            if (row.get("max_output_tokens") or 0) != want["max_output_tokens"]:
                act["changes"].append(f"改输出上限 {row.get('max_output_tokens')}→{want['max_output_tokens']}")
            if not row.get("enabled"):
                act["changes"].append("启用")
            act["pm"] = "update" if act["changes"] else "ok"
    m = by_name.get(name)
    if m is not None:
        act["model_id"] = m["id"]
        act["model"] = "ok" if m.get("enabled") else "enable"
    r = route_keys.get((name, provider))
    if r is not None:
        act["route_id"] = r["id"]
        act["route"] = "ok" if r.get("enabled") else "enable"
    actions.append(act)

json.dump({"actions": actions, "fatals": fatals}, open(out_path, "w"), ensure_ascii=False, indent=2)

for act in actions:
    print(f"  {act['public']:16s} 供应商模型={act['pm']:6s} 对客模型={act['model']:6s} 路由={act['route']:6s}"
          + (f"  ({'、'.join(act['changes'])})" if act["changes"] else ""))
if fatals:
    for line in fatals:
        print("  " + line, file=sys.stderr)
    raise SystemExit(f"有 {len(fatals)} 处冲突，整批中止（未写入任何一行）")
PY

if [ "$APPLY" != "1" ]; then
  echo
  echo "干跑结束（未写入）。加 --apply 写入 $GW_BASE 的供应商 $PROVIDER_NAME。"
  echo "供部署根 config.yaml 的 bootstrap 使用（bootstrap.mode=upsert 只在行不存在时插入，"
  echo "所以重建库时要靠它把这三层与价一起种进去）："
  python3 - "$PLAN" "$PROVIDER_NAME" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1]))
provider = sys.argv[2]
prov = {"name": provider, "models": []}
for name, row in plan.items():
    prov["models"].append({
        "public": row["upstream"], "upstream": row["upstream"],
        "context_window": row["context_window"], "max_output_tokens": row["max_output_tokens"],
        "capabilities": row["capabilities"],
        "pricing_rules": json.dumps(row["pricing_rules"], ensure_ascii=False, separators=(",", ":")),
    })
snippet = {"bootstrap": {"providers": [prov],
                         "models": [{"public_name": n} for n in plan],
                         "routes": [{"model": n, "provider": provider,
                                     "upstream_model": row["upstream"],
                                     "priority": 10, "weight": 100} for n, row in plan.items()]}}
print(json.dumps(snippet, ensure_ascii=False, indent=2))
PY
  exit 0
fi

# ── 写入：供应商模型行（**同一笔带上成本价**）→ 对客模型 → 路由 ─────────────────
# 顺序不能换：add-provider-models.py 的注释里那条纪律——新建的行必须当场带上成本价，
# 先建行再补价会有一段时间是按 0 成本计费的。
echo
echo "开始写入…"
python3 - "$PLAN" "$WORK/plan-actions.json" "$WORK/bodies.json" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1]))
actions = json.load(open(sys.argv[2]))["actions"]
count = 0
out = {"pm": [], "model": [], "route": []}
for act in actions:
    want = plan[act["public"]]
    if act["pm"] in ("create", "update"):
        body = {"public_model": act["public"], "upstream_model": want["upstream"],
                "context_window": want["context_window"],
                "max_output_tokens": want["max_output_tokens"],
                "capabilities": want["capabilities"],
                "pricing_rules": want["pricing_rules"], "enabled": True}
        out["pm"].append({"public": act["public"], "body": body})
        count += 1
    if act["model"] in ("create", "enable"):
        out["model"].append({"public": act["public"], "body": {"public_name": act["public"], "enabled": True}})
        count += 1
    if act["route"] in ("create", "enable"):
        out["route"].append({"public": act["public"], "body": {
            "model": act["public"], "provider": "PROVIDER_NAME",
            "upstream_model": want["upstream"], "priority": 10, "weight": 100, "enabled": True}})
        count += 1
json.dump(out, open(sys.argv[3], "w"), ensure_ascii=False, indent=2)
print(f"共 {count} 次写入。")
PY

failed=0
for section in pm model route; do
  case "$section" in
    pm)    path="/admin/api/v1/providers/$PROVIDER_ID/models" ;;
    model) path="/admin/api/v1/models" ;;
    route) path="/admin/api/v1/routes" ;;
  esac
  count="$(python3 -c "import json,sys; print(len(json.load(open(sys.argv[1]))['$section']))" "$WORK/bodies.json")"
  i=0
  while [ "$i" -lt "$count" ]; do
    python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))['$section'][$i]
body=json.dumps(d['body']).replace('PROVIDER_NAME', sys.argv[2])
open(sys.argv[3],'w').write(body)
print(d['public'])" "$WORK/bodies.json" "$PROVIDER_NAME" "$WORK/one.json" > "$WORK/one.name"
    name="$(cat "$WORK/one.name")"
    code="$(curl -s -m 15 -b "$COOKIE" -o "$WORK/out.json" -w '%{http_code}' \
      -X POST "$GW_BASE$path" -H 'Content-Type: application/json' --data-binary "@$WORK/one.json")"
    if [ "$code" = "200" ] || [ "$code" = "201" ]; then
      echo "  ok   $section  $name"
      python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
if d.get('warning'): print('       warning: ' + d['warning'])" "$WORK/out.json" || true
    else
      echo "  FAIL $section  $name (HTTP $code): $(cat "$WORK/out.json")" >&2
      failed=$((failed + 1))
    fi
    i=$((i + 1))
  done
done
[ "$failed" = "0" ] || { echo "$failed 次写入失败" >&2; exit 1; }

# ── 读回核对：三层都要与计划逐字段相同 ─────────────────────────────────────────
echo
echo "读回核对…"
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models?limit=200" -o "$WORK/pm2.json"
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/models?limit=200" -o "$WORK/models2.json"
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/routes?limit=200" -o "$WORK/routes2.json"
python3 - "$PLAN" "$WORK/pm2.json" "$WORK/models2.json" "$WORK/routes2.json" "$PROVIDER_NAME" <<'PY'
import json, sys
plan = json.load(open(sys.argv[1]))
pms = {r["public_model"]: r for r in json.load(open(sys.argv[2]))["data"]}
models = {r["public_name"]: r for r in json.load(open(sys.argv[3]))["data"]}
provider = sys.argv[5]
routes = {(r["model"], r["provider"]): r for r in json.load(open(sys.argv[4]))["data"]}
bad = 0
for name, want in plan.items():
    row = pms.get(name)
    if row is None:
        print(f"  MISSING 供应商模型 {name}", file=sys.stderr); bad += 1; continue
    if row.get("upstream_model") != want["upstream"]:
        print(f"  DIFF    供应商模型 {name}：upstream={row.get('upstream_model')!r}", file=sys.stderr); bad += 1
    if row.get("pricing_rules") != want["pricing_rules"]:
        print(f"  DIFF    供应商模型 {name}：成本价与计划不一致", file=sys.stderr); bad += 1
    if (row.get("capabilities") or {}) != want["capabilities"]:
        print(f"  DIFF    供应商模型 {name}：能力申报与计划不一致", file=sys.stderr); bad += 1
    if row.get("context_window") != want["context_window"]:
        print(f"  DIFF    供应商模型 {name}：上下文与计划不一致", file=sys.stderr); bad += 1
    if not row.get("enabled"):
        print(f"  DIFF    供应商模型 {name}：未启用", file=sys.stderr); bad += 1
    if name not in models:
        print(f"  MISSING 对客模型 {name}", file=sys.stderr); bad += 1
    route = routes.get((name, provider))
    if route is None:
        print(f"  MISSING 路由 {name} -> {provider}", file=sys.stderr); bad += 1
    elif not route.get("enabled"):
        print(f"  DIFF    路由 {name}：未启用", file=sys.stderr); bad += 1
if bad:
    raise SystemExit(f"{bad} 处读回不一致")
print("读回核对：三层（供应商模型含价 / 对客模型 / 路由）与计划逐字段一致。")
PY

# ── 路由解释：候选在、没有 excluded ──────────────────────────────────────────
echo
echo "路由解释…"
for name in $(python3 -c "import json,sys; print(' '.join(json.load(open(sys.argv[1]))))" "$PLAN"); do
  curl -s -m 10 -b "$COOKIE" --get --data-urlencode "model=$name" \
    "$GW_BASE/admin/api/v1/router/explain" -o "$WORK/explain.json"
  python3 - "$WORK/explain.json" "$name" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
name = sys.argv[2]
order = [c for c in d.get("order") or []]
excluded = d.get("excluded") or []
ok = not excluded and any(c.get("upstream_model") for c in order)
print(f"  {'ok  ' if ok else 'FAIL'} {name}: 候选 {len(order)} 个, excluded {len(excluded)} 个"
      + (f" -> {[e.get('reason') for e in excluded]}" if excluded else ""))
if d.get("failure"):
    print(f"       failure: {d['failure']}")
if not ok:
    raise SystemExit(f"{name}: 路由解释不通过（failure={d.get('failure')!r}）")
PY
done

# ── 计价试算：真计价引擎算 1M 未命中 + 1M 输出，原生人民币与账本美元都要对上 ──────
echo
echo "计价试算（1M 未命中 + 1M 输出）…"
for name in $(python3 -c "import json,sys; print(' '.join(json.load(open(sys.argv[1]))))" "$PLAN"); do
  curl -s -m 10 -b "$COOKIE" -X POST "$GW_BASE/admin/api/v1/pricing/simulate" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$name\",\"dimensions\":{\"input_cache_miss\":1000000,\"output\":1000000}}" \
    -o "$WORK/sim.json"
  python3 - "$WORK/sim.json" "$PLAN" "$name" <<'PY'
import json, sys
sim = json.load(open(sys.argv[1]))
plan = json.load(open(sys.argv[2]))
name = sys.argv[3]
rates = plan[name]["pricing_rules"]["rules"][0]["rates"]
want_native = rates["input_cache_miss"] + rates["output"]
# 账本币种 USD，汇率 150000 微元/元（1 元 = 0.15 美元）：账本微美元 = 原生微元 × 0.15。
want_ledger = want_native * 150000 // 1_000_000
cost, charge = sim.get("cost_micros"), sim.get("charge_micros")
lcost, lcharge = sim.get("ledger_cost_micros"), sim.get("ledger_charge_micros")
ok = (cost == want_native and sim.get("cost_currency") == "CNY"
      and sim.get("ledger_currency") == "USD"
      and lcost == want_ledger and lcharge == want_ledger)
print(f"  {'ok  ' if ok else 'FAIL'} {name}: 原生 {sim.get('cost_currency')} cost={cost}"
      f"（期望 {want_native}）账本 {sim.get('ledger_currency')} cost={lcost} charge={lcharge}"
      f"（期望 {want_ledger}）rule={sim.get('cost_rule_id')}")
if not ok:
    print(json.dumps(sim, ensure_ascii=False, indent=2), file=sys.stderr)
    raise SystemExit(f"{name}: 试算结果与「会员价 × 汇率 0.15」不符")
PY
done

echo
echo "完成。核对入口："
echo "  控制台定价页：$GW_BASE/admin/ui/"
echo "  读回：curl -s -b <cookie> $GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models"
echo "  路由：curl -s -b <cookie> '$GW_BASE/admin/api/v1/router/explain?model=GLM-5.3-flash'"
echo "  真实请求：curl -s $GW_BASE/v1/responses -H 'Authorization: Bearer <key>' \\"
echo "            -H 'Content-Type: application/json' -d '{\"model\":\"GLM-5.3-flash\",\"input\":\"hi\"}'"
echo
echo "回滚（逐模型）："
echo "  停用对客可见：POST $GW_BASE/admin/api/v1/models  {\"public_name\":\"<模型>\",\"enabled\":false}"
echo "  停用路由：    PATCH $GW_BASE/admin/api/v1/routes/<route_id>  {\"enabled\":false}"
echo "  删净：        DELETE $GW_BASE/admin/api/v1/routes/<route_id>"
echo "                DELETE $GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models/<pm_id>"
