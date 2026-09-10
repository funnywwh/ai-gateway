# M8 设计文档：管理 REST API、会话鉴权与热更新

## 目标
给运维一个受保护的管理面：会话登录、供应商/模型/映射/Key/标签/MCP 令牌/hooks 的 CRUD、
用量与请求日志查询、统计与审计；**任何写操作后立刻生效**（缓存失效 + 注册表原子换新快照）。

## 关键决策
1. **两个面严格分离**：公开面 `/v1/*`（API Key 鉴权）与管理面 `/admin/api/v1/*`（会话或管理员令牌）；
   中间件与 Cookie 名互不共用。
2. **口令哈希用标准库 PBKDF2-HMAC-SHA256**（Go 1.24+ `crypto/pbkdf2`，21 万次迭代，16 字节盐）：
   计划原写 argon2id（`x/crypto`），改用标准库可**少一个依赖**且同属合规 KDF；格式
   `pbkdf2-sha256$<iter>$<salt-b64>$<hash-b64>` 便于将来平滑升级。
3. **会话**：登录成功签发 `sess_<随机>`，**库里只存 SHA-256 哈希** + 过期时间（默认 12h）；
   Cookie `aigw_admin`：HttpOnly、SameSite=Lax、Path=/admin；退出即删除该会话行。
4. **登录限速**：按 (IP, 用户名) 做失败计数窗口，超限返回 429（内存实现，复用分片思路）。
5. **热更新三件套**：写操作成功后 → ① 失效相关鉴权缓存（Key 变更 → `Invalidate(prefix)`，
   账户/标签变更 → `InvalidateAll`）；② `registry.Reload` 原子换新快照；③ 审计 + `config.reloaded` 类事件。
6. **审计**：每次写操作记 `audit_logs`（actor/action/target/changes/result），值做脱敏。
7. **分页与筛选**：列表端点统一 `limit`（默认 50，上限 500）+ `cursor`/`offset`；
   请求日志支持按账户、Key、状态、时间窗筛选。
8. **不泄露**：供应商凭据永不回显（只回 `set:true, preview`），hook secret 与 MCP 令牌明文只在创建时返回一次。

## 首批端点（本轮）
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/admin/api/v1/auth/login` | 用户名口令 → 会话 Cookie |
| POST | `/admin/api/v1/auth/logout` | 删除当前会话 |
| GET | `/admin/api/v1/auth/me` | 当前管理员信息 |
| GET | `/admin/api/v1/stats` | 注册表规模、限速/熔断/冷却、hook 计数 |
| GET | `/admin/api/v1/requests` | 请求日志查询（分页/筛选，含三通道标记） |
| GET | `/admin/api/v1/audit-logs` | 审计日志 |

后续端点（Key/供应商/模型/映射/标签/MCP 令牌/hooks 的 CRUD）沿用同一套中间件与热更新流程，
在 M8 剩余部分与 M9 界面中逐步补齐。

## 数据流
```
POST /admin/api/v1/auth/login → 校验用户 → 建会话（存哈希）→ Set-Cookie
后续请求 → 中间件解析 Cookie → 按哈希查会话（含过期）→ 注入管理员身份
写操作 → 业务写入 → 审计 → 失效缓存 → registry.Reload → 返回
```

## 异常与边界
- 无/无效/过期会话 → 401；角色不足 → 403。
- 登录失败：统一错误信息（不区分"用户不存在/口令错误"），并计入限速窗口。
- 会话过期清理：查询时惰性判断 + 定期清理任务（后台）。
- 写操作后 Reload 失败：返回 500 并保留旧快照（读路径不受影响），审计记为失败。

## 测试策略
- 口令哈希往返与错误口令拒绝；哈希串格式可解析。
- 登录 → 会话 Cookie 可用 → `me` 返回身份 → 退出后 401；过期会话 401。
- 登录失败限速触发 429。
- 管理端点未认证 401；用 API Key 访问管理端点同样 401（面隔离）。
- 请求日志查询分页与筛选；审计日志写入。

## 依赖
标准库（含 `crypto/pbkdf2`）+ `internal/{domain,store,registry,quota,logx}`。

## 实现与设计差异
- **口令哈希改用标准库 PBKDF2-HMAC-SHA256**：计划写的是 argon2id（`x/crypto`）。Go 1.24+ 标准库自带
  `crypto/pbkdf2`，用它可**不增加依赖**且同属合规 KDF；哈希串自带算法与迭代数（`pbkdf2-sha256$…`），
  将来切回 argon2id 时可按前缀分支平滑迁移。
- **会话令牌与 Cookie 双层**：Cookie 形如 `<session_id>.<token>`；session_id 只用于索引，
  token 经 SHA-256 后入库比较——即使数据库泄露也无法直接冒用（token 本身不可从库中恢复）。
- **本轮到鉴权与服务层为止**：管理面 HTTP 路由、资源 CRUD 与热更新接线放在本轮之后，
  避免在上下文受限时留下半可用的端点集合。
