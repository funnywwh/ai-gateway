# M60 设计文档：aigw 的 API Key 飞书绑定与解绑

> 状态：**已实现（M60，代码与自动化测试完成；真机验收待执行——需要飞书自建应用与已登记的回调地址）**。
> 面向使用者的规格：[docs/feishu.md](../feishu.md)；M61（dshgw 门户飞书登录）消费本里程碑写入的绑定，
> 设计见 `docs/design/m61-dshgw-feishu-login.md`。
>
> 需求原话：「dsh实现飞书绑定apikey登录」→「先实现aigw 在key里绑定和解绑」→「同时实现dshgw飞书登录到dsh」。

## 1. 目标

1. 管理员在控制台 **API Keys** 页把一把 API Key 绑定到一个**真实存在的飞书账号**（通过飞书授权页证明所有权，而不是手填一个 id）。
2. 同一页可以解绑；列表清楚显示「这把 Key 绑的是谁」。
3. 一把 Key 只能绑一个飞书账号，一个飞书账号只能绑一把 Key；冲突必须被拒绝且**不覆盖**已有绑定。
4. 绑定状态不会被任何其它编辑路径（PATCH 标签/状态/录制/策略）清掉。
5. 绑定本身**不参与数据面鉴权**：它只回答「这个人是谁」，供下一个里程碑（DSH 门户登录）使用。

### 非目标（本里程碑明确不做）

- dshgw 侧的登录消费（M61）、客户门户（`internal/portal`）的自助绑定、管理员用飞书登录控制台。
- 按飞书部门/组授权、通讯录同步、事件订阅（离职自动解绑）、多对多绑定。
- 手工录入 `open_id` 的离线绑定：本里程碑只有授权页一条证明路径。

## 2. 关键决策

### D1 绑定真值落在 `api_keys` 行上

`feishu_open_id` / `feishu_union_id` / `feishu_name` / `feishu_bound_at` / `feishu_bound_by` 五列。
一对一关系，不建独立表：Key 被删时绑定随之消失（语义天然正确），列表查询也不需要 join。
`feishu_bound_by` 记录「谁做的这次绑定」，是审计线索，不是权限判据。

### D2 唯一性用表达式唯一索引

```sql
CREATE UNIQUE INDEX idx_api_keys_feishu_open_id ON api_keys(NULLIF(feishu_open_id, ''));
```

空串映射成 NULL，SQLite 的唯一索引把 NULL 视为互不相同，因此未绑定的 Key 不会互相冲突，而绑定的
`open_id` 全局唯一。表达式索引在本仓库已有先例（`0018_org_structure.sql` 的 `COALESCE(parent_id,0)`），
不依赖 partial index 支持。

### D3 绑定键是 `open_id`

应用内唯一、稳定，并且飞书文档标注 `authen/v1/user_info` 返回的 `open_id` 与姓名**无需任何权限**。
因此本功能不申请任何 scope：授权页只显示最基础的同意项，也不申请邮箱/手机号（飞书明确这两项是管理员
导入、非本人实时验证，不适合当登录凭据）。

### D4 一个飞书应用、一个回调，两种 flow

`GET /feishu/callback` 同时服务「控制台绑定」与「DSH 门户登录」（M61），靠签名 state 里的 `flow` 分派。
**发起**则是两条入口：绑定走 `/admin/api/v1/keys/{id}/feishu/bind`（需要管理员会话），登录走
`/feishu/login`（匿名）。取舍：只需在飞书后台登记**一个**重定向 URL，少一处
`redirect_uri unmatch`(2000)/20029 的来源；代价是回调里多一个分支。

绑定为什么必须从 `/admin/...` 直接跳飞书，而不是先跳到公开的 `/feishu/login?mode=bind`：
管理会话 cookie 的 `Path` 是 `/admin`，公开路由收不到它——这正是最初实现在浏览器里报
「missing admin session」的原因（见 §6.9）。

### D5 state 自签 + 单次使用，不依赖会话 cookie

`state = base64url(payload).base64url(HMAC-SHA256(key, payload))`，`payload={v,flow,nonce,exp,key_id,actor}`。
签名密钥由 `feishu.state_secret` 提供，为空时从 `credentials_key` 派生（`DeriveSecret(material,"feishu-state")`）。
校验要求：签名（常量时间）、`exp` 未过期、且不超过配置窗口+1 分钟（防止旧密钥签出的长期状态）、nonce 未用过。
nonce 集合在内存中有界（4096）并按 TTL 清理。

- **绑定流程复用管理会话而不是新的 cookie**：state 由 `adminActor(requireAdmin)` 之后才签发，
  **回调时再次读取操作者记录**（`GetAdminUserByUsername`）并要求仍是 `admin` —— 中途被降权/删号的
  操作者不能完成他发起的绑定。
- **登录流程（M61）完全匿名**：任何员工都可以为自己登录，capability 就是 state 本身（10 分钟、单次使用）。

### D6 绑定/解绑用专用单列 UPDATE，且 `UpsertAPIKey` 不碰飞书列

`handleAdminPatchKey` 的形态是「读整行 → 改若干字段 → `UpsertAPIKey` 重写所有列」，该函数注释里
记着录制开关曾被这样静默覆盖。因此：

- `BindAPIKeyFeishu` / `UnbindAPIKeyFeishu` 各自是一条只写飞书列的 UPDATE；
- `UpsertAPIKey` 的 INSERT/UPDATE 列表**不包含**飞书列 —— 任何整行重写都无法表达（也无法清空）绑定。

回归测试 `TestAPIKeyFeishuBindingSurvivesARowRewrite` 把这条钉死：先绑定，再模拟 PATCH 路径写回整行
（甚至故意把结构体里的飞书字段清空），绑定必须仍在。

### D7 回调地址显式配置 + 启动期一致性校验

`feishu.callback_url` 是**浏览器可见**的绝对地址（本部署经 gwproxy 8090）。aigw 无法可靠推断自己
前面的 origin（端口、前门、base path 都会变），所以它是配置项；启动时校验它的 path 等于
`<server.base_path>/feishu/callback`，填错就在启动时报错，而不是等用户走完授权页再撞一个飞书错误页。

### D8 失败一律回「知道自己在哪」的那一面

- flow 属于绑定 → 303 回控制台 `#/keys?feishu=<result>&key=<id>`，由控制台把结果码翻成中文；
- flow 属于登录（M61）→ 303 回门户 `/feishu/error?reason=<reason>`，由门户渲染它自己的页面；
- **state 不可信时 flow 未知** → 在 aigw 上渲染一个极简说明页（400），不猜测去处。

### D9 出网防御

飞书客户端用标准库自写（`internal/httpapi` 不允许 import `internal/providers/httpx`）：禁重定向
（否则 app_secret 会被送到 Location 指向的地方）、64 KiB 响应上限、显式超时、显式不跟随环境代理。
错误按四类归因：授权码无效/过期/已用、应用凭据被拒、应用不可用（未发布/不在可用范围）、不可达。

### D10 分层与解耦

新包 `internal/feishu`（client / state / ticket）登记进 `internal/arch` 的分层表：它只依赖
`internal/config` 与 `internal/domain`，不碰 store 与传输层。aigw **不得** import `internal/dshgw/**`
（连测试也不行），两侧对票据的一致理解靠**共享测试向量**保证（见 M61 §票据契约）。

## 3. 接口

### 3.1 路由

| 方法 | 路径 | 鉴权 | 行为 |
|---|---|---|---|
| GET | `/feishu/login` | 匿名（每 IP 限流） | 签 `flow=dsh` 的 state → 302 到飞书授权页（门户登录的唯一公开入口） |
| GET | `/feishu/callback` | 无（state 验签） | 换 token → user_info → 按 flow 分派 |
| GET | `/admin/api/v1/keys/{id}/feishu/bind` | admin | 校验 Key 存在且 active → 签 `flow=bind` 的 state → **一步** 302 到飞书授权页 |
| DELETE | `/admin/api/v1/keys/{id}/feishu` | admin | 解绑，幂等，`{"unbound":bool,"key_id":int}` |
| GET | `/admin/api/v1/keys` | viewer+ | 每行新增 `feishu` 对象 |

未配置飞书时**路由不注册**（404），控制台不渲染任何飞书元素。

结果码（控制台绑定）：`bound` / `replaced` / `cancelled` / `conflict` / `rejected` / `expired` /
`invalid` / `rate_limited` / `no_app_permission` / `app_error` / `error`。

### 3.2 列表 JSON

```json
"feishu": {"bound": true, "open_id": "ou_…", "name": "张三", "union_id": "on_…",
           "bound_by": "admin", "bound_at": "2026-09-18T10:00:00Z"}
```

未绑定恒为 `{"bound": false}`（形状稳定，前端不必猜）。

### 3.3 审计

`feishu_bind` / `feishu_bind_reject` / `feishu_unbind` / `feishu_bind_start` / `feishu_login_reject`
（`target_type=api_key`，changes 含 `open_id`/`union_id`/`name`/`previous_open_id`，绑定失败含 reason）。
`feishu_dsh_login` 属于 M61。**不含** code / access_token / app_secret / state 明文。

### 3.4 MCP 工具面

按 `docs/mcp.md` §4.5 写全：`admin_bind_key_feishu`（GET，`NoTool`——MCP 没有浏览器，302 到授权页没有意义）
与 `admin_unbind_key_feishu`（DELETE，`Dangerous` + `ConfirmReason`，含 body/返回形状说明）；
`admin_list_keys` 的 Summary 补充飞书绑定。守卫测试 `admin_routes_test.go` 的期望集合同步更新。

## 4. 数据流（绑定）

```
控制台「绑定飞书」→ GET /admin/api/v1/keys/{id}/feishu/bind（带 aigw_admin cookie）
  → adminActor(requireAdmin) → GetAPIKeyByID（不存在 404 / status!=active 409）
  → 302 /feishu/login?mode=bind&key=<id>
  → 限流 → sign(state{flow:bind,key_id,actor,nonce}) → 302 飞书授权页
  → 用户同意 → GET <callback_url>?code=…&state=…
      ├ state 不可信/过期/重放 → 400 说明页（不猜去处），审计 feishu_login_reject
      ├ error=access_denied     → 303 #/keys?feishu=cancelled
      └ 通过 → 重新确认 actor 仍是 admin（否则 rejected）
            → POST oauth/v3/token → GET authen/v1/user_info → open_id
            → BindAPIKeyFeishu（单列 UPDATE；唯一冲突 → conflict，不覆盖）
            → 303 #/keys?feishu=bound|replaced&key=<id>
```

## 5. 异常与边界

- 飞书拒绝授权：`cancelled`，不做任何写入。
- 授权码无效/过期/已用（20003/20004/20065）：控制台提示「重新绑定」（`expired`）。
- 应用凭据被拒（20002 等）与「用户无应用使用权限/未安装/未启用」（20009/20010/20069）：日志 WARN + 明确文案，
  不冒充用户错误（`app_error` / `no_app_permission`）。
- 超时/5xx/非 JSON/超大响应/重定向：按不可达归因（`error`），绝不把「aigw 到不了飞书」显示成「你没有权限」。
- 换绑：允许（管理员显式操作），旧 `open_id` 记入 `previous_open_id` 并返回 `replaced`。
- 已绑定的 Key 之后被停用/过期：**绑定保留**（不清空），只是该身份无法用它登录。
- nonce 集合在内存：进程重启后 10 分钟内的 state 理论上可再用一次，仍受一次性授权码与唯一索引约束。
- `open_id` 是应用内标识：换 app_id（重建应用）后所有绑定失效，需要重新绑定；文档明示。

## 6. 实现与设计差异（回填）

1. **`UpsertAPIKey` 未纳入飞书列**（设计 §D6 原写「加进列表并保证 round-trip」）：实现时发现
   *不*加入才是更强的保证——整行重写无法表达绑定，PATCH 路径永远不可能清空它。回归测试按这个更强的
   语义写（故意把结构体里的飞书字段清空后 Upsert，绑定仍在）。
2. **state 校验额外要求「exp 不超过配置窗口+1 分钟」**：设计只写了「未过期」。加上上界后，一把旧密钥
   签出的长期状态也无效。
3. **`/feishu/login` 拒绝携带 `code`/`state` 的请求**（400），但对 `mode=bind&key=…` 放行——第一版误把
   `key` 也列入拒绝参数，绑定入口因此打不通，测试当场发现。
4. **state 不可信时改为 aigw 自带说明页**（设计原本写「一律 303 回控制台或门户」）：flow 未知时无法判断
   该回哪一面，猜测会把管理员送到门户、把普通用户送到控制台。说明页只含一行文案与一个返回控制台的链接。
5. **`feishu.dsh_login=true` 且 aigw 未监督 dshgw 时要求显式 `feishu.portal_url`**：否则会从 `dshgw`
   块的默认值（`localhost:31000`）派生出一个并不存在的门户地址。独立形态（门户在他处）手填即可。
6. **回调的 `redirect_uri` 一定随 token 请求发送**：飞书对不一致返回 20071。
7. **票据 cookie 的 `Secure` 跟随 `callback_url` 的 scheme**：明文 HTTP 部署下浏览器会丢弃 Secure cookie，
   那会表现成「登录成功又被弹回门户」。
8. **顺带修复两处既有失效断言**（与本里程碑无关、在 HEAD 上就已失败）：`tags_binding_test.mjs` 与
   `org_tree_test.mjs` 仍在断言 M49 之前的代码形状（accounts 页多了一个派生字段、组织页已不再挂侧边栏树、
   账户列表改用 `orgQuery()`）。修法是断言**意图**（按 id PATCH、经 splitTags、走 orgQuery）而不是整段
   字面量，且把「不得触及侧边栏」写成结构化检查而不是全文 `/sidebar/` 子串禁止——后者会被一句解释性注释
   绊倒。发现方式：在新 worktree 里跑 HEAD 复现，确认与本次改动无关。

9. **绑定从「两跳」改成「一跳」**（真机反馈驱动）：最初设计里控制台的绑定入口 302 到公开的
   `/feishu/login?mode=bind&key=…`，由它签 state。但管理会话 cookie 的 `Path=/admin`，浏览器**不会**
   把它发给 `/feishu/login`，于是第二跳拿到的是「missing admin session」的 JSON（用户点击「绑定飞书」
   时的实际现象）。现在绑定入口自己签 state 并直接 302 到飞书授权页，公开入口只保留匿名登录。
   教训同时改进了验收：端到端脚本的 `Browser` 原先忽略 cookie 的 `Path`，会把浏览器根本走不通的
   链路判为通过——现在它按 Path 作用域发送 cookie（这条修正让同一个 bug 在脚本里也会失败）。
   单元测试也补了「公开路由不接受 mode=bind（404）」与「绑定入口一步直达授权页」。

## 7. 测试策略（已实现）

- `internal/store`：迁移幂等；绑定/解绑/按 open_id 解析往返；唯一冲突 → `ErrConflict` 且两侧都不被破坏；
  解绑幂等；**整行重写不清空绑定**；列表带出绑定。
- `internal/feishu`：state 篡改/过期/重放/异密钥/超窗；client 的错误码归因、非 JSON、缺 token、
  超大响应、禁重定向、空授权码；授权 URL 参数与转义；票据共享向量（见 M61）。
- `internal/httpapi`：角色矩阵（401/403/通过）；非 active Key 409、未知 Key 404；绑定全链路（含
  `redirect_uri`/`client_id`/`no scope` 断言）与列表形状；11 个结果码分支；state 三类不可信输入 → 400 说明页；
  一次性 state 重放 → 400 且不写入；操作者被降权 → `rejected` 且不写入；冲突不覆盖；换绑 `replaced`；
  未配置时四条路由 404；限流 429；按 host 选择 cookie/query 票据；Secure 随 scheme。
- 机密性：成功与失败路径都扫描响应体、Location 与审计，断言不含 `secret`/`u-token`/授权码。
- 控制台：`internal/webui/tests/keys_feishu_test.mjs`（node，无浏览器）验证飞书列、绑定/解绑动作、
  只读禁写、11 个结果码文案、参数清除，以及「控制台绝不构造飞书请求、绝不出现凭据字段」。

## 8. 验收（真机步骤见 docs/feishu.md §5）

自动：`go test ./internal/... ./cmd/...`、`make ui-base`、`make verify`。
真机：飞书后台建应用 → 安全设置登记 `http://<host>:8090/feishu/callback` → 发布版本并设可用范围 →
配 `feishu.*` 与 `dshgw.public_scheme` → 重启 aigw → 控制台绑定 → 编辑该 Key 复核绑定仍在 → 解绑 →
同飞书号绑第二把 Key 冲突 → 日志/审计无凭据。
