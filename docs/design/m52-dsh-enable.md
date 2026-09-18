# M52 设计文档：aigw 后台“启用 DSH / 停用 DSH”账号级开关

> **已被 M58 取代（部署形态）**：本文记录的 root 安装 + systemd 单元 + 每租户 OS 用户 + nginx 边缘
> 已从代码中删除；dshgw 现在是 aigw 拉起并监督的同目录子进程（同 UID、无 root、无 systemd、
> 无共享服务账号），租户 worker 是它的 bubblewrap 子进程。当前形态见
> `docs/design/m58-aigw-supervised-dshgw.md`、`deploy/dshgw/README.md` 与 `docs/dshgw.md`；
> 本文保留为历史记录（其中的协议、权限与信任边界的分析仍然有效）。

> 状态：**已实现（M52-rev2，代码与回归完成；主机验收待执行、未提交）**。
> 2026-09-16 两轮确认：①默认 `login` 档、停用即停 worker；②按用户要求升级为 **rev2**——启用=账号级授权，账号下所有 Key（含新建）都能登录，且启用/停用全部由后台按钮完成，不经 root CLI。前提：M51 的端口门户 `https://chat.tirisen.hk:32600/`
> 与已实现 dshgw 保持原样；M51 剩余主机验收与发布被本里程碑取代优先级，M51 未提交改动原样保留。
> 背景决策：用户认为“租户自己粘贴 Key + 复杂真实 Key 轮换验收”操作过重，要求在 aigw 后台
> 账号列表提供 dsh 入口的启用/停用按钮。

## 1. 目标

1. 管理员在 aigw 后台账号列表点击 **启用 DSH / 停用 DSH**，控制该账号能否使用 dsh 入口。
2. 停用后：新登录被拒绝；在合理时限内既有 dsh 会话也失效（档位见 D4）。
3. 不向浏览器/管理员/日志暴露 API Key、worker cookie、session token。
4. 不修改 dsh 发行包；不要求 aigw 直接操作 root 侧 dshgw registry 或 systemd。
5. 本里程碑只做**账号级开关与执行**；自动建户/provisioning 另立后续里程碑。

### 非目标（M52 明确不做）

- 自动创建/删除 dsh 租户、OS 用户、端口分配（需要 root 生命周期，属后续里程碑）。
- dsh 会话 cookie 的跨服务直接吊销（dshgw 侧通过执行点实现，不走 aigw 直连 dshgw）。
- 把账号 API Key 用作 dsh 登录凭据以外的凭据分发。

## 2. 关键决策

### D1 状态真值放 aigw，不放 dshgw

`accounts` 表新增一列 `dsh_enabled`（SQLite 迁移，默认 false）。后台按钮、审计、界面读它；
dshgw 不新增任何租户拓扑知识。取舍：aigw 不知道租户名/端口，换来单一真值、零拓扑复制。

### D2 执行点 = dshgw 登录/重验调用 aigw 的授权判定

新增 aigw 公开端点（与 `/v1/models` 同级鉴权语义，凭 Bearer 用户 Key）：

```
POST /v1/dshgw/authorize
Authorization: Bearer <key>   （也接受 X-API-Key）
→ 200 {"allowed":true}
→ 403 {"allowed":false}       （dsh_enabled=false 或账号 suspended/closed）
→ 401                          （Key 无效/停用）
```

dshgw 门户 login 在现有 `/v1/models` 校验之后追加调用该端点；403/401 拒绝登录并沿用现有
login_reject 审计。dshgw 与 aigw 都在既有仓库内改动，不触碰 dsh 发行包。
取舍：多一次 dshgw→aigw 调用；换来停用语义即时生效且无需 dshgw 拓扑同步。

### D3 `dshgw authorize` 档位（复用 key_revalidate 形态）

| dshgw 配置 `dsh_enforce` | 行为 |
|---|---|
| `login`（默认） | 仅登录时判定；停用后新登录拒绝，既有会话存活至 TTL/退出 |
| `interval:<秒>` | 按租户缓存判定，过期后下一请求重新判定；近似及时停用 |
| `per-request` | 每请求判定；停用几乎立即生效，代价是每请求一跳 aigw |

判定失败（超时/5xx）不冒充“已停用”：与 key_revalidate 相同语义，返回 503 而不是放行。
403（明确停用）才撤销/拒绝。账号 suspended/closed 时 authorize 恒为 403，与按钮独立。

### D4 后台接口与界面

```
GET  /admin/api/v1/accounts/{id}/dsh        → {"enabled":bool,"updated_at":...}
POST /admin/api/v1/accounts/{id}/dsh        body {"enabled":true|false}
```

- 账号列表行内按钮：未启用→[启用 DSH]；已启用→[停用 DSH]；显示状态徽标。
- 响应不含 Key/cookie/入口 URL 拓扑；管理员告知用户入口仍走门户 `:32600`。
- 审计 `dsh.enable` / `dsh.disable`，记录操作者、账号 id，不记录任何凭据。
- 若后续把该操作暴露进 MCP 工具，按 `docs/mcp.md` §4.5 补 Schema/RawBody，并跑 mcpsrv/httpapi 测试守卫。

### D5 与既有 dshgw 会话/退出语义的关系

- `logout`、TTL、跨租户隔离、Origin/CSP 闸门全部不变。
- `key_revalidate` 仍验证 worker `gateway.key`（模型凭据）；`dsh_enforce` 只判定“账号可否继续用 dsh”，
  二者正交，不互相替代。
- 停用不删除租户数据/workspace；重新启用即恢复（数据未动）。

## 3. 数据流

```
[后台按钮] → PATCH(dsh) → accounts.dsh_enabled=1/0 → 审计
[用户登录] → dshgw /login → aigw /v1/models → aigw /v1/dshgw/authorize → 允许:302 租户 / 拒绝:审计+403
[租户请求] → (dsh_enforce≠login) dshgw → authorize（带缓存） → 403: 撤销该请求会话并 302 门户
```

## 4. 异常与边界

- authorize 不可达/超时：login→503“认证服务暂不可用”；请求期→503，绝不放行。
- Key 有效但 dsh_enabled=false：login_reject 审计 reason=dsh-disabled，页面提示“该账号未启用 dsh”。
- 账号 suspended/closed：authorize 403（reason=account-status），优先于 dsh_enabled。
- 重复设置同值：幂等 200，不重复审计成功（可记 no-op，避免日志噪声）。
- 迁移向后兼容：列默认 false；旧行为（不部署新 dshgw / 不调用 authorize）不受影响——
  authorize 是 dshgw 主动调用，aigw 独立升级时旧 dshgw 照常工作。
- 并发：单行 UPDATE + 审计同一事务语义沿用现有 store 模式；按钮防双击由前端 disabled。

## 5. 测试策略

- store 迁移与开关读写单测；账户 suspended 时 authorize 恒 403。
- httpapi：新端点鉴权矩阵（admin cookie、无凭据、非 admin）；幂等与审计断言。
- dshgw proxy：login 追加 authorize 的三态（allow/deny/503）；`dsh_enforce` 三档缓存行为；
  deny 后请求期撤销会话；现有 Origin/CSP/logout 回归保持全绿。
- UI：账号列表按钮状态切换与禁用态；参照现有管理界面测试方式。
- 主机验收（后置）：真实宿主上启用→登录成功；停用→登录拒绝 + interval 档既有会话按缓存时限失效；
  重新启用恢复；A/B 租户互不影响。

## 6. 实现锚点（只读核对，2026-09-16）

- 迁移：`internal/store/migrations/` 下已至 `0018_org_structure.sql`，新列为
  `0019_account_dsh_enabled.sql`：`ALTER TABLE accounts ADD COLUMN dsh_enabled INTEGER NOT NULL DEFAULT 0;`
  与 `0005_account_markup_flag.sql`/`0017_account_tags.sql` 同模式；`internal/domain/entities.go`
  Account 增字段，`internal/store/accounts.go` 的读取/UPSERT 列清单同步。
- dshgw 侧 authorize 调用落在 `internal/dshgw/aigw/client.go` 的 `ValidateKey`（:59）旁，
  复用其超时/重定向/尺寸防御与测试模式（`client_test.go`）。
- 后台路由挂在 `internal/httpapi/admin_catalog.go`（现有 `handleAdminPatchAccount` :239 的
  decodeJSON→store→audit→writeJSON 模式）；路由注册与守卫测试同步补 `GET/POST .../dsh`。

## 7. 依赖

- aigw：store 迁移（0019）、httpapi 新端点、admin 路由表、webui 账号列表。
- dshgw：config 新增 `dsh_enforce`，aigw 客户端新增 authorize 调用，proxy login/revalidate 集成。
- 不依赖：root、systemd、dsh 发行包、nginx 变更。


## 8. 实现与设计差异（回填）

- `ParseDSHEnforce` 额外接受空串为 `login`：config 结构体字面量构造（测试/内部）缺省字段时与 YAML 默认一致，避免隐式 `off`。
- authorize 处理器把 Verifier 对 suspended/closed 账号的 402 映射为 403 `{"allowed":false,"reason":"account_status"}`；401 保持 401。dshgw 侧 401→登录页"Key 无效或已停用"，403→`*aigw.DSHDenial`，其余→503 fail-closed。
- 管理端启用/停用走 `UpsertAccount` 后调用 `s.reload(..., invalidateAll=true)`：key verifier 缓存了账号行，不清缓存则开关在一个 verifier TTL 内不生效。与 PATCH accounts 的既有缓存失效规则一致。
- `accountJSON` 增加 `dsh_enabled` 字段，账号列表据此渲染徽标与按钮文案（设计 D4 的"状态徽标"落地形态）。
- dshgw `Authorizer` 为可注入字段（serve.go 注入 aigw client）；nil 且需要判定时一律 fail-closed：登录 503、请求期 503，绝不放行。
- 请求期判定缓存（interval 档）复用 revalidation 锁条带与结构，独立 `dshChecks` map，避免与 key_revalidate 的缓存互相干扰；拒绝结果同样缓存（拒绝在一个 interval 内持续，属设计内的时限语义）。
- 登录失败文案：`dsh_disabled`→"该账号未启用 dsh"；`account_status`→"账号已停用，无法登录 dsh"；审计 kind 仍为 `login_reject`，reason 分别为 `dsh disabled` / `account suspended`。

## 9. 部署与主机验收（待执行）

1. 重建并重启 aigw（新端点/迁移在 aigw 侧）；aigw 启动即自动执行 0019 迁移。
2. 重建并以既有 install.sh 流程更新 dshgw + 重启 dshgw.service（新的 Authorizer 接线与 dsh_enforce 默认 login）。
3. 验收：后台启用某测试账号→该账号 Key 登录门户成功；停用→新登录 403"未启用"、既有会话保持（login 档）；改 `dsh_enforce: interval:60` 后既有会话 ≤60s 失效；重新启用恢复；A/B 租户互不影响；审计出现 dsh_enable/dsh_disable。
4. 浏览器人工走查账号列表按钮与徽标。


## 10. M52-rev2：账号级授权 + 后台自动 provisioning（按用户确认实现）

### 语义变更（覆盖 §2/D1–D5 中与登录授权相关的部分）

- **登录身份 = 账号开关**：authorize 200 响应携带 `tenant` 名；dshgw 按租户名解析登录，不再要求 Key 前缀绑定。账号下所有 Key——现有与新建——都能登录，新建 Key 无需任何绑定步骤。
- 前缀绑定降级为兜底：仅当 aigw 返回空 tenant（旧版 aigw 或未迁移账号）时走原前缀路径。
- worker 的模型凭据与登录身份彻底分离：启用时由 aigw 铸造专用内部 Key（`dshgw-<tenant>-<rand>`），经本地通道写入 gateway.key；登录 Key 只做身份。

### 新组件：dshgw root 守护（`dshgw admin-serve`）

- UNIX socket（`admin_socket` 配置，默认经 systemd `RuntimeDirectory=dshgw` 提供 `/run/dshgw/admin.sock`）；**无 TCP**。
- 每连接 `SO_PEERCRED` 校验 `admin_allowed_uids`（UID 0 恒允许）；socket 文件 0660 且单一允许 UID 时 chown 给该 UID。
- 操作：`ping / tenant-list / tenant-create / tenant-start / tenant-stop / tenant-set-key`；单连接单请求一行 JSON。
- `tenant-create/set-key` 由守护自行向 aigw `/v1/models` 验 Key 取模型清单（与 CLI 同路径），复用 Manager（Create/RotateKey/StartWorker/StopWorker）与生命周期锁；不写任何日志含 Key。
- 新 systemd unit `dshgw-admin.service`（root 运行，Restart=on-failure），install.sh 一并安装。

### aigw 侧

- 迁移 0020：`accounts.dsh_tenant`。
- `POST /admin/api/v1/accounts/{id}/dsh` 启用流程：租户名（请求指定 > 既有映射 > `dsh-<账号 slug>` 自动生成并去重）→ 铸造内部 Key → 不存在则 create / 已存在则 set-key+start → 写 `dsh_tenant`+`dsh_enabled` → 审计（含租户名、不含 Key）→ InvalidateAll。
- 停用流程：`tenant-stop` → 吊销 `dshgw-<tenant>` 前缀的 active Key → `dsh_enabled=0`（保留映射供重启用）→ 审计。
- 失败一致性：provisioning 失败不改账号标记，可重试；suspended/closed 账号拒绝启用。
- `GET /v1/dshgw/authorize`：200 带 `tenant`；启用但未指定租户 → 403 `dsh_tenant_unassigned`。
- UI：启用为模态框（可填租户名，预填 slug），停用为确认框；列表徽标显示租户名。

### 新分层

- `internal/localdshgw`：aigw 侧 socket 客户端（协议对等），aigw↔dshgw 仍互不 import 内部包；layering 表登记。

### rev2 测试

- cmd/dshgw：协议往返、全操作语义、路径穿越/缺 Key/未知 op 拒绝、外来 UID 拒绝、ListenAdmin 配置与权限（0600/0660/chown）。
- internal/httpapi：启用→租户建立+映射+审计无 Key 泄漏；新 Key 未经绑定即可通过 authorize；停用→worker 停+Key 吊销+映射保留；重启用→rotate+start 不重建；失败不改标记；suspended 拒绝；指定租户名生效。
- proxy：按租户名登录、空 tenant 前缀兜底、未知租户 403。

### rev2 主机验收（待执行）

1. root 安装后 `systemctl enable --now dshgw-admin.service`；`/etc/dshgw/config.yaml` 增加 `admin_socket` 与 `admin_allowed_uids: [aigw 运行 UID]`；aigw 配置确认 `dshgw.admin_socket` 默认值一致后重启 aigw。
2. 后台对测试账号启用（可指定租户名）→ 不碰 root：租户建立、worker 启动、账号下旧 Key 与**新建 Key** 均可登录门户并跳转租户端口。
3. 停用 → worker 停止、新登录拒绝、内部 Key 变 disabled；重新启用 → worker 恢复、新内部 Key 生效。
4. A/B 等其他租户与账号全程不受影响；审计无 Key 明文。


### rev2 部署后排查记录：租户 UI「settings are unavailable」（2026-09-16）

用户在后台启用账号并登录新租户后，dsh UI 报「加载提供方目录失败: settings are unavailable in this browser」。排查结论：

1. 授权链排除：worker 内部 Key（dshgw-dsh-tenant-*）经 /v1/models 返回 4 个模型，租户 models_pending=false。
2. worker 层排除：临时 HOME + 真实 DSH + browser-fs/picker-clamp 插件，直连 `POST /api/settings/describe` 返回 ok:true 全量视图（14 namespaces）。注意该 RPC 的 args 为空对象（带 request 包装会被 typert 拒绝）。
3. 网关层排除：新增回归 `TestSettingsDescribeThroughGatewayWithRealWorker`（真实 worker + 完整登录/握手/代理链路）证明 describe 经网关完整到达浏览器（200 / ~22.5KB / value 完整）。已固化为环境门控回归。
4. 浏览器层排除：本地全栈 + 真实 Firefox（selenium + geckodriver 0.37.1，自签 TLS 双端口门户/租户），门户登录→租户应用→模型/提供方页全部正常渲染，无该报错。
5. 残余差异（在生产侧待确认）：用户浏览器（Chrome）的陈旧页面/缓存状态、nginx 层、或启用轮换窗口期的瞬时状态。处理建议：彻底关闭标签后用隐私窗口重新登录复验；若仍复现，回传 F12→Network→XHR 过滤 `describe` 的状态码与响应预览（result.value 是否存在），以及 e2e-b 租户同页对照。
6. 过程中修复：守护 create 补 listeningPorts 端口占用守卫（与 CLI 一致）；aigw 启用流程不再允许空模型建户（无授权时明确报错而非静默建残缺租户）。

### settings 视图上游限制（排查定论，2026-09-16；2026-09-17 已复核修正，见下节）

租户 UI「settings are unavailable in this browser」根因为 **DSH 0.1.2-rc.1 上游设计**：设置镜像
`ensure()` 在页面 authority 非回环时直接返回（`persistence="memory"`，构造注释
"non-loopback pages may remain process-local"），从发起 `settings.describe`。`--trusted-host` 是
worker 端 /api 的 Host 防护（DNS rebinding/跨站），nginx loopback 转发已满足，与此无关。
服务端全链路（worker 直连、含插件、经网关、真实 Firefox 端到端）已全部验证健康并固化为回归
`TestSettingsDescribeThroughGatewayWithRealWorker`；本地 `127.0.0.1` 访问时该视图正常。

待用户决策：A 接受上游限制并文档明示（保持零垫片边界，推荐）；B 在网关注入
`__DSH_TRANSPORT__` 垫片（需实现 fetch 传输并突破 D5 决策，不推荐）。

### 复核修正（2026-09-17）：差异来自入口 nginx，而非上游

用户对照发现 `https://chat.tirisen.hk/dsh/` 的“设置/模型”不报错、租户端口 `:32604` 报错。复核证据：

1. 两侧运行的都是同一份 DSH 0.1.2-rc.1（`~/.local/dsh-0.1.2-rc.1` 与 `/opt/dsh/current`），
   磁盘上的 `dsh-client-connection`/`dsh-client-ui-settings` 逐字节相同。
2. 但**下发到浏览器**的插件包不同：`/dsh/` 的 bundle 里
   `isLoopbackHostname` 首行为 `if (hostname === "chat.tirisen.hk" || hostname === "localhost" …)`，
   租户 worker 的是原版 `if (hostname === "localhost" || hostname === "[::1]")`。
3. 改写者是宿主机 nginxWebUI 容器生成的 `/home/nginxWebUI/nginx.conf`：
   `location ^~ /dsh/` 内 `sub_filter 'if (hostname === "localhost" || hostname === "[::1]") return true;'
   'if (hostname === "chat.tirisen.hk" || hostname === "localhost" || hostname === "[::1]") return true;'`
   （注释写明 “Extend that client-side allowlist to this LAN-only trusted domain”），并配
   `proxy_set_header Accept-Encoding ""` 保证过滤生效。dshgw 生成的租户 vhost 无此类指令。
4. 日志佐证：nginx 访问日志中 40 次 `POST /api/settings/describe`（200）**全部** Referer 为
   `https://chat.tirisen.hk/dsh/`；租户端口侧只出现 `credentials/describe`，从未发出
   `settings/describe`——与“memory 档不发 describe”的代码路径一致。
5. 两侧加载的前端资源同名同内容（`index-Df-65__b.js` 等），因此差异只能来自下发时改写。

结论：这是**入口层已存在的域名白名单改写**（`/dsh/` 有、租户端口没有），不是上游不可绕过的限制。
A/B 之外新增可选项 C：在 dshgw 渲染的租户 vhost 上做等价最小改写，使租户端口同样可用；
代价是网关侧出现对上游客户端代码的字符串级补丁（上游改字即失效），需按 D5 边界显式决策。
