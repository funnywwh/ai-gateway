#!/usr/bin/env bash
# 给运行中的 ai-gateway 实例写入 DeepSeek 官方价格（成本侧 pricing_rules）。
#
#   scripts/deepseek-official-pricing.sh            # 干跑：只打印将要写入的规则
#   scripts/deepseek-official-pricing.sh --apply    # 真正写入（需要管理员会话）
#
# 环境变量：GW_BASE（默认 http://127.0.0.1:8088）、GW_ADMIN_USER / GW_ADMIN_PASS
#          （默认取自 config.yaml 的 bootstrap.admin）。
#
# 价格来源（2026-09-11 抓取，中文页为元、英文页为美元，两页口径一致）：
#   https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
#
#   元/百万 tokens        缓存命中        缓存未命中      输出
#   Flash   空闲          0.02            1               4
#           高峰          0.04            2               8
#   V4-Pro  空闲          0.15            4.5             13.5
#           高峰          0.30            9.0             27.0
#
# 高峰时段 = 北京时间周一至周五 09:00-12:00、14:00-18:00。网关按 UTC 判定
# （billing.peak_boundary 默认 request_start），换算成 UTC 即 01:00-04:00 与 06:00-10:00，
# 与英文页 "(3) Peak hours are 01:00 - 04:00 and 06:00 - 10:00 UTC, Monday through Friday" 一致。
# 空闲价恰为高峰价的一半，所以规则只需两条：一条高峰窗口 + 一条 catch-all 空闲。
#
# 折算：网关单价单位是「微美分 / 百万单位」（pricing.RateScale = 1e6）。
# 官方按元计价，按 7.10 元/美元折算：rate = round(元 × 1e6 / 7.10)。
#
# 模型归属（官方脚注）：
#   deepseek-flash    —— 现行模型 V4.1-Flash，按 Flash 价。
#   deepseek-v4-flash —— 旧名，模型已下线，请求由 V4.1-Flash 服务，**按 Flash 价计费**。
#   deepseek-v4-pro   —— 当前仍按 Pro 价；官方公告 2026-09-14 12:00（北京时间，
#                        = 2026-09-14T04:00:00Z）起，v4-pro 请求全部路由到 V4.1-Flash
#                        并按 Flash 价计费。因此 Pro 规则带 valid_to，到点自动回落到
#                        下面的 Flash catch-all，无需人工改表。
#
# 实测核对（2026-09-11，运行中的 8088 实例，请求 deepseek-flash）：
#   usage_records#419 的 pricing_snapshot 命中 cost_rule=deepseek-peak，matched_windows
#   "mon,tue,wed,thu,fri 06:00-10:00 UTC"，逐维度成本 input_cache_hit 68608×5634=387、
#   input_cache_miss 251×281690=71、output 349×1126761=394，合计 852 微美分 = $0.000852，
#   与官方价一致（349 output tokens ÷ 1e6 × $1.2 = $0.0004188… 逐维度 ceil 相加）。
#
# 注意 `unpriced_dimensions: ["reasoning"]` 是**预期**的、不是漏配：DeepSeek 的
#   completion_tokens 已包含 reasoning_tokens（该次 349 output 含其中 48 reasoning），
#   所以思考 token 是经 `output` 维度按官方输出价计费的。给 `reasoning` 再写一个单价
#   会**重复计费**——docs/pricing.md §1 的"默认计入 output，可单列"正是这个含义。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GW_BASE="${GW_BASE:-http://127.0.0.1:8088}"
APPLY=0
[ "${1:-}" = "--apply" ] && APPLY=1

CONFIG="${CONFIG:-$ROOT/config.yaml}"
GW_ADMIN_USER="${GW_ADMIN_USER:-$(awk '/^  admin:/{f=1} f&&/username:/{print $2; exit}' "$CONFIG" | tr -d '"')}"
GW_ADMIN_PASS="${GW_ADMIN_PASS:-$(awk '/^  admin:/{f=1} f&&/password:/{print $2; exit}' "$CONFIG" | tr -d '"')}"
[ -n "$GW_ADMIN_USER" ] && [ -n "$GW_ADMIN_PASS" ] || { echo "缺少管理员凭据（GW_ADMIN_USER/GW_ADMIN_PASS）" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
COOKIE="$WORK/cookie.txt"

# 与规则一一对应的 Python 求值器：从元价算出微美分，保证脚本里的数字不是手抄的。
python3 - "$WORK/plan.json" <<'PY'
import json, sys

FX = 7.10  # 元/美元
def mc(yuan):
    """元/百万 tokens -> 微美分/百万 tokens（整数，四舍五入）"""
    return round(yuan * 1_000_000 / FX)

# 高峰：北京时间周一至周五 09:00-12:00 与 14:00-18:00 == UTC 01:00-04:00 与 06:00-10:00。
PEAK_WINDOWS = [
    {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "01:00", "end": "04:00", "tz": "UTC"},
    {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "06:00", "end": "10:00", "tz": "UTC"},
]
# V4-Pro 退役时刻：北京时间 2026-09-14 12:00 == 2026-09-14T04:00:00Z
PRO_RETIRE = "2026-09-14T04:00:00Z"

def rules(hit_off, hit_peak, miss_off, miss_peak, out_off, out_peak, pro=False):
    out = []
    if pro:
        out.append({
            "id": "deepseek-v4-pro-retire",
            "title": "V4-Pro 退役后回落 Flash 价（官方 2026-09-14 12:00 北京时间起路由到 V4.1-Flash）",
            "order": 5,
            "when": {"valid_from": PRO_RETIRE},
            "rates": {"input_cache_hit": mc(0.02), "input_cache_miss": mc(1), "output": mc(4)},
        })
    out.append({
        "id": "deepseek-peak",
        "title": "高峰时段（北京时间周一至周五 09:00-12:00 / 14:00-18:00）",
        "order": 10,
        "when": {"time_windows": PEAK_WINDOWS},
        "rates": {"input_cache_hit": mc(hit_peak), "input_cache_miss": mc(miss_peak), "output": mc(out_peak)},
    })
    out.append({
        "id": "deepseek-offpeak",
        "title": "空闲时段（catch-all，官方空闲价 = 高峰价的一半）",
        "order": 100,
        "when": {},
        "rates": {"input_cache_hit": mc(hit_off), "input_cache_miss": mc(miss_off), "output": mc(out_off)},
    })
    return {"rules": out}

flash = rules(0.02, 0.04, 1, 2, 4, 8)
pro = rules(0.15, 0.30, 4.5, 9.0, 13.5, 27.0, pro=True)
plan = {"deepseek-flash": flash, "deepseek-v4-flash": flash, "deepseek-v4-pro": pro}
json.dump(plan, open(sys.argv[1], "w"), ensure_ascii=False, indent=2)

print(f"汇率 {FX} 元/美元，费率单位：微美分/百万 tokens")
for name in ("deepseek-flash", "deepseek-v4-pro"):
    for rule in plan[name]["rules"]:
        r = rule["rates"]
        print(f"  {name:18s} {rule['id']:24s} hit={r['input_cache_hit']:>9,d} "
              f"miss={r['input_cache_miss']:>9,d} out={r['output']:>9,d}")

# 自检：官方英文页同一张表是美元价（1e6 微美分 = $1）。用元价折出来的费率应当只在
# 「汇率」这一项上与它成比例；差得离谱说明抄错了数或官方改了表。
USD_PAGE = {  # 官方英文页，$/百万 tokens：(缓存命中, 缓存未命中, 输出)
    "deepseek-flash":    {"offpeak": (0.003, 0.15, 0.6),  "peak": (0.006, 0.3, 1.2)},
    "deepseek-v4-flash": {"offpeak": (0.003, 0.15, 0.6),  "peak": (0.006, 0.3, 1.2)},
    "deepseek-v4-pro":   {"offpeak": (0.022, 0.66, 1.98), "peak": (0.044, 1.32, 3.96)},
}
print()
print("自检：折算后的费率 / 官方英文页美元价（应恒等于 7.0/FX，两页汇率口径都是 7.0）")
dims = ("input_cache_hit", "input_cache_miss", "output")
for name, ruleset in plan.items():
    for rule in ruleset["rules"]:
        # 注意：不能用 endswith("peak") 判断——"deepseek-offpeak" 也以 peak 结尾。
        if rule["id"] == "deepseek-v4-pro-retire":
            ref, label = USD_PAGE["deepseek-flash"]["offpeak"], "Flash 表"
        else:
            tag = {"deepseek-peak": "peak", "deepseek-offpeak": "offpeak"}[rule["id"]]
            ref, label = USD_PAGE[name][tag], "本模型表"
        ratios = [rule["rates"][d] / (usd * 1_000_000) for d, usd in zip(dims, ref)]
        print(f"  {name:18s} {rule['id']:24s} = {label} × {min(ratios):.4f}~{max(ratios):.4f}"
              f"   (期望 {7.0/FX:.4f})")
        if max(ratios) > 1.15 or min(ratios) < 0.85:
            raise SystemExit(f"自检失败：{name}/{rule['id']} 与官方美元价差异超过 15%，先核对价格表")
PY

if [ "$APPLY" != "1" ]; then
  echo
  echo "干跑结束（未写入）。加 --apply 写入 $GW_BASE。"
  exit 0
fi

code="$(curl -s -m 10 -c "$COOKIE" -o "$WORK/login.json" -w '%{http_code}' \
  -X POST "$GW_BASE/admin/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$GW_ADMIN_USER" "$GW_ADMIN_PASS")")"
[ "$code" = "200" ] || { echo "登录失败 HTTP $code: $(cat "$WORK/login.json")" >&2; exit 1; }
echo "已登录 $GW_BASE ($GW_ADMIN_USER)"

# 供应商 id 按名字查，不写死。
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers" -o "$WORK/providers.json"
PROVIDER_ID="$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for p in d["data"]:
    if p["name"]=="deepseek": print(p["id"]); break
else: raise SystemExit("providers 列表里没有 deepseek")
' "$WORK/providers.json")"
echo "deepseek provider id=$PROVIDER_ID"

curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models" -o "$WORK/existing.json"

# 关键：POST /providers/{id}/models 过去是整行覆盖（UPSERT 会把未提供的字段重置为默认值，
# enabled 默认 true、priority/weight 默认 100）；M28 起改成部分更新——省缺的字段保持原值、
# 写 null 才清空。脚本仍然取回当前行再合并提交：对旧版实例同样成立，且提交内容自解释。
python3 - "$WORK/existing.json" "$WORK/plan.json" "$WORK/bodies" <<'PY'
import json, sys, os
existing = {m["public_model"]: m for m in json.load(open(sys.argv[1]))["data"]}
plan = json.load(open(sys.argv[2]))
os.makedirs(sys.argv[3], exist_ok=True)
for name, rules in plan.items():
    cur = existing.get(name)
    if cur is None:
        raise SystemExit(f"provider 上没有这个模型映射：{name}")
    body = {
        # 原样保留，避免整行覆盖把运行态参数打回默认值
        "public_model": name,
        "upstream_model": cur["upstream_model"],
        "enabled": cur["enabled"],
        "priority": cur["priority"],
        "weight": cur["weight"],
        "context_window": cur["context_window"],
        "max_output_tokens": cur["max_output_tokens"],
        "capabilities": cur["capabilities"],
        "pricing_rules": rules,
    }
    if cur.get("capabilities_override"):
        body["capabilities_override"] = cur["capabilities_override"]
    json.dump(body, open(os.path.join(sys.argv[3], name + ".json"), "w"), ensure_ascii=False)
PY

for name in deepseek-flash deepseek-v4-flash deepseek-v4-pro; do
  code="$(curl -s -m 10 -b "$COOKIE" -o "$WORK/out.json" -w '%{http_code}' \
    -X POST "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models" \
    -H 'Content-Type: application/json' --data-binary "@$WORK/bodies/$name.json")"
  if [ "$code" = "200" ]; then
    echo "  ok   $name (HTTP 200)"
  else
    echo "  FAIL $name (HTTP $code): $(cat "$WORK/out.json")" >&2
    exit 1
  fi
done

echo
echo "写入完成。下一步核对：读回规则并做价格试算。"
echo "  读回：curl -s -b <cookie> $GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models"
echo "  试算：POST $GW_BASE/admin/api/v1/pricing/simulate"
