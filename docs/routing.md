# 路由与模型自由映射

> 状态：**已实现（M3）**；会话粘性见 §4.4（M38）。实现见 `internal/modelmap`、`internal/balancer`、`internal/routing`。

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
