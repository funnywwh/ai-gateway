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
  path: /home/winger/work/ai_gateway/data/aigw-local.db
credentials_key: "<32 字节>"
dshgw:
  enabled: true                 # aigw 启动时拉起并监督 dshgw
  # binary: ""                  # 默认：aigw 同目录的 dshgw
  state_dir: /home/winger/.local/share/dshgw
  public_host: localhost
  listen: 127.0.0.1:18099       # dshgw 自己的监听地址（租户门户直接由它服务）
  portal_port: 18100
  tenant_port_lo: 18101
  tenant_port_hi: 18199
  worker_port_lo: 18200
  worker_port_hi: 18299
  template_home: /home/winger/.local/share/dshgw/template-home
  plugin_path: /opt/dshgw/share/dsh-plugin/picker-clamp.js
  plugin_browser_fs: on         # on 需要准备了 browser-fs 的模板，否则用 off
  public_listen: 127.0.0.1      # 门户与租户公开端口的绑定地址（默认 loopback）
  # tls_certificate: /path/fullchain.pem      # 指定证书即在这些端口上启用 HTTPS
  # tls_certificate_key: /path/privkey.pem
  node_bin: /home/winger/.local/node-v22.23.1-linux-x64/bin/node
  current_link: /home/winger/.local/dsh-0.1.2-rc.1   # bin_js 由它推导
```

未设置时按 `DSHGW_NODE`/`DSHGW_DSH_ROOT`/`DSHGW_TEMPLATE_HOME` 环境变量兜底。

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

实现方式是**每个 worker 一个 systemd 用户 scope**（`systemd-run --user --scope --unit=dshgw-worker-<t>
-p MemoryMax=… -- <bwrap argv>`），不需要 root。

为什么不用 dshgw 自己建子 cgroup：cgroup v2 有一条"无内部进程"规则 —— **含有进程的 cgroup 不能把控制器
下放给子 cgroup**。systemd 服务自己的 cgroup 里总有主进程，因此 `aigw-local.service` 的
`cgroup.subtree_control` 在本机实测**根本写不进去**（同一目录下 `mkdir` 可以，写控制器不行）。用户 scope
没有这个问题：用户管理器拥有被委托的树，由它创建 scope 并设限额，实测生效：

```
cgroup=…/dshgw-worker-<tenant>.scope  memory.max=2147483648  pids.max=512  cpu.max=200000 100000
```

**整个部署（单元级汇总上限，替代旧 slice 的 `MemoryMax=40G`）**

```bash
scripts/aigw_user_service.sh install --memory-max 40G --tasks-max 4096 --cpu-quota 800
```

降级行为：宿主没有用户管理器（例如把 dshgw 单独跑在没有 systemd 的环境里）时，限额**不可用但 worker 照常启动**，
并按部署告警一次 —— 缺一条配额不该变成一个起不来的租户。该行为有测试钉住。

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

## 9. 安全边界（必读）

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
