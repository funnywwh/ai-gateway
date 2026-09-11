# 设计：M22 多币种（模型级币种 + 单一账本换算 + 可选显示币种）

> 需求原文：「不同的模型用不同的币种，默认 USD，显示时可以选择币种」。
> 已确认的四个口径：① 单一账本币种 + 结算换算；② 成本侧与售价侧各自可配币种；
> ③ 汇率 = `config.yaml` 静态表 + 控制台设置页可覆盖（审计、立即生效）；
> ④ 显示币种覆盖管理控制台，并顺带修正 `/v1/models` 与 MCP 的币种字段。
>
> 本文档先于代码存在；「实现与设计差异」一节实现完成后回填。

## 1. 目标

1. 模型的**成本币种**（我们付上游的币种）与**售价币种**（对客报价的币种）可以各自声明，缺省 = 账本币种（`billing.currency`，默认 `USD`）。
2. **账本永远单一币种**：余额、授信上限、在途预留、账本条目、用量、发票、对账全部继续是账本币种的整数微单位；跨界金额在**结算时**按整数汇率换算入账。
3. 每次计价的**原生金额与所用汇率**写进 `pricing_snapshot_json`，因此历史账单在没有网络、没有当前汇率表的情况下依然可以逐笔复算（延续 `docs/pricing.md` §5 的可复算承诺）。
4. 管理控制台**可以选择显示币种**：账本类金额按所选币种换算显示（带 `≈`），计价类金额永远按模型原生币种显示（带币种码）。
5. 对外契约的币种字段不说谎：`GET /v1/models` 的 `x-gateway-pricing.currency` 是该模型的售价币种；MCP 金额字段带真实币种。

## 2. 现状与约束（改动前的事实）

| 事实 | 位置 |
|---|---|
| 金额一律 `int64` 微单位且**隐含单一币种** | `docs/billing.md` §1；`accounts.balance_micros`、`ledger_entries.amount_micros`、`usage_records.*_micros`、`billing_reservations.reserved_micros` 均无币种列 |
| 只有发票带币种列（默认 `USD`，写入时取 `billing.currency`） | `internal/store/migrations/0001_init.sql`、`internal/billing/invoice.go` |
| `pricing.RuleSet` 已有 `Currency` 字段，**全仓库无人读写** | `internal/pricing/rule.go:92` |
| `PATCH /pricing/markup` 已承诺「`rules` 数组、`currency` 等其它键原样保留」 | `docs/design/pricing-ui.md` §6.3、`internal/httpapi/admin_pricing_targets.go` |
| `billing.display_currency_rate`（float）与 `billing.reserve_micros_default` 是从未被读取的死配置 | `internal/config/config.go` |
| 控制台 4 个页面各自复制了 `micros/1e6` 的 `money()`，标签硬编码「USD」「微美元」 | `accounts.js`、`billing.js`、`codes.js`、`pricing.js` |
| 管理面接口只能经路由表声明（否则 `admin_routes_test.go` 失败） | `internal/httpapi/admin_routes.go` |

## 3. 关键决策与取舍

1. **账本单一币种 + 结算换算**（不做多币种分账）。多币种余额/授信/预留/发票/对账要重建五套视图，收益不抵成本；换算把改动收敛在「计价 → 入账」一处。
2. **币种是规则集的属性**，不新增表列、不新增数据库迁移：成本币种写在 `provider_models.pricing_rules_json.currency`，售价币种写在 `models.sale_pricing_json.currency`。好处：复用已存在的字段与「保留未知键」的写入语义，写权限与审计路径不变。代价：币种随规则文档一起存，`currency` 的合法性只能由解析器与接口层校验。
3. **汇率语义**：`billing.fx_rates: {CNY: 141000}` 表示 **1 CNY = 141000 微账本币种**（0.141000 USD）。纯整数，账本币种隐含 `1e6`，且禁止出现在表里（避免两处定义打架）。
4. **取整方向**：外币 → 账本一律 `ceil`（与 `docs/pricing.md`「宁可高估成本」的原则一致：不低估收入、不低估成本）；账本 → 显示币种用四舍五入（展示不偏）。**换算只发生在两处**：结算入账（写金额）与前端展示（不写回任何金额）。
5. **`cost_follow` 跨币种必须「先换币、再乘倍数」**。改动前 `售价 = 成本 × 倍数` 是逐维度直接乘；若成本币种 ≠ 售价币种，那等于把 CNY 的数字当成 USD 收。新规则逐维度：`saleRate = ceil(costRate × rate(cost→sale) / 1e6)`，再 `ceil(saleRate × markupBP / 10000)` 得到金额。同币种时 `rate = 1e6`，与改动前逐位等价（回归由既有测试保证）。
6. **`min_charge_micros` 仍按「售价币种（原生）」解释**：与规则里的 `rates` 同口径；配置里的全局默认值对 USD 模型语义不变，跨币种模型的门槛按该模型币种理解（文档写明）。
7. **缺汇率不打断客户请求**：延续 `ruleSetsFor` 的既有原则（计价错误降级、不让请求失败）。写入时 400 拦截；运行时（手改库造出）该次 cost/charge 记 0、快照标 `fx_unavailable`、Error 日志 + hook + invariants 计数；预留回退到 `billing.reserve_micros_default`（顺带启用这个既有死配置）。
8. **显示币种是视图偏好**，不是账务口径：`localStorage` 记住每台浏览器的选择，`GET /admin/api/v1/billing/currency` 提供服务端默认值（`billing.display_currency`）。发票/账本的正式币种永远是账本币种。
9. **不新增 Go 包**（换算放进 `internal/pricing`），因此 `internal/arch` 分层表不需要改；`internal/mcpsrv` 也不新增 `internal/pricing` 依赖（MCP 只从售价 JSON 里读一个 `currency` 键）。

## 4. 接口（类型与签名）

### 4.1 `internal/pricing/currency.go`（新增）

```go
var CurrencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// NormalizeCurrency 去空白并大写；空串返回 ""（由调用方补默认值）。
func NormalizeCurrency(code string) (string, error)

// FXTable：Rates[X] = 1 单位 X 折多少微账本币种；Ledger 自身隐含 1e6。
// 零值（Ledger == ""）表示「关闭换算」：所有币种按账本币种解释，
// 于是未接线的调用方（测试、旧路径）行为与改动前逐位一致。
type FXTable struct {
    Ledger string
    Rates  map[string]int64
}

func (t FXTable) Enabled() bool
func (t FXTable) RateOf(code string) (int64, bool)          // 账本/空串 -> 1e6
func (t FXTable) ToLedger(micros int64, from string) (int64, bool)   // ceil
func (t FXTable) FromLedger(micros int64, to string) (int64, bool)   // half-up
func (t FXTable) Convert(micros int64, from, to string) (int64, bool) // 以账本为中转
func (t FXTable) Codes() []string                            // 账本 + 排序后的汇率键

// FXStore 是运行期可替换的汇率表（config 初始化，设置页覆盖）。
type FXStore struct{ /* atomic.Pointer[FXTable] */ }
func NewFXStore(ledger string, rates map[string]int64) *FXStore
func (s *FXStore) Snapshot() FXTable
func (s *FXStore) Replace(ledger string, rates map[string]int64)
```

### 4.2 `internal/pricing`（改动）

```go
// RuleSet.Currency 参与校验：非空时必须匹配 ^[A-Z]{3}$；解析后规整为大写。
func Validate(set *RuleSet) error

// Input 新增两个字段；零值 = 不换算。
type Input struct {
    ...
    Ledger string    // 账本币种
    FX     FXTable   // 汇率表
}

// Result：CostMicros/ChargeMicros 语义明确为「原生币种」；
// 新增账本币种金额与币种/汇率信息。
type Result struct {
    CostMicros       int64  // 成本币种原生
    ChargeMicros     int64  // 售价币种原生
    CostCurrency     string
    SaleCurrency     string
    LedgerCurrency   string
    LedgerCostMicros   int64 // 账本币种，ceil
    LedgerChargeMicros int64 // 账本币种，ceil
    FXUnavailable    []string
    ...
}

// Snapshot 新增（全部 omitempty，旧记录不受影响）：
//   cost_currency / sale_currency / ledger_currency
//   cost_micros_native / charge_micros_native
//   ledger_cost_micros / ledger_charge_micros
//   fx_cost_ledger / fx_sale_ledger / fx_cost_sale / fx_unavailable
```

### 4.3 `internal/billing`

```go
type EstimateInput struct {
    ...
    FX                   pricing.FXTable // 两侧 rate 各自换算到账本币种后再取 max
    DefaultReserveMicros int64           // 汇率缺失时的兜底预留（billing.reserve_micros_default）
}

func EstimateReserve(in EstimateInput) int64

// NewCharge 写 usage.cost_micros / charge_micros 与账本条目时使用
// result.LedgerCostMicros / result.LedgerChargeMicros（账本币种）。
func NewCharge(usage *domain.UsageRecord, result *pricing.Result, charge bool) *Settlement
```

### 4.4 `internal/httpapi`

```go
type Deps struct {
    ...
    FX *pricing.FXStore // 新增
    ReloadFX func(ctx context.Context) error // 新增：设置写入后重载汇率
}

// 新增路由表条目（admin_routes.go）：
//   GET /admin/api/v1/billing/currency
//   name=admin_get_billing_currency, group=billing, role=viewer
```

响应形状：

```json
{ "ledger_currency": "USD", "display_currency": "USD", "fx_source": "config|settings",
  "currencies": [{"code":"USD","rate_micros":1000000,"rate_source":"ledger"},
                 {"code":"CNY","rate_micros":141000,"rate_source":"settings"}],
  "missing_rates": ["EUR"] }
```

- `GET /pricing/targets`：每个 target 增加 `currency`、`currency_source`（`declared|ledger`）、`fx_rate_micros`、`fx_rate_known`；顶层增加 `ledger_currency`、`display_currency`。
- `POST /pricing/simulate`：增加 `ledger_currency`、`cost_currency`、`sale_currency`、`ledger_cost_micros`、`ledger_charge_micros`、`fx_cost_ledger`、`fx_sale_ledger`、`fx_unavailable`；`cost_micros`/`charge_micros` 保持原生语义。
- `PATCH /pricing/markup`：新增可选 `currency`（只改这一个键；空串 = 清除回继承）。
- 写入校验 `validateRuleSetCurrency`：用于 `POST|PATCH /models`、`PATCH /pricing/markup`、`POST /providers/{id}/models`（后者顺带补上缺失的 `ParseRuleSet` 校验）。
- `GET /v1/models`：`x-gateway-pricing.currency` = 模型售价币种（缺省账本币种）。
- `PUT /settings/billing.fx_rates`：该 key 单独做结构校验（非法 400），成功后调 `ReloadFX` 并写审计；其它 key 保持自由 JSON。

### 4.5 控制台（`internal/webui/static/js/`）

- 新增 `money.js`：`initCurrency()`、`currencies()`、`currentCurrency()`、`setDisplayCurrency(code)`、`money(micros)`（账本 → 显示，BigInt 整数换算，`≈` 前缀 + 币种后缀）、`moneyIn(code, micros, rateMicros)`（原生币种，只带码不换算）、`missingRates()`。
- `app.js` 顶栏挂 `<select>`（标题「显示币种」）；切换后重渲染当前页（`router.js` 导出 `rerender()`）；`missing_rates` 非空显示红色徽标。
- `accounts.js` / `billing.js` / `codes.js` / `pricing.js` 的重复 `money()` 与硬编码币种标签统一到 `money.js`。
- 显示规则：**账本类金额**以账本币种为基准、按选择换算并带 `≈`；**计价类金额**永远按模型原生币种显示、带币种码、不换算。

### 4.6 MCP

- `getModels`：`currency` = 售价文档里的 `currency`，缺省 `cfg.Currency`。
- 金额工具：新增不带 `_usd` 后缀的 `balance`/`charge`/`cost`/`margin` 并始终附 `currency`；`*_usd` 仅当账本币种为 USD 时输出（兼容旧客户端）。

## 5. 数据流（一次请求）

```
ruleSetsFor → cost RuleSet(currency 可空) + sale RuleSet(currency 可空)
Admit       → EstimateReserve：两侧 worst-case rate 各自换算到账本币种后取 max
              缺汇率 → DefaultReserveMicros
priceAttempt→ pricing.Evaluate(Input{Ledger, FX, ...})
                cost_lines   : 成本币种原生
                sale_lines   : cost_follow → ceil(成本单价 × rate(cost→sale)) × markup
                               absolute    → 售价币种原生
                LedgerCost   = ceil(成本总额 × rate(cost→ledger))
                LedgerCharge = ceil(售价总额 × rate(sale→ledger))
NewCharge   → usage.cost_micros = LedgerCost；charge_micros = LedgerCharge
              账本条目 -LedgerCharge（账本币种）；快照含原生金额与两级汇率
控制台       → GET /billing/currency 取汇率表 → 前端展示换算（不写回）
```

## 6. 异常与边界

| 场景 | 行为 |
|---|---|
| 同币种（未配 `currency`） | 汇率恒 `1e6`；`Rate`/`AmountMicros` 与改动前逐位一致（既有测试即回归网） |
| 旧历史记录（快照无币种字段） | 按账本币种解释；`rebuild-ledger` 只重放 `usage.charge_micros`，不受影响、不回溯改写 |
| 缺汇率（写入时） | 400，提示「请先在 设置 → 汇率表 补 X 的汇率」；`/pricing/targets` 与页面红色徽标同时暴露 |
| 缺汇率（运行时） | cost/charge 记 0 + 快照 `fx_unavailable` + Error 日志 + `billing.fx_missing` hook + invariants 计数；不改路由、不返回错误 |
| `fx_rates` 含账本币种 / 值 ≤ 0 / 非法码 | 启动校验失败（config）；设置页写入 400 |
| 跨币种 `cost_follow` | 先换币再乘倍数；快照记 `fx_cost_sale` 以便复算 |
| 跨币种 + `absolute` | 直接用售价币种规则，不涉及成本币种 |
| `min_charge_micros` | 按售价币种（原生）先应用，再换算入账 |
| 显示币种 = 账本币种 | 不加 `≈`；输出与改动前一致 |
| `display_currency` 无汇率 | 启动校验失败；老配置里的 `display_currency_rate` 被忽略（yaml 非严格解析） |
| 跨币种换算导致 `charge_ledger < cost_ledger`（1.0× 且两侧独立取整） | 允许；`invariants` 只断言同币种不变式（账本条目和 == usage 和），不新增跨币种毛利断言，文档写明 |
| 溢出 | `a × rate` 在 int64 内的现实额度（≤1e12 单位 × ≤1e9 微）安全；仍按 `mulDivCeil` 的既有护栏处理 |

## 7. 测试策略

1. `pricing/currency_test.go`：双向换算与取整方向、账本恒等、零值 FXTable 关闭换算、缺汇率 `ok=false`、非法码、`FXStore.Replace` 原子性。
2. `pricing_test.go`：`currency` 校验（`cny`/`RMB` 拒、`CNY` 规整）；跨币种 `cost_follow` 的单价与金额可复算；快照新字段；`min_charge` 原生口径；**快照复算**（用快照的币种 + 汇率重放得到同样的账本金额）。
3. `billing_test.go`：`NewCharge` 写入账本金额；`EstimateReserve` 跨币种两侧换算后取 max（同币种结果与改动前相等）；缺汇率 → `DefaultReserveMicros`。
4. `config_test.go`：`fx_rates` 解析 + 五条校验各一例。
5. `httpapi`：写入校验 400 三例；`/billing/currency` 形状与 `missing_rates`；`/pricing/targets` 币种字段；`/pricing/simulate` 双币种金额；`/v1/models` 币种；端到端（CNY 售价模型 → usage/账本/快照断言）。
6. 变异验证（至少各一次）：`ceil` 改截断、`cost_follow` 改回「先乘倍数」、`missing_rates` 恒空 → 对应测试必须精确失败。
7. 前端：`scripts/ui-harness` 新增 `currency` 视图（`currency.page.html` + fixtures + run.sh 的「视图 → 模板」映射），断言默认无 `≈`、切到 CNY 后出现 `≈`/后缀/正确数值、缺汇率红色徽标、无页面错误。

## 8. 依赖与不做

- 依赖：无新增第三方依赖，无数据库迁移。
- 不做：多币种账本（每账户/每币种余额）、按模型币种开票、汇率联网自动抓取、历史账单按新汇率回溯重算、客户门户 UI（仓库目前只有门户身份层）、`account.price_overrides` 的完整规则文档（延续 `pricing-ui.md` 的既有「不做」）。

## 9. 实现与设计差异

实现与批准的设计一致；以下是落地时才定下来的细节与顺手修掉的缺陷。

1. **`billing.currency` 就是账本币种**，没有新增 `ledger_currency` 配置键：同名键在 M11 起就承担这个语义（发票币种、MCP 口径都用它），新增第二个名字只会让两处取值可能漂移。
2. **`billing.fx_rates` 只接受整数微单位**（1 CNY = 0.141000 USD 写作 `141000`）。最初考虑过「整数按微单位、小数按币种单位」的双解释，实测下来歧义太大（`141000` 到底是 0.141 还是 141000 单位？），最终只留一种写法，控制台把 `141000` 显示成 `1 CNY = 0.141000 USD` 帮助阅读。
3. **控制台换算用 BigInt 而不是浮点**：微单位 × 汇率会超过 JS 精确整数范围（2^53），展示换算也必须精确；服务端仍是全整数 `math/bits` 128 位中间值，溢出时**饱和到 MaxInt64**（宁可高估，不静默回绕）。
4. **`withLedgerKeys` 在 MCP 出口统一改名**：不是逐个工具改 map 字面量，而是在 `CallAs` 出口把 `*_usd` 补一个中性名、并在账本币种非 USD 时删掉 `_usd` 键。这样新增工具不会漏改，代价是 map 会被原地补键（工具结果都是本次调用新建的，安全）。
5. **`PATCH /pricing/markup` 新增可选 `currency`**：只改这一个键、空串表示清除回继承，与既有「保留 `rules` 数组」的约定一致；写入时立刻校验汇率，避免出现「存下来但收不到钱」的模型。
6. **顺手修掉两处既有缺陷**（都在本次必须改动的路径上）：
   - `EstimateReserve` 把 `per_request_fee_micros` 也除以了 `RateScale`，等于每请求固定费在预留里恒为 0；现在固定费按绝对金额单独相加（并按倍数放大）。
   - `POST /providers/{id}/models` 之前**不校验** `pricing_rules`，坏 JSON 能直接入库；现在与售价侧一样走 `pricing.ParseRuleSet` + 币种校验。
7. **`GET /admin/api/v1/billing/currency` 走路由表**：M21 之后管理面接口只能声明在 `admin_routes.go`，所以它同时自动获得 MCP 工具元数据；新增 pattern 已同步进 `admin_routes_test.go` 的 `expectedAdminPatterns`。
8. **充值/兑换码的请求体新增 `amount`（账本币种小数）**，`amount_usd`/`amount_micros` 继续可用：字段名里带 USD 在非 USD 账本下会误导，但直接改名会破坏既有脚本，所以是「加新名、留旧名」。
9. **`billing.display_currency_rate` 被删除**：这个 float 配置从未被任何代码读取（`display_currency` + `fx_rates` 取代它）。yaml 解析非严格，旧配置文件里留着该键不会报错。
10. **`docs/mcp.md` 的金额字段是行为变更**：账本币种非 USD 时不再输出 `*_usd`。这是刻意的（该字段名会撒谎），已写进规格文档。

## 10. 实测记录（离线 testecho 实例，2026-09-11）

临时实例 `127.0.0.1:8099`（`.cache/e2e-currency/`，独立数据库，未入库），
模型 `echo-cny`：成本规则 `{"currency":"CNY","rates":{input:2000000,output:4000000}}`，
售价 `{"currency":"CNY","basis":"cost_follow","markup_bp":20000}`，
账本 `USD`、`fx_rates.CNY=141000`：

| 证据 | 结果 |
|---|---|
| `GET /v1/models` | `"x-gateway-pricing":{"currency":"CNY",...}` —— 报的是模型售价币种 |
| 一次 `POST /v1/responses`（input 25 / output 17） | 快照：`cost_micros_native=118`、`charge_micros_native=236`（2.0× 倍数）、`cost_currency=CNY`、`sale_currency=CNY`、`ledger_currency=USD`、`fx_sale_ledger=141000` |
| 入账 | `ledger_cost_micros=17`（=ceil(118×0.141)）、`ledger_charge_micros=34`（=ceil(236×0.141)）；`usage_records` 与账本条目同为 17/34，余额 -34 |
| 写入校验 | 无汇率的 `currency":"EUR"` → **400** `currency EUR has no exchange rate; add it to billing.fx_rates …` |
| 汇率热更新 | `PUT /settings/billing.fx_rates {"CNY":150000}` → `/billing/currency` 立刻显示 `rate_source=settings`；下一次请求按 150000 入账（cost 12 / charge 23），**第一条历史快照仍是 141000** |
| 控制台 | `make ui-check` 全视图通过，新增 `currency` 视图 16 项断言（默认无 `≈`、切 CNY 后 `≈3.546099 CNY`、缺汇率徽标、无页面错误） |
