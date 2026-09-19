# Browser workspace 实现与验证记录

## 实现范围

- 浏览器读写授权、二进制 FSA 执行器、真实 DSH 插件发现与侧栏入口。
- 独立于 worker 的同源认证 HTTP 长轮询反向通道。
- Go FUSE、只读管理容器 + 读写子挂载、DSH 工作区注册与连接。
- 默认关闭配置、独立/监督启动、停用/删除/备份保护。
- tenant/session capability 绑定、限额、超时、过期队列及迟到响应处理。
- worker 重启后的客户端重连、失败清理重试、持久残留记录与单实例锁。
- 安全卸载顺序、进程生命周期串行化与 terminal Shutdown。

## 真实 Chromium 端到端挂载验收（2026-09-19）

```sh
go build -o /tmp/dshgw-e2e ./cmd/dshgw
python3 scripts/browser_workspace_mount_e2e.py --dshgw /tmp/dshgw-e2e
```

一次性 dshgw（私有端口 13xxx、私有状态目录、stub aigw）+ 真实 headless Chromium，走完
「一次点击 → 目录选择 → open → 反向长轮询 → 真实 go-fuse 挂载 → 工作区注册 → activate 重启
worker → 侧栏已挂载 → 双向 I/O → 卸载 → 删除账号」。实际结果 **PASS：23 步**。

| 证据 | 观测 |
|---|---|
| 浏览器侧授权 | 真实 CDP 输入事件：选择器恰好被调用 1 次、参数 `{mode:'readwrite'}`，调用瞬间 `navigator.userActivation.isActive === true` |
| 唯一被替换的环节 | `showDirectoryPicker` 换成**真实 OPFS `FileSystemDirectoryHandle`**（子目录 `picked`，`queryPermission` = granted）。其后的权限查询、FSA 执行器、反向长轮询、FUSE、sandbox 绑定全是生产路径 |
| 同源网关契约 | 页面内实测：`open:200, poll:200, respond:200, activate:200 …` |
| 内核挂载 | `<workspace>/browser/<48hex>` 在 `/proc/self/mounts` 里是 `fuse.browser-workspace`，状态记录 `ready` |
| 浏览器 → 服务器 | 挂载前由浏览器写入的 `from-browser.txt`，经 FUSE 读回内容一致 |
| 服务器 → 浏览器 | 宿主写入 `from-host.txt`，浏览器侧 FSA 读回内容一致 |
| 沙箱内可见 | 用**运行中 worker 自己的 argv**（`/proc/<pid>/cmdline`，不是 CLI 的 `sandbox-exec --print`）执行命令：profile 含 `--bind <mountpoint> <mountpoint>`；沙箱内 `ls` 列出两个文件、`cat` 读到浏览器内容、写入 `from-sandbox.txt` 成功 |
| 沙箱 → 浏览器 | 沙箱写入的文件在浏览器侧读到 |
| 沙箱没有变宽 | 操作员的 `~/.ssh`、`aigw/config.yaml`、`data` 在沙箱内不可见 |
| 卸载 | 再点一次：侧栏「已断开」、内核挂载消失、挂载点被删除、浏览器目录与其文件仍在 |
| 删除账号 | `tenant remove -purge` 后 workspace 消失 |

**不夸大**：OS 目录选择/授权对话框仍无法自动化——它是唯一被替换的环节，替换物是真实 OPFS 句柄而非
假对象；未部署或重启在线实例；只证明本机 Linux + 本机 Chromium + 本机 DSH 版本这一组合。
沙箱内 profile 必须取运行中 worker 的 argv：`sandbox-exec --print` 是另一个进程，它自建的
browser-mount 服务里没有 share，因此永远看不到活动挂载（先前用它会得出"没有绑定"的错误结论）。

## 端到端验收发现并修复的三个缺陷（2026-09-19）

前两个只有「真实 GUI 里真点一次」才会暴露；第三个连宿主侧 `ls` 都失败。三个都先复现、再修、再回归。

1. **插件缺 `remote` 注入**：`client.js` 只声明 `inject: ['slots','connection','remote.workspace','uiWorkspace']`，
   代码却读 `ctx.remote.workspace.*`。真实 ctx 是 Cordis 代理，读未声明的父服务直接抛
   `cannot get property "remote" without inject` → 首次点击立即「挂载失败」，随后 close 卸载。
   修：补 `'remote'`（与 `dsh-api-workspace-controller` 的 `inject = ["remote","remote.workspace"]` 一致），
   并在 `ui.test.mjs` 加注入表回归断言——mock ctx 直接交出 `remote` 对象，原先无论如何测不出。
2. **注册工作区之前必须已经在轮询**：宿主上的 FUSE 挂载会**传播进正在运行的 worker 命名空间**
   （实测 `/proc/<node pid>/mountinfo` 含 `fuse.browser-workspace`，`master:135`），
   于是 worker 自己 `workspace.create(path)` 的 realpath 会 stat 该挂载点；而客户端原先在注册成功
   之后才 `poll()`，没人应答 → 该 stat 阻塞整个 FUSE 超时（实测宿主无 poller 时 stat 15 秒后 ETIMEDOUT）
   → 10 秒后「挂载失败：workspace registration timed out」。
   修：把 `void poll(share)` 提到 `createWorkspace` 之前，`ui.test.mjs` 增加 `poll < create` 断言。
3. **`list` 应答被整条拒绝**：浏览器执行器每个目录项都带 `{name,kind,size,lastModified}`，
   而 Go 侧 `fs.Entry` 只有 `Name`/`Kind`，而 respond 用 `DisallowUnknownFields` 解码 →
   应答被丢弃、FUSE readdir 等到超时，宿主与沙箱内 `ls` 都是 `Connection timed out`。
   修：`Entry` 补 `Size`/`LastModified`。Go 单测用 mock backend（只回 name/kind）、JS 单测不经过
   网关解码，所以这条只有真实浏览器能暴露。

## 回归

```sh
source scripts/goenv.sh
go test ./cmd/... ./internal/... ./pkg/... ./examples/... -count=1 -timeout 180s
make dshgw-browser-test
```

源码 Go 包回归通过，插件 **20 项 Node 测试通过**。`git diff --check`、相关包 `go vet` 通过。

架构测试原先内部 `go list ./...` 遍历部署 data/node_modules 超时，现限定四个源码根并加入 context timeout，原允许依赖表不变，错误仍会失败而非 skip。使用显式源码根是为了避免工作目录部署数据影响发现。

前期 worker 诊断输出测试曾偶发失败，独立重复 5 次和后续完整回归通过；未删断言掩盖。新增生命周期/清理并发测试进行了重复运行，但不能替代 race detector。

## 真实内核挂载、反向 HTTP、sandbox 与清理

```sh
source scripts/goenv.sh
BROWSERWORKSPACE_SANDBOX_TEST=1 BROWSERWORKSPACE_FUSE_TEST=1 \
DSHGW_NODE=/path/to/node DSHGW_DSH_ROOT=/path/to/dsh \
go test ./internal/dshgw/browsermount ./internal/dshgw/browserworkspace \
  -run 'TestReal' -v -count=1 -timeout 90s
```

实际结果全部 **PASS**：

| 测试 | 证据范围 |
|---|---|
| `TestRealFUSEMount` | 二进制 I/O、mkdir/truncate、文件与父目录 rename、嵌套已打开句柄、O_EXCL/O_TRUNC、不支持 chmod/utimes、外部修改、断线、超时 |
| `TestRealReverseHTTPFUSE` | open/poll/respond/activate + 真实 FUSE，文件内容落到模拟客户端目录 |
| `TestRealBrowserMountInsideSandbox` | 使用真实 sandbox.Profile 与 bwrap；只读容器不可写，FUSE 子挂载读写有效；无 /dev/fuse |
| `TestRealBoundClose` | shell 读取 FUSE 后发 READY，并保持 namespace 存活；关闭 callback 停止并等待 shell，随后 Unmount 完成；严格版 PASS 约 0.13s |
| `TestRealCrashCleanup` | 保留真实内核挂载+记录+registry，模拟失去内存所有权，安全清理有效记录，拒绝恶意记录 |
| `TestRealDeadBrowserMountLeavesProfile` | 真实 FUSE 挂载 + 浏览器侧消失（poll 连接断开）：该路径不进入 `sandbox.Profile`，随后的 close 真正卸载并删除挂载点 |

另外，两项 sandbox staging 测试验证真实只读容器不能 mkdir/rename/remove/替换，真实 FUSE 子挂载内部仍能写入。

**不夸大证据**：上述对端是 Go mock filesystem，crash 是所有权丢失模拟，不是实际 SIGKILL gateway；所有测试使用临时路径，结束卸载，mountinfo 无遗留 browser-workspace 挂载。

## 真实 Chromium 原生文件系统执行器

```sh
python3 scripts/browser_workspace_native_test.py --chrome /path/to/chromium
```

**PASS**：临时 browser profile、loopback 安全 origin、原生 OPFS FileSystemDirectoryHandle，验证插件二进制/偏移读写、截断、目录元数据、原生 move、删除、越界和只读拒绝。

OPFS 不是 fake handle，但此测试**没有点击 OS 目录选择与权限对话框**，也没有授权任何用户目录。只改动临时 browser profile 并在退出清理。脚本需要 Python 3、`websocket-client` 和已安装 Chromium。

## 真实 DSH 插件发现与浏览器 UI

```sh
DSHGW_BROWSER_DSH_TEST=1 \
DSHGW_NODE=/path/to/node DSHGW_DSH_ROOT=/path/to/dsh \
go test ./internal/dshgw/contract \
  -run TestBrowserWorkspacePluginRealDSH -v -count=1 -timeout 90s

python3 scripts/browser_workspace_ui_smoke.py \
  --node /path/to/node --dsh-root /path/to/dsh --chrome /path/to/chromium
```

均 **PASS**：
- 临时 web profile 加载插件，首页 boot manifest 精确找到一个 entry；实际广告 JS URL HTTP 200、MIME 和 ModuleLoader 内容正确。
- Chromium 实际侧栏行「浏览器工作区」可见可用（与 ssh 工作区同一形态，排在其上方），相关脚本 URL 1 个，JS exception/console error/plugin error 均为 0。
- UI 冒烟会用 CDP 发**一次真实可信点击**（先按真人做法关掉 DSH 自己的两个首启弹窗：测试须知与「添加 API Key」），
  点击前把 `showDirectoryPicker` 换成探针：断言这一次点击恰好到达选择器一次、参数为 `{mode:'readwrite'}`，
  且调用发生时 `navigator.userActivation.isActive` 仍为 true（即点击的 transient user activation 没有被任何确认步骤耗掉）；
  探针随后以 `AbortError` 模拟用户取消，行状态必须不变、无 JS 报错。它不驱动真实 OS 对话框、不建立 backend 挂载，
  也不冒充整段授权流程。
- 临时 DSH/Chromium 进程、profile 和工作目录全部清理，没有改动安装目录或现网配置。

## 测试发现并修复的关键问题

- **UI 形态（用户要求，2026-09-19）**：侧栏入口原先是"第一次点击出风险文案、第二次点击才开选择器"的按钮，
  文案直接写在按钮上。现改为与 ssh 工作区同一形态的紧凑行（图标 + 「浏览器工作区」+ 截断的状态注记，
  `sidebar.footer.action` order 90，排在 ssh 的 100 之前），**单击即打开目录选择器**：风险提示改由该行的
  tooltip 与状态注记承载，不再作为点击前的确认。理由是可测的——`showDirectoryPicker()` 需要 transient user
  activation（Chromium 约 5 秒），任何前置点击或阻塞对话框都会先把它耗掉；真实 Chromium 冒烟现在直接断言
  "一次点击 → 一次选择器调用，且调用时 activation 仍有效"。
- Setattr 方法签名缺 FileHandle，使 truncate 落到默认 ENOTSUP；现所有 Node 接口有编译期断言。
- 零 TTL Lookup 替换 inode，使 rename 后打开句柄访问旧路径；现保留 inode 身份并从树推导实时路径。
- strict JSON Entry 字段不匹配、零写误报完整成功、新建文件缺 DIRECT_IO、无效 UTF-8 经 JSON 替换后可能路径别名。
- 已取消 mutation 留在队列、迟到响应断开连接、UI 清理失败假报成功。
- Unmount 在 worker namespace 尚持引用时等待；现关闭、租约、停用、退出统一按正确顺序释放引用。
- **真机反馈（2026-09-19）**：关闭一个挂载时，同租户另一个"浏览器已经不在、但还没到 60 秒租约"的挂载
  仍在 worker profile 里，重启 worker 解析该路径时卡满 FUSE 超时并失败
  （`worker restart before close: resolve browser mount: lstat …: connection timed out`），
  页面因此显示"清理未确认"，该租户 worker 也停了约 30 秒。宿主实测同一形态：解析一个无人应答的挂载点
  耗时等于整个 FUSE 超时后报 EIO/ETIMEDOUT，bubblewrap 自己的 `--bind` 也会
  `Can't get type of source …: Input/output error` —— 两者都在 worker 启动路径上，所以唯一安全的做法是
  **不把这个挂载写进 profile**。修法：`MountsFor` 与 `Call` 共用同一条存活规则（未断开且租约内），
  并且 poll 连接断开即断开挂载（net/http 在页面刷新/关闭/崩溃或客户端 abort 时取消该 context），
  不再等满租约。回归：`serving_test.go` 三条用例在修复前失败、修复后通过（已实测对照），
  另加一条真实 FUSE 的 gated 用例。
- browser 父目录检查到绑定间的 symlink 竞态；现只读容器与读写子挂载分层保护。
- 全网关备份遍历浏览器目录及关闭功能误漏同名普通目录；均有正反测试。

## 交付限制

实现与自动化验证完成，功能仍为默认关闭的实验性能力，**未部署或重启在线实例**（本仓库 `bin/dshgw`
也仍是修复前的构建；端到端验收用 `go build -o /tmp/dshgw-e2e ./cmd/dshgw` 的产物，以免在线实例
重启时先拿到半验证的二进制）。

端到端挂载已由真实 Chromium 证明（见上），但仍有：用户环境需人工走通 OS 目录选择/授权撤销及真实
DSH 会话操作；不同浏览器和本机文件系统不保证完整 POSIX，尤其 move、原子创建、权限、链接、锁与
打开后 unlink。Race detector 未运行：CGO 开启后仍缺 gcc/cc/clang。生命周期之外的 namespace
引用/内核异常可能使同步 go-fuse Unmount 阻塞。

详见 `docs/design/browser-fuse-workspace.md`、`internal/dshgw/browserworkspace/README.md` 与 `cmd/dshgw/plugin/browser-workspace/README.md`。
