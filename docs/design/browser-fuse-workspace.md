# 浏览器本机目录 → 反向请求 → FUSE 工作区

## 目标与结构

浏览器通过 File System Access API 授权本机目录，主动建立同源长轮询反向通道。Linux 网关上的 go-fuse 将目录映射为租户的真实工作区路径。**命令仍在服务器执行**，工作区文件 I/O 按需往返浏览器；用户机器无需安装代理，也不提供本机绝对路径。

```
DSH bash/read/write → sandbox 内的 FUSE 挂载
 → browserworkspace.Backend.Call
 → 按租户、登录会话、随机 capability 隔离的 browsermount 队列
 → 浏览器 poll/respond → 用户授权的 FileSystemDirectoryHandle
```

主要组成：
- `internal/dshgw/browserworkspace`：FUSE 适配器、协议、errno、inode 与缓存语义。
- `internal/dshgw/browsermount`：反向请求、多挂载生命周期、稳定目录 key、持久记录及崩溃清理。
- `cmd/dshgw/plugin/browser-workspace`：客户端目录授权、多目录文件夹列表（增删/连接/断开）、文件执行器和工作区入口；host 文件用于插件发现。
- dshgw proxy：在既有 Host/Origin、cookie、租户、会话及权限校验后分派管理请求，不依赖 worker 在线。

## 与初始计划的差别

使用 **HTTP 长轮询**，不是独立 WebSocket upgrade；它承载同样的反向文件操作，复用网关认证且避免新监听端口。不存在 WebSocket 101/426 的虚假验收声明。

本插件独立于旧 `dsh-browser-fs`。后者仍只是额外文件工具，本功能真正注册 FUSE 绝对路径工作区。限制首版采用固定安全上限，不新增所有预想的可调配置项。IndexedDB 句柄恢复已实现（2026-09-19）：保存的目录句柄与已授予的 `readwrite` 权限跨刷新/跨标签页有效，刷新后一次点击即可接回同一挂载。

## 配置与使用

独立 dshgw：

```yaml
browser_workspaces:
  enabled: true
deploy:
  plugin_path: /absolute/path/to/cmd/dshgw/plugin/picker-clamp.js
```

插件 `browser-workspace/index.js` 与 picker 同级随仓库部署。aigw 监督形态用 `dshgw.browser_workspaces.enabled`。默认关闭。

要求：Linux、可用 `/dev/fuse`、`fusermount3`（兼容清理 helper 为 `fusermount`），网关运行账号能用户态挂载；浏览器使用 HTTPS/localhost 且支持 File System Access API。**不会向租户 sandbox 增加 /dev/fuse 或权限能力。**

操作：侧栏「浏览器工作区」这一行有两个控件。**行体**是自适应的一次点击：没有已保存目录时直接弹目录选择器挂载（与最初一致）；恰好一个目录时是那个目录的连接/断开；多个目录时打开文件夹列表（「断哪一个」没有唯一答案）。**行体右侧的文件夹图标**始终打开文件夹列表。

文件夹列表（`data-dshgw-dialog="browser-workspace"`）里每个已保存目录一行：目录名、状态、`连接`/`断开`、`打开`（跳到该目录的工作区会话）与 `删除`（两步确认，不用 `window.confirm`），页脚是 `添加文件夹` 与 `关闭`。窗口是管理界面，不自关；只有行体发起的那次挂载成功后约 1.5 秒自动关闭。侧栏的浏览器工作区与 SSH 工作区仍在同一 footer slot 中上下两行（`sidebar.footer.action`，order 90 对 100），浏览器行是容器 `.dshgw-bw-row`（行体 + 文件夹图标），折叠导轨只留一个图标；状态由行注记、`data-dshgw-state`（行）与 `data-dshgw-folder-state`（每个目录行）以文字承载，不用状态点。风险提示写在行的 tooltip、窗口与状态里，不作为点击前的确认步骤——目录选择器需要 transient user activation，多一次点击或阻塞对话框都会先耗尽它。

连接/断开都会重启该账号的 worker，可能中断其他正在执行的任务。页面必须保持打开；关闭、断网或撤销权限会导致 I/O 失败，不能把浏览器目录当作永远在线的远程磁盘。

## 多目录与稳定虚拟路径（本地目录 ↔ 工作区映射）

一个账号可以同时挂载多个本机目录（网关上限每账号 4 个，见下），每个目录一条独立的 poll 连接、一个独立的内核挂载与一个独立的 DSH 工作区条目。

**挂载点由客户端提供的稳定 key 命名**：`<workspace>/browser/<key>`，key 是保存该目录时生成一次的 32 位十六进制串，存在 IndexedDB 里，重连、刷新、换标签页都不变。这一点是「工作区映射不要因为挂载 id 变化而变化」的实现基础：

- DSH 的工作区注册表按 **canonical path** 复用（`create` 幂等：同路径返回同一实体，不新建），因此固定路径 ⇒ 固定 workspaceId、固定标题、固定会话归属；会话归属还要求路径真实存在（`realpath` + `stat` 成功），所以**断开时挂载点目录保留为空目录**，路径始终有效，会话不会被从工作区成员里过滤掉。删除目录（`close{purge:true}`）才释放该目录并删除对应工作区条目（工作区注册删除只删注册，**保留目录与会话日志**）。
- 同一 key 被另一条 share 占着（正在服务或在宽限期内）时 `open` 直接拒绝（`directory key already mounted for this account`），绝不叠第二个 FUSE 挂载——两个挂载同一本机目录会让两个浏览器同时写同一份文件。宽限期内的旧记录由 tombstone 携带路径，`purge` 只在该 key 没有活挂载时才会删除目录。
- 复用已存在的挂载点目录只接受「本服务自己会创建的那种」：私有权限、非符号链接、**空目录**；有残留内容就报错而不是覆盖。
- 启动清理（`CleanupStale`）对带稳定 key 的记录只卸载、不删目录（记录照旧删除），因此网关重启不会破坏映射。

`open` 未带 key（旧客户端）时仍生成随机 48 位 id，挂载点随挂载结束删除，行为与之前完全一致。

**刷新/断线可以恢复（2026-09-19 起）**：`poll` 长轮询仍是浏览器那一端，但它死掉只意味着「暂时没人服务」，不再等于销毁挂载。网关保留内核挂载与挂载点 45 秒（`reconnectGrace`），期间同一个页面（或替代它的新标签页/新文档）用 `resume` 把**同一个挂载**接回来：同一路径、同一 worker 绑定，不重建、不重启 worker。窗口内没人回来才走原来的清理顺序（重启 worker → 卸载 FUSE → 删挂载点 → 删记录）。

两种恢复路径：

1. **页面内自动重连**（不需要刷新、不需要点击）：传输层坏了（连接被断、网关重启、代理抖动）时客户端自己重试 `resume`，行短暂显示「正在重连」，成功后回到「已挂载」。前提是挂载还在——所以它救的是「能恢复的故障」，不是「挂载被销毁」。
2. **刷新后点击恢复**（不需要重新选目录）：新的文档启动时读取 IndexedDB 里的能力令牌与目录句柄，把行置为「刷新前挂载的是 X；点击恢复」，一次点击 `resume` 接回同一个挂载。句柄与 `readwrite` 授权在真实 Chromium 中跨刷新、跨新标签页都存在且无需手势（实测），所以点击不再打开系统目录选择器。

宽限期内该挂载**立即**从所有 worker profile 排除（`serving` 与 `MountsFor` 共用同一条规则），因此等待不会让任何一个 worker 启动卡在无人应答的 FUSE 上。同一挂载同时只允许一条 poll 连接；被顶掉的旧页面会收到 `directory revoked` 并停止，而不是和新页面抢答。`resume` 只要求 capability（token+tenant+session 哈希），且必须落在同一个 tenant 与同一个登录会话上（`owner` = `sha256(会话 cookie)`，刷新后 cookie 不变，因此刷新能恢复、换浏览器或换账号不能）。

## 生命周期和崩溃恢复

- `open` 在 FUSE 挂载前持久化 preparing 记录，成功后原子更新 ready；带稳定 key 的记录标记 `Persistent`，供启动清理区分「稳定虚拟路径」与「一次性挂载点」。只记录服务端路径/租户/ID，不持久化 capability 或浏览器句柄。
- 断开（`close`）对稳定 key 只卸载并删除记录，**保留空挂载点目录**；`close{purge:true}`（操作员删除该目录）才连目录一起释放，tombstone 会记住路径，使「先断开、后删除」也能真正释放。
- HTTP close 和租约过期：先拒绝新 I/O、从 worker profile 排除路径，再重启并等待旧 namespace 退出，最后卸载 FUSE。
- **卸载的强制阶梯（M76）**：`cleanupLocked` 先走 go-fuse 的优雅卸载；失败且挂载表里仍在该条目时，升级为
  `browserworkspace.ForceUnmount`（`fusermount3 -u -z` 惰性摘除 → abort 这条 FUSE 连接 → 再 `-u -z`）。
  触发场景是实测到的：挂载被**另一个挂载命名空间**（本机是某个 snap 的私有 ns，`shared` 传播把挂载复制了进去）
  持有，普通 `umount` 永远 EBUSY，reaper 每 5 秒重试同一次必然失败的调用（本机 2026-09-22 连续刷了一小时）。
  保证的是**我方挂载表条目消失、挂载点可复用**；别的命名空间里那份副本由内核管到那个进程退出。
- **退出（点「退出」）按 M76 的顺序**：排除 → 强制卸载（本挂载 + sshfs）→ **最后**强杀 dsh worker。
  这是唯一允许"worker 还活着就先卸载"的路径，因此走上面那条强制阶梯（惰性 detach 不需要命名空间先释放）；
  其余路径（HTTP close、租约过期、停用、Shutdown）继续沿用"先释放 namespace 再卸载"的不变式。
  退出路径同时给 share 打 `final`：**不再为了收挂载而重启 worker**（`expire` 的 restart 分支对它是死的）。
- **反向通道的 poll 连接就是浏览器侧本身**：net/http 在该连接断开时取消它的 context（页面刷新/关闭/崩溃、客户端在 close 前主动 abort 长轮询），此时立即断开该挂载，而不是等到 60 秒租约到期。晚到的 poll 不会复活已断开的 capability（客户端本来就把 poll 失败当作断线并调用 close）。
- **注册工作区之前必须已经在 poll**：宿主上的 FUSE 挂载会传播进正在运行的 worker 命名空间（实测 worker 的 mountinfo 里就有 `fuse.browser-workspace`），因此 worker 自己 `workspace.create(path)` 的 realpath 会 stat 这个挂载点。客户端必须在调用注册之前就开始 poll，否则该 stat 阻塞整个 FUSE 超时，表现为「挂载失败：workspace registration timed out」（2026-09-19 真实 Chromium 验收抓到并修复）。
- **只有仍可服务的挂载才进入 worker profile**：`MountsFor` 与 `Call` 共用同一条存活规则（未断开且租约内）。profile 会解析每个挂载路径、bubblewrap 会 stat 每个 bind 源，所以一个没有浏览器的挂载会阻塞整个 worker 启动（真机实测：解析该路径耗时等于整个 FUSE 超时后失败，bwrap 也以 `Can't get type of source ...` 失败），表现为另一个挂载的 close 报
  `worker restart before close: resolve browser mount: lstat …: connection timed out`，页面显示"清理未确认"。
- 停用/删除：排除所有挂载，等待竞态 activate，raw stop 再次保证 namespace 释放；不递归调用有 mount hook 的 StopWorker。
- 网关退出：停止 reaper、Quiesce 管理操作、等待启动任务、终止 worker runner，最后清理 FUSE。terminal Shutdown 禁止晚到的 Start/Restart。
- 卸载、目录或记录删除失败会保留重试状态，不伪报成功；成功 close 使用绑定 tenant/owner 的短期 tombstone 支持重试，最多 256 条。
- 状态目录采用私有文件与整个服务生命周期的 `flock`，新实例不能误清理仍运行的旧实例。启动只清理与 registry、路径、记录文件名及内核 FUSE 类型/source 全部匹配的残留；不恢复权限，拒绝可疑记录并保留诊断。
- 同步 go-fuse Unmount 依赖管理中的 namespace 引用已经释放。管理边界以外的 namespace 持有者或内核异常仍可能阻塞，不能用遗留 goroutine 的超时包装伪装清理成功。

## 文件系统语义限制

- 支持二进制分块读写、偏移、截断、文件和目录创建、遍历、删除；单块最多 1 MiB。
- 浏览器内事务串行执行；不要在多个标签页/外部程序同时修改同一个目录并假定 POSIX 锁语义。
- rename 仅使用浏览器原生 `move`，不支持就返回 ENOTSUP，不以复制删除冒充原子操作。不同浏览器、本机目录与 OPFS 的支持范围可能不同。
- 不承诺完整 POSIX：symlink、hardlink、设备、chmod/chown、文件锁、exec 权限、inotify、打开后 unlink 及并发外部 namespace 改动等可能不支持。
- FSA 无真正原子 O_EXCL 保证，也不保证 fsync 的主机掉电持久性；返回写成功意味着浏览器事务已经关闭。
- mutation 超时可能发生在实际写入之后，不自动重放。git、构建器、包管理器等需按自身依赖的语义单独验收。

## 安全与资源边界

1. 句柄不离开浏览器，相对路径在两侧校验，拒绝无效 UTF-8、路径穿越、绝对路径、空段及控制字符。
2. 启用时 `workspace/browser` 是网关管理的真实私有目录。sandbox 将**容器只读绑定，再把活动子挂载读写绑定**，防止同 UID 租户替换容器制造检查到绑定间的 symlink 竞态。功能关闭时不修改同名普通目录。只读容器**不足以**让活动挂载可写：宿主挂载会传播进运行中的 worker 命名空间，此时它落在只读容器之下，沙箱内写入报 EROFS——所以 profile 里那条 `--bind <mountpoint> <mountpoint>`（`MountsFor` 只广告仍可服务的挂载）是必需的，且只有 activate 之后重启的 worker 才带它。
3. FUSE 不使用 allow_other，隔离仍以租户 mount namespace 为界。网关运行账号与宿主同 UID 进程是信任边界，不提供不同 UID 隔离的虚假承诺。
4. capability 绑定 tenant 和登录会话哈希；token 不进 URL、不入持久记录或日志。
5. 每租户最多 4 个（`maxMountsPerTenant`，单独报错文案，客户端译成「已达每账号 4 个目录上限」）、全网关最多 128 个挂载，每挂载最多 64 个等待请求；每挂载一条常驻 poll 长轮询，因此同源并发连接数是这个上限的现实依据（HTTP/1.1 每源 6 条）。浏览器侧最多保存 8 个目录（含未连接的）。I/O 预算 15 秒、租约 60 秒、HTTP body 上限 2 MiB；目录结果最多 10000 项且 JSON 最多 1 MiB，超限报错而非截断伪造完整目录。稳定 key 只允许 32 位小写十六进制；空挂载点目录按 key 复用，删除目录才释放。
6. 已取消的排队请求不再下发，迟到响应丢弃；断线后不依赖内核写回缓存假报成功。poll 连接断开即断开挂载（见"生命周期"），因此不会出现"浏览器已不在、网关仍在等租约"的窗口被 worker 启动撞上。
7. 全网关/租户备份排除远程浏览器内容；关闭功能时保留同名普通目录的备份。离线 CLI 检测到活动挂载时拒绝递归删除。

## 验证与交付边界

已通过源码 Go 回归、53 项 JS 测试、真实 FUSE/HTTP/sandbox、活跃 namespace 关闭及崩溃状态清理模拟、Chromium 原生 OPFS 执行器、真实 DSH 插件发现和浏览器侧栏渲染，以及多目录真机端到端（两个目录并存、逐目录断开/重连/删除、刷新后恢复）。

具体命令与证据见 `docs/browser-workspace-verification.md`（单目录挂载 29 步、断线/刷新恢复、多目录管理 22 步、真实侧栏渲染）。仍需用户环境验收 OS 目录选择/授权对话框及实际目录兼容性；race detector 因环境缺 C 编译器未运行。未部署当前在线实例，不宣称完整 POSIX 或生产环境全覆盖。
