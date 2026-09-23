# M77 设计文档：dshgw 多机分布式运行（单一控制面 + 工作节点 + 控制台节点管理 + SSH 一键部署）

> 状态：**设计已定稿，等确认后写代码**。
> 规格：[docs/dshgw.md](../dshgw.md) §9（新增）、[deploy/dshgw/README.md](../../deploy/dshgw/README.md)（节点安装与一键部署）、
> [docs/deployment-layout.md](../deployment-layout.md)（节点数据布局）、[docs/mcp.md](../mcp.md)（新管理面端点）。
> 相关：[M51 dshgw](m51-dshgw.md)（历史形态与 D3/D5/D6 的 origin 与隔离分析）、[M58 aigw 监督的 rootless dshgw](m58-aigw-supervised-dshgw.md)、
> [M64 SSH 工作区](m64-ssh-workspace.md)、[M69 登录驱动的生命周期](m69-login-lifecycle-and-settings-merge.md)、
> [M76 退出强制拆除](m76-dsh-exit-force-teardown.md)、[浏览器 FUSE 工作区](browser-fuse-workspace.md)。
> 需求原话（本轮对话）：「实现dshgw 可以在局域网内的多台机器分布式运行」→「节点管理没有管理后台webui」（补充：节点管理必须有管理后台 WebUI）
> →「后台支持一键通过ssh部署」（补充：控制台要能一键经 SSH 把节点部署到目标机器）。

## 1. 目标与非目标

**目标**：把今天单机形态的 dshgw 拆成「**一个控制面 + 多个工作节点**」：

- **控制面**（仍由 aigw 监督，仍是 `dshgw serve`）独占对外面与真值：门户、每个租户的公开端口（`internal/dshgw/edge`）、
  会话存储、租户注册表、审计、activity、与 aigw 的全部交互。
- **工作节点**（新子命令 `dshgw node serve`，跑在局域网其它机器上）只跑**租户侧**的执行与服务：
  bwrap 沙箱里的 dsh worker、SSH 工作区（sshfs + 邮箱请求）、浏览器本机目录 FUSE 工作区、`host_shares`、
  三块租户插件。
- **管理后台**：aigw 控制台新增「DSH 节点」页 —— 节点清单、健康/漂移、租户生命周期与迁移、
  **通过 SSH 一键把节点部署/升级到目标机器**。节点本身不提供 WebUI。

可验收的标准：

1. **单机不回归**：不配置任何节点时行为与今天逐字节等价（既有 `make dshgw-test` / `make dshgw-supervised-test` 全绿；
   不含 `node` 字段的旧 `registry.json` 照常加载，租户仍留在控制面本机）。
2. **多机可用**：`dshgw tenant create --node <n> alice`（或控制台选定节点建租户）之后，门户登录 → 租户 UI 200 →
   `GET /api` 401，且**控制面的进程树里没有该租户的 worker**（bwrap 只在节点上），
   节点侧功能面（SSH 工作区 / 浏览器目录 FUSE / host_shares / 三插件，含终端 WS 与流式）全部可用。
3. **一键部署**：控制台填一张表单（名称、SSH 主机/端口/用户、私钥、部署目录、监听地址、端口段与功能覆盖）→
   点「部署」→ 页面分阶段显示进度与日志 → 状态变「就绪」→ 该节点立刻可用来建租户；
   同一个按钮再点一次即**升级/重建**；失败自动回滚到上一版本并保留日志。
4. **异常可诊断**：控制台显式呈现并处理「节点不可达」「协议/版本不匹配」「注册表与节点自述漂移」，
   能一键探测与对账；节点不可达时租户请求返回 **503 `node_unreachable`**，而门户与登录不受影响。
5. **失败不伪装成功**：节点重启后按其本地分配表恢复（尊重 `suspended`）；控制面重启不打断节点上的 worker，
   `reconcile` 后接管；会话在上游 401 后重握手一次自愈。

**非目标**（明确不做，见 D15）：

- 自动放置 / 按负载调度 / 节点宕机时租户自动漂移；控制面多副本或 HA；共享存储上的租户数据。
- 控制通道 TLS（本里程碑只留配置位）；节点由 aigw 监督（跨机器不成立）。
- 代装 OS 包与 Node/dsh release（预检缺失即失败并给出确切命令；`--with-packages` 为可选开关，默认关）。
- 密码登录、SSH agent 转发、跳板机；远端定时自动升级。
- 租户侧栏显示节点；节点自身提供 WebUI。

## 2. 现状与约束（证据）

### 2.1 今天的形态

| 事实 | 出处 |
|---|---|
| aigw 拉起并监督同目录子进程：`exec.Command(binary, "--config", cfgPath, "serve")` | `internal/dshgwsup/supervisor.go` |
| `dshgw serve` 把门户/租户公开端口、会话、registry、握手、租户生命周期、SSH/FUSE 服务装在一个进程里 | `cmd/dshgw/serve.go` |
| 每个租户 = 一个 bwrap 子进程里的 `dsh web --port <workerPort> --no-open`（外加 `--trusted-host <公开主机>`）；父进程死亡即随死（Pdeathsig）、可加 cgroup 限额 | `internal/dshgw/tenancy/worker.go`、`runner.go` |
| worker 启动行里的令牌被父进程读走并写成 `<handshake_dir>/<tenant>.url`（worker 自身 stdout） | `internal/dshgw/tenancy/runner.go`（`recordStartURL`） |
| 上游目标固定为 `127.0.0.1:<workerPort>`，且 **Host 也固定成同一个 authority** | `internal/dshgw/proxy/proxy.go`（`ensureUpstream` / `reverseProxy`） |
| 握手：`GET http://127.0.0.1:<port>/?token=…`（redirect manual）→ 必须 303 + 一个非空未过期的 `dsh-auth-*` cookie | `internal/dshgw/handshake/handshake.go` |
| 会话里保存的就是「会话 → worker cookie」，`Upstream{Name,Value,Authority,ExpiresAt}`，401 时 CAS 清除并重握手一次 | `internal/dshgw/session`、`proxy.go` 的 `retryTransport` |
| 控制台 → aigw → dshgw 的本地通道是 UNIX socket + JSON 行协议（SO_PEERCRED 白名单，无 TCP），只有 5 个租户生命周期 op | `cmd/dshgw/admin_serve.go`、`internal/localdshgw/client.go` |
| 管理面路由表驱动：每条路由声明 `Name/Group/Role/Summary/Notes/Body`，并**自动**出现在 MCP 的 `admin_endpoints` 目录里 | `internal/httpapi/admin_routes.go`、`docs/mcp.md` §4.5 |
| 控制台是 hash 路由 + 按需 ES module：加一个页面 = 加一个文件 + `routes` 一条表项 | `internal/webui/static/js/router.js` |
| 控制台行为在没有 node/npm 的环境里由真实浏览器 + fixtures 走查（`make ui-check`），DOM 级断言另有 `internal/webui/tests/*.mjs` | `scripts/ui-harness/`、`Makefile` |

### 2.2 硬约束（决定了本设计）

1. **worker 不能监听 LAN**：`dsh web` 明确拒绝 `--host 0.0.0.0`
   （`node_modules/@deepseek-ai/dsh-web-app/lib/startup.js`：「it would expose remote code execution to the network」）。
   ⇒ 节点侧必须有一个我们自己写的「唯一 LAN 面」组件，worker 永远只绑回环。
2. **cookie 绑定 Host authority**：cookie 名是 `dsh-auth-<base64url(sha256(authority))>`，签名载荷含 authority，
   逐请求比对（M51 D4 及其契约 `host-origin-fence`，实测：回环 Host + cookie = 放行；外部 Host = 403）。
   ⇒ **只要节点向 worker 的每一跳都强制 `Host: 127.0.0.1:<workerPort>`，cookie 与栅栏语义一字不改**，
   控制面也不需要复刻 dsh 的 HMAC。
3. **mount 是内核状态、属于执行机器**：sshfs 与浏览器目录 FUSE 都由「跑 worker 的那台机器」创建并持有
   （租户沙箱里没有 `/dev/fuse`，也没有可用的 `fusermount3`，见 M64 §3）。⇒ 这两个服务必须随 worker 一起搬到节点。
4. **模板制备需要 Corepack/pnpm 与网络**（`deploy/dshgw/prepare-template.sh` 用 `corepack pnpm` 装
   `dsh-browser-fs@0.2.0` 并校验 integrity）。⇒ 一键部署默认**把控制面已备好的模板与插件目录推过去**，
   让节点首启不依赖联网（D13）。
5. **常驻方式只有「用户级」**：本形态无 root、无 systemd 系统单元；`scripts/aigw_user_service.sh` 已经确立了
   `systemctl --user` + `loginctl enable-linger` 的做法。⇒ 节点在一键部署时装**用户单元**；linger 不可用时退化
   为 `setsid nohup` 并如实告知「重启机器不会自动拉起」。
6. **容量基线**：空闲 worker ≈304 MB RSS、每租户 ≈193 MB 磁盘（M51 D11 实测）。⇒ 节点是水平扩容单位，
   控制台要显示每节点承载租户数/运行 worker 数。

## 3. 关键决策（含取舍）

### D1 形态 = 单一控制面 + 多工作节点

控制面独占门户、公开端口、会话、registry、审计、activity 与 aigw 交互；节点只跑租户侧执行与服务。
取舍：多一层 LAN 反代与一套控制协议，换来**一份真值**（registry/会话）与**一套登录语义**；
另一种形态（每台机器一个完整 dshgw + 入口按租户分片 + 登录票据交接）代码量更小，但会引出多份 registry 的漂移
与「登录发生在 A 机、租户在 B 机」的跨机会话问题，被否决。

### D2 租户数据跟随节点本地磁盘

`workspace`、`.dsh`、ssh 私钥、会话记录都只在租户所属节点上；控制面只保存 registry 里的路径字符串。
取舍：放弃「任意节点可服务任意租户」的自由，换来不引入网络文件系统语义（flock / 原子 rename / FUSE 挂载 /
bwrap 绑定在 NFS 上的行为需要重新论证）。迁移 = 停租户 → 运维搬目录 → 受守卫的 `set-node` 接管（§7）。

### D3 远程节点保留全部租户侧功能

`sshworkspace`、`browsermount`(FUSE)、`hostshare`、`tenant_plugins` 都在节点运行；节点需要
`bwrap`、Node + dsh release、模板、插件目录，启用相应功能时还需要 `sshfs`/`fusermount3`/`/dev/fuse`。
取舍：节点环境要求变高，换来「远程租户 = 本机租户」的功能等价（见验收标准 2）。

### D4 控制通道 = 明文 HTTP + 每节点共享令牌（v1）

局域网内明文 HTTP，`Authorization` 用每节点独立令牌（32 字节随机，`token_file` 0600）。
**必须写进文档的前提**：这个监听面等价于「该节点上全部租户数据的完全访问权」，因此
①只绑内网接口（`node.listen` 默认 `0.0.0.0` 会拒绝，必须显式写目标地址）；②防火墙限制来源到控制面；
③worker 端口始终只绑回环；④控制通道 TLS 留到后续里程碑（配置位预留）。
取舍：v1 少一套证书生成/轮换/指纹流程，换来内网内的明文令牌；这是用户明确选择的档位。

### D5 worker 永不暴露 LAN

见 §2.2 第 1 条。节点代理是唯一 LAN 面组件，且它只按 `X-Dshgw-Tenant` 转发到**本机回环** worker。

### D6 保真不变量：worker 的 Host authority 端到端不变

节点向本机 worker 的每一跳强制 `Host: 127.0.0.1:<workerPort>`（与今天控制面直连完全一致），
因此 dsh 的 cookie 派生、`/api` Host 栅栏、`Location` 改写判定与 `contract` 的 `host-origin-fence` 全部不变。
这是本设计能「不动 dsh、不动契约、不动会话语义」的根因，必须由测试逐条锁住（§8）。

### D7 控制面是运行期唯一真值

- 控制面：registry（含 `node`）、sessions、audit、activity、`tenant-config/<t>/gateway.key` 主副本。
- 节点：自己的 `registry.json`（**分配表**：这台机器上承载哪些租户、各自的路径与 worker 端口，
  用于节点重启后恢复自己的 worker）与本地 key 副本（worker 需要它向 aigw 取模型，见 §3.6 决策）。
- 方向单向：控制面推送，节点不自作主张；节点在 `status` 里自述实际状态，差异由控制面 `reconcile` 修正。

### D8 同一二进制、同一配置类型

节点模式是 `dshgw node serve`（同一 `bin/dshgw`，同一 `internal/dshgw/config`）；控制面仍是 `dshgw serve`。
节点模式**不**绑定门户/公开端口/会话存储/飞书/admin socket，只绑 `node.listen`。
取舍：节点镜像与控制面必须同版本（协议=1 时强制），换来一套代码、一种配置心智、一个构建产物。

### D9 控制面本机即节点 `local`

`tenant.Node` 为空或 `local` 走今天的本地代码路径；`local` 是保留名（配置与状态里都不允许定义同名节点）。

### D10 节点管理全在 aigw 控制台，节点无 WebUI

节点没有账号体系与会话，只暴露令牌保护的 API 与健康端点；运维面统一在控制台（同一套登录、角色、审计、
MCP 目录）。取舍：控制台在 dshgw 通道不可用时无法展示节点（页面显式报错，沿用现有 501 语义），
换来不新增第二套认证面与第二套 UI。

### D11（修订）节点清单的真值是 dshgw 的节点状态，不是 aigw 数据库

节点记录（名称、监听、令牌、SSH 目标、部署路径与配置覆盖、部署状态与日志）由 dshgw 拥有，持久化在
`<state_dir>/nodes.json`（0600）+ `<state_dir>/node-ssh/`（私钥与 known_hosts）；控制台经既有 admin socket 增删改与部署。
`config.yaml` 的 `nodes:` 仍作为**静态/引导清单**支持，与状态清单合并，规则一句话：

> **配置说了算的是身份（地址 + 令牌），状态清单贡献的是部署信息**（SSH 目标、部署路径与覆盖项、部署状态与日志）。

两处都有同一个名字因此不是错误，而是"在配置里声明节点、在控制台按部署按钮"这个正当用法；
控制台**新增**一个配置里已声明的名字会被拒绝（提示"由配置定义，直接点部署"）。
**aigw 数据库不新增节点表**：否则同一个事实会有两份（DB 一份、dshgw 一份），而 dshgw 是唯一能操作节点的一方；
`config.yaml` 也不适合承载「控制台里点出来的」东西。取舍：节点清单跟着 dshgw 的 state 走（它在同一个数据根、
同一份备份里），换来单一真值与「aigw 不持有节点令牌与 SSH 私钥」。

### D12 一键部署由 dshgw 执行 SSH

`nodedep` 用系统 `ssh`（`-F /dev/null`、`-o IdentitiesOnly=yes`、`-o BatchMode=yes`、`-o StrictHostKeyChecking=yes`）
经 stdin 传 `tar` 到目标机，沿用 `internal/dshgw/sshworkspace/exec.go` 已经验证过的调用与错误分类模式，
并复用它的 `ExecFunc` 测试缝。不选「aigw 执行 SSH」的理由：aigw 必须保持不 import dshgw 内部、
不持有节点令牌（架构分层表与 M51 D2 的口径）；控制台只提交**结构化意图**，不下发任意命令或脚本。

### D13 部署载荷默认由控制面推送，节点首启不联网

载荷 = `bin/dshgw`（同版本同 revision，保证协议一致）+ 插件目录（`picker-clamp.js`、`web-tty/`、
`workspace-files/`、`git-diff/`）+ 已备好的 `template_home`（`prepare-template.sh` 的产物，含
`dsh-browser-fs@0.2.0` 与 integrity 校验）+ 生成的节点配置与令牌。备选 `--prepare-template-on-node`
让节点自己跑 `prepare-template.sh`（需节点的 Corepack 与网络）。**不代装 OS 包与 Node/dsh release**：
预检缺失就失败并给确切命令；`--with-packages` 可选（需 passwordless sudo，默认关）。

### D14 SSH 只支持密钥、主机指纹固定

私钥存放控制面 `<state>/node-ssh/<name>/id_ed25519`（0600），来源是运维预置路径或控制台粘贴上传
（接口/日志永不回显私钥，只给 SHA256 指纹 —— 与 M64「私钥管理」面板同一口径）。
`StrictHostKeyChecking=yes` + 专用 `known_hosts`；**首次部署必须由操作者确认目标主机指纹**
（控制台弹窗显示指纹与来源，CLI `--accept-host-key`），此后固定。永不使用 `StrictHostKeyChecking=no`。

### D15 明确不做

自动放置/负载调度、节点宕机租户自动漂移、控制面多副本/HA、共享存储、控制通道 TLS、节点由 aigw 监督、
远端定时自动升级（只在点「部署/升级」时发生）、代装系统包与 Node/dsh、密码登录、agent 转发、跳板机、
租户侧栏显示节点、节点自身 WebUI。

## 4. 接口

### 4.1 `internal/dshgw/config`（控制面与节点共用）

```go
// 静态/引导清单：与 nodes.json 合并（配置=身份，状态=部署信息；见 D11）。
type Node struct {
    Name      string `yaml:"name"`       // ^[a-z][a-z0-9-]{0,25}$，"local" 保留
    URL       string `yaml:"url"`        // http://host:port，无 path
    Token     string `yaml:"token"`      // 二选一
    TokenFile string `yaml:"token_file"` // 推荐：0600 文件，令牌不出现在配置里
}

// 节点模式（dshgw node serve）
type NodeSelf struct {
    Name      string `yaml:"name"`
    Listen    string `yaml:"listen"`     // 必须显式给出（拒绝 0.0.0.0/空）
    Token     string `yaml:"token"`
    TokenFile string `yaml:"token_file"`
}
```

`Config` 增 `Nodes []Node`、`DefaultNode string`、`NodeSelf NodeSelf`，以及
`NodeByName(name) (Node, bool)`、`IsLocalNode(name) bool`、`NodeMode() bool`。
校验：
①节点名唯一、非 `local`、形状合法；②`url` 是无 path 的 `http://`；③令牌来源二选一且 `token_file` 0600；
④`default_node` 必须是已知节点或 `local`；⑤`node.listen` 不得落在本机 `worker_port_lo..hi` 与公开端口段内；
⑥节点模式必填 `aigw_base_url`、`deploy.template_home`、`deploy.plugin_path`、`dsh.{node_bin,bin_js,current_link}`、
`worker_port_lo/hi`、`node.{name,listen}`；⑦`worker_limits` 语义 = 谁起 worker 谁生效（节点机器本地）。

### 4.2 `internal/dshgw/registry`

```go
type Tenant struct {
    // …既有字段…
    // Node 是承载该租户 worker 的节点名；空 = 本机（local）。旧 registry 不含该键，
    // 空值即历史语义，因此升级不需要迁移。
    Node string `json:"node,omitempty"`
}
func (r *Registry) TenantsForNode(node string) []Tenant
func (r *Registry) SetNode(name, node string) error        // 只改放置，不动数据
func (r *Registry) SetWorkerPort(name string, port int) error // reconcile 用
// 可注入校验钩子（默认接受，保持 registry 单测与旧文件不被配置耦合）：
func (r *Registry) ValidateNodes(ok func(string) bool)
```

### 4.3 `internal/dshgw/nodestore`（节点记录）

`<state>/nodes.json`（0600，原子替换 + flock 单写者）：

```go
type Node struct {
    Name   string; Listen string; Token string; TokenFile string
    Source string           // config | console
    SSH    SSHConfig        // host, port, user, key_file, known_hosts, host_key_fingerprint, accepted_at
    Deploy DeployPaths      // dir, state_dir, plugin_path, template_home, bwrap_bin,
                            // node_bin, bin_js, current_link, worker_port_lo, worker_port_hi
    Overrides Overrides     // host_shares, ssh_workspaces.enabled, browser_workspaces.enabled,
                            // worker_limits, workspace_seed, directory_picker, plugin_browser_fs
    Status Status           // state: pending|deploying|ready|failed|unreachable, phase,
                            // version, revision, protocol, started_at, finished_at, error, log_path
}
```

`Status` 与运行期健康探测结果分开存：`nodes.json` 里是**持久化的最后一次结果 + 部署状态**，
内存里另有一份带 TTL（10s）的健康快照供控制台轮询（`node-list` 不因此变慢）。

### 4.4 新包

| 包 | 内容 |
|---|---|
| `nodeproto` | `ProtocolVersion = 1`；头名 `X-Dshgw-Node-Token`、`X-Dshgw-Tenant`、`X-Dshgw-Browser-Session`、`X-Dshgw-Protocol`；路径前缀 `/node/v1/…`；信封 `{ok,value,error:{code,message}}`；错误码 `node_unreachable`/`node_auth_failed`/`node_protocol_mismatch`/`tenant_unknown`/`worker_not_running`。 |
| `nodeclient` | 控制面客户端：`Health/Status`、`TenantCreate/Start/Stop/Restart/Remove/SetKey/SyncModels/SetNode`、`Handshake`、`SandboxExec`、`CaptureURL`、`Backup`、`Reconcile`、`LogoutStop`；健康 5s、生命周期 5min 超时；连接复用（keep-alive）；错误映射到 `nodeproto` 的错误码。 |
| `nodeserve` | 节点侧服务：令牌闸门（常数时间比较，**401 前不查任何租户**）→ 控制路由（委派节点本地 `tenancy.Manager`）→ `/node/v1/tenant/…` 数据面反代（按 `X-Dshgw-Tenant` 选租户、强制 `Host: 127.0.0.1:<workerPort>`、剥离全部 `X-Dshgw-*`、WS/upgrade 直通、`FlushInterval=-1`、请求体 ≤64 MiB）→ `/browser-workspace/**` 交节点本地 `browsermount`（会话 id 取 `X-Dshgw-Browser-Session`，与控制面今天的 `sha256(cookie)` 口径一致）→ `/node/v1/health`。 |
| `nodedep` | SSH 一键部署：`Plan`（预检项 + 载荷清单 + 生成配置）、`Runner`（`ExecFunc` 缝）、阶段机、单飞、有界日志、失败回滚（见 §6）。 |

### 4.5 控制面改动（调用点不重复实现分派）

- `tenancy.Manager` 增 `Nodes *NodeSet`；`Create/StartWorker/StopWorker/Restart/Remove/SetKey/SyncModels/
  EnsureRunning/Status/StopForLogout/Backup/SandboxProfile/SandboxProfileReady/ProbeWorker` **按 `t.Node` 分派**：
  本地走今天实现，远程走一条 RPC。于是 `cmd/dshgw/admin_serve.go`、`login_prepare.go`、`ops.go`
  不需要各自写一遍分派（那里是今天所有入口的汇合点）。
- `tenancy.Manager` 的 `ModelRefresh`/`SyncModels` 对远程租户由节点执行（节点持有该租户的 worker key 副本，
  也有到 aigw 的网络——worker 本来就要直连 aigw）。控制面在登录时仍然自己向 aigw 校验并取模型清单
  （身份与授权判定留在控制面），把结果通过 RPC 交给节点应用。
- `proxy.Proxy`：抽出 per-tenant provider，替代今天写死的
  「`handshake.FileSource` + `HTTPExchanger` + `127.0.0.1:<port>`」三件套：

  ```go
  type UpstreamProvider interface {
      Authority(registry.Tenant) string
      Exchange(ctx context.Context, t registry.Tenant, authority string) (*session.Upstream, error)
      BaseTransport(*config.Config, registry.Tenant, string) http.RoundTripper
      ServesBrowserWorkspaces() bool
  }
  ```

  - `localProvider` = 今天的实现（逐字节保留）。
  - `remoteProvider` = `nodeclient`：`Exchange` 让节点读它本地的 handshake 文件并完成 303+Set-Cookie，
    回传 `{name,value,authority,expires_at}` 存进**同一个 session store**（会话语义不变，401 重握手路径不变）。

- `TenantHandler` 的次序**完全不变**：target 校验 → edge origin 栅栏 → 会话 → `handleAccountRoute`/`/dshgw/**`
  （仍由控制面服务，它们只需要会话，不需要 worker）→ `prepareReplayable` → 上游握手 → `ReverseProxy`。
  只改两处：**远程租户不在控制面拦截 `/browser-workspace/`**（转发给节点）；上游目标与 `Host` 由 provider 决定。
  `ModifyResponse`（剥 worker cookie、no-store、`injectSettingsBootstrap`、Location 过滤）与 `retryTransport`
  留在控制面 ⇒ **响应侧语义零变化**。
- 错误映射：`node_unreachable`/`node_protocol_mismatch` → **503**（不用 502：上游是网关自身不可达），
  审计 `node_unreachable`；节点透传的 worker 5xx 原样返回。
- 头不变量：节点头由 `nodeclient` 在 `stripRequestHeaders` **之后** `Set`（不是 `Add`），
  浏览器伪造的 `X-Dshgw-*` 永不外流；节点向 worker 转发前再剥一次。测试逐条断言。

### 4.6 会话与握手（不动的原因）

cookie 由 worker 按请求 Host 派生（§2.2 第 2 条）。节点向 worker 用的 authority 仍是
`127.0.0.1:<workerPort>`（节点本地分配、控制面记录），因此：控制面持有的 `Upstream` 在任何节点上都有效，
**不需要**在控制面与节点之间同步 cookie 派生密钥，也不需要改 `internal/dshgw/contract` 的任何一条。

节点重启或 worker 端口变化 ⇒ 旧 cookie 失效 ⇒ 上游 401 ⇒ `retryTransport` 走 `remoteProvider.Exchange`
重握手一次 ⇒ 透明恢复。这条路径由 e2e 直接验证（§8 第 5 条）。

### 4.7 dshgw admin socket 协议扩展

| op | 作用 |
|---|---|
| `tenant-list`（扩展） | 结果增 `node/running/suspended/handshake/public_port/worker_port/last_login` |
| `tenant-create`（扩展） | 请求增可选 `node`（缺省 = `default_node`） |
| `node-list` | 节点清单 + 健康快照 + 承载/运行统计 + `default_node` + 通道状态 |
| `node-add` / `node-update` / `node-remove` | 记录增删改（`remove` 默认**不碰**远端数据，`purge` 才删） |
| `node-deploy` / `node-deploy-status` | 异步一键部署/升级 + 阶段与日志尾部 |
| `node-probe` / `node-reconcile` / `node-rotate-token` | 立即探测 / 权威分配表对账 / 令牌轮换 |
| `tenant-restart` / `tenant-set-node` | 远程租户重启 / 受守卫的放置变更 |

`internal/localdshgw.Ops` 相应扩展（`CreateTenantIn`、`RestartTenant`、`MoveTenant`、`NodeList/AddNode/
UpdateNode/RemoveNode/DeployNode/DeployStatus/ProbeNode/ReconcileNode/RotateNodeToken`），
`TenantInfo` 增节点与 worker 状态字段，新增 `NodeInfo`/`NodeDeployStatus`。通道不可用时的错误语义保持不变
（控制台按 501/503 显示原始文案）。

### 4.8 aigw 管理面 API（新增；路由表驱动 ⇒ 自动进入 MCP 目录）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/admin/api/v1/dshgw/nodes` | viewer | 节点清单 + 健康快照 + 承载/运行统计 + `default_node` + 通道状态 |
| POST | `/admin/api/v1/dshgw/nodes` | admin | 新增节点记录（名称/监听/SSH 目标/私钥来源/部署路径/端口段/覆盖项） |
| GET | `/admin/api/v1/dshgw/nodes/{name}` | viewer | 节点详情：`status/phase/error/log_tail/version/revision/protocol/tenant_counts/drift` |
| PATCH | `/admin/api/v1/dshgw/nodes/{name}` | admin | 改记录（SSH 目标、路径、覆盖项；不允许改名） |
| DELETE | `/admin/api/v1/dshgw/nodes/{name}` | admin（Dangerous） | 删记录；默认保留远端数据，`purge=true` 才删远端 |
| POST | `/admin/api/v1/dshgw/nodes/{name}/deploy` | admin（Dangerous） | **一键部署/升级**（异步作业） |
| POST | `/admin/api/v1/dshgw/nodes/{name}/probe` | admin | 立即探测 |
| POST | `/admin/api/v1/dshgw/nodes/{name}/reconcile` | admin（Dangerous） | 推送权威分配表并修正差异 |
| POST | `/admin/api/v1/dshgw/nodes/{name}/rotate-token` | admin（Dangerous） | 轮换节点令牌并重新部署 |
| GET | `/admin/api/v1/dshgw/tenants` | viewer | 租户清单（分页 + `node` 过滤） |
| POST | `/admin/api/v1/dshgw/tenants/{name}/start\|stop\|restart` | admin（Dangerous） | worker 生命周期 |
| POST | `/admin/api/v1/dshgw/tenants/{name}/move` | admin（Dangerous） | body `node`；受守卫 |
| POST | `/admin/api/v1/accounts/{id}/dsh`（扩展） | admin | body 增可选 `node`，只作用于**新建**租户 |

每条路由按 `docs/mcp.md` §4.5 写全 `Name/Group/Role/Summary/Notes/Body`（对象字段必须给 `Schema`，
否则构造期 panic）。新增 `groupDshgw` 分组。

### 4.9 CLI

`dshgw node serve|doctor|status`；`dshgw node add|update|remove|deploy|probe|reconcile|rotate-token`
（与控制台同一套语义，允许纯 CLI 运维）；`dshgw tenant create --node`、`tenant list` 增 `NODE` 列、
`tenant set-node`（受守卫）、`start/stop/restart/rotate-key/remove` 按 registry 分派；
`sync-models/capture-url/sandbox-exec --print` 远程走 RPC（`capture-url` 回传的 URL 含 worker bearer token，
按敏感数据处理）；`dshgw backup` 逐节点调用 `backup`（节点把自己承载的租户快照写进节点 `backup_dir`，
控制面只记录返回路径并提示「跨机归档由运维负责」）；`doctor` 增节点段（可达性、协议、版本、linger）。

## 5. 数据流

**登录**（与今天同一条链，只是末端的「确保 worker 在跑」落到节点）：

```
门户表单 → aigw /v1/models 校验 → authorize（账号级 dsh 开关 + 租户名）→ 会话 cookie
  → PrepareLogin：AdoptKey（可选）→ aigw 取模型清单 → RPC(node).sync-models/凭据引用/挂载恢复
  → RPC(node).ensure-running → 303 到租户 URL
```

**租户请求**：

```
浏览器 → 控制面 edge（租户公开端口/路径，Host + edge 头栅栏不变）
      → 会话校验 / key_revalidate / dsh_enforce（控制面，不变）
      → /dshgw/**（控制台行/退出）由控制面服务；/browser-workspace/** 远程改为转发
      → nodeclient: POST http://<node>:<port>/node/v1/tenant/<path>
            X-Dshgw-Node-Token: <token>
            X-Dshgw-Tenant: <tenant>
            Cookie: dsh-auth-…（来自 session store）
      → 节点令牌闸门 → 查节点本地 registry → 强制 Host: 127.0.0.1:<workerPort>
      → 本机回环 worker → 响应原样回程
      → 控制面 ModifyResponse：剥 worker cookie / no-store / HTML 注入 / Location 过滤
```

**握手**：

```
控制面无 upstream（或节点重启后 401）→ RPC(node).handshake
  → 节点读 <node_state>/handshake/<tenant>.url → 本地 HTTPExchanger（303 + dsh-auth-*）
  → 回传 {name,value,authority,expires_at} → 控制面写入 session store → 重试一次
```

**控制面对账（启动与漂移修复）**：

```
控制面加载 registry → 逐节点 RPC(node).status（自述：承载租户、worker 端口、运行状态）
  → 差异：续跑（节点已在跑且端口一致）/ 启动（应有未跑）/ 停止（suspended）/ 端口变化则更新 registry
  → 审计 node_reconcile
```

## 6. SSH 一键部署规格（`nodedep`）

### 6.1 阶段机

| 阶段 | 动作 | 失败处置 |
|---|---|---|
| `preflight` | ssh 连通性与指纹校验；`uname -m`、`id -u`、`id -un`；`systemctl --user` 与 linger 状态；按功能开关检查 `bwrap`/`sshfs`/`fusermount3`/`/dev/fuse`；`node`、`dsh bin.js` 可执行；部署目录可写；`node.listen` 与 worker 端口段未被占用；磁盘余量 | 记 `failed` + phase + 确切命令（如 `sudo loginctl enable-linger <user>`、`apt-get install -y bubblewrap`），**不改动远端** |
| `upload` | `tar` 经 ssh stdin 解到 `<deploy_dir>/{bin,dshgw-node.yaml,plugins,template-home}`；原子替换（`.new` → `mv`），上一版本留 `<deploy_dir>/.prev/` | 保留上一版本继续运行 |
| `configure` | 生成 `dshgw-node.yaml`（`node.{name,listen,token_file}`、`state_dir`、`worker_port_lo/hi`、`aigw_base_url`、`deploy.*`、`dsh.*` 与控制面同构的功能块 + 该节点覆盖项）；32 字节随机令牌写节点 `token_file`（0600）并记入控制面 `nodes.json`；写 `known_hosts` | 回滚配置 |
| `unit` | 生成 `~/.config/systemd/user/dshgw-node.service`（`WorkingDirectory=<deploy_dir>`、`ExecStart=<deploy_dir>/bin/dshgw --config <deploy_dir>/dshgw-node.yaml node serve`、`Restart=always`，沿用 `scripts/aigw_user_service.sh` 的做法）、`daemon-reload`、`enable`、`restart`；无 user manager 时退化 `setsid nohup` 并在结果里标明「重启机器不会自动拉起」 | 恢复旧单元并启动旧版本 |
| `start` | 等 `/node/v1/health` 就绪（≤60s） | 回滚到 `.prev` 并启动旧版本，`failed` 附日志 |
| `verify` | 校验节点自述 `name`、`protocol=1`、`revision` 与控制面一致；对已有租户做一次 `/api` 401 探针，或建一个临时验收租户（默认开，验完删除） | 就绪 + warning |

**幂等与升级**：同按钮重跑（幂等）。节点 `revision` 与控制面不同 ⇒ 走 upload→unit→start→verify，
**会重启该节点上的 worker**（控制台确认框显式警告「将重启该节点 N 个运行中的租户」）。
`node-rotate-token` 单独提供（重新部署 + 轮换令牌，存在短暂 401 窗口，由重握手自愈）。

### 6.2 载荷清单

| 内容 | 来源 | 缺失时 |
|---|---|---|
| `bin/dshgw` | 控制面同目录可执行文件（`os.Executable()`；测试可注入） | 部署失败（内部错误） |
| 插件目录 `picker-clamp.js`、`web-tty/`、`workspace-files/`、`git-diff/` | `deploy.plugin_path` 及其同级目录 | 按功能开关：开着则失败并列出缺失路径（与建户同一原则） |
| `template-home/` | `deploy.template_home`（已含 `dsh-browser-fs@0.2.0` 与 integrity 校验） | `--prepare-template-on-node` 时在节点跑 `prepare-template.sh`，否则失败 |
| `dshgw-node.yaml` + 令牌 | 由 `nodeclient`/记录生成 | — |

**不代装**：OS 包（`bwrap`、`sshfs`、`fusermount3`、`snap`/`apt` 系）与 Node/dsh release。
预检给出确切命令；`--with-packages` 仅在 passwordless sudo 可用时执行且默认关。

### 6.3 一次性必要动作与退化

- 首次部署：操作者在控制台确认目标主机指纹（显示 `SHA256:…` 与来源），控制面固定它；
  CLI 等价物 `dshgw node deploy --accept-host-key <指纹>`。
- linger：`loginctl enable-linger <user>` 可能需要 root/polkit。部署会先尝试（`loginctl enable-linger` 自身），
  再核对 `Linger=yes` 与 `systemctl --user` 可用；不可用时退化 `setsid nohup` 启动并在结果与页面上
  给出「一次性执行 `sudo loginctl enable-linger <user>` 后重启节点即可开机自启」。
- 审计：`node_add`、`node_deploy_started`、`node_deploy_finished`、`node_deploy_failed`（含 phase 与错误正文）、
  `node_remove`、`node_token_rotated`、`node_reconcile`。

## 7. 迁移与放置

- **放置是显式的**：`--node` / 控制台启用弹窗 / `default_node`；没有自动调度。
- **迁移（v1）**：`dshgw tenant set-node <t> <node>` / 控制台「迁移到…」受守卫：
  ①租户必须已停止；②目标节点必须已就绪；③目标节点上目标路径的数据必须已存在（`node adopt` 只登记不搬数据）；
  ④目标与当前不同。真正的搬运（停租户 → `rsync` 数据 → 修属主/权限 → `set-node`）写进部署手册，
  并在控制台弹窗里给出同样的前置条件清单。
- **不允许**：运行中迁移、跨节点共享同一个 `dsh_home`/`workspace`、把 `host_shares` 指向 `state_dir`（沿用 M71 的拒绝）。

## 8. 测试策略

1. **Go 单测（新包全覆盖）**
   - `nodeproto`：编解码、错误码、协议版本拒绝。
   - `nodeclient`：对 httptest 假节点覆盖 401/426/超时/5xx/坏 JSON/慢响应/半关闭，断言错误码映射与超时语义。
   - 控制面分派：远程租户断言目标 URL、`X-Dshgw-*`、`Host`、body 回放；本地租户逐条与今天一致（同一批用例）。
   - 头不变量：浏览器携 `X-Dshgw-Tenant`/`X-Dshgw-Node-Token`/`X-Dshgw-Browser-Session` 的请求，
     到达节点时这些头只可能是我们 `Set` 的值（逐条断言，含 WS upgrade）。
   - `registry`/`nodestore`：`node` 字段往返、旧文件（无 `node`）兼容、未知节点拒绝、原子写与 flock、
     配置与状态清单的合并优先级（身份取配置、部署信息取状态；控制台不能新增
     配置里已有的名字）。
   - `nodeserve`：令牌闸门（无/错/过期、常数时间）、租户选择、未知租户 404、未运行 503、
     `Host` 强制改写、WS 直通、`/browser-workspace/**` 拦截、请求体上限、`X-Dshgw-*` 剥离。
   - `nodedep`（假 `ssh`/`tar`，`ExecFunc` 缝）：阶段推进与幂等判定、预检缺失项报文、载荷清单、
     生成配置 YAML 的正确性（与 `config.Load` 往返校验）、单飞、失败回滚（旧版本恢复）、日志尾部有界、
     首次指纹门、`--with-packages` 默认关。
2. **配置与 doctor**：新键的严格解码（未知键报错）、六条校验、节点模式必填项、`doctor` 节点段的输出。
3. **管理面**：`internal/httpapi` 新路由的角色（viewer/admin）、参数校验、`move`/`remove` 守卫、
   通道不可用语义；MCP 目录断言（端点出现在 `admin_endpoints`，body 字段齐备）。
4. **控制台**：`internal/webui/tests/dshgw_nodes_test.mjs`（DOM 级断言：正常 / 部署中 / 部署失败 /
   节点不可达 / 管理通道不可用 / viewer 只读 / 迁移守卫七态）+ `scripts/ui-harness/dshgw_nodes.page.html`
   与 `fixtures.json` 的 `/dshgw/nodes*`、`/dshgw/tenants`，登记 `run.sh` 的 `VIEWS`/`page_for_view`；
   `make ui-check` 对源码与压缩镜像（`make ui-dist` + `UI_STATIC_DIR`）各跑一次；账户页启用弹窗的
   节点下拉加断言。
5. **真实 SSH 部署 e2e**（同机 localhost 当目标机）：`make dshgw-node-deploy-e2e`（临时密钥、临时
   `known_hosts`、临时部署目录与端口段）：`node add` → `node deploy`（真跑 `ssh`/`tar`/`systemctl --user`）
   → 就绪与健康断言 → 经该节点建租户并走通门户与 `/api` → 再次 deploy 走升级路径且租户数据保留 →
   `node remove`（默认）远端数据仍在、`purge` 才删除 → `node rotate-token` 后旧令牌 401。
6. **多机 e2e（同机两进程）**：`scripts/dshgw_node_e2e.py` + `make dshgw-node-e2e`：控制面 + 节点两进程 →
   建租户到节点 → UI 200 / `/api` 401 → worker 只在节点进程树 → 重启节点恢复 → 停节点 503
   `node_unreachable` 且门户正常 → `reconcile` 修复漂移 → 远程 `tenant remove` 清理节点数据。
7. **功能面 e2e**：远程租户的 SSH 工作区挂载（sshfs + 邮箱）、浏览器目录 FUSE 长轮询（经控制面）、
   `host_shares` 只读/可写、三插件（含终端 WS 与流式）。
8. **回归**：`make dshgw-test dshgw-sandbox-test`、`go vet ./...`、导入闸门（`internal/arch`）、
   `make dshgw-supervised-test`（单机等价）、`scripts/ui-harness/run.sh`。
9. **实测记录**（写进 §11 与 `docs/dshgw.md`）：每请求 +1 跳 LAN 的 p50/p99 增量、流式不引入缓冲的证据、
   64 MiB 请求体与 WS 直通、一次部署的总耗时与载荷大小、节点重启/控制面重启的恢复时间。

## 9. 依赖

- 不新增第三方依赖：Go 标准库 + 既有 `gopkg.in/yaml.v3`；控制台沿用原生前端。
- 全部新代码在 `internal/dshgw/**` 内，M51 的导入闸门（`internal/arch`）不变；
  aigw 侧只在 `internal/config`、`internal/dshgwsup`、`internal/localdshgw`、`internal/httpapi`、`internal/webui` 内改动。
- 运行时依赖（节点机器）：`bwrap`、Node + dsh release、模板与插件目录、`sshfs`/`fusermount3`/`/dev/fuse`（按功能开关）。
- 部署依赖（控制面 → 目标机）：ssh 密钥登录、首次指纹确认、可选 passwordless sudo（仅 `--with-packages` 与 linger）。

## 10. 分阶段实施

| 阶段 | 内容 | 完成判据 |
|---|---|---|
| P1 | `nodeproto`、`nodeserve` 骨架（健康 + 令牌闸门）、`nodeclient`、`nodestore`、配置与校验、`registry.Node` | 单机回归全绿；`dshgw node doctor` 可用；无节点时行为不变 |
| P2 | 控制面生命周期分派 + 节点本地分配表与重启恢复 + `reconcile` | 两进程 e2e 的建租/启停/恢复/迁移守卫通过 |
| P3 | 数据面（转发 + 握手委托 + `/browser-workspace/` 路由 + 失败映射 + 头不变量） | 远程租户浏览器走查 200/401、WS 与流式实测、503 语义断言 |
| P4 | 节点侧服务面（sshworkspace/browsermount/hostshare/tenant_plugins/审计回传） | §8 第 7 条四条通过 |
| P5 | `nodedep` SSH 一键部署（含升级、令牌轮换、日志）+ CLI | `make dshgw-node-deploy-e2e` 通过 |
| P6 | admin socket 扩展 + `localdshgw` + aigw 路由（含 MCP 字段）+ 角色/守卫 | §8 第 3 条通过 |
| P7 | 控制台 `/dsh-nodes` 页（含添加/一键部署抽屉）+ 账户页节点列与选择 + ui-base/ui-check + 文档回填 | `make ui-base ui-check` 通过；§11 实测记录与差异回填 |

P1–P3 完成后已经是可用的「控制面 + 一台节点」；P4 补齐功能面；P5–P7 交付「一键部署 + 管理后台」。
每阶段独立可回滚。

## 11. 实现与设计差异

> 实现已完成（P1–P7）。本节记录**与本文的偏差、原因，以及实现期间发现并修掉的真实缺陷**——
> 后者比"按计划完成"更值得留档，因为它们都是按设计写不出来的东西。

### 11.1 与设计的偏差

| 设计 | 实现 | 原因 |
| --- | --- | --- |
| §6 部署阶段列表 preflight/upload/configure/unit/start/verify | 把 configure 并入 upload/activate（生成配置在控制面完成，随载荷一起上传并原子换入），verify 并入 start 阶段（start 里做探活，systemd 起了却没跑就自动退化重试） | 分成两个阶段会让"配置写坏"与"上传中断"落在同一个回滚点上；而"起了但没跑起来"是同一件事的两个结果，分开反而容易漏 |
| §6 首次部署要人工确认主机指纹 | 同设计，但确认值从"人工抄写"变成**部署拒绝 + 报出指纹 + 一条可复制命令**（CLI 与 API 都是这个形状） | 让运维去 known_hosts 里找指纹是把人当 grep 用 |
| §4 令牌由控制面生成并下发 | 增加**哈希比对**：节点回传目标机令牌文件的 SHA256（不是令牌本身），与控制面记录一致才保留，否则轮换 | 消掉"节点有令牌、控制面没有"这类只能靠重启修复的错配 |
| §7 节点审计"尽力回传" | 实现为**游标拉取**（`audit-tail` op + 持久化游标 + 首次从当前末尾开始） | "尽力"没有可验证语义：游标让重复/丢失都可判定，并写进测试 |
| §9 控制台只有 `/dsh-nodes` 一页 | 同设计，另在**账户页加"节点"列**（落点）与启用弹窗的节点选择 | 账户页是运维最常停留的页面，"这个租户在哪台机器"不该要求切页 |
| §10 控制面在管理通道暴露 `node-*` op | **另加了 `node-deploy-status`**，且部署 op 是异步的 | 一次 ssh 安装几十秒，同步请求只会把超时搬进浏览器 |
| §8 验收脚本一条 | 两条：多机协议/数据面 35 步 + SSH 部署 17 步 | 前者证明"分布式能跑"，后者证明"一键装得上"；它们的失败模式完全不同 |

### 11.2 实现期间发现并修掉的真实缺陷

1. **本地路径同源的建户顺序 bug（P4）**：`ensureHostShares` 排在 `SandboxProfileReady` 之后，而 profile 要绑定
   host share 容器并 `EvalSymlinks` 它——**任何声明了 `host_shares` 的部署建租户都会失败**。M71 只做了 staging
   探针，没覆盖建户路径，所以它一直没被触发。本地路径同样受益，并加了回归测试。
2. **配置声明的节点没有 store 记录（P4）**：审计游标无处可存，于是每次重启都重复导入或整段跳过节点历史。
   现在为游标建一条空记录（身份仍由配置决定）。
3. **令牌模型错误（P5）**：节点记录里的 `TokenFile` 被当成"目标机上的路径"用，`loadRuntime` 会在控制面上读
   一个不存在的文件并把整个网关启动打挂。现在分成 `Deploy.TokenPath`（目标机）与 `Token`/`TokenFile`（控制面副本）。
4. **运行期不换令牌（P5/P6）**：部署或轮换之后，运行中的网关仍拿启动快照里的旧令牌——控制台按钮装完的节点
   要重启网关才可用。现在是 `nodeclient.Set.Refresh` + 监视 `nodes.json`。
5. **purge 不彻底（P5）**：升级留下的 `.prev` 不删，"彻底删除"名不副实。
6. **"user manager 接受了 unit 却不运行"（P5）**：容器、未开 linger 的会话是真实形态。start 阶段现在先探活，
   systemd 路径不通就改用 detached 启动并如实报告（`systemd=false`）。
7. **`worker_user` 回退成数字 uid（P6）**：`USER`/`LOGNAME` 都缺失时 aigw 会生成一份 dshgw load 不了的子配置，
   报错离病因很远。回退顺序改为 环境变量 → 账号数据库 → 数字（并加测试钉住）。
8. **`make ui-check` 的假失败（P7）**：没有真 firefox 的机器上，`/usr/bin/firefox` 是 snap 包装器，它打印一句
   提示后退出 0、什么都不加载，于是**每个视图都报"no report"**——看起来像页面坏了。现在先问一次 `--version`，
   不可用就带原因跳过。

### 11.3 未按计划做的部分（有意）

- **共享存储、自动放置、节点故障自动迁移、控制面 HA、通道 TLS** 仍是非目标（§D15）：它们改变的是运维模型，
  不是这一版要解决的问题。
- **指标回填**（+1 跳 LAN 的 p50/p99、64 MiB 体、WS 直通）需要真机多机环境，记在 `docs/TODO.md`，不在本会话内伪造。
- **真机 sandbox 版验收**（`make dshgw-node-e2e`）在本会话沙箱内跑不了（禁嵌套 namespace），已用
  `--passthrough-bwrap` 跑通 35 步协议与进程路径；真机复跑记在 TODO。
