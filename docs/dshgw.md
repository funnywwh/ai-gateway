---
description: "多租户 dsh 网关：用 aigw API Key 登录，每租户一个 OS 用户、dsh 进程与工作区；独立二进制，与 aigw/dsh 分别升级。"
kind: "spec"
---

# dshgw：多租户 dsh 网关

> 状态：**实现中（M51，已部署并通过双租户主机 baseline 与浏览器登录复验；其余主机验收待完成）**。
> 设计与验收记录：[M51 设计](design/m51-dshgw.md)；安装步骤：[部署手册](../deploy/dshgw/README.md)。

## 1. 组成与升级边界

`dshgw` 让多名同事分别使用自己的 DeepSeek Harness Web 界面：

- **身份与计费**：用各自的 aigw API Key 登录，用量/限速/余额仍由 aigw 计算。
- **租户边界**：每个租户有独立 Linux UID、一个 dsh worker、独立 `$DSH_HOME` 与工作区。不是单进程内的逻辑隔离。
- **独立交付**：`cmd/dshgw` 编译为单独的 Go 二进制；只可导入本仓库 `internal/dshgw/**`，不依赖 aigw 的其它内部包。
- **不修改 dsh 发行包**：通过公开 CLI、HTTP、profile/settings 与 DirectoryPicker 接缝集成。没有 URL 前缀 shim。

运行组件是低权限 `dshgw serve`、`dsh-worker@<tenant>.service` 和宿主 nginx。dsh 直接调用 aigw；模型流量不经过 dshgw。

## 2. 入口与可信边缘

| 用途 | 默认地址 |
|---|---|
| 门户 | `https://chat.tirisen.hk:32600/` |
| 租户应用 | 同一主机名的 `32601–32799`，每租户独占一个端口 |
| 会话网关 | `127.0.0.1:3099`，只供本机 nginx 访问 |
| worker | `127.0.0.1:32100–32299`，每租户独占一个端口 |

端口属于浏览器 origin，因此不需要改写 dsh 前端的 `/api`、插件资源和 WebSocket URL。证书仍按主机名验证，可复用已有证书；防火墙需要单独放行公开端口段，建议限制受信网络。

宿主 nginx 覆写 `Host: $server_name:$server_port` 和 `X-DSHGW-Port: $server_port`。网关核对二者一致，按 registry 分派租户；未知 authority/端口返回 404。`X-Real-IP` 只在直连来源是 loopback 时用于登录限流和审计，不能信任任意 XFF 链。

**Cookie 不按端口隔离。** 浏览器会向同 hostname 的所有 HTTPS 服务发送 `dshgw_s_*` cookie，因此这些服务及其日志系统必须全部可信。端口隔离不是“同主机其它服务可以不可信”的替代品。

## 3. 登录、续期、退出与 Key 重验

1. 在门户表单中提交一个 Key。可接受裸 Key 或 `Bearer ` 前缀；不接受 URL 中的 Key 或重复表单 Key。
2. 网关向 aigw `GET /v1/models` 验证：401 表示 Key 无效/停用；200 且 `data: []` 仍是有效身份。超时/不可达/其它状态不冒充“Key 无效”，登录返回 503。
3. 用 Key 前 12 字节查 **canonical `registry.json`**。没有绑定就拒绝；登录不会自动建立 OS 用户。`keys.map` 是派生的运维索引，不是认证真相。
4. 账号有 **≥2 把可用 Key** 时，先显示选择页（`/login/pick`，M72）再建会话；只有 1 把时直接进入下一步。
5. 门户下发 `dshgw_s_<tenant>`，属性为 `HttpOnly; Secure; SameSite=Lax; Path=/`，然后 302 跳转租户端口。
6. 服务端仅持久化浏览器 token 的 SHA-256，并保存该会话对应的 worker cookie。默认 TTL 为 7 天；浏览器逐响应续期，服务端落盘按 TTL 的 1%（一分钟至一小时）节流，过期判断不依赖浏览器。

同一浏览器可同时登录多个租户。把某租户的有效 token 放进另一个租户的 cookie 名不会获得访问权；重复同名 cookie 被拒绝。

退出是门户同源 **`POST /logout`**：撤销当前浏览器携带的各租户会话，并清除 cookie。GET 不改变状态，返回 405。其它浏览器的会话不受影响。**只退出一个租户**（租户侧栏那一行的「退出」，M67）走各租户 origin 下的 `POST /dshgw/logout/`，见 §7d。

**门户表单策略（已实现）**：门户使用 `Referrer-Policy: same-origin`，保证原生同源POST可携带Origin，跨源不发送Referer。登录/退出仍严格拒绝null、缺失或跨源Origin。403表示请求被拒，不能当成退出成功。更新后需刷新门户再提交表单。 CSP `form-action` 显式允许门户自身和 registry 中的租户 origins，以允许原生表单登录后跨端口重定向；这不放宽服务端的 Origin 校验。

`key_revalidate`：

| 配置 | 运行期行为 |
|---|---|
| `off`（默认） | 不逐请求向 aigw 验证；Key 停用会使模型调用失败，但不会立刻退出 Web 界面 |
| `per-request` | 每次新请求重验租户当前的 `gateway.key`；401 撤销请求所用会话，其它错误返回 503 |
| `interval:<秒>` | 按租户缓存验证结果至指定间隔；不是即时吊销 |

**账号级 dsh 开关（M52-rev2）**：门户登录在 Key 验证后追加调用 aigw `POST /v1/dshgw/authorize`，
200 响应携带该账号的**租户名**——账号下所有 Key（含新建）都登录该租户，无需逐 Key 前缀绑定；
仅当响应无租户名（旧版 aigw）时退回前缀绑定。后台「启用 DSH」按钮经本机 root 守护
（`dshgw admin-serve`，UNIX socket + 对端 UID 白名单，无 TCP）自动完成铸造 worker Key、
创建/启动租户；「停用」停止 worker、吊销 worker Key 并保留数据。

**租户名的自动生成规则（M74）**：`dsh-<账号名拼音>-<账号ID>` —— 陈景峰 / 10 →
`dsh-chenjingfeng-10`，杨妙 / 36 → `dsh-yangmiao-36`，`李智超(colin)` / 8 →
`dsh-lizhichao-colin-8`。规则只有一处实现（aigw 的 `dshTenantNameForAccount`）：控制台弹窗**预填**
服务端下发的 `dsh_tenant_suggested`（仍可手改），飞书首登的自动开通走同一个函数，两条入口因此给出
同一个名字。中文名取拼音表里的**首读音**（多音字 长 → zhang），ID 让同音同名（两个张伟）天然不重；
名字超过 dshgw 的 27 字符上限时**先截断拼音、保留 ID**，且截断落在音节边界。
**既有映射与显式请求名仍然优先**：规则只作用于"没有映射且没人指定"的账号，**不重命名**任何既有租户
（`dsh-tenant`、`dsh-colin` 这类老名字保持原样）。

`dsh_enforce` 决定请求期是否复查：

| `dsh_enforce` | 行为 |
|---|---|
| `login`（默认） | 仅登录时判定；后台停用后新登录 403"该账号未启用 dsh"，既有会话存活至 TTL/退出 |
| `interval:<秒>` | 按租户缓存判定；停用后既有会话至多一个间隔内失效 |
| `per-request` | 每请求判定；停用几乎立即生效 |

`dsh_enforce` 与 `key_revalidate` 正交：前者问"账号可否使用 dsh"，后者验"worker 的模型凭据"。
判定失败（超时/5xx）一律 503 fail-closed，绝不放行。aigw 后台账号列表的"启用/停用 DSH"
按钮控制该开关的真值（`accounts.dsh_enabled`）；停用不删除租户数据，重新启用即恢复。

**所有激活账号默认可用（M72，`dshgw.auto_enable`）**：配置 `dshgw.auto_enable: true` 后，判定改为

```
有效 = accounts.dsh_enabled || (dshgw.auto_enable && accounts.dsh_disabled_at IS NULL)
```

——「从未启用」的激活账号默认可用，管理员不必逐个点「启用 DSH」；被**显式停用**过的账号
（`dsh_disabled_at` 非空，由控制台「停用 DSH」写入）保持停用，配置开关不会撤销管理员的决定。
首次登录时若该账号还没有租户，aigw 在**同一次** `POST /v1/dshgw/authorize` 里按需完成供应
（铸 `dshgw-*` worker Key → 创建/启动租户 → 写 `dsh_tenant` → 审计 `dsh_enable`，actor=`dshgw-auto`），
因此"登录即可用"不需要任何后台点击。供应失败回答 `403 {"allowed":false,"reason":"provision_failed"}`
（fail-closed，下一次登录重试），最常见的原因是该账号在当前网关没有任何可用模型
（`routing.default_grant: none` 的部署）——此时控制台手动「启用 DSH」同样会被拒并给出原因。

**多 Key 账号先选 Key（M72）**：一个账号下所有 Key 都登录同一个租户，所以 aigw 的 authorize 响应
额外带该账号的可用 Key 列表（`keys[]`：**active 且未过期、不含 `dshgw-*` worker Key**）。
门户在**两种登录路径**上都据此分流：

```
Key 登录：POST /login → 验 Key → authorize → keys[] ≥2 → 签 keypick 票据 → 303 /login/pick（不建会话）
飞书登录：aigw 回调签 keypick 票据 → 门户 /login/pick
选择页（GET /login/pick，门户自己的 origin）→ 选一把 → POST /login/pick → 建会话
```

- `keypick` 票据与登录票据同一密钥、同一 codec，但 mode 不同（两侧验证器按 mode 严格分流），
  一次性、默认 120 秒（`feishu.pick_ttl_s`）；
- 选择结果只进审计（`login_key_selected`，记 `key_id`/`key_name`）与本次会话归属：
  **不改**租户 worker 的模型凭据（`PrepareLogin` 仍以空 key 调用），模型额度始终按账号计算；
- 提交的 `key_id` 与门户刚取到的列表比对，不在列表内一律拒绝（表单不可信）；
- 只有 1 把可用 Key 时不出现选择页；0 把时页面提示联系管理员签发。

重验使用**租户当前 worker Key**，不保存登录时提交的旧 alias Key。`--keep-old-prefix` 允许仍有效的旧 Key 登录同一租户；它不使旧 Key 成为 worker 的模型凭据。

**飞书登录（M61）**：配置 `feishu.enabled` 后，门户登录页多一个「飞书登录」；点它会把浏览器送到
aigw 的 `/feishu/login`，由 aigw 完成飞书 OAuth 并**签一张一次性票据**，再送回门户的
`/login/feishu` 兑换成与 Key 登录**完全相同**的会话。dshgw 不持有任何飞书凭据、不登记第二个回调、
不需要出站访问飞书——票据密钥与 aigw 入口 URL 在监督形态下由 aigw 注入生成的配置。
绑定是**账号级**的（M72：`accounts.feishu_open_id`，由管理员在控制台选人写入，不要求扫码；
一个账号有 ≥2 把可用 Key 时先经 `/login/pick` 选一把，见上）。
收票时 dshgw 还会用该租户的 worker Key 调 `POST /v1/dshgw/authorize` 复核账号级授权，
因此控制台「停用 DSH」对飞书登录同样生效，判定失败一律 503（fail-closed）。
未绑定任何账号、账号停用、租户未就绪、票据过期/重放都会在门户给出明确文案。
规格见 [docs/feishu.md](feishu.md) §5。

默认登录限流为每 IP 每分钟 10 次；会话默认上限 10,000，状态文件另有 64 MiB 上限。已建立的 WebSocket 不会被 logout/TTL 追溯关闭，新请求或重连会重新验证。

**租户端口“设置/模型”不可用的实证根因（2026-09-17 复核，取代此前“与网关无关”的判断）**：
DSH 0.1.2-rc.1 客户端把设置持久化绑定在浏览器页面 authority 的回环判定上——
`dsh-client-connection` 的 `isLoopback = transport?.ownsHost === true || !pageLocation ||
isLoopbackHostname(pageLocation.hostname)`，`dsh-client-ui-settings` 据此取
`persistence = isLoopback ? "host" : "memory"`；`memory` 时 `SettingsDescribeMirror.ensure()`
直接返回、视图恒为 `undefined`，模型页即报 "settings are unavailable in this browser"。
判定只看 `location.hostname`，**不看端口**，也**不看** `--trusted-host`（后者仅是 worker 端 /api 的
Host 防护）。真正的差异不在上游，而在入口 nginx 是否改写了该函数：宿主机 nginxWebUI 为既有
`https://chat.tirisen.hk/dsh/` 写了 `sub_filter`，把
`if (hostname === "localhost" || hostname === "[::1]")` 改写成同时接受 `chat.tirisen.hk`
（容器 `/home/nginxWebUI/nginx.conf` 的 `location ^~ /dsh/`），因此 `/dsh/` 下发的 bundle 中
`isLoopbackHostname` 认 `chat.tirisen.hk` 为回环，设置/模型可用；dshgw 生成的租户 vhost
（`internal/dshgw/tenancy/render.go`）是纯透传、无任何 `sub_filter`，故 `:32604` 走原版闸门。
`Accept-Encoding ""` 必须保留，否则上游压缩会让 `sub_filter` 失效。
不受影响：会话对话、模型调用（默认模型由 `sync-models` 写入）、工作区目录选择与 browser-fs
面板（独立插件）。租户侧模型与授权管理在 aigw 控制台完成；若要租户端口同样可用，见
`docs/design/m52-dsh-enable.md` 的 A/B 决策。

### 3b. 设置的归属、每次登录同步与退出即停（M69，规格）

> 状态：**已实现（M69）**。设计：[M69](design/m69-login-lifecycle-and-settings-merge.md)；
> 运维说明与排障：[部署手册 §12b](../deploy/dshgw/README.md)。

**归属按「键」划分，不按文件。** 租户的 `settings.yaml` 与 `.credentials.yaml` 由两方共同写：
平台（dshgw）与租户自己（dsh 的 settings-file + 租户页面的设置面板）。

| 段 | 谁拥有 | 内容 |
|---|---|---|
| 平台段 | dshgw，每次同步重写 | `llm-pi-ai.providers.aigw`（模型清单 = 该租户 worker key 在 aigw 的授权结果）、由它派生的 `agent-default-model` 纠正、`.credentials.yaml` 的 `refs.AIGW_API_KEY` |
| 租户段 | 租户，dshgw 只原样保留 | 其它 provider（含租户自己的 key 与 `baseURL`）、`llm-deepseek`、`ui-theme`、`permission`、`ui-onboarding`、`agent-default-model` 指向非 aigw provider 时的值、`.credentials.yaml` 的其它 refs 与全部 records |

因此：租户手工增删 aigw 段或删掉 `AIGW_API_KEY`，下一次登录同步会被平台段覆盖回授权结果；
租户自己的 provider 与界面偏好不受影响。**平台的模型限制始终来自平台的授权结果，不来自租户文件。**

**不碰宿主机的 settings。** dshgw 只写 `state_dir/tenants/<tenant>/.dsh/**`（以及
`tenant-config/<tenant>/gateway.key`）；操作者自己的 `~/.dsh/settings.yaml` 既不读也不写，
也不会被当作租户模板。

**同步时机**：建户、`dshgw sync-models <tenant>`、worker 启动前，以及**每次登录**
（门户 Key 登录与飞书登录）。登录同步用**该租户存储的 worker key**（不是登录提交的那把 key：
同账号可能有多把 key、授权不同），aigw 不可达或拒绝时只告警、不阻断登录。

**退出即停、退出即卸载（M69 + M76）**：用户点击退出（门户 `POST /logout` 或租户侧栏
`POST /dshgw/logout/`）后按固定顺序做完三件事：①该账号的挂载**不再进入任何 worker profile 并拒绝新 I/O**；
②**强制卸载**它的浏览器本机目录挂载（FUSE）与 SSH 工作区挂载（sshfs）；③**最后强制停掉它的 dsh worker**
（SIGTERM→超时 SIGKILL→scope 回收）并校验进程确实不在跑。审计事件：`logout_mount_detach`（数量）、
`logout_mount_leftover`（没能摘掉的挂载点与原因）、`logout_worker_stop`（校验通过才写）、
`logout_worker_stop_failed`（`reason` 记错误正文，不再是类型名）。
只对**这次退出真正撤销了会话的租户**动手（伪造的 cookie 名不能让别人的 dsh 掉线）。
停 worker **不写** `suspended`——那是运维的停用意图，写入会让 dshgw 重启后不再拉起该租户。
登录时若 worker 没在跑且租户未被运维停用，则先把它的 SSH 工作区挂载按记录**重挂**、再启动 worker
并就绪后才跳转，因此"退出即卸载即停、再登录即起即挂回"。

**强制卸载能到哪一步**：先试优雅卸载（**限时 1s**——go-fuse 的优雅卸载在挂载卸不动时会永远等待自己的
serve loop，实测正是它让退出请求与 reaper 一起卡死），失败或超时（例如挂载被活着的 worker 沙箱或另一个
挂载命名空间持有 → `EBUSY`）就 `fusermount3 -u -z` 惰性摘除，再不行就 abort 这条 FUSE 连接后重试。**保证的是我方挂载表里不再有该条目、
挂载点可复用**；**不保证**别的挂载命名空间（宿主上某个 snap、别的持有者）里那份副本立刻消失——
那份由内核管到那个进程退出（边界声明见 [M76 设计](design/m76-dsh-exit-force-teardown.md) §5）。
一次退出最多等 `logoutStopTimeout`（55s：两段卸载各 ≤15s、停 dsh ≤30s）；门户一次退出多个租户时
整体预算 150s，超出的租户记 `logout_worker_stop_skipped`，由下一次登录或运维处理。

> 为什么不是"最后一个会话退出才停"：浏览器关掉标签页后，它的会话在 TTL 内仍然有效，所以
> "这个租户已经没人了"用会话数根本判不出来——本机实测某租户有 16 个存活会话，多数是几天前的。
> 采用"最后会话"规则等于退出后 dsh 还会跑好几天。代价：同一个人另一个窗口的 dsh 也会被停掉
> （租户=账号=一个人），那个窗口重新登录即可。

## 4. 代理契约

- 只连接 registry 指定的 `127.0.0.1:<workerPort>`，HTTP `Host` 固定为同一 authority。
- 浏览器 `Cookie`、`Origin`、`Sec-Fetch-*`、授权头、代理/转发头和可信边缘头不会进入 worker。仅注入服务端代持的一个 `dsh-auth-*` cookie。
- worker 的 `Set-Cookie`/`Set-Cookie2` 不透传，包括 101 和 HTTP trailers；**nginx 不得全局隐藏网关自己的 `Set-Cookie`**。
- 上游 401 使用会话锁与单调 generation/CAS 刷新握手并重试一次；第二个 401 原样返回。延迟的旧响应不能恢复已清除或覆盖已更新的 worker cookie。
- 握手使用精确的 loopback 启动 URL，禁用自动 redirect；必须得到 303 和一个非空、未过期的 `dsh-auth-*` cookie，不实现泛用 cookie jar。
- unsafe 请求和 WebSocket 必须带精确同源 `Origin`。无 Origin 的浏览器跨站子资源请求被拒绝；门户跳转租户端口的同站顶层导航可通过。无 Fetch Metadata 的普通 GET/HEAD 仍兼容 CLI 客户端。
- 拒绝 absolute-form/authority target、CONNECT、`OPTIONS *`、遍历段、编码遍历、NUL、反斜杠和 token-exchange query。合法 Path/RawPath/query **原样保留**，不使用 `path.Clean` 改写插件路由。
- 不采用路由白名单：插件静态面及任意注册的 WS 路由均可转发。请求体最多 64 MiB，用于一次重握手后的可靠回放；响应与 WS 流不缓冲。

## 5. 管理员命令

**形态（M58）**：aigw 是主程序，dshgw 是它拉起并监督的同目录子进程（同 UID、无 root、无 systemd、
无共享服务账号）；每个租户 worker 是 dshgw 的 bwrap 子进程。因此：

- **租户生命周期必须由运行中的 dshgw 执行**（单独跑一次 CLI 所启动的 worker 会随该进程退出而死），
  日常入口是同 UID 的 admin socket —— 控制台「启用/停用 DSH」走的就是它；
- CLI 保留用于离线只读检查与产物生成。

**路径（M63）**：dshgw 的数据默认全在部署根的 `./data` 之下 —— 独立形态 `state_dir: ./data/dshgw`
（registry、sessions、audit、handshake、tenants、workspaces、template-home、backups 都由它派生），
监督形态默认 `<database.path 所在目录>/dshgw`。相对路径在加载时按进程工作目录（部署根）归一为绝对路径；
`node_bin`/`bin_js`/`current_link`/`bwrap_bin` 与 TLS 证书属**运行时安装**，必须是绝对路径（留空读
`DSHGW_NODE`/`DSHGW_DSH_ROOT`）；`deploy.plugin_path` 没有默认值，`directory_picker: clamp`（默认）
时必须显式给出，否则配置加载失败。完整口径见 [`docs/deployment-layout.md`](deployment-layout.md)。

```bash
# 只读检查与产物（不需要 root）
dshgw --config <state>/config.yaml doctor                    # 部署不变量：私有权限、运行时、bwrap 前置条件
dshgw --config <state>/config.yaml sandbox-exec --print alice # 打印该租户的 bwrap profile（不执行）
dshgw --config <state>/config.yaml capture-url alice          # worker 上报的启动 URL
dshgw --config <state>/config.yaml contract dsh               # 真实 dsh 契约检查
```

`tenant create/start/stop/rotate-key/remove` 等写操作经 admin socket（JSON 行协议，示例见
[部署手册](../deploy/dshgw/README.md) §4）。几点约定：

- **worker 启动前自动同步该租户的模型清单**：401/403（Key 失效或账号被禁）拒绝启动，
  aigw 暂时不可达则告警后用现有清单启动，保证可用性。
- `tenant-stop` 把"停机"写进 registry（`suspended`），aigw 重启后不会自动拉起；`tenant-start` 清除它。
- 未停用的租户在 aigw 启动时自动回来；失败逐个上报，不影响其它租户与 aigw 本身。
- 建户/删除仍会在发布 registry 前做 preflight（模板可用、profile 可构造、目录私有），失败即回滚。

## 6. 模型与运行配置

`aigw_base_url` 是网关根 URL，例如 `http://192.168.190.86:8088`，不要再加 `/v1`。验证访问 `/v1/models`；写入 dsh 的 provider 使用 `api: openai-responses`、`baseURL: <根URL>/v1`，因此模型请求是 **`POST /v1/responses`**。

生成的 provider 同时带 `compat.supportsStrictMode: true`，使普通 Responses 工具显式发送 `strict: false`，防止某些上游把可选参数（如 `sandbox_permissions`）变成必填；不放宽 DSH 沙箱或审批策略。

### 6a 每个模型带什么参数（M68）

渲染出的每个模型条目按 **DSH 官方文档的字段**填写，事实全部来自 aigw 的 `GET /v1/models`
（字段表见 [`docs/api-responses.md`](api-responses.md) 的「能力扩展字段」），dshgw 不自己判断能力：

| dsh 字段 | 来自 | 说明 |
|---|---|---|
| `name` | `name` | aigw 的显示名；没有则回退 id |
| `contextWindow` | `context_window` | 该模型可服务路由里**已申报值的最小值**；未申报则整个键不写，dsh 用适配器默认 262144 |
| `maxTokens` | `max_output_tokens` | 同上（未申报 → 适配器默认 32768）。注意 dsh 的语义：显式写了 `maxTokens` 才成为**每请求默认上限** |
| `input` | `input_modalities` | 声明了图片才写 `[text, image]`；纯文本省略（按文档回落路由 `defaultInput`，默认 `[text]`） |
| `reasoningEfforts` | `capabilities.reasoning` + `reasoning.mode` | 见下 |

`reasoningEfforts` 四态（每种对应一句不同的事实，所以不能合并）：

1. 声明支持思考且模型策略不是 `force` → 全 7 档 `{off: none, minimal: minimal, …, max: max}`；
   `off: none` 让「不选档位」= 显式关思考（M20 实测）。
2. 声明支持思考但模型策略是 `force` → **不写**：网关会覆盖客户端的选择，给出可选菜单等于说谎；
   不写之后 dsh 不发 reasoning 参数，由网关强制，行为仍然正确。
3. 响应里有 `capabilities` 但没有 `reasoning` → `reasoningEfforts: false`（明确的非推理模型）。
4. 响应里没有 `capabilities`（能力未知）→ 不写，dsh 继承（不把「不知道」谎报成「不支持」）。

已知交互：模型级策略用 `mode: default` 时，一旦声明了档位表，dsh 每次请求都会带显式 effort
（未选档位时按 `off: none`），网关的 `default` 策略因此不会生效；要强制请用 `mode: force`。

有任一模型带图片时，路由级写 `maxRequestImageBytes`（配置键 `image_request_max_bytes`，默认 7 MiB）：
dsh 默认 20 MiB 会超过 aigw 的 `server.max_body_bytes`（默认 10 MiB），而网关对请求体是**有界读取**，
超限时截断成 JSON 解析错误而不是回一句「太大」。设了这个界，dsh 会把最旧的图片换成占位符，请求继续能成。

**权威入口在网关侧**：`provider_models` 的 `capabilities` / `context_window` / `max_output_tokens`
（控制台「供应商 → 模型映射」，或 MCP `admin_upsert_provider_model`）。`SyncModels` 每次整段重写
`llm-pi-ai.providers.aigw`，所以**在租户 `settings.yaml` 里手改这一段会在下次同步丢失**。

刷新时机：worker 启动前自动同步、`dshgw sync-models TENANT`、门户登录/换 key；dsh 的 `settings-file`
有文件监听，改写在下一次请求生效，无需重启。想更准就给 provider model 补声明——网关把 `0`
一律当「未申报」，不会拿它去覆盖别的路由的真实值。

同一套规则也可以手写进个人 `~/.dsh/settings.yaml`（那不是 dshgw 租户，链路不同）：

```yaml
llm-pi-ai:
  providers:
    aigw:
      apiKeyEnv: AIGW_API_KEY
      api: openai-responses
      baseURL: http://192.168.190.86:8088/v1
      maxRequestImageBytes: 7340032
      compat: {supportsStrictMode: true}
      models:
        - id: deepseek-flash
          name: deepseek-flash
          contextWindow: 1000000
          maxTokens: 65536
          input: [text, image]
          reasoningEfforts:
            off: none
            minimal: minimal
            low: low
            medium: medium
            high: high
            xhigh: xhigh
            max: max
        - id: u2-flash
          name: u2-flash（unisound）
          reasoningEfforts: false
```

（`llm-deepseek` 那类直接适配器用 `inputModalities`/`imagePixelBudget`/`imageMaxBytes`；pi-ai 路由用上面的
`input`，两套字段不通用。）

飞书登录在**子进程配置**里需要两项（监督形态由 aigw 写入，独立形态手填，见
`deploy/dshgw/config.example.yaml`）：

```yaml
feishu:
  enabled: true
  aigw_login_url: http://192.168.190.86:8090/feishu/login   # 浏览器可见的 aigw 入口
  ticket_secret: "…"                                        # 与 aigw 的同一项一致
```

明文 HTTP 部署必须同时设 `public_scheme: http`：否则 dshgw 会发 `Secure` cookie，浏览器直接丢弃，
表现成「登录成功又被弹回门户」（`doctor` 会复核这一条）。

多租户上线前必须将 aigw **`auth.default_grant: none`**，再显式授予模型。M51 不会替部署方静默修改 aigw 的授权配置。没有实现 `dshgw usage`，也不持有 aigw 管理/MCP 凭据。

## 7. 隔离、工作区与 browser-fs

形态是**单一模式**：所有租户 worker 与 aigw 同 UID，隔离来自 bubblewrap mount namespace。

| 层面 | 边界 |
|---|---|
| 文件系统视图 | 空 tmpfs 根 + 逐路径绑定：租户只能看到自己的 workspace/`.dsh`、只读运行时（`/usr`、`/bin`、node、dsh release）、目录选择器插件，以及少量 `/etc` 白名单文件 |
| 宿主树 | `/home`、`/root`、`/tmp`、`/var`、`/srv`、`/etc/dshgw` 为空；其他租户目录**不存在** |
| 敏感配置 | per-tenant 的 `gateway.key`/`tenant-config` **不挂载**；registry/sessions 不可见 |
| 可写性 | 仅 workspace 与 `.dsh` 父目录（可建子目录）；`/usr`、`/etc/passwd` 只读 |
| namespace | `--unshare-pid --die-with-parent`；共享网络；宿主 `apparmor_restrict_unprivileged_userns=1` 时租户不能嵌套 namespace |
| 纵深防御 | 租户叶 `0700`（`doctor` 逐租户复核）+ dsh 内层 sandbox（本宿主 AppArmor 拒绝嵌套 bwrap，dsh 回退 Landlock） |

**单域名路径模式（可选）**：配置 `public_base_url` 后，租户的 URL 变成
`https://<域名>/t/<租户>/`，会话 cookie 的 Path 随之收窄到该租户路径 —— 多个租户共享一个 origin 时，
路径是区分两个租户会话的唯一依据（否则浏览器会把 A 的 cookie 发给 B 的路径）。Origin 栅栏以基础 origin
为准，异源仍被拒。此模式不需要子域名，配合 `bin/gwproxy` 使用（见部署手册 §9）。

**诚实结论**：没有 UID 边界 —— 租户数据属主就是运行 aigw 的账号，任何以该账号运行的进程都能读全部租户的
凭据；宿主账号的爆炸半径就是边界失守时的地板。需要"连运行时账号都读不到"的场景应改为分账号/分主机部署。
完整口径见[部署手册 §7](../deploy/dshgw/README.md)与设计文档 `docs/design/m58-aigw-supervised-dshgw.md`。

预注册工作区默认是 `<state_dir>/workspaces/<tenant>/work`。目录选择器浏览的是**服务器**的文件系统，
不是浏览器本机；clamp 只是防误操作，真正边界是 mount namespace。

**已消失的目录不再让选择器整体失败**：浏览器/SSH 挂载在属主断开时会被网关拆除，挂载点目录也随
之删除，而 dsh 侧可能仍记得该路径（工作区登记、对话框上次的位置）。此时 `list` 回退到**根之内最近
仍存在的祖先目录**（根永远存在）并如实返回该目录的 `path`/`crumbs`，而不是抛
`directory-picker/unreadable`；`createDirectory` 保持严格（父目录不存在仍是调用方的错误），
根外路径照旧拒绝，clamp 边界不变。

`dsh-browser-fs@0.2.0` 由模板固定版本与 integrity；它只访问用户在浏览器明确授权的**本机**目录，
不改变服务器工作区、agent cwd 或 bash 执行位置。**已授权文件内容可能进入模型请求**，必须向使用者说明；
可按部署（或租户）`--browser-fs off` 关闭，此时模板无需 browser-fs。

## 7b. SSH 工作区（M64）

租户在自己的 dsh 里点「SSH 工作区」：在「我的主机」里管理本账号自己的主机（添加即写入本账号
`~/.ssh/config` 的一条别名，删除即移除），或直接手输 `user@host`（支持独立 SSH 用户名与端口输入）
→ 浏览远端目录 → 新建远端目录 → 挂载并打开。挂载点是
`<workspace>/<mount_subdir>/<host>/<远端路径>`（默认 `ssh`），因此它落在该账号的 clamp 根之内，
可以直接作为一个工作区打开；因为沙箱只绑定本账号的这两棵树，**别的账号看不到也进不去**。

**自嵌套挂载会被拒绝（2026-09-21 事故）**：当远端目录是挂载点自己的祖先时，挂载出来的树里包含挂载点
本身，也就是**挂载包含它自己**。挂载那一刻没有异常，坏在第一个递归读者上：`grep -r`/`find`/工作区索引
会走进挂载里的那份拷贝，在里面又遇到同一个目录，再往下走，每一步都是这条挂载的一次 sftp 往返；请求在
一条连接上堆积，直到整条挂载在内核里挂死。实测（本机）：一个会话在工作区根上跑 `grep -rn … .` 之后，
FUSE 连接上积压 8 个无人应答的请求，3 个进程进入 **D 态**（不可中断等待）—— `grep` 自己（它打开的目录
句柄已经指向第二层同一个目录）以及之后每一次探测挂载点的 `ls`；D 态忽略信号，`kill -9` 也无效，而一个
账号的**所有会话共用这一条挂载**，所以它们同时卡住。
因此网关在挂载前判定并拒绝：**只有当 SSH 目标就是本机**（别名解析出的地址属于本机，或远端 `machine-id`
与本机相同）**且远端路径是挂载点的祖先**时才拒绝。别的机器上同样的路径字符串是另一个目录，属于正常
用法，不受影响；本机上不包含工作区的目录（例如 `/home/winger/ZT20Q`、`/tmp`）也照常可挂。判定按
**设备号 + inode** 比较而不是字符串前缀 —— 本机 `/data/home/winger/work` 与 `/home/winger/work` 是同
一个 ext4 的两次挂载，字符串比较会漏掉它。错误码 `mount/forbidden`，审计事件 `ssh-mount-refused`。

**分工（为什么不是一个纯插件）**：ssh 那一半在租户沙箱内、用**该租户自己的密钥**完成（列目录、
建目录、探测）；挂载那一半由 dshgw 在沙箱外完成。租户 worker 挂不了：profile 只给最小 `/dev`
（没有 `/dev/fuse`），且宿主 root 在它的 user namespace 里没有映射，`fusermount3` 的 setuid 因此
失效 —— 实测连 `tmpfs`/`proc` 的 `mount(2)` 都是 `EPERM`，加 `CAP_SYS_ADMIN` 也一样
（见 `docs/design/m64-ssh-workspace.md` §3）。代价是**挂载后要重启该账号的 worker**：bubblewrap 的
`--bind` 不携带子挂载，profile 在启动时为每个活动挂载点追加一次 `--bind-try`，所以挂载/卸载都会让该
账号的 dsh 重载（进行中的回合会中断，会话日志可 resume）。

**退出卸载、登录重挂（M76）**：用户点「退出」时，该账号的 sshfs 挂载被**强制卸载**（先优雅卸载，
EBUSY 就惰性 `-u -z`，仍不行就杀掉 sshfs 守护进程 / abort 这条 FUSE 连接后重试），但**挂载记录、
镜像文件与挂载点目录都保留**：远端目录仍留在该账号的「SSH 工作区」列表里，工作区条目与会话归属不变。
**下次登录时网关按记录自动重挂**（`Restore`，在启动 worker 之前），因此"退出卸载、再登录挂回"。
若某次重挂失败，登录仍然成功（fail-soft，审计 `login_mount_restore_failed`）；若登录时该账号 worker
已经在跑，网关**不会为挂载重启它**（不打断进行中的回合），该挂载在下次 worker 启动时进入沙箱，
用户也可以在「SSH 工作区」里再点一次「连接」（那条路本来就会重启 worker）。

| 面 | 是什么 |
|---|---|
| 通道 | 该账号 DSH home 里的文件信箱：`<dsh_home>/ssh-requests/<id>.json`（租户写）、`<dsh_home>/ssh-replies/<id>.json`（网关写）。不新增监听端口、不新增令牌 |
| 挂载记录 | `<state_dir>/ssh-mounts.json`（0600）；同时镜像一份到 `<dsh_home>/ssh-mounts.json`，租户插件靠它区分「真挂载」与镜像布局产生的父目录 |
| 别名清单 | **一账号一份**：`<workspace>/.ssh/config`（0644），初始内容来自 `ssh_config_dir/<账号>` 这份种子；账号自己没有 config 时才写一次，之后归该账号所有（插件增删、也可手改）。**没有全局来源** —— 宿主的 `~/.ssh/config` 不是租户来源（配置校验也会拒绝落在本进程账号 `~/.ssh` 里的 `ssh_config_dir`） |
| 密钥 | 账号默认：`<workspace>/.ssh/id_rsa`；主机专用：`<workspace>/.ssh/host_keys/<SHA256(host)>/id_rsa`（均 0600）。只选主机专用，否则选账号默认；没有密钥则拒绝连接。`known_hosts` 同目录 |
| 隔离增量 | 只为活动挂载点各加一条 `--bind-try <挂载点> <挂载点>`；**不加设备、不加 capability**，M57/M58 口径不变 |
| sshfs 选项 | 默认 `reconnect, ServerAliveInterval=15, ServerAliveCountMax=3, idmap=user, max_conns=4`；配置写了 `max_conns` 则以配置为准；`allow_other`/`allow_root` 被代码丢弃（所有 worker 共用一个 uid，共享挂载等于跨账号可读）。`max_conns` 不是 sshfs 的默认值（它默认 1 条连接）：一条挂载服务该账号的**所有**会话，单通道会让一次慢遍历把其它会话的读全部排在后面 —— 外部看到的就是「多个会话一起卡」 |

**密钥就是边界**：账号能读到自己的 `id_rsa`（跑 key 的进程就是它自己），所以「这个账号能到哪些主机」
完全由发给它的密钥决定。因此**一账号一把**是唯一的形状：来源只有运维预置的 `identity_dir/<账号>`
和账号自己上传两条路。**没有共享密钥来源** —— 早期版本有一个 `identity_source`（一把密钥灌给所有
账号），本机曾把它指向部署账号自己的 `~/.ssh/id_rsa`，结果是 8 个租户各拿到一份运维私钥的副本，
而该密钥就在本机 `authorized_keys` 里：租户可以从沙箱内 `ssh` 回宿主，直接变成部署账号（2026-09-22
事故，处置记录见本节末）。该键已删除，配置里出现会直接拒绝启动；`identity_dir` 也不允许落在本进程
账号的 `~/.ssh` 里。`hosts` 白名单只是防跑偏，不是安全边界。

### 别名（`.ssh/config`）与「我的主机」

每个账号有一份**自己的** `~/.ssh/config`，网关挂载与沙箱内的插件都只读这一份，因此账号之间不会互相
影响，也不会碰到宿主（部署账号）的 ssh 配置：

- **初始种子**：`ssh_config_dir/<账号>`（0644，非符号链接、非硬链接、非 world-writable）。账号还没有
  config 时，网关把它剪成只含 `Host`/`HostName`/`User`/`Port` 的四行块写进工作区；账号已有 config 则
  **一律不动**（那里面可能有账号自己加的主机与指令）。种子缺失不是错误：账号从「没有别名」开始，可以
  手输 `user@host` 或自己添加。
- **「我的主机」**：对话框里列出该账号的全部别名（名称、`[user@]host:port`、私钥状态、是否正被挂载），
  可以添加与删除。**添加成功后别名立即写进本账号的 config**（同名不覆盖：先删除再加）；**删除即从
  config 移除该块并删掉这台主机的专用私钥**。带私钥添加时，私钥存在
  `host_keys/<SHA256(别名)>/id_rsa`，与该别名被选中时网关查找的路径一致。
- 只改写 `Host`/`HostName`/`User`/`Port`：块内其他指令（例如某个老服务器需要的
  `HostKeyAlgorithms +ssh-rsa`）与文件里其它内容原样保留；写入是「临时文件 + rename」，不会留下半份。
- 正在被挂载使用的别名不能删除（挂载记录用的是别名，删了网关下次重挂就解析不到）：先卸载再删。
- `hosts` 白名单非空时，添加要求「别名」与「解析出的连接标识」都在白名单里。
- **重置某个账号的别名**：改 `<ssh_config_dir>/<账号>` → 删 `<workspace>/.ssh/config` →
  `dshgw tenant restart <账号>`（下一次 worker 启动时按种子重新生成）。
- **改「主机/用户名/端口」时界面不动**：身份状态查询是 200ms 防抖（一次输入只发一次请求），
  并且不会清空已经列出的远端目录、已填的远端目录路径或已选的私钥文件；目录列表会标注它来自
  哪台主机（换主机后点「浏览」刷新即可）。对话框顶端对齐并预留滚动条位置，内容增减不会让它
  重新居中。
- 本机从旧版本迁移（旧的单一 `ssh_config_source`）用 `scripts/ssh_config_adopt.sh`：把每个账号现有的
  `<workspace>/.ssh/config` 收编为它的种子，不覆盖已有种子，再按需裁剪。

### 端口与私钥管理

- 主机地址可填别名、域名、IPv4 或 `user@host:2222`；独立端口输入接受 `1..65535`。留空则沿用别名的 `Port`，否则默认 SSH 端口 22。暂不支持 IPv6 字面量。
- 在「私钥管理」选择账号默认或当前主机专用，再选择本地私钥文件上传；支持 OpenSSH/RSA/Ed25519 等 `ssh-keygen` 可读取的**无密码私钥**，上限 64 KiB。加密私钥不支持，不要上传服务器 `/etc/ssh/ssh_host_*_key`。
- 界面只显示配置状态及 SHA256 公钥指纹，不回显私钥；再次上传替换所选范围的密钥，删除需确认。私钥以 0600 明文保存在本账号工作区（目录 0700），生产环境必须使用 HTTPS，备份应按敏感数据保护。
- 主机专用密钥绑定完整连接标识（包含显式用户名、端口）；别名和实际地址是不同绑定（「我的主机」添加的条目绑在**别名**上，因为选中该条目时请求里带的就是别名）。不同账号即使填写同一主机也不共享上传文件。
- 删除主机密钥后回退账号默认；删除默认后不会在重启时从运维源重新复制（`identity-managed` 标记）。替换/删除不会撤销已经建立的 SSHFS 连接：要立即切换，请先卸载再重新打开，远端撤销授权需移除其 `authorized_keys` 中对应公钥。
- 密钥来源只有两个：运维预置 `identity_dir/<账号>`（只在账号没有密钥且没有 `identity-managed` 标记时复制一次）、账号自己在界面上传。**共享密钥能力已删除**（旧键 `identity_source`，写了会直接拒绝启动）；两处都留空是合法配置，此时账号从「没有身份」开始，插件会提示它上传。运行连接不会回退到运维的共享源或 SSH agent。SSH 配置仅使用具体别名的 `HostName` / `User` / `Port`，不执行 `ProxyCommand`，不加载其中的额外 `IdentityFile`（两侧 ssh 都带 `-F /dev/null`）。
- **共享密钥事故的处置**（2026-09-22，本机 `dshgw-verify`）：`scripts/dshgw_ssh_identity.sh purge-shared` 扫过每个账号私钥可能存在的两处 —— `<workspace>/.ssh/id_rsa`（账号默认）与 `<workspace>/.ssh/host_keys/<SHA256(host)>/id_rsa`（主机专用）—— 按**公钥指纹**（不是逐字节）匹配被撤销的密钥，删除命中的副本，并在删掉账号默认密钥时写下 `identity-managed`（默认只打印计划，`--apply` 才动手；`ssh-mounts.json` 还有挂载时拒绝执行）。按指纹匹配是必需的：本机有一份主机专用副本与原文件仅差一个结尾换行，摘要不同、密钥相同。随后 `scripts/rotate_operator_ssh_key.sh` 轮换被泄露的宿主密钥（生成新密钥 → 逐主机先加新公钥并验证、再从远端 `authorized_keys` 移除旧公钥 → 本机同样处理 → 就地替换密钥文件），最后 `scripts/dshgw_ssh_identity.sh provision --tenant <账号> --key <私钥>` 为每个账号装上自己的密钥（该脚本拒绝安装与被撤销密钥相同的密钥），并把同一份字节写进该账号自己的 `<workspace>/.ssh/id_rsa`（不重启 worker，因此不打断正在进行的会话）。远端 `authorized_keys` 必须自己加：账号拿到的密钥在远端被授权之前，它到不了那台主机。别名种子可用 `trim-seeds` 收口：只保留账号自己添加过的别名（种子与活动 config 同时改写并各留快照）。
- 若运维配置了 `hosts` 白名单，需包含完整连接标识（例如 `ubuntu@server:2222`），不能靠改端口绕过。

**前置**：宿主装 `sshfs`，租户环境提供 `ssh` 和 `ssh-keygen`；目标主机已授权所上传私钥对应的公钥。
启用时若 `sshfs` 不可执行，配置加载即失败（与 `deploy.plugin_path` 同一原则：不静默降级）。
`sshfs` 上的 `git status`/`grep` 比本地慢，inotify 不生效 —— 远端构建/测试请让会话显式 `ssh` 过去跑。

## 7c. 浏览器本机目录 FUSE 工作区（可选，默认关闭）

与上面的 `dsh-browser-fs` 额外工具不同，本功能把浏览器授权目录挂载为服务器上的真实工作区：
**命令仍在服务器运行，文件读写通过浏览器落到用户本机**，无需用户安装本地代理。

- 独立 dshgw 设置 `browser_workspaces.enabled: true`；aigw 监督形态设置
  `dshgw.browser_workspaces.enabled: true`。开启必须配置 `deploy.plugin_path`（监督形态为
  `dshgw.plugin_path`），其所在目录须包含 `browser-workspace/index.js` 及随附客户端文件。
- 网关主机需 Linux、可用 `/dev/fuse`、`fusermount3` 与用户态挂载权限；浏览器需支持 File System
  Access API 的 Chromium，并通过 HTTPS 或受信任 localhost 使用。租户沙箱不增加设备或 capability。
- 侧栏「浏览器工作区」一行：**行体**是一次自适应点击（没有目录时直接弹系统目录选择器；恰好一个目录
  时是它的连接/断开；多个目录时打开文件夹列表），**行体右侧的文件夹图标**打开文件夹列表，可**添加**、
  **连接**、**断开**、**打开**与**删除**（两步确认）目录。一个账号可同时挂载多个本机目录（每租户上限 4
  个，浏览器侧最多保存 8 个），连接与断开都会重启该账号 worker。
- 挂载位于 `<workspace>/browser/<目录的稳定key>`，key 在保存该目录时生成一次并保存在浏览器里：同一个
  本机目录永远映射到同一个路径，因此 DSH 的工作区条目（按路径复用）保持同一个 id 与会话归属，重连不会
  产生重复或失效的工作区行。**断开**只卸载、保留空挂载点与该工作区条目；**删除**才释放挂载点并删除工作区
  注册（会话记录保留）。
- 现有已鉴权租户入口上的同源 HTTP 长轮询传递文件请求，不新增监听端口。目录内容可进入模型请求，读写授权应谨慎授予。
- 挂载后重启该租户 worker 以加入显式 bind，**会中断正在运行的回合**。授权页面必须保持连接；
  关闭页面、撤销权限、断网会使 I/O 失败。已保存的目录句柄与授权跨刷新、跨标签页有效：刷新后行显示
  「可恢复」，一次点击即可接回同一个挂载（只发 `resume`，不新建挂载、不重启 worker）。不承诺完整 POSIX 语义。
  页面侧的长轮询连接断开（刷新、关闭标签页、崩溃、客户端在 close 前主动 abort）即视为断开该挂载，
  不再等满 60 秒租约；并且只有浏览器仍在应答的挂载才会进入 worker 的 bind 列表——一个没有浏览器的
  挂载路径会让整个 worker 启动卡满 FUSE 超时后失败（表现为另一个挂载的"清理未确认"）。
- 开启功能时 `browser` 是网关管理的保留挂载子目录（启动前建立为真实私有目录），在租户沙箱中始终只读绑定，
  即使尚无活动挂载也不能在其中建目录、替换容器或挂载点；各活动 FUSE 子挂载另外显式读写绑定，内部文件读写不受影响。
  租户及全网关备份排除该子树；勿将仅存于服务器的重要文件放入其中。
  关闭功能且没有活动挂载时，同名普通目录仍正常备份；残存活动挂载始终排除。
  租户停用或删除会清理挂载，普通 worker Restart 不清理。离线 CLI 无权接管活动挂载，
  检测到挂载时拒绝破坏性操作，应通过运行中的网关处理。

运行 `make dshgw-browser-test` 检查 Go 集成与 Node fake-FSA 契约；真机验收是
`make dshgw-browser-e2e`（单目录挂载）、`make dshgw-browser-reload-e2e`（断线/刷新/换标签页恢复）与
`make dshgw-browser-multi-e2e`（多目录并存、逐目录断开/重连/删除）。完整架构、限制与实测证据见
[浏览器 FUSE 工作区设计](design/browser-fuse-workspace.md) 与
[实现与验证记录](browser-workspace-verification.md)。

## 7d. 侧栏账号行与退出（M67）

租户 dsh 的侧栏底部多一行：左边是登录者的**飞书名**（该账号任一 Key 上的飞书绑定；没有就回退
**账号名**，再回退**租户名**），右边是**「退出」**按钮。开关是 aigw 的 `dshgw.account_card.enabled`
（独立部署则在子进程配置里写 `account_card.enabled`），默认关闭；关闭时 `/dshgw/**` 在租户 origin 下
一律 404，那一行也就不出现 —— 与一个没有网关的普通 dsh 表现一致。

| 面 | 是什么 |
|---|---|
| `GET /dshgw/session/` | 该租户 origin 下，`{"ok":true,"value":{"authenticated":true,"tenant":…,"account":…,"feishu_name":…,"name":…}}`；`name` 是上面那条回退链的结果。需要该租户自己的会话 cookie |
| `POST /dshgw/logout/` | 撤销**本租户**的会话、清 cookie，`303` 到门户登录页。必须带本租户 origin（与其它写请求同一道栅栏）；`GET` 返回 405，不改变状态 |
| 鉴权 | 与 worker 请求**同一条链**：唯一会话 cookie → 会话归属该租户 → `key_revalidate` → dsh 授权复核。两个端点都在 worker 握手之前处理，所以 worker 没起来也能问「我是谁」 |
| 名字来源 | aigw `POST /v1/dshgw/authorize` 的 200 响应新增 `account` / `feishu_name`（纯新增字段）。dshgw 按租户缓存 5 分钟，并在登录成功时预热；取不到只降级成租户名，**绝不因此拒绝请求或登录** |
| 账号名落库 | 控制台创建租户 / 轮换密钥时把 `accounts.name` 随 `tenant-create`/`tenant-set-key` 写入 dshgw 注册表的 `account` 字段（旧租户下次启用或轮换时补上；补不上就显示租户名） |
| 隔离 | 退出只撤销当前租户的会话，同一浏览器里其它租户保持登录；这一行不引入新的监听端口、令牌或凭据，只是给已有会话多加两条读/写路径 |

**点「退出」后要等多久（M76）**：网关在这个 POST 里**同步**做完"强制卸载该账号的挂载 → 最后强制停掉
它的 dsh"才回答 303，所以按钮上的「退出中…」覆盖整个过程。正常情况在 1 秒内返回；最坏 55s（两段卸载
各 ≤15s、停 dsh ≤30s），失败的部分记审计、下一次登录或运维接手，不会让退出本身失败（会话已经撤销）。
这条路由也是**唯一**允许"worker 还活着就先卸载"的路径（顺序与取舍见
[M76 设计](design/m76-dsh-exit-force-teardown.md) §3 D1/D2）。

**为什么退出不直接跳门户的 `POST /logout`**：门户那条路由要求 `Origin` 精确等于门户 origin，而租户
页面只能发出自己租户的 origin（端口模式下两者端口不同），请求会被 403。因此退出由网关在租户 origin 下
执行同一份会话存储的删除；门户只作为落地页。

## 7e. 宿主目录工作区（M71，无 FUSE）

要挂**宿主机自己的目录**时，不要走 SSH 工作区（§7b）：本机目录不需要 ssh，而且 sshfs 恰好在本机这个
场景最危险 —— 账号的工作区就在被挂目录里面，于是「挂载树包含挂载点自身」，任何递归读者都会一路走进
自己的拷贝，把整条挂载的请求堆死（2026-09-21 真机事故：FUSE 连接积压 8 个请求、3 个进程进 D 态、
该账号全部会话同时卡死；网关现在会直接拒绝这种自嵌套挂载）。

宿主目录工作区改用 bubblewrap 直接 bind，**没有 ssh、没有 sshfs、没有 FUSE**：沙箱里
`<workspace>/<host_shares.subdir>/<name>`（默认 `host`）就是一个普通目录 —— 本地读、inotify 有效、
不会有不可中断等待。目录由**运维在配置里声明**，租户不能自助添加（SSH 那半有 open/close 信箱请求，
这里刻意没有：宿主目录不是租户能选的东西）。

| 面 | 是什么 |
|---|---|
| 配置 | `host_shares: {enabled, subdir, shares: [{name, path, read_only, tenants}]}`；默认关闭，`subdir` 默认 `host` |
| 绑定 | 容器先 `--ro-bind <workspace>/<subdir>`（只读，租户不能替换它）；每份共享一条 `--ro-bind-try`（只读）或 `--bind-try`（可写）`<宿主目录> <目标>`。没有内核挂载，也没有卸载动作 |
| 镜像 | `<dsh_home>/host-shares.json`（0600）：`{version, tenant, subdir, shares:[{name, target, read_only}]}`。**不含宿主路径** —— 名字到宿主目录的映射是运维的配置，不必出现在租户屏幕上 |
| 生效时机 | worker 启动时绑定，profile 渲染前由网关建好容器与目标（0700）；改配置后重启该账号 worker 生效 |
| 权限 | **默认只读**：写授权必须显式写 `read_only: false`。可写共享直接写穿到宿主目录（不是拷贝） |

**规则与拒绝**（配置加载即校验，见 `internal/dshgw/config`）：

- `tenants` 必须非空：没有「所有人」这种默认，一份宿主目录的授权要写明给谁。
- 共享目录与 `state_dir` **必须不相交**（符号链接解析后再比）：`state_dir` 里有每个账号的工作区、`.dsh`、
  ssh 私钥与会话记录，一份包含它的共享等于把一个账号的数据交给另一个账号。因此
  `/home/winger/work/ai_gateway`（本机部署根）会被拒 —— 请声明它下面具体那个子目录。
- `subdir` 不能与 `ssh_workspaces.mount_subdir`、`workspace_seed` 撞名，必须是可见的单段目录名。
- 宿主目录被删掉：该份共享在 worker 启动时被跳过并记一行日志，不让账号起不来。
- 目标路径一律在 `<workspace>/<subdir>` 之内（`Profile` 会再校验一次，越界直接拒绝渲染）。

**打开方式**：账号自己的目录选择器（clamp 在 workspace 内）进入 `<workspace>/<subdir>/<name>` 即可作为
工作区打开；侧栏面板行尚未做，`host-shares.json` 就是给它的数据源。仍然**别对工作区根跑递归
`grep -r`/`find`**：合法共享不会死锁，但会把同一棵树读两遍（共享里包含工作区时更明显），很慢。

验证：`go test ./internal/dshgw/sandbox -run HostShare` —— 其中 staging 用**真 bwrap**跑出四条断言：
只读共享可读不可写、可写共享写穿到宿主目录、宿主路径本身在沙箱内不可见、容器条目只列出声明的共享。

## 7f. 租户侧 web 插件（M75，默认开启）

每个账号的 dsh 默认就带三块面板，都由网关渲染进该租户的 profile，插件文件放在 `deploy.plugin_path`
同级的三个目录里（该目录已被只读绑进租户沙箱，因此不需要任何新的绑定、设备或 capability）：

| 面板 | 插件目录 | 做什么 | 边界 |
|---|---|---|---|
| 「终端」（侧栏底） | `web-tty/` | 浮动终端，每个标签一个真 PTY（`node-pty` 从 dsh 发行版解析），xterm.js 渲染；面板可拖动/缩放/最大化，`Ctrl+反引号` 开关 | PTY 起在该账号**自己的 bwrap 沙箱**里，能力等同于它的 bash 工具；`cwd`/`cwdRoot` 都钉在它的 workspace |
| 「文件」（侧栏底） | `workspace-files/` | 浏览/预览/编辑/上传/下载/改名/删除 | 一切路径夹紧在 `root`（该账号 workspace）内，符号链接越界即拒 |
| 「变更」（会话主区 View） | `git-diff/` | 左列改动文件、右侧两栏 diff | 只读：没有 stage/checkout/discard，git 一律 `--no-optional-locks`，`.git/index` 字节不变 |

**开关**（三个都默认 `true`；关掉即从每个租户的 profile 移除该行，既有租户在下次 worker 启动时生效）：

```yaml
tenant_plugins:            # 独立形态：dshgw.yaml；监督形态：aigw config.yaml 的 dshgw.tenant_plugins
  web_tty:         { enabled: true }
  workspace_files: { enabled: true }
  git_diff:        { enabled: true }
  root_label: 工作区        # 两个工作区面板对 root 的显示名；留空即此默认值
```

**前置**：三个插件目录要在 `plugin_path` 同级（仓库里的 `./cmd/dshgw/plugin/` 已是这个形状；生产部署见
[部署手册](../deploy/dshgw/README.md)）。`dshgw doctor` 会逐个体检（`web-tty-plugin`、`workspace-files-plugin`、
`git-diff-plugin`）。开着但没部署时：**建户/轮换密钥直接失败**并给出缺失路径，既有租户启动只丢掉那一行
并写一条 warning —— 一个行指向不存在的模块会让整棵插件树加载失败，所以宁可少一行，不可给一行坏行。
`directory_picker: browse` 且没有 `plugin_path` 的老配置因此在加载期被拒：要么命名一个插件目录，要么把
这三项显式关掉（**升级注意**）。

**运行期状态按账号隔离**，全部落在该账号自己的 DSH home 下（插件目录是共享的，生产中可能 root 拥有、
不可写）：

```
<DshHome>/plugin-state/web-tty.trace.jsonl
<DshHome>/plugin-state/workspace-files.trace.jsonl
<DshHome>/plugin-state/git-diff.trace.jsonl
<DshHome>/plugin-state/git-diff.cache.json      # 扫描缓存（含仓库路径）—— 绝不跨账号共享
```

目录由插件在首次写入时创建（它以租户账号身份运行，只有它能在自己 home 下建出属主正确的目录）。
关掉一个插件只移除行，插件文件留在原地。三块面板都走 `ctx.connection.rpc`（租户 origin 下、既有鉴权链），
不新增端口、令牌或凭据。细节、决策依据与失败模式见
[docs/design/m75-tenant-plugins.md](design/m75-tenant-plugins.md)。

## 8. 运维与验收

```bash
make dshgw-test            # Go 测试（含 sandbox 单测与监督器测试）
make dshgw-sandbox-test    # 真实 bwrap：宿主隐藏、workspace 可写、真实 dsh web 在沙箱内启动并 401
make dshgw-verify          # 上述 + 构建 + vet + 真实 dsh 契约 + 模板准备
```

本机实测过的完整链路（无 root、无 systemd、无 per-tenant 账号）：aigw 启动 → 生成子进程配置 →
拉起同目录 dshgw → 收到 ready → 经 admin socket 建租户 → worker 以 bwrap 子进程起来 →
`GET /api` 返回 401；`tenant-stop`/`tenant-start` 生效（后者启动前会同步模型）；aigw 重启后
未停用租户自动回来；停止 aigw 后子进程与 worker 零残留。

监督形态验收（`make dshgw-supervised-test`）还覆盖飞书链路本身：真实 aigw 与 dshgw、飞书 stub 授权页，
从门户点「飞书登录」直到进入租户 UI（200），并验证复核被调用、解绑后被拒、上游吊销后被拒、恢复后成功。

这些自动化仍**不能**替代：公网 TLS/防火墙（当前形态没有 nginx，租户门户由 dshgw 明文直接监听）、
资源压测、真实浏览器授权动作。旧的 root 宿主验收脚本（基于 UID/cgroup/systemd 断言）已随旧形态删除，
新的宿主验收需要按本形态重写（见 `docs/TODO.md`）。
