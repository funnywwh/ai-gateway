# M3 设计文档：模型自由映射、权限并集与路由选择

## 目标
把「请求里的 model 字符串」变成「一个具体供应商 + 上游模型名」的可解释决策：
自由映射（含通配/透传/钉死）→ 权限并集校验 → 候选过滤 → 优先级分层 → 层内负载均衡 → 熔断/冷却剔除，
并提供一个`Explain` 纯函数供管理界面"模拟路由"复用。

## 关键决策

1. **两段式解析**：`requested → canonical model`（映射层）与 `canonical → (provider, upstream_model)`（路由层）分离。
   映射层只回答"这是哪个模型"，路由层只回答"用哪个供应商"。二者可独立演进。
2. **解析顺序**（首个命中）：`@provider`/请求头显式钉死 → `models.public_name` 精确 →
   `model_mappings`（按 priority，kind ∈ exact|prefix|glob|regex）→ `aliases_json` → `routing.model_fallback` → 404。
3. **映射目标两类**：① 指向 canonical 模型（继续走 routes）；② **直接钉死** `provider_id + upstream_model`（跳过路由）。
4. **上游名占位符**：`upstream_model` 支持 `{model}`（透传客户端原始名，适合自建 vLLM/Ollama）
   与捕获组 `{1}`/`{name}`（prefix/glob/regex 的匹配片段）。
5. **跨模型替换受能力校验**：A→B 允许，但目标能力必须覆盖请求特征，否则按 `degradation` 剥离或 400。
6. **权限是并集**：`granted(models|providers) = ∪(key.grants, 各 tag.grants)`；`["*"]` 表示全部；
   key 无 tag 且 grants 为空时取 `auth.default_grant`（all|none）。
7. **策略合并**：tag 按 priority 升序 → key 覆盖 tag → 模型 policy → 全局默认；**限额取最严**。
8. **候选过滤条件**（任一不满足即剔除，并被 Explain 记录原因）：
   provider.enabled && !draining && route.enabled && 在授权集合内 && 不在冷却期内 && 特征 ⊆ 能力。
9. **分层与层内策略**：先按 `route.priority` 升序分组（模型 policy 的 `provider_order` 可覆盖顺序）；
   层内按策略排序：`weighted_random`(默认) / `round_robin` / `least_inflight` / `least_latency` / `strict_order`。
10. **least_latency 用每 token 延迟归一化**（`latency / max_output_tokens`），并对 `capabilities.reasoning`
    的模型降权，避免长任务模型被饿死；样本 <10 退化为加权随机。
11. **熔断**：每 route 维度，60s 窗口内连续 5 次失败或失败率 >60%（样本 ≥10）→ open 30s，随后半开放行 1 次探测。
12. **Explain 是纯函数**：输入 (model, key/tag, features)，输出解析链路 + 有序候选 + 被剔除候选及原因，
    与真实路由**共用同一份代码**，保证界面解释与线上行为一致。

## 接口（M3 产出）
- `internal/modelmap.Resolver`：`Resolve(requested) (domain.ResolvedModel, error)`；纯函数，输入注册表快照。
- `internal/balancer`：`Strategy` 接口 + 注册表；`State`（inflight/EWMA/轮询游标/熔断窗口），并发安全。
- `internal/routing.Router`：`Candidates(ctx, req) ([]Candidate, error)`、`Explain(ctx, req) (*RouteExplanation, error)`、
  `Order(tier) []Candidate`。
- 复用 `domain` 中的 `Grant`/`Policy`/`ResolvedModel`/`Candidate`/`RouteExplanation`/`Exclusion`。

## 数据流
```
model 字符串
  └─ modelmap.Resolve ──► ResolvedModel{Canonical, Pinned?, ProviderID, Upstream}
        └─ routing.Candidates
              ├─ 权限并集过滤（未授权 → Exclusion{reason:"not_granted"}）
              ├─ 冷却/熔断/停用过滤
              ├─ 能力过滤（缺失 → Exclusion{reason:"missing_capability:tools"}）
              └─ 分层 + 层内排序 ──► []Candidate（有序）
                    └─ 调用方逐个 attempt，失败按 retryable 继续下一个
```

## 异常与边界
- 未命中任何映射：`404 model_not_found`（若配置了 fallback 则走 fallback）。
- 正则非法：写入时校验拒绝；运行时再校验一次并跳过该规则（记录告警）。
- 显式钉死未授权：`403 permission_denied`（不静默降级到其他供应商）。
- 全部候选被剔除：返回 `403`（未授权）或 `400 unsupported_parameter`（能力不满足），
  错误信息包含 Explain 的首个剔除原因。
- 冷却到期：惰性判断（读取时比较 `cooldown_until`），不依赖后台任务。

## 测试策略
- 映射：exact/prefix/glob/regex 命中顺序与 priority 抢占、`{model}` 与捕获组代入、fallback、钉死、非法正则。
- 权限：并集语义、`*` 通配、default_grant=all/none、tag 优先级与 key 覆盖。
- 候选：能力过滤、draining/停用、冷却、未授权，逐条断言 Explain 原因。
- 策略：加权随机的分布（卡方/边界）、轮询均衡、least_inflight、least_latency 归一化无长任务偏置、strict_order 确定性。
- 熔断：连续失败开断、半开探测、冷却期跳过。
- 并发：策略状态在 `-race`（本环境不可用则以单测覆盖原子性）与 200 并发下无数据竞争、无超发。

## 依赖
仅标准库 + 现有 `internal/domain`、`internal/registry`。

## 实现与设计差异
- **快照排序契约**：`registry.NewSnapshot` 会对 `Mappings` 按 (priority, id) 稳定排序，
  因此解析器可以依赖顺序而不必自己排序。`NewStatic` 用于测试与管理面预览。
- **能力未知视为放行**：`provider_models.capabilities_json` 为空时视为"未声明"，
  不做能力过滤（否则未填能力的存量供应商会被全部拦掉）。`capabilities_override` 优先。
- **`degradation=strip` 的语义**：候选保留，但把缺失特征记入 `Candidate.Degraded`（供响应头
  `x-gateway-degraded` 与 hook 使用）；`reject` 则剔除该候选。
- **错误优先级调整**：当同时存在"未授权"和"能力不足"的剔除时，返回 **400 unsupported** 而非 403 ——
  因为能力剔除必然来自已授权供应商，403 会误导调用方。仅当全部剔除都是未授权时才返回 403。
- **`least_latency` 的未测量目标**：取该层已测量目标的中位/均值参与比较，而不是直接排到末尾，
  避免新供应商永久拿不到流量（冷启动问题）。
- **确定性**：`strict_order` 按 (priority, weight) 排序；同层相等时保持注册表顺序（`SliceStable`），
  便于复现问题。
- **权重归一化**：`weight <= 0` 视为 100（与配置默认一致），避免误配导致候选饿死。
