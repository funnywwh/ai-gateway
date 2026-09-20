# M58 aigw 监督的 rootless dshgw（同目录、按需随主程序拉起）

## 1. 需求（用户原话与确认结论）

原话：

> 「重新整理一下需求，dshgw 放在 aigw 同目录，aigw 是主程序，只有用到 dshgw 才把他拉起来」
> 「或者 dshgw 由 aigw 启动时拉起来」

逐项确认后的口径：

| 需求 | 结论 |
|---|---|
| 同目录 | `dshgw` 与 `aigw` 放在同一个目录（本地 `bin/`；部署目录同理），不再有独立的 `/opt/dshgw` 安装根 |
| aigw 是主程序 | aigw 是唯一常驻入口与生命周期负责人；dshgw 从"独立系统服务"降为它管理的**子进程** |
| 拉起点 | **aigw 启动时拉起 dshgw**（用户确认"由 aigw 启动时拉起来"），不做 per-request 懒加载 |
| 拉起方式 | aigw `fork/exec` dshgw 作为自己的子进程：**同一 UID、无 root、无 systemd** |
| 停止策略 | **只随 aigw 退出而停**，运行期间不自动停（不做 idle 超时） |
| 特权模型 | **接受彻底 rootless**：不要 root 安装、不要共享服务账号，全部跑在当前用户下 + bwrap 隔离 |

## 2. 目标拓扑

```
aigw（主程序，当前用户 euid）
 ├─ HTTP :8088                     数据面 + 控制台（现有）
 ├─ 子进程：<aigw 同目录>/dshgw     由 aigw 拉起并监督，同 UID
 │    ├─ 多租户代理 + 门户（租户端口）
 │    ├─ 本地 API（UNIX socket，同 UID 校验）← 控制台「启用/停用 DSH」用它
 │    ├─ registry / handshake / 租户状态（都在 aigw 自己的数据目录下）
 │    └─ 每租户 worker：bwrap 子进程（M57 profile），同 UID，**无 systemd 单元、无 per-tenant 账号**
 └─ 插件子进程（已有 internal/pluginhost 机制，形态不变）
```

要点：

- **一个 UID**：aigw、dshgw、所有租户 worker 都是同一个非 root 用户（本地即 `winger`）。
- **隔离只来自 mount namespace**：租户之间、租户与宿主之间由 M57 的 bwrap profile 分隔（空 tmpfs 根 + 逐路径绑定 + 只读运行时 + 权限位）。**没有 UID 边界**。
- **零特权**：安装 = 拷贝两个二进制到同一目录；运行 = 一个前台/用户级单元；建户/删户不再 `useradd`/`userdel`/`chown`/`systemctl`。

## 3. 与现状的差异（M51/M52/M57 → M58）

| 方面 | 现状 | M58 |
|---|---|---|
| dshgw 进程 | `dshgw.service` 系统单元，`User=dshgw`，`Restart=always` | aigw 的子进程，同 UID，随 aigw 退出 |
| dshgw 安装位置 | `/opt/dshgw/bin/dshgw` | 与 aigw 同目录（`bin/` 或部署目录） |
| dshgw 配置 | root 所有 `/etc/dshgw/config.yaml`（0640 `root:dshgw`） | **由 aigw 生成**到自己的数据目录，aigw 是唯一配置来源 |
| 租户状态 | `/var/lib/dshgw`（`root:dshgw` 0751）、`/srv/dsh/<t>` | aigw 数据目录之下，当前用户所有 |
| worker | `dsh-worker@<t>.service` 系统单元（User=dsh-<t> 或共享账号） | dshgw 的 bwrap 子进程（M57 profile） |
| 建户 | root：`useradd` + `chown` + `systemctl` | 无特权：建目录 + 生成配置 + 起子进程 |
| provisioning 通道 | `dshgw-admin.service`（**root**）+ `/run/dshgw/admin.sock` | 同 UID 的本地 socket（root 校验取消，改同 UID 校验） |
| 资源限额 | systemd `MemoryHigh/MemoryMax/CPUQuota/TasksMax` + slice 汇总 | **先放弃**（文档标注；后续可用 cgroup v2 可选补回） |
| 冷启动延迟 | 常驻，无 | 随 aigw 启动一并起（用户已确认不需要 per-request 懒加载） |

## 4. 安全影响（必须写清，不掩饰）

1. **没有 UID 边界**：所有租户 worker 与 aigw 同 UID。租户数据属主 = 当前登录用户，
   因此任何以该用户运行的进程都能读全部租户的 `.dsh/.credentials.yaml`（含各自 aigw Key）。
   M51 记录过的"root 与网关账号可冒充租户"在这里扩大为"**宿主登录用户即全部租户**"。
2. **宿主账号的爆炸半径就是隔离失效时的地板**：本机 `winger` 在 `sudo`、`docker`、`lxd` 组里，
   所以 namespace 边界一旦失守，逃逸进程等价于 root。共享服务账号（`dshgw`，nologin、无附加组）
   原本提供的地板在这里没有了。
3. **配置文件不再由 root 拥有**：profile 策略（bwrap 路径、挂载白名单、worker 账号）
   现在是"与 worker 同 UID 的文件"，能改它的东西也能放宽沙箱。
4. 因此本形态适用于：**单用户自用 / 本地开发 / 可信单租户环境**。
   对外多租户生产仍应使用 M51+M57 的形态（root 安装 + `dsh-worker-bwrap@.service` + 共享服务账号）。
   这一条必须同时写进 `deploy/dshgw/README.md` 与 `docs/dshgw.md`。

## 5. 实施阶段

### 阶段 1：aigw 监督 dshgw（本文件的主要交付）

- `internal/dshgwsup`：查找同目录 `dshgw`、生成子进程配置、`fork/exec`、ready 探测、
  日志转发到 aigw 日志、退出传播、有限重启。
- aigw 配置新增 `dshgw.enabled` / `dshgw.binary` / `dshgw.state_dir` / `dshgw.auto_start`，
  以及生成 dshgw 配置所需的端口/门户字段；默认**关闭**（不改变现有部署行为）。
- `cmd/aigw/main.go`：启动时（HTTP 就绪后）拉起 dshgw；`ctx.Done()` 时先停 dshgw 再走原有优雅关闭。
- 控制台「启用/停用 DSH」的目标 socket 改为本实例的 socket（同 UID）。

### 阶段 2：dshgw 的 rootless runner

- worker 不再走 systemd：`bwrap` + M57 profile 直接作为 dshgw 的子进程（启动/停止/重启 = 进程管理）。
- 去掉 `useradd/userdel/chown/runuser`；worker 运行 UID = 当前 euid；目录 0700 由当前用户拥有。
- readiness/URL 捕获：从"`systemctl show MainPID` + `journalctl`"改为**直接读子进程输出**。
- `doctor` 的 bwrap 检查保留；systemd 相关检查在 rootless 形态下改为不适用而非失败。

### 阶段 4：删除 root 时代的代码（清单与门禁）

用户要求「把之前 root 那套方案的不用的代码清理干净」。前提事实（实测）：这些代码**目前仍在为
线上供能**（本机 `dshgw.service` + 5 个 `dsh-worker@*.service` 租户），所以删除必须按依赖排序，
每一步都要先有替代件、再删、并保持 `go build` / `make dshgw-test` / 本机端到端为绿。

| 删除目标 | 规模（实测） | 门禁（先满足才能删） |
|---|---|---|
| 每租户 OS 用户（user 模式）：`useradd`/`userdel`/`runuser`、`VerifyWorkerAccess` 的 runuser 探针、`DshUserPrefix`、`checkSharedTraversal`、`deploy/dshgw/dsh-worker@.service` | 9 处调用 + 单元 | 阶段 2 的 bwrap 子进程 runner 落地；本机既有租户 `tenant re-isolate --to bwrap` 验证通过 |
| systemd 单元管理：`unit()`/`unitForIsolation`、`Status`/`Enable`/`StartWorker`/`StopWorker`、25 处 `systemctl`、`deploy/dshgw/{dshgw.service,dsh-workers.slice}`、`verify-dshgw.sh` 的单元解析、`deployment_test.go` 的单元断言 | 25 处 + 2 单元 + 测试 | 阶段 2 runner 提供等价的 start/stop/状态；registry 增加"期望运行"字段（原来靠 systemd enablement 表达） |
| root CLI 与提权：`requireRoot`（14 处）、`tenant create/rotate/remove` 的 chown/systemctl 分支、`backup`/`upgrade-dsh` 的 systemctl 停机、`capture-url` 的 `journalctl` 取 URL | 14 处 + `upgrade_stub.go`(30) + `backup.go`(485) 部分 | 阶段 2：URL 改为读子进程输出；备份/升级改为进程停机 |
| root admin 通道：`admin-serve` 与其 service、`admin_socket`/`admin_allowed_uids` 的 root 校验、`internal/localdshgw` 的 peer-UID 允许表 | `admin_serve.go`(378) + 单元 + 配置 | 阶段 1：aigw↔子进程改为**同 UID** 本地 socket，协议与客户端可原样复用 |
| 宿主安装：`deploy/dshgw/install.sh`、`dshgw.logrotate`、`/opt/dshgw`+`/etc/dshgw`+`/var/lib/dshgw` 布局、`/opt/dsh` 复制逻辑 | `install.sh`(4874B) + logrotate | 阶段 3：无特权安装 = 两个二进制同目录；dsh 运行时路径改为配置/发现 |
| nginx 边缘：`tenancy/render.go`(376)、`InstallNginx`、`render-nginx`/`migrate-nginx`、32 处 nginx 引用、`proxy/nginx_test.go`(656) | ≈1000 行 + 测试 | **待定**：rootless 下谁终止租户门户 TLS（见 §6.2）——若由 aigw/dshgw 直连则全删；若保留 nginx 则只删渲染逻辑的 root 部分 |
| root 验收脚本：`scripts/dshgw_host_acceptance.py`(864)、`scripts/test_dshgw_host_acceptance.py`(641) 中基于 UID/systemd 的断言 | ≈1500 行 | 阶段 3：改写成"无特权 + bwrap 子进程"的验收（UID/cgroup/slice 断言失去对象） |

删除顺序：阶段 2 → 删 user 模式与 systemd 管理 → 阶段 3 → 删 root CLI、admin 通道、install.sh
→ nginx 按决定删除或收窄 → 最后改验收脚本。**在阶段 2 落地前，任何删除都会让仓库无法运行 dshgw。**

### 阶段 3：验收与文档

- 本机端到端（**不碰现有 systemd 部署**：另一套端口 + 另一个数据目录）：
  aigw 启动 → dshgw 子进程起来 → 建一个租户 → 租户 worker 是 bwrap 子进程 → `/api` 401、门户可登录；
  全程无 root、无 `dsh-*` 账号、无新 systemd 单元。
- 文档：本文件 + `docs/dshgw.md`/`deploy/dshgw/README.md` 增补"两种形态"的对照与选择建议；
  `docs/TODO.md` 记录未决项。

## 5b. 资源限额（实测与取舍）

每 worker 的限额不再来自 systemd 单元，而是**每个 worker 一个 systemd 用户 scope**：
`systemd-run --user --scope --unit=dshgw-worker-<t>-<id> -p MemoryMax=… -- <bwrap argv>`（`<id>` 是每个
进程世代一个的序号；固定名字会被 systemd 拒绝复用，见 `deploy/dshgw/README.md` 的限额一节）。
本机实测（单位 = 实测值）：scope 内 `memory.max=2147483648`、`pids.max=512`、`cpu.max=200000 100000`
（即 CPUQuota=200%），进程退出后 scope 自动回收；worker 仍是 dshgw 的后代（scope 包裹现有进程树，
不把进程改挂到用户管理器下），因此"worker 是网关的子进程"这条性质没有被破坏。

**为什么不自建子 cgroup**：cgroup v2 的"无内部进程"（no internal processes）规则规定，
含有进程的 cgroup 不能把控制器下放给子 cgroup。systemd 服务的 cgroup 里总有主进程，因此实测
`aigw-local.service` 的 `cgroup.subtree_control` 写入失败（同目录 `mkdir` 成功、写控制器失败）。
用户 scope 由用户管理器在委托树上创建，没有这个限制，且不需要 root。

降级：没有用户管理器时限额不可用但 worker 照常启动，并只告警一次（有测试）。

## 6. 未决项（默认选择，用户可改）

1. **子进程异常退出策略**：默认有限重启（5 分钟内最多 3 次），超限后记录 ERROR 并标记 DSH 不可用，
   aigw 继续提供其余功能（不因 DSH 挂掉而整体退出）。
2. **租户端口与 TLS**：阶段 3 先用 loopback/无 TLS 的本地形态验证；宿主上的对外 TLS
   仍走 nginx（那一步需要 root 或改用由当前用户持有的证书直连），**另立设计**。
3. ~~**资源限额**~~：已按 §5b 实现（每 worker 一个用户 scope + 单元级汇总上限）。
4. **`dshgw` 是否保留独立运行能力**：保留（`dshgw serve` 仍可单独跑），
   以便现有 systemd 部署不改动、两种形态共存。
