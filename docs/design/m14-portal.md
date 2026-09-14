# M14 设计：客户自助门户

> 计划第四版 §4（已批准）。实现完成后回填第 8 节差异。分两次提交：**(1) 会话层抽取 + 门户 schema + 管理侧门户用户**；
> **(2) 门户 API + 第二套 UI**。

## 1. 身份模型（迁移 `0004_portal.sql`）

```sql
CREATE TABLE IF NOT EXISTS portal_users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',            -- active|disabled
  must_change_password INTEGER NOT NULL DEFAULT 0,
  last_login_at INTEGER, created_at INTEGER NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS portal_sessions (
  id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES portal_users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL, expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL DEFAULT 0
);
```

用户名全局唯一（登录表单只需一项），每个门户用户恰好绑定一个账户；账户被删则用户与会话级联删除。

## 2. 会话层抽取（唯一重构，先单独提交）

新增 `internal/sessionauth`：把与「主体是谁」无关的四件事从 `internal/admin` 抽出来——
1. PBKDF2-HMAC-SHA256 口令哈希/校验（沿用 21 万次迭代与 16 字节盐）；
2. 会话签发与校验：`sess_<随机>` id + 每会话随机 token，**库里只存 token 哈希**，cookie 值 `<id>.<token>`；
3. 登录失败窗口限速（按客户端键）；
4. `Config{SessionTTL, LoginAttempts, LoginWindow}`。

接口形状（泛化到 `Principal`）：

```go
type Principal struct { ID int64; Username string; Role string }
type Store interface {
  PrincipalByUsername(ctx, username) (*Principal, string /*passwordHash*/, error)
  CreateSession(ctx, id string, principalID int64, tokenHash string, expiresAt time.Time) error
  SessionPrincipal(ctx, id string) (*Principal, time.Time, error)
  SessionTokenHash(ctx, id string) (string, error)
  DeleteSession(ctx, id string) error
  TouchLogin(ctx, id int64) error
}
type Service struct{ ... }
func (s *Service) Login(ctx, username, password, clientKey string) (*Session, error)
func (s *Service) Authenticate(ctx, sessionID, token string) (*Principal, error)
func (s *Service) Logout(ctx, sessionID string) error
```

`internal/admin` 变成它的薄适配器（`domain.AdminUser` ↔ `sessionauth.Principal`，保留既有导出 API 与行为），
`internal/portal` 是第二个适配器（`domain.PortalUser`）。**既有 admin 测试是这次重构的回归网，必须继续全绿。**

## 3. 门户 API（`/portal/api/v1/*`）

- 独立 cookie `aigw_portal`（HttpOnly、SameSite=Lax、Path=/portal）；
- 中间件只认该 cookie（admin 中间件只认 `aigw_admin`），**两个方向都断言 401**；
- 所有查询/写入按 `session.account_id` 过滤；路径 id 属于他人 → **404**（不用 403，避免泄露存在性）；
- CSRF 沿用「写操作必须 `Content-Type: application/json`」规则（扩展到 `/portal/api/v1/*`）。

**读**：`/me`、`/usage?period&group_by=model|day`、`/ledger`、`/invoices`、`/invoices/{id}`（`?format=csv`）、
`/requests`、`/requests/{id}`、`/keys`、`/models`、`/mcp-tokens`。

**写（四类）**：

| 方法 | 路径 | 要点 |
|---|---|---|
| POST | `/keys` | name + 三个录制开关 + 标签（**只能选自 `portal.allowed_tags`**，为空则沿用账号绑定标签）；不接受 grants/policy；明文只回一次 |
| PATCH | `/keys/{id}` | 状态（active/suspended）与录制开关；`{"rotate":true}` 轮换并返回新明文；写后立即 `InvalidateKey(prefix)` |
| POST | `/redeem` | 兑换码入自己账户（复用 `billing.Service.RedeemCode`） |
| POST | `/password` | 当前口令校验 + 新口令 ≥12 字符；成功后**吊销该用户其它会话** |

不提供：充值/赠送/调整、账单状态流转、标签与策略、模型/路由/供应商、审计与其它账户数据。

## 4. 管理侧配套

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/v1/accounts/{id}/portal-users` | 列出该账户的门户用户（不含哈希） |
| POST | `/admin/api/v1/accounts/{id}/portal-users` | 创建；生成一次性初始口令（只回一次），`must_change_password=true` |
| POST | `/admin/api/v1/portal-users/{id}/password` | 重置口令（再次一次性返回） |
| DELETE | `/admin/api/v1/portal-users/{id}` | 停用（status=disabled）并吊销其全部会话 |

管理员的这些写操作照既有规则记审计；门户侧的写操作记 `actor=portal:<username>`。

## 5. 前端：第二套嵌入式 UI

`internal/webui` 目前 `//go:embed static` 只服务 admin。改造为**一份共享代码 + 两个入口**：

- `internal/webui/shared/{api.js,ui.js}`：把现有 `static/js/{api.js,ui.js}` 搬到这里，两套 UI 各自 embed 同一份目录
  （`//go:embed static shared` 与 `//go:embed portal shared`），并在各自前缀下服务（`/admin/ui/shared/…`、`/portal/ui/shared/…`），
  零复制、零跨前缀耦合；
- `shared/api.js` 暴露 `configure(base)`，admin 传 `/admin/api/v1`、门户传 `/portal/api/v1`（`index.html` 不写内联脚本，保持现有 CSP）；
- 门户页面：`overview`（余额/在途/本月用量）、`keys`（建/停用/轮换/录制开关）、`usage`、`invoices`（含 CSV）、
  `requests`（输入/思考/输出分栏）、`redeem`、`mcp-tokens`、`settings`（改口令）；
- admin 页面的导入路径机械改为 `../shared/api.js`，用既有 Node 导入图校验确认无遗漏。

## 6. 配置

```yaml
portal:
  enabled: false          # 默认关闭：路由不注册，/portal/* 一律 404
  session_ttl_h: 12
  login_attempts: 10
  allowed_tags: []        # 为空 = 门户只能使用账号绑定标签
```

`bootstrap` 可选 `portal_users: [{account, username, password}]`，沿用既有 upsert 语义。

## 7. 测试

1. 会话隔离双向（门户 cookie 打 admin → 401；admin cookie 打门户 → 401）；登录限速 429；
2. 作用域：账户 A 的门户用户读不到 B 的账单/请求/Key（逐端点 404）；
3. Key 生命周期：创建（标签白名单强制、明文只回一次）→ 轮换（旧秘钥立即 401、新秘钥可用）→ 停用；
4. 录制开关生效（打开后 `requests/{id}` 能看到输出文本，关闭则返回原因）；
5. 兑换码：成功入账、二次 409、跨账户码拒绝；
6. 改口令：旧口令失效、其它会话被吊销、当前会话保留；
7. `portal.enabled=false` 时所有 `/portal/*` 404；
8. 端到端：建门户用户 → 门户建 Key → 用该 Key 调 `/v1/responses` → 门户看到自己的用量与账单 → 轮换 → 旧 Key 401。

## 8. 实现与设计差异

（实现完成后回填）

## 8. 实现与设计差异（第一段：会话层 + schema + 管理侧）

1. **`sessionauth` 用泛化的 `Principal{ID, Username, Role}`**，`Store` 的 `PrincipalByUsername` 同时返回口令哈希，
   由会话层负责校验——这样管理面与门户不可能出现两套口令规则。
2. **`internal/admin` 的导出 API 与行为完全不变**（`Auth`/`Session`/`CookieValue`/`ParseCookie`/`RequireRole`/`HashPassword`），
   只是内部委托给共享实现；既有 5 个 admin 测试作为回归网，重构后全绿。
3. **`Store` 新增 `DeleteSessionsForPrincipal`**：门户改口令/停用、以及「登出所有设备」都需要按用户批量吊销；
   管理面同时获得 `DeleteAdminSessions`（此前只有单会话删除）。
4. **一次性初始口令用 `ids.New("pwd")`**（高熵随机串）而不是让人工设置弱口令；创建与重置都只回显一次，
   并强制 `must_change_password`。
5. **禁用账号的登录失败信息与「用户不存在」完全一致**（不区分原因），避免登录表单被用来枚举账号。
6. **管理侧列表端点同时支持全局与按账户**（`/portal-users` 与 `/accounts/{id}/portal-users`），控制台两种视角都能用。
7. **门户 API 与 UI 尚未落地**（本段只做身份层与管理侧），已按计划拆成第二次提交；
   因此 `portal.{...}` 配置项目前只被管理侧读取，`enabled=false` 时门户路由尚未注册（下一次提交接上）。
