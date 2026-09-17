# M56 设计文档：供应商成本上限与复位

> 起因（用户原话）：「供应商成本要能设置上限，可以复位」。
> 口径当场确认（三问三答）：① 指的是**按供应商累计「我们付给上游的成本」设上限 + 把累计复位**，
> 不是成本单价（`pricing_rules`）的写入校验；② 周期**每供应商可选**（不限周期 / 每天 / 每月）；
> ③ 达到上限后**从路由候选中剔除**，自动故障转移到其它候选。
>
> 本文档在编码前输出，实现后回填第 8 节「实现与设计差异」。

## 1. 目标

让"这家上游最多让我花多少钱"从一个只能靠人肉盯请求日志的问题，变成一个**可配置、可读回、可复位、
会被路由层执行**的运行时约束：

1. 每个供应商可设 `cost_limit_micros`（账本微单位，0 = 不限，默认）与 `cost_period`
   （`none` / `daily` / `monthly`），随时可复位（起算点挪到当前时刻）；
2. 该供应商的**累计成本达到上限后不再被选中**：同层/跨层候选继续按既有策略故障转移；
3. 全部候选都达上限时，客户端拿到 **503 `provider_cost_capped`**——既不冒充上游故障（502
   `upstream_error`），也不冒充客户端配额（429 `rate_limit_exceeded` / `provider_busy`）；
4. 控制台（`GET /providers`、`GET /providers/{id}`）与 MCP（`admin_list_providers` /
   `admin_get_provider` / `admin_stats`）能读回「已用 / 上限 / 是否超限 / 起算时刻」，并能写上限、周期与复位；
5. `cost_limit_micros = 0`（默认）时行为与 M56 之前**逐字节一致**：不查库、不筛选、不出现新字段。

**非目标**

- **不**改账号/Key/标签侧的 `monthly_cost_micros`（现状"只解析不执行"，见
  `internal/quota/limiter.go` 的注释）；本次是**供应商侧**的成本护栏，两者口径不同（一个是客户花了多少，
  一个是我们付给上游多少）。
- **不**做「供应商 × 模型映射」粒度的上限：运营者心智是"这家上游最多花多少"，一个供应商的所有模型共用额度。
- **不**把上限写进 `config.yaml`（`bootstrap.providers`）：与 `providers.max_inflight` 同口径，
  运行期旋钮由控制台/管理 API 拥有，引导流程只管"有哪些供应商"。
- **不**新增后台端点：复位复用 `PATCH /admin/api/v1/providers/{id}`（字段 `reset_cost`），
  与既有 `reset_cooldown` 同形。
- **不**升 `VERSION`、不打 tag、不发版。

## 2. 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | 上限计的是 `usage_records.cost_micros` 按 `provider_id` 的累计值，**账本币种** | 与请求日志「成本」列、M53 供应商维度统计、发票明细**同源**：控制台显示的成本与路由判定用的成本必须是同一个数。`settle.go` 把 `LedgerCostMicros` 写进 `usage_records.cost_micros`，所以这里天然是账本口径，不需要汇率换算。**否决**按 NATIVE 币种累计：一个供应商的不同模型可以各自声明币种，累计会变成加法错误 |
| D2 | 上限与周期存在 `providers` 三列（`cost_limit_micros` / `cost_period` / `cost_window_start`），**不**新建表 | 配置是一对一、粒度就是供应商实例；新表多一次 join 与一套外键生命周期（删除供应商时要级联）。与 `max_inflight` 放在同一张表同一行，控制台/API 的读写路径完全复用它 |
| D3 | 已用**不落库**，由计量表按时间窗实时聚合；`cost_window_start` 就是"起算点" | "已用"是可从真相推导的派生值，落一列累加器就要面对重放、回滚、批写失败、多实例四类分叉。**否决** `providers.cost_used_micros` 累加列（M12 对账/补偿那套复杂度会重演） |
| D4 | 复位 = **把起算点写成 now**，不修改、不删除任何计量行 | 复位可审计（审计行里就是 `cost_reset`）、可理解、不可能丢账。**否决**"清零已用列"（派生值与账分叉）、**否决**"插入一条负向调整记录"（会让发票/对账口径污染） |
| D5 | 起算点 = `max(周期起点, 手动复位时刻)`；周期起点按 **UTC** 计算（`daily`=UTC 零点，`monthly`=UTC 月初） | 一处纯函数决定，控制台/路由/统计不会各算一套。UTC 与 `usage_counters.period`（`YYYY-MM`）的既有口径一致。**否决**本地时区：部署跨时区时"每月几号归零"会变成不可推理的问题 |
| D6 | **首次启用上限时自动起算**：`cost_limit_micros` 从 0 变为正数且从未复位过 → `cost_window_start = now` | 否则给一个跑了两周的老供应商设上限，历史成本会**瞬间**把它判超限，运营者只会看到"刚设完就全挂了"。这也让"上限 = 从现在起的预算"成为可预期的语义。**否决**默认从有记录以来：见上；真要按历史算，先复位到想要的时间点（文档写明） |
| D7 | 读数是**进程内缓存 + 后台 5 秒刷新**（`runtime.CostTracker`），周期为**常量**而非新配置项 | 热路径（候选选择）必须是内存判断：每次候选选择打一次 SQL，会把"按供应商统计成本"的成本摊到每个请求上。5 秒的代价是"最坏超额 = 这 5 秒内该供应商的流量成本"，对一个运营护栏可接受，并被文档与控制台读回的数（`as_of`）如实标注。**否决**新增 `routing.provider_cost_refresh_s`：多一个旋钮、多一份文档与校验，而 5s 与 1s 的语义差别只是这几秒的钱，收益不足 |
| D8 | 读数失败（DB 不可读）**fail-open**：不阻断流量，保留最后一次成功读数，错误进 `/stats` 的 `last_error` | 成本护栏不该在数据库短暂不可读时掐断全站流量。这条与 M44 的并发门不同（那个 fail-open 没有代价：没有读数就没有名额），所以必须在文档与 `/stats` 里说清"读数是护栏、不是账" |
| D9 | 判定在**路由层**（`Router.filterRoute` / `buildPinned`），不在 `Dispatcher` 的 `admit` 里 | 剔除候选 = 让这次请求**用下一个候选**，而不是"尝试失败再换"：省一次出网点、多一层优先级降级机会，并且能出现在 `GET /router/explain` 的 `excluded` 里（运营者唯一能自问"为什么不用这家"的地方）。**否决**做成 `Dispatcher` 的 `ErrProviderBusy` 式拒绝：那会把 503 语义混进"可重试失败"，且 Explain 里看不到原因 |
| D10 | 判定依据 = 注册表快照里的**当前上限** × 追踪器里的**缓存已用** | 上限调高**立即生效**（比较用的是新上限），上限调低、复位、周期切换最多滞后一个刷新周期。这样"改配置立刻生效"这件运营者最在意的事不需要刷新参与 |
| D11 | 周期切换那一刻**保守**：缓存读数属于旧窗口，判定继续按旧值（可能仍拦截），下一个 tick（≤5s）用真实值修正 | 护栏偏保守只会短暂少用一家上游；偏乐观会在月初开门放行一段超额流量。手动复位不走这条路（`MarkReset` 立即归零，见 D12） |
| D12 | 复位由写路径显式通知追踪器（`MarkReset`），本地立即归零 | 复位后的真值就是 0（起算点 = now，之后才可能有新成本），本地归零不是猜测而是**精确值**：下一次刷新算出来的也是它。这样控制台点完复位、供应商立刻回到候选里，没有"等 5 秒"的诡异体验 |
| D13 | 全部候选都超限 → 新增 `domain.ErrProviderCostCapped`（**503**，code `provider_cost_capped`） | 502 `upstream_error` 会让人去查上游（上游好好的），429 会让客户端退避重试（重试一万次也不会变好）。503 + 专属 code 才是"这个部署的这家/这些上游额度用完了"。判定规则刻意简单：**所有**剔除原因都是 `cost_cap_reached` 时才是它，与 capability/not_granted 的既有优先级不变 |
| D14 | 查询走新索引 `usage_records(provider_id, created_at)`；按**相同起算点分组**，每组一条范围扫描 | 没有这个索引就是全表扫计量表（`usage_records` 是部署里最大的表之一）。分组是因为"从未复位"的供应商通常共享零值/同一时刻起算点，一次查询覆盖一批；复位过的各自一条，条数 = 有上限且各自复位过的供应商数（现实中个位数） |
| D15 | 一个上限都没有时**一次查询都不发** | 默认部署（没人用这个功能）的开销必须是零：刷新先看快照里有没有 `cost_limit_micros > 0` 的供应商 |
| D16 | 追踪器放 `internal/runtime`（新文件 `provider_cost.go`），端口 `CostSource` 由 `*store.DB` 结构满足，**不**改 `domain.Store` | 沿用 M44 D11 的先例（`capacity.go`）；`internal/routing` **不能** import `internal/store`（`internal/arch/layering_test.go` 的硬规则），所以路由侧只吃一个 `routing.CostGate` 接口，实现由 `cmd/aigw` 注入。`ProviderCostsSince` 不进 `domain.Store` 叙述接口，避免牵动所有测试假实现 |
| D17 | `Router.SetCostGate(g)` 而不是给 `routing.New` 加参数；nil gate = 不做成本筛选 | `routing.New` 有 1 处生产调用 + 9 处测试调用，加参数是纯噪声；nil = 未接线 = 行为与今天完全一致，是测试与未接线部署要的默认 |
| D18 | 上限/周期/复位的写入口只有 `admin_create_provider` / `admin_update_provider`（scope=admin），读回走既有的 provider 读接口 + `admin_stats` | 管理面刻意是"渐进披露三入口"，每个字段一个工具既贵又难发现。`MCP 工具说明标准`（`docs/mcp.md` §4.5）要求把**行为语义**写进 description，所以这次的工作量在说明文字与端到端证据，而不是新端点 |
| D19 | 控制台：列表加一列、详情加一组 kv、行操作加「复位成本」按钮、编辑表单加三个字段 | 「可以复位」是用户的原话，必须是**看得见的按钮**（而不是只在编辑表单里藏一个勾选框）。同时保留编辑表单里的 `reset_cost` 勾选框，与既有 `reset_cooldown` 同形，一次编辑就能顺手复位 |
| D20 | 被剔除的供应商在 `usage_records` 里**不留行**（请求根本没出网） | 与 `disabled`/`draining`/`not_granted` 等既有剔除一致：这些是"网关侧的本地拒绝"，只在 `request_logs` 里留下记录（M44 D9 的口径只针对"已拿到名额但被容量门拒绝"的尝试） |

## 3. 接口

### 3.1 数据模型（迁移 `0021_provider_cost_cap.sql`）

```sql
ALTER TABLE providers ADD COLUMN cost_limit_micros INTEGER NOT NULL DEFAULT 0; -- 0 = 不限
ALTER TABLE providers ADD COLUMN cost_period TEXT NOT NULL DEFAULT 'none';     -- none|daily|monthly
ALTER TABLE providers ADD COLUMN cost_window_start INTEGER;                    -- 最后一次起算/复位（NULL = 从未）
CREATE INDEX IF NOT EXISTS idx_usage_provider_time ON usage_records(provider_id, created_at);
```

### 3.2 领域层（`internal/domain/provider_cost.go`）

```go
const (
    CostPeriodNone    = "none"
    CostPeriodDaily   = "daily"
    CostPeriodMonthly = "monthly"
)

// NormalizeCostPeriod: "" 视作 none；其余非法值返回错误（写路径 400 用）。
func NormalizeCostPeriod(raw string) (string, error)
func ValidCostPeriod(raw string) bool

// ProviderCostWindowStart 是上限统计的起算时刻：max(周期起点, 手动复位时刻)。
// 零值时间表示"从有记录以来"。
func ProviderCostWindowStart(p *Provider, now time.Time) time.Time

// ProviderCostExceeded 报告累计是否已达到上限（CostLimitMicros <= 0 时恒 false）。
func ProviderCostExceeded(p *Provider, usedMicros int64) bool
```

`domain.Provider` 新增 `CostLimitMicros int64` / `CostPeriod string` / `CostWindowStart *time.Time`。

### 3.3 存储层（`internal/store/providers.go`）

```go
// ProviderCostsSince 返回每个供应商自各自起算点以来的累计成本（账本微单位）。
// 相同起算点的供应商合并成一条查询；结果里没有的 provider 由调用方按 0 处理。
func (db *DB) ProviderCostsSince(ctx context.Context, windows map[int64]time.Time) (map[int64]int64, error)
```

`providerCols` / `scanProvider` / `UpsertProvider` 同步三列；bootstrap 的 merge 分支先读既有记录
（`seedProviders`）再 upsert，所以引导**不会**重置上限——这条要有单测钉住。

### 3.4 运行时（`internal/runtime/provider_cost.go`）

```go
// CostSource 是读数需要的聚合读取；*store.DB 实现它。
type CostSource interface {
    ProviderCostsSince(ctx context.Context, windows map[int64]time.Time) (map[int64]int64, error)
}

const CostRefreshInterval = 5 * time.Second

type ProviderCostStat struct {
    LimitMicros int64     `json:"limit_micros"`
    UsedMicros  int64     `json:"used_micros"`
    Period      string    `json:"period"`
    WindowStart time.Time `json:"window_start"`
    Exceeded    bool      `json:"exceeded"`
}

type CostStatus struct {
    IntervalS int       `json:"interval_s"`
    AsOf      time.Time `json:"as_of"`      // 最后一次成功刷新
    LastError string    `json:"last_error"` // 空 = 健康
    Tracked   int       `json:"tracked"`    // 有上限的供应商数
}

func NewCostTracker(src CostSource, reg *registry.Registry, log *slog.Logger) *CostTracker
func (t *CostTracker) Refresh(ctx context.Context) error   // 同步；并发调用只跑一个（TryLock）
func (t *CostTracker) Start(ctx context.Context) func()    // 先同步 seed 一次，再起 ticker；返回 stop-and-wait
func (t *CostTracker) Stats() map[int64]ProviderCostStat
func (t *CostTracker) Status() CostStatus
func (t *CostTracker) Exceeded(p *domain.Provider, now time.Time) bool
func (t *CostTracker) MarkReset(providerID int64, at time.Time)
```

### 3.5 路由层（`internal/routing/routing.go`）

```go
// CostGate 报告某供应商是否已达到配置的成本上限；nil = 不做成本筛选（默认）。
type CostGate interface {
    Exceeded(p *domain.Provider, now time.Time) bool
}
func (r *Router) SetCostGate(g CostGate)
```

`filterRoute` 与 `buildPinned` 在被剔除时给出原因 `cost_cap_reached`；`noCandidatesError` 在
"所有剔除原因都是 `cost_cap_reached`" 时返回 `domain.ErrProviderCostCapped`。

### 3.6 领域错误（`internal/domain/errors.go`）

```go
// ErrProviderCostCapped 报告该模型的所有候选都达到了配置的供应商成本上限（M56）。
func ErrProviderCostCapped(msg string) *APIError // 503, type api_error, code provider_cost_capped
```

### 3.7 管理面（`internal/httpapi`）

```go
// providerBody 增（create / patch 共用）
CostLimitMicros *int64  `json:"cost_limit_micros"`
CostPeriod      *string `json:"cost_period"`
ResetCost       bool    `json:"reset_cost"`

// internal/httpapi/provider_cost.go —— 与 capacity.go 对称的窄端口
type ProviderCost interface {
    Stats() map[int64]runtime.ProviderCostStat
    Status() runtime.CostStatus
    MarkReset(providerID int64, at time.Time)
}
func attachCost(row map[string]any, p *domain.Provider, stats map[int64]runtime.ProviderCostStat, currency string)
func (s *Server) costBlock() map[string]any
```

- `providerJSON` 增配置态三字段（恒在，供控制台表单回填）；`attachCost` 只在有上限时挂
  `cost` 运行时块：`{"limit_micros","used_micros","period","window_start","exceeded","currency"}`。
- `applyProviderBody`：`cost_limit_micros < 0` → 400；`cost_period` 经 `NormalizeCostPeriod` → 非法 400；
  首次启用自动起算（D6）；`reset_cost` → `cost_window_start = now` 且写成功后 `MarkReset`。
- 审计 `changes` 增 `cost_limit_micros` / `cost_period` / `cost_reset`。
- `/stats` 增 `provider_cost` 块（`costBlock()`，无端口时不出现）；Prometheus 增
  `aigw_provider_cost_used_micros{target}` / `aigw_provider_cost_limit_micros{target}` /
  `aigw_provider_cost_exceeded{target}`。
- MCP 工具说明：`admin_create_provider` / `admin_update_provider` 的 `RawBody` 补三个字段
  （共享常量 `costLimitDesc` / `costPeriodDesc` / `resetCostDesc`，按 `docs/mcp.md` §4.5 写清单位、
  默认值、超限后果与复位语义），`Summary` 与 `maxInflightDesc` 同法提到读回 `cost`。

## 4. 数据流

```
① 配置写入（控制台 / MCP / curl）
   PATCH /admin/api/v1/providers/{id} {cost_limit_micros, cost_period, reset_cost}
     → applyProviderBody 校验 + 首次自动起算/复位
     → store.UpsertProvider（三列）
     → s.reload(): reg.Reload()（快照里立刻带上新上限；Router 因此立刻用新上限判定）
     → MarkReset（仅复位时）：追踪器该供应商读数归零
     → 响应 attachCost：{"used_micros":0,...}

② 后台读数（cmd/aigw 启动时 seed，之后每 5s）
   CostTracker.Refresh:
     快照里筛 cost_limit_micros > 0 的供应商（一个都没有 → 直接返回）
     每个供应商算起算点 domain.ProviderCostWindowStart(p, now)
     按相同起算点分组 → store.ProviderCostsSince（每组一条 (provider_id, created_at) 范围扫描）
     整体替换读数表；边沿触发日志（未超→超 Warn，超→未超 Info）

③ 请求路径（每个候选一次内存判断）
   Router.Plan → filterRoute:
     provider.Enabled / draining / not_granted / 冷却 / 熔断 / not_mapped / 能力
     → CostGate.Exceeded(provider, now)  ⇒ 剔除原因 cost_cap_reached
   全部候选被剔除:
     全为 cost_cap_reached → 503 provider_cost_capped
     否则沿用既有 403/400/502 优先级

④ 读回（控制台 / MCP / /stats）
   GET /providers、GET /providers/{id} → providerJSON + attachCost（含 as_of 语义的 5s 缓存值）
   GET /stats → provider_cost{refresh_s, as_of, last_error, tracked, providers{}}
```

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| `cost_limit_micros = 0`（默认） | 不查库、不筛选、无 `cost` 字段：与 M56 之前逐字节一致 |
| 上限调低到已用之下 | 最多 5 秒后被剔除（无需复位） |
| 上限调高到已用之上 | **立即**恢复被选中（比较用快照里的新上限，读数是旧已用） |
| 复位 | 本地立即归零 + 起算点 = now；复位之前的成本不计入 |
| 周期切换（UTC 零点/月初） | 最多滞后一个刷新周期；期间保持保守（继续拦截） |
| 供应商被删除 | 下次刷新整体替换读数表 → 条目消失；`attachCost` 不再挂任何东西 |
| 数据库读失败 | fail-open：不阻断流量、保留最后成功读数、`last_error` 可在 `/stats` 读到 |
| 全部候选超限 | 503 `provider_cost_capped`（不是 502，也不是 429） |
| pinned（`model@provider` / `X-Gateway-Provider`） | 同受上限约束，返回同一错误 |
| 失败的尝试（4xx/5xx/流中断） | 其成本照常计入（成本表口径：失败也花钱） |
| 未计量/成本为 0 的尝试 | 计 0，不推进累计 |
| 多实例部署 | 各自 5 秒读数、各自判定；读的是同一张计量表，所以看到的数一致，超额窗口各自 ≤5s |
| 账本币种变更 | 上限与已用按**当前**账本币种解释，历史 `cost_micros` 不重算（已知限制） |
| 精度 | `cost_limit_micros` 为 int64 微单位；与账号授信上限同精度，够用 |

## 6. 测试策略

- `internal/domain`：3 周期 × （复位早于/晚于周期起点）的起算点表驱动；`ProviderCostExceeded`
  边界（上限 0、恰好相等、超过）。
- `internal/store`：迁移幂等 + 三列默认值；`ProviderCostsSince` 的分组与补 0、空入参不发查询；
  bootstrap merge 不重置上限。
- `internal/runtime`：只读有上限的供应商（无上限时一次查询都不发，用计数假实现钉住）；失败
  fail-open 且 `Status().LastError` 非空；`MarkReset` 立即归零；周期切换保守 + 下一 tick 修正；
  边沿日志只打一次；并发 `Refresh` 只跑一个。
- `internal/routing`：超限 → `cost_cap_reached`；同层还有未超限候选时选中它；pinned 超限 → 无候选；
  全部超限 → `ErrProviderCostCapped`；nil gate → 既有测试全绿（行为不变）。
- `internal/httpapi`（端到端，参照 `capacity_test.go`）：设上限/周期（非法值 400）、首次启用自动起算、
  `reset_cost` 后归零并立刻恢复选中、provider 载荷携带 `cost` 与账本币种、超限 503、
  MCP `admin_describe` / `admin_request` 能写能读，`/stats` 与指标出现。
- `make ui-check` 新视图 `cost`：列表列文案、`已超上限` 徽标、详情 kv、编辑表单三字段、
  「复位成本」按钮与确认文案。
- `make test` / `make vet` 全绿。

## 7. 依赖

- M53（请求日志的供应商维度：`usage_records.provider_id` 的计量口径、"成本与账单同源"）——本设计的读数与它同源。
- M44（供应商并发上限）：`capacity.go` 的端口/读回/守卫测试写法、`internal/arch` 的分层先例。
- M40 / `docs/mcp.md` §4.5（工具说明标准）与 `internal/httpapi` 的守卫测试（描述不为空、
  结构化字段必须有形状与示例、示例必须满足自己的 schema）。
- `docs/PROCESS.md` 的文档先行流程。

## 8. 实现与设计差异

实现与本文档一致，没有需要回退的决策。以下是实现时落定、值得记下来的细节：

1. **503 的触发条件按 D13 严格实现**：只有当一次 `Plan` 的**全部**剔除原因都是 `cost_cap_reached`
   时才是 503。因此"两个候选超限 + 一个候选已停用"仍是既有的 502 `upstream_error`
   （剔除原因里照样能看到 `cost_cap_reached`）。这是刻意的：宁可少报一次预算信号，也不要报一句
   "全部候选都超限"而其中一条其实是停用。`internal/routing/cost_test.go` 用两个用例分别钉住两侧。
2. **`Exceeded(p, now)` 的 `now` 参数没有被读取**（函数体里显式 `_ = now`）。原因写在注释里：
   窗口已经被折进读数（缓存里存的是"那个窗口的已用值"），而候选过滤器的签名统一带 `now`。
   周期切换到新窗口时，旧窗口的读数**偏大**，继续拦截是安全方向（D11）。
3. **周期值的规范化落在两处**：写路径用 `domain.NormalizeCostPeriod`（非法值 400），存储层用
   `costPeriodOrDefault` 把空串写成唯一的 `none`（避免同一状态两种拼写让控制台 select 选不中），
   读回时 `costPeriodJSON` 再把手工写库留下的空串显示成 `none`。`ProviderCostWindowStart` 对未知周期
   按"不限周期"处理并各有单测（这是最后一道防线，不是主校验）。
4. **首次启用自动起算只在更新路径生效**（`applyProviderCost` 里 `!creating`）：新建的供应商没有任何
   历史，窗口留 `NULL` 就是"从有记录以来"，语义相同且少一次写。
5. **控制台的"新建"表单也带了上限与周期两个字段**（原计划只写编辑表单）：既然新建接口接受它们，
   让操作者建完再进去改一遍没有意义。
6. **`attachCost` 只在有上限时挂 `cost` 块**，而配置三字段（`cost_limit_micros`/`cost_period`/
   `cost_window_start`）恒在 `providerJSON` 里：前者是"网关正在为它计数"的证据，后者是编辑表单要回填的配置。
   无上限的供应商报告 `0 / none`，而不是一个看起来像读数的 `cost: {used: 0}`。
7. **指标是三个 gauge**（`aigw_provider_cost_used_micros` / `_limit_micros` / `_exceeded`），
   `# HELP` 里带上生效的 `refresh_s`；`/stats` 的 `provider_cost` 块给 `currency`/`refresh_s`/`tracked`/
   `as_of`/`last_error` + 每供应商读数。读者自身的状态先于读数，和 `provider_capacity` 同一写法。
8. **没有任何新配置项**：`runtime.CostRefreshInterval = 5s` 是常量（D7）。`internal/config` 未改动。
9. **`internal/arch` 白名单未改动**：`runtime` 早已依赖 `registry`/`domain`，新文件不引入新边；
   `routing` 只吃 `CostGate` 接口，仍然不碰 `store`。
10. **测试落点**：`internal/domain/provider_cost_test.go`（起算点表驱动 + UTC 语义 + 超限边界）、
    `internal/store/provider_cost_test.go`（列往返、bootstrap merge 不清上限、按窗口聚合、空入参不发查询）、
    `internal/runtime/provider_cost_test.go`（无上限零查询、达到即拦、调高立即释放、失败 fail-open、
    复位精确归零、删除上限即丢读数、Start 先 seed、并发 Refresh 只跑一次、边沿日志只报一次且复位后重新计次）、
    `internal/routing/cost_test.go`（剔除原因、同层换候选、pinned 受限、全超限 503、混合原因不误报、nil gate 不变）、
    `internal/httpapi/provider_cost_test.go`（503 / 调高与复位立即释放 / 故障转移到第二家并核对计量行 /
    指标 / 管理面全生命周期含审计、"计量行未被改写"与 `router/explain` 的 `cost_cap_reached` /
    `admin_stats` / MCP 写入+读回+`admin_describe` 说明断言）。
11. **ui-harness 新增 `cost` 视图 15 项断言**，fixtures 快照未重新采集：与 `capacity` 视图同一手法，
    在页面 stub 里合成"已超上限"的状态（快照早于 M56）。
12. **实测数据**（本机，2026-09-17）：
    - `make test`、`make vet` 干净；`make ui-check` **22 个视图全绿**（含新增的 `cost`，15 项断言）。
    - 迁移在**真实旧库的副本**（`data/aigw.db`，M56 之前）上应用：21 个迁移、最后 21；既有供应商
      `local-chat` 读回 `cost_limit=0 / period="none" / window=<nil>`，即"不限"，行为与升级前一致；
      该库上 `ProviderCostsSince` 正常返回。
    - 读数查询的执行计划：`SEARCH usage_records USING INDEX idx_usage_provider_time (provider_id=? AND created_at>?)`
      —— 新索引被当作范围扫描使用（不是全表扫）。
