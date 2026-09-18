# Browser workspace 实现与验证记录

## 实现范围

- 浏览器读写授权、二进制 FSA 执行器、真实 DSH 插件发现与侧栏入口。
- 独立于 worker 的同源认证 HTTP 长轮询反向通道。
- Go FUSE、只读管理容器 + 读写子挂载、DSH 工作区注册与连接。
- 默认关闭配置、独立/监督启动、停用/删除/备份保护。
- tenant/session capability 绑定、限额、超时、过期队列及迟到响应处理。
- worker 重启后的客户端重连、失败清理重试、持久残留记录与单实例锁。
- 安全卸载顺序、进程生命周期串行化与 terminal Shutdown。

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
- Chromium 实际侧栏按钮「挂载本地目录（读写）」可见可用，相关脚本 URL 1 个，JS exception/console error/plugin error 均为 0。
- UI 冒烟没有点击按钮或建立 backend 挂载；它验证真实加载与 UI 注册，不冒充整段授权流程。
- 临时 DSH/Chromium 进程、profile 和工作目录全部清理，没有改动安装目录或现网配置。

## 测试发现并修复的关键问题

- Setattr 方法签名缺 FileHandle，使 truncate 落到默认 ENOTSUP；现所有 Node 接口有编译期断言。
- 零 TTL Lookup 替换 inode，使 rename 后打开句柄访问旧路径；现保留 inode 身份并从树推导实时路径。
- strict JSON Entry 字段不匹配、零写误报完整成功、新建文件缺 DIRECT_IO、无效 UTF-8 经 JSON 替换后可能路径别名。
- 已取消 mutation 留在队列、迟到响应断开连接、UI 清理失败假报成功。
- Unmount 在 worker namespace 尚持引用时等待；现关闭、租约、停用、退出统一按正确顺序释放引用。
- browser 父目录检查到绑定间的 symlink 竞态；现只读容器与读写子挂载分层保护。
- 全网关备份遍历浏览器目录及关闭功能误漏同名普通目录；均有正反测试。

## 交付限制

实现与自动化验证完成，功能仍为默认关闭的实验性能力，**未部署或重启在线实例**。

用户环境仍需人工走通 OS 目录选择/授权撤销及真实 DSH 会话操作；不同浏览器和本机文件系统不保证完整 POSIX，尤其 move、原子创建、权限、链接、锁与打开后 unlink。Race detector 未运行：CGO 开启后仍缺 gcc/cc/clang。生命周期之外的 namespace 引用/内核异常可能使同步 go-fuse Unmount 阻塞。

详见 `docs/design/browser-fuse-workspace.md`、`internal/dshgw/browserworkspace/README.md` 与 `cmd/dshgw/plugin/browser-workspace/README.md`。
