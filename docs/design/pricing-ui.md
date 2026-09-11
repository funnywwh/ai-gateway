# 设计：对客价 = 成本 × 倍数（Pricing 页面与生效链）

> 计划第四版 §3（已批准）。实现完成后回填第 6 节差异。

## 1. 计费方式

| 方式 | 语义 | 用途 |
|---|---|---|
| **倍数（`cost_follow`，默认）** | 售价 = 该次实际成本 × 倍数；逐维度 `ceil(cost_dim × bp / 10000)`，每请求固定费同样乘倍数 | 常规经营：上游调价、优惠时段、缓存命中自动跟随 |
| 绝对定价（`absolute`） | 用自己的单价规则集 | 固定价、促销窗口、白标定价 |

倍数是**整数基点**（10000 = 1.0×），不引入浮点；支持按维度覆写（例如输出 2×、输入 1.2×）。
这意味着「成本规则一改，售价自动跟随」——代价是**没有成本规则时售价恒为 0**，因此页面必须对此红色告警（§4）。

## 2. 生效链（本轮补齐请求路径）

```
key.policy.margin_bp  →  tag.policy.margin_bp（按 priority 升序，后者覆盖）
                      →  account.markup_override_bp
                      →  model.sale_pricing.markup_bp
                      →  billing.default_markup_bp（全局兜底）
```

首个命中的生效，并把来源写进计价快照的 `markup_source`（`key|tag|account|model|default`）。
现状：M11a 只实现了后两级；本轮接上前三级中的**倍数**部分。`account.price_overrides` 的完整规则文档仍不接（列为不做）。

实现要点：
- 策略里的字段名统一为 `margin_bp`（与 `rate_limit` 等并列），解析失败不影响其它策略字段；
- 复用一个纯函数 `billing.ResolveMarkup(key, tags, account, sale *pricing.RuleSet, defaultBP int) (bp int, source string)`，
  便于单测覆盖优先级；请求路径只调用它，不再自己判断；
- 快照记录 `markup_source`，账本与 MCP 查询据此解释「这个价怎么来的」。

## 3. 后端接口（都很小）

### 3.1 `GET /admin/api/v1/pricing/targets`

一次返回所有可编辑目标，避免「模型数 × 供应商数」的 N+1 拉取：

```json
{ "targets": [
  { "kind":"sale", "model":"gpt-4o", "basis":"cost_follow", "markup_bp":15000,
    "dimension_markup_bp":{"output":20000}, "rules":[…], "valid":true, "parse_error":"",
    "shadowed":0, "catch_all":true, "cost_rules_configured":true,
    "effective_markup":{"bp":15000,"source":"model"} },
  { "kind":"cost", "model":"gpt-4o", "provider_id":2, "provider_name":"openai-main",
    "provider_model_id":7, "upstream_model":"gpt-4o-2024-11-20", "rules":[…],
    "valid":false, "parse_error":"rules[0]: …", "shadowed":1, "catch_all":false,
    "cost_rules_configured":true, "effective_markup":{"bp":15000,"source":"model"} }
] }
```

- 规则用 `pricing.ParseRuleSet` 解析；解析失败**不返回 5xx**，而是带 `parse_error` 让页面显示并修复坏数据；
- `cost_rules_configured`：该公开模型是否存在**任何**配置了成本规则的供应商映射——为 false 时页面显示红色「倍数空转」告警；
- `effective_markup`：按 §2 的链条算出的当前生效倍数与来源（sale 目标含此字段）。

### 3.2 `PATCH /admin/api/v1/pricing/markup`

```json
{ "model": "gpt-4o", "basis": "cost_follow", "markup_bp": 15000,
  "dimension_markup_bp": { "output": 20000 } }
```

- **只读改写这两个字段**：读出 `models.sale_pricing_json` → 解析 → 改 `basis`/`markup_bp`/`dimension_markup_bp` → 写回，
  **规则数组原样保留**（避免整份 JSON 覆盖别人的编辑）；JSON 损坏时拒绝并提示改用页面里的 JSON 视图修复；
- `basis` 缺省为 `cost_follow`；`markup_bp` 必须在 [0, 1000000]（0–100×）；`dimension_markup_bp` 的键必须是合法维度名；
- 写后 `Reload` 注册表让新价立即生效（注册表快照带 `sale_pricing_json`），并写审计。

## 4. 页面（`internal/webui/static/js/pages/pricing.js`）

两个标签页，默认落在**倍数**：

**① 倍数（默认）**
- 目标选择：对客模型下拉 + 搜索；每行徽章显示当前倍数与来源（模型/账户/标签/Key/全局默认）；
- 倍数输入：`×1.50` 与 `15000 bp` 双向联动；「按维度覆写」可展开，逐维度填倍数（留空＝继承总倍数）；
- 右侧实时预览：可调维度与时间点 → `POST /pricing/simulate`（内联当前编辑值）显示 成本 → 售价 → 毛利；
- **空转告警**：`cost_rules_configured=false` 时红色提示并给「去配置成本规则」的跳转；
- 保存 → `PATCH /pricing/markup`。

**② 高级：绝对定价 / 促销规则**
- 规则表格：按 order 排序，行内改 id/title/when/rates/per_request_fee；`when` 结构化控件（星期多选 + HH:MM + 时区、valid_from/to、档位 basis/gte/lt、model_variant）；rates 按规则集出现的维度动态生成列并标注「微美元 / 100 万单位」；行操作上移/下移/复制/删除；「JSON 视图」与表格双向同步；「模板」（DeepSeek 标准/优惠 × 缓存命中/未命中 × 输入分档、OpenAI 长上下文档、Ollama 零价）可追加或替换；
- 侧栏：**校验**（防抖 400ms 调 `/pricing/validate`，显示 valid/错误/遮蔽告警）、**试算**（内联规则）、**阶梯预览**（1K/8K/32K/128K）；
- 选择 `absolute` 时提示「此方式不再跟随成本，倍数字段不生效」。

保存路由：倍数 → `PATCH /pricing/markup`；sale 规则 → `PATCH /models/{name}`；cost 规则 → `POST /providers/{id}/models`。
保存成功后重拉 targets；有未保存改动切目标/离开要确认；viewer 角色看不到保存按钮。

## 5. 测试

1. `ResolveMarkup` 优先级表驱动：key > tag > account > model > default，且 tag 之间按 priority 后者覆盖；
2. 倍数换算：`ceil(cost_dim × bp / 10000)` 逐维度、按维度覆写、bp≥10000 时 `charge ≥ cost`；
3. 快照含 `markup_source`；账本金额与 `ceil` 一致；
4. `GET /pricing/targets`：解析成功/失败（parse_error）、遮蔽计数、`cost_rules_configured`、`effective_markup`；
5. `PATCH /pricing/markup`：只改倍数而**保留规则数组**、坏 JSON 拒绝、越界拒绝、审计落库；
6. 端到端：模型倍数 1.0×→1.5× 后发请求，账本 `charge = ceil(cost × 1.5)` 且 `markup_source=model`；再把账户倍数设为 2.0×，断言来源变为 `account` 且金额随之变化；
7. 前端：Node 语法 + 导入图校验；curl 走查取目标 → 改倍数 → 校验坏文档 → 试算 → 保存 → 重读。

## 6. 实现与设计差异

1. **账户级倍数需要「是否设置过」的标志位**：0 是合法倍数（免费额度），无法与「没设过」区分，
   因此加了迁移 `0005_account_markup_flag.sql`（`accounts.markup_override_set`），
   并由 `PATCH /accounts/{id}` 在写入 `markup_override_bp` 时置位。首次实测正是因为缺这一位，
   账户倍数被静默忽略（快照里 `markup_source` 仍是 `model`）。
2. **按维度覆写与账户倍数是「组合」而不是「覆盖」**：基础倍数按 §2 的链条取首个命中（可能是账户级），
   而 `dimension_markup_bp` 属于模型售价文档，作为逐维度细化继续生效。实测：账户 2.0× + 模型 output 2.5× →
   同一请求里 input 按 2.0×、output 按 2.5×，账本 charge 与 `sum(sale_lines)` 一致。
   这一条是策略选择，写进文档与页面提示，避免被误读为 bug。
3. **`PATCH /pricing/markup` 只改 `basis`/`markup_bp`/`dimension_markup_bp` 三个键**，
   `rules` 数组、`currency` 等其它键原样保留；文档解析失败时拒绝并提示改用 JSON 视图修复，
   而不是整份覆盖（防止把别人正在编辑的规则冲掉）。
4. **`GET /pricing/targets` 一次返回 sale 与 cost 两类目标**，并附带 `cost_rules_configured`
   （该模型是否存在带成本规则的供应商映射）与 `effective_markup`（含来源）。解析失败以 `parse_error` 返回而不是 5xx，
   因为页面必须能显示并修复坏数据。
5. **页面默认落在「倍数」标签页**，`absolute` 与规则编辑降为第二个标签页，并提示「切换后倍数不再生效」；
   实时试算把编辑器内的内联规则发给 `/pricing/simulate`，因此预览的就是未保存的改动。
6. **倍数与成本的换算仍是逐维度 `ceil`**：实测 1.0× cost=7/charge=7；1.5×（output 2.5×）cost=12/charge=28，
   且 `sum(sale_lines) == charge`；账户 2.0× 后 `markup_source` 变为 `account`。
7. **`ResolveMarkup` 对坏策略容错**：解析失败的 key/tag 策略被忽略并继续向下匹配，
   不会因为一个字段写错就把整条计价链打断（有单测覆盖）。