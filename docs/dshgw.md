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
4. 门户下发 `dshgw_s_<tenant>`，属性为 `HttpOnly; Secure; SameSite=Lax; Path=/`，然后 302 跳转租户端口。
5. 服务端仅持久化浏览器 token 的 SHA-256，并保存该会话对应的 worker cookie。默认 TTL 为 7 天；浏览器逐响应续期，服务端落盘按 TTL 的 1%（一分钟至一小时）节流，过期判断不依赖浏览器。

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
`dsh_enforce` 决定请求期是否复查：

| `dsh_enforce` | 行为 |
|---|---|
| `login`（默认） | 仅登录时判定；后台停用后新登录 403"该账号未启用 dsh"，既有会话存活至 TTL/退出 |
| `interval:<秒>` | 按租户缓存判定；停用后既有会话至多一个间隔内失效 |
| `per-request` | 每请求判定；停用几乎立即生效 |

`dsh_enforce` 与 `key_revalidate` 正交：前者问"账号可否使用 dsh"，后者验"worker 的模型凭据"。
判定失败（超时/5xx）一律 503 fail-closed，绝不放行。aigw 后台账号列表的"启用/停用 DSH"
按钮控制该开关的真值（`accounts.dsh_enabled`）；停用不删除租户数据，重新启用即恢复。

重验使用**租户当前 worker Key**，不保存登录时提交的旧 alias Key。`--keep-old-prefix` 允许仍有效的旧 Key 登录同一租户；它不使旧 Key 成为 worker 的模型凭据。

**飞书登录（M61）**：配置 `feishu.enabled` 后，门户登录页多一个「飞书登录」；点它会把浏览器送到
aigw 的 `/feishu/login`，由 aigw 完成飞书 OAuth 并**签一张一次性票据**，再送回门户的
`/login/feishu` 兑换成与 Key 登录**完全相同**的会话。dshgw 不持有任何飞书凭据、不登记第二个回调、
不需要出站访问飞书——票据密钥与 aigw 入口 URL 在监督形态下由 aigw 注入生成的配置。
收票时 dshgw 还会用该租户的 worker Key 调 `POST /v1/dshgw/authorize` 复核账号级授权，
因此控制台「停用 DSH」对飞书登录同样生效，判定失败一律 503（fail-closed）。
未绑定、账号停用、租户未就绪、票据过期/重放都会在门户给出明确文案。规格见 [docs/feishu.md](feishu.md) §5。

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

租户在自己的 dsh 里点「SSH 工作区」：选主机（读该账号 `~/.ssh/config` 的别名，也可手输
`user@host`，支持独立 SSH 用户名与端口输入）→ 浏览远端目录 → 新建远端目录 → 挂载并打开。挂载点是
`<workspace>/<mount_subdir>/<host>/<远端路径>`（默认 `ssh`），因此它落在该账号的 clamp 根之内，
可以直接作为一个工作区打开；因为沙箱只绑定本账号的这两棵树，**别的账号看不到也进不去**。

**分工（为什么不是一个纯插件）**：ssh 那一半在租户沙箱内、用**该租户自己的密钥**完成（列目录、
建目录、探测）；挂载那一半由 dshgw 在沙箱外完成。租户 worker 挂不了：profile 只给最小 `/dev`
（没有 `/dev/fuse`），且宿主 root 在它的 user namespace 里没有映射，`fusermount3` 的 setuid 因此
失效 —— 实测连 `tmpfs`/`proc` 的 `mount(2)` 都是 `EPERM`，加 `CAP_SYS_ADMIN` 也一样
（见 `docs/design/m64-ssh-workspace.md` §3）。代价是**挂载后要重启该账号的 worker**：bubblewrap 的
`--bind` 不携带子挂载，profile 在启动时为每个活动挂载点追加一次 `--bind-try`，所以挂载/卸载都会让该
账号的 dsh 重载（进行中的回合会中断，会话日志可 resume）。

| 面 | 是什么 |
|---|---|
| 通道 | 该账号 DSH home 里的文件信箱：`<dsh_home>/ssh-requests/<id>.json`（租户写）、`<dsh_home>/ssh-replies/<id>.json`（网关写）。不新增监听端口、不新增令牌 |
| 挂载记录 | `<state_dir>/ssh-mounts.json`（0600）；同时镜像一份到 `<dsh_home>/ssh-mounts.json`，租户插件靠它区分「真挂载」与镜像布局产生的父目录 |
| 密钥 | 账号默认：`<workspace>/.ssh/id_rsa`；主机专用：`<workspace>/.ssh/host_keys/<SHA256(host)>/id_rsa`（均 0600）。只选主机专用，否则选账号默认；没有密钥则拒绝连接。`known_hosts` 与可选 `config`（别名清单）同目录 |
| 隔离增量 | 只为活动挂载点各加一条 `--bind-try <挂载点> <挂载点>`；**不加设备、不加 capability**，M57/M58 口径不变 |
| sshfs 选项 | 默认 `reconnect, ServerAliveInterval=15, ServerAliveCountMax=3, idmap=user`；`allow_other`/`allow_root` 被代码丢弃（所有 worker 共用一个 uid，共享挂载等于跨账号可读） |

**密钥就是边界**：账号能读到自己的 `id_rsa`（跑 key 的进程就是它自己），所以「这个账号能到哪些主机」
完全由发给它的密钥决定。按账号限权要用 `identity_dir`（一账号一把）；共用 `identity_source` 等于所有
账号共享同一身份 —— `hosts` 白名单只是防跑偏，不是安全边界。

### 端口与私钥管理

- 主机地址可填别名、域名、IPv4 或 `user@host:2222`；独立端口输入接受 `1..65535`。留空则沿用别名的 `Port`，否则默认 SSH 端口 22。暂不支持 IPv6 字面量。
- 在「私钥管理」选择账号默认或当前主机专用，再选择本地私钥文件上传；支持 OpenSSH/RSA/Ed25519 等 `ssh-keygen` 可读取的**无密码私钥**，上限 64 KiB。加密私钥不支持，不要上传服务器 `/etc/ssh/ssh_host_*_key`。
- 界面只显示配置状态及 SHA256 公钥指纹，不回显私钥；再次上传替换所选范围的密钥，删除需确认。私钥以 0600 明文保存在本账号工作区（目录 0700），生产环境必须使用 HTTPS，备份应按敏感数据保护。
- 主机专用密钥绑定完整连接标识（包含显式用户名、端口）；别名和实际地址是不同绑定。不同账号即使填写同一主机也不共享上传文件。
- 删除主机密钥后回退账号默认；删除默认后不会在重启时从运维源重新复制（`identity-managed` 标记）。替换/删除不会撤销已经建立的 SSHFS 连接：要立即切换，请先卸载再重新打开，远端撤销授权需移除其 `authorized_keys` 中对应公钥。
- `identity_source` / `identity_dir` 仅用于初次预置，可以全部留空让用户自行上传；运行连接不会回退到运维共享源或 SSH agent。SSH 配置仅使用具体别名的 `HostName` / `User` / `Port`，不执行 `ProxyCommand`，不加载其中的额外 `IdentityFile`。
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

**为什么退出不直接跳门户的 `POST /logout`**：门户那条路由要求 `Origin` 精确等于门户 origin，而租户
页面只能发出自己租户的 origin（端口模式下两者端口不同），请求会被 403。因此退出由网关在租户 origin 下
执行同一份会话存储的删除；门户只作为落地页。

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
