# M11a 设计：计价引擎（维度 × 有序价格规则集）

> 规格：`docs/pricing.md`（已输出）。本文件是落地设计，实现后回填差异。

## 1. 目标

一个**纯函数**计价引擎：输入（规则集 + 各维度数量 + 请求发起时刻 + 变体），输出（分维度金额、合计、命中规则、可复算快照）。
数据面结算、管理面试算器、对账重放**共用同一个函数**，因此「界面算出来的」必然等于「实际收的」。

## 2. 包与分层

```
internal/pricing/
  rule.go      // RuleSet/Rule/When 类型 + Parse（含全部校验）
  window.go    // 时区/星期/跨午夜的时间窗判定
  engine.go    // Evaluate：唯一纯函数；成本侧与售价侧走同一条路径
  snapshot.go  // 命中规则的完整内联副本（可复算）
  shadow.go    // 遮蔽检测（只告警，不拒绝）
```
依赖方向：`pricing` 只依赖标准库与 `internal/domain` 的错误类型，不依赖 store/httpapi；
store 负责把 `pricing_snapshot_json` 原样落库。

## 3. 单位与取整（写死，避免歧义）

- 所有 `rates` 单位统一为 **微美元 / 100 万单位**（token、次、秒都一样）：
  `amount_micros = ceil(units * rate / 1_000_000)`；
  这样 $0.27/1M token = `270000`，$0.04/张图 = `40000000000`，int64 完全够用。
  （规格原文对 count/second 写的是「微美分/单位」，这里统一成同一把尺子，减少两条代码路径与口径歧义。）
- `per_request_fee_micros` 直接是**微美元/请求**，不参与上面的缩放。
- 全程 **int64 + ceil**，禁止浮点；倍率用基点（bp，10000 = 1.0×）。
- 负数数量视为 0（上游偶发脏数据不应该变成退款）。

## 4. 求值顺序

1. 成本侧：按 `order` 升序取**第一条 when 全部满足**的规则；
2. 售价侧：`absolute` 走自己的规则集；`cost_follow` 用成本金额 × 倍率（可按维度覆写倍率）；
3. 每请求固定费按 `per_request_fee_scope` 加到对应侧；
4. 应用 `min_charge_micros`（售价侧，仅当 > 0）；
5. 命中规则缺少某维度单价 → 该维度计 0，但在结果里列出 `unpriced_dimensions` 并告警。

售价来源优先级（首个命中的来源生效）：key.policy → tag.policy（按 priority）→ account.price_overrides →
模型售价规则 → 兜底 `cost_follow` + `billing.default_markup_bp`。M11a 只实现「模型售价规则 + 兜底」，
前三级需要账户/Key 策略解析，放在 M11b 与 metering 接线时一起做（那里才有 Key/Tag 上下文）。

## 5. 时间窗

- 支持 `UTC`、`Local` 与固定偏移（`+08:00`）；内嵌 tzdata（`import _ "time/tzdata"`），
  避免依赖宿主机的 zoneinfo；
- `[start, end)` 半开；`end < start` 表示跨午夜；`end == start` 直接判为非法（避免「全天还是零长」的歧义）；
- 跨午夜时星期归属**按窗口起点所在日**：`(今天命中且 m >= start) || (昨天命中且 m < end)`；
- 判定时点默认 `request_start`（可配 `completion`），由调用方传入时间，引擎不读时钟。

## 6. 校验（写入时，400）

- 必须存在 **catch-all** 规则（`when` 为空对象）；
- `order` 必须严格递增且唯一（否则规则的相对顺序不确定）；
- `rates` 键非空、值 >= 0；维度键匹配 `^[a-z][a-z0-9_]{0,31}$`；
- `tier.lt > tier.gte`（`lt` 缺省表示无上界）；`tier.basis ∈ input|total|output`；
- 时间窗 `start`/`end` 形如 `HH:MM`、时区可解析、`days` 取自 mon..sun；
- `valid_from < valid_to`（若都给）。

## 7. 遮蔽检测（告警，不拒绝）

规则 B（order 更大）被 A 遮蔽的判定：A 的每个条件都**不比 B 强**——
- A 无时间窗（或与 B 完全相同的时间窗集合）、
- A 的 `valid_from` 不晚于 B、`valid_to` 不早于 B、
- A 无档位（或档位区间包含 B 的区间）、
- A 无变体限制（或与 B 相同）。

特别地：**A 是 catch-all 时，其后所有规则都不可达**。检测结果作为 warning 返回，界面显示为黄色告警，
不阻止保存（运维可能在准备未来的促销）。

## 8. 快照（可复算）

`pricing_snapshot_json` 内联：命中的成本规则与售价规则**完整副本**、双方 rule id、各维度数量与单价、
命中时间窗与档位、`tier_basis`、倍率、`unpriced_dimensions`、`usage_dimensions_incomplete`。
规则改了或删了，历史账仍可逐笔复算——对账与 rebuild 只读快照。

## 9. 管理面

- `POST /admin/api/v1/pricing/simulate`：入参 `{model, at, variant, dimensions:{...}, markup_bp?, cost_rules?, sale_rules?}`，
  返回命中规则、逐维度金额、合计与快照；`cost_rules`/`sale_rules` 省略时取模型与供应商映射上已配置的规则，
  给了则用给的（做「改了会怎样」的对比，不落库）；
- `POST /admin/api/v1/pricing/validate`：只做校验与遮蔽检测，供编辑器实时提示；
- 两个端点都是**只读**（不写库），仅要求会话。

## 10. 测试

1. 单元：首命中顺序、catch-all、跨午夜窗口（含星期归属）、档位边界（半开）、valid_from/to、变体限定；
2. 取整：`ceil` 逐维度求和（1 token × 270000/1M = ceil(0.27) = 1 微美元）；
3. `cost_follow` 与 `absolute` 的互斥语义、按维度覆写倍率、`min_charge`、`per_request_fee`；
4. 校验：缺 catch-all 400、order 重复 400、坏时段 400、`lt <= gte` 400；
5. 遮蔽：catch-all 之后的规则被标出；完全相同的两条规则后者被标出；
6. 快照可复算：用快照里的副本重新算一遍，金额必须一致；
7. HTTP：simulate 端点返回逐维度金额；坏规则返回 400 并给出字段名。

## 11. 实现与设计差异

1. **单位口径统一**：规格里 count/second 类维度写的是「微美分/单位」，实现统一为微美元 / 100 万单位（对所有维度一视同仁）。理由：只有一套缩放与取整代码，结算、试算、对账不会出现两种口径；$0.04/张图 = `40000000000`，int64 绰绰有余。
2. **`cost_follow` 允许没有规则**：售价侧用 `cost_follow` 时规则不参与求值，只有 `markup_bp` 与 `dimension_markup_bp` 起作用，因此校验不再强制它带 catch-all；但 `absolute` 没规则仍然 400（那等于白送）。
3. **`mulDivCeil` 加了溢出保护**：乘积超过 2^62 时不再静默回绕，而是截断（现实中不可达，但不留隐患）。
4. **售价来源优先级只实现了后两级**（模型售价规则 → 兜底 cost_follow + 默认倍率）：key.policy / tag.policy / account.price_overrides 需要请求上下文，放到 M11b 与 metering 接线时实现。
5. **试算器额外解析规则来源**：返回 `sources.cost` / `sources.sale`（如 `provider_model:1/gpt-4o`），否则运维无法判断「这条价是哪来的」。多个供应商有成本规则时取 priority 最小的一条并说明来源。
6. **`/pricing/validate` 返回 200 + `valid:false`**（而不是 400）：这是编辑器的实时提示接口，400 会让前端把「规则还没写完」当成请求失败。
7. **快照里同时内联成本与售价规则副本**，并额外记录 `matched_windows`、`matched_tier`、`unpriced_dimensions`，让对账页面能直接解释「为什么是这个价」。
8. **未接入请求路径**：本里程碑只做引擎与管理面；把 cost/charge 写进 `usage_records` 与账本在 M11b 完成（数据面热路径不动，是刻意的）。