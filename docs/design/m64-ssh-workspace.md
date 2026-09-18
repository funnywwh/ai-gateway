# M64 aigw 账号的 SSH 工作区（远端目录 → 挂载 → 该账号 DSH 里的工作区）

> 状态：**设计（阶段 0 实测已完成，结论：不做沙箱内挂载，改由 dshgw 在沙箱外挂载）**。
> 规格文档：`docs/dshgw.md`（新增一节）、`docs/deployment-layout.md`（新增 `.ssh/` 与 `ssh/`）。

## 1. 需求与定位

aigw 账号（= dshgw 租户）登录自己的 DSH 之后，能：

1. 选一台 ssh 主机（远端），浏览它的目录、**在远端新建目录**；
2. 把某个远端目录挂成本机路径，并直接作为该账号的一个工作区打开（会话在里面读写文件就是读写远端）。

边界：**只有该账号看得见、用得了**；其它账号既看不到挂载点，也拿不到它的 ssh 身份。

## 2. 已核实的事实（本机现网形态）

| # | 事实 | 证据 |
|---|---|---|
| F1 | 租户布局：`<workspace_root>/<账号>` 是会话 cwd 与 picker 的 clamp 根；`<tenant_root>/<账号>/.dsh` 是 DSH home | `internal/dshgw/tenancy/render.go:267-279`、`registry.Tenant` |
| F2 | 现网（M63 布局）实际路径：部署根 `/home/winger/work/ai_gateway`，state 根 `./data/dshgw-verify/state`。`dsh-colin` → workspace `…/state/workspaces/dsh-colin`、dsh_home `…/state/tenants/dsh-colin/.dsh`、worker 18402、公开 18303 | 运行中 worker 的 bwrap argv + `registry.json` |
| F3 | 租户 worker 的 `HOME` **就是**它的 workspace ⇒ `~/.ssh` 就是 `<workspace>/.ssh` | `internal/dshgw/tenancy/runner.go` 的 `workerEnv` |
| F4 | 沙箱内 `ssh` 可用（`/usr` 整树只读绑定），`PATH` 含 `/usr/bin` | 同上 + `sandbox/profile.go` |
| F5 | 沙箱网络命名空间**共享** ⇒ 出站 ssh 可达 | `sandbox/profile.go`（"deliberately shared: the worker must reach aigw"） |
| F6 | 租户沙箱**不能挂载**（见 §3 实测） | 阶段 0 实测 |
| F7 | bwrap 用的是**非递归** `--bind workspace workspace` ⇒ 子挂载不会自动进入沙箱 | `sandbox/profile.go`（`--bind`，非 `--rbind`） |
| F8 | 沙箱里所有租户都是同一 UID（1000）；跨账号不可见靠**路径绑定**，不是 Unix 权限 | `profile.go` 设计 + 沙箱实测（`uid=1000`、宿主 root 未映射 → 显示 65534） |

## 3. 阶段 0 实测结论：沙箱内挂载不可行（A 档否决）

在宿主上（经 `ssh 127.0.0.1`，bwrap 0.11.1、宿主 `/dev/fuse` 存在、`sshfs` 尚未安装）实测：

| 实验 | 结果 |
|---|---|
| `--dev /dev` 之后 `--dev-bind-try /dev/fuse /dev/fuse` | `/dev/fuse` 在沙箱内可见（crw-rw-rw-，宿主 root 显示为 65534） ✅ |
| 反序（先绑 `/dev/fuse` 再 `--dev /dev`） | 被 tmpfs 盖掉，不可见 ⇒ 顺序必须正确 |
| 沙箱内直接 `mount(2)`：`tmpfs` / `proc` / `fuse` | **全部 `EPERM`（errno 1）** |
| 同上 + `--cap-add CAP_SYS_ADMIN` | `CapEff=0x200000`（cap 确实置位）**仍然 `EPERM`** |
| 同上 + `--unshare-all` | 仍然 `EPERM` |
| 沙箱内 `fusermount3` | 文件显示 `-rwsr-xr-x 65534 65534`（宿主 root 未映射），`setuid(0)` → `EINVAL` ⇒ **setuid 助手在沙箱内失效** |
| 宿主 `unshare -U -m -r`（原生 rootless userns 挂载） | `write failed /proc/self/uid_map: Operation not permitted`（Ubuntu 的 AppArmor userns 限制） |
| 沙箱内身份 | `id=1000:1000`，写入宿主后属主仍是 `1000:1000`（身份映射正常，未提权） |

**判定**：租户沙箱内既没有可用的 setuid 助手、也没有能落地的 `CAP_SYS_ADMIN`（挂载命名空间不归它所有），因此 **sshfs 不能由租户 DSH 自己挂**。A 档（纯插件自挂）在本机否决；这也意味着**不需要**给租户沙箱绑 `/dev/fuse`、**不需要**加任何 capability —— M57/M58 的隔离口径保持不变，是更安全的结果。

**因此采用 B 档**：ssh（浏览/建目录/探活）在插件内、用账号自己的 `id_rsa` 完成；**挂载由 dshgw 在沙箱外执行**，并通过 profile 的逐挂载点绑定让该账号看得见。

## 4. 目录与数据布局（新增部分）

```
<state_dir>/
  ssh-mounts.json                              # 0600：tenant/host/remote/canonicalRemote/mountpoint/createdAt
  workspaces/<账号>/.ssh/{id_rsa,known_hosts,config}   # 0600/0600/0644，插件与 dshgw 共用这把身份
  workspaces/<账号>/ssh/<主机>/<远端路径>          # 挂载点（0700），只有该账号的沙箱绑定了它
  tenants/<账号>/.dsh/ssh-requests/<id>.json     # 0600：插件 → dshgw 的请求（原子写）
  tenants/<账号>/.dsh/ssh-replies/<id>.json      # 0600：dshgw → 插件的回执
```

## 5. 配置面（`internal/dshgw/config`）

```yaml
ssh_workspaces:
  enabled: false          # 关闭时：不消费信箱、不挂载、不渲染插件行（默认关闭）
  mount_subdir: "ssh"      # 挂载点前缀：<workspace>/<mount_subdir>/<host>/<远端路径>
  ssh_bin: ""              # 默认 PATH 里的 ssh
  sshfs_bin: ""            # 默认 PATH 里的 sshfs（enabled 时必须可执行，否则加载即失败）
  identity_source: ""      # 可选初始密钥源（0600）；为空时由账号自行上传
  identity_dir: ""         # 可选：按账号覆盖 <identity_dir>/<账号>
  hosts: []                # 空 = 允许手输（仍受 host 正则约束）；非空 = 白名单
  connect_timeout: 10s
  poll_interval: 2s        # 信箱轮询
  max_entries: 1000
  sshfs_options: ["reconnect", "ServerAliveInterval=15", "ServerAliveCountMax=3", "idmap=user"]
  auto_remount: true       # dshgw 启动时按状态文件重挂
```

校验（`config_test.go` / `security_test.go`）：`mount_subdir` 必须是单段相对路径（不得含 `/`、`..`）；`enabled` 时若指定 `identity_source`（或账号级 `identity_dir/<账号>`）则必须存在且 0600；可不指定以允许账号上传。`sshfs` 必须可执行；`hosts` 每项过 host 正则。

## 6. 控制通道：文件信箱（不新增端口、不新增令牌）

- 插件写请求：`<DshHome>/ssh-requests/<id>.json`（`{id, op:"open"|"close", host, remote, mountpoint?, createdAt}`，0600，临时文件 + rename）。
- dshgw 每 `poll_interval` 扫一次**在跑租户**的请求目录，处理完写 `<DshHome>/ssh-replies/<id>.json`（`{id, ok, mountpoint?, error?, restarted?}`）并删除请求。
- 选择理由：租户与网关本来就同 UID、信箱就在该账号自己的 `.dsh` 内（已被绑定）；不引入新监听面、不引入令牌、不需要鉴权协商——**跨账号越权在文件系统层面就不存在**（各自只能写自己的 `.dsh`）。

## 7. 挂载与「让该账号看得见」

`open` 的处理顺序（全部在 dshgw，沙箱外，以运行 dshgw 的账号身份）：

1. `probe` 远端（`ssh -o BatchMode=yes -o ConnectTimeout=…`）→ 取 `$HOME`；
2. 远端 `pwd -P` 规范化（去重键）；需要时 `mkdir -p` 远端目录；
3. 本机建挂载点 `<workspace>/<mount_subdir>/<host>/<远端路径>`（逐级 `0700`）；
4. `sshfs -o IdentityFile=<workspace>/.ssh/id_rsa,<sshfs_options> <host>:<remote> <mountpoint>`（**永不 `allow_other`**）；
5. 校验：挂载点可 `readdir`；写入 `ssh-mounts.json`；
6. **重启该账号 worker**（`manager` 现有 stop/start 路径）——因为 bwrap 的 `--bind` 是非递归的（F7），profile 在启动时为每个活动挂载追加 `--bind-try <mountpoint> <mountpoint>`，只有重启后才生效；
7. 写回执（含 `restarted:true`）。

`close`：`fusermount3 -u`（忙则 `-z`，回执标 `lazy:true`）→ 从状态文件删除 → 重启 worker（让绑定消失）。

挂载点的 FUSE 访问控制靠 UID：挂载进程与沙箱内进程同为 uid 1000，故沙箱内可读；其它账号不可达该路径（未绑定）。

## 8. 租户侧插件（`cmd/dshgw/plugin/ssh-workspace/`）

新增子目录（**不要**在 `cmd/dshgw/plugin/` 直接放 `package.json`：那会改变 `picker-clamp.js` 的模块判定而让租户 DSH 起不来）：

- `package.json`：`{"name":"dshgw-ssh-workspace","private":true,"type":"module","exports":{".":"./index.js","./client":"./client.js"},"dsh":{"client":{"platform":"web","inject":["@deepseek-ai/dsh-client-connection","@deepseek-ai/dsh-api-workspace-controller","@deepseek-ai/dsh-client-ui-workspace"]}}}`
- `index.js`（宿主半）：沿用 `picker-clamp.js` 的 `createRequire(DSHGW_DSH_ANCHOR)` 样板；注册自有 RPC 通道 `ctx.connection.rpc.handle("/ssh-workspace", …)`；端点：
  | 端点 | 说明 |
  |---|---|
  | `hosts` | 别名（读 `$HOME/.ssh/config`）+ 白名单 + `sshfs` 可用性 |
  | `probe` | 探活 + 远端 `$HOME` |
  | `list` | 远端目录层（面包屑、隐藏项、`truncated`） |
  | `mkdir` | 远端建目录（单段名校验） |
  | `open` | 写信箱请求；返回 `{id, state:"pending"}` |
  | `close` | 写信箱请求卸载 |
  | `mounts` | 状态文件 + 未消费回执（供 UI 提示） |
  ssh 原语全部 `execFile`（不经 shell）：host 正则 `^[A-Za-z0-9._@][A-Za-z0-9._@:-]*$` 且不得以 `-` 开头；远端路径必须绝对、无换行/NUL、`shQuote`；列目录 `cd <q> && LC_ALL=C ls -1ap`；错误码 `ssh/host-unknown|unreachable|auth-failed|invalid-path|path-not-found|mkdir-exists|mkdir-failed`。
- `client.js`（浏览器半）：手写 `window.__ModuleLoader__.load({id, factory})`（发行版无打包器，bundle 逐字节服务），仅用种子表里的 `react`（`React.createElement`，不写 JSX）；入口注册进 `sidebar.footer.action`（list 槽），对话框注册进 `shell.overlay`（list 槽，容器 `pointerEvents:"auto"`）——都是 list 槽，不碰任何 `single` 槽；流程：主机 → 远端浏览/新建目录 → 「挂载并打开」→ 提示「正在挂载，该账号 DSH 将重载」→ 轮询 `mounts`/回执 → 拿到 `<workspace>/<mount_subdir>/<host>/…` → `workspace/create` → `rename` 成 `<host>:<远端路径>` → `uiWorkspace.connectWorkspace`。
- `ssh-workspace.test.mjs`：纯函数单测（host/路径校验、`shQuote`、挂载点映射、`within` 越界矩阵、ssh config 解析、信箱请求构形、错误映射），并入 `make dshgw-test`。

`internal/dshgw/tenancy/render.go` 在 `enabled` 时追加两行 patch（host 插件 `file://…/plugin/ssh-workspace/index.js` + 其 client 半），保持 `picker-clamp` 原样。

## 9. 安全与隔离口径

- **密钥就是边界**：账号能 ssh 到哪些主机，完全由发给它的 `id_rsa` 决定。要按账号限权就用 `identity_dir/<账号>`（一账号一把）；共用 `identity_source` 等于所有账号共享同一身份 —— 文档必须写清，`hosts` 白名单只是防跑偏，不是安全边界。
- 沙箱只绑本账号路径 ⇒ 挂载点跨账号不可见（不是靠 0700 或 `allow_other`，而是靠绑定）；不启用 `allow_other`，不碰 `/etc/fuse.conf`。
- 不做沙箱内挂载 ⇒ **profile 不新增设备、不新增 capability**；M57/M58 的隔离口径不变，只多出「为活动挂载追加 `--bind-try <mountpoint> <mountpoint>`」这一条，需在 `docs/design/m57-dshgw-strict-isolation.md` 的差异节记明。
- 审计：`open`/`close`/`mkdir`/失败都写 `audit.jsonl`（tenant、host、remote、mountpoint、结果），令牌类敏感值不入日志。

## 10. 测试与验收

- **Go 单测**：配置校验矩阵；挂载点映射与 `within(workspace)` 越权矩阵（`..`、符号链接、绝对逃逸）；host/远端路径校验与 quoting；状态文件原子读写与坏文件容错；信箱请求的解析/回执/幂等（同 id 重放只处理一次）；profile argv 断言（仅多出 `--bind-try <mp> <mp>`）；密钥 provisioning 的权限（0600）与幂等。
- **JS 单测**：见 §8。
- **集成（宿主，`127.0.0.1` 作冒烟远端）**：建测试账号 → 插件端到端（hosts/probe/list/mkdir）→ `open` → 信箱被消费 → `findmnt` 出现 `fuse.sshfs` → worker 重启后沙箱内 `ls <mountpoint>` 可见且可读写 → 远端复核 → `close` → 无残留。脚本 `scripts/ssh_workspace_e2e.py`，纳入 `make dshgw-supervised-test`。
- **浏览器验收（人工）**：账号 A 门户登录 → 侧栏入口 → 建远端目录（远端 `ls -d` 复核）→ 挂载并打开 → 会话里写文件 → 远端 `cat` 复核；账号 B 的 DSH 里看不到该挂载点。
- **回归**：`go test ./...`、`make verify`、`make dshgw-test`、`make dshgw-sandbox-test`、`make dshgw-supervised-test`。

## 11. 前置、假设与风险

- **前置**：宿主装 `sshfs`（`sudo apt install -y sshfs`，需操作者执行一次）；dshgw 运行账号能非交互 ssh 到目标主机。
- **假设**：目标主机为 Linux 且 `sh/ls/pwd` 可用；`~/.ssh/config` 语法常规。
- **风险**：新建/删除挂载会重启该账号 worker（进行中的回合会中断，会话日志可 resume）——UI 必须明示；FUSE 上 `git status`/`grep` 较慢、inotify 不生效；远端掉线靠 `reconnect` 自愈，长时间不可达时卸载需 `-z`。
- **风险**：`--bind-try <mp> <mp>` 依赖 bwrap 对「绑定一个挂载点」的语义（预期包含该挂载本身）；集成测试里必须显式断言，不靠推断。

## 12. 非目标

- 不做租户沙箱内挂载（阶段 0 已否决），不做远端命令执行（沙箱内 bash 仍在宿主跑，经 FUSE 落到远端）。
- 不新增网关 ssh 凭据、不新增监听端口与鉴权令牌（信箱即通道）。
- 不改 DSH 上游、不引入前端打包器、不改 `renderWorkspace` 的既有写入路径。

## 13. §6 差异（实现后回填）

**与设计的偏差（都是实现时发现更合适的做法）**：

1. **状态可读性**：设计里插件靠遍历 `$HOME/<mount_subdir>/<host>/…` 来列举挂载，实现改成
   **网关把该账号的挂载记录镜像到 `<dsh_home>/ssh-mounts.json`（0600）**，插件读它。原因：镜像布局
   会嵌套（`ssh/<host>/<远端路径>`），沙箱内每个层级看起来都是普通目录，而挂载本身是内核状态、插件
   观察不到，靠遍历无法区分「真挂载」与「父目录」。镜像由 `EnsureIdentity`（每次 worker 启动）与
   `Open`/`Close`/`Reconcile` 之后刷新。
2. **hook 签名**：`SSHWorkspaceHook.EnsureIdentity` 多了 `dshHome` 参数，就是为了上面那次镜像刷新。
3. **`Close` 签名**：多了 `dshHome` 参数（镜像刷新需要），调用方是 `HandleRequest`（它持有 Remote）。
4. **监督形态的透传**：设计只写了 dshgw 的配置键，实现发现监督形态会**生成**子进程配置，因此额外加了
   `internal/dshgwsup.ChildConfig.SSHWorkspaces`、aigw 侧 `dshgw.ssh_workspaces` 与
   `buildDshgwChild` 的透传（含相对路径按部署根解析），并有单测钉住；否则该功能在生产形态下等于不存在。
5. **`allow_root` 也被丢弃**：设计只点了 `allow_other`；测试发现 `allow_root` 同样会放宽到其它账号，
   一并拒绝。
6. **未做（如实记录）**：设计 §5 提到的 admin socket 运维指令（`ssh-mount-list`/`ssh-mount-close`）
   没做 —— 排障目前靠 `ssh-mounts.json`、审计流与插件日志；真机验收清单里也还没有它。
7. **新增配置键**（都在 `ssh_workspaces` 下）：`mount_subdir`、`ssh_bin`、`sshfs_bin`、
   `identity_source`、`identity_dir`、`ssh_config_source`、`hosts`、`connect_timeout`、`poll_interval`、
   `max_entries`、`sshfs_options`、`disable_auto_remount`（设计里叫 `auto_remount`，实现改成 opt-out，
   因为「默认开启」无法用零值表达）。没有删除任何配置键。

## 14. 真机验收与它抓到的四个缺陷（2026-09-18）

`sshfs` 装好后跑了三档真机验收，全部通过：

| 验收 | 命令 | 结果 |
|---|---|---|
| 网关侧挂载闭环 | `make dshgw-ssh-integration` | PASS：真实 sshfs 挂载 → 经挂载读远端文件 → 写入落到远端 → 镜像记录 → 幂等重开 → 卸载干净 |
| 端到端（含沙箱可见性） | `make dshgw-ssh-e2e` | PASS（8 步）：信箱请求被消费 → `fuse.sshfs` 出现在账号 workspace 内 → **挂载与内容在沙箱内可见** → 运维密钥/aigw 配置/现网数据根均不可见 → 删除账号卸载并清干净 |
| 单元与插件断言 | `make dshgw-test` | PASS（Go 全量 + 71 + 24 条 JS 断言） |

**四个缺陷全部是假执行器/沙箱看不见、只有真机才暴露的**（每一个都补了回归断言）：

1. **ssh 参数拼接**：ssh 把 `host sh -c <script>` 用空格拼成一条命令交给远端 shell 重新解析，脚本必须
   自带引号。原来传裸脚本 → 远端实际执行 `sh -c printf`（`$0="%s"`）→ 所有调用报 printf 用法错误。
   Go 与插件两侧都修了（`ShellQuote(script)` / `shellQuote(script)`）。
2. **`ls -1ap` 的 `./`、`../`**：过滤写在「去掉尾斜杠」之前，于是这两个伪目录被当成子目录列出来。
   Go 与插件两侧都改成先剥标记再判断。
3. **运行中的注册表可能是旧的**：`serve` 的信箱轮询按注册表列账号，而账号可能由**另一个 CLI 进程**
   建出来（或任何带外改动）。轮询前先 `Reload()`（与 manager/proxy 已有的做法一致），否则该账号的
   请求会一直无人应答直到网关重启。
4. **拆除竞态 + hook 只装在 serve 进程**：`tenant remove` 常由 CLI 进程执行，它构造的 Manager 原来
   没有 SSH hook → 从不卸载就 purge → 工作区里还挂着 sshfs，`os.RemoveAll` 报 EBUSY。修法是两层：
   把服务装配提到共享的 runtime 构造里（CLI 与 serve 共用；只有 serve 对缺 sshfs 硬失败），并在
   purge 路径上整组重试 + 解挂时以「挂载点能否被移除」为真正判据（挂载表在别的命名空间还有内部引用
   时会先于目录可用性说谎）。顺带修掉一个更危险的隐含行为：`RemoveAll` 若走进仍然挂着的挂载点，
   会把**远端文件**删掉 —— 现在卸载确认在 purge 之前。

**验收状态**：Go 单测、插件两侧 JS 断言、`make dshgw-test`、`make dshgw-ssh-integration`、
`make dshgw-ssh-e2e` 全绿。仍未做的真机项（监督形态 `aigw-local.service` 上的浏览器验收、跨账号
不可见断言、`sshfs` 缺失时拒绝启动）见 `docs/TODO.md` 的 M64 小节。
