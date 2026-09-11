# 定价：计量维度 × 有序价格规则集

> 状态：**已实现（M11a：internal/pricing + 管理面试算/校验）**；**多币种已实现（M22：模型级币种 + 账本换算）**。
> 目标：用同一套模型表达"固定单价""分时优惠""按长度分档""缓存命中/未命中"
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
  "currency": "CNY",
  "rules": [
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
  ]
}
```

- `currency`：该规则集的**计价币种**（3 位大写码，如 `USD` / `CNY`），**缺省 = 账本币种**（`billing.currency`，默认 `USD`）。
  文档里的 `rates.*` 与 `per_request_fee_micros` 都按该币种解释；成本侧与售价侧各自独立声明（见 §3、§9）。
- `rates.*` 单位：**微单位 / 百万单位**（token 类）或**微单位 / 单位**（count/second 类）；
  微单位 = 1e-6 个该规则集的币种（USD 规则集里 270000 = $0.27/百万 token）。**整数，禁止浮点**。
- `order` 决定求值顺序；`when` 为空对象即 **catch-all**。
- **求值**：按 `order` 升序，第一条 `when` 全部满足的规则生效（首命中）。
- **写入校验**：必须存在 catch-all 规则（否则 400）；检测**被遮蔽规则**（永远不可能命中）并在 UI 告警；
  校验 `lt > gte`、单价非负、时段格式、维度键合法、**币种形状合法且汇率表里有该币种**（否则 400）。

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

- **成本价**（我方付上游）：`provider_models.pricing_rules_json`，忠实还原上游真实计费，可带自己的 `currency`（上游按 CNY 报价时就写 `CNY`）。
- **售价**（对客）：`models.sale_pricing_json`，可带自己的 `currency`（对客报价币种），`basis` 二选一：
  - `cost_follow`：售价 = 该次成本 × `markup_bp`（**上游调价或切换时段时售价自动跟随**），可按维度覆写倍率；
    成本币种 ≠ 售价币种时**先按汇率把成本单价换算到售价币种、再乘倍率**（见 §9）；
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
- 各维度数量与单价、命中的时段窗口与档位、`tier_basis`、`usage_dimensions_incomplete`；
- **多币种**：`cost_currency` / `sale_currency` / `ledger_currency`、原生金额
  `cost_micros_native` / `charge_micros_native`、入账金额 `ledger_cost_micros` / `ledger_charge_micros`、
  本次使用的汇率 `fx_cost_ledger` / `fx_sale_ledger`（跨币种 `cost_follow` 时另有 `fx_cost_sale`），
  以及汇率缺失时的 `fx_unavailable` 币种列表。

因此规则被修改或删除后，历史账单仍可逐笔复算；**汇率表改了也不影响历史**（快照自带当次汇率）。
对账与 `rebuild-ledger` 都只依赖快照（后者只重放账本币种金额）。

## 6. 其他计价项

- `min_charge_micros`：每请求最低收费（默认 0），**按该模型售价币种（原生）解释**，在换算入账之前应用。
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

## 9. 多币种（M22）

**一句话**：模型按自己的币种计价，账本永远一个币种（`billing.currency`，默认 USD），跨界金额在结算时按整数汇率换算。

### 9.1 汇率表

```yaml
billing:
  currency: USD            # 账本币种：余额/授信/预留/账本/发票/对账都用它
  display_currency: USD    # 控制台默认展示币种（可选值 ∈ 账本 + 汇率表）
  fx_rates:                # 1 单位该币种 = N 微账本币种（整数，6 位小数精度）
    CNY: 141000            # 1 CNY = 0.141000 USD
```

- 账本币种**不得**出现在 `fx_rates` 里（它隐含 1e6）。
- 控制台「设置 → 汇率表」（`PUT /admin/api/v1/settings/billing.fx_rates`）可覆盖，写审计并立即生效（不重启）。
- 写入模型规则时若币种没有汇率 → **400**，提示先补汇率；`GET /pricing/targets` 与页面会以红色徽标暴露缺汇率的币种。

### 9.2 换算与取整

| 方向 | 公式 | 取整 |
|---|---|---|
| 外币 → 账本（入账） | `ceil(金额 × rate / 1e6)` | 向上取整：不低估收入，也不低估成本 |
| 账本 → 显示币种（仅展示） | `round(金额 × 1e6 / rate)` | 四舍五入：展示不偏 |

`cost_follow` 且成本币种 ≠ 售价币种时，**先换算成本单价**再乘倍率：
`saleRate = ceil(costRate × rate(cost→sale) / 1e6)`，`amount = ceil(saleRate × markup_bp / 10000)`。
同币种时 `rate = 1e6`，结果与单币种实现逐位一致。

### 9.3 口径

- `usage_records.cost_micros` / `charge_micros`、账本条目、余额：**一律账本币种微单位**（与 M22 之前语义相同，只是"微单位"现在明确等于账本币种）。
- 计价页/试算接口返回的 `cost_micros` / `charge_micros`：**模型原生币种**（同时给出 `ledger_*_micros`）。
- 控制台**账本类金额**按所选显示币种换算（带 `≈` 与币种码）；**计价类金额**永远按模型原生币种显示（带币种码、不换算）。
- 发票/账单的正式币种永远是账本币种：显示币种只是视图偏好，不改变任何凭证。
- 汇率缺失（只可能来自手改库）：该次 cost/charge 记 0、快照标 `fx_unavailable`、Error 日志 + `billing.fx_missing` hook + invariants 计数；**不打断客户请求**，预留回退 `billing.reserve_micros_default`。

## 10. 管理界面工具（M22 增补）

- 「定价」页：倍数面板新增**售价币种**下拉（继承账本币种 / 具体币种），保存走 `PATCH /pricing/markup` 的 `currency`；
  成本币种在成本规则 JSON 里写 `currency`（保存走 `POST /providers/{id}/models`）；目标卡片显示币种徽标与缺汇率告警。
- 「设置」页：汇率表编辑器（键值对 + 校验），保存即生效。
- 顶栏：**显示币种**选择器（`GET /admin/api/v1/billing/currency` 提供列表与汇率），选择记住在本浏览器。
