# M38 设计文档：会话粘性路由 + 授权范围内故障转移

## 目标

把"同一个客户端会话的连续请求"与"上一次真正服务成功的那个 route"绑定起来：

1. 同一 `API Key + session_id + canonical model` 的请求，在粘性有效期内优先复用上一次**成功**用过的 route；
2. 粘性只影响**排序**，绝不扩大授权——命中候选必须仍然是本次请求经授权/能力/冷却/熔断过滤后的候选；
3. 首选上游可重试失败时，只在**本次请求已授权的候选集合**内转移；成功的备用路由成为该会话新的粘性目标；
4. 无 `prompt_cache_key` 的客户端行为逐字节不变；显式钉死供应商（`model@provider` / `X-Gateway-Provider`）完全不参与粘性。

## 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | 粘性键 = `api_key_id \x00 session_id \x00 canonical_model` | canonical 而非 requested：别名/映射指向同一模型时必须共享粘性（`docs/routing.md` §2 的两段式解析）。跨 Key 共享会越权，跨模型共享会把"这个模型在这家可用"错误外推 |
| D2 | 进程内状态、带 TTL 与容量上限、**不持久化** | 重启后重新负载均衡；无迁移、无清理任务、无并发写。备选（SQLite 持久化）因需要迁移 + 保留期 + 并发写处理而被否 |
| D3 | 是否参与粘性由 `Plan` 决定，并以**不可构造的不透明标记** `Result.Affinity string` 交给调用方 | `""` 表示不参与（功能关闭 / 无 session / pinned）。调用方无法凭空构造键，从类型上杜绝"忘了判断 pinned"这类调用点错误 |
| D4 | 只在**同一 priority 层内**把命中候选提升到该层首位 | `route.priority` 是运营者"先用谁"的显式契约（`docs/routing.md` §4.2「层间即优先级降级」）。**否决**跨层提升：一次失败就会永久反转运营者的优先级意图；坏路由由既有熔断/冷却剔除，而不是由粘性绕开 |
| D5 | 有效策略为 `strict_order` 时**不做**粘性重排 | `strict_order` 的定义就是「严格按权重降序，永不打散（确定性）」；重排会破坏该契约。其余四个策略都参与 |
| D6 | 只有**可重试失败**才清除粘性 | 4xx 类客户端错误说明不了路由坏了；当成"路由坏了"会让会话无故漂移。判定完全复用 `runtime.Retryable()` |
| D7 | 粘性命中**不**写响应头、不进请求日志、不发 hook | 流式分支本来就不设响应头（M32 记录在案），只给非流式加头会形成不对称契约；日志列/表结构改动需要迁移与保留期联动。可观测性走 `/stats` 聚合 + 结构化日志 |
| D8 | 配置默认**开启**，但只对携带 `prompt_cache_key` 的客户端生效 | 无 session 的客户端行为完全不变；携带 session 的客户端正是会因加权随机而在上游之间来回跳的那批。`session_affinity: false` 一键回到今天的行为 |
| D9 | 粘性用 `req.PromptCacheKey` 的原始值（仅 128 字节截断），**不**受 `recording.redact_paths` 影响 | 脱敏管的是"写进请求日志的东西"；粘性键只存在于进程内存、有 TTL 与容量上限、不出进程。跟随脱敏会让开关一开就静默失去粘性，更难排查 |
| D10 | 复用 `internal/routing`，不新建包、不改 `internal/arch` 分层表 | `routing` 已是"纯函数 + 快照 + balancer 状态"的家；affinity 只依赖 `sync`/`time`/`strconv` |

## 接口

```go
// internal/responses/dimensions.go
// SessionKey 是 dimensions 记录的那个会话键；Dimensions() 复用它，保证截断只有一处定义。
func (r *Request) SessionKey() string

// internal/domain/interfaces.go
type RouteRequest struct { /* ... */ SessionID string }   // 客户端 prompt_cache_key，"" 表示不参与

// internal/routing/routing.go
type Config struct {
    /* ... */
    SessionAffinity    bool
    AffinityTTL        time.Duration // 0 → 内置默认 30m（仅 SessionAffinity 为真时填充）
    AffinityMaxEntries int           // 0 → 内置默认 10000
}
type Result struct { /* ... */ Affinity string } // 不透明；"" = 本请求不参与粘性

func (r *Router) NoteSuccess(affinity string, routeID int64)
func (r *Router) NoteFailure(affinity string, routeID int64) // 仅当记录正指向 routeID 时删除
func (r *Router) AffinityStats() AffinityStats               // Enabled/Entries/Hits/Misses/Stale/Evictions

// internal/routing/affinity.go（新）
type affinityStore struct { /* mutex + map + ttl + max */ }
```

`domain.Router` 接口保持不变：httpapi 用的是具体类型 `*routing.Router`，粘性不属于抽象路由器契约。

## 数据流

```
POST /v1/responses
 └─ responses.Parse
 └─ req.SessionKey()                     ← O(1)：TrimSpace + 128B 截断
      └─ Router.Plan(RouteRequest{..., SessionID})
           ├─ 解析 requested → canonical
           ├─ 授权 / 熔断 / 冷却 / 能力 / 映射 过滤（与 M3 完全一致）
           ├─ 层内按策略排序
           ├─ 粘性查找：命中且该 route 仍在**本层**候选里 → 提到本层首位
           │            命中但候选里没有 → 删除该记录（stale）
           └─ Result.Affinity（"" 或不可构造的键）
      └─ attempt 循环（候选 = 本次 Plan 的列表，授权边界天然成立）
           ├─ 成功          → NoteSuccess(res.Affinity, cand.RouteID)
           ├─ 可重试失败     → NoteFailure(res.Affinity, cand.RouteID)
           └─ 其余错误/已有 delta/取消 → 不动粘性，按既有逻辑退出
```

## 异常与边界

- **无 `prompt_cache_key`**：`SessionKey() == ""` → `Affinity == ""` → 整套逻辑短路。
- **pinned 请求**：`Plan` 的 pinned 分支不产出键，`Note*` 天然空转。
- **同会话并发多请求**：各自可能写入，最后写入者胜；两条都已被验证可用且都在授权内，无正确性问题。
- **stale 目标**：撤权/停用/draining/冷却/熔断/能力不足/`not_mapped` → 命中落空 → 删除记录 → 原生策略接管；重新启用不会复活（下一次请求重新写入）。
- **跨层命中**：不提升、也不删除（该层下次仍可用）；但若该路由已被完全剔除出候选，则走 stale 删除。
- **容量压力**：客户端伪造大量 session id → `put` 先清过期项、再淘汰最旧项；内存上界 = `max_entries`。
- **TTL 语义**：命中即刷新（滑动 TTL）；只有成功才刷新，失败由 D6 决定是否删除。
- **热重载**：粘性开关是启动期配置（与 routing 其他配置一致），改动需重启。
- **日志**：只记 `route_id`/`provider`/`session_id`（**不记** affinity 原始键）、hit/miss/stale 与转移前后 provider；失败转移用 Info，命中用 Debug。

## 测试策略

- **单元（`internal/routing/affinity_test.go`）**：TTL 到期（注入 `now`，不 sleep）、命中刷新、容量淘汰最旧、键三轴隔离、`nil` store 空转、并发 put/get（`-race` 真跑）、统计计数。
- **路由（`internal/routing/affinity_test.go` 同文件）**：同层提升（判定用"整个候选序列"，不只是队首）、**跨层不提升**、`strict_order` 不参与、stale 删除且不复活、`NoteFailure` 只清"就是它"的记录、pinned 不产出槽、无 session 时顺序等于策略输出、配置零值不开启。
  判定顺序需要一台**确定性**策略做底座：用例用 `least_inflight`（并列时按权重、再按 registry 顺序），否则加权随机下"有没有提升"不可判定。
  同理，夹具必须包含**同层两家 + 下一层一家**：只给每家一个层级，跨层不提升的用例会连变异体都测不出来（层内提升对"本层唯一候选"是空操作）。
- **端到端（`internal/httpapi/affinity_test.go`，独立文件避免污染既有 `v1_test.go` 夹具）**：三个 `testecho` 供应商用不同 `prefix` 暴露"谁作答"；
  同层两家等价 → 同会话连发 6 次同一家；flaky(priority 5) 失败 → 200 且来自已授权的下一候选；只授权 flaky 的 key 复用别人的 `prompt_cache_key` → 既不借到好供应商、也不继承别人的粘性；粘性目标 `enabled=false` + `SetProviderFlags`/`Reload` → 同会话改由另一家作答并**重新绑定**；无 `prompt_cache_key` → `affinity` 计数全 0。
  注意：`testecho.Complete` 才会执行 `fail_mode`，`Stream` 只在 `FailAfter` 时失败，所以端到端一律用非流式 body；新增 API key 的 token 必须在 `secret.PrefixLen`（12）字符内就与既有 token 不同，否则 `ON CONFLICT(key_prefix)` 会改写夹具自己的 key。
- **配置（`internal/config/config_test.go`）**：默认值、YAML 覆盖、`GW_ROUTING_*` 覆盖、开启时非法值报错 / 关闭时不报错。
- **维度（`internal/responses/dimensions_test.go`）**：`SessionKey()` 与 `Dimensions().SessionID` 相等（含截断与首尾空白）。
- **统计（`internal/httpapi/admin_test.go`）**：`/stats` 的 `affinity` 块形状。
- **变异验证**（逐条临时改代码，确认**恰好**对应用例变红，然后还原）：① `Plan` 不应用粘性 → 路由用例 + 端到端用例同时红；② 提升时越过 tier 边界 → 跨层用例红；③ 去掉 stale 删除 → 不复活用例红；④ `NoteFailure` 不清粘性 → 对应用例红；⑤ pinned 分支也产出槽 → pinned 用例红；⑥ `v1.go` 成功回调不写粘性 → 端到端用例红。

## 依赖

仅标准库（`sync`、`time`、`strconv`）+ 既有 `internal/{domain,registry,balancer,modelmap}`。不新增第三方依赖，不改分层表。

## 实现与设计差异

实现与本文档的设计一致，只有下面几处落地时收紧或收窄的判断，逐条记录：

1. **`strict_order` 的处理从"不重排"收紧为"完全不参与"**。设计里写的是"有效策略为 `strict_order` 时不重排"，实现把这一类请求排除在整套机制之外：`Plan` 连 `Result.Affinity` 都不产出，`NoteSuccess`/`NoteFailure` 于是天然空转。
   理由：如果只"不重排"而仍然产出槽，调用方拿到的就是一个永远用不上的键——它会占容量、会让 `/stats` 的 `entries` 说谎，而且一旦以后有人给 `strict_order` 加别的排序逻辑，这个键会突然生效。不参与比不生效更容易验证。
2. **粘性槽也在 `Affinity` 上携带"是否参与"的信息**，没有另设 `Sticky bool`。`Result.Affinity == ""` 即"本次请求不参与"，调用方（`internal/httpapi/v1.go`）不需要再判断 pinned/strict_order/无 session 三种情况，避免了"调用点忘记判断"这类错误。
3. **无 `api_key_id`（`KeyID <= 0`）时不产出槽**。设计只说了粘性键包含 key id，没说 key id 缺失怎么办；实现选择不参与，而不是退化成"按 session 全局共享"——否则两个不同客户端的同一 `prompt_cache_key` 会互相带偏。
4. **`SessionKey()` 复用 `Dimensions()` 的同一个常量**（`maxSessionIDBytes = 128`），而不是各写一份截断。二者必须相等，否则"日志里看到的 session"和"路由粘住的 session"会是两个东西（`dimensions_test.go` 已钉死）。
5. **stale 只用一个信号判定：命中路由是否还在本次候选列表里**。设计把 stale 列成一串原因（撤权/停用/draining/冷却/熔断/能力不足/`not_mapped`），实现不去逐个分辨——这些原因的作用正是让候选列表不含该路由，于是 `promoteWithinTier` 返回 `false`，删除该记录并记一次 `stale`。"跨层命中"不在此列：它仍在候选里，只是层更靠后，因此既不提升也不删除（与设计一致，也有用例钉住）。
6. **`/stats` 的 `affinity` 块只在进程内聚合**，没有计数器的持久化或重置端点（`make verify` 之外无新端点）。重启即归零，与"粘性不持久化"一致。
7. **`record_input_mode`/脱敏与粘性键无关**：粘性键在内存里由 `key id + session + canonical model` 现场拼出，不经过 `recording.redact_paths`，也不会因为日志脱敏而变化。
