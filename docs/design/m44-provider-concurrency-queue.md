# M44 设计文档：供应商并发上限与排队等待

## 目标

让「供应商实例最大并发」从一个**存在但无人读取**的字段（`providers.max_inflight`）变成真正的运行时闸门，
并让超限的请求**排队等待**而不是立刻失败：

1. 同一供应商同时在途的上游调用数**恒 ≤ `max_inflight`**（0 = 不限，默认）；
2. 达到上限后的尝试**排队**，名额释放时按 **FIFO** 放行；
3. 排队有上界：等待时长（`routing.provider_queue_wait_s`）与队列深度（`routing.provider_queue_max_waiters`）；
   触界后该次尝试按**可重试失败**处理 → 走既有候选循环换下一个供应商；全部候选耗尽 → **429 `provider_busy`** + `Retry-After`；
4. 运营者可在**控制台**或**MCP**（`admin_update_provider` / `admin_create_provider`，scope=admin）设置并发数，并在
   `admin_get_provider` / `admin_list_providers` / `admin_stats` 读回实时「在途 / 排队」；
5. `max_inflight = 0` 时行为与 M44 之前**逐字节一致**；排队时长不污染 `usage_records.latency_ms`/`ttft_ms`。

## 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | 上限归属**供应商实例**（复用 `providers.max_inflight`），该供应商下所有模型共享 | 运营者心智是"这家上游最多同时几个请求"。route/model 级需要新列与新语义，本次不做（非目标） |
| D2 | 闸门在 `internal/runtime.Dispatcher.Complete/Stream`，且**在 `bal.Acquire` 之前** | 排队中的请求不得计入 `least_inflight` / 延迟 EWMA，否则排队会污染负载均衡与"上游延迟"的含义。这里也是唯一出网点（含控制台问答的回流请求） |
| D3 | 排队 = **FIFO + 精确交付**：`Release` 时把名额**直接交给队首**（`close(ready)` + `inflight++`），不广播争抢 | 广播唤醒会让后来者抢走名额且需要轮询。精确交付让"公平"与"计数准确"是同一个动作 |
| D4 | `provider_queue_wait_s = 0` 表示**不排队**（超限立即失败） | 给"宁可快速故障转移、不要等待"的部署一个开关；也让 `runtime.Config{}` 零值 = 不排队（测试与其它工具不会凭空引入等待） |
| D5 | 拒绝语义：等待超时 / 队列满 → `*runtime.CapacityError`（**可重试**）；客户端取消 → ctx 错误（**不**换候选） | 排队与既有故障转移共存：先等，等不到再降级。**否决 503**：容量就是速率语义，且 429 与既有 `retry-after` 退避习惯一致 |
| D6 | 计数口径 = **同时在途 attempt**（流式持有到流结束），名额在 `Dispatcher` 返回时释放 | "供应商并发"在上游看来就是同时在飞的调用数。与 `bal.Release` 同点，两个计量不会长期分叉 |
| D7 | 上限来源：本次尝试读到的供应商记录（快照优先、DB 兜底）；门保存"最近观察到的上限"，等待者每次被唤醒都按**最新值**重判；`reload` 后 `SyncLimits()` 推上限以**立即唤醒**已在排队的请求 | 管理员把上限调大时，已排队的请求不该继续傻等（否则要等一次 Release 或超时）。**否决**持久化/跨进程协调：状态是进程内的，多实例各自计数（当前部署单实例），文档写明 |
| D8 | 探测与后台动作（`Probe`/`Actions`/`RunAction`/`Logs`/`Restart`）**不占名额** | 供应商饱和时管理员仍必须能探测、看日志、重启；它们是运营者自己的路径，不是流量 |
| D9 | 被闸门拒绝的尝试**照常写一行 `usage_records`**（`status=failed`、`error_code=provider_busy`、`terminated_reason=provider_capacity`、`usage_source=unavailable`、**不计费**） | 与既有"插件启动失败"等未出网尝试完全一致；请求日志能看到"排队后在谁那里失败"。**否决**特例跳过写行：会与既有口径分叉，且丢掉诊断信息 |
| D10 | 排队时长**不进** `latency_ms`/`ttft_ms`：`Dispatcher` 用返回值 `Attempt{QueueWaitMS}` 交回，调用方减去并 clamp ≥ 0 | `latency_ms` 的含义是"上游多慢"，排队是网关侧等待。**否决**新增 `usage_records.queue_ms` 列：迁移 + 保留期 + 控制台列，收益不足（非目标）；排队时长走日志/指标/统计 |
| D11 | 门放在 **`internal/runtime` 包内**（新文件 `capacity.go`），不新建包、不改 `internal/arch` 白名单；`CapacityError`/`CapacityStat`/`CapacityPolicy` 导出给 httpapi 映射 | 沿用 M38 D10 先例；`httpapi` 已依赖 `runtime`（`Prober`、`Retryable`），不新增分层边 |
| D12 | 排队中**不**发 SSE 事件、**不**加响应头 | 流式的响应头在尝试之前就随 `response.created` 发出了（`assembler.Start()`），只给非流式加头会形成不对称契约 |
| D13 | **MCP 不新增工具**：并发数走既有 `admin_update_provider`/`admin_create_provider`（scope=admin），读回走 `admin_get_provider`/`admin_list_providers`/`admin_stats`（scope=admin_read 即可） | 管理面刻意是"渐进披露三入口"（`admin_endpoints`→`admin_describe`→`admin_request`），每个接口一个工具既贵又难发现；`max_inflight` 本来就在 `RawBody` 里，缺的只是完整说明与端到端证据 |
| D14 | MCP/管理面**只写并发数**，不写排队策略；但把**生效的排队策略**通过 `admin_stats.provider_capacity` 只读暴露 | 避免"YAML 与设置表两个真相"；agent 仍能回答"超出后最多等多久"，不必读配置文件 |
| D15 | 429 的 `code` 用新的 `provider_busy`，**不复用** `rate_limit_exceeded`、**不带** `x-ratelimit-*` | 客户侧配额（rpm/tpm/并发）与供应商容量是两回事：前者按 Key/标签、后者按供应商。`x-ratelimit-*` 是配额口径，混用会让客户以为自己的额度有问题 |

## 接口

```go
// internal/config/config.go —— Routing 段（默认 30 / 100）
type Routing struct {
    /* ... */
    ProviderQueueWaitS      int `yaml:"provider_queue_wait_s"`       // 0 = 不排队，超限立即失败
    ProviderQueueMaxWaiters int `yaml:"provider_queue_max_waiters"`  // 0 = 队列深度不限（仍受等待时长约束）
}
// 环境变量：GW_ROUTING_PROVIDER_QUEUE_WAIT_S / GW_ROUTING_PROVIDER_QUEUE_MAX_WAITERS

// internal/runtime/capacity.go
type CapacityStat struct {
    Limit     int   // 配置的上限；0 = 不限
    Inflight  int   // 正持有名额
    Waiting   int   // 排队中
    Admitted  int64 // 累计放行
    QueueFull int64 // 因队满被拒
    TimedOut  int64 // 等待超时
    Cancelled int64 // 等待期间上下文结束
    WaitTotalMS int64 // 累计等待毫秒（除以 Admitted 得平均等待）
}

type CapacityPolicy struct {
    QueueWaitS      int // 等待上限（秒），0 = 不排队
    QueueMaxWaiters int // 队列深度上限，0 = 不限
}

// ErrProviderBusy 是"供应商并发门拒绝"的哨兵；CapacityError 包住它并带现场。
var ErrProviderBusy = errors.New("provider at its concurrency limit")

type CapacityError struct {
    ProviderID int64
    Provider   string
    Reason     string // wait_timeout | queue_full | limit_reached
    Limit      int
    Waiters    int
    Waited     time.Duration
    WaitLimit  time.Duration
}
func (e *CapacityError) Error() string
func (e *CapacityError) Unwrap() error          // → ErrProviderBusy
func (e *CapacityError) RetryAfterSeconds() int // 提示值 = 配置的等待上限（≥1）

// 包内私有状态机（每供应商一个槽位状态：上限 / 在途 / FIFO 等待队列 / 计数器）
func newGate(cfg Config, log *slog.Logger) *gate
func (g *gate) acquire(ctx context.Context, id int64, limit int, provider string) (*permit, error)
func (g *gate) setLimit(id int64, limit int)
func (g *gate) stats() map[int64]CapacityStat

// permit 是一个名额。Release 幂等且 nil 安全（与 quota.Ticket 同风格）。
func (p *permit) Waited() time.Duration
func (p *permit) Release()

// internal/runtime/dispatcher.go
type Config struct {
    CredentialsKey  []byte
    CooldownDefault time.Duration
    QueueWait       time.Duration // <= 0：不排队
    QueueMaxWaiters int           // <= 0：深度不限
}

// Attempt 是"网关侧"的一次派发事实：上游响应本身不携带它。
// QueueWaitMS 是这次尝试在并发门里等待的毫秒数，调用方从自己的墙钟计时里减掉它，
// 使 usage_records.latency_ms/ttft_ms 继续只表示上游耗时。
type Attempt struct{ QueueWaitMS int }

func (d *Dispatcher) Complete(ctx context.Context, providerID int64, req *pluginapi.Request) (*pluginapi.Response, Attempt, error)
func (d *Dispatcher) Stream(ctx context.Context, providerID int64, req *pluginapi.Request, emit func(pluginapi.Event) error) (*pluginapi.StreamEnd, Attempt, error)
func (d *Dispatcher) CapacityStats() map[int64]CapacityStat
func (d *Dispatcher) CapacityPolicy() CapacityPolicy
func (d *Dispatcher) SyncLimits()          // reload 后把当前快照的上限推给门（唤醒等待者）
func Retryable(err error) bool             // + errors.Is(err, ErrProviderBusy) → true

// internal/domain/errors.go
func ErrProviderBusy(msg string) *APIError // 429 / rate_limit_error / provider_busy

// internal/httpapi
type Capacity interface {
    CapacityStats() map[int64]runtime.CapacityStat
    CapacityPolicy() runtime.CapacityPolicy
} // Deps.Capacity（nil = 不输出 provider_capacity 块）
func toAPIError(err error) *domain.APIError // errors.Is(err, runtime.ErrProviderBusy) → 429 provider_busy
```

## 数据流

一次请求的数据面（`POST /v1/responses`，流式与非流式同路径）：

```
准入（客户侧 quota）→ Router.Plan（候选有序）→ 计费预留
  └─ for i, cand := range plan.Candidates[:max_attempts]:
       1. provider := 快照优先 / DB 兜底的供应商记录      // 原来在 complete/stream 内部，现上移到门之前
       2. permit, err := gate.acquire(ctx, provider.ID, provider.MaxInflight, provider.Name)
            limit <= 0            → 直接放行（booked=false，不记名额）
            inflight < limit      → 占名额（booked=true）放行
            队满                   → CapacityError{queue_full}
            QueueWait <= 0        → CapacityError{limit_reached}
            否则                   → 入队（FIFO），select { <-ready | <-ctx.Done() | <-timer }
       3. defer permit.Release()                        // 归还名额或交给队首
       4. bal.Acquire(key) → 上游调用 → bal.Observe(key, latency, ok)   // 计时从放行后开始
  └─ latency = wall - attempt.QueueWaitMS（clamp ≥ 0）；ttft 同源
```

管理面/ MCP 的读路径（同一处理函数，HTTP 与 MCP 共用）：

```jsonc
// GET /admin/api/v1/stats  →  MCP admin_stats
"provider_capacity": {
  "queue_wait_s": 30,          // 生效的排队策略（只读，来自配置）
  "queue_max_waiters": 100,
  "providers": {               // 键是供应商数字 id（字符串）
    "3": {"limit":2,"inflight":1,"waiting":3,"admitted":12,
          "queue_full":0,"timed_out":1,"cancelled":0,"wait_total_ms":3450}
  }
}

// GET /admin/api/v1/providers 与 /providers/{id}（→ MCP admin_list_providers / admin_get_provider）
{"id":3, "name":"codex-main", "max_inflight":2,
 "capacity":{"limit":2,"inflight":1,"waiting":3,"admitted":12, /* … */}}
```

写路径（MCP）：`admin_request{name:"admin_update_provider", params:{id:3}, body:{max_inflight:2}}`
→ 真实 handler `PATCH /admin/api/v1/providers/3` → `applyProviderBody`（`>= 0` 校验）→ upsert → `reload`
→ `Dispatcher.SyncLimits()` 推上限并唤醒等待者。**不需要**新的 MCP 工具或新的 body 字段。

## 异常与边界

| 场景 | 行为 |
|---|---|
| `max_inflight = 0`（默认） | 完全放行；门不建状态（默认部署行为不变） |
| `max_inflight` 为负 | 写入侧 400（既有 `validateNonNegative`，MCP 同源） |
| 上限被调小到低于当前在途 | 新请求排队直到 `inflight < limit`；已在途的跑完 |
| 上限被调大 | `SyncLimits()`（reload 后）与任何新请求的 `acquire` 都唤醒等待者（`inflight < limit` 时逐位交付） |
| 上限变为 0（不限） | 一次性把所有等待者放行为"不占名额"，`Waiting` 归零 |
| 等待者被取消（客户端断开） | `select` 走 `ctx.Done()`；若名额已交付（`handed`）则**转交队首**，不丢名额；返回 ctx 错误，`Retryable` 为 false，**不**换候选 |
| 等待超时 | 同上但返回 `CapacityError{wait_timeout}`（可重试）。若已交付则转交，保证在途计数准确 |
| 队列满 | 立即 `queue_full`（可重试），不等待；`Waiting` 不变 |
| 队首已被取消但名额刚交付 | 由它自己 `Release` 转交下一位；交付链不会因取消而停摆 |
| `Release` 重复调用 / nil 接收者 | `sync.Once` + nil 判断（与 `quota.Ticket` 一致） |
| 等待期间供应商被删除/停用 | 放行后走既有 `pluginClient`/`builtin` 路径按原语义失败（未知供应商是本地错误，不再计入熔断样本） |
| 排队时长 vs 余额在途预留 | 预留 TTL 默认 660s，远大于最坏排队 `wait × max_attempts`；配置校验把这条写成硬约束 |
| 多实例部署 | 每实例独立计数（进程内状态），有效上限 = N × 实例数；文档写明（当前单实例部署） |
| 前端代理超时 | 排队期间流式连接已在 `response.created` 时建立，不会被判为空闲；运维提示：代理读超时 > `provider_queue_wait_s` |

## 测试策略

1. **门单元测试**（`internal/runtime/capacity_test.go`）：不限放行、恰好 N 个、FIFO 顺序、等待超时、队满、
   不排队立即失败、取消后名额转交、上限调大唤醒、Release 幂等、统计计数、200 并发 limit 8 峰值 ≤ 8（无超发）。
2. **派发层**（`internal/runtime/dispatcher_test.go`）：`testecho`（`chunks`/`delay_ms`）+ 静态快照，
   断言第二个并发尝试 `Attempt.QueueWaitMS > 0`、总耗时 ≈ 2×上游、排队期间 `bal.Inflight ≤ 1`、
   超时错误 `Retryable == true`、上限变更即时生效。
3. **HTTP 端到端**（`internal/httpapi/capacity_test.go`）：两个并发流式请求都 200 且串行（总耗时 ≥ 2×上游）、
   两条 `usage_records.latency_ms` ≈ 上游时长（证明排队被扣除）、`provider_queue_wait_s=0` 时第二个请求 429 `provider_busy` + `Retry-After`。
4. **MCP 设置面**（`internal/httpapi/mcp_admin_test.go`）：admin 令牌 `admin_request` 写 `max_inflight` → `admin_get_provider`
   读回一致 → `admin_stats` 读到 `provider_capacity.providers[id].limit` 与排队策略；`admin_describe` 的
   `max_inflight` 说明含默认值与排队语义；admin_read 可读不可写；query 令牌看不到后台工具。
5. **配置**（`internal/config/config_test.go`）：默认 30/100、负值被拒、`wait × max_attempts >= reservation_ttl_s` 被拒。
6. **守卫**：`go test ./internal/mcpsrv/ ./internal/httpapi/`（`docs/mcp.md` §4.5 说明标准、路由元数据、body 字段形状）。

## 依赖

- 复用：`internal/runtime`（派发、`Retryable`、`Config`）、`internal/registry`（上限来源）、`internal/config`、
  `internal/domain`（错误封装）、`internal/httpapi`（管理面与 MCP 桥）、`internal/quota`（**只作风格参照**，不复用其状态）。
- 不新增第三方依赖；不新增包；不改 `internal/arch` 分层白名单（`capacity.go` 落在 `internal/runtime` 内，仅用标准库）。
- MCP 侧复用既有 `admin_routes.go` 路由表与 `mcp_admin.go` 桥，不新增工具。

## 实现与设计差异

实现过程中偏离本文档的地方，以及原因：

1. **日志在 `httpapi` 而不是 `runtime`**：本文档的"异常与边界"只写了"排队 ≥1s 记一条 Info 日志"，
   没写在哪一层。最终放在 `v1.go`（`logCapacityWait`），因为只有请求路径知道 `request_id`——
   `runtime` 拿不到它（`pluginapi.Request.Metadata` 是客户端元数据，不是网关的请求 id），
   而"哪次请求在排队"正是运维要关联起来的东西。拒绝（`CapacityError`）记 Warn，等待 ≥1s 记 Info。
2. **被拒绝的尝试 `Attempt` 是零值**：一次没出网的派发没有"上游调用"可以描述，它的排队时长挂在
   `*CapacityError.Waited` 上（`RetryAfterSeconds()` 同源）。本文档只写了 `Attempt{QueueWaitMS}`，
   没有交代拒绝路径；实现里两者分工是：跑过的尝试看 `Attempt`，被拒的看错误本身。
3. **`WaitTotalMS` 的口径比文档写的大**：文档的接口注释写成"除以 `Admitted` 得平均等待"，
   实现里它累计**所有**排队尝试的等待（含超时与取消）。理由：一个供应商造成的排队总量才是容量规划要看的，
   只统计成功放行会把"排队排到放弃"的那部分藏起来；代价是不存在一个能直接相除得到平均值的分母。
4. **上限被取消后闸门条目保留**：`max_inflight` 从正数写回 0 时，`setLimit` 把上限设为 0 并释放所有等待者，
   但**不删除**条目——仍在途的尝试还要计数，否则把上限再调起来时会短暂超额。因此
   `attachCapacity` 只在"从未设过上限"时不输出字段，曾经设过的行会带着 `limit: 0` 出现（如实反映：不限，但仍有人在途）。
5. **里程碑编号从 M43 改为 M44**：开工期间另一条线的提交占用了 M43（`docs/design/m43-api-key-hash-import.md`
   "API Key 哈希导入 + sub2api 用户迁移"）。设计文档与所有引用随之改为 M44，内容不变。
6. **测试夹具与浏览器走查的配套改动**（超出本文档"测试策略"的范围，但属于同一件事的可验证性）：
   `newFixture` 增加 `fixtureOption`（`withProviderCeiling` / `withCapacityQueue`），
   `adminFixture` 按生产默认接上 `Capacity` 端口与 `dispatcher.SyncLimits()`（reload 闭包内）；
   UI harness 新增 `#capacity` 视图，并在其 fetch stub 里**就地合成** `capacity` 块——
   快照来自一个没设上限的网关，而 `capacity` 只对设了上限的供应商出现。
7. **`config.example.yaml` 的排队键默认值与 `config.Default()` 一致（30 / 100）**，文档未特别说明；
   零值语义仍是"不排队"，所以直接把 `runtime.Config{}` 交给 `New()` 的调用方（测试、工具）
   不会凭空引入等待。
