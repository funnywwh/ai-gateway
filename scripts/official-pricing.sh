#!/usr/bin/env bash
# 给运行中的 ai-gateway 实例写入**官方价格**（成本侧 pricing_rules），逐个覆盖该实例上
# **每一个供应商模型**（provider_models 行）。
#
#   scripts/official-pricing.sh                      # 干跑：登录（只读）→ 列目录 → 逐行取价 → 打印计划
#   scripts/official-pricing.sh --apply              # 真正写入（并在写完后读回核对、试算、体检）
#   GW_BASE=https://gpt.lagenio.xyz/aigw scripts/official-pricing.sh --apply
#
# 环境变量：GW_BASE（默认 http://127.0.0.1:8088；实例带 base_path 时要把前缀写进去，
#          例如 gptjp 的 /aigw）、GW_ADMIN_USER / GW_ADMIN_PASS
#          （默认取自 config.yaml 的 bootstrap.admin；gptjp 的 config 里没有 bootstrap，
#           密码在 /opt/aigw/.admin-password，用环境变量传）。
#
# 与 scripts/deepseek-official-pricing.sh、scripts/codex-official-pricing.sh 的关系：
# 那两个脚本各自写死一家供应商、一份价格表，只覆盖它当时知道的两个模型 id。本脚本是
# 它们的**并集 + 补齐**：价格按「上游模型 id」组织（见下面的取价口径），先把实例上所有
# 供应商模型列出来再逐行取价，取不到价的行走显式白名单并在写入前整体报告。
#
# ── 取价口径（三条，都是刻意的）──────────────────────────────────────────────
#
# 1. **按上游模型取价，不按 public 名取价。** 成本侧规则描述的是「我方付上游多少钱」，
#    而同一个 public 名在不同供应商上可以指向不同上游：gptjp 上 `deepseek-v4-flash` 在
#    三家 codex 供应商指向 `gpt-5.6-luna`、在 deepseek 供应商指向 `deepseek-flash`。
#    按 public 名取价必然给其中一家记错成本，所以规则跟着 `upstream_model` 走。
#    同理，DeepSeek 官方公告说旧名 `deepseek-v4-flash` 的请求由 V4.1-Flash 提供并按 Flash
#    价计费——上游是 flash，价格就是 flash。
#
# 2. **币种 USD，直接写微美元。** OpenAI 与 DeepSeek 的官方英文页都以美元报价
#    （DeepSeek 中文页是元，两页口径一致、汇率 7.0），而运行实例的账本币种是 USD 且
#    `billing.fx_rates` 常为空表——写 CNY 规则集会被写时校验拒掉（汇率表里没有该币种）。
#    直接采用官方美元价既忠实又不需要动汇率表。USD 规则集的费率单位就是微美元/百万 token：
#    $0.20/百万 == 200000。
#
# 3. **没有官方价的 id 不许猜。** 供应商目录是可变的（控制台能加模型、插件能同步目录），
#    而猜出来的单价会静默变成账单。所以脚本把「没有官方价」写成一个**显式白名单**
#    （下面的 UNPRICED，附理由与最接近的真实 id），名单外的未知 upstream 一律在写入前
#    报错退出；名单内的行保持原样、明确列为「未定价」，不会写一堆 0 进去冒充有价。
#
# ── 已知的近似（不是遗漏）────────────────────────────────────────────────────
#
# * **只有五个计量维度**：input / input_cache_hit / input_cache_miss / output / reasoning
#   （internal/httpapi/admin_pricing_schema.go、docs/pricing.md §1）。官方对图像模型按
#   「文本输入 / 图像输入 / 图像输出」三种 token 分别计价、对音频模型按「文本 / 音频」两档
#   计价，而网关没有 `image_*` / `audio_*` 维度（docs/pricing.md §1 的「扩展」行至今没有
#   采集方）。本脚本的处理写在每条规则的 title 里，控制台上看得见：
#     - 图像模型：output 取**图像输出**价（图像请求的主成本），input 取**文本输入**价
#       （提示词必然存在；reference image 的输入 token 无法区分，只能按文本价计，这是本
#       脚本唯一一处**低估**）。
#     - 音频模型：input/output 一律取**音频**价（这两个模型的典型用法就是音频进出；
#       文本档费率写进 title 备查）。这是刻意的保守方向：少计成本会变成超卖。
# * **缓存写入（cache write）收不到**：官方对 cache write 另收 1.25× 输入价，但计量维度里
#   没有 cache_write。不编造 per_request_fee 去凑——那会把费用摊到所有请求上。
# * **不写 `reasoning` 费率**：codex 插件把 reasoning 单列并从 output 里减掉
#   （examples/provider-codex/main.go 的 usageFromWire），计价引擎在规则没有 `reasoning`
#   费率时按 `output` 计价（internal/pricing/engine.go 的 dimensionFallbacks）。再写一条
#   反而会重复计费；DeepSeek 那边 reasoning 本来就含在 output 里。
#
# ── 价格来源（2026-09-14 抓取）──────────────────────────────────────────────
#
# 本机出网访问 openai.com / developers.openai.com / platform.openai.com 一律被 Cloudflare
# 403，但 **gptjp（8.211.157.165）能取到官方定价页**，所以下面的 OpenAI 数字是**从官方页面
# 的定价表里逐条读出来的**（页面把每张表的 JSON 内嵌在 Astro island 的 props 属性里），
# 不是二手转述。复核命令见文件末尾。
#
#   https://developers.openai.com/api/docs/pricing （Standard 档，USD / 百万 tokens）
#     模型                      输入   缓存命中  缓存写入  输出    |  >272K 输入：输入/缓存 2×、输出 1.5×
#     gpt-6-astra               10      1       12.5     50     |  20 / 2 / 25 / 75
#     gpt-5.6-sol                4      0.4      5       20     |   8 / 0.8 / 10 / 30
#     gpt-5.6-terra              2      0.2      2.5     12     |   4 / 0.4 / 5 / 18
#     gpt-5.6-luna               0.2    0.02     0.25     1.2   |   0.4 / 0.04 / 0.5 / 1.8
#     gpt-5.5 (<272K context length)   5      0.5      –      30
#     gpt-5.4 (<272K context length)   2.5    0.25     –      15
#     * 长上下文档表头的 tooltip 是「>272K input tokens」，官方只给 astra 与 5.6 三兄弟列了
#       这一档。**gpt-5.5 / gpt-5.4 的官方行明确写着 "(<272K context length)"，但定价页没有
#       给出它们 >272K 的费率**（第三方登记表 LiteLLM 声称 2×/1.5×，官方页里查无此数），
#       所以本脚本不给这两个模型写长上下文规则——宁可少一条，也不编一条。
#     图像（同一张表按 Modality 分行）：
#       gpt-image-1    Image 10 / 2.5 / 40    Text 5 / 1.25 / –
#       gpt-image-1.5  Image  8 / 2   / 32    Text 5 / 1.25 / 10
#       gpt-image-2    Image  8 / 2   / 30    Text 5 / 1.25 / –
#       gpt-image-2.5-flare / gpt-image-2.5-sunburst   同 gpt-image-2
#     ⚠ 官方**没有**裸 `gpt-image-2.5` 这个 id（2.5 只有 -flare 与 -sunburst），gptjp 上那
#       一行恰恰用的是裸 id；两者费率相同，脚本按 2.5 家族价写并在 title 里标注这一点。
#     音频：官方当前列的是 gpt-audio / gpt-realtime 一族；**gpt-4o-audio-preview 与
#       gpt-4o-realtime-preview 已经不在定价页上**（GPT-4o 于 2026-07 移除）。这两个 id 的
#       费率取自 https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
#       与 https://modelcosts.com/provider/openai/model/gpt-4o-audio-preview 一致的记录
#       （都是它们在线时的官方价）：
#         gpt-4o-audio-preview     Text 2.5 / – / 10      Audio 40 / – / 80
#         gpt-4o-realtime-preview  Text 5 / 2.5 / 20      Audio 40 / – / 80
#
#   https://api-docs.deepseek.com/quick_start/pricing （英文页，USD；中文页按元，汇率 7.0）
#     deepseek-flash（模型版本 V4.1-Flash；旧名 deepseek-v4-flash 由它服务、按 Flash 价）
#         缓存命中 $0.003 空闲 / $0.006 高峰；未命中 $0.15 / $0.30；输出 $0.60 / $1.20
#     deepseek-v4-pro（V4-Pro-0813）
#         缓存命中 $0.022 / $0.044；未命中 $0.66 / $1.32；输出 $1.98 / $3.96
#     高峰 = 北京时间周一至周五 09:00-12:00、14:00-18:00 == UTC 01:00-04:00、06:00-10:00。
#     官方 2026-09-14 公告：**V4 Pro 继续提供、计费不变**——所以本脚本不写
#     deepseek-official-pricing.sh 里那条 `deepseek-v4-pro-retire` 退役规则（它按
#     「9/14 12:00 起回落 Flash 价」写，已被官方公告推翻）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GW_BASE="${GW_BASE:-http://127.0.0.1:8088}"
APPLY=0
[ "${1:-}" = "--apply" ] && APPLY=1

CONFIG="${CONFIG:-$ROOT/config.yaml}"
GW_ADMIN_USER="${GW_ADMIN_USER:-$(awk '/^  admin:/{f=1} f&&/username:/{print $2; exit}' "$CONFIG" 2>/dev/null | tr -d '"')}"
GW_ADMIN_PASS="${GW_ADMIN_PASS:-$(awk '/^  admin:/{f=1} f&&/password:/{print $2; exit}' "$CONFIG" 2>/dev/null | tr -d '"')}"
[ -n "$GW_ADMIN_USER" ] && [ -n "$GW_ADMIN_PASS" ] || { echo "缺少管理员凭据（GW_ADMIN_USER/GW_ADMIN_PASS）" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
COOKIE="$WORK/cookie.txt"
PLAN="$WORK/plan.json"

# ── 价格表：上游模型 id -> 规则集 ─────────────────────────────────────────────
# 数字全部由官方报价算出（禁止手抄费率）：usd() 把「美元/百万 token」变成微美元/百万 token。
python3 - "$PLAN" <<'PY'
import json, sys

USD = 1_000_000  # 微美元 / 百万 token == $1 / 百万 token

def usd(per_mtok):
    """美元/百万 token -> 微美元/百万 token（整数）"""
    return round(per_mtok * USD)

def rates(miss, hit, out):
    """一条规则的四档费率。

    裸 `input` 是**显式兜底**：codex 插件在上游没给 cached_tokens 明细时只上报 `input`
    （examples/provider-codex/main.go 的 usageFromWire 循环首分支）。计价引擎自己也会把
    无费率的 `input` 按 `input_cache_miss` 计价（internal/pricing/engine.go 的
    dimensionFallbacks），显式写出来是为了费率表自解释、且回滚到旧二进制时仍然计费。
    """
    m = usd(miss)
    return {"input": m, "input_cache_hit": usd(hit), "input_cache_miss": m, "output": usd(out)}

def long_context_rule(model_id, miss, hit, out):
    """官方「长上下文」规则：>272K 输入时整个请求输入/缓存 2×、输出 1.5×。

    边界：官方表头的 tooltip 是「>272K input tokens」（严格大于），而网关的 tier 是半开区间
    [gte, lt)，所以 gte 取 272001 才**精确**表达「大于 272000」——写 272000 会把恰好落在
    272000 的请求也算进长档。codex-official-pricing.sh 里那条旧规则用的是 272000（相差一个
    token 的边界），本脚本以官方 tooltip 为准。
    """
    return {
        "id": model_id + "-long-context",
        "title": "输入 >272K：整请求按输入/缓存 2×、输出 1.5×（官方长上下文档位）",
        "order": 10,
        "when": {"tier": {"basis": "input", "gte": 272_001}},
        "rates": rates(miss * 2, hit * 2, out * 1.5),
    }

def openai_text(model_id, title, miss, hit, out, long_ok=True):
    """OpenAI 文本模型：标准档 catch-all，官方列了长上下文档位的再加一条。"""
    rules = []
    if long_ok:
        rules.append(long_context_rule(model_id, miss, hit, out))
    rules.append({"id": model_id + "-standard", "title": title, "order": 100, "when": {},
                  "rates": rates(miss, hit, out)})
    return {model_id: {"currency": "USD", "rules": rules}}

plan = {}

# ── DeepSeek：官方分时价（高峰 / 空闲），两条规则 ─────────────────────────────
# 高峰时段按 UTC 判定（billing.peak_boundary 默认 request_start），与英文页
# "Peak hours are 01:00 - 04:00 and 06:00 - 10:00 UTC, Monday through Friday" 一致。
# 空闲价恰为高峰价的一半，所以只需要「一条高峰窗口 + 一条 catch-all 空闲」。
DEEPSEEK_PEAK = [
    {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "01:00", "end": "04:00", "tz": "UTC"},
    {"days": ["mon", "tue", "wed", "thu", "fri"], "start": "06:00", "end": "10:00", "tz": "UTC"},
]

def deepseek(model_id, title, hit_off, hit_peak, miss_off, miss_peak, out_off, out_peak):
    return {model_id: {"currency": "USD", "rules": [
        {"id": model_id + "-peak",
         "title": title + "·高峰时段（UTC 周一至周五 01:00-04:00 / 06:00-10:00）",
         "order": 10, "when": {"time_windows": DEEPSEEK_PEAK},
         "rates": rates(miss_peak, hit_peak, out_peak)},
        {"id": model_id + "-offpeak",
         "title": title + "·空闲时段（catch-all，官方空闲价 = 高峰价的一半）",
         "order": 100, "when": {}, "rates": rates(miss_off, hit_off, out_off)},
    ]}}

plan.update(deepseek(
    "deepseek-flash", "DeepSeek 官方价 $0.15 未命中 / $0.003 缓存命中 / $0.60 输出（每百万 tokens）",
    0.003, 0.006, 0.15, 0.30, 0.60, 1.20))
plan.update(deepseek(
    "deepseek-v4-pro", "DeepSeek 官方价 $0.66 未命中 / $0.022 缓存命中 / $1.98 输出（每百万 tokens）",
    0.022, 0.044, 0.66, 1.32, 1.98, 3.96))

# ── OpenAI 文本模型：官方 Standard 档；astra 与 5.6 三兄弟另有长上下文档 ───────
plan.update(openai_text("gpt-6-astra",
    "OpenAI 官方标准价 $10 输入 / $1 缓存命中 / $12.50 缓存写入 / $50 输出（每百万 tokens）",
    10.00, 1.00, 50.00))
plan.update(openai_text("gpt-5.6-sol",
    "OpenAI 官方标准价 $4 输入 / $0.40 缓存命中 / $5 缓存写入 / $20 输出（每百万 tokens；促销价至少到 2026-11-21）",
    4.00, 0.40, 20.00))
plan.update(openai_text("gpt-5.6-terra",
    "OpenAI 官方标准价 $2 输入 / $0.20 缓存命中 / $2.50 缓存写入 / $12 输出（每百万 tokens）",
    2.00, 0.20, 12.00))
plan.update(openai_text("gpt-5.6-luna",
    "OpenAI 官方标准价 $0.20 输入 / $0.02 缓存命中 / $0.25 缓存写入 / $1.20 输出（每百万 tokens，2026-07-30 降价后）",
    0.20, 0.02, 1.20))
# 官方行标注 "(<272K context length)"，但没有给出 >272K 的费率（见文件头），所以长档留空。
plan.update(openai_text("gpt-5.5",
    "OpenAI 官方标准价 $5 输入 / $0.50 缓存命中 / $30 输出（每百万 tokens，<272K 上下文档；"
    "官方未公布 >272K 的费率，故本规则集没有长上下文档）",
    5.00, 0.50, 30.00, long_ok=False))
plan.update(openai_text("gpt-5.4",
    "OpenAI 官方标准价 $2.50 输入 / $0.25 缓存命中 / $15 输出（每百万 tokens，<272K 上下文档；"
    "官方未公布 >272K 的费率，故本规则集没有长上下文档）",
    2.50, 0.25, 15.00, long_ok=False))

# ── 图像模型：output 按图像输出价，input 按文本输入价 ─────────────────────────
def image_model(model_id, text_in, cached_text_in, image_in, cached_image_in, image_out, note=""):
    r = {"input": usd(text_in), "input_cache_hit": usd(cached_text_in),
         "input_cache_miss": usd(text_in), "output": usd(image_out)}
    title = (f"OpenAI 官方标准价：文本输入 ${text_in:g} / 缓存 ${cached_text_in:g} / "
             f"图像输出 ${image_out:g}（每百万 tokens）；官方另有图像输入 ${image_in:g}"
             f"（缓存 ${cached_image_in:g}），但网关没有图像 token 维度，reference image 的输入 "
             f"token 只能按文本输入价计")
    if note:
        title += "；" + note
    return {model_id: {"currency": "USD", "rules": [
        {"id": model_id + "-standard", "title": title, "order": 100, "when": {}, "rates": r},
    ]}}

plan.update(image_model("gpt-image-1", 5.00, 1.25, 10.00, 2.50, 40.00))
plan.update(image_model("gpt-image-1.5", 5.00, 1.25, 8.00, 2.00, 32.00))
plan.update(image_model("gpt-image-2", 5.00, 1.25, 8.00, 2.00, 30.00))
plan.update(image_model("gpt-image-2.5", 5.00, 1.25, 8.00, 2.00, 30.00,
    note="官方的 2.5 只有 gpt-image-2.5-flare 与 gpt-image-2.5-sunburst 两个 id，"
         "裸 `gpt-image-2.5` 不是官方 id，此处按 2.5 家族价（与 gpt-image-2 同价）计"))

# ── 音频模型：input/output 一律按音频档（这两个模型的典型用法就是音频进出）────
def audio_model(model_id, text_in, text_out, audio_in, audio_out, cached_note):
    r = {"input": usd(audio_in), "input_cache_hit": usd(audio_in),
         "input_cache_miss": usd(audio_in), "output": usd(audio_out)}
    return {model_id: {"currency": "USD", "rules": [
        {"id": model_id + "-audio",
         "title": (f"官方价：音频输入 ${audio_in:g} / 音频输出 ${audio_out:g}（每百万 tokens）；"
                   f"文本档 ${text_in:g} 输入 / ${text_out:g} 输出{cached_note}。"
                   f"网关没有音频 token 维度，input/output 一律按音频档计（保守方向）；"
                   f"该模型已从官方定价页移除，费率取自 LiteLLM 与 ModelCosts 一致的记录"),
         "order": 100, "when": {}, "rates": r},
    ]}}

plan.update(audio_model("gpt-4o-audio-preview", 2.50, 10.00, 40.00, 80.00, "（缓存命中档官方未列）"))
plan.update(audio_model("gpt-4o-realtime-preview", 5.00, 20.00, 40.00, 80.00, "，缓存命中 $2.50"))

json.dump(plan, open(sys.argv[1], "w"), ensure_ascii=False, indent=2)

print(f"价格表：{len(plan)} 个上游模型（费率单位：微美元 / 百万 token，1e6 = $1）")
for name in sorted(plan):
    for rule in plan[name]["rules"]:
        r = rule["rates"]
        tier = rule["when"].get("tier")
        band = f"input>{tier['gte'] - 1}" if tier else ("peak" if "time_windows" in rule["when"] else "catch-all")
        print(f"  {name:24s} {rule['id']:28s} {band:14s} "
              f"miss={r['input_cache_miss']:>9,d} hit={r['input_cache_hit']:>8,d} out={r['output']:>10,d}")

# 自检：把结构约束与官方关系写成断言，避免手抄错一位数却看不出来。
for name, ruleset in plan.items():
    assert ruleset["currency"] == "USD", name
    assert any(rule["when"] == {} for rule in ruleset["rules"]), f"{name}：缺少 catch-all 规则"
    for rule in ruleset["rules"]:
        r = rule["rates"]
        assert r["input"] == r["input_cache_miss"], f"{name}/{rule['id']}：裸 input 必须显式按未命中价"
        assert r["input_cache_miss"] >= r["input_cache_hit"] >= 0, f"{name}/{rule['id']}：缓存命中价不该高于未命中价"

# 长上下文档：官方口径是输入/缓存 2×、输出 1.5×，且边界必须是「>272K」。
LONG = ("gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna")
for name in LONG:
    rules = plan[name]["rules"]
    assert len(rules) == 2, f"{name}：长上下文档缺失"
    long_rule, std = rules[0], rules[1]
    assert long_rule["when"]["tier"] == {"basis": "input", "gte": 272_001}, f"{name}：长上下文边界不是 >272K"
    assert long_rule["order"] < std["order"], f"{name}：长档必须先于标准档求值"
    for dim in ("input_cache_miss", "input_cache_hit"):
        assert long_rule["rates"][dim] == std["rates"][dim] * 2, f"{name}/{dim}：长档不是 2×"
    assert long_rule["rates"]["output"] == std["rates"]["output"] * 3 // 2, f"{name}/output：长档不是 1.5×"

# 没有官方长档费率的模型，不许偷偷有条长档规则。
for name in ("gpt-5.5", "gpt-5.4"):
    assert len(plan[name]["rules"]) == 1, f"{name}：官方没有 >272K 费率，不该有长上下文档"

# 官方缓存命中价 = 输入价的 1 折（OpenAI 全线）。
for name in LONG + ("gpt-5.5", "gpt-5.4"):
    r = plan[name]["rules"][-1]["rates"]
    assert r["input_cache_hit"] * 10 == r["input_cache_miss"], f"{name}：官方缓存命中价应为输入价的 1 折"

# DeepSeek：空闲价必须是高峰价的一半（官方口径），且两条规则顺序正确。
for name in ("deepseek-flash", "deepseek-v4-pro"):
    peak, off = plan[name]["rules"]
    assert peak["when"].get("time_windows") and off["when"] == {}, f"{name}：分时规则顺序不对"
    for dim in ("input_cache_hit", "input_cache_miss", "output"):
        assert off["rates"][dim] * 2 == peak["rates"][dim], f"{name}/{dim}：空闲价不是高峰价的一半"

# 图像模型：output 必须是该模型的**图像输出**价（不是文本输出价）。
for name, expect in (("gpt-image-1", 40), ("gpt-image-1.5", 32), ("gpt-image-2", 30), ("gpt-image-2.5", 30)):
    assert plan[name]["rules"][0]["rates"]["output"] == usd(expect), f"{name}：图像输出价不对"
# 音频模型：output 是音频输出价（$80），不是文本输出价。
for name in ("gpt-4o-audio-preview", "gpt-4o-realtime-preview"):
    assert plan[name]["rules"][0]["rates"]["output"] == usd(80), f"{name}：应按音频输出价计"

print("\n自检通过：catch-all / 裸 input / 缓存折扣 / 长上下文 2× 与 1.5× / 分时半价 / 图像音频档位一致。")
PY

code="$(curl -s -m 10 -c "$COOKIE" -o "$WORK/login.json" -w '%{http_code}' \
  -X POST "$GW_BASE/admin/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$GW_ADMIN_USER" "$GW_ADMIN_PASS")")"
[ "$code" = "200" ] || { echo "登录失败 HTTP $code: $(cat "$WORK/login.json")" >&2; exit 1; }
echo "已登录 $GW_BASE ($GW_ADMIN_USER)"

curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers" -o "$WORK/providers.json"

PROVIDER_IDS="$(python3 -c '
import json,sys
print(" ".join(str(p["id"]) for p in json.load(open(sys.argv[1]))["data"]))
' "$WORK/providers.json")"
[ -n "$PROVIDER_IDS" ] || { echo "实例上没有任何供应商" >&2; exit 1; }

for pid in $PROVIDER_IDS; do
  curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$pid/models" -o "$WORK/models-$pid.json"
done

# 提交体只写 public_model + pricing_rules：POST /providers/{id}/models 是**部分更新**
# （M28 起，省缺字段保持原值），所以不去碰 capabilities / 上下文 / enabled——控制台定价页
# 正是因为整行覆盖把 capabilities 打回默认值才出过事。upstream_model 也不提交：规则是
# 按它算出来的，但改映射不是本脚本的职责。
python3 - "$WORK" "$PLAN" "$WORK/providers.json" $WORK/models-*.json <<'PY'
import json, os, sys

work, planfile, providers_file = sys.argv[1], sys.argv[2], sys.argv[3]
listing_files = sys.argv[4:]
plan = json.load(open(planfile))
providers = {p["id"]: p["name"] for p in json.load(open(providers_file))["data"]}

# 没有官方价、且已确认过的 id：显式白名单（写清理由与最接近的真实 id），避免脚本对着一台
# 新实例乱报错，也避免有人顺手给它编一个价。名单外的未知 upstream 一律失败。
UNPRICED = {
    "gpt-6": "官方没有裸 `gpt-6` 这个 API 模型 id（GPT-6 家族目前只有 gpt-6-astra）；"
             "官方定价页、LiteLLM、models.dev、OpenRouter 四处都查不到它",
    "gpt-4o-translate": "官方没有这个 id；定价页的语音类只有 gpt-4o-transcribe（$2.5/$10）、"
                        "gpt-4o-mini-transcribe（$1.25/$5）、gpt-transcribe（$0.0045/分钟）与 "
                        "gpt-realtime-translate（$0.034/分钟，按分钟计费）——转写/实时翻译都不是同一个模型",
    "gpt-4o-translate-mini": "同上：官方没有这个 id（最接近的是 gpt-4o-mini-transcribe，但那是转写模型）",
}

bodies_dir = os.path.join(work, "bodies")
os.makedirs(bodies_dir, exist_ok=True)
manifest = []
unpriced, unknown, priced = [], [], 0

for path in sorted(listing_files):
    rows = json.load(open(path))["data"]
    for row in rows:
        upstream = row["upstream_model"] or row["public_model"]
        if upstream not in plan:
            entry = (providers.get(row["provider_id"], row["provider_id"]), row["public_model"], upstream)
            (unpriced if upstream in UNPRICED else unknown).append(entry)
            continue
        body = {"public_model": row["public_model"], "pricing_rules": plan[upstream]}
        name = f"{row['provider_id']}--{row['public_model']}.json".replace("/", "_")
        json.dump(body, open(os.path.join(bodies_dir, name), "w"), ensure_ascii=False)
        manifest.append((row["provider_id"], providers.get(row["provider_id"], ""), row["public_model"], upstream, name))
        priced += 1

with open(os.path.join(work, "manifest.tsv"), "w") as fh:
    for pid, pname, public, upstream, name in manifest:
        fh.write(f"{pid}\t{pname}\t{public}\t{upstream}\t{name}\n")

print(f"\n将写入 {priced} 行供应商模型：")
for pid, pname, public, upstream, _ in manifest:
    print(f"  provider {pid:<3} {pname:28s} {public:24s} <- {upstream}")

if unpriced:
    print(f"\n未定价 {len(unpriced)} 行（白名单：没有官方价，保持原样）：")
    for pname, public, upstream in unpriced:
        print(f"  {pname:28s} {public:24s} <- {upstream}：{UNPRICED[upstream]}")

if unknown:
    print(f"\n取不到价 {len(unknown)} 行（**不在白名单**，先补价格表或补白名单）：", file=sys.stderr)
    for pname, public, upstream in unknown:
        print(f"  {pname:28s} {public:24s} <- {upstream}", file=sys.stderr)
    raise SystemExit(f"有 {len(unknown)} 个 upstream 模型没有官方价，整批中止（未写入任何一行）")
PY

# 干跑也走完「列目录 → 逐行取价 → 报未定价」这一段：只有把目标实例的真实行打印出来，
# 干跑才算预览。写完的步骤（POST / 读回 / 试算）留在 --apply 里。
if [ "$APPLY" != "1" ]; then
  echo
  echo "干跑结束（未写入）。加 --apply 写入 $GW_BASE。"
  exit 0
fi

echo
echo "开始逐行写入…"
failed=0
while IFS=$'\t' read -r pid pname public upstream name; do
  [ -n "$pid" ] || continue
  code="$(curl -s -m 15 -b "$COOKIE" -o "$WORK/out.json" -w '%{http_code}' \
    -X POST "$GW_BASE/admin/api/v1/providers/$pid/models" \
    -H 'Content-Type: application/json' --data-binary "@$WORK/bodies/$name")"
  if [ "$code" = "200" ]; then
    echo "  ok   provider $pid  $public <- $upstream"
  else
    echo "  FAIL provider $pid  $public <- $upstream (HTTP $code): $(cat "$WORK/out.json")" >&2
    failed=$((failed + 1))
  fi
done < "$WORK/manifest.tsv"
[ "$failed" = "0" ] || { echo "$failed 行写入失败" >&2; exit 1; }

# ── 读回核对：逐行把库里的 pricing_rules 与计划逐字段比对 ─────────────────────
echo
echo "读回核对…"
for pid in $PROVIDER_IDS; do
  curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/providers/$pid/models" -o "$WORK/verify-$pid.json"
done
python3 - "$PLAN" "$WORK/manifest.tsv" $WORK/verify-*.json <<'PY'
import json, sys
plan = json.load(open(sys.argv[1]))
manifest = [line.rstrip("\n").split("\t") for line in open(sys.argv[2]) if line.strip()]
stored = {}
for path in sys.argv[3:]:
    for row in json.load(open(path))["data"]:
        stored[(str(row["provider_id"]), row["public_model"])] = row

bad = 0
for pid, pname, public, upstream, _ in manifest:
    row = stored.get((pid, public))
    if row is None:
        print(f"  MISSING provider {pid} {public}", file=sys.stderr); bad += 1; continue
    if row.get("upstream_model") != upstream:
        print(f"  DRIFT   provider {pid} {public}：upstream 现在是 {row.get('upstream_model')}（计划时是 {upstream}）", file=sys.stderr)
        bad += 1
        continue
    if row.get("pricing_rules") != plan[upstream]:
        print(f"  DIFF    provider {pid} {public} <- {upstream}：读回的规则与计划不一致", file=sys.stderr)
        bad += 1
raise SystemExit(1 if bad else 0)
PY
echo "  ok   全部 $(wc -l < "$WORK/manifest.tsv") 行的规则与计划一致"

# ── 试算：用真实计价引擎算几个代表性场景，数字与官方价对上 ────────────────────
echo
echo "价格试算（POST /pricing/simulate）…"
simulate() { # model at json-dimensions
  curl -s -m 10 -b "$COOKIE" -X POST "$GW_BASE/admin/api/v1/pricing/simulate" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$1\",\"at\":\"$2\",\"dimensions\":$3}"
}
# 输入一律压在 272K 以下，除非就是要验长上下文档——1M 输入会（正确地）落进长档，
# 本脚本的第一版自检正是这么写错的，彩排时才暴露出来。
peak=$(simulate deepseek-flash 2026-09-14T02:00:00Z '{"input_cache_miss":1000000,"output":1000000}')
off=$(simulate deepseek-flash 2026-09-14T00:30:00Z '{"input_cache_miss":1000000,"output":1000000}')
luna=$(simulate gpt-5.6-luna 2026-09-14T00:30:00Z '{"input_cache_miss":200000,"output":1000000}')
astra=$(simulate gpt-6-astra 2026-09-14T00:30:00Z '{"input_cache_miss":200000,"output":1000000}')
astralong=$(simulate gpt-6-astra 2026-09-14T00:30:00Z '{"input_cache_miss":300000,"output":1000000}')
boundary=$(simulate gpt-6-astra 2026-09-14T00:30:00Z '{"input_cache_miss":272000,"output":0}')
image=$(simulate gpt-image-2 2026-09-14T00:30:00Z '{"input_cache_miss":200000,"output":1000000}')
python3 - "$peak" "$off" "$luna" "$astra" "$astralong" "$boundary" "$image" <<'PY'
import json, sys

CASES = [
    ("deepseek-flash 高峰（UTC 周一 02:00）", 0.30 + 1.20, sys.argv[1]),
    ("deepseek-flash 空闲（UTC 周一 00:30）", 0.15 + 0.60, sys.argv[2]),
    ("gpt-5.6-luna 标准档（200K 输入 + 1M 输出）", 0.20 * 0.2 + 1.20, sys.argv[3]),
    ("gpt-6-astra 标准档（200K 输入 + 1M 输出）", 10.00 * 0.2 + 50.00, sys.argv[4]),
    ("gpt-6-astra 长档（300K 输入 + 1M 输出）", 20.00 * 0.3 + 75.00, sys.argv[5]),
    ("gpt-6-astra 边界（恰好 272000 输入，应走标准档）", 10.00 * 0.272, sys.argv[6]),
    ("gpt-image-2（200K 文本输入 + 1M 图像输出）", 5.00 * 0.2 + 30.00, sys.argv[7]),
]
bad = 0
for label, expect_usd, raw in CASES:
    try:
        got = json.loads(raw)
    except Exception:
        print(f"  FAIL {label}: 响应无法解析：{raw[:200]}"); bad += 1; continue
    cost = got.get("cost_micros", 0) / 1e6
    ok = abs(cost - expect_usd) < 1e-6
    bad += 0 if ok else 1
    print(f"  {'ok  ' if ok else 'FAIL'} {label:44s} cost=${cost:.6f} 期望 ${expect_usd:.6f} "
          f"rule={got.get('cost_rule_id', '?')}")
raise SystemExit(1 if bad else 0)
PY

# ── 定价面体检：没有解析错误、没有非法规则集 ──────────────────────────────────
echo
echo "定价面体检（GET /pricing/targets）…"
curl -s -m 10 -b "$COOKIE" "$GW_BASE/admin/api/v1/pricing/targets" -o "$WORK/targets.json"
python3 - "$WORK/targets.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
cost = [t for t in d["targets"] if t["kind"] == "cost"]
broken = [t for t in cost if t.get("parse_error") or not t.get("valid")]
shadow = [t for t in cost if t.get("shadowed")]
print(f"  成本侧目标 {len(cost)} 个；解析失败/非法 {len(broken)} 个；被遮蔽规则 {len(shadow)} 个")
print(f"  声明缺费率的维度：{d.get('missing_rates') or '无'}")
for t in broken:
    print(f"  FAIL provider={t['provider_name']} model={t['model']} parse_error={t['parse_error']}")
if broken:
    raise SystemExit(1)
PY

echo
echo "完成。核对入口："
echo "  控制台「定价」页：$GW_BASE/admin/ui/"
echo "  逐行读回：curl -s -b <cookie> $GW_BASE/admin/api/v1/providers/<id>/models"
echo "  重新核对官方价（本机被 403 时在能出网的机器上跑）："
echo "    curl -s https://developers.openai.com/api/docs/pricing | grep -o 'gpt-6-astra.\\{0,200\\}'"
