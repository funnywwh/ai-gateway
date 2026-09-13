# M40 设计文档：MCP 工具说明的完整性与「优先用表单」的交互

> 状态：**实现中**。规格文档：`docs/mcp.md`（工具与说明标准）、`docs/chat.md`（智能问答输出形态）、
> `docs/pricing.md`（价格规则集形状）。流程约定见 `docs/PROCESS.md`。

## 1. 触发点：一次真实的任务失败

运维在控制台问：「给 codex-sub 的 gpt-5.6-luna 配置成本价：输入 $0.20/1M、输出 $1.20/1M、
缓存命中 $0.02/1M」。会话绑定的令牌是 `scope=admin`，模型也确实按流程先调了
`admin_list_models` / `admin_list_routes` / `admin_list_all_provider_models` /
`admin_describe(admin_upsert_provider_model)`，然后**停在那里，要求管理员补文档**：

> 「当前网关的成本规则字段 `pricing_rules` 只标注为对象，但没有公开其具体规则格式。为了避免写入错误规则，
> 我不会猜字段名或结构……否则只能安全地保持当前配置不变。」

**这不是模型过于保守，是工具说明不完整。** 代码链路：

| 环节 | 事实 |
|---|---|
| 路由声明 | `admin_routes.go` 的 `admin_upsert_provider_model` 里 `pricing_rules` 只写了 `bodyOptional("pricing_rules","object","成本侧计价规则")` |
| `admin_describe` 输出 | `adminRoute.bodySchema()` 对这类字段**只输出 `{"type":"object"}`**（没有 properties） |
| 示例 | `adminRoute.example()` → `sampleBody()` 对 `object` 生成 `{}`，即"照抄示例"也会被拒 |
| 写入侧 | `handleAdminUpsertProviderModel` 走 `pricing.ParseRuleSet`，它 `DisallowUnknownFields()`——字段名猜错必然 400 |
| 结果 | agent 看到的信息**不足以安全写入**，于是拒绝写入。这是正确行为，不是缺陷行为 |

同一缺陷族（只标 `object`、形状不可见）：`sale_pricing`、`capabilities`、`policy`、`grants`、
`price_overrides`、供应商 `config`/`credentials`/`meta`/`timeout_overrides`、
`admin_validate_pricing` 的 `RawBody`（用的是 `freeFormSchema`），以及
`admin_simulate_pricing.dimensions`（说明里的维度名是错的）。

## 2. 目标

1. **补齐形状**：`admin_describe` 必须给出可直接照抄的字段名、单位、枚举与示例，尤其是配置类文档。
2. **以后不可能再退化**（本次的重点）：把"工具说明要写清什么"变成**文档标准 + 机械守卫 + 贴身的注释**，
   让下一次新增工具或字段时，漏写形状会**立刻失败**，并且失败信息直接告诉人怎么改。
3. **交互前移**：智能问答在关键参数缺失时，优先用**内联表单**问清再动手，而不是猜默认值、也不是用散文追问。

## 3. 关键决策（含取舍）

### D1 形状单一来源，放在 httpapi 而不是 pricing

字段形状必须与 `pricing.RuleSet` 同源同改，否则又会漂移。但 `internal/arch/layering_test.go` 的允许边表
不允许 `internal/mcpsrv` 依赖 `internal/pricing`，而 `mcpsrv.Tool` 的 schema 是每个工具手写的。
因此：**pricing 规则集的 schema 定义在 `internal/httpapi`（本就在允许边内），由路由表的 body 字段携带，
经 `admin_describe` 暴露给 agent**；查询工具的 schema 仍留在 `mcpsrv`。

取舍：这等于在 httpapi 里复述一份 `RuleSet` 结构。接受的理由是它同时受 §5 的两条测试约束
（文档示例必须能被 `admin_validate_pricing` 接受、必须能真正写库成功），**复述错了会红**，
而不是"多写一份就多一份漂移"。

### D2 说明语言改为中文

路由摘要、审计、控制台、聊天全是中文，工具说明却是英文；控制台聊天的模型因此要在两种语言间跳。
统一为中文，术语与后台接口一致（"后台接口""令牌 scope"）。
取舍：外部英文 agent 客户端会看到中文描述——模型读中文没有障碍，而人类的可评审性更重要。

### D3 形状缺失从"静默降级"改为"构造期 panic"

原来的降级（`{"type":"object"}` + `{}` 示例）**正是把模型推去拒绝的根因**：它看起来像文档，
实际是不可用的占位。改为：`bodySchema()` 遇到声明为 `object` 却没有 `Schema` 的字段直接 panic。
panic 只发生在构造工具元数据时（启动 / 首次 `tools/list` / `make test`），不在请求路径上，
因此代价是"进程起不来"，收益是"不可能悄悄发出去"。

### D4 守卫做成测试 + 说明标准做成文档章节，两者互相引用

只写文档没人看，只写测试没人知道为什么。做法：
- `docs/mcp.md` 新增**「工具说明标准」**一节（含本次的反例、四要素模板、判定表）；
- 测试与 panic 的文案统一指向该章节，并说明"下一步怎么做"；
- `docs/PROCESS.md` 的提交前自检加一条，把"改了工具说明/字段"与"必须补形状与示例"绑在一起。

### D5 「不存不用」的字段不写进 schema

`pricing.When.monthly_usage` 与 `when.region` 在 `docs/pricing.md` 里有描述，但
`internal/pricing/rule.go` 的 `When` 结构体里**根本没有这两个字段**（`ParseRuleSet` 又会拒绝未知字段）。
schema 里绝不能出现它们，否则 agent 会写出"看着有效、实际 400"的文档。
同理，模型级与路由级 `policy`（`models.policy_json` / `routes.policy_json`）**运行时没有任何读取方**
（全仓只有 `key.PolicyJSON` / `tag.PolicyJSON` 被 `routing.mergePolicy` 与 `billing.policyMarginBP` 读），
必须在描述里标明"当前不生效"，而不是假装它能配置。

## 4. 接口

### 4.1 `adminField` 携带形状与示例（`internal/httpapi/admin_routes.go`）

```go
type adminField struct {
	Name     string
	Type     string
	Desc     string
	Required bool
	Enum     []string
	// Schema 非空时取代按 Type 生成的 {"type": Type}：复杂对象的字段名、单位与约束写在这里。
	Schema map[string]any
	// Example 非空时取代占位示例（sampleForType 会给 object 生成 {}，那等于没写）。
	Example any
}

func schemaField(field adminField, schema map[string]any) adminField
func exampleField(field adminField, value any) adminField
func objectShapeField(field adminField, schema map[string]any, example map[string]any) adminField
```

`adminRoute.bodySchema()` 在字段声明为 `object` 且既无 `Schema` 也无形状时 **panic**；
`adminRoute.example()` / `sampleBody()` 优先使用 `Example` 与 `Schema.example`。

`summaryRow()` 增加 `body_fields`：`admin_endpoints` 的概览行直接列出该接口的 body 字段名，
让 agent 少一次 `admin_describe` 往返（附加字段，向后兼容）。

### 4.2 价格规则集 schema（新增 `internal/httpapi/admin_pricing_schema.go`）

```go
// pricingRuleSetSchema 描述 pricing.RuleSet（成本侧 pricing_rules 与售价侧 sale_pricing 同形）。
func pricingRuleSetSchema() map[string]any
// pricingRuleSetExample 给出可直接提交的一份 catch-all 成本文档。
func pricingRuleSetExample(currency string) map[string]any
```

覆盖：`currency`（3 位大写，缺省=账本币种）、`basis`、`markup_bp`、`dimension_markup_bp`、
`rules[]`（`id`/`title`/`order`/`when`/`rates`/`per_request_fee_micros`）；
`when` 的 `time_windows[]`/`valid_from`/`valid_to`/`tier`/`model_variant`；
`rates` 的键为计量维度（`input`/`input_cache_hit`/`input_cache_miss`/`output`/`reasoning`），
**单位写死在描述里**：微单位 / 百万 token（`200000` = $0.20/1M）；
`additionalProperties: false` 与 `DisallowUnknownFields` 对齐。

接线四处：`admin_upsert_provider_model.pricing_rules`、`admin_upsert_model.sale_pricing`、
`admin_update_model.sale_pricing`、`admin_validate_pricing`（`RawBody` 由 `freeFormSchema` 换成规则集 schema）。

### 4.3 查询工具说明（`internal/mcpsrv`）

11 条描述改为中文并按四要素写（用途 / 何时用与分工 / 参数默认值与单位 / 「返回：」段），
`queryToolNames` 从同一张声明表派生，避免"名字三处写"的老问题扩散到描述上。

### 4.4 提示词（`internal/chat/prompt.go`）

基础提示词增加「信息不足先问」规则；`DefaultInlineFormInstructions` 标明表单是**默认**交互手段，
`DefaultUIBridgeInstructions`（整页 HTML）标注为"仅在需要自由排版/脚本时"。

## 5. 测试策略

| 测试 | 断言 | 防的是什么 |
|---|---|---|
| `TestQueryToolDescriptionsAreComplete`（mcpsrv） | 每条描述含中文、含「返回：」且其后有内容 | 说明退化为一句术语 |
| `TestQueryToolDescriptionsCoverTheirSchema`（mcpsrv） | `inputSchema.properties` 的每个属性名都出现在描述里；每个属性有 `description` | 加了参数不写说明 |
| `TestStructuredBodyFieldsCarryTheirShape`（httpapi） | 任何 `object` 字段没有 `Schema` 即失败；任何 body 字段描述为空即失败 | **本次的根因**（名词式描述 + 只有 `{"type":"object"}`） |
| `TestBodyFieldsAreDocumentedInTheCatalogue`（httpapi） | `body_fields` 与声明的字段一一对应 | 概览行与 describe 不一致 |
| `TestGeneratedExamplesSatisfyTheirSchema`（httpapi） | 每个 body 字段的生成示例满足它自己的 schema；整份示例 body 满足 `bodySchema()` 发布的 schema | **示例与 schema 各说各话**（发现时抓到 `markup_bp`/`context_window` 被渲染成 `{}`） |
| `TestMCPDescribeCarriesThePricingRuleSchema`（httpapi） | `admin_describe` 里 `pricing_rules` 含 `rules`/`rates`/`when`/`currency` 与单位字样，示例的 `when` 是空对象 | 形状写了但没接到 describe |
| `TestMCPPricingExampleIsWritable`（httpapi，端到端） | 文档示例喂给 `admin_validate_pricing` → `valid:true`；再真实写库 → 200 且读回一致 | **说明与写入契约脱节** |
| 既有 `TestInlineFormContractMatchesTheRenderer` 等 | 表单契约标记仍在 | 提示词改写破坏契约 |

## 6. 异常与边界

| 情况 | 行为 |
|---|---|
| 新增 body 字段但不写 `Desc` | 测试失败（文案指向 `docs/mcp.md` 标准一节） |
| 示例与 schema 冲突（例如 object 被渲染成 `{}`） | `TestGeneratedExamplesSatisfyTheirSchema` 失败 |
| 新增 `object` 字段但不给形状 | 构造期 panic + 测试失败 |
| 文档示例与写入侧校验不一致 | `TestMCPPricingExampleIsWritable` 失败 |
| 只有存、没有读的字段（`monthly_usage`、模型/路由 `policy`） | 不写进 schema；描述里标明"当前不生效" |
| 表单收集凭据 | 仍被拒绝（M35 硬边界不变）；表单只是收集参数，不等于人工确认 |
| 危险接口 | 仍必须 `confirm=true`（M21 不变） |

## 7. 不做的事（边界声明）

- 不改权限模型、不加端点、不做迁移、不改工具名与调用签名。
- 不修「tag 的 policy 未经 `keyPolicyDocument` 校验」这一既有不一致（本次只在描述里如实标注），
  也不修「模型/路由 policy 未生效」——两者都记入 `docs/TODO.md` 观察项。
- **不改「模型级 `sale_pricing.markup_bp: 0` 等同于未设置」这一语义**：它会让 0 回落到
  `billing.default_markup_bp`。这是既有行为（`ResolveMarkup` 只在 >0 时采信模型倍率），
  改它属于计价语义变更；本次只把这件事写进 schema 与规格文档，并把示例改成 11000，
  使"照抄示例"得到的倍率与示例所示一致。
  真正的 0% 用 Key/tag 的 `policy.margin_bp: 0`（那条链是显式的），文档已指明。

## 8. 实现与设计差异

实现过程中偏离本文档的地方，以及原因：

1. **`objectField` 改名 `structuredField(name, typ, desc, schema, example)`**：`internal/httpapi`
   已有一个 `objectField([]byte, string)`（聊天工具参数解析用），同名会冲突；同时它也需要服务
   `array` 类型（`aliases`/`tags`/`event`），所以名字按"产生一个有形状的字段"而不是按 JSON 类型取。
2. **schema 的构建从 `objectSchema(...)` 改为 `schemaForFields(where, fields)`**：panic 需要知道
   是哪条路由的哪个字段，`objectSchema` 只有字段列表、拿不到上下文。`objectSchema` 仍被
   `RawBody` 与工具自带 schema 使用，未改签名。
3. **多出一组守卫测试**（本文档只列了对 `object` 无形状的检查）：
   - `TestGeneratedExamplesSatisfyTheirSchema`：把"示例必须能被端点接受"从
     `TestMCPPricingExampleIsWritable` 一个端点的端到端证明，扩展成**每条路由、每个字段**的
     静态检查。写它的时候立刻抓到两个真实缺陷：`sampleBody` 对带 `Schema` 的字段一律套用
     "按 properties 生成对象"，把 `context_window`（integer）和 `capabilities_override`（enum）
     渲染成 `{}`；以及函数名/示例与逻辑错位。
   - `TestBodyFieldsAreDocumentedInTheCatalogue`：`body_fields` 是新增的公开字段，需要自己的守卫。
4. **`exampleField` 会把示例回写进 `Schema["example"]`**：`body_schema` 与 `example` 是同一份
   事实的两个视图，分开维护必然会漂移；回写后"只读 body_schema 的客户端"也能拿到可抄的值。
5. **`summaryRow()` 的 `body_fields` 用 `[]string`**：与既有 `params` 一致，避免影响既有断言。
6. **`pricingRuleSetSchema` 里 `required: ["rules"]` 是条件性的**：`pricing.Validate` 只对
   `basis=absolute` 要求规则；JSON Schema 的扁平 `required` 表达不了，于是 schema 里加了显式的
   `x-rules-required-unless: ["basis=cost_follow"]` 标记供守卫测试读取，同一条件也写进了 `rules`
   与 `basis` 的描述给 agent 看。条件写两处是刻意的：两个读者，各自可读。
7. **新增 `admin_field_schemas.go` 而不是把 schema 内联在路由表里**：路由表要能一眼读完；
   同时这些 schema 需要复用（`admin_create_key`/`admin_update_key`/tag 的 policy 完全同形）。
8. **发现并修掉的既有错误**（不在本文档最初范围内，但属于同一缺陷族）：
   - `admin_simulate_pricing.dimensions` 的描述声称键是 `input_tokens` 一类，实际是计量维度名
     （`input`/`output`/…），已改正；
   - `aliases`（字符串数组）曾被写成 `Type: "object"`；
   - `accounts.price_overrides` 的 schema 初次命名成"模型级"（作用域写错），已改为账户级并在
     描述里注明它与 `docs/pricing.md` §3 的差异。
9. **模型级 `markup_bp: 0` 的陷阱**：走查（在隔离实例上按运维的原场景跑一遍 describe → validate →
   write → simulate）时发现示例里的 `markup_bp: 0` 实际按 10000 计费。示例已改为 11000，
   `TestMCPPricingExampleIsWritable` 增加"写入的倍率必须与试算出的 `markup_bp` 一致"这条断言，
   成本 1.6 USD 也被钉住（验证单位换算没写反）。试算器与数据面的倍率口径差异记入 TODO 观察项。
10. **查询工具说明的表化**：实现时把 11 条声明收进 `queryTools()` 方法（需要实例才能读到
    `mcp.max_query_rows` 写进 `limit` 的说明），`queryToolNames` 由它派生，
    `Tool{...}` 的组装只剩一层循环。
11. **提示词多了「不要拦着用户填表」一句**：原设计只写了"缺关键信息先问"，实测这种单边指令
    会让模型对"这个月花了多少"这类问题也反问窗口。反例与正例一起写，并由
    `internal/chat/prompt_test.go` 钉住。
