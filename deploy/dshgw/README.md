# dshgw 部署手册（M58 形态：aigw 监督的 rootless 子进程）

`dshgw` 是 DSH 多租户网关。它不再是独立的系统服务：

```
aigw（主程序，当前用户）
 ├─ HTTP :8088                     数据面 + 控制台
 ├─ 子进程：<aigw 同目录>/dshgw      aigw 启动时拉起，随 aigw 退出而停（同 UID）
 │    ├─ 多租户代理 + 门户（直接监听租户端口，无 nginx）
 │    ├─ 本地 admin socket（同 UID，控制台「启用/停用 DSH」用它）
 │    └─ 每租户 worker：bwrap 子进程（无 systemd 单元、无 per-tenant 账号）
 └─ 插件子进程
```

要点：**一个 UID**（aigw、dshgw、所有租户 worker 都是同一个非 root 账号）、**零特权**（安装 = 拷贝两个二进制，
运行 = 一个用户级服务）、**隔离只来自 bubblewrap mount namespace**。

> 旧形态（root 安装 + systemd 单元 + 每租户 OS 用户 + nginx 边缘）已被删除：`install.sh`、
> `dshgw.service`、`dsh-worker@.service`、`dshgw-admin.service`、`dsh-workers.slice`、
> `dshgw.logrotate`、`render-nginx` 与 `admin_allowed_uids` 都不再存在。历史记录见
> `docs/design/m51-dshgw.md` 与 `docs/todo_done.md`。

## 1. 安装（不需要 root）

```bash
make build dshgw-build        # 产出 bin/aigw 与 bin/dshgw
install -d ~/.local/bin
install -m 0755 bin/aigw bin/dshgw ~/.local/bin/     # 必须同目录
```

dshgw 只在 **aigw 自己所在目录**里被查找（`dshgw.binary` 可覆盖为绝对路径）。这条规则的意义是：
搬动安装目录时不会继续用某个陈旧的 dshgw。

dsh 运行时（node 与一个完整 dsh release）也必须是当前用户可读的路径，通常直接复用本地已装的那份：

```bash
export DSHGW_NODE=/home/winger/.local/node-v22.23.1-linux-x64/bin/node
export DSHGW_DSH_ROOT=/home/winger/.local/dsh-0.1.2-rc.1
```

## 2. 配置 aigw

```yaml
server:
  listen: 127.0.0.1:8088
database:
  path: ./data/aigw-local.db    # 数据根（M63）：相对路径以部署根为基准
credentials_key: "<32 字节>"
dshgw:
  enabled: true                 # aigw 启动时拉起并监督 dshgw
  # binary: ""                  # 默认：aigw 同目录的 dshgw
  # state_dir: ./data/dshgw     # 默认 = <database.path 所在目录>/dshgw
  public_host: localhost
  listen: 127.0.0.1:18099       # dshgw 自己的监听地址（租户门户直接由它服务）
  portal_port: 18100
  tenant_port_lo: 18101
  tenant_port_hi: 18199
  worker_port_lo: 18200
  worker_port_hi: 18299
  # template_home: ./data/dshgw/template-home   # 默认 = <state_dir>/template-home
  plugin_path: ./cmd/dshgw/plugin/picker-clamp.js   # 必填：子进程默认目录选择器是 clamp
  plugin_browser_fs: on         # on 需要准备了 browser-fs 的模板，否则用 off
  public_listen: 127.0.0.1      # 门户与租户公开端口的绑定地址（默认 loopback）
  # tls_certificate: /path/fullchain.pem      # 指定证书即在这些端口上启用 HTTPS
  # tls_certificate_key: /path/privkey.pem
  node_bin: /home/winger/.local/node-v22.23.1-linux-x64/bin/node
  current_link: /home/winger/.local/dsh-0.1.2-rc.1   # bin_js 由它推导
```

路径规则（M63，完整版见 [`docs/deployment-layout.md`](../../docs/deployment-layout.md)）：
**数据**默认在部署根的 `./data` 之下（`state_dir` 及其派生的 template/tenant/workspace/backup 根），
相对路径在加载时按进程工作目录归一为绝对路径；**运行时安装**（`node_bin`/`current_link`、
`bwrap_bin`、TLS 证书）必须是绝对路径，留空则按 `DSHGW_NODE`/`DSHGW_DSH_ROOT`/
`DSHGW_TEMPLATE_HOME` 环境变量兜底；**配置**（`config.yaml`、`dshgw.yaml`、`gwproxy.yaml`）留在部署根。

准备租户模板（无特权）：

```bash
DSHGW_NODE=... DSHGW_DSH_ROOT=... DSHGW_TEMPLATE_HOME=<上面 template_home 的值> \
  deploy/dshgw/prepare-template.sh
```

该脚本把 `dsh-browser-fs@0.2.0` 固定安装进模板并校验锁文件 integrity；它需要能访问 npm registry。
无法访问时可以用 `plugin_browser_fs: off`，模板改用 dsh 自己的基础 profile：

```bash
HOME=$T DSH_HOME=$T "$DSHGW_NODE" "$DSHGW_DSH_ROOT/lib/bin.js" --profile web --dump-config
```

### 可选：侧栏账号行与退出（M67）

在 aigw 配置中设置 `dshgw.account_card.enabled: true`（默认关闭；独立运行的 dshgw 则写自己的
`account_card.enabled`），并部署 `plugin_path` 同级的 `account-card/` 插件目录。租户 dsh 侧栏底部会多
一行：登录者的飞书名（取不到回退账号名、再回退租户名）与「退出」按钮；退出只撤销本租户会话并把浏览器
送回门户登录页。

这一行的两个数据面由网关在**租户 origin** 下提供：`GET /dshgw/session/` 与 `POST /dshgw/logout/`，
鉴权与 worker 请求走同一条链；关闭开关时整个 `/dshgw/**` 返回 404。名字来自 aigw（只有它知道飞书绑定），
并在控制台创建租户/轮换密钥时把账号名写进 dshgw 注册表 —— 旧租户下次启用或轮换时会补上。详见
`docs/dshgw.md` §7d 与 `docs/design/m67-dshgw-account-card.md`。

### 可选：浏览器本机目录工作区

在 aigw 配置中设置 `dshgw.browser_workspaces.enabled: true`（默认关闭），并部署
`plugin_path` 同级的完整 `browser-workspace/` 插件目录。网关宿主需要可用 `/dev/fuse`、
`fusermount3` 与当前账号的用户态挂载权限；客户端需要 HTTPS/localhost 和支持 File System
Access API 的 Chromium。该功能不会给租户 sandbox 增加 `/dev/fuse` 或 capability。

用户授权的本机目录映射到 `<workspace>/browser/<随机ID>`，服务器命令通过 FUSE 读写，
不在用户机器执行命令。新增挂载会重启本租户 worker、中断进行中的回合；授权页面须保持连接。
开启功能时 `browser` 子目录是网关管理的保留命名空间，启动前创建为私有真实目录，在租户沙箱中只读绑定，
防止命令替换挂载容器或子挂载点；活动 FUSE 子挂载单独读写绑定。租户及全网关备份不包含该子树内容；关闭功能且没有活动挂载
时，同名普通目录仍正常备份。活动挂载始终排除，应由运行中的网关清理，
离线 CLI 检测到挂载会拒绝破坏性生命周期操作。

`make dshgw-browser-test` 运行 Go 集成和 Node fake-FSA 测试；真实 FUSE 与浏览器权限链路仍需
单独验收。部署前请阅读[浏览器 FUSE 工作区设计与限制](../../docs/design/browser-fuse-workspace.md)。

## 3. 启动与停止

```bash
# 前台（调试）：dshgw 子进程由 aigw 拉起，日志在同一个流里
bin/aigw --config config.yaml

# 用户级常驻单元（推荐；不需要 root）
scripts/aigw_user_service.sh install              # 写入 ~/.config/systemd/user/aigw-local.service 并启动
scripts/aigw_user_service.sh status
scripts/aigw_user_service.sh restart
scripts/aigw_user_service.sh uninstall

systemctl --user stop aigw-local     # 停止：子进程与所有租户 worker 一并退出（--die-with-parent 兜底）
journalctl --user -u aigw-local -f   # 或 tail -f data/aigw-local.log
```

单元文件由脚本按当前仓库路径生成（`WorkingDirectory`、`ExecStart`、日志追加到 `data/aigw-local.log`），
`KillMode=mixed` + `TimeoutStopSec=45` 让 dshgw 有机会按序停掉租户 worker，而不是被直接砍掉。

**开机自启需要 linger**（用户级服务默认只在登录会话里跑）：

```bash
loginctl show-user "$(id -un)" | grep Linger      # 期望 Linger=yes
sudo loginctl enable-linger "$(id -un)"           # 若为 no：一次性、需要 root/polkit
```

脚本会检查并明确告诉你这一步（不会假装用户单元本身就能跨重启）。
注意：systemd 不允许在同名 transient 实例存在时 enable 文件单元；若之前用
`systemd-run --user --unit=aigw-local` 起过实例，先 `systemctl --user stop aigw-local` 再执行 install
（实测：切换只造成秒级中断）。

aigw 启动日志里应能看到：

```
msg="dshgw child configured" binary=.../dshgw config=.../dshgw/config.yaml
msg="dshgw child ready" pid=...
```

## 4. 租户生命周期

生命周期必须由**运行中的 dshgw** 执行：单独跑一次 CLI 进程所启动的 worker 会随该进程退出而死
（`Pdeathsig`）。日常入口因此是 admin socket（同 UID，控制台按钮走的就是它）：

```bash
# JSON 行协议
printf '%s\n' '{"id":1,"op":"tenant-create","name":"alice","key":"sk-..."}' | nc -U <state_dir>/admin.sock
printf '%s\n' '{"id":2,"op":"tenant-list"}' | nc -U <state_dir>/admin.sock
printf '%s\n' '{"id":3,"op":"tenant-start","name":"alice"}' | nc -U <state_dir>/admin.sock
printf '%s\n' '{"id":4,"op":"tenant-stop","name":"alice"}' | nc -U <state_dir>/admin.sock
```

CLI 保留用于离线检查与产物生成：

```bash
bin/dshgw --config <state_dir>/config.yaml doctor                        # 只读前置条件检查
bin/dshgw --config <state_dir>/config.yaml sandbox-exec --print alice    # 打印该租户的 bwrap profile
bin/dshgw --config <state_dir>/config.yaml capture-url alice             # 打印 worker 上报的启动 URL
bin/dshgw --config <state_dir>/config.yaml contract dsh                  # 真实 dsh 契约检查
```

行为约定：

- **worker 启动前会从 aigw 同步该租户的模型清单**（401/403 拒绝启动；aigw 暂时不可达则告警后用现有清单启动）。
- `tenant-stop` 把"该租户停机"写进 registry（`suspended`），因此 **aigw 重启后不会自动拉起它**；
  `tenant-start` 清除该状态 —— 与旧形态用 systemd enablement 表达是同一语义。
- 未停用的租户在 aigw 启动时自动回来；单个租户起不来会逐个上报，不影响其它租户与 aigw 本身。

## 5. 状态与权限

所有状态都在 `dshgw.state_dir` 之下，属主就是运行 aigw 的账号：

| 路径 | 模式 | 内容 |
|---|---|---|
| `<state>/config.yaml` | `0600` | aigw 生成的子进程配置（按需重写） |
| `<state>/admin.sock` | `0600` | 本地 provisioning 通道（只接受同 UID） |
| `<state>/registry.json` | `0600` | 租户 registry（含 `suspended` 持久意图） |
| `<state>/tenants/<t>/.dsh` | `0700` | 租户 dsh 状态与 profile |
| `<state>/tenant-config/<t>/gateway.key` | `0600` | 网关侧保存的租户 Key（沙箱内不可见） |
| `<state>/workspaces/<t>` | `0700` | 租户工作区（可写、可建子目录） |
| `<state>/handshake/<t>.url` | `0600` | 当前 worker 的精确启动 URL（含 session token） |

`doctor` 核对以上私有权限、dsh 运行时、bwrap 前置条件，并对每个租户复核 profile 可构造与目录权限。

## 6. 资源限额

旧形态靠 systemd 单元的 `MemoryMax`/`CPUQuota`/`TasksMax` 管住每个 worker；rootless 形态没有单元，
所以在两层上补回来：

**每 worker（在 aigw 配置的 `dshgw` 段里）**

```yaml
dshgw:
  worker_memory_high_bytes: 1610612736   # 0 = 不限
  worker_memory_max_bytes: 2147483648
  worker_tasks_max: 512
  worker_cpu_quota_percent: 200
```

实现方式是**每个 worker 一个 systemd 用户 scope**（`systemd-run --user --scope
--unit=dshgw-worker-<t>-<id> -p MemoryMax=… -- <bwrap argv>`），不需要 root。

scope 名里的 `<id>` 是每个"进程世代"一个的序号，不是租户名本身：**systemd 拒绝创建一个名字已被占用的
单元**（`Unit … was already loaded or has a fragment file`），所以固定名字会让上一代 worker 的遗留物变成
下一代启动的地雷。2026-09-20 本机就出过这个故障：一个卡在 D 态的 `find`（遍历 sshfs 挂载）让旧 scope 无法
回收，之后该租户每次启动都秒退，网关照常返回 `worker authentication unavailable`。现在的行为：每次启动用新
名字，启动被 systemd 拒绝时自动换名重试一次，并对该 scope 的 cgroup 做收尾（见下一节）。

为什么不用 dshgw 自己建子 cgroup：cgroup v2 有一条"无内部进程"规则 —— **含有进程的 cgroup 不能把控制器
下放给子 cgroup**。systemd 服务自己的 cgroup 里总有主进程，因此 `aigw-local.service` 的
`cgroup.subtree_control` 在本机实测**根本写不进去**（同一目录下 `mkdir` 可以，写控制器不行）。用户 scope
没有这个问题：用户管理器拥有被委托的树，由它创建 scope 并设限额，实测生效：

```
cgroup=…/app.slice/dshgw-worker-<tenant>-<id>.scope  memory.max=2147483648  pids.max=512  cpu.max=200000 100000
```

**整个部署（单元级汇总上限，替代旧 slice 的 `MemoryMax=40G`）**

```bash
scripts/aigw_user_service.sh install --memory-max 40G --tasks-max 4096 --cpu-quota 800
```

降级行为：宿主没有用户管理器（例如把 dshgw 单独跑在没有 systemd 的环境里）时，限额**不可用但 worker 照常启动**，
并按部署告警一次 —— 缺一条配额不该变成一个起不来的租户。该行为有测试钉住。

**scope 的收尾（2026-09-20 修复）**：worker 的 SIGTERM 只覆盖它自己的进程组，而 agent 的工具调用跑在
worker 的 cgroup 里、不一定在同一个进程组 —— 一个卡在 FUSE 等待里的进程就是这样活过了 worker，并把
scope（以及它占的名字）一直挂住。现在停止租户时按顺序做三件事：`systemctl --user stop`、清空该 scope 的
cgroup（先 TERM 后 KILL，带超时）、再 stop 一次回收单元；杀不掉的进程会被记成告警并留在它自己的 scope 里
（新名字不阻塞下一次启动）。

**ssh 工作区的死挂载（2026-09-20 修复）**：sshfs 守护进程死掉后，挂载表里的条目还在，读它是 ENOTCONN，
而 `sshfs` 无法覆盖这个残留条目再挂（`failed to access mountpoint … Transport endpoint is not connected`）。
现在 `Open` 会先探测该挂载的 FUSE 连接（`/sys/fs/fuse/connections/<id>`，id 由挂载点的 st_dev 解出）：
连接没了就把残留条目卸载掉、删掉记录，然后真正重新挂载。卸载连续失败时按"卡死"处理 —— 先杀掉服务该挂载
的 sshfs 进程（它的 fd 一关，所有等待中的请求立刻失败），必要时再写 `abort` 让内核释放等待者。

## 7. 验证

```bash
make dshgw-test            # Go 测试（含 sandbox 单测）
make dshgw-sandbox-test    # 真实 bwrap：宿主隐藏/workspace 可写/真实 dsh web 在沙箱内启动并 401
make dshgw-verify          # 上述 + 构建 + vet + 真实 dsh 契约 + 模板准备
```

本机实测过的完整链路（无 root、无 systemd、无 per-tenant 账号）：

1. `aigw` 启动 → 生成子进程配置 → 拉起同目录 `dshgw` → 收到 ready；
2. 经 admin socket `tenant-create` → worker 以 bwrap 子进程起来 → `GET /api` 返回 **401**；
3. 进程树 aigw → dshgw → bwrap → node 全是同一个非 root 账号，`getent passwd` 里**没有**新的 `dsh-*`；
4. `tenant-stop` 后端口关闭；`tenant-start` 前日志出现 `models refreshed before worker start`（模型同步）；
5. aigw 重启后未停用租户自动回来；停止 aigw 后**子进程与 worker 零残留**。

## 8. 从旧的 root/systemd 部署迁移

旧的 root 形态（systemd 单元 + 每租户 OS 用户 + nginx）与当前形态并存没有意义，迁移脚本把它换过来：

```bash
# 1) 先看它打算做什么（不需要 root，什么都不改）
scripts/migrate_dshgw_to_supervised.sh --dry-run --apply \
  --old-config /etc/dshgw/config.yaml --old-registry /var/lib/dshgw/registry.json \
  --aigw-config /path/to/aigw/config.yaml --aigw-user <运行 aigw 的账号>

# 2) 确认后执行（需要 root：要停系统单元、改属主）
sudo scripts/migrate_dshgw_to_supervised.sh --apply \
  --old-config /etc/dshgw/config.yaml --old-registry /var/lib/dshgw/registry.json \
  --aigw-config /path/to/aigw/config.yaml --aigw-user <账号>

# 3) 不满意就回滚（先看计划，再执行）
scripts/migrate_dshgw_to_supervised.sh --rollback --dry-run ...   # 看
sudo scripts/migrate_dshgw_to_supervised.sh --rollback ...        # 做
```

脚本按顺序做这些事（每一步都会先打印）：

1. 备份 registry 与 per-tenant 配置到 `--backup-dir`；
2. `systemctl disable --now` 所有旧 worker 单元 + `dshgw.service` + `dshgw-admin.service`；
3. 把 state / workspace / tenant-config 三棵树的属主交给运行 aigw 的账号，并收紧到 `go-rwx`
   —— 这是唯一较"硬"的一步，备份是它的安全网；
4. 改写 registry：每个租户标记 `isolation: bwrap`、`uid` 换成该账号、保留 `suspended` 语义；
5. 在 aigw 配置末尾追加 `dshgw:` 段（若已有该段则只提示你核对，绝不覆盖）；
6. 以该账号安装并启动用户级单元，然后**逐个租户探测 `/api` 是否回到 401**。

租户数据不搬家：脚本把新形态指向旧形态已经填好的目录（§2 的 `state_dir`/`tenant_root`/`workspace_root`）。

回滚会停掉受监督实例、重新启用旧单元，并**按 registry 把属主逐一改回**（`dsh-<t>` / `root:dshgw`）——
少了这一步，旧服务会读不到自己的数据，那不叫回滚。旧单元只被 disable、不被删除：确认迁移站得住之后，
再手工清理 `/opt/dshgw`、`/etc/dshgw` 与 `dsh-*` 账号。

计划本身有自动化测试（`scripts/test_dshgw_migration_plan.py`，跑在 `make dshgw-test` 里）：
它用一份合成的旧部署夹具断言"apply/rollback 的每一项关键步骤都出现在计划里，且 dry-run 不执行任何命令"。

### 8.1 直接下线旧形态（M63：归档而不是迁移）

不需要旧租户的数据时，用下线脚本：它把三棵目录树、nginx 的 dshgw 转发与旧单元文件**先归档**
（默认 `data/prev/legacy-dshgw/`），验证归档可读且含 `registry.json` 之后，才停单元、删 nginx 转发、
删 `/opt/dshgw`、`/etc/dshgw`、`/var/lib/dshgw`。归档是唯一的回滚来源。

```bash
# 1) 清单 + 计划（不需要 root；归档前不动任何东西）
scripts/decommission_legacy_dshgw.sh

# 2) 确认后执行（需要 root；在宿主终端里跑）
sudo scripts/decommission_legacy_dshgw.sh --apply

# 可选：只先取归档与校验，不碰任何服务
sudo scripts/decommission_legacy_dshgw.sh --archive-only --apply

# 可选：连 per-tenant 账号一并删除（默认不做）
sudo scripts/decommission_legacy_dshgw.sh --apply --remove-accounts
```

归档覆盖面（除三棵被删的树）：旧配置里写明的 `workspace_root` 与 `deploy.backup_dir` 也会被打包，
但脚本**不删它们**——那是租户自己的文件，删之前必须有人明确决定；运行时会逐条打印"保留"清单。
`--old-config`/`--state-dir`/`--etc-dir`/`--nginx-dir` 可指向非默认位置（旧配置里的 `state_dir`、
`registry_path`、`workspace_root`、`nginx_include_path`、`backup_dir` 会被读出来）。
计划同样有测试：`scripts/test_decommission_legacy_plan.py`
（跑在 `make dshgw-test` 里），断言"归档 → 停服 → 校验 → 删除"的顺序、归档覆盖面，以及 dry-run 什么都不做。

### 8.2 把已有的 dshgw 状态树搬到数据根里

`scripts/move_dshgw_state.sh --from <旧根> --to <部署根>/data/<名字> [--apply]`：先停 dshgw，
再搬 `state/`、`template-home/`、`current` 与日志，然后改写 registry 与每租户文件里嵌的绝对路径
（`registry.json`、`profiles/web/cordis.patch.yml`、`storages/workspace.json`、session 缓存），
**绝不改写 `workspaces/**`**（那是租户自己的内容）。dry-run 会打印每个将被改写的文件。

## 9. 可选：单域名 + 路径前缀的入口反代（`gwproxy`）

不想为每个租户开一个端口时，用 `bin/gwproxy` 把三个服务收进**一个域名、一个端口、一张证书**：

```bash
make gwproxy-build                       # 产出 bin/gwproxy
bin/gwproxy --config deploy/dshgw/frontproxy.example.yaml
```

| 路径 | 去向 | 说明 |
|---|---|---|
| `/`（兜底） | aigw | **根挂载**：控制台 `/admin/ui/`、`/version`、`/v1/…` 保持原路径；`/` 本身跳转到 `/admin/ui/`（aigw 在根上是 404） |
| `/aigw/*`（可选挂法） | aigw | 前缀挂载：`aigw_strip_prefix: true` 让反代剥前缀，aigw 收到的路径与直连一致 |
| `/dshgw/*` | dshgw 门户 | 剥前缀，并按 `portal_port` 补上 Host 与 edge 头（dshgw 两者都校验） |
| `/t/<tenant>/*` | 该租户的 dsh | 剥 `/t/<tenant>`，按 registry 里的该租户公开端口补 Host 与 edge 头；**会话、账号 DSH 开关、worker 握手仍由 dshgw 负责**，反代不重复实现 |
| `/` | → `/dshgw/` | 门户是前门 |
| `/healthz` | 反代自身 | 探针 |

反代只做路由、TLS 与头清洗：Host 不匹配 `public_host` 直接 404；未知租户 404；上游不可达 502；
租户列表每 `registry_reload` 从 dshgw 的 registry.json 重读，所以控制台新建的租户无需重启反代即可访问。

### 9.0 实测纠正：dsh UI **必须有自己的 origin**，路径前缀装不下它

这条是本项目用真浏览器（headless chromium + CDP，抓请求/异常/控制台）测出来的，推翻了早先"路径前缀可以承载 dsh UI"的假设：

- dsh 前端用 **`location.origin`**、**`/api`**、**`/api/remote.mux`** 这些**绝对/根路径**构造请求；
  **origin 按定义不含路径**，所以在 `https://域名/t/<租户>/` 下，它的 API 调用必然打到 `https://域名/api/…`
  —— 那里是 aigw，不是租户 → 页面**白屏**。
- 实测对照（同一租户、同一浏览器）：
  - **端口模式**（`http://域名:18302/`）：UI 正常渲染（`<title>DeepSeek Harness`、DOM 30KB、
    "Choose workspace"），约 30 个请求**全 200、零失败、零异常、零控制台错误**；
  - **路径模式**（`http://域名:8090/t/dsh-tenant/`）：shell 与静态资源都 200（含 3.7MB 插件 bundle），
    但应用发起的 `GET /api/session` → **404**（落到 aigw），`/api/remote.mux` 被拒——
    即"HTML 到了、应用起不来"，表现就是白屏。
- 因此：**路径前缀只适用于门户**（那是我们自己的 HTML）；**租户 UI 必须独占一个 origin**。
  不能分配子域名时，唯一可行的多租户拓扑是**同一域名 + 每租户一个端口**。
  另外两条路：① 只有一个租户时把该租户放在域名**根路径**（零改写、单端口）；
  ② 改写 dsh 的客户端 bundle（`location.origin` / `/api` 字面量）——生成物、随 dsh 升级而变，**不推荐**。

`gwproxy` 因此提供"前门跳转"：域名上的 `/dshgw/` 与 `/t/<租户>/` 会 **302 到该服务自己的端口**，
浏览器最终落在具备独立 origin 的地址上，dsh UI 正常工作。

### 9.1 没有子域名时的配置（前门跳转 + 每租户一个端口）

反代只是入口；**dshgw 也要知道自己在路径模式下服务**，否则它生成的跳转与会话 cookie 还是按端口：

```yaml
dshgw:
  public_host: chat.example
  public_scheme: https          # 端口模式下公开 URL 的 scheme（纯 HTTP 部署必须设 http）
  # 不要设 public_base_url：租户 UI 需要独立 origin（见 §9.0）
```

```yaml
# gwproxy：域名只做前门，控制台留在域名根，门户与租户前缀跳到各自端口
aigw_prefix: /
root_redirect: /admin/ui/
portal_prefix: /dshgw
portal_redirect: true
portal_port: 18100
tenant_prefix: /t
tenant_redirect: true
public_scheme: https
```

端口模式下每个租户拿到自己的 origin（`https://chat.example:18101/`），会话 cookie 按租户命名、
`Path=/`，而不同端口就是不同 origin，浏览器天然隔离。域名上的 `/t/<租户>/` 只是前门，
302 到该租户的端口后一切照旧。

**另外修掉一个同族缺陷**：端口模式过去把公开 URL 的 scheme **写死成 https**。纯 HTTP 部署下
每次跳转都会指向没人监听的 https 端口，表现同样是"点了没反应"。现在由 `public_scheme`（默认 auto：
路径模式看 `public_base_url`，端口模式沿用 https）决定，会话 cookie 的 `Secure` 也跟随它。

### 9.2 为什么"同源 iframe"解决不了

`dsh web` 只提供 `--host/--port/--trusted-host/--no-open`，**没有 base-path 选项**。实测它的 shell：
资源引用是**相对路径**（`./assets/…`），两个 JS bundle 里**没有硬编码 `/api`**（端点由 `import.meta.url`/`baseUrl`
推导），只有 HTML 里少数**根绝对引用**（`/plugins/??…` 插件 bundle、`href="/"`）。

因此反代只对 `text/html` 做一处很窄的改写：把**标签内**以 `/` 开头的 `href/src/action` 值加上租户前缀。
正文、注释、相对路径、协议相对 URL（`//host/x`）与已经带前缀的值都不动；`application/json` 等一律不碰。
实现是"走标签"而不是整串替换，所以正文里恰好出现的 `href="/"` 也不会被改（有测试钉住这两个边界）。

同源 iframe 不解决问题：iframe 里的文档仍然按**顶层 origin** 解析绝对路径，`/api` 依旧打到域名根。
跨 origin iframe（`<iframe src="https://<租户>.chat.example/">`）确实零改写，但需要 **子域名**
（通配 DNS + 通配证书）—— 在"不能分配子域名"的前提下不可用。所以本部署走的是"每租户一个端口"。

## 10. 本机验证部署（已就绪）

本机（`rag-server` / `192.168.190.86`）已按 §9 部署了**验证用**的入口，特点是**完全不动现有 `:8088`**：

| 组件 | 形态 | 端口 | 与线上的关系 |
|---|---|---|---|
| 线上 aigw | 用户单元 `aigw-local`（文件单元、enabled） | `:8088` | **保持不变**：反代以根挂载转发，aigw 配置一字未改 |
| `gwproxy` | 用户单元 `gwproxy-verify`（enabled） | `0.0.0.0:8090` | 新的公开入口，单域名 + 路径前缀 |
| 验证 dshgw | 用户单元 `dshgw-verify`（enabled），路径模式 | 网关 `127.0.0.1:18299`、门户 `18300`、租户 `18301+`、worker `18400+` | 独立于旧的 `dshgw.service`，同 UID、无 root |
| 旧 dshgw（root/systemd 形态） | `dshgw.service` + 5 个 worker | `:32600`+ | **未受影响**，仍在运行 |

配置在部署根（`./dshgw.yaml`、`./gwproxy.yaml`，0600），数据在 `./data/dshgw-verify/`
（`state/`、`template-home/`、日志；state 为 0700、文件 0600）—— M63 之前它们在
`~/.local/share/dshgw-verify/`，现在都收进单一数据根（见 `docs/deployment-layout.md`）。
两个单元都是文件单元并 `enabled`，重启机器后自动拉起；日志分别在 `state/../dshgw.log`、`gwproxy.log`。

访问入口：

```bash
http://192.168.190.86:8090/              # → 302 到 aigw 控制台
http://192.168.190.86:8090/admin/ui/     # aigw 后台（控制台）
http://192.168.190.86:8090/version       # aigw API
http://192.168.190.86:8090/dshgw/        # dshgw 门户（登录页）
http://192.168.190.86:8090/t/verify1/    # 租户 dsh（未登录会 302 回门户路径）
http://192.168.190.86:8088/version       # 直连 aigw，仍然可用
```

**实测结果**（本机，2026-09-18）：

- `:8088` 直连 `/version`、`/healthz` 均 200 —— 反代上线没有影响它；
- 根挂载的 aigw：`/` → 302 `/admin/ui/`；`/admin/ui/` 200；`/admin/ui` → 301 且**带端口**
  （`http://192.168.190.86:8090/admin/ui/`，反代保留浏览器 authority，aigw 用它构造绝对跳转）；
  控制台静态资源 `/admin/ui/app.css`、`/admin/ui/js/app.js`、`/admin/ui/favicon.svg` 均 200；
  `/version`、`/healthz` 200；`/admin/api/v1/accounts` 401；
  **控制台响应与直连 `:8088` 逐字节一致**（`/admin/ui/` 669 字节、401 响应体相同）；
- `/dshgw/` 200，登录表单 action 已是 `/dshgw/login`（dshgw 自己按 `public_base_url` 生成）；
- `GET /t/verify1/` 未登录 → 302 到 `http://192.168.190.86:8090/dshgw/`（**路径式门户，不是 host:port**）；
- `POST /t/verify1/api`（带 Origin）→ 401；
- 注入一个临时会话后 `GET /t/verify1/` → **200（24KB 真实 dsh shell）**，HTML 里根绝对引用已被加上前缀
  （`href="/t/verify1/"`、`src="/t/verify1/plugins/??…"`），相对资源保持 `./assets/…`；
  经前缀取 `/t/verify1/assets/index-*.js` → **200 / 423038 字节**；会话记录里出现绑定 `127.0.0.1:18400` 的
  `dsh-auth-…`，即 dshgw→worker 握手成功；
- worker 是 dshgw 的 bwrap 子进程、与 aigw 同 UID，且限额生效：
  cgroup `dshgw-worker-verify1.scope`，`memory.max=2147483648`、`pids.max=512`、`cpu.max=200000 100000`；
- 外部 Host 404、未知租户 404；旧 `dshgw.service` 与 `aigw-local` 均仍 active。

### 10.1 会话 cookie 的 Secure 属性（纯 HTTP 下必须跟随实际协议）

门户登录成功后会下发会话 cookie。**浏览器会拒绝在纯 HTTP 源上保存 `Secure` cookie**（只有 localhost 例外），
于是"登录成功 → 跳到租户路径 → 没有 cookie → 又被送回门户"，看起来就像"登录没有任何反应"。
因此 `session_cookie_secure`（默认 `auto`）按部署的真实协议决定：

| 值 | 行为 |
|---|---|
| `auto`（默认） | 路径模式看 `public_base_url` 的 scheme（https 才加 `Secure`）；端口模式保持历史行为（加 `Secure`，假定前面有 TLS 终止） |
| `always` | 总是加 `Secure`（部署在 TLS 终止之后时用） |
| `never` | 从不加（纯 HTTP 的端口模式部署用） |

实测（本机纯 HTTP 部署）：修复后登录响应头为
`Set-Cookie: dshgw_s_dsh-tenant=…; Path=/t/dsh-tenant/; Max-Age=604800; HttpOnly; SameSite=Lax`
（**无 `Secure`**），带该 cookie 访问 `/t/dsh-tenant/` 返回 200 与真实 dsh shell。

### 10.2 让验证租户真正可用（需要你的一把 Key）

本机验证租户是**直接写 registry** 建的（`tenant-create` 会向 aigw 校验 Key，而部署者没有可用 Key），
因此它没有 `gateway.key`，也就没有 `<DshHome>/settings.yaml` 与 `.credentials.yaml` —— 登录能进 dsh，
但 dsh 内部**没有可用的模型 provider**。装一把真实 Key 即可（同时会写入 worker 需要的三个产物）：

`tenant-create` 会向 aigw 校验 Key，因此验证租户是直接写 registry 建的（无 Key → 跳过启动前模型同步 →
worker 照常起来）。给它装一把真实 Key，然后打开门户登录：

```bash
printf '%s\n' '{"id":1,"op":"tenant-set-key","name":"dsh-tenant","key":"<你的 aigw Key>"}' \
  | nc -U data/dshgw-verify/state/admin.sock
# 然后浏览器打开 http://192.168.190.86:8090/dshgw/ ，用同一把 Key 登录
```

租户名必须是 **aigw 授权返回的那个**：控制台「启用 DSH」时写入 `accounts.dsh_tenant`，
门户要求它与 dshgw registry 里的租户名一致，否则报「该账号的 dsh 租户尚未就绪」。
本机该账号的 `dsh_tenant` 是 `dsh-tenant`，验证 registry 里已按这个名字建了租户。

拆掉验证栈（不影响 `:8088` 与旧部署）：

```bash
systemctl --user disable --now gwproxy-verify dshgw-verify
rm -f ~/.config/systemd/user/{gwproxy,dshgw}-verify.service
# 数据目录按需保留：./data/dshgw-verify（配置 ./dshgw.yaml、./gwproxy.yaml 可一并删除）
```

## 11. LAN 用户的"设置/模型"面板（`settings_ui`）

### 11.1 为什么默认会失败

dsh 把设置/模型面板挂在**客户端**判定上：

```js
// @deepseek-ai/dsh-client-ui-settings/lib/client.js
const persistence = ctx.remote.$host.isLoopback ? "host" : "memory";
// persistence === "memory" 时镜像的 load()/ensure() 直接返回，view 永远 undefined
// → 面板报 "settings are unavailable in this browser"

// @deepseek-ai/dsh-client-connection/lib/client.js
isLoopback: transport?.ownsHost === true || pageLocation === void 0
            || isLoopbackHostname(pageLocation.hostname)   // localhost / ::1 / 127.0.0.0/8
```

`pageLocation` 就是浏览器地址栏，所以页面只要不是 loopback 主机名（`192.168.190.86`、`chat.tirisen.hk`
都算），面板就报错。实测 `--trusted-host` 只作用于**服务端** `/api` 的防 DNS-rebinding 栅栏，
加上它面板依旧失败；`--host`/`--port` 无关。

### 11.2 dshgw 的开关

`transport?.ownsHost === true` 这条出口正是为"我代理着一个 dsh、并声明它的 host 由我负责"准备的。
dshgw 就扮演这个角色，因此它在租户 shell 文档里注入一行：

```html
<script>globalThis.__DSH_TRANSPORT__=Object.assign(globalThis.__DSH_TRANSPORT__||{},{ownsHost:true});</script>
```

```yaml
settings_ui: lan        # 默认：LAN 页面也能用设置/模型（provider 与 API key 管理）
# settings_ui: loopback # 保留 dsh 原行为：只有 loopback 页面可用
```

实现细节（均有测试）：只改 `text/html`；脚本插在 `<head>` 之后、先于 shell 的模块执行；已带该声明的
文档不再重复注入；**文档请求不索取压缩**（在 gzip 体上改写会让浏览器报 `ERR_CONTENT_DECODING_FAILED`），
子资源保持压缩；无法解码的响应体原样放行、绝不改写；改写后清掉上游 `ETag`。

### 11.3 安全权衡

开启它等于**把 dsh 的"只有本机页面可改设置"换成 dshgw 自己的边界**。dshgw 在前面已经拦住了那两类
混淆代理攻击：Host 必须等于 `public_host`（DNS rebinding 不成立），非安全方法的 `Origin` 必须是该租户
自己的 origin（跨站请求不成立），且整条链路在租户会话与账号 DSH 开关之后。代价：**任何已登录该租户的人
都能从 LAN 页面改这个租户自己的 provider 与凭据**（即他自己的 dsh 主目录）。要禁止这一点就设
`settings_ui: loopback`。

### 11.4 实测对照

| 页面 | `settings_ui: lan` | `settings_ui: loopback` |
|---|---|---|
| `http://127.0.0.1:<port>/` | 正常 | 正常 |
| `http://192.168.190.86:<port>/`（LAN） | **正常**："Models / Enter your API keys to use models from the following providers." | "Loading the provider directory failed: settings are unavailable in this browser" |

本机既有部署 `https://chat.tirisen.hk/dsh/` 之所以能用，也是同一处声明 —— 它注入在自己的 nginx 层
（那份 `/dsh/` 配置本账号无读权限）。

> 更正历史记录：本项目早先一度把这条报错记为"dsh 自身限制、配置无法绕过"，那是**错的** ——
> 当时的对照实验只比较了页面 hostname，漏了 `transport.ownsHost` 这条出口。现已按 §11.2 修复，
> 并在 LAN 页面上实测通过。

## 12. API 数据不缓存

三条路径都**不缓存 API 请求的数据**，因为每个响应都属于某个已认证的租户，被复用就是错的：

| 层 | 覆盖范围 | 做法 |
|---|---|---|
| `gwproxy`（入口） | `/api/`、`/v1/`、`/admin/api/`、`/version`、`/healthz`、`/readyz` | 响应加 `Cache-Control: no-store, no-cache, must-revalidate, max-age=0` + `Pragma: no-cache` + `Expires: 0`；请求上的 `If-None-Match`/`If-Modified-Since` **不转发**（防止用旧副本换 304）；自身的 `/healthz` 同样 no-store |
| `dshgw`（网关） | 租户的 `/api` 与 `/api/…` | 同上。**必须在这一层做**：当前拓扑里 `/t/<租户>/` 是前门跳转，dsh 的 API 流量直接到该租户的端口，不经过 gwproxy；而 dsh 自身对这些响应**不给任何缓存指令** |
| dshgw 门户 | `/`、`/login`、`/logout` | 本来就带 `Cache-Control: no-store` 与 CSP |

静态资源（`/admin/ui/app.css` 等）**不受影响**，保留上游的 `Cache-Control: public, max-age=300` —— 对它们强加
no-store 会让控制台每次打开都重新下载。

写侧前置条件（`If-Match`/`If-Unmodified-Since`）**刻意保留**：它们表达的是乐观并发，不是缓存。

配置：`gwproxy` 的 `api_paths` / `no_store_apis`，dshgw 的 `no_store_apis`（aigw 侧同名键透传给子进程）。
默认全为"不缓存"；显式打开（`no_store_apis: false`）才会允许缓存复用 API 响应。

**实测**（经真实流量）：`:8090/v1/models` → `no-store…`；`:8090/admin/ui/app.css` → `public, max-age=300`（未受影响）；
租户 `POST /api/session/modelCatalog` → `no-store…`；`GET /`（shell）不加 no-store。

## 13. 安全边界（必读）

- **没有 UID 边界**：所有租户 worker 与 aigw 同 UID；隔离来自 bubblewrap mount namespace
  （空 tmpfs 根 + 逐路径绑定 + 只读运行时 + 0700 权限位）与宿主 AppArmor 对嵌套 namespace 的限制。
- **租户数据属主就是运行 aigw 的账号**：任何以该账号运行的进程都能读全部租户的
  `.dsh/.credentials.yaml`。需要"连运行时账号都读不到"的场景应改为分账号/分主机部署。
- **宿主账号的爆炸半径就是隔离失效时的地板**：该账号若在 `sudo`/`docker`/`lxd` 组里，
  namespace 边界一旦失守即等价于 root。建议用专用账号运行 aigw + dshgw。
- per-tenant 的 `gateway.key` 与 `tenant-config` **不挂载进沙箱**；`/etc/dshgw`、`/etc/nginx`、
  `/var`、`/home`、`/srv` 在租户视角里不存在。
- 没有 nginx：租户门户由 dshgw 直接监听。公网部署需自行解决 TLS 与防火墙
  （当前默认明文监听；证书终止是下一步的独立设计）。
- 资源限额见 §6：每 worker 由自己的 systemd 用户 scope 承担（实测生效），部署级汇总上限由单元属性承担。
