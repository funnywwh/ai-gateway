# M4 设计文档：API Key 鉴权、分片限速与计量

## 目标
把"请求带着 Bearer key 进来"这段做成**热路径零 DB**的鉴权 + 分片限速 + attempt 粒度计量：
鉴权结果带 TTL 缓存、限速用分片滑动窗口、计量按"每次真实出网 attempt"落库。

## 关键决策

1. **前缀索引 + 常量时间比较**：`token → sha256`，用前 12 字符（`key_prefix`，唯一索引）取行，
   再 `subtle.ConstantTimeCompare` 比对哈希。DB 只存哈希与前缀，明文永不落盘。
2. **正/负缓存**：命中后缓存 `(key, account)` TTL 30s；**未命中（401）也做短 TTL 负缓存**（5s），
   避免无效 key 打爆数据库。缓存按前缀键控，容量上限（默认 10000）超出时先清理过期项。
3. **失效**：管理面任何 key/account/tag 写操作后调用 `Invalidate(prefix)` 或 `InvalidateAll()`，
   与注册表快照换入一起生效，无需重启。
4. **last_used_at 节流**：只有距上次更新 >60s 才写，且**异步**执行，不阻塞请求。
5. **账户状态**：账户 `suspended` → **402 `billing_hard_limit_reached`**（不是 401），
   因为凭据有效但账户被停用。
6. **分片限速**：默认 64 个分片（`ratelimit.shards`），按 `hash(scope) % shards` 定位；
   每分片内按 scope 维护 **60 个每秒槽位**的滑动窗口（近似精确的滑窗），
   以及并发热计数。锁粒度=分片，避免全局锁。
7. **三个维度分别判定**：`rpm`（请求数）、`tpm`（token，**完成后结算**）、`concurrency`（进行中）。
   任一超限即拒绝，错误里带 **哪个维度、上限、剩余、重置时刻**，用于生成 `x-ratelimit-*` 与 `Retry-After`。
8. **票据模型**：`Reserve` 返回 `Ticket`；请求结束必须 `Release()`（释放并发），
   流式结束再 `Settle(tokens)`（回填 tpm）。保证失败/中断路径也释放（defer）。
9. **计量的最小单位是 attempt**：每次真实出网调用写一行 `usage_records`（含失败 attempt），
   **本地拒绝（未出网）不写用量**，只写 `request_logs` + 审计 + hook。
10. **用量累加器**：流式场景把 `usage.delta` 累加、以最终 `usage` 事件为准；
    只有增量时标 `usage_source=estimated`；两者都没有时用字符估算兜底。

## 接口（M4 产出）
- `internal/apikey.Verifier`：`Verify(ctx, token) (*domain.APIKey, *domain.Account, error)`、`Invalidate/InvalidateAll`。
- `internal/quota.Limiter`：`Reserve(ctx, scope, Limits) (*Ticket, error)`；`Ticket.Release()/Settle(tokens)`；
  `Limits` 由 key/tag/账户三处**取最严**合并而来。
- `internal/quota.ExceededError`：携带 `Scope/Limit/Value/Remaining/ResetAt`，可转成 `domain.APIError` 与响应头。
- `internal/usage.Meter`：`Record(ctx, *Attempt) error`；`usage.Accumulator` 合并流式用量事件。

## 数据流
```
Authorization: Bearer sk-gw-…
   └─ apikey.Verify ──(缓存命中/DB+常量时间比对)──► (key, account)
        └─ routing.Plan（M3）──► 候选
             └─ quota.Reserve(scope=key:<id>, limits=最严合并)
                  ├─ 通过 → 调用供应商（attempt 开始）→ usage.Meter.Record → Ticket.Settle(tokens) → Release
                  └─ 超限 → 429/402（未出网，不写 usage_records）
```

## 异常与边界
- 空/畸形 Authorization → 401 `invalid_api_key`。
- key 过期或 `status != active` → 401。
- 账户 `suspended` → 402。
- 限速超限 → 429，带 `Retry-After` 与 `x-ratelimit-{limit,remaining,reset}-{requests,tokens}`。
- 缓存容量打满 → 清理过期项后按最早过期淘汰；不阻塞请求。
- 时钟：限速窗口用可注入的 `now()`，测试可精确控制。

## 测试策略
- 鉴权：正确 key、错误哈希、未知前缀、过期、停用、账户 suspended、负缓存生效、`Invalidate` 后立即失效、`last_used_at` 节流。
- 限速：rpm 边界（第 N 与第 N+1 次）、tpm 结算后生效、并发上限与释放、窗口滑动（时间推进后恢复）、多 scope 互不影响、分片一致性。
- 计量：attempt 落库字段完整、dimensions JSON、估算来源标记、overshoot 记录、失败 attempt 也落库。
- 累加器：`usage.delta` 累加、最终 `usage` 覆盖、只有增量标 estimated、都没有则估算。

## 依赖
标准库 + `internal/domain`、`internal/secret`、`pkg/pluginapi`（事件类型）。

## 实现与设计差异
- **缓存未命中也会 touch**：命中缓存与从 DB 加载两条路径都会按同一节流规则更新 `last_used_at`，
  并把 `touchedAt` 初始化为数据库里的 `last_used_at`，避免"每个 TTL 周期都写一次"。
- **限速窗口实现**：用 60 个每秒槽位的环形数组近似滑动窗口（`expire` 惰性清零过期槽位），
  而非精确的逐秒滑窗；`ResetAt` 取"最旧非空槽位 + 窗口长度"，比固定窗口更贴近真实恢复时刻。
- **票据幂等释放**：`Ticket.Release` 用 `sync.Once` 保证重复调用安全（defer 与显式调用并存时不双减）。
- **策略解析不引额外依赖**：`LimitsFromPolicy` 只识别 rpm/tpm/concurrency/monthly_*，
  非 JSON 或空值一律返回"无限制"，避免配置写错导致全量拒绝。
- **计量不写本地拒绝**：`Meter.Record` 只由真实出网路径调用；本地拒绝由 M7 的 request_logs 承担。
- **`Record` 返回落库后的记录**（含 id 与时间戳），便于 M5/M11 在同事务结算里复用。
