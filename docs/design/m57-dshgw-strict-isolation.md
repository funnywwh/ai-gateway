# M57 dshgw 严格租户隔离（bubblewrap 模式，无每租户 OS 用户）

## 1. 目标与口径（用户原话）

> 增加严格的 bwrap 租户隔离模式：不为每个租户创建 OS 用户；每个租户只能看到自己的目录；
> 看不到宿主机文件、其他租户目录和敏感配置；可以在自己的工作目录中创建子目录；
> 保留原有的每租户 OS 用户模式作为兼容方案；完成测试、文档和宿主机部署验收命令。

由此确定的验收口径：

1. `deploy.isolation: bwrap` 下建户**不执行 `useradd`**；所有 worker 以同一个共享非特权账号运行。
2. 沙箱内 `/home`、`/root`、`/srv`、`/var` 等宿主树为空；其他租户的目录**不存在**（不是"无权限"，是不可见）。
3. `/etc` 不是被整体挂载的树，只有运行时确实要读的具名文件；`/etc/dshgw`（gateway 配置与 per-tenant key）不可见。
4. 租户在**自己的 workspace 内可写、可建子目录**；`/usr` 等只读。
5. `deploy.isolation: user` 的既有行为、权限模型与验收**完全不改**，继续可用。
6. 两种模式之间可对**已存在租户**做显式迁移（`tenant re-isolate`），不重建租户。
7. 交付测试、文档与**宿主终端可执行的部署/验收/回滚命令清单**。

## 2. 为什么不能再用 UID 边界：隔离强度的诚实说明

`user` 模式的边界是 Linux UID/systemd 文件权限：每租户一个 `dsh-<t>` 账号，跨租户读写由内核拒绝。
`bwrap` 模式**放弃了这条边界**（用户明确要求不建 OS 用户），因此它的隔离强度必须由其它机制共同承担，
且这些机制的前提必须在文档里写清，而不是让读者以为它与 UID 边界等价：

| 机制 | 作用 | 前提/限制 |
|---|---|---|
| bubblewrap mount namespace | 租户只能看到 profile 显式挂载的路径 | 需要宿主机允许非特权 user namespace |
| **空 tmpfs 根**（实测） | bwrap 的根不是宿主根；**没有挂载的东西根本不存在** | 这是本模式最主要、也最容易被误以为"靠 tmpfs 隐藏"的机制 |
| 逐路径只读绑定 | `/usr`、`/bin`、node、dsh release | 只读，租户不能改写运行时 |
| 逐租户可写绑定 | 仅 `/srv/dsh/<t>` 与 `/var/lib/dshgw/tenants/<t>` | 可建子目录 |
| 父目录 `0711` / 租户叶 `0700` | 共享账号下"其他用户"位的第二道防线 | `doctor` 逐租户复核 |
| AppArmor 限制非特权 userns | 阻止租户在 profile 内再套一层 namespace | 依赖宿主机 sysctl=1 |
| dsh 内层 sandbox | worker 内部写越界（Landlock 回退） | 嵌套 bwrap 在此宿主被 AppArmor 拒绝，dsh 回退 Landlock |

**必须说明的结论**：无每租户 UID 时，隔离性来自 namespace 视图，而不是内核身份。
`root` 与共享 worker 账号本身可以进入任何租户的数据；这与 M51 已记录的"root/dshgw 可冒充租户"同源。
需要"连 root 都不能读租户数据"的部署不应使用本模式。

## 3. 实测事实（决定 argv 形状的三条）

设计不是推测出来的，下面三条都在目标宿主（bubblewrap 0.11.1）实测得到，并已固化进代码注释与测试：

1. **bwrap 的根是空 tmpfs**：沙箱内 `/` 只有 profile 挂载出来的目录。
   实测 `ls -A /` → 仅 `dev etc home lib lib64 proc root srv tmp usr var`（全部是挂载点）。
   因此 `/home`、`/root` 等 tmpfs 是"纵深防御"，而不是隐藏手段；**没有挂载=不存在**。
2. **动态 loader 走顶层符号链接**：宿主 `/bin → usr/bin`、`/lib → usr/lib`，而这些**符号链接本身**
   位于被绑定树之外。只绑 `/usr` 时 `execvp ...: No such file or directory`；
   必须显式绑定 `/usr`、`/usr/lib→/lib`、`/usr/lib64→/lib64`，
   以及宿主自己的 `/bin`、`/sbin`（`--ro-bind-try`，兼容非 merged-/usr 宿主）。
   `/bin` 缺失的直接后果：`#!/bin/sh` 脚本与 Node `child_process` 的默认 shell 在沙箱内不存在。
3. **systemd 拒绝未渲染的占位符模板**：实测
   `systemd-analyze verify` 对 `ExecStart={{DSHGW_WORKER_EXEC}}` 报
   `Command {{DSHGW_WORKER_EXEC}} is not executable` 并 **exit 1**，对 `User={{...}}` 给出警告。
   因此 bwrap unit **不使用占位符渲染**，而是与 `dsh-worker@.service` 同构的静态 `%i` 模板；
   配置漂移由 `doctor` 交叉核对（见 §5）。

## 4. 实现结构

### 4.0 unit 加固与 `CapabilityBoundingSet=`（实测与内核语义）

bwrap worker unit 复用了既有 `dsh-worker@.service` 的加固：`NoNewPrivileges=yes`、`PrivateTmp`、
`PrivateDevices`、`ProtectKernel*`、`RestrictSUIDSGID`、`LockPersonality`、**`CapabilityBoundingSet=`（空）**、
`RestrictAddressFamilies`。其中"空 capability bounding set 会不会让 bubblewrap 起不来"值得单独交代，
因为这是一个会让人不敢开的开关：

- **`systemd-run --user` 不能作为证据**：`--property=CapabilityBoundingSet=` 在 user manager 下
  连 `/bin/true` 都以 `status=218/CAPABILITIES` 失败，所以那个失败是 user manager 无法应用该属性，
  不是 bwrap 的问题。
- **同一宿主机上的系统 unit（`dshgw.service`、`dsh-worker@.service`）本来就带 `CapabilityBoundingSet=`**
  并已在 M51 部署中正常工作——system manager 应用"清空 bounding set"是常规操作。
- **内核语义**：`user_namespaces(7)` 明确"新建 user namespace 的进程在新 namespace 内获得完整 capability 集合"，
  而 `capabilities(7)` 明确 bounding set 只"限制 `execve(2)` 期间获得的 capability"。
  bwrap 的挂载全部发生在 exec 目标程序**之前**，此时它持有的是 namespace 内的完整能力集合，
  因此空 bounding set 不会影响沙箱构造；exec 之后 node 以空能力集合运行，正是我们想要的。

结论：保留 `CapabilityBoundingSet=`，并把"首个 bwrap 租户 `systemctl show ActiveState=active`"
写进宿主验收（若宿主 systemd 拒绝该属性，会立刻表现为 `218/CAPABILITIES`，而不是静默故障）。

```text
internal/dshgw/sandbox/profile.go       profile 计算（纯函数，无副作用）
internal/dshgw/sandbox/staging_test.go  真实 bwrap 验收（自跳过）
internal/dshgw/tenancy/worker.go        共享账号校验、profile 就绪、租户叶权限
internal/dshgw/tenancy/reisolate.go     user ⇄ bwrap 迁移（含回滚）
cmd/dshgw/sandboxexec.go                worker unit 的 ExecStart 启动器 + doctor 辅助
cmd/dshgw/tenant.go                     tenant re-isolate
deploy/dshgw/dsh-worker-bwrap@.service  静态 %i 模板（systemd-analyze verify 通过）
```

### 4.1 profile（`sandbox.Profile`）

顺序即语义，先隐藏后绑定：

1. `--tmpfs /home /root /tmp /var /srv /etc/dshgw`；
2. 只读运行时：`/usr`、`/usr/lib→/lib`、`/usr/lib64→/lib64`、`/bin`、`/sbin`，外加 node 与 dsh release 的目录；
3. `/etc` 只绑具名文件，外加**唯一一个目录** `alternatives`：`resolv.conf`、`hosts`、`nsswitch.conf`、
   `passwd`、`group`、`localtime`、`ssl`、`ca-certificates`、`alternatives`。
   最后一项不是锦上添花：发行版的 `/usr/bin/pager`、`awk`、`which`、`vi` 等全是
   `-> /etc/alternatives/<name>` 的软链，少了它这些名字在一个"看起来完整"的 `/usr` 里全部悬空，
   而症状离病因很远——这台机器的 git 默认 pager 就是字面量 `pager`（alternatives 包装器名），
   于是交互终端里 `git log` 报 `error: cannot run pager: No such file or directory` +
   `fatal: unable to execute pager 'pager'`，管道给非 tty 却正常；
   该目录只含指向已绑定 `/usr` 路径的软链，不额外暴露宿主数据；用 `--ro-bind-try` 保持非 Debian 宿主不变；
   注意 `passwd` 绑的**不是宿主原件而是每租户渲染的视图**（`Tenant.PasswdFile` = `<DshHome>/sandbox/passwd`，
   由 tenancy 层用 `sandbox.RenderPasswd` 把本账号的 home 改写成 workspace）——沙箱里 `HOME` 是 workspace，
   而 OpenSSH 用 `getpwuid()` 而不是 `$HOME` 展开 `~`，不渲染视图时 `~/.ssh/config`/`known_hosts`/默认私钥
   都会落到被 `--tmpfs /home` 藏起来的宿主家目录上，租户里 `ssh <别名>` 会退化成对别名做 DNS 解析；
   视图只改「名字 → 家目录」一个字段，其余行原样；它落在租户自己的 DSH home 里，是**视图不是边界**
   （租户改写它只能改自己沙箱里的这份映射，挂载与权限都不读它）；
4. 租户自己的可写根：仅 workspace 与 `.dsh` 的父目录；
   per-tenant 配置目录（`tenant.env`、`gateway.key`）**完全不挂载**——它由宿主侧 systemd 读取，
   挂进去只会把 `gateway.key` 交给共享账号，而 user 模式下租户读不到它；
5. `--dev /dev --proc /proc --unshare-pid --die-with-parent`，**不** unshare 网络（worker 要连 aigw）。

`resolve()` 把 registry 记录当**数据**而不是权限：workspace 必须落在 `workspace_root` 内、
`.dsh` 必须落在 `tenant_root` 内，租户名有正则约束，环境 argv 元素禁止换行/空串。
即使 `registry.json` 被篡改，也无法把 profile 变成"绑定宿主根"。

### 4.2 启动器（`dshgw sandbox-exec`）

worker unit 的 `ExecStart=/opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml sandbox-exec %i`。
启动器只接受租户名：bwrap 路径、node、release、每个挂载点全部来自 **root 所有的配置与 registry**，
租户可写的任何内容都不参与决策；校验失败以退出码 **127** 结束（与"命令自身失败"区分）。
`--print` 打印 profile argv 供评审与验收，不执行。

**启动器刻意不要求 root**：unit 的 `User=` 就是共享 worker 账号，若在这里 `requireRoot()`，
每个 bwrap worker 都会以 "must run as root" 启动失败（实现期真实踩到，已加回归测试
`TestSandboxExecRunsWithoutRoot`）。启动器也不提权：`syscall.Exec` 让 bubblewrap 以调用者身份接管进程；
它读取的 `/etc/dshgw/config.yaml`（`root:dshgw` 0640）与 `registry.json`（`dshgw:dshgw` 0600）
本来就只有 root 与 gateway 组可读，因此"谁能运行它"与"谁已可冒充租户"是同一个集合。

### 4.3 建户/删除/迁移

- `tenant create`：bwrap 模式下不 `useradd`，改以共享账号拥有 workspace 与 `.dsh`；
  发布 registry 前先 `SandboxProfileReady()`（账号存在、运行时可用、profile 可构造、租户叶无 other 位）。
- `tenant remove`：bwrap 模式**跳过 `userdel`**（共享账号必须活下来），其余快照/路由/nginx 语义不变。
- `tenant re-isolate --to user|bwrap NAME`：
  preflight → 停旧 unit → **`chown -R` 到目标模式账号** → registry 落盘 → daemon-reload →
  启动新 unit → loopback 401 readiness → 恢复 enable 状态。
  任一步失败：停新 unit、chown 回原账号、恢复 registry、重启原 unit。
  user→bwrap 迁移后**不删除**遗留的 `dsh-<t>` 账号（不可逆且无收益），文档中明确交代。

## 5. doctor 前置条件

`isolation: bwrap` 被配置、或任一租户记录为 bwrap 时追加检查：

| 检查 | 失败含义 |
|---|---|
| `bwrap-bin` | 配置的可执行文件不存在/不可执行 |
| `bwrap-apparmor-userns` | `/proc/sys/kernel/apparmor_restrict_unprivileged_userns` 不是 `1`（租户可嵌套 namespace） |
| `bwrap-sandbox-runtime` | profile 需要的绑定源（bwrap/node/bin.js/release + `/usr`、`/usr/lib`、`/usr/lib64`）不可用 |
| `bwrap-worker-account` | `worker_user` 为空、root、或账号不存在 |
| `worker-unit-bwrap` | unit 缺失，或 `User`/`Group`/`ExecStart` 与配置**不一致**（静态模板的防漂移核对） |
| `tenant-<t>-sandbox` | 该租户 profile 已不能构造，或租户叶出现 other 位 |

`worker_user` 可省略：默认取 `gateway_user`（与配置注释一致），避免为共享 worker 再造一个服务账号。

## 6. 验收

### 6.1 自动化（本仓库）

```bash
make dshgw-test           # 含 sandbox 单测；staging 用例在有 bwrap 的机器上真跑
make dshgw-sandbox-test   # 只跑真实 bwrap staging（缺条件时明确 skip，不伪称通过）
make dshgw-verify         # + vet/import gate/静态构建/host contract/nginx(可选)/staging
```

staging 用例用**真实 bubblewrap**跑真实 profile argv：

- `TestStagingSandboxHidesHostAndOtherTenants`：stand-in node 报告沙箱内实际可见内容——
  自己的 workspace PRESENT、其他租户状态与 gateway state MISSING、`/home`/`/root`/`/srv`/`/var` 条目数为 0、
  `/etc` 仅白名单（`/etc/systemd` 不可见、`/etc/dshgw` 为空挂载点）、`passwd`/`resolv.conf` 可读、
  `sh` 存在（`/bin` 绑定生效）、workspace 可写且可建子目录、`/usr` 与 `/etc/passwd` 只读、
  宿主 sysctl=1 时嵌套 bwrap 必须 `DENIED`。
- `TestStagingRealDshWebServesInsideSandbox`：真实 node + 真实 dsh release 在 profile 内启动
  `dsh web`，断言 dsh 打印 loopback 启动 URL，且未认证 `GET /api` 返回 **401**（gateway 的 readiness 契约）。
  这一条同时是"内层 sandbox 可用"的证据：dsh 在内层 sandbox 不可用时 fail-closed，能起来即说明
  它的探针选到了可用机制（本宿主为 Landlock 回退）。

本机（bubblewrap 0.11.1，`apparmor_restrict_unprivileged_userns=1`）实测两者均通过，
真实 dsh web 在沙箱内约 1 秒内起来并返回 401。

### 6.2 宿主验收（见 §7 命令清单）

自动化不能替代宿主证据：per-tenant 真实 UID/文件权限（user 模式）、共享账号下的实际 worker、
`systemctl` 单元状态、nginx 门户、以及"租户在真实浏览器里看不到宿主文件"的人工确认。

## 7. 与 M51 的差异（已知取舍）

1. `bwrap` 模式**没有 UID 边界**：跨租户隔离靠 namespace 视图 + 权限位 + AppArmor + dsh 内层 sandbox。
2. 共享账号模式下 gateway 与 worker 同账号时（默认 `worker_user = gateway_user`），
   租户无法从沙箱内触达 gateway 的 key，但**主机的 root/gateway 账号可读租户数据**（同 M51 已知边界）。
3. `tenant re-isolate` 是**在线迁移**：会停一次 worker（数十秒不可用），不做零停机迁移。
4. bwrap unit 的 `User`/`ExecStart` 是静态文本；改 `deploy.worker_user` 或二进制路径后必须重装 unit
   （`doctor` 会明确报出不一致，而不是静默运行错误身份）。
5. 内层 dsh sandbox 在当前宿主无法用 bwrap（AppArmor 拒绝嵌套），dsh 回退到 Landlock。
   这不是推测：dsh 在 sandbox 不可用时是 **fail-closed** 的（`verify-dshgw.sh` 里的
   `sandbox-unavailable-fails-closed` 就是这个契约），而 staging 测试里真实 dsh web 在我们的
   bwrap 沙箱内**成功启动并返回 401**，说明它的内层探针找到了可用机制（本宿主即 Landlock 回退）。
   "内层也是 bwrap"在本宿主不成立；本套自动化断言的是"内层存在可用 sandbox 且不阻塞启动"。
