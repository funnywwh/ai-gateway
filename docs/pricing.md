# 定价：计量维度 × 有序价格规则集

> 状态：**规格（M11a 实现）**。目标：用同一套模型表达"固定单价""分时优惠""按长度分档""缓存命中/未命中"
> 以及"分维度计费"，**不使用浮点**。

## 1. 计量维度（dimensions）

统一为 `map[string]int64`：

| 维度 | 单位 | 说明 |
|---|---|---|
| `input` | token | 未区分缓存的输入 |
| `input_cache_hit` | token | 缓存命中输入（DeepSeek `prompt_cache_hit_tokens`） |
| `input_cache_miss` | token | 缓存未命中输入 |
| `output` | token | 输出 |
| `reasoning` | token | 思考（默认计入 output，可单列） |
| 扩展 | count/second | `image`、`audio_second`、`tool_call` |

**缺失维度兜底**：上游未给分维度 usage 时，`input_cache_hit = 0`、其余计入 `input_cache_miss`，
并标 `usage_dimensions_incomplete = true` + 告警（**宁可高估成本，不把缓存命中按未命中漏算**）。

## 2. 价格规则

```json
{
  "id": "deepseek-offpeak-hit",
  "title": "优惠时段·缓存命中",
  "order": 10,
  "when": {
    "time_windows": [{"days": ["mon","tue","wed","thu","fri","sat","sun"],
                      "start": "16:30", "end": "00:30", "tz": "UTC"}],
    "valid_from": "2026-01-01T00:00:00Z",
    "valid_to": null,
    "tier": {"basis": "input", "gte": 0, "lt": 32768},
    "model_variant": "deepseek-chat"
  },
  "rates": {"input_cache_hit": 70000, "input_cache_miss": 270000, "output": 1100000},
  "per_request_fee_micros": 0
}
```

- `rates.*` 单位：**微美分/百万单位**（token 类）或**微美分/单位**（count/second 类）。**整数，禁止浮点**。
- `order` 决定求值顺序；`when` 为空对象即 **catch-all**。
- **求值**：按 `order` 升序，第一条 `when` 全部满足的规则生效（首命中）。
- **写入校验**：必须存在 catch-all 规则（否则 400）；检测**被遮蔽规则**（永远不可能命中）并在 UI 告警；
  校验 `lt > gte`、单价非负、时段格式、维度键合法。

### `when` 支持的条件

| 条件 | 语义 |
|---|---|
| `time_windows[]` | `days` + `start`/`end` + `tz`；**跨午夜用 `end < start` 表示**，命中 `[start,24:00) ∪ [00:00,end)`；星期归属按窗口起点所在日；边界**半开** `[start,end)`；时区支持 `UTC` 或固定偏移（二进制内嵌 tzdata） |
| `valid_from` / `valid_to` | 促销/临时价生效区间 |
| `tier` | `{basis: input\|total\|output, gte, lt}` 长度档位 |
| `model_variant` | 限定上游模型变体 |
| `region` | 预留 |
| `monthly_usage` | `{basis: tokens\|cost\|requests, gte, lt}` 累计用量折扣（默认关闭，会增加一次缓存读） |

## 3. 成本规则集 与 售价规则集（分离）

- **成本价**（我方付上游）：`provider_models.pricing_rules_json`，忠实还原上游真实计费。
- **售价**（对客）：`models.sale_pricing_json`，`basis` 二选一：
  - `cost_follow`：售价 = 该次成本 × `markup_bp`（**上游调价或切换时段时售价自动跟随**），可按维度覆写倍率；
  - `absolute`：独立单价规则（可定义自己的时段/档位，例如只对客户在标准时段加价）。
- 售价来源优先级（首个命中）：`key.policy` → `tag.policy`（按 priority）→ `account.price_overrides` →
  模型售价规则 → 兜底 `cost_follow` + `billing.default_markup_bp`（默认 10000 = 1.0×）。

**两种 basis 互斥**：`absolute` 直接用给定单价（不再乘倍率）；`cost_follow` 先算成本再乘倍率。

## 4. 判定时点（消除歧义）

- **时段**以"网关向供应商发出请求的时刻"（`request_start`，UTC）为准（可配 `billing.peak_boundary=request_start|completion`）——
  跨时段的长调用归属同一时段。
- **档位**：`basis=input` 在准入阶段即可精确确定；`basis=total|output` 时准入按
  `input + max_output_tokens` 取**可能落入的最贵档**，结算按实际数量校正。
- **在途估算**一律取**当前可能适用的最贵规则**（含更高档位与更贵时段），结束时按真实维度校正（多退少补）。

## 5. 快照与可复算

`usage_records.pricing_snapshot_json` 内联：

- 命中的**成本规则完整副本**与 `sale_rule` 副本（`when` + `rates` + `per_request_fee`）+ 双方 rule id；
- 各维度数量与单价、命中的时段窗口与档位、`tier_basis`、`usage_dimensions_incomplete`。

因此规则被修改或删除后，历史账单仍可逐笔复算；对账与 `rebuild-ledger` 都只依赖快照。

## 6. 其他计价项

- `min_charge_micros`：每请求最低收费（默认 0）。
- 零价模型：`charge = 0`（内部/赠送用途）。
- 按次固定费：`per_request_fee_micros`，`billing.per_request_fee_scope = attempt|request`（默认 attempt）。
- 多供应商故障切换：失败的 attempt 记成本但不计费（`charge_on_error=false`），并计入"浪费成本"报表与告警。

## 7. 管理界面工具

- **规则表格编辑器**：拖拽排序、时段可视条、档位边界、各维度单价、被遮蔽规则告警、导入/导出 JSON。
- **模板**：DeepSeek（标准/优惠时段 × 缓存命中/未命中 × 输入档位）、OpenAI/Gemini 长上下文档、Ollama 零价。
- **价格试算器**（`POST /pricing/simulate`）：输入时间点 + 各维度数量 + 模型 → 命中规则、逐维度金额、合计。
  与结算**共用同一纯函数**，保证"界面说的"和"实际收的"一致。
- **阶梯预览**：同一模型在 1K/8K/32K/128K 输入下的成本/售价/毛利率。

## 8. DeepSeek 风格示例（成本侧）

| 场景 | 规则要点 |
|---|---|
| 标准时段 · 缓存命中 | `when:{}`（catch-all），`rates:{input_cache_hit:…, input_cache_miss:…, output:…}` |
| 优惠时段 · 缓存命中 | `when.time_windows=[{start:"16:30", end:"00:30", tz:"UTC"}]`，`order` 小于 catch-all |
| 输入 ≥ 32K | `when.tier={basis:"input", gte:32768}`，单价更高 |
