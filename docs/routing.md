# 路由与模型自由映射

> 状态：**已实现（M3）**。实现见 `internal/modelmap`、`internal/balancer`、`internal/routing`。

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

## 3. 权限层（API Key 与标签）

```
grantedModels    = ∪( key.grants.models,    各 tag.grants.models    )   ["*"] = 全部
grantedProviders = ∪( key.grants.providers, 各 tag.grants.providers )   ["*"] = 全部
```

- Key 没有标签且自身 grants 为空时，取 `auth.default_grant`（`all` 默认 / `none`）。
- **策略合并**：标签按 `priority` 升序 → Key 覆盖标签 → 模型 policy → 全局默认；
  **限额取各来源的最严值**（min）。
- 定价覆写优先级：key → tag → account → 模型（见 `docs/pricing.md`）。

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

### 4.3 熔断与冷却

- **熔断**：每 route 维度，60s 窗口内连续 5 次失败或失败率 >60%（样本 ≥10）→ open 30s，随后半开放行 1 次探测。
- **上游额度冷却**：上游返回 `quota_exhausted` 时按 `reset_at` 冷却该 route（缺省 1800s），并持久化到
  `routes.cooldown_until`，重启后不复活。
- 冷却为**惰性判断**（读取时比较时间），不依赖后台任务。

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
