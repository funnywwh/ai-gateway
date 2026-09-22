# M76 设计文档：点「退出」后强制卸载挂载文件系统，最后强制退出 dsh

> 状态：**设计已定稿，等确认后写代码**。
> 规格：[docs/dshgw.md](../dshgw.md) §3b / §7b / §7d。
> 相关：[M67 侧栏账号行与退出](m67-dshgw-account-card.md)、[M69 登录驱动的租户生命周期](m69-login-lifecycle-and-settings-merge.md)、
> [浏览器 FUSE 工作区](browser-fuse-workspace.md)、[M64 SSH 工作区](m64-ssh-workspace.md)。
> 需求原话（2026-09-22）：「dsh 点击退出按钮后，强制 umount 使用挂载文件系统，最后强制退出 dsh」。

## 1. 目标与非目标

**目标**：租户 dsh 侧栏点「退出」（M67 的 `⏻ 退出`）之后，**该账号正在使用的挂载文件系统被强制卸载**，
**最后**该账号的 dsh 被强制结束。两条路径都要成立：租户 origin 的 `POST /dshgw/logout/`，以及门户的
`POST /logout`（后者按 `stopSignedOutTenants` 会波及这次真的撤销了会话的多个租户）。

可验收的标准：

1. 退出返回后，该账号在内核挂载表里没有残留：既没有 `fuse.browser-workspace`（浏览器本机目录），
   也没有 `fuse.sshfs`（SSH 工作区）。
2. 该账号的 worker 进程消失、worker 端口无监听、handshake 文件删除；同一浏览器里**其它租户不受影响**。
3. 卸载/停止失败**不伪报成功**：审计与日志写明是哪一条挂载、什么原因；浏览器仍然 303（会话已撤销，
   退出本身成功，沿用 M69 D6 的口径）。
4. 下次登录：SSH 工作区挂载**按记录自动重挂**（记录、挂载点目录、工作区条目都保留）；
   浏览器挂载语义不变（仍由页面上手挂，断开保留空挂载点）。

**非目标**：

- 不改 dsh 发行包、不改任何租户侧插件、不改前端（现有客户端已经在等这个 POST 并显示「退出中…」）。
- 不新增配置键：功能开关仍是 `browser_workspaces.enabled` / `ssh_workspaces.enabled`，关掉即整段跳过。
- 不做 `host_shares`（M71）：它是 bwrap 绑定，没有内核挂载，没有可卸载的东西。
- 不承诺销毁**别的挂载命名空间**里那份副本（见 §5 的限制声明）。
- 不把「停用/删除/Shutdown/备份」路径改成"先卸载后停"：那些路径继续沿用
  「先释放命名空间、再卸载」的既有不变式（见 D2）。
- 不管会话 TTL 自然过期（M69 已声明只覆盖"点退出"）。

## 2. 现状与证据（2026-09-22 本机实测）

### 2.1 触发本次改动的现场

- `.../state/workspaces/dsh-tenant/browser/ZT20Q` 自 **14:36:32** 起持续
  `fusermount3: failed to unmount … Device or resource busy`，reaper（`browsermount.expire`）每 5 秒
  重试一次，到 15:32 仍在刷（600+ 条 `browser mount expiry cleanup failed; will retry`）。
- 同一账号的 `.../dsh-tenant/ssh/aipc/home/winger/ZT20Q`（sshfs）在它的 worker **已经停掉之后**
  仍然挂着（03:50 挂上，一直没摘）。
- `state/audit.jsonl` 里 15:14 / 15:23 / 15:24 三次 `POST /dshgw/logout/` 全部是
  `logout_worker_stop_failed`，而同一秒的日志是 `tenant worker exited tenant=dsh-tenant … signal: terminated`
  ⇒ **dsh 确实停了，失败的是随后那一步卸载**（`Manager.StopForLogout` 里的
  `BrowserWorkspaces.DropTenant`）。失败原因只落成 `error_type=*fmt.wrapError`，现场无法定位。
- 该挂载被另一个挂载命名空间持有：`snapd-desktop-integration`（`mnt:[4026532797]`，
  `/proc/<pid>/mountinfo` 里有 `shared:131` 的传播副本）⇒ 普通 `umount` **必然** EBUSY，
  只有惰性 detach（`fusermount3 -u -z`）能摘掉我方挂载表条目。

### 2.2 代码里的缺口

| 缺口 | 位置 | 现象 |
|---|---|---|
| 浏览器挂载只有优雅卸载一条路 | `browsermount/lifecycle.go`（`mounted.Unmount()`） | `go-fuse Server.Unmount()` 失败即返回错误，没有 `-u -z`、没有 sysfs abort；reaper 只能每 5 秒重试同一个必然失败的调用 |
| **优雅卸载会永久阻塞（M76 实现期实测，本次的第一原因）** | `go-fuse@v2.9.0/fuse/server.go:145` | `Server.Unmount()` 跑完 `fusermount3 -u` 之后要 **等它的 serve loop 结束**，而 serve loop 只在**内核释放这条 FUSE 连接**时才结束——挂载被别人持有时它既没成功也没失败，**永远不返回**。实测（本次 e2e，worker 仍活着且挂载已 bind 进沙箱）：退出请求与**整个 reaper** 一起卡在 `WaitGroup.Wait` 上，落库只留下 worker 还在跑、两个挂载都还挂着 ⇒ 用户看到的正是"点了退出没反应、挂载还在"。原代码把升级到强制卸载写在 `Unmount()` 返回之后，而它根本不会返回 |
| SSH 挂载在退出时根本没被调用 | `tenancy/manager.go StopForLogout` | 只 drop 浏览器挂载；sshfs 挂载留到下一次 `Reconcile` / 手工断开 |
| 卸载失败会短路掉后续清理 | `StopForLogout` 的 `return err` | `workers().Stop` 报错就直接返回，挂载清理整段不执行 |
| 失败后仍可能被"为收挂载而重启 worker" | `browsermount/lifecycle.go closeContext` | `expire` 走 `workerStopped=false`，`!sh.detached` 时会 `restart`，把已退出的租户的 dsh 又拉起来 |
| 失败原因不可诊断 | `proxy.go` 审计 | `reason` 记的是错误**类型** |
| 顺序 | `StopForLogout` | 先停 dsh 再卸载；dsh 停在 FUSE 请求里时要把 20s StopTimeout 熬完 |

已具备、本次复用的能力：`sshworkspace` 的 `detach`（重试 + `-u -z` 惰性回退）、`breakWedge`
（杀 sshfs 守护进程 + `/sys/fs/fuse/connections/<minor>/abort`）、`fuseAbort`、挂载表读取；
`browsermount.CleanupStale` 也已经在用 `fusermount3 -u -z`。

## 3. 关键决策

### D1 顺序：排除 → 强制卸载 → 最后强杀 dsh

退出按用户要求的顺序执行：

1. **排除**：该账号的挂载不再进入任何 worker profile、拒绝新 I/O（浏览器侧 `disconnect()`，
   SSH 侧从 `MountsFor` 的活挂载判定中排除）；
2. **强制卸载**：浏览器 FUSE（D2 阶梯）+ sshfs（已有 `detach`/`breakWedge`）；
3. **最后强制退出 dsh**：`WorkerRunner.Stop`（TERM 20s → SIGKILL → scope 回收 → 删 handshake）；
4. **校验**：worker 不再在跑；残留挂载进审计。

**与既有不变式的关系**：`browser-fuse-workspace.md` 的「先释放命名空间、再卸载」继续适用于
HTTP close、租约过期、停用、网关 Shutdown（`TestAllClosePathsReleaseNamespaceBeforeUnmount` 钉住它）。
**只有退出这条路**允许在 worker 还活着时卸载，且必须走 D2 的强制阶梯——惰性 detach 不需要命名空间
先消失，它把挂载从**我方挂载表**摘掉，内核为仍持有引用的命名空间留旧超级块。

**为什么这个顺序更好**（而不是"先停后卸"）：dsh 若正卡在 FUSE 请求里，TERM 要熬满 StopTimeout 才能
KILL；先把挂载摘掉/中止连接会让它的 I/O 立刻失败，随后的 TERM/KILL 迅速收敛——这正是现场
"dsh 停了但挂载留着、还要每 5 秒重试"的反面。

### D2 浏览器挂载的强制阶梯（新 `browserworkspace.ForceUnmount`）

优雅卸载必须**先被限时**，否则阶梯永远不会被走到：`Mounted.Unmount()` 用 `unmountGracefully`
包成有界调用（预算 `gracefulUnmountBudget = 1s`），超时返回 `errUnmountSlow` 并记 WARN，
然后才进阶梯。被放弃的那次 `Unmount` 的 goroutine 会在**连接被释放后自己返回**——阶梯里的
abort 正是释放它的动作。这条改动推翻了 `browsermount` 原来"绝不把 Unmount 包进超时 goroutine"
的注释：那句话是在"Unmount 一定会返回"的假设下写的，而实测它**可以永不返回**；两害相权，
一个会自己结束的 goroutine 远好过一个卡死的 reaper 与一个永不回答的退出请求。

```go
// ForceUnmount detaches one browser-workspace mount even when the graceful path cannot:
// the mount table entry is what must go, and a lazy detach is what removes it while another
// mount namespace (a snap, a sandbox that outlived its worker) still holds a copy.
func ForceUnmount(mountpoint string) error
```

单次调用预算 5s，步骤：

1. 挂载表里没有该条目（`fusekernel.MountedAt`）→ 直接成功（幂等）；
2. `fusermount3 -u -z <path>`，失败回退 `fusermount -u -z`；
3. 仍在表里 → 写 `/sys/fs/fuse/connections/<minor>/abort`（minor 由 `stat(mountpoint)` 的 dev 解码，
   只读缓存属性、不会卡在坏挂载上）→ 等 ≤300ms → 再 `-u -z` 一次；
4. 复查挂载表：仍在则返回带原因的错误，交给调用方记 `leftover`；已摘掉即成功。

`browsermount.CleanupStale` 里手写的那段 `fusermount3 -u -z` 改为调用同一个函数（单一实现）。

### D3 宿主 FUSE 探测收敛到一个包（`internal/dshgw/fusekernel`）

浏览器侧需要的能力（挂载表判定、minor、abort）与 `sshworkspace/fuse.go` 已有的实现重复，
因此把**低层宿主事实**提到新包：

- `MountedAt(path) (fstype string, _ error)`、`eachMount`、`decodeMountField`（缝 `ProcRoot`）；
- `MinorAt(path) (int, bool)`、`ConnectionDir/ConnectionLive`、`Abort(path) error`（缝 `SysfsFuse`、`StatDevice`）；
- `FindDaemon(mountpoint, comm string) int`、`KillDaemon(...) bool`（沟通名参数化，sshfs 专用语义留在调用方）。

`sshworkspace` 改为调用它（`fuse.go` 里的重复实现删除，`fuse_test.go` 的 fixture 测试搬到该包），
`browserworkspace` 也用它。**重试阶梯留在各自服务里**：SSH 侧是 sshfs 守护进程 + 远端可达性，
浏览器侧是 go-fuse 服务器 + abort，两者的"最后一招"本来就不一样。

### D4 退出后不再为收挂载而重启 worker

`share` 增 `final bool`，退出路径的 `DetachTenant` 置位。`closeContext` 与 `expire` 对 `final`
的 share **跳过 restart 分支**（等价 `workerStopped=true`），只反复重试卸载并把失败记到日志。
这修掉现网"退出后 worker 被 reaper 反复拉起来/杀掉"的病态，并且仍然保留"失败可重试、
不伪造成功"的性质（`sh.cleaned` 不置位、记录保留）。

### D5 浏览器侧新增退出半边：`browsermount.Service.DetachTenant`

```go
// DetachTenant is the logout half of DropTenant: the account's browser mounts are excluded
// from every worker profile and force-detached, but no worker is started or stopped for them
// (the caller owns the worker) and a share that cannot be detached is never restarted into
// existence. Errors are aggregated per share; what could not be detached stays retryable.
func (s *Service) DetachTenant(ctx context.Context, tenant string) error
```

实现沿用 `DropTenant` 的形状：快照 → 逐个 `disconnect()` → 等 in-flight 激活（lifecycle 锁）→
逐个 `closeContext(ctx, sh, workerStopped=true, purge=false)`（此时 `final` 已置位）。
`DropTenant` 变成「先 `stopWorker`，再 `DetachTenant`」，停用/删除/Shutdown/备份语义不变。

### D6 SSH 侧新增退出半边：`sshworkspace.Service.DetachTenant`

```go
// DetachTenant detaches every mount an account owns but keeps its records: a signed-out
// account must not keep an sshfs daemon and a kernel mount alive, while the next sign-in
// re-mounts the same paths (Restore). The mount point directory and the account's mirror
// are left untouched, so the workspace entry the account sees does not change.
func (s *Service) DetachTenant(ctx context.Context, tenant string) error
```

每条记录走已有的 `detach(ctx, mountpoint, 6, 250ms)`（内含惰性回退与 `breakWedge`），
**不** `store.Remove`、**不**删挂载点目录、**不**改镜像；每条审计 `ssh-mount-detach`；失败聚合返回。

### D7 登录自动重挂：`sshworkspace.Service.Restore`

把 `Reconcile` 的单账号主体抽成 `reconcileRemote(ctx, Remote)`，新增：

```go
// Restore re-mounts the recorded mounts of one account that are not attached. It is the login
// half of DetachTenant (M76): the records survived the logout, so the account gets its
// workspaces back without another click.
func (s *Service) Restore(ctx context.Context, tenant, workspace, dshHome string) error
```

`managerOps.PrepareLogin` 在 `EnsureCredentialRef` 之后、**`EnsureRunning` 之前**调用它：
先重挂、再起 worker，worker profile 才能把活挂载绑进沙箱。失败**不阻断登录**（沿用 M69 的
fail-soft 口径，与 `login_prepare_failed` 同级），日志 + 审计 `login_mount_restore_failed`。
若此刻 worker 已在跑：**不静默重启**（不打断进行中的回合），写 WARN + 审计
`ssh_mount_restore_deferred`，该挂载在下次 worker 启动时进入沙箱，用户也可在「SSH 工作区」里
再点一次「连接」（那条路本来就会重启 worker）。

### D8 编排、预算、不短路

`tenancy.Manager.StopForLogout` 保留名字与签名，语义按 M76 重写：

```go
// StopForLogout stops a tenant's dsh because its last session signed out (M69, reordered by M76):
// exclude its mounts, force-detach them (browser FUSE and sshfs), and LAST force-stop the worker.
// It deliberately does NOT record an operator suspension.
func (m *Manager) StopForLogout(ctx context.Context, t registry.Tenant) (LogoutResult, error)
```

```go
type LogoutResult struct {
    MountsDetached int      // 成功摘掉的挂载数（两类合计）
    MountsLeftover []string // 没能摘掉的挂载点（进审计，交给 reaper / 下次登录）
    WorkerStopped  bool     // 最后一步的校验结果
}
```

- 阶段预算：浏览器卸载 ≤15s、SSH 卸载 ≤15s、worker 停止 ≤30s（`Runner.Stop` 的 TERM 20s + SIGKILL
  + scope 回收）；每阶段用独立 ctx，**任何一步失败都不再短路**——失败会聚合返回，但后续步骤照跑。
- `proxy.logoutStopTimeout` 30s → **55s**（总上界）；门户一次退出多租户时整体预算 150s，
  超出的租户记 `logout_worker_stop_skipped`（WARN + 审计），不拖住浏览器。

### D9 审计与可诊断

`proxy.LogoutStop` 接口改为返回 `LogoutResult`：

```go
type LogoutStop interface{ StopSignedOut(ctx context.Context, tenant string) (LogoutResult, error) }
```

- 新增审计：`logout_mount_detach`（数量）、`logout_mount_leftover`（逐条路径 + 原因）、
  `logout_worker_stop_skipped`、`login_mount_restore_failed`、`ssh_mount_restore_deferred`、
  SSH 侧的 `ssh-mount-detach` / `ssh-mount-restore`。
- `logout_worker_stop_failed` 的 `reason` 由 `fmt.Sprintf("%T", err)` 改成**错误正文（截断到 512 字节）**，
  且日志同时打正文——本次现场就是因为只记类型而查不下去。

## 4. 接口改动一览

| 包 | 新增 / 改动 |
|---|---|
| `fusekernel`（新） | `MountedAt`、`MinorAt`、`ConnectionDir/Live`、`Abort`、`FindDaemon`、`KillDaemon`、缝 `ProcRoot/SysfsFuse/StatDevice` |
| `browserworkspace` | 新增 `ForceUnmount(mountpoint string) error`（缝 `runUnmount`）；`fuse.go` 不变 |
| `browsermount` | `Service.detach func(string) error` 缝（默认 `ForceUnmount`）；`share.final`；`DetachTenant`；`DropTenant` 改为 `stopWorker`+`DetachTenant`；`cleanupLocked` 强制升级；`CleanupStale` 复用 |
| `sshworkspace` | `DetachTenant`、`Restore`、抽 `reconcileRemote`；`fuse.go` 改为调用 `fusekernel` |
| `tenancy` | `Manager.StopForLogout` 返回 `(LogoutResult, error)`；`SSHWorkspaceHook` 增 `DetachTenant`/`Restore`；`BrowserWorkspaceHook` 增 `DetachTenant` |
| `proxy` | `LogoutStop` 新签名；`stopSignedOutTenants` 逐条审计 + 整体预算；`logoutStopTimeout = 55s` |
| `cmd/dshgw` | `PrepareLogin` 插入 `Restore`；`StopSignedOut` 适配新返回值 |

## 5. 异常与边界

| 情况 | 行为 |
|---|---|
| 挂载被别的命名空间（本机 snap）持有 → EBUSY | 惰性 detach 成功：我方挂载表条目消失、挂载点可复用；**别的命名空间里那份副本由内核管到那个进程退出**，`abort` 让它的挂载立即报错——这是能力边界，文档明写，不宣称"彻底销毁" |
| worker 卡在 FUSE 请求里、TERM 无效 | 先卸载使 I/O 立即失败，再 TERM 20s → SIGKILL → scope 回收（`reapScope` 已有） |
| 卸载或停止任一失败 | 所有步骤继续执行；`LogoutResult.MountsLeftover` + 审计 + 日志写原因；浏览器仍 303（会话已撤销） |
| 退出时该账号还有别的窗口 | 仍按 M69 D5 **无条件**停（不变） |
| 退出与"正在进行的挂载激活"竞态 | `DetachTenant` 先 disconnect 再等 lifecycle 锁（沿用 `DropTenant` 的等待语义） |
| 退出后有晚到的 `close` / `resume` | `sh.cleaned`/`closed` + 会话已撤销 ⇒ 幂等拒绝，不会复活挂载 |
| 页面自己在退出瞬间还在 poll | 挂载被摘掉后 poll 立即结束，插件按既有逻辑把断线当断开处理；页面随后被 303 带到门户 |
| 功能关闭 | 钩子为 nil ⇒ 该步跳过，无配置改动 |
| SSH 日志里 `sshfs` 守护进程在 worker 死后仍在 | 挂载表已无该条目时只记 WARN（守护进程会随连接释放退出）；记录仍在 ⇒ 下次登录 `Restore` 可重挂 |

## 6. 测试策略

**单测（Go）**

- `fusekernel`：挂载表解析/转义、dev 解码、`Abort` 路径（fixture 目录 + 假 `stat`）。
- `browserworkspace`：`ForceUnmount` 三分支（表里没有 ⇒ 成功；优雅失败 ⇒ `-u -z` 成功；`-u -z` 失败 ⇒
  abort 后成功；仍失败 ⇒ 带原因报错）。
- `browsermount`：优雅卸载失败 → 升级到强制缝（并断言调用顺序）；强制成功 ⇒ 清理完成（记录删、
  稳定挂载点留）；强制也失败 ⇒ 记录保留 + 错误上抛；`final` 的 share 不再被 `expire` 重启 worker；
  既有 `TestAllClosePathsReleaseNamespaceBeforeUnmount` 继续成立（优雅路径仍要求命名空间已释放）。
- `sshworkspace`：`DetachTenant` 保留记录/镜像/挂载点并真的 detach；`Restore` 重挂缺失的挂载；
  `MountsFor` 语义不变。
- `tenancy`：fake 记录调用顺序（browser detach → ssh detach → stop）；某步失败后续步骤仍执行；
  钩子为 nil 时跳过；`WorkerStopped` 校验。
- `cmd/dshgw`：`PrepareLogin` 里 `Restore` 先于 `EnsureRunning`；`Restore` 失败仍登录成功且审计。
- `proxy`：`LogoutResult` 的审计行（detach / leftover / stop）；失败仍 303；门户整体预算的
  `logout_worker_stop_skipped`。

**真机**

- `browserworkspace` 集成测试新增用例：真 FUSE 挂载 + 子进程把 cwd 钉在挂载点里（让优雅卸载必然
  EBUSY）→ `ForceUnmount` 成功且挂载表条目消失。
- `browsermount` 单测新增：优雅卸载**永久阻塞**时，清理在有界预算内升级到强制卸载并完成
  （`TestABlockingUnmountIsBoundedAndForced`）。
- `make dshgw-ssh-integration` 增：挂载 → 退出式 detach → `Restore` 重挂同一路径。
- 新增 `scripts/dshgw_logout_teardown_e2e.py`（一次性实例、自带 state/端口段，不碰现网
  `data/dshgw-verify`）：真浏览器目录挂载（协议由脚本内的小型 stand-in 驱动，不需要 Chromium）
  + 真 sshfs 挂载，浏览器挂载**已 bind 进沙箱**（即优雅卸载必然失败的形状）→
  `POST /dshgw/logout/` → 断言两个挂载都离开挂载表、worker 端口关闭、审计出现
  `logout_mount_detach` + `logout_worker_stop` 且无失败行 → 重新登录断言 ssh 挂载回到同一路径。
  **实测（2026-09-22 本机）：PASS 12 步，退出请求 1.58s 返回**。
- 本机现网：重建 `bin/dshgw` → 重启 `dshgw-verify`（会短暂带走全部租户会话）→ 真实点一次「退出」，
  证据 = `/proc/self/mounts`、`ps`、`ss`、`state/audit.jsonl`、日志。

## 7. 依赖与影响面

- 新包只依赖标准库与 `log/slog` 级依赖，不引入新第三方依赖。
- 影响面：`internal/dshgw/{fusekernel,browserworkspace,browsermount,sshworkspace,tenancy,proxy}`、`cmd/dshgw`、
  `Makefile`（新验收目标）、`scripts/`（新脚本）、文档。
- 回归面：M64 SSH 工作区（挂载/断开/删除/Reconcile/备份）、M65 浏览器工作区（挂载/断开/刷新恢复/
  多目录/启动清理）、M69 退出即停与登录即起、M71 host_shares（不受影响）、M73/M74（不受影响）。

## 8. 实现与设计差异

1. **多了一条设计时没想到的原因**：go-fuse 的 `Server.Unmount()` 在挂载卸不动时会**永远阻塞**
   （§2.2 第 2 行的实测）。因此 D2 从"失败后升级"变成"**限时 1s 后升级**"，
   `browsermount` 里"绝不把 Unmount 包进超时 goroutine"的旧注释被这条事实推翻并改写（更详细的
   理由写在 `gracefulUnmountBudget` 的注释里）。这是本次修复里最要紧的一条：没有它，强制阶梯
   永远不会被执行到。
2. **`AttachedMounts`（挂载点清单）是新增的钩子方法**，设计里没写：`MountsFor` 回答的是"worker 启动
   可以 bind 什么"，一个正在拆除中的 share 故意不在里面，因此它数不出"卸载前有几个、卸载后还剩几个"。
   `LogoutResult` 的计数与残留清单都基于它。
3. **`fusekernel` 用参数化的宿主事实函数**（`MountedAt(procRoot, path)`、`ConnectionLive(..., connDir, ...)`）
   而不是一个带全局缝的结构体：`sshworkspace` 的测试缝（`procRoot`/`sysfsFuse`/`statDevice`/
   `serviceMounted`/`fuseConnDir`）因此原样保留，只有实现搬了家，测试改动接近零。
4. **`share.final` 的作用范围比设计写的窄**：它只在"share 已发布但还没 detached"时才改变行为
   （`closeContext` 的 restart 分支）。实测中那一个小时的 EBUSY 刷屏并不是它造成的（那是限时卸载缺失
   导致的），所以文档把它记成"关掉我们确知的那条重启路径"，不夸大成根因。
5. **SSH 侧新增 `detachKeeping(..., keepMountpoint)`**：原 `detach` 会在成功时删掉空的挂载点目录
   （`Close`/purge 要的行为），而退出必须保留它（账号的工作区条目指着它）。这是设计里"保留挂载点目录"
   落到代码时多出来的一个参数。
6. **验收脚本不需要 Chromium**：设计里写"复用 `browser_workspace_mount_e2e.py` 的插桩"，
   实现改成脚本内一个约 80 行的 poll 协议 stand-in（`BrowserSide`），支持 `stat`/`list`/`read`/`flush`。
   这样这份验收不依赖浏览器自动化，同时仍然走真 FUSE 挂载、真 profile 绑定与真卸载路径。
7. **`audit_path` 必须显式配置**：默认审计文件是 `state/gateway/audit.jsonl`，
   而现网 `dshgw.yaml` 写的是 `state/audit.jsonl`；脚本按现网口径显式设置，避免断言读错文件。
