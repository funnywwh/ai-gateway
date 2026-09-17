# 路由与模型自由映射

> 状态：**已实现（M3）**；会话粘性见 §4.4（M38）、供应商并发上限见 §4.5（M44）、供应商成本上限见 §4.6（M56）。
> 实现见 `internal/modelmap`、`internal/balancer`、`internal/routing`。

## 1. 两段式解析

```
客户端 model 字符串 ──(映射层)──► canonical 模型 ──(路由层)──► (供应商, 上游模型名)
```

分离的好处：映射层只管"这是什么模型"（可自由配置、可通配）；路由层只管"用哪个供应商"（权重/优先级/熔断）。
管理界面可分别编辑，互不影响。

## 2. 映射层（model_mappings + aliases）

解析顺序（首个命中即生效）：

| 顺序 | 来源 | 说明 |
|---|---|---|
| 1 | `model@provider` / `X-Gateway-Provider` | 请求级显式钉死（未授权 → 403，不静默降级） |
| 2 | `models.public_name` | 精确匹配 canonical 模型 |
| 3 | `model_mappings` | 按 `priority` 升序；`kind ∈ exact、prefix、glob、regex` |
| 4 | `models.aliases_json` | 兼容旧式精确别名 |
| 5 | `routing.model_fallback` | 兜底模型名（未配置则 404 `model_not_found`） |

映射目标有两种：

- **指向 canonical 模型**（`target_model`）：继续走路由层；
- **直接钉死**（`target_provider_id` + `target_upstream_model`）：跳过路由层，直接指定供应商与上游模型名。

### 上游名占位符

`routes.upstream_model` 与 `provider_models.upstream_model` 支持：

| 占位符 | 含义 | 例子 |
|---|---|---|
| `{model}` | 客户端请求的原始模型名 | 客户端 `qwen2.5:32b` → 上游同名透传 |
| `{1}`, `{2}` … | 规则捕获组（prefix/glob/regex） | `gpt-*` 命中 `gpt-4o` → `{1}` = `4o` |
| `{name}` | 命名捕获组（regex 的 `(?P<name>…)`） | `(?P<name>o[0-9])` |

### 跨模型替换

允许 `A → B`（例如把旧模型名迁到新模型）。**受能力校验**：目标能力不满足请求特征时，
按 `routing.degradation` 处理（`strip` 剥离不支持字段并记录 `degraded_features`；`reject` 返回 400）。
界面对"替换模型"的规则给出风险标注。

## 3. 权限层（账号、API Key 与标签）

```
effectiveTags    = account.tags ∪ key.tags
grantedModels    = ∪( key.grants.models,    effectiveTags[].grants.models )   ["*"] = 全部
grantedProviders = ∪( key.grants.providers, effectiveTags[].grants.providers )   ["*"] = 全部
```

- `account.tags` 与 `key.tags` 都是可编辑的绑定；重复标签只计算一次，账号标签不能被 Key 排除。
- Key 没有生效标签且自身 grants 为空时，取 `auth.default_grant`（`all` 默认 / `none`）。
- **策略合并**：生效标签按 `priority` 升序 → Key 覆盖标签 → 模型 policy → 全局默认；
  **限额取各来源的最严值**（min）。
- 定价覆写优先级：key → 生效 tag（账号标签与 Key 标签并集）→ account → 模型。

## 4. 路由层（候选选择）

### 4.1 过滤（被剔除的候选都会在 Explain 中给出原因）

| 条件 | 剔除原因 |
|---|---|
| provider 停用 / route 停用 | `disabled` |
| provider 正在 draining | `draining` |
| 供应商不在授权集合 | `not_granted` |
| 处于上游额度冷却期 | `cooldown_until=<ts>` |
| 熔断打开 | `circuit_open` |
| 请求特征不被能力覆盖 | `missing_capability:<feature>` |
| 该模型在此供应商没有映射 | `not_mapped` |
| 达到该供应商的成本上限（§4.6） | `cost_cap_reached` |

### 4.2 分层与层内策略

1. 按 `route.priority` **升序**分组（小的先用）；模型 policy 的 `provider_order` 可覆盖顺序。
2. 层内按策略排序：

| 策略 | 语义 |
|---|---|
| `weighted_random`（默认） | 权重 = `route.weight × provider.weight` |
| `round_robin` | 层内轮询 |
| `least_inflight` | 当前在途请求最少者优先 |
| `least_latency` | **每 token 延迟**（`延迟 / max_output_tokens`）EWMA；样本 <10 退化为加权随机；对 `reasoning` 模型降权 |
| `strict_order` | 严格按权重降序，永不打散（确定性） |

3. 本层候选耗尽（全部可重试失败）后进入下一层；层间即"优先级降级"。

> 会话粘性（§4.4）只在本层内部重排，且对 `strict_order` 不生效——"永不打散"是这个策略的全部意义。

### 4.3 熔断与冷却

- **熔断**：每 route 维度，60s 窗口内连续 5 次失败或失败率 >60%（样本 ≥10）→ open 30s，随后半开放行 1 次探测。
- **上游额度冷却**：上游返回 `quota_exhausted` 时按 `reset_at` 冷却该 route（缺省 1800s），并持久化到
  `routes.cooldown_until`，重启后不复活。
- 冷却为**惰性判断**（读取时比较时间），不依赖后台任务。

### 4.4 会话粘性（M38）

同一 `API Key + session_id + canonical 模型` 的连续请求优先复用**上一次真正服务成功**的那个 route，
让一个会话的上游保持稳定（前缀缓存命中率、上游账号一致性），而不是每次都由加权随机重新掷骰子。

| 项 | 规则 |
|---|---|
| 键 | `api_key_id + session_id + canonical_model`；session_id 取请求的 `prompt_cache_key`（128 字节截断），无该字段的请求**完全不参与** |
| 生效范围 | 只在**同一 `route.priority` 层内**把命中的候选提到该层首位；跨层不提升（层间是运营者写下的优先级） |
| 策略交互 | 有效策略为 `strict_order` 时**完全不参与**（连槽都不产出，`Note*` 天然空转）；其余策略都参与 |
| 写粘性 | 只有 attempt **成功**才写入/刷新；失败的 attempt 不写 |
| 清粘性 | 仅当该候选**可重试失败**（`runtime.Retryable`）时清除，且只清"正好指向它"的记录 |
| 失效 | 命中候选若已被撤权、停用、draining、冷却、熔断、能力不足或不再映射 → 记录被删除，按原生策略路由；重新启用不会复活旧粘性 |
| 钉死 | `model@provider` / `X-Gateway-Provider` 的请求既不读也不写粘性 |
| 存储 | 进程内、TTL 默认 1800s（命中即刷新）、容量默认 10000 条（先清过期、再淘汰最旧）；**不持久化**，重启后重新负载均衡 |
| 开关 | `routing.session_affinity`（默认 `true`）、`routing.session_affinity_ttl_s`、`routing.session_affinity_max_entries` |
| 可观测 | `/admin/api/v1/stats` 的 `affinity` 块（启用状态、条数、命中/落空/失效/淘汰计数）；日志记 route/provider/session_id，不记原始键 |

**粘性不放宽授权**：它只能重排本次请求**已经**通过全部过滤的候选，因此永远不会把一个未授权、已撤权或不可用的供应商拉回来；
故障转移也只在同一个候选列表内进行（候选耗尽 → 既有 4xx/5xx 语义）。

### 4.5 供应商并发上限与排队（M44）

路由选出候选之后、真正出网之前，还有一道**供应商容量闸门**：`providers.max_inflight` 限制「同一供应商同时
在途的上游调用数」，超出的请求**排队等待**名额，而不是直接失败。

| 项 | 规则 |
|---|---|
| 上限 | `providers.max_inflight`（供应商实例级，其下所有模型共享）；`0` = 不限（默认）。控制台「最大并发」或管理 API `PATCH /admin/api/v1/providers/{id}`（MCP：`admin_request` → `admin_update_provider`）设置 |
| 计数口径 | **同时在途 attempt**：流式请求持有到流结束；一次尝试释放后名额才给下一个 |
| 排队 | 每个供应商内部 **FIFO**；名额释放时**直接交给队首**（不广播争抢），因此公平性与计数是同一个动作 |
| 等待上限 | `routing.provider_queue_wait_s`（默认 `30`，`0` = 不排队，超限立即失败） |
| 队列深度 | `routing.provider_queue_max_waiters`（默认 `100`，`0` = 深度不限，仍受等待时长约束） |
| 触界后果 | 等待超时 / 队列已满 / 不排队而超限 → 该次尝试判为**可重试失败**（`runtime.Retryable`）→ 走既有候选循环换下一个供应商；全部候选耗尽 → **429 `rate_limit_error` / `provider_busy`** + `Retry-After` |
| 上限变更 | 管理面写入后即时生效（下一次尝试读新值）；上限**调大**会立刻唤醒已在排队的请求；调小则在途请求跑完、新请求排队 |
| 排队与延迟 | 排队时长**不计入** `usage_records.latency_ms`/`ttft_ms`（那两个字段继续只表示上游耗时）；排队 ≥1s 记一条 Info 日志 |
| 不被限制的路径 | 供应商探测与后台动作（health/models/actions/logs/restart）**不占名额**：上游饱和时管理员仍要能操作 |
| 可观测 | `/metrics` 的 `aigw_provider_capacity_*`（limit/inflight/waiting 与 admitted/timeouts/rejected/wait_ms 计数）；`/admin/api/v1/stats` 的 `provider_capacity` 块（含生效的排队策略）；供应商列表/详情行内的 `capacity` |
| 边界 | 状态在**进程内**且不持久化：多实例部署各自计数（有效上限 = N × 实例数）；重启后排队与计数清零 |

**空闲与否不影响授权**：闸门在候选已通过全部过滤之后生效，只会让请求等待或按可重试失败降级，
不会把未授权/已撤权的供应商拉回来，也不会绕过熔断、冷却与能力校验。

### 4.6 供应商成本上限与复位（M56）

§4.5 管的是"同时几个请求"，这一节管的是"**这家上游总共让我花多少钱**"：
`providers.cost_limit_micros` 限制该供应商的**累计成本**（我们付给上游的钱，账本币种），
达到上限后它**从候选里被剔除**（原因 `cost_cap_reached`），请求按既有策略故障转移到其它候选。

| 项 | 规则 |
|---|---|
| 上限 | `providers.cost_limit_micros`（供应商实例级，其下所有模型共享），单位 = **账本币种微单位**；`0` = 不限（默认）。控制台「成本上限」或 `PATCH /admin/api/v1/providers/{id}`（MCP：`admin_request` → `admin_update_provider`）设置 |
| 计什么 | `usage_records.cost_micros` 按 `provider_id` 的累计值——与请求日志「成本」列、发票明细、供应商维度统计**同源**；**包含失败尝试的成本**（失败一样花钱） |
| 周期 | `providers.cost_period`：`none`（默认，累计自上次复位；从未复位则从有记录以来）/ `daily`（UTC 零点）/ `monthly`（UTC 月初自动重新起算） |
| 起算点 | `max(周期起点, 手动复位时刻)`；**首次启用上限**（从 0 改为正数且从未复位过）自动把起算点设为当前时刻，否则历史成本会瞬间把老供应商标成超限 |
| 复位 | 控制台行操作「复位成本」或 `PATCH` 的 `reset_cost:true`：只把起算点挪到当前时刻，**不修改、不删除任何计量行**；复位后该供应商立刻回到候选里 |
| 触界后果 | 该供应商被剔除（`cost_cap_reached`，见 §4.1）；**全部**候选都因成本上限被剔除时 → **503 `provider_cost_capped`**（不是 502：上游没坏；也不是 429：重试不会变好）。`model@provider` / `X-Gateway-Provider` 钉死的请求同样受约束 |
| 上限变更 | **调高立即生效**（判定用的是当前上限，不依赖读数）；**复位立即生效**（写路径把起算点挪到当前时刻并通知追踪器归零）；调低与周期切换最多滞后一个读数周期（切换期间保持保守：继续拦截） |
| 读数 | 进程内后台每 **5 秒**按计量表重读一次（热路径零查询）。最坏超额 = 这 5 秒内该供应商的流量成本；控制台读回的 `used_micros` 就是路由判定用的那个数 |
| 读失败 | **不阻断流量**（fail-open）：保留最后一次成功读数，错误在 `/stats` 的 `provider_cost.last_error` 可见。这条护栏是运营护栏，不是账务凭证 |
| 可观测 | `/metrics` 的 `aigw_provider_cost_used_micros` / `aigw_provider_cost_limit_micros` / `aigw_provider_cost_exceeded`；`/admin/api/v1/stats` 的 `provider_cost` 块（`refresh_s`/`as_of`/`last_error`/`tracked` + 每个有上限的供应商）；供应商列表/详情行内的 `cost`，Explain 的 `excluded` |
| 边界 | 多实例部署各自读数（读的是同一张计量表，所以数值一致，超额窗口各自 ≤5s）；账本币种变更后上限与已用按新币种解释，历史不重算 |

## 5. 请求级覆盖

| 方式 | 说明 |
|---|---|
| `model@provider_name` | 请求体里钉死供应商（未授权 → 403） |
| `X-Gateway-Provider` | 请求头钉死 |
| `X-Gateway-Strategy` | 覆盖本次请求的层内策略 |
| `X-Gateway-Trace` | 附带在响应与 hook 中便于排障 |

## 6. 诊断（模拟路由）

`POST /admin/api/v1/routes/explain` 入参：`key_id` 或 `tag`、`model`、`features`（是否带 tools/vision/reasoning/stream 等），
返回：

```json
{
  "requested": "gpt-4o",
  "canonical": "gpt-4o",
  "rule": "mapping:prefix:gpt-*",
  "order": [{"provider":"openai-main","upstream":"gpt-4o-2026-01-01","priority":10,"weight":100}],
  "excluded": [{"provider":"deepseek","reason":"not_granted"}]
}
```

**与真实路由共用同一纯函数**，因此"界面看到的顺序"就是"线上会用的顺序"。
