#!/usr/bin/env bash
# 给运行中的 ai-gateway 实例写入 autodl-api 上 DeepSeek-V4.1-Flash 的成本价：
# **DeepSeek 官方价目表打五折**（官方价 × 50%）。
#
#   scripts/autodl-pricing.sh                       # 干跑：只打印将要写入的规则与自检
#   scripts/autodl-pricing.sh --apply               # 真正写入，并读回 + 试算核对
#   GW_BASE=http://127.0.0.1:8088 scripts/autodl-pricing.sh --apply
#
# 环境变量：GW_BASE（默认 http://127.0.0.1:8088）、GW_ADMIN_USER / GW_ADMIN_PASS
#          （默认取自 $CONFIG 的 bootstrap.admin）、CONFIG（默认 仓库根/config.yaml，
#           脚本在部署根下时自动改用与脚本同目录的 config.yaml）、
#          PROVIDER_NAME（默认 autodl-api）、PUBLIC_MODEL（默认 DeepSeek-V4.1-Flash）。
#
# ── 价格来源（2026-10-05 抓取）────────────────────────────────────────────────
#   https://api-docs.deepseek.com/quick_start/pricing
#   （英文页；中文页按元报价，两页口径一致）
#
#   deepseek-flash —— MODEL VERSION 就是 **DeepSeek-V4.1-Flash**，即本供应商这一行
#   的上游模型。官方页脚注 (1)：旧名 deepseek-v4-flash / deepseek-v4-flash-vision-exp
#   仍被接受，但请求由 DeepSeek-V4.1-Flash 服务、按 Flash 价计费。
#
#   官方价（USD / 百万 tokens，2026-10-05 官方页原值）：
#                      缓存命中     缓存未命中    输出
#     Flash 空闲        0.003        0.15         0.60
#     Flash 高峰        0.006        0.30         1.20
#
#   高峰 = 北京时间周一至周五 09:00-12:00、14:00-18:00；官方英文页写作
#   「Peak hours are 01:00 - 04:00 and 06:00 - 10:00 UTC, Monday through Friday」，
#   网关按 UTC 判定（billing.peak_boundary 默认 request_start），规则里就用这两个窗口。
#   空闲价恰为高峰价的一半，所以规则只需两条：一条高峰窗口 + 一条 catch-all 空闲。
#
# ── 本次写入的价：官方价 × 50%（五折）─────────────────────────────────────────
#   用户口径：「配置 autodl-api DeepSeek-V4.1-Flash 的价格用 deepseek 官方价格打五折设置」，
#   并确认**整张官方价目表打五折、保留分时**（而不是把某一档拍成平价）。于是：
#
#                      缓存命中     缓存未命中    输出
#     高峰（对客）      0.003        0.15         0.60     ← 官方高峰价 ÷ 2
#     空闲（对客）      0.0015       0.075        0.30     ← 官方空闲价 ÷ 2
#
#   为什么写成本侧就够：本实例售价侧没有任何 models.sale_pricing，走
#   billing.basis_default=cost_follow × billing.default_markup_bp=10000（=1.0×），
#   所以成本价就是计费价（1:1 透传）。若哪天给该模型写了售价规则，本脚本的价只再是成本。
#
# ── 一处刻意的近似 ───────────────────────────────────────────────────────────
#   官方「高峰时段…excluding Chinese public holidays（法定节假日全天按空闲）」在网关里
#   表达不了——规则只有 time_windows（星期 + 时刻），没有节假日日历。法定节假日里本规则会
#   按高峰价计（对客户略偏高，方向是保守的）。与 scripts/deepseek-official-pricing.sh 同一处近似。
#
# ── 单位 ─────────────────────────────────────────────────────────────────────
#   费率单位是「微美元 / 百万 token」（pricing.RateScale = 1e6）：$0.30/百万 = 300000。
#   数字全部由官方价在脚本里算出并自检（禁止手抄费率），避免浮点：官方价直接用微美元整数写，
#   五折 = 整除 2，除不尽即报错退出（本表都能整除）。
#
# 实测核对口径见写入后的输出：读回逐字段比对 + POST /pricing/simulate（高峰/空闲/缓存命中
# 三个场景）用真实计价引擎算，数字与「官方价 ÷ 2」逐位对上才算通过。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
GW_BASE="${GW_BASE:-http://127.0.0.1:8088}"
PROVIDER_NAME="${PROVIDER_NAME:-autodl-api}"
PUBLIC_MODEL="${PUBLIC_MODEL:-DeepSeek-V4.1-Flash}"
APPLY=0
[ "${1:-}" = "--apply" ] && APPLY=1

CONFIG="${CONFIG:-$ROOT/config.yaml}"
# 部署形态（脚本与 config.yaml 同目录，如 /media/.../aigw/scripts/autodl-pricing.sh）里上面那个
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

# ── 价格表：官方价（微美元/百万 token）→ 五折 → 规则集 ────────────────────────
python3 - "$PLAN" "$PUBLIC_MODEL" <<'PY'
import json, sys

PLAN_OUT, MODEL = sys.argv[1], sys.argv[2]

# 官方价（USD / 百万 token）直接写成微美元整数，避免任何浮点：1e6 微美元 = $1。
# 官方 2026-10-05 页：Flash 高峰 0.006 / 0.30 / 1.20，空闲 0.003 / 0.15 / 0.60。
OFFICIAL = {
    "peak":    {"input_cache_hit":    6_000, "input_cache_miss":  300_000, "output": 1_200_000},
    "offpeak": {"input_cache_hit":    3_000, "input_cache_miss":  150_000, "output":   600_000},
}
DISCOUNT_NUM, DISCOUNT_DEN = 1, 2  # 五折

def half(rates, label):
    out = {}
    for dim, micros in rates.items():
        assert micros % DISCOUNT_DEN == 0, f"{label}/{dim}：{micros} 不能被 {DISCOUNT_DEN} 整除，别用四舍五入掩盖口径"
        out[dim] = micros * DISCOUNT_NUM // DISCOUNT_DEN
    return out

PEAK = half(OFFICIAL["peak"], "官方高峰价")
OFF = half(OFFICIAL["offpeak"], "官方空闲价")

def usd(micros):
    return micros / 1_000_000

rules = [
    {"id": "deepseek-flash-half-peak",
     "title": (f"官方高峰价（${usd(OFFICIAL['peak']['input_cache_hit']):g} / "
               f"${usd(OFFICIAL['peak']['input_cache_miss']):g} / ${usd(OFFICIAL['peak']['output']):g} "
               f"每百万 tokens）打五折：${usd(PEAK['input_cache_hit']):g} 缓存命中 / "
               f"${usd(PEAK['input_cache_miss']):g} 未命中 / ${usd(PEAK['output']):g} 输出。"
               f"高峰 = 北京时间周一至周五 09:00-12:00 / 14:00-18:00（UTC 01:00-04:00 / 06:00-10:00）；"
               f"官方另按中国法定节假日全天算空闲，网关规则表达不了节假日日历，节假日按高峰价计（偏保守）"),
     "order": 10, "when": {"time_windows": [
         {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "01:00", "end": "04:00", "tz": "UTC"},
         {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "06:00", "end": "10:00", "tz": "UTC"},
     ]},
     "rates": PEAK},
    {"id": "deepseek-flash-half-offpeak",
     "title": (f"官方空闲价（${usd(OFFICIAL['offpeak']['input_cache_hit']):g} / "
               f"${usd(OFFICIAL['offpeak']['input_cache_miss']):g} / ${usd(OFFICIAL['offpeak']['output']):g} "
               f"每百万 tokens）打五折：${usd(OFF['input_cache_hit']):g} 缓存命中 / "
               f"${usd(OFF['input_cache_miss']):g} 未命中 / ${usd(OFF['output']):g} 输出。"
               f"catch-all：官方空闲价 = 高峰价的一半，故本档 = 官方高峰价的 1/4"),
     "order": 100, "when": {}, "rates": OFF},
]
plan = {"currency": "USD", "rules": rules}
json.dump(plan, open(PLAN_OUT, "w"), ensure_ascii=False, indent=2)

# 部署根 config.yaml 的 bootstrap 片段（同一条规则的第二处落点）。
# 为什么两处都要写：bootstrap.mode=upsert 只在**行不存在**时插入，所以脚本写库的规则不会因
# 重启丢失，但也意味着**重建库（删 data/、换机器）时新行会没有价**。把规则同时写进部署配置，
# 重建出来的实例才是带价的。单价只在上面维护，这里只是同一条 JSON 的 YAML 块标量形态。
bootstrap = (
    f"          pricing_rules: |-\n"
    f"            " + json.dumps(plan, ensure_ascii=False, separators=(",", ":")) + "\n"
)
open(PLAN_OUT + ".bootstrap.yaml", "w").write(bootstrap)

print(f"模型 {MODEL}：官方价 × {DISCOUNT_NUM}/{DISCOUNT_DEN}（费率单位 微美元/百万 token，1e6 = $1）")
for rule, official in ((rules[0], OFFICIAL["peak"]), (rules[1], OFFICIAL["offpeak"])):
    r, o = rule["rates"], official
    print(f"  {rule['id']:28s} hit={r['input_cache_hit']:>7,d} miss={r['input_cache_miss']:>8,d} "
          f"out={r['output']:>9,d}   官方 hit={o['input_cache_hit']:>6,d} miss={o['input_cache_miss']:>7,d} "
          f"out={o['output']:>9,d}")

# ── 自检：口径错了在这里就红，而不是等账单 ──────────────────────────────────
assert rules[0]["when"].get("time_windows") and rules[1]["when"] == {}, "分时规则顺序/兜底不对"
for dim in ("input_cache_hit", "input_cache_miss", "output"):
    assert OFF[dim] * 2 == PEAK[dim], f"{dim}：空闲价不是高峰价的一半（官方口径）"
    assert PEAK[dim] * DISCOUNT_DEN == OFFICIAL["peak"][dim] * DISCOUNT_NUM, f"{dim}：高峰档不是官方价的五折"
    assert OFF[dim] * DISCOUNT_DEN == OFFICIAL["offpeak"][dim] * DISCOUNT_NUM, f"{dim}：空闲档不是官方价的五折"
    assert PEAK[dim] > 0 and OFF[dim] > 0, f"{dim}：五折后价格为 0，会变成免费"
print("\n自检通过：两条规则（高峰窗口 + catch-all）、空闲=高峰一半、两档都恰为官方价的 50%、费率非 0。")
print("\n供部署根 config.yaml 的 bootstrap 使用（该 provider 的模型行下，缩进对齐 models 项）：\n"
      + bootstrap, end="")
PY

if [ "$APPLY" != "1" ]; then
  echo
  echo "干跑结束（未写入）。加 --apply 写入 $GW_BASE 的供应商 $PROVIDER_NAME / 模型 $PUBLIC_MODEL。"
  exit 0
fi

code="$(curl -s -m 10 -c "$COOKIE" -o "$WORK/login.json" -w '%{http_code}' \
  -X POST "$GW_BASE/admin/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$GW_ADMIN_USER" "$GW_ADMIN_PASS")")"
[ "$code" = "200" ] || { echo "登录失败 HTTP $code: $(cat "$WORK/login.json")" >&2; exit 1; }
echo "已登录 $GW_BASE ($GW_ADMIN_USER)"

# 供应商 id 与模型行都按名字查，不写死：写错目标比不写更糟。
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

curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models" -o "$WORK/models.json"
python3 - "$WORK/models.json" "$PLAN" "$WORK/body.json" "$PUBLIC_MODEL" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))["data"]
plan = json.load(open(sys.argv[2]))
public = sys.argv[4]
row = next((r for r in rows if r["public_model"] == public), None)
if row is None:
    raise SystemExit(f"供应商上没有这个模型映射：{public}（现有：" +
                     ", ".join(r["public_model"] for r in rows) + "）")
print(f"目标行 id={row['id']} upstream={row['upstream_model']} 当前 pricing_rules="
      f"{'（空）' if not row.get('pricing_rules') else '已有规则，将被覆盖'}")
# POST /providers/{id}/models 自 M28 起是**部分更新**：只提交 public_model + pricing_rules，
# 不碰 capabilities / 上下文 / 权重 / enabled，避免把运行态参数打回默认值。
json.dump({"public_model": public, "pricing_rules": plan}, open(sys.argv[3], "w"), ensure_ascii=False)
PY

code="$(curl -s -m 15 -b "$COOKIE" -o "$WORK/out.json" -w '%{http_code}' \
  -X POST "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models" \
  -H 'Content-Type: application/json' --data-binary "@$WORK/body.json")"
if [ "$code" != "200" ]; then
  echo "写入失败 HTTP $code: $(cat "$WORK/out.json")" >&2; exit 1
fi
echo "已写入 $PROVIDER_ID/$PUBLIC_MODEL"

# ── 读回核对：库里的规则必须与计划逐字段相同 ────────────────────────────────
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models" -o "$WORK/verify.json"
python3 - "$WORK/verify.json" "$PLAN" "$PUBLIC_MODEL" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))["data"]
plan = json.load(open(sys.argv[2]))
row = next((r for r in rows if r["public_model"] == sys.argv[3]), None)
if row is None:
    raise SystemExit(f"读回失败：找不到 {sys.argv[3]}")
if row.get("pricing_rules") != plan:
    print(f"读回的规则：{json.dumps(row.get('pricing_rules'), ensure_ascii=False)}", file=sys.stderr)
    raise SystemExit("读回的定价规则与计划不一致")
print("读回核对：pricing_rules 与计划逐字段一致")
PY

# ── 试算：用真实计价引擎算三个场景，数字必须等于官方价 ÷ 2 ──────────────────
simulate() { # at dimensions
  curl -s -m 10 -b "$COOKIE" -X POST "$GW_BASE/admin/api/v1/pricing/simulate" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$PUBLIC_MODEL\",\"at\":\"$1\",\"dimensions\":$2}"
}
# 2026-10-05 是周一：01:30 UTC 落进第一个高峰窗口，00:30 UTC（周一）落进空闲。
peak=$(simulate 2026-10-05T01:30:00Z '{"input_cache_miss":1000000,"output":1000000}')
off=$(simulate 2026-10-05T00:30:00Z '{"input_cache_miss":1000000,"output":1000000}')
hit=$(simulate 2026-10-05T01:30:00Z '{"input_cache_hit":1000000}')
weekend=$(simulate 2026-10-04T02:00:00Z '{"input_cache_miss":1000000,"output":1000000}')
python3 - "$peak" "$off" "$hit" "$weekend" <<'PY'
import json, sys

def parse(raw, label):
    try:
        return json.loads(raw)
    except Exception:
        raise SystemExit(f"{label}: 响应无法解析：{raw[:200]}")

CASES = [
    # 标签, 期望成本(微美元), 期望命中规则
    ("高峰（UTC 周一 01:30）1M 未命中 + 1M 输出", 150_000 + 600_000, "deepseek-flash-half-peak", sys.argv[1]),
    ("空闲（UTC 周一 00:30）1M 未命中 + 1M 输出",  75_000 + 300_000, "deepseek-flash-half-offpeak", sys.argv[2]),
    ("高峰 1M 缓存命中",                             3_000,           "deepseek-flash-half-peak", sys.argv[3]),
    ("周日 02:00 也算空闲（周末无高峰）1M+1M",      75_000 + 300_000, "deepseek-flash-half-offpeak", sys.argv[4]),
]
bad = 0
for label, expect, rule, raw in CASES:
    d = parse(raw, label)
    cost, charge, matched = d.get("cost_micros"), d.get("charge_micros"), d.get("cost_rule_id")
    ok = (cost == expect) and (charge == expect) and (matched == rule)
    bad += 0 if ok else 1
    print(f"  {'ok  ' if ok else 'FAIL'} {label}：cost={cost} charge={charge} rule={matched}"
          f"（期望 {expect} / {rule}）")
if bad:
    raise SystemExit(f"{bad} 个场景与「官方价 × 50%」不符")
print("\n试算核对：四个场景的成本、计费与命中规则全部与「DeepSeek 官方价打五折」一致。")
PY

echo
echo "完成。核对入口："
echo "  控制台定价页：$GW_BASE/admin/ui/"
echo "  读回：curl -s -b <cookie> $GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models"
echo "  试算：POST $GW_BASE/admin/api/v1/pricing/simulate"
echo "  真实请求：curl -s $GW_BASE/v1/responses -H 'Authorization: Bearer <key>' -d '{\"model\":\"$PUBLIC_MODEL\",\"input\":\"hi\"}'"
