#!/usr/bin/env bash
# 给运行中的 ai-gateway 实例写入 codex 供应商（OpenAI 第一方）官方价格（成本侧 pricing_rules）。
#
#   scripts/codex-official-pricing.sh            # 干跑：只打印将要写入的规则
#   scripts/codex-official-pricing.sh --apply    # 真正写入（需要管理员会话）
#
# 环境变量：GW_BASE（默认 http://127.0.0.1:8088）、GW_ADMIN_USER / GW_ADMIN_PASS
#          （默认取自 config.yaml 的 bootstrap.admin）。
#
# 价格来源（USD / 百万 tokens，官方 Standard 档）：
#
#   GPT-6 Astra（2026-09-04 上线，模型 id gpt-6-astra）
#       输入 $10 · 缓存命中 $1 · 缓存写入 $12.50 · 输出 $50
#       输入 >272K 时**整个请求**按输入与缓存 2×、输出 1.5× 计（长上下文档位）
#       https://llm-stats.com/blog/research/gpt-6-astra-launch
#
#   GPT-5.6 Luna（2026-07-30 降价 80% 后，模型 id gpt-5.6-luna）
#       输入 $0.20 · 输出 $1.20 · 缓存命中 = 输入价 1 折（$0.02）
#       https://www.edenai.co/post/openai-cuts-gpt-5-6-api-prices-luna-falls-80-terra-20-sol-holds
#
# 折算：网关费率单位是「微单位 / 百万 tokens」（pricing.RateScale = 1e6），规则集的
# `currency` 写 USD 时，$X/百万 tokens == X × 1e6 微美元，整数、无浮点。
# 账本币种就是 USD（config.yaml 的 billing.currency），所以不需要汇率表条目。
#
# 两个刻意的口径说明：
#   1. **缓存写入（cache write）收不到**：官方对 cache write 另收 1.25× 输入价
#      （Astra $12.50、Luna $0.25），但计量维度里没有 cache_write（只有
#      input_cache_hit / input_cache_miss / output / reasoning）。这里不编造一个
#      per_request_fee 去凑——那会把费用摊到所有请求上。要精确还原需先给
#      internal/pricing 增加维度（docs/pricing.md §1）。
#   2. 这两个模型走的是 **ChatGPT 订阅后端**（examples/provider-codex），不是按量
#      API：这份规则是影子成本，用于毛利核算与路由比较，不代表真实账单金额。
#      订阅账号实际授权范围与限流另见 config.yaml 里 codex 段的注释。
#
# 为什么每条规则里除了 input_cache_hit / input_cache_miss 还要写一个裸 `input`：
#   codex 插件在上游没给 cached_tokens 明细时上报的是**裸 `input` 维度**
#   （examples/provider-codex/main.go:1268 的 cached==0 分支）。
#
#   计价引擎现在自己会兜底：`input` 没有自有费率时按 `input_cache_miss` 计价，并标
#   usage_dimensions_incomplete=true、把 `input->input_cache_miss` 记进快照的
#   bucketed_dimensions（internal/pricing/engine.go 的 dimensionFallbacks，
#   见 docs/pricing.md §1）。所以**不写**裸 `input` 也已经能正确计费。
#   修复（2026-09-12）之前不行：引擎只按维度名精确查表，查不到的维度只记进
#   unpriced_dimensions 并按 0 计费——实测 usage_records#2855 dimensions=
#   {"input":13,"output":5} → cost_lines 只有 output，13 个输入 token 记成 0。
#
#   本脚本仍然**显式写出**裸 `input`，两个理由：
#     1. 费率表自解释，不依赖引擎的隐式约定；回滚到旧二进制时也仍然计费，
#        不会因为旧引擎精确查表而把整段 prompt 记成 0；
#     2. 显式费率优先于兜底，**含显式 0**——想表达"这个维度免费"必须显式写 0，
#        不会被兜底改写。
#   上游真的给了明细时维度名是 hit/miss，按名字精确命中，插件的分支互斥，不会重复计费。
#
# 与 scripts/deepseek-official-pricing.sh 同样的两个坑：
#   * POST /providers/{id}/models 过去是**整行覆盖**（UPSERT 把未提供的字段重置为默认值，
#     capabilities/上下文会被打回默认）；M28 起后端改成部分更新——**省缺的字段保持原值**、
#     写 null 才清空——控制台定价页就是踩了这条才把 capabilities 清掉的。
#     脚本仍然显式读回整行再合并提交：对旧版实例同样成立，且提交内容自解释。
#   * 供应商上没有该模型的映射时（路由存在也没用，见下），脚本会按同供应商的兄弟模型
#     补建这一行——"模型 + 路由都在、却漏了 provider model"正是 gpt-6-astra 当初
#     /v1/models 里看不到的原因（internal/routing/routing.go 的 filterRoute 判 not_mapped）。
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

# 与规则一一对应的 Python 求值器：从官方美元价算出费率，保证脚本里的数字不是手抄的。
python3 - "$WORK/plan.json" <<'PY'
import json, sys

USD = 1_000_000  # 微美元 / 百万 tokens == $1 / 百万 tokens
def mc(usd_per_mtok):
    """美元/百万 tokens -> 微美元/百万 tokens（整数）"""
    return round(usd_per_mtok * USD)

# 官方标准价（USD / 百万 tokens）
ASTRA_IN, ASTRA_HIT, ASTRA_OUT = 10.0, 1.0, 50.0
ASTRA_LONG_GTE = 272_000          # 官方：>272K 输入进入长上下文档位
ASTRA_LONG_IN_MULT, ASTRA_LONG_OUT_MULT = 2.0, 1.5
LUNA_IN, LUNA_HIT, LUNA_OUT = 0.20, 0.02, 1.20

def rates(miss_usd, hit_usd, out_usd):
    """一条规则的三档费率。裸 `input` 显式按未命中价，理由见文件头。"""
    miss = mc(miss_usd)
    return {
        "input": miss,                 # 显式兜底；引擎也会把无费率的 input 按 miss 计
        "input_cache_hit": mc(hit_usd),
        "input_cache_miss": miss,
        "output": mc(out_usd),
    }

astra = {"currency": "USD", "rules": [
    {
        "id": "openai-astra-long-context",
        "title": f"输入 ≥{ASTRA_LONG_GTE // 1000}K：整请求按输入/缓存 {ASTRA_LONG_IN_MULT:g}×、输出 {ASTRA_LONG_OUT_MULT:g}×",
        "order": 10,
        "when": {"tier": {"basis": "input", "gte": ASTRA_LONG_GTE}},
        "rates": rates(ASTRA_IN * ASTRA_LONG_IN_MULT, ASTRA_HIT * ASTRA_LONG_IN_MULT, ASTRA_OUT * ASTRA_LONG_OUT_MULT),
    },
    {
        "id": "openai-astra-standard",
        "title": f"官方标准价 ${ASTRA_IN:g} 输入 / ${ASTRA_HIT:g} 缓存命中 / ${ASTRA_OUT:g} 输出（每百万 tokens）",
        "order": 100,
        "when": {},
        "rates": rates(ASTRA_IN, ASTRA_HIT, ASTRA_OUT),
    },
]}

luna = {"currency": "USD", "rules": [
    {
        "id": "openai-luna-standard",
        "title": f"官方 2026-07-30 降价后 ${LUNA_IN:g} 输入 / ${LUNA_HIT:g} 缓存命中 / ${LUNA_OUT:g} 输出（每百万 tokens）",
        "order": 100,
        "when": {},
        "rates": rates(LUNA_IN, LUNA_HIT, LUNA_OUT),
    },
]}

plan = {"gpt-6-astra": astra, "gpt-5.6-luna": luna}
json.dump(plan, open(sys.argv[1], "w"), ensure_ascii=False, indent=2)

print("费率单位：微美元 / 百万 tokens（1e6 = $1 / 百万 tokens）")
for name, ruleset in plan.items():
    for rule in ruleset["rules"]:
        r = rule["rates"]
        tier = rule["when"].get("tier")
        band = f"input>={tier['gte']}" if tier else "catch-all"
        print(f"  {name:14s} {rule['id']:26s} {band:14s} "
              f"miss={r['input_cache_miss']:>10,d} hit={r['input_cache_hit']:>9,d} "
              f"input={r['input']:>10,d} out={r['output']:>10,d}")

# 自检：Astra 长上下文档位必须恰好是标准档位的 2×/2×/1.5×，Luna 命中价必须是输入价的 1 折，
# 且每条规则的裸 input 必须等于未命中价（否则等于给上游没报缓存的请求打折）。
a = astra["rules"][0]["rates"]; s = astra["rules"][1]["rates"]
assert (a["input_cache_miss"], a["input_cache_hit"], a["output"]) == \
       (s["input_cache_miss"] * 2, s["input_cache_hit"] * 2, s["output"] * 3 // 2), "Astra 长上下文档位倍数不对"
assert (astra["rules"][1]["rates"]["input_cache_miss"], astra["rules"][1]["rates"]["output"]) == (mc(10), mc(50)), "Astra 标准价不对"
assert luna["rules"][0]["rates"]["input_cache_hit"] * 10 == luna["rules"][0]["rates"]["input_cache_miss"], "Luna 缓存命中价不是输入价的 1 折"
assert (luna["rules"][0]["rates"]["input_cache_miss"], luna["rules"][0]["rates"]["output"]) == (mc(0.20), mc(1.20)), "Luna 标准价不对"
for ruleset in plan.values():
    for rule in ruleset["rules"]:
        assert rule["rates"]["input"] == rule["rates"]["input_cache_miss"], \
            f"{rule['id']}：裸 input 必须显式按未命中价，不能比 miss 便宜"
print("\n自检通过：档位倍数、官方贴价与裸 input 兜底一致。")
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
    if p["name"]=="codex": print(p["id"]); break
else: raise SystemExit("providers 列表里没有 codex")
' "$WORK/providers.json")"
echo "codex provider id=$PROVIDER_ID"

curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$PROVIDER_ID/models" -o "$WORK/existing.json"

python3 - "$WORK/existing.json" "$WORK/plan.json" "$WORK/bodies" <<'PY'
import json, sys, os
existing = {m["public_model"]: m for m in json.load(open(sys.argv[1]))["data"]}
plan = json.load(open(sys.argv[2]))
outdir = sys.argv[3]
os.makedirs(outdir, exist_ok=True)
# 新建缺失映射时的模板：取同供应商的兄弟模型，避免凭空猜 capabilities/上下文。
sibling = existing.get("gpt-5.6-luna") or (next(iter(existing.values())) if existing else None)
for name, rules in plan.items():
    cur = existing.get(name)
    if cur is None:
        if sibling is None:
            raise SystemExit(f"provider 上既没有 {name}，也没有可参照的兄弟模型，请先在控制台加一行")
        print(f"  注意：provider 上没有 {name}，将按 {sibling['public_model']} 的元数据补建这一行")
        cur = {
            "upstream_model": name,
            "enabled": True,
            "priority": sibling.get("priority", 100),
            "weight": sibling.get("weight", 100),
            "context_window": sibling.get("context_window", 0),
            "max_output_tokens": sibling.get("max_output_tokens", 0),
            "capabilities": sibling.get("capabilities") or {},
            "capabilities_override": sibling.get("capabilities_override", ""),
        }
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
    json.dump(body, open(os.path.join(outdir, name + ".json"), "w"), ensure_ascii=False)
PY

for name in gpt-6-astra gpt-5.6-luna; do
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
echo "        标准档 {\"model\":\"gpt-6-astra\",\"dimensions\":{\"input_cache_miss\":1000000,\"output\":1000000}}"
echo "        长文档 {\"model\":\"gpt-6-astra\",\"dimensions\":{\"input_cache_miss\":300000,\"output\":1000000}}"
