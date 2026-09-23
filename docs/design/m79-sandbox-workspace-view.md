# M79 沙箱内工作区短路径视图

## 0. 需求原话

> 实现 `~` 就等于 `/home/winger/work/ai_gateway/data/dshgw-verify/state/workspaces/dsh-tenant` 呢？并在沙箱里缩短路径

前置讨论确认了两件事：沙箱里的 `/home` 是 bwrap 自己挂的**私有 tmpfs**，只有 profile 显式绑定的路径才通到宿主；
而租户工作区恰好是整个部署里最长的那条路径（`<部署根>/data/<部署名>/state/workspaces/<租户>/…`）。

## 1. 现状（实测）

| 事实 | 证据 |
|---|---|
| `~` 已经是工作区 | `runner.workerEnv` 导出 `HOME=<workspace>`；每租户渲染的 `/etc/passwd` 视图把该账号的家目录字段改成同一个值（否则 OpenSSH 的 `getpwuid()` 会落到被 `--tmpfs /home` 藏起来的宿主家目录） |
| 但读到的路径很长 | workspace 是 `state_dir` 派生的绝对路径，cwd、提示符、目录选择器面包屑、面板 root 都是它的子路径 |
| bwrap 的 `--bind` **不递归** | M64 已记录（F7）：workspace 里的 mount（browser 挂载点、`host_shares`、ssh 工作区）必须逐条显式绑定，否则在沙箱里是空目录 |
| `getcwd()` 走挂载树 | 进程的工作目录由 dentry 链重建，符号链接在 `chdir` 时就被解析掉了 —— 软链只能骗过显示层，骗不过 `process.cwd()` |
| 本机无法嵌套 bwrap | `/proc/sys/kernel/apparmor_restrict_unprivileged_userns=1`，`bwrap --unshare-pid` 退出 1 → 真机 staging 只能在宿主跑 |

## 2. 设计

### 2.1 配置（默认关闭）

```yaml
deploy:
  sandbox_workspace: /workspace   # 留空 = 保持"只有宿主长路径"这一形态
```

- 独立形态写 `dshgw.yaml`（本机就是这一形态：`$ROOT/dshgw.yaml` + 用户单元 `dshgw-verify.service`）。
- 监督形态写 aigw 的 `dshgw:` 块，由 `internal/dshgwsup` 生成子配置 —— 该文件注释里写明"子进程支持的能力
  必须显式带过去"，所以 `SandboxWorkspace` 也一并透传（`internal/config` → `cmd/aigw/dshgw_child.go`
  → `childconfig.Deploy`）。
- 多节点（M77）：profile 由承载租户的节点渲染，键写在各节点自己的配置里；给同一短路径租户体验才一致。

### 2.2 profile：两个视图、镜像子挂载、`--chdir`

长路径绑定**保留**（网关自己的状态仍在宿主侧用它），短路径是**新增**的第二个视图：

```
--bind <workspace> <workspace>          # 原样保留
--bind <workspace> /workspace           # 新增：视图
--ro-bind <ws>/browser <ws>/browser     # 原样保留
--ro-bind <ws>/browser /workspace/browser            # 镜像（同样只读，否则短路径能替换容器）
--bind <picked> <picked> / --bind <picked> /workspace/browser/<id>
…（host_shares、ssh 挂载同理，flag 完全一致）
--dev /dev --proc /proc --unshare-pid --die-with-parent
--chdir /workspace                      # 新增：进程 cwd 也落在短路径上
```

实现上把"工作区内部的绑定"收敛成一个 `bind(flag, source, target)`，它自动补一条目标为
`viewTarget(view, workspace, target)` 的镜像绑定；校验仍然只针对宿主路径。

### 2.3 租户可见的路径都走同一个 helper

`tenancy.sandboxWorkspacePath(cfg, t)`：配了视图返回视图，否则返回 `t.Workspace`。使用者：

- `workerEnv` 的 `HOME`（`DSH_HOME` 不变，见非目标）；
- `prepareTenantPasswd` 渲染的家目录字段（必须与 `HOME` 一致）；
- `pickerRows` 的 clamp root —— **这是让新会话真的变短的那一环**：选择器决定会话跑在哪条路径上；
- `webTTYRow` 的 `cwd`/`cwdRoot`、`workspaceFilesRow`/`gitDiffRow` 的 `root`；
- `renderWorkspace` 的 `workspace_seed` 路径。

宿主侧状态（registry、`ssh-mounts.json`、browser 挂载记录、备份根）**不动**：它们在宿主读写，沙箱里没有那个视图。

### 2.4 已存在租户如何生效

picker 行原本只在 create/rotate 时写入 patch，只改配置会让"老租户"永远看不到短路径。因此新增
`EnsureDirectoryPickerRow`，在 `startWorker` 里与 ssh/browser/account-card/tenant-plugins 的行一样每次启动刷新：
只改 patch 的那一对行，**不**重新渲染 artifacts（那会重写 `workspace.json`、丢掉用户在界面上加的工作区）。

其余行（终端/文件管理器/变更审阅）本来就走 `EnsureTenantPlugins`，自动跟随。

历史不迁移：`sessions/` 的目录名由 cwd 派生，长路径的老会话仍在侧栏里以长路径工作区打开（长路径照旧绑定）。

### 2.5 校验与失败模式

加载期（`config.validateSandboxWorkspace` + `sandbox.ValidateWorkspaceView`）拒绝：非绝对 / 非 clean / `/`；
位于或等于 `/home`、`/root`、`/tmp`、`/var`、`/srv`、`/etc/dshgw` 这些隐藏根；覆盖 `/usr`、`/etc`、`/bin`、
`/sbin`、`/lib`、`/lib64`、`/proc`、`/dev` 这些运行时树；与 `state_dir` / `tenant_root` / `workspace_root` /
插件目录互相包含。profile 侧再拒绝与 node / dsh release 绑定树、租户工作区本身重叠的值。

| 失败模式 | 表现 |
|---|---|
| 视图落在运行时树上 | 加载期报错（不是运行期 "execvp: No such file or directory"） |
| 忘记镜像子挂载 | 短路径下 browser/ssh/host-shares 是空目录 —— 所以 staging 断言把它钉住 |
| 旧 bwrap 不认识 `--chdir` | worker 起不来，`doctor`/staging 用真 bwrap 覆盖 |

## 3. 非目标（本次明确不做）

1. **`DSH_HOME` 短路径**：它基本不出现在界面上，改它只会把状态路径换个名字；收益与面积不成比例。
2. **历史迁移**：不改 `workspace.json` 里的既有路径、不改 `sessions/--…--` 目录名（后者要复刻 dsh 的 slug
   规则，会把 dshgw 绑到 dsh 的实现细节上）。
3. **browser-pick / host_shares / ssh 面板里显示的长路径**：那些路径来自网关侧记录，功能上两个视图都通，
   显示长度是次要问题（见 `docs/TODO.md` M79 的观察项）。
4. **符号链接方案**：`getcwd()` 会把它解析回长路径，只解决显示层。

## 4. 验收

单元（任意机器）：

- `sandbox`：配视图时 argv 含 `--bind <ws> /workspace`、`--chdir /workspace`，且 browser 容器/挂载、
  host_shares、ssh 挂载逐条镜像且 flag 相同；不配时 argv 与基线一致；校验表；与 node/dsh release/
  工作区重叠一律拒绝。
- `config`：加载期接受 `/workspace`，拒绝运行时树/隐藏根/相对/非 clean，以及与 state/tenant/workspace
  根和插件目录的重叠；未配置时默认值为空。
- `tenancy`：`HOME`、passwd 家目录、三类插件行 root、`workspace.json` 种子 = 视图；`EnsureDirectoryPickerRow`
  刷新/幂等/模式切换/无 patch 跳过/无 insert list 只告警；未配置时全部保持宿主路径。
- `cmd/dshgw`：`sandbox-exec --print` 打出的 profile 含视图绑定与 `--chdir`。
- `dshgwsup` + `cmd/aigw`：透传字段出现在生成的子配置里。

真机（宿主，沙箱内无嵌套 userns 权限）：`make dshgw-sandbox-test` 应在视图下断言 `/workspace` 是挂载点、
两个视图写同一批文件、`HOME` 与 `getent passwd` 一致、镜像子挂载可见、cwd 落在视图上、宿主树仍不可见。

生效验收（新会话）：`pwd` 在 `/workspace/…`、`echo $HOME` = `/workspace`、`ls ~` = 工作区内容、
`readlink -f /workspace` = `/workspace`、选择器/终端/文件面板根 = `/workspace`。

## 5. 影响面

| 文件 | 改动 |
|---|---|
| `internal/dshgw/config/config.go` | `deploy.sandbox_workspace` + 校验 |
| `internal/dshgw/sandbox/profile.go` | `Tenant.WorkspaceView`、视图绑定、镜像子挂载、`--chdir`、`ValidateWorkspaceView` |
| `internal/dshgw/tenancy/{worker,runner,passwd,patch,render,manager}.go` | 路径 helper 与启动期 picker 刷新 |
| `internal/config/config.go`、`cmd/aigw/dshgw_child.go`、`internal/dshgwsup/childconfig.go` | 监督形态透传 |
| `config.example.yaml`、`deploy/dshgw/{config,node}.example.yaml`、`docs/dshgw.md`、`docs/deployment-layout.md` | 文档与样例 |

## 6. 与 M77（多节点）的关系

同一份 profile 生成代码被控制面与工作节点共用，因此本特性对两种形态同时生效；差别只在配置写在哪台机器上。
本次不涉及节点协议字段（视图是**渲染 profile 的机器**的本地配置，不需要在控制面与节点之间同步）。
