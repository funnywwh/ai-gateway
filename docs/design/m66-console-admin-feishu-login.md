# M66 设计文档：控制台多管理员与飞书扫码登录

> 状态：**已实现（M66）**；自动化验收见 §5（Go 单测、`internal/webui` 契约测试、`scripts/ui-harness`、
> `scripts/dshgw_supervised_e2e.py`）。真机扫码验收依赖飞书应用登记与一部手机，见 `docs/TODO.md` M66。
> 上游：Key 绑定见 [M60](m60-aigw-key-feishu-binding.md)，门户登录见 [M61](m61-dshgw-feishu-login.md)，
> 自动启用 DSH 见 [M62](m62-feishu-auto-enable-dsh.md)；规格文档 [docs/feishu.md](../feishu.md)。
>
> 需求原话：「实现多管理员飞书扫码登录，账号可以设置成管理员身份」。

## 1. 目标

1. 控制台管理员从「配置文件里播种的一个账号」变成**库里的多个管理员账号**（`admin_users` 多行），
   每个账号有角色（`admin`/`viewer`）与状态（`pending`/`active`/`disabled`）。
2. 每个管理员可以**绑定自己的飞书身份**，之后在控制台登录页点「飞书扫码登录」即可进入控制台——
   用手机飞书扫码（或直接点同意）完成，不必记口令。
3. 新增管理员**可以没有口令**：管理员在控制台生成**一次性邀请链接**，被邀请人打开链接完成飞书授权，
   身份即写入该账号、账号激活，并当场进入控制台。

## 2. 关键决策

### D1 扫码由飞书的授权页提供，网关不自己画二维码

控制台登录页只放一个「飞书扫码登录」按钮，点了就 302 到飞书授权页
（`feishu.authorize_url`，页面自带二维码，也可以直接点同意）。理由：

- 现有 M60/M61 已经是一条「浏览器去授权页 → 唯一回调 → 取 open_id」的链路，扫码只是授权页的一种登录方式，
  复用同一条链路就没有第二套状态机。
- 自己画二维码意味着要引入二维码编码器（本仓库只有 4 个直接依赖），还要把「手机授权」与「桌面等待」
  绑在一起做轮询；而**桌面上那张二维码被别人的手机扫走**正是登录 CSRF 的经典形态，本功能不值得引入。

### D2 管理员身份与客户 Key 身份是两个命名空间

登录只查 `admin_users.feishu_open_id`，与 `api_keys.feishu_open_id` 无关：同一个飞书 open_id 可以既是
某把 Key 的绑定身份、又是某个管理员的登录身份，两者互不影响。反过来，**客户身份永远拿不到控制台会话**
（有一条专门的用例）。这条边界是本里程碑最重要的一条安全线。

### D3 邀请用两级令牌：长期邀请令牌 + 短时 OAuth state

飞书的 state 是**一次性**的：`StateCodec.Verify` 在回调里消耗 nonce，而回调在「用户点了取消」时也会
走一遍 state 校验（要先知道是哪个 flow 才知道把浏览器送回哪里）。若邀请令牌本身就是 OAuth state，
被邀请人在同意页误点一次取消，链接就废了，只能让管理员重新生成——这在"发链接给同事、对方在手机上试"的
场景里太脆。因此拆成两级：

| 层级 | 载体 | 有效期 | 一次性 | 撤销方式 |
|---|---|---|---|---|
| 邀请令牌 | `GET /feishu/invite?invite=<签名值>` | `feishu.invite_ttl_s`（默认 1 小时） | 否（可重复打开，直到兑换成功） | 重新生成（轮换 `invite_nonce`）/停用账号 |
| OAuth state | 授权页 URL 里的 `state` | `feishu.state_ttl_s`（默认 10 分钟） | 是（沿用现有语义） | 自然过期 |

邀请令牌的 nonce 存在 `admin_users.invite_nonce` 里：入口路由用 `Peek`（校验但不消耗）确认它仍是该账号
**当前**有效的邀请，再新签一个短时 state；回调兑换成功后**清空** `invite_nonce`，链接立即失效。
「重新生成」就是换一个 nonce —— 旧链接当场作废。

### D4 邀请兑换成功即登录，无需口令

被邀请人刚用飞书证明了自己就是要绑定的那个身份，而该身份此刻已经写进一个 `active` 的管理员账号，
所以回调直接签发控制台会话并把浏览器送进控制台。这样「首次登录」不需要任何口令：管理员建号（可选口令）
→ 生成邀请链接 → 对方打开链接 → 进入控制台。口令登录仍然保留，但只是 break-glass。

### D5 状态机：pending / active / disabled，且停用立即生效

| 状态 | 含义 | 口令登录 | 飞书登录 |
|---|---|---|---|
| `pending` | 已建号但没有任何可用凭据（无口令、未绑定） | 否（hash 为空） | 否（未绑定） |
| `active` | 至少有一个可用凭据 | 有口令才行 | 已绑定才行 |
| `disabled` | 管理员显式停用 | 否 | 否 |

- 迁移给既有行默认 `active`，bootstrap 播种的账号不受影响。
- **每请求现读**：角色来自 `admin_sessions JOIN admin_users`，所以改角色下一次请求就生效；
  停用额外在 `GetAdminSession` 里判定（并把既有会话删掉），避免"停用了但会话还能用"。
- 只邀请型账号的 `password_hash` 写空串（列是 `NOT NULL`，空串让 `VerifyPassword` 恒为 false）。

### D6 最后一名可用管理员不可被移除，自己不能删除自己

停用/降级/删除如果会让 `admin_users` 里不再有 `role=admin AND status=active` 的行，直接 409；
删除自己、或把自己从唯一管理员降级，同样 409。网关没有第二个入口能救回来（bootstrap 只在配置里写了
用户名+口令时才会重建），所以这条守卫比"操作自由"重要。

### D7 bootstrap 行：不许删，降级/停用随你

`username` 等于 `cfg.Bootstrap.Admin.Username` 的行在接口里标 `bootstrap: true`：删除直接 409
（提示改配置），因为下次启动 `UpsertAdminUser` 会按配置把它重建回来，删了只会造成"删不掉"的困惑；
停用/改角色不受限（`UpsertAdminUser` 只覆盖 `password_hash` 与 `role`，不碰 `status`）。
重置它的口令会在响应里带一条 `note`：该口令会在重启时被 `bootstrap.admin.password` 覆盖。

### D8 控制台「管理员」页与飞书功能解耦

多管理员本身不依赖飞书：`feishu.enabled=false` 时该页照常可用（列表/建号/改角色/停用/删除/重置口令），
只是隐藏邀请与解绑按钮。登录页是否显示「飞书扫码登录」由一个公开的
`GET /admin/api/v1/auth/methods` 决定（前端登录前就能问），而不是靠猜配置。

### D9 控制台与回调不同主机名时，用一次性票据交接（部署时才发现）

会话 cookie 属于**主机名**，而飞书回调只能跑在登记给飞书的那个 origin 上。本机部署的形态是
「控制台在 `http://192.168.190.86:8088`（局域网，前门不对外暴露控制台），回调在
`https://chat.tirisen.hk/feishu/callback`（公网，手机才够得着）」——两者**不同主机名**：
回调可以证明「这个人是谁」，却无法把会话 cookie 交给控制台那台主机。设计的 D2/D4 假设了同主机，
部署当天就撞上了。

补上 M61 给门户做过的同一件事：跨主机名时回调只发一张**一次性票据**
（`TicketModeConsole`，120 秒、单次兑换、绑定一个管理员账号），把浏览器送到控制台自己 origin 上的
`/admin/feishu/session?ticket=…`，由那里签发会话。新增 `feishu.console_url`（本部署写
`http://192.168.190.86:8088/admin/ui/`）说明控制台在浏览器里的地址；同主机名时沿用直接写 cookie 的
路径（少一跳，且与 M61 的 `feishuSameHost` 口径一致）。

票据与门户票据**同密钥不同 mode**（`dsh` / `console`，mode 在签名载荷里），两侧各只接受自己的 mode，
因此门户票据不能兑换控制台会话、反之亦然；`admin_user_id` 用 `omitempty`，dsh 票据的字节与 M66 之前
完全一致（共享向量测试因此不变）。

## 3. 接口与数据流

### 3.1 数据（迁移 0023）

`admin_users` 增补：`status`、`feishu_open_id`、`feishu_union_id`、`feishu_name`、`feishu_bound_at`、
`feishu_bound_by`、`invite_nonce`，以及 `NULLIF(feishu_open_id,'')` 上的唯一索引
（一个飞书身份最多绑定一个管理员账号，写法与 `api_keys` 的 M60 绑定一致）。

### 3.2 扫码登录

```
控制台登录页「飞书扫码登录」 → <base>/feishu/login?mode=admin        （匿名，按 IP 限流）
  → 飞书授权页（手机扫码或点同意）
  → <base>/feishu/callback?code=…&state=…                            （唯一回调）
       state.flow = admin：FindAdminUserByFeishuOpenID(open_id)
         ├ 没有该身份            → 说明页：尚未绑定，请联系管理员生成邀请链接
         ├ status != active      → 说明页：账号已停用
         └ active → IssueSession → Set-Cookie aigw_admin → 303 /admin/ui/
                     （控制台与回调不同主机名时改为：签一次性票据 → 303
                       <console_url 的 origin>/admin/feishu/session?ticket=… → 在那里签发会话）
```

### 3.3 邀请

```
控制台「生成邀请链接」 POST /admin/api/v1/admin-users/{id}/invite
  → 轮换 invite_nonce（旧链接作废）→ 邀请 codec 签 FlowAdminInvite(AdminUserID, Actor, nonce, InviteTTL)
  → 返回 url = <origin(callback_url)>/feishu/invite?invite=<签名值>

被邀请人打开链接 GET /feishu/invite?invite=…
  → Invites.Peek（不消耗）→ 账号存在、未停用、invite_nonce 匹配、发起人仍是 active 管理员
  → States.Sign(FlowAdminInvite, AdminUserID, Actor, Invite=该 nonce, 新 nonce) → 302 飞书授权页
  → callback：复核账号/邀请/发起人 → BindAdminUserFeishu（写身份 + 清 invite_nonce + 置 active）
             → 审计 feishu_bind → IssueSession → 303 /admin/ui/
```

### 3.4 管理接口（全部进管理路由表，新 group `admins`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/admin/api/v1/auth/methods` | 公开 | 登录方式探测（口令是否可用、飞书登录入口 URL） |
| GET | `/admin/api/v1/admin-users` | viewer | 管理员列表（含飞书绑定与是否已发出邀请） |
| POST | `/admin/api/v1/admin-users` | admin | 建号（可选初始口令；无口令则 `pending`，只能靠邀请激活） |
| PATCH | `/admin/api/v1/admin-users/{id}` | admin | 改角色 / 改状态 |
| POST | `/admin/api/v1/admin-users/{id}/password` | admin | 重置口令（一次性返回，注销既有会话） |
| POST | `/admin/api/v1/admin-users/{id}/invite` | admin | 生成/重新生成邀请链接 |
| DELETE | `/admin/api/v1/admin-users/{id}/feishu` | admin | 解绑飞书身份 |
| DELETE | `/admin/api/v1/admin-users/{id}` | admin | 删除（连带其控制台问答会话与技能库） |

响应里**绝不出现** `password_hash` 与 `invite_nonce`；邀请链接只在生成那一次返回。

### 3.5 配置

```yaml
feishu:
  admin_login: true      # 控制台允许飞书扫码登录（需先给管理员账号绑定身份）
  invite_ttl_s: 3600     # 管理员邀请链接有效期（300..604800）
  invite_secret: ""      # 邀请令牌签名密钥；空则从 credentials_key 派生（按用途分离）
  console_url: ""        # 控制台在浏览器里的地址；与回调不同主机名时必填（见 §D9）
```

## 4. 异常与边界

- **限流**：`/feishu/login?mode=admin` 与 `/feishu/invite` 共用按 IP 的 10 次/分钟窗口。
- **邀请在兑换前被撤销**：重新生成（换 nonce）、停用账号、删除账号，回调与入口两处都会拒绝。
- **邀请在同意页被取消**：短时 state 消耗掉，但**邀请令牌仍然有效**（这正是 D3 的目的），重开链接即可。
- **同一个人已是管理员并再次被邀请**：允许换绑（旧 open_id 记进审计），与 Key 换绑的口径一致。
- **open_id 已被另一个管理员绑定**：唯一索引拒绝，说明页给 `conflict`。
- **飞书不可达/凭据错误/无应用权限**：沿用 `feishu.Error` 的分类，页面只显示通用原因，细节进日志。
- **口令登录对 `pending`/`disabled` 账号**：适配层返回空 hash，接口统一答 `invalid username or password`
  （不泄露账号是否存在）。
- **删除管理员**：`admin_users` 的级联会一起删掉他的控制台问答会话与技能库——确认文案必须写明。
- **并发**：建号/改角色/邀请都是单条 SQL；「最后一名管理员」判定在写之前读一次，两个并发降级理论上可能
  同时通过——接受（控制台是单管理员操作面，且 bootstrap 仍是兜底），不为它引入事务协调。

## 5. 测试策略

- **`internal/feishu`**：4 个 flow 的签发与校验规则；`admin_invite` 缺 `AdminUserID`/`Invite` 被拒；
  `Peek` 不消耗（Peek 后 `Verify` 仍成功、可重复 Peek）；未知 flow 一律拒绝。
- **`internal/store`**：管理员 CRUD、角色枚举、状态切换、邀请轮换、绑定/解绑/按 open_id 查找、
  唯一索引冲突；`GetAdminSession` 对 `disabled` 返回未授权。
- **`internal/admin`**：`IssueSession` 签发的 cookie 能通过 `Authenticate`；`disabled` 账号口令登录必败。
- **`internal/config`**：新字段默认值、`admin_login` 关闭时不校验邀请项、TTL 越界、无签名密钥来源、
  env 覆盖、YAML 解码。
- **`internal/httpapi`**：`mode=admin` 全链路（启动 → 回调 → cookie → `/auth/me`）；邀请全链路
  （另一身份绑定 + 激活 + 开场即登录）；拒绝面（未绑定、停用、state 重放、轮换后的旧链接、
  **客户 Key 身份不得登录控制台**）；管理接口 CRUD 与三类守卫（最后一个管理员、自我操作、bootstrap 行）；
  审计行存在。
- **控制台**：`internal/webui/embed_test.go` 钉住新页与登录页的飞书入口；`scripts/ui-harness` 新增
  `admins` 视图跑真实浏览器。
- **端到端**：`scripts/dshgw_supervised_e2e.py` 新增一步，用真实二进制走完"建号 → 邀请 → 绑定并登录 →
  链接失效 → 扫码登录 → 客户身份被拒"。
- **真机**：本机部署（`feishu.enabled=true`，回调已登记）用手机飞书扫码进入控制台。

## 6. 实现与设计差异（回填）

1. **顺手修掉了一个既有缺陷：前缀部署下整个飞书面是 404 的。** 设计里只写了"新增 `/feishu/invite`"，
   实现时发现 `FeishuDeps.LoginPath/CallbackPath` 原本带着 `server.base_path` 前缀，而路由模式是注册在
   **去前缀之后**的 mux 上的（`withBasePath` 在进入 mux 前就把前缀剥掉了），所以 `base_path` 非空的部署里
   `/feishu/login`、`/feishu/callback` 从来不会匹配（本机 `base_path` 为空，因此一直没暴露）。
   现在这三个字段都是**不带前缀的服务路径**，浏览器可见地址由 `base_path + path` 拼出来，启动检查也相应改成
   比较 `<base_path>/feishu/callback`。新增的 `TestBasePathServesTheFeishuSurface` 钉住这条。
2. **口令登录与飞书登录各记一条带 `method` 的 `login` 审计**：`{"method":"password"}` /
   `{"method":"feishu"}` / `{"method":"feishu_invite"}`。设计只写了"审计 login"，但两种进门方式混在一张表里
   无法回答"他是怎么进来的"。
3. **`Peek` 也会拒绝已兑换过的 state**，不只是"不消耗"：入口路由回答的是"这条邀请还有效吗"，
   对已经用掉的 state 回答"有效"会误导调用方（新增 `reserve(nonce, now, record)`，未记录但同样查重）。
4. **改角色/改状态按列各记一条审计**：设计写的是"一次请求一条"。实现时改成写成功一列就记一条，
   因为一次请求可能改两列而第二列失败——那样第一列的权限变更就会完全没有痕迹。
5. **`pending` 账号的"重置口令"会顺带把账号置为 `active`**（设计只写了发口令）：否则新口令对一个
   `pending` 账号毫无用处（`pending` 一律拒绝登录），操作者会拿到一个不能用的凭据。若这一步失败，
   接口报错而不是把口令交出去。
6. **解绑飞书让"只剩飞书一种凭据"的账号退回 `pending`**：写在同一条 UPDATE 里（`CASE WHEN status='active'
   AND password_hash='' THEN 'pending'`），因此它的既有会话在下一次请求就被拒——与 D5 的状态机一致。
7. **`auth/methods` 进的是管理路由目录**（`group=system`，`NoTool`）：它必须是公开的，而"注册在目录里但不暴露给
   MCP"正是这个仓库表达这类路由的方式，于是它也会出现在 `admin_endpoints` 里并附上原因。
8. **邀请句柄就是 state 的 nonce 本身**，没有另造一个随机值：`invite_nonce` 存的就是"当前有效那条邀请链接"
   的 nonce，`Invite` 字段把它带过 OAuth 往返，回调比对一次即可。
9. **管理员页与飞书解耦**（设计 D8）落到实现上是：页面用 `auth/methods` 的答案决定是否显示
   「邀请链接」「解绑飞书」两个动作，其余动作在 `feishu.enabled=false` 的部署里照常可用。
11. **部署当天补上了跨主机名交接（见 D9）**：设计假设控制台与回调同主机，本机部署不是。
    新增 `feishu.console_url`、`TicketModeConsole` 票据与 `GET /admin/feishu/session` 兑换路由；
    单向约定是「同主机名走 cookie、不同主机名走票据」，与 M61 的门户口径一致。端到端脚本把
    `console_url` 故意设成 `http://127.0.0.1:<port>/admin/ui/`（回调是 `localhost`），于是每一步
    真实二进制验收都走票据路径，另有三条单测覆盖拒绝面（篡改、dsh 票据、兑换后账号被停用、重放）。
12. **端到端脚本新增 `check_admin_feishu_login` 一步**（在既有 `check_feishu_login` 之后跑）：
    建号（无口令）→ 邀请 → 另一个浏览器打开链接 → 绑定并登录 → 同一链接重开提示"已失效" →
    用登录页入口再登一次 → 客户身份被拒。设计里只写了"用真实二进制走完"，这一步就是它。
