#!/usr/bin/env bash
# 给运行中的 ai-gateway 实例写入 autodl-api 上 DeepSeek-V4.1-Flash 的**人民币**成本价。
#
#   scripts/autodl-pricing.sh                       # 干跑：只打印将要写入的规则与自检
#   scripts/autodl-pricing.sh --apply               # 写入规则 + 汇率，并读回 + 试算核对
#   GW_BASE=http://127.0.0.1:8088 scripts/autodl-pricing.sh --apply
#
# 环境变量：GW_BASE（默认 http://127.0.0.1:8088）、GW_ADMIN_USER / GW_ADMIN_PASS
#          （默认取自 $CONFIG 的 bootstrap.admin）、CONFIG（默认 仓库根/config.yaml，
#           脚本在部署根下时自动改用与脚本同目录的 config.yaml）、
#          PROVIDER_NAME（默认 autodl-api）、PUBLIC_MODEL（默认 DeepSeek-V4.1-Flash）。
#
# ── 价格来源（2026-10-05 抓取）────────────────────────────────────────────────
#   官方人民币页 https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
#   官方美元页   https://api-docs.deepseek.com/quick_start/pricing
#
#   deepseek-flash —— MODEL VERSION 就是 **DeepSeek-V4.1-Flash**，即本供应商这一行
#   的上游模型。官方页脚注 (1)：旧名 deepseek-v4-flash / deepseek-v4-flash-vision-exp
#   仍被接受，但请求由 DeepSeek-V4.1-Flash 服务、按 Flash 价计费。
#
#   官方人民币原价（元 / 百万 tokens，2026-10-05）：
#                      缓存命中     缓存未命中    输出
#     Flash 空闲        0.02         1            4
#     Flash 高峰        0.04         2            8
#   官方美元页同一张表是 0.003/0.15/0.6（空闲）与 0.006/0.30/1.20（高峰），
#   两页隐含汇率处处是 **6.6667 元/美元**（1 元 = 0.15 美元）。
#
#   高峰 = 北京时间周一至周五 09:00-12:00、14:00-18:00（官方文字口径，不含中国法定
#   节假日）；官方英文页写作「Peak hours are 01:00 - 04:00 and 06:00 - 10:00 UTC,
#   Monday through Friday」，网关按 UTC 判定（billing.peak_boundary 默认
#   request_start），规则里就用这两个 UTC 窗口。官方「空闲价恰为高峰价的一半」，
#   所以规则只需两条：一条高峰窗口 + 一条 catch-all 空闲。
#
# ── 本次写入的价（用户口径）──────────────────────────────────────────────────
#   用户原话：「autodl-api 给用人民币计价」，并贴出价目表、确认取**第一列**：
#
#                      缓存命中     缓存未命中    输出
#     高峰（对客）      0.02         1.000        4.000    ← 官方高峰价 ÷ 2
#     空闲（对客）      0.010        0.500        2.000    ← 官方空闲价 ÷ 2
#
#   即「DeepSeek 官方人民币价目表打五折」，与上一版按美元写的五折完全同一个价
#   （0.15 美元 ⇔ 1 元），只是把规则集币种从 USD 换成 CNY。
#
# ── 汇率：为什么必须写，写多少 ───────────────────────────────────────────────
#   运行实例的账本币种是 **USD**（billing.currency），账本永远单一币种（M22 设计）。
#   CNY 规则集要入账就得先换算，而写入被拒的条件很硬：币种不在 billing.fx_rates 里
#   → 400（internal/httpapi/currency.go 的 validateRuleSetCurrency）。所以本脚本先
#   确保 billing.fx_rates.CNY 存在，再写规则集。
#
#   汇率取 **150000**（1 元 = 0.150000 美元）：这是官方中英文两页逐项隐含的同一个
#   汇率（见上面「价格来源」），不是市场汇率——它让「用人民币计价」成为一次**币种改
#   写**，客户实际被扣的美元与按美元规则集时**逐位相同**（0.5 元 = 0.075 美元）。用户
#   确认取此汇率（另两个候选 147567 市场中间价、140845≈7.1 会在同样的人民币价下把
#   扣费压低 1.6% / 6%）。
#
#   汇率写在部署根 config.yaml 的 billing.fx_rates（文件基线），运行期也可由控制台
#   「设置 → 汇率表」覆盖（PUT /settings/billing.fx_rates，立即生效、写审计）。脚本
#   只读不写这两个地方之外的配置，--apply 时若发现实例上汇率与 150000 不一致会**报错
#   停下**而不是默默按别的汇率计费。
#
#   ⚠ 汇率是「元 → 美元」的折算系数，不是价格本身：改它等于改所有人民币规则集的实收。
#     核对口径：/admin/api/v1/billing/currency 看 rate_micros，快照里看 fx_cost_ledger。
#
# ── 一处刻意的近似 ───────────────────────────────────────────────────────────
#   官方「北京时间周一至周五（**不含中国法定节假日**）9:00-12:00、14:00-18:00 为高峰；
#   其余时段，包括周末及中国法定节假日全天均为空闲」——节假日日历网关表达不了（规则只有
#   time_windows：星期 + 时刻），法定节假日里本规则按高峰价计（对客户略偏高、方向保守）。
#   与 scripts/deepseek-official-pricing.sh 同一处近似。
#
# ── 单位 ─────────────────────────────────────────────────────────────────────
#   费率单位是「微单位 / 百万 token」（pricing.RateScale = 1e6）：1 元/百万 = 1000000。
#   数字全部由官方元价在脚本里用整数算出（禁止手抄费率、禁止浮点），五折 = 整除 2，
#   除不尽即报错退出（本表都能整除）。

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

# ── 价格表：官方元价（微元/百万 token）→ 五折 → CNY 规则集 ────────────────────
python3 - "$PLAN" "$PUBLIC_MODEL" <<'PY'
import json, sys

PLAN_OUT, MODEL = sys.argv[1], sys.argv[2]

# 官方人民币价（元 / 百万 token）直接写成微元整数，避免任何浮点：1e6 微元 = 1 元。
# 官方 2026-10-05 中文页：Flash 高峰 0.04 / 2 / 8，空闲 0.02 / 1 / 4。
OFFICIAL = {
    "peak":    {"input_cache_hit":    40_000, "input_cache_miss": 2_000_000, "output": 8_000_000},
    "offpeak": {"input_cache_hit":    20_000, "input_cache_miss": 1_000_000, "output": 4_000_000},
}
DISCOUNT_NUM, DISCOUNT_DEN = 1, 2  # 五折

# 官方美元页的同一张表（微美元/百万 token）。它和上面的元价必须落在同一个汇率上——
# 这是本脚本最值得自动核对的一件事：两页任何一处抄错、或官方改了其中一页，这里先红。
OFFICIAL_USD = {
    "peak":    {"input_cache_hit":    6_000, "input_cache_miss":  300_000, "output": 1_200_000},
    "offpeak": {"input_cache_hit":    3_000, "input_cache_miss":  150_000, "output":   600_000},
}
CNY_PER_USD_MICROS = 150_000  # 1 元 = 0.15 美元 == 1 美元 = 6.6667 元（官方两页隐含）

def half(rates, label):
    out = {}
    for dim, micros in rates.items():
        assert micros % DISCOUNT_DEN == 0, f"{label}/{dim}：{micros} 不能被 {DISCOUNT_DEN} 整除，别用四舍五入掩盖口径"
        out[dim] = micros * DISCOUNT_NUM // DISCOUNT_DEN
    return out

PEAK = half(OFFICIAL["peak"], "官方高峰价")
OFF = half(OFFICIAL["offpeak"], "官方空闲价")

def yuan(micros):
    return micros / 1_000_000

DIMS = ("input_cache_hit", "input_cache_miss", "output")
# 逐项交叉自检：元价 × 0.15 必须恰好等于美元价（整数域里用 元 × 150000 / 1e6 表达，
# 两边都是 1e6 尺度，所以等价于 元 × 150000 == 美元 × 1e6）。
for tier in ("peak", "offpeak"):
    for dim in DIMS:
        cny, usd = OFFICIAL[tier][dim], OFFICIAL_USD[tier][dim]
        assert cny * CNY_PER_USD_MICROS == usd * 1_000_000, (
            f"{tier}/{dim}：官方元价 {cny} 与美元价 {usd} 不在汇率 0.15 上（"
            f"元×0.15={cny * CNY_PER_USD_MICROS / 1e6:.4f} vs 美元={usd / 1e6:.4f}）")

rules = [
    {"id": "deepseek-flash-half-peak",
     "title": (f"DeepSeek 官方人民币高峰价（￥{yuan(OFFICIAL['peak']['input_cache_hit']):g} 缓存命中 / "
               f"￥{yuan(OFFICIAL['peak']['input_cache_miss']):g} 未命中 / ￥{yuan(OFFICIAL['peak']['output']):g} 输出 "
               f"每百万 tokens）打五折：￥{yuan(PEAK['input_cache_hit']):g} / "
               f"￥{yuan(PEAK['input_cache_miss']):g} / ￥{yuan(PEAK['output']):g}。"
               f"高峰 = 北京时间周一至周五 09:00-12:00 / 14:00-18:00（UTC 01:00-04:00 / 06:00-10:00）；"
               f"官方另按中国法定节假日全天算空闲，网关规则表达不了节假日日历，节假日按高峰价计（偏保守）"),
     "order": 10, "when": {"time_windows": [
         {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "01:00", "end": "04:00", "tz": "UTC"},
         {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "06:00", "end": "10:00", "tz": "UTC"},
     ]},
     "rates": PEAK},
    {"id": "deepseek-flash-half-offpeak",
     "title": (f"DeepSeek 官方人民币空闲价（￥{yuan(OFFICIAL['offpeak']['input_cache_hit']):g} / "
               f"￥{yuan(OFFICIAL['offpeak']['input_cache_miss']):g} / ￥{yuan(OFFICIAL['offpeak']['output']):g} "
               f"每百万 tokens）打五折：￥{yuan(OFF['input_cache_hit']):g} / "
               f"￥{yuan(OFF['input_cache_miss']):g} / ￥{yuan(OFF['output']):g}。"
               f"catch-all：官方空闲价 = 高峰价的一半，故本档 = 官方高峰价的 1/4"),
     "order": 100, "when": {}, "rates": OFF},
]
plan = {"currency": "CNY", "rules": rules}
json.dump(plan, open(PLAN_OUT, "w"), ensure_ascii=False, indent=2)

# 部署根 config.yaml 的两处 bootstrap 片段（规则与汇率各一份）。
# 为什么两处都要写：bootstrap.mode=upsert 只在**行/设置不存在**时插入，所以脚本写进去的
# 规则与汇率不会因重启丢失，但也意味着**重建库（删 data/、换机器）时新库里既没有价也没有
# 汇率**——CNY 规则集没有汇率就是一次按 0 计费的请求。把两者同时写进部署配置，重建出来的
# 实例才是带价的。单价只在上面维护，这里只是同一条 JSON 的 YAML 形态。
bootstrap = (
    f"          pricing_rules: |-\n"
    f"            " + json.dumps(plan, ensure_ascii=False, separators=(",", ":")) + "\n"
)
open(PLAN_OUT + ".bootstrap.yaml", "w").write(bootstrap)
fx_snippet = f"  fx_rates: {{CNY: {CNY_PER_USD_MICROS}}}   # 1 元 = 0.150000 美元（官方中英文两页隐含汇率）\n"
open(PLAN_OUT + ".fx.yaml", "w").write(fx_snippet)

print(f"模型 {MODEL}：官方人民币价 × {DISCOUNT_NUM}/{DISCOUNT_DEN}（费率单位 微元/百万 token，1e6 = ￥1，规则集币种 CNY）")
for rule, official in ((rules[0], OFFICIAL["peak"]), (rules[1], OFFICIAL["offpeak"])):
    r, o = rule["rates"], official
    print(f"  {rule['id']:28s} hit=￥{yuan(r['input_cache_hit']):<8g} miss=￥{yuan(r['input_cache_miss']):<8g} "
          f"out=￥{yuan(r['output']):<8g}   官方 hit=￥{yuan(o['input_cache_hit']):<6g} "
          f"miss=￥{yuan(o['input_cache_miss']):<6g} out=￥{yuan(o['output']):<6g}")
print(f"\n汇率前提：billing.fx_rates.CNY = {CNY_PER_USD_MICROS}（1 元 = "
      f"{CNY_PER_USD_MICROS / 1e6:.6f} 美元），与官方美元页逐项一致。")

# ── 自检：口径错了在这里就红，而不是等账单 ──────────────────────────────────
assert rules[0]["when"].get("time_windows") and rules[1]["when"] == {}, "分时规则顺序/兜底不对"
assert plan["currency"] == "CNY", "规则集币种必须是 CNY"
for dim in DIMS:
    assert OFF[dim] * 2 == PEAK[dim], f"{dim}：空闲价不是高峰价的一半（官方口径）"
    assert PEAK[dim] * DISCOUNT_DEN == OFFICIAL["peak"][dim] * DISCOUNT_NUM, f"{dim}：高峰档不是官方价的五折"
    assert OFF[dim] * DISCOUNT_DEN == OFFICIAL["offpeak"][dim] * DISCOUNT_NUM, f"{dim}：空闲档不是官方价的五折"
    assert PEAK[dim] > 0 and OFF[dim] > 0, f"{dim}：五折后价格为 0，会变成免费"
    # 与上一版按美元写的五折必须逐位相同：CNY 规则 × 汇率 == 原 USD 规则。
    assert PEAK[dim] * CNY_PER_USD_MICROS // 1_000_000 == OFFICIAL_USD["peak"][dim] // 2, f"{dim}：高峰档换回美元与 USD 版五折不一致"
    assert OFF[dim] * CNY_PER_USD_MICROS // 1_000_000 == OFFICIAL_USD["offpeak"][dim] // 2, f"{dim}：空闲档换回美元与 USD 版五折不一致"
print("\n自检通过：两条规则（高峰窗口 + catch-all）、空闲=高峰一半、两档都恰为官方人民币价的 50%、"
      "费率非 0、且换回美元与官方美元页五折逐位相同。")
print("\n供部署根 config.yaml 的 bootstrap 使用（该 provider 的模型行下，缩进对齐 models 项）：\n"
      + bootstrap, end="")
print("\n以及 billing 段的汇率（合并进现有 fx_rates，别覆盖别的币种）：\n" + fx_snippet, end="")
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

# ── 汇率前提：CNY 不在表里则写规则会被 400；汇率不等于 150000 则实收与确认口径不符 ──
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/billing/currency" -o "$WORK/currency.json"
python3 - "$WORK/currency.json" "$WORK/fx-expected" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
ledger = d["ledger_currency"]
rates = {c["code"]: c["rate_micros"] for c in d.get("currencies", [])}
open(sys.argv[2], "w").write(json.dumps({"ledger": ledger, "rates": rates}))
print(f"账本币种 {ledger}；现有汇率表 {rates or '（空）'}；display={d.get('display_currency')}")
if ledger == "CNY":
    raise SystemExit("账本币种已经是 CNY，本脚本按「账本 USD + CNY 规则集」设计，请先确认口径")
PY

# 汇率缺失就写；存在但不等于 150000 就停下问人（静默按别的汇率计费是最坏的结果）。
CUR="$WORK/currency.json"
if python3 -c "
import json,sys
d=json.load(open('$CUR')); rates={c['code']:c['rate_micros'] for c in d.get('currencies',[])}
sys.exit(0 if 'CNY' in rates else 1)
"; then
  rate="$(python3 -c "
import json;d=json.load(open('$CUR'));print({c['code']:c['rate_micros'] for c in d['currencies']}['CNY'])")"
  if [ "$rate" != "150000" ]; then
    echo "实例上的 billing.fx_rates.CNY = $rate，不是确认口径 150000（1 元 = 0.15 美元）。" >&2
    echo "汇率改的是实收金额，所以这里停下：确认后用 PUT /admin/api/v1/settings/billing.fx_rates 改。" >&2
    exit 1
  fi
  echo "汇率已就位：CNY = $rate"
else
  code="$(curl -s -m 10 -b "$COOKIE" -o "$WORK/fx.json" -w '%{http_code}' \
    -X PUT "$GW_BASE/admin/api/v1/settings/billing.fx_rates" \
    -H 'Content-Type: application/json' -d '{"CNY":150000}')"
  [ "$code" = "200" ] || { echo "写汇率失败 HTTP $code: $(cat "$WORK/fx.json")" >&2; exit 1; }
  echo "已写入汇率 billing.fx_rates = {\"CNY\":150000}"
fi

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
cur = row.get("pricing_rules") or {}
cur_cur = cur.get("currency") or "（未声明 = 账本币种）"
print(f"目标行 id={row['id']} upstream={row['upstream_model']} 当前币种={cur_cur} "
      f"当前规则={'（空）' if not row.get('pricing_rules') else str(len(cur.get('rules') or [])) + ' 条'}")
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
echo "已写入 $PROVIDER_ID/$PUBLIC_MODEL（币种 CNY）"

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
print("读回核对：pricing_rules 与计划逐字段一致（含 currency=CNY）")
PY

# ── 试算：真实计价引擎算四个场景，原生 CNY 与账本 USD 都要对上 ────────────────
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

# 每项：标签, 原生微元成本期望（= 官方元价五折）, 账本微美元期望（= 原生 × 0.15）, 期望命中规则。
#
# 注意 cost_micros 与 charge_micros 的币种**不一定相同**：引擎里两者各按自己规则集的币种
# 报告（internal/pricing/engine.go 的 CostCurrency / SaleCurrency）。本模型没有 sale_pricing，
# 售价侧就落在账本币种（USD），所以 cost_micros 是人民币、charge_micros 是美元；两者换算后
# 数字相等（cost_follow 1.0×）。下面的断言把这一点显式写出来，避免"看起来相等"掩盖币种错误。
CASES = [
    ("高峰（UTC 周一 01:30）1M 未命中 + 1M 输出",
     1_000_000 + 4_000_000, 150_000 + 600_000, "deepseek-flash-half-peak", sys.argv[1]),
    ("空闲（UTC 周一 00:30）1M 未命中 + 1M 输出",
     500_000 + 2_000_000, 75_000 + 300_000, "deepseek-flash-half-offpeak", sys.argv[2]),
    ("高峰 1M 缓存命中", 20_000, 3_000, "deepseek-flash-half-peak", sys.argv[3]),
    ("周日 02:00 也算空闲（周末无高峰）1M+1M",
     500_000 + 2_000_000, 75_000 + 300_000, "deepseek-flash-half-offpeak", sys.argv[4]),
]
bad = 0
for label, want_native, want_ledger, rule, raw in CASES:
    d = parse(raw, label)
    cost, charge = d.get("cost_micros"), d.get("charge_micros")
    lcost, lcharge = d.get("ledger_cost_micros"), d.get("ledger_charge_micros")
    cost_cur, sale_cur, ledger_cur = (d.get("cost_currency"), d.get("sale_currency"),
                                      d.get("ledger_currency"))
    matched = d.get("cost_rule_id")
    ok = (cost == want_native and cost_cur == "CNY"      # 成本是人民币（原生）
          and charge == want_ledger and sale_cur == "USD"  # 售价侧无规则 → 落在账本币种
          and lcost == want_ledger and lcharge == want_ledger and ledger_cur == "USD"
          and matched == rule)
    bad += 0 if ok else 1
    print(f"  {'ok  ' if ok else 'FAIL'} {label}")
    print(f"       成本原生 {cost_cur}: cost={cost}（期望 {want_native} 微元）")
    print(f"       售价 {sale_cur}: charge={charge}（期望 {want_ledger}）")
    print(f"       账本 {ledger_cur}: cost={lcost} charge={lcharge}（期望 {want_ledger}）rule={matched}（期望 {rule}）")
if bad:
    raise SystemExit(f"{bad} 个场景与「官方人民币价五折 × 汇率 0.15」不符")
print("\n试算核对：四个场景的原生人民币成本、账本美元金额与命中规则全部一致；"
      "账本数字与按美元规则集写入时逐位相同。")
PY

echo
echo "完成。核对入口："
echo "  控制台定价页：$GW_BASE/admin/ui/"
echo "  汇率：curl -s -b <cookie> $GW_BASE/admin/api/v1/billing/currency"
echo "  读回：curl -s -b <cookie> $GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models"
echo "  试算：POST $GW_BASE/admin/api/v1/pricing/simulate"
echo "  真实请求：curl -s $GW_BASE/v1/responses -H 'Authorization: Bearer <key>' -d '{\"model\":\"$PUBLIC_MODEL\",\"input\":\"hi\"}'"
