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

退出是门户同源 **`POST /logout`**：撤销当前浏览器携带的各租户会话，并清除 cookie。GET 不改变状态，返回 405。其它浏览器的会话不受影响。

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

默认登录限流为每 IP 每分钟 10 次；会话默认上限 10,000，状态文件另有 64 MiB 上限。已建立的 WebSocket 不会被 logout/TTL 追溯关闭，新请求或重连会重新验证。

**已知上游限制（DSH 0.1.2-rc.1）**：dsh Web UI 的“设置/模型与提供方目录”视图只在浏览器地址栏为
回环（localhost/127.x）时工作——DSH 客户端对非回环页面将设置持久化设计为进程内（不发
`settings.describe`），模型目录因此显示 "settings are unavailable in this browser"。这与网关无关
（任何域名反代部署都一样，`--trusted-host` 是 worker 端 /api 的 Host 防护，与此闸门无关）。
不受影响：会话对话、模型调用（默认模型由 `sync-models` 写入）、工作区目录选择与 browser-fs
面板（独立插件）。模型与授权管理在 aigw 控制台完成。

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

全局参数在命令前，子命令选项在位置参数前。完整 Key **只从 stdin 或绝对 mode-0600 文件读取**，不接受 `--key sk-...`：

```bash
dshgw --config /etc/dshgw/config.yaml tenant create --key-file /root/alice.key alice
dshgw --config /etc/dshgw/config.yaml tenant list --json
dshgw --config /etc/dshgw/config.yaml bind <12字节前缀> alice
dshgw --config /etc/dshgw/config.yaml login-url                 # 门户 URL
dshgw --config /etc/dshgw/config.yaml login-url <12字节前缀>
dshgw --config /etc/dshgw/config.yaml tenant rotate-key --key-file /root/new.key alice
dshgw --config /etc/dshgw/config.yaml tenant restart alice
dshgw --config /etc/dshgw/config.yaml sync-models alice
dshgw --config /etc/dshgw/config.yaml revalidate alice
dshgw --config /etc/dshgw/config.yaml doctor
dshgw --config /etc/dshgw/config.yaml contract --key-file /root/alice.key all
dshgw --config /etc/dshgw/config.yaml render-nginx --reload
dshgw --config /etc/dshgw/config.yaml backup
dshgw --config /etc/dshgw/config.yaml tenant remove --purge --yes alice
dshgw --config /etc/dshgw/config.yaml upgrade-dsh /opt/dsh/releases/<候选目录>
```

- 建户、绑定、轮换、删除、备份、nginx 修改等运维命令需 root；`serve` 以专用低权用户 `dshgw` 运行。
- 租户名显式给出，与 aigw 控制台中的 Key 名无自动派生关系；`dsh-alice` 只是便于识别的命名建议。
- 空模型清单默认拒绝建户。`--allow-empty-models` 会标记 `models_pending`，不写 DSH 不接受的 `models: []` provider。授权模型后运行 `sync-models`。
- Key 轮换同时更新 credentials refs、模型 settings、gateway.key 与 registry；保留 `records` 和其它 provider 配置。旧 prefix 默认删除，`--keep-old-prefix` 可无限期保留。
- 已存在的保留目录或符号链接不能被建户流程覆盖。删除不加 `--purge` 时保留数据；后续不能用同名 `create` 当作“恢复”，应按备份恢复流程处理。
- 删除前停 worker、生成快照；`--purge` 另需 `--yes`。恢复/清理失败会明确报错，不把部分回滚伪装成成功。
- `migrate-nginx` 当前是 `render-nginx` 的兼容别名，不编辑 nginxWebUI 数据库。
- `doctor` 检查路径/模式/属主、运行文件与单元、模板及 nginx 配置；它不声称可以从 loopback 证明外部防火墙可达，也不代替 `revalidate`、模型调用或资源压测。

## 6. 模型与运行配置

`aigw_base_url` 是网关根 URL，例如 `http://192.168.190.86:8088`，不要再加 `/v1`。验证访问 `/v1/models`；写入 dsh 的 provider 使用 `api: openai-responses`、`baseURL: <根URL>/v1`，因此模型请求是 **`POST /v1/responses`**。

生成的 provider 同时带 `compat.supportsStrictMode: true`，使普通 Responses 工具显式发送 `strict: false`，防止某些上游把可选参数（如 `sandbox_permissions`）变成必填；不放宽 DSH 沙箱或审批策略。

多租户上线前必须将 aigw **`auth.default_grant: none`**，再显式授予模型。M51 不会替部署方静默修改 aigw 的授权配置。没有实现 `dshgw usage`，也不持有 aigw 管理/MCP 凭据。

## 7. 隔离、工作区与 browser-fs

| 层面 | 边界 |
|---|---|
| UID/文件权限 | 每租户独立用户，`/srv/dsh/<t>` 与租户状态私有；跨租户读写由 OS 拒绝 |
| systemd | `ProtectHome=tmpfs` 隐藏 `/home`、`/root` 等，`PrivateTmp=yes` 隔离临时目录；运行包必须在 `/opt` 而不是 symlink 回 `/home` |
| 资源 | 单 worker `MemoryHigh=1536M`、`MemoryMax=2G`、`CPUQuota=200%`、`TasksMax=512`；汇总 slice `MemoryMax=40G` |
| DirectoryPicker | `clamp` 默认将浏览与新建夹在租户根，realpath 拒绝 symlink 逃逸；`browse` 仅调试，不提供 `off` |
| 网络 | 共享宿主网络 namespace；不宣称 per-tenant 网络隔离，另加防火墙策略必须兼顾必要的 loopback 通信 |

预注册工作区默认是 `/srv/dsh/<tenant>/work`。目录选择器浏览的是**服务器**的文件系统，不是浏览器本机。clamp 只是防误操作：租户可改自己的 patch，agent 仍可读公开系统文件；真正边界始终是 UID/systemd。realpath 不能解决所有 Node TOCTOU 或 bind mount 情形。

`dsh-browser-fs@0.2.0` 默认开启，模板在供应阶段固定版本与 integrity、复制完整依赖。它另外提供 `browser_fs_list/read/write`，只访问用户在浏览器明确授权的**本机**目录；不改变服务器工作区、agent cwd 或 bash 执行位置。

完整模式需要 Chromium 系浏览器与安全上下文；标签页需保持打开，多设备时由持有授权句柄的浏览器执行。**已授权文件内容可能进入模型请求**，门户和部署方必须向使用者说明；可在建户时 `--browser-fs off`。

## 8. 运维与验收

```bash
make dshgw-test
make dshgw-verify
```

独立目标不会拖入原有 aigw `verify`。自动化验收覆盖 Go/import gate、静态构建、真实 DSH 基础契约与 picker、同一 worker 的真实凭据热载（dummy Key/假服务）、临时 browser-fs 模板及 HTTP/WS，以及 root 验收脚本的无特权自测试。它们不能替代 root 主机的两租户 UID、systemd、TLS/防火墙和 10–30 worker 资源验收。精确记录见设计文档 §9；运维分阶段运行 `scripts/dshgw_host_acceptance.py`，只分享 public report，私有 state/CLI 日志不得分享。

备份含完整 Key/upstream cookie，固定 mode 0600，仍需受控存储或额外加密。升级只接受已供应到 releases_root 的候选目录，先备份和运行契约，通过后才切换 current 并恢复原先活跃的 workers；失败先恢复旧 symlink 再回滚 workers。停止中的租户不会被升级流程意外启动。

详细的 owner/mode、恢复步骤、nginx include 管理和 root 验收要求见[部署手册](../deploy/dshgw/README.md)。root 与 dshgw 账户可冒充租户；每个 tenant agent 可读取自己的 API Key。以上均是已知信任边界，而不是隐藏的安全保证。
