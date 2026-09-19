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
- `internal/dshgw/browsermount`：反向请求、挂载生命周期、持久记录及崩溃清理。
- `cmd/dshgw/plugin/browser-workspace`：客户端目录授权、文件执行器和工作区入口；host 文件用于插件发现。
- dshgw proxy：在既有 Host/Origin、cookie、租户、会话及权限校验后分派管理请求，不依赖 worker 在线。

## 与初始计划的差别

使用 **HTTP 长轮询**，不是独立 WebSocket upgrade；它承载同样的反向文件操作，复用网关认证且避免新监听端口。不存在 WebSocket 101/426 的虚假验收声明。

本插件独立于旧 `dsh-browser-fs`。后者仍只是额外文件工具，本功能真正注册 FUSE 绝对路径工作区。限制首版采用固定安全上限，不新增所有预想的可调配置项；不实现 IndexedDB 句柄恢复或自动重新授权。

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

操作：侧栏「浏览器工作区」→（单击即打开目录选择器，无二次确认）授权目录 → 弹出状态窗口 → 网关建立 `<workspace>/browser/<random-id>` → 浏览器开始响应文件请求 → activate 验证根并重启该账号 worker → 等待 DSH 连接恢复 → 注册并打开工作区。侧栏的浏览器工作区与 SSH 工作区在同一 footer slot 中上下两行（`sidebar.footer.action`，order 90 对 100），浏览器行不用状态点，状态由行注记、`data-dshgw-state` 与状态窗口以文字承载；成功状态窗口约 1.5 秒后自动关闭，失败窗口保留供用户阅读。风险提示写在行的 tooltip、状态窗口与状态里，不再作为点击前的确认步骤——目录选择器需要 transient user activation，多一次点击或阻塞对话框都会先耗尽它。

挂载/卸载可能中断该账号其他正在执行的任务。页面必须保持打开；关闭、断网或撤销权限会导致 I/O 失败，不能把浏览器目录当作永远在线的远程磁盘。重新选择目录会创建新的挂载。

## 生命周期和崩溃恢复

- `open` 在 FUSE 挂载前持久化 preparing 记录，成功后原子更新 ready。只记录服务端路径/租户/随机 ID，不持久化 capability 或浏览器句柄。
- HTTP close 和租约过期：先拒绝新 I/O、从 worker profile 排除路径，再重启并等待旧 namespace 退出，最后卸载 FUSE。
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
5. 每租户最多 4 个、全网关最多 128 个挂载，每挂载最多 64 个等待请求；I/O 预算 15 秒、租约 60 秒、HTTP body 上限 2 MiB；目录结果最多 10000 项且 JSON 最多 1 MiB，超限报错而非截断伪造完整目录。
6. 已取消的排队请求不再下发，迟到响应丢弃；断线后不依赖内核写回缓存假报成功。poll 连接断开即断开挂载（见"生命周期"），因此不会出现"浏览器已不在、网关仍在等租约"的窗口被 worker 启动撞上。
7. 全网关/租户备份排除远程浏览器内容；关闭功能时保留同名普通目录的备份。离线 CLI 检测到活动挂载时拒绝递归删除。

## 验证与交付边界

已通过源码 Go 回归、20 项 JS 测试、真实 FUSE/HTTP/sandbox、活跃 namespace 关闭及崩溃状态清理模拟、Chromium 原生 OPFS 执行器、真实 DSH 插件发现和浏览器侧栏渲染。

具体命令与证据见 `docs/browser-workspace-verification.md`。仍需用户环境验收 OS 目录选择/授权对话框及实际目录兼容性；race detector 因环境缺 C 编译器未运行。未部署当前在线实例，不宣称完整 POSIX 或生产环境全覆盖。
