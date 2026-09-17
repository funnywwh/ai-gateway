# dshgw 部署手册（M51）

`dshgw` 是独立于 `aigw` 与 `dsh` 的进程/发布单元。它只使用：

- aigw：`GET /v1/models` + Bearer Key；
- dsh：公开 CLI、启动日志、HTTP/Host/Origin/Cookie 契约；
- 主机：systemd、nginx、Linux 用户与文件权限。

它不会导入 aigw 内部 Go 包，也不会修改 nginxWebUI 数据库。

## 1. 前置条件

1. 生产多租户启用前，把 aigw 的 `auth.default_grant` 设为 `none`，再显式给每个 Key 授权模型。
2. 确认通配符证书覆盖 `chat.tirisen.hk`，且 nginx 主配置保留：
   `include /etc/nginx/conf.d/*.conf;`。
3. 防火墙仅向受信网络开放 TCP `32600-32799`；不要开放 worker 段 `32100-32299`。worker 只绑定 loopback。
4. 准备 Node 22 与一个完整 dsh release 目录。worker 的 `ProtectHome=tmpfs` 会隐藏 `/home`，所以安装脚本把二者复制到 `/opt/dsh`，不能让 `/opt` symlink 回 `/home`。
5. 预留 `/srv/dsh`、`/var/lib/dshgw` 和足够空间。默认 worker 汇总内存上限为 40 GiB。

## 2. 构建与安装

```bash
make dshgw-verify
sudo env \
  NODE_SOURCE=/home/winger/.local/node-v22.23.1-linux-x64 \
  DSH_SOURCE=/home/winger/.local/dsh-0.1.2-rc.1 \
  DSH_VERSION=0.1.2-rc.1 \
  deploy/dshgw/install.sh
```

安装脚本不会启动服务，也不会覆盖已有 `/etc/dshgw/config.yaml`；有效的 `/opt/dsh/current` 与 Node 选择链接会被保留，避免 dshgw 重装意外降级独立升级过的 dsh。检查并修改该文件后，创建离线 profile 模板：

```bash
sudo deploy/dshgw/prepare-template.sh
```

模板步骤通过一个临时、名字确为 `pnpm` 的 wrapper 调用 Corepack，精确安装
`dsh-browser-fs@0.2.0`，并校验发布 tarball 的固定 SHA-512 integrity。模板保留
`package.json`、`pnpm-lock.yaml`、bundle stack 与完整 `node_modules`；新租户第一次启动不访问网络。

随后生成 nginx 配置并启动 gateway：

```bash
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml render-nginx --reload
sudo systemctl enable --now dshgw.service
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml doctor
```

`render-nginx` 管理 `/etc/nginx/conf.d/dshgw/*.conf`，并原子维护
`/etc/nginx/conf.d/dshgw.conf` include shim。流程为“写候选 → `nginx -t` → reload”；失败会恢复原文件。不要在 nginxWebUI 数据库中复制这些 server。

### 当前主机的阶段 2 运维接续（不是通用安装器）

`scripts/dshgw_stage2.sh` 是本次 `/home/winger/work/ai_gateway` 本地 `:8088` 部署的人工交接脚本，只在用户确认现有 Key 显式授权、已备份并把实际配置改成 `auth.default_grant: none` 后执行：

```bash
# 宿主 root 终端；将短暂重启正在使用的本地 aigw，请先结束在途请求。
bash /home/winger/work/ai_gateway/scripts/dshgw_stage2.sh \
  --confirm-explicit-grants --confirm-restart-aigw
```

该脚本核对 pidfile 对应进程的 UID/可执行文件/配置参数与环境（拒绝未审查的 GW_ 覆盖），更新 dshgw 安装文件，再以原 `winger` 身份重启本地 aigw。只检查新启动日志中的 default_grant=none，不展示日志/配置内容；重验 A/B Key、`deepseek-flash` 授权后启动 **loopback** dshgw 并要求门户200。重启诊断保存在 root 私有文件，出错不要公开原始日志。

若 aigw 重启已成功、但测试模型授权检查未通过，先在管理面补齐测试 Key 的显式标签并复查模型清单，再运行 `bash scripts/dshgw_stage2.sh --verify-and-start`。这个入口不再次安装/重启，只做同一套身份/配置/Key/model 预检并启动回环 gateway；只应在操作者已确认先前重启成功后使用，不是自动绕过运行态确认。

它不更改 aigw 二进制、不编辑授权配置、不重载 nginx、不创建租户、不启用开机启动，也不会关闭 TLS/SSH 校验。其它部署管理方式或使用环境覆盖的实例不能套用，预检会拒绝。阶段 2 的通过输出必须由宿主回传，不能以脚本语法测试当作已重启成功。

本次经用户批准的授权配置回滚副本：`.cache/dshgw-auth/config.before-none-7a4a5aa529a9.yaml`（0600）。恢复该文件并按原管理方式重启会恢复先前的 all 策略，因此**只有在公网租户入口尚未开放、并明确需要回滚时才使用**；不能在生产多租户已开放后静默扩大权限。

## 3. 租户生命周期

API Key 从 stdin 或绝对的 mode-0600 文件读取，**不接受命令行明文 Key**：

```bash
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml \
  tenant create --key-file /root/alice.key alice

sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant list
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant restart alice
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml sync-models alice
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml revalidate alice
```

创建事务执行：验证 Key/模型 → 在全局 flock 下分配端口 → 建立 `dsh-<tenant>` 系统用户 → 复制完整 profile → 写配置 → 启动 worker → 当前 PID 的启动 URL 捕获 → loopback 401 readiness → 标记 handshake OK → enable unit → nginx test/reload。失败时只清理本事务独占建立的资源；既有保留目录/symlink 在 useradd 前被拒绝。如果停 worker 或删用户失败，会保留数据并报告回滚错误，绝不为“清理成功”盲目删数据。

有效 Key 暂无模型时，默认拒绝创建；明确使用 `--allow-empty-models` 会创建
`models_pending=true` 的租户，但不会写一个 DSH 不接受的空 provider。授权模型后执行
`sync-models`。

轮换 Key：

```bash
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml \
  tenant rotate-key --key-file /root/alice-new.key alice
```

`--keep-old-prefix` 会把旧的 12 字节公开前缀无限期保留为登录 alias；默认立即删除旧 alias。轮换会先验证新 Key，并在 dsh 兼容的 `<file>.lock` 下保留 credentials `records`、更新 refs/settings/gateway.key，再原子更新 registry；失败回滚文件与 registry。

删除总是先停 worker 并生成 mode-0600 snapshot：

```bash
# 删除身份与路由，但保留数据目录
sudo dshgw --config /etc/dshgw/config.yaml tenant remove alice

# snapshot 成功后才不可逆清理
sudo dshgw --config /etc/dshgw/config.yaml tenant remove --purge --yes alice
```

## 4. 权限与状态

默认布局：

| 路径 | owner:group | mode | 内容 |
|---|---|---:|---|
| `/etc/dshgw/config.yaml` | `root:dshgw` | `0640` | 非密钥运行配置 |
| `/var/lib/dshgw` | `root:dshgw` | `0751` | 共享父目录；独立 tenant 只需穿过，不需 gateway 组成员资格 |
| `/var/lib/dshgw/tenants` | `root:root` | `0711` | 共享 search-only 父目录，不允许 tenant 枚举或写入 |
| `/var/lib/dshgw/registry.json` | `dshgw:dshgw` | `0600` | canonical tenant registry |
| `/var/lib/dshgw/keys.map` | `root:dshgw` | `0640` | 从 registry 派生的运维索引 |
| `/var/lib/dshgw/gateway/sessions.json` | `dshgw:dshgw` | `0600` | session token 的 SHA-256 与 upstream cookie |
| `/var/lib/dshgw/gateway/audit.jsonl` | `dshgw:dshgw` | `0600` | 不含 secret 的安全审计；logrotate 管理 |
| `/var/lib/dshgw/gateway/activity.json` | `dshgw:dshgw` | `0600` | 与 provisioning registry 分离的登录时间 |
| `/var/lib/dshgw/handshake/<t>.url` | `root:dshgw` | `0640` | 当前 worker PID 的精确启动 URL |
| `/etc/dshgw/tenants/<t>/gateway.key` | `root:dshgw` | `0640` | 可选 revalidate 使用的完整 Key |
| `/var/lib/dshgw/tenants/<t>/.dsh` | `dsh-<t>:dsh-<t>` | 私有 | dsh 状态/profile/credentials |
| `/srv/dsh/<t>` | `dsh-<t>:dsh-<t>` | `0700` 起 | HOME 与 workspace |

共享父目录的 search bit（`x`）与私有叶的权限必须同时成立：root 能读取 settings 不代表 worker UID 能读。`tenant create` 会在发布 registry 前以新 UID 验证 settings/credentials 可读、HOME/DSH_HOME 可写；错误会先停止创建并回滚，而不是启动一个必然 EACCES 的 worker。

gateway unit 对整个 state **目录**设只读，只有 `gateway/` 可写；不要将 registry.json/keys.map 各自设为单文件 bind/只读挂载，否则 root 原子 rename 后运行中的 gateway 可能固定看到旧 inode。

`registry.json` 是唯一授权真相；`keys.map` 是可重建的派生索引。两者都在同一 flock 下原子替换，即使主机在两次 rename 之间掉电，也不会让 gateway 根据半写的 `keys.map` 授权；下一次保存会修复索引。

## 5. 备份与 dsh 升级

```bash
sudo dshgw --config /etc/dshgw/config.yaml backup
sudo dshgw --config /etc/dshgw/config.yaml upgrade-dsh /opt/dsh/releases/<candidate>
```

全局备份持有 lifecycle flock，并先停 gateway 与活跃 workers，以得到一致的 registry、session、credentials、profile 与 workspace 快照，完成后恢复原先活跃的服务，恢复失败会作为命令失败报告。每次归档使用不覆盖旧快照的唯一名字，并含 manifest v1（源路径映射；租户快照另含 registry 行）。已知租户数据缺失或遍历出错时拒绝生成成功快照。归档含明文 tenant Key/upstream cookie，文件固定 `0600`，仍应把备份目录视为 bearer-secret 存储。

`upgrade-dsh` 只接受 `dsh.releases_root` 的直接子目录。它先做全局备份，再在一次性 HOME 上运行 7 项基础 dsh contract 加真实凭据热载 contract，并对候选 release 运行相同 picker seam 测试；全部通过后才原子切换 `/opt/dsh/current`，只对原先 active 的租户执行 restart/readiness。任一失败先恢复旧 symlink，再回滚已尝试过的 workers；链接恢复失败不执行错误版本上的重启，所有恢复失败均明确报告。下载、签名与把候选 release 放入 `/opt/dsh/releases` 属于 release 供应流程，不由 dshgw 猜测。

### 恢复流程（root 维护窗口；尚需目标主机演练）

M51 没有自动 `restore` 命令，不应把租户可写文件里的任意路径直接当作 root 恢复指令。归档也不包含 `/opt` 的 Node/dsh 发行包；运维必须另行记录并保留使用的版本与 current 链接目标。

1. 停止 gateway 与相关 workers，禁止并行生命周期操作；另存当前状态作为恢复前回滚点。不要在业务仍写文件时覆盖 registry/credentials。
2. 用 `tar -tzf <归档>` 审查条目，再读取 `manifest.json` 的版本与 `roots[].archive_path → path` 对照。只信任自己生成的归档，在独立临时目录验证；**不要直接把 tar 解到 `/`**。额外检查 symlink/hardlink 的目标，不跟随链接写入现网目录。
3. 重建/核对 tenant 专用 OS 身份。租户快照的 `manifest.tenant` 保留了 UID、端口、prefix 与目录；UID/名字/端口不得与现有租户冲突。单租户恢复需把该行审慎合入 canonical registry，不能用旧的全局 registry 覆盖其它租户的新记录。
4. 按 manifest 还原状态、workspace 与 `/etc/dshgw/tenants/<t>`。恢复目录的属主/组并核对本手册权限表；credentials/settings 保持 0600、gateway.key/tenant.env 为 root:dshgw 0640。root 管理文件不得落在 tenant 可改的 symlink 路径下。
5. **不要复活旧登录会话。** 保持 gateway 停止，清理旧 sessions 状态及对应 lock（下一次由 dshgw 用户创建）；旧 handshake 不作为新 worker 的凭据使用，重启后由当前 PID 的 capture-url 重建。activity 仅是展示元数据。
6. 确认相应 Node/dsh 发行包仍可用并选择经过契约验证的版本。重新运行 `render-nginx --reload`，依次启动需恢复的 workers 和 gateway，再执行 `doctor`、`revalidate <t>`。
7. 用新门户登录验证 UI、模型调用、跨租户拒绝及文件内容。只有这一实际演练通过才能把恢复能力计入主机验收。

只删除而未 purge 的数据同样不能被后续 `tenant create` 自动覆盖。若 userdel 已尝试后失败，命令会保留 snapshot 路径与数据、保持路由移除；先检查 OS 身份实际状态，再按上述流程恢复，不盲目重试带 purge 的动作。

## 6. 验证

无特权、不会改宿主机的完整验证：

```bash
make dshgw-verify
# 可单独强制执行真实 nginx 非特权链路测试（nginx 必须在 PATH 上）
make dshgw-nginx-test
```

它运行 Go 测试/import gate/vet/static build、真实 picker Cordis 测试、非特权临时 nginx 链路回归（缺 nginx 或以 root 调用时明确跳过，不伪称通过）、systemd unit 解析、本机 dsh 的 7 项基础 disposable contract 加真实凭据热载 contract，以及临时 browser-fs 模板的真实供应与 fresh HOME 无安装启动。HTTP 验证页面及 DSH 广告的插件批量 client URL 为 200、普通 WS GET 为 426、有效 upgrade 为 101；它不声称已验证真实浏览器的文件授权操作。

### 双真实租户验收：由运维在 root 终端分阶段执行

先按 §2 安装并确认 nginx/gateway 正常；**已有租户的主机应先进入维护窗口**，不要把安装/服务重启当作无影响的只读操作。下面的验收脚本本身不安装软件、不改 aigw 配置或防火墙、不调用 sudo、不关闭 TLS 校验，只通过 dshgw 生命周期 CLI 创建/清理本次临时租户。创建/删除会正常 reconcile/reload nginx；已有租户的身份/Key/工作区不被改动。

准备条件：

- 两把**尚未绑定 dsh 租户**的专用测试 Key，在同一 aigw 实例签发，只授予测试模型并设置小额预算；保存到 root 拥有的 mode-0600 文件，不放 argv、不贴到聊天。
- 本例使用已确认的 `http://192.168.190.86:8088`、`deepseek-flash`、`/root/dshgw-e2e/a.key` 与 `b.key`。
- `/root/dshgw-e2e` 及 run 目录父链必须 root 拥有且无组/其它用户写权限。每次 baseline 的 run 目录必须**不存在**，防止覆盖旧验收状态。
- 主机具有 systemd、cgroup v2、`ss`、`runuser` 与 Python 3。确认安装的是本次构建的 dshgw；新 `tenant list --json` 会提供 UID、路径、unit、origin 等只读元数据，不输出 Key/cookie。

**阶段 A — 基线（会创建两个临时租户并产生两次真实、短模型请求）**：

```bash
python3 /home/winger/work/ai_gateway/scripts/dshgw_host_acceptance.py baseline \
  --binary /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml \
  --aigw-url http://192.168.190.86:8088 \
  --key-a /root/dshgw-e2e/a.key --key-b /root/dshgw-e2e/b.key \
  --model deepseek-flash --run-dir /root/dshgw-e2e/run-001 \
  --confirm-create --allow-model-call
```

它先做只读 doctor/认证/模型授权检查，再建立 `m51-e2e-<随机ID>-a/b`：核对两个真实 UID、跨租户 EACCES、worker 进程 UID、loopback listener、实际 cgroup 文件的 1536M/2G/512/200% 及汇总 40G，随后验证 TLS 门户登录、cookie 属性与不泄漏、插件 client 200/WS426/101、跨端口 HTTP/WS 403、cookie 身份绑定、真实 worker 模型请求、重启后的握手自愈和退出隔离。

模型请求走 **租户端口 → gateway → 正在运行的 worker 的公开 RPC**，不是拿同一 Key 绕过 worker 直接请求 aigw。脚本先给自己的新 session 显式命名，避免辅助标题 LLM；用公开 `session/rename` 返回的 seq 作为 `session/page` 游标栅栏，要求自己的 prompt rpcId、同一 turn 的 assistant 回复和 completed 终态。超时/失败会取消该测试 session，并尝试停止身份仍匹配的临时 units，保留数据供检查，不后台继续计费或盲目 purge。

如果 baseline 在模型额度/其它可修复检查失败后停下，先根据私有诊断或安全错误摘要修复原因，不要重复建户或删除 run 目录。两临时worker已被停止且身份仍匹配时，可在明确允许再次产生两次短模型请求后续验：

```bash
python3 /home/winger/work/ai_gateway/scripts/dshgw_host_acceptance.py resume-baseline \
  --run-dir /root/dshgw-e2e/run-001 --confirm-start-workers --allow-model-call
```

续验使用原run记录的Key/model/配置与两个精确租户身份，拒绝override/文件指纹漂移/陌生unit；保留此前所有检查和失败记录，不create/remove/purge，只启动匹配且inactive的临时worker。全部检查通过后留worker active，失败仍安全停止；不要反复重试一个尚未查明的错误。

**阶段 B — 外部 TLS/防火墙（在另一台机器运行；不需要 root 或 Key）**：

只复制 `run-001/report.json` 到另一台机器，然后运行：

```bash
python3 scripts/dshgw_host_acceptance.py external \
  --report /绝对路径/report.json --confirm-external-machine
```

这一阶段使用正常 DNS/网络与系统 CA，检查门户 200、两个无会话租户端口跳回门户 302。主机上的阶段 A 强制 loopback 建连但保留 TLS SNI/主机名验证，**不能**拿它替代外部可达性证据。

**阶段 C — 停用 Key A**：

仅在 aigw 管理面停用本次测试 Key A，保持 Key B 启用，再运行：

```bash
python3 /home/winger/work/ai_gateway/scripts/dshgw_host_acceptance.py revoked \
  --run-dir /root/dshgw-e2e/run-001 --confirm-key-disabled
```

脚本确认 A 的模型清单与 `/v1/responses` 返回 401、B 不受影响、CLI revalidate 与当前 gateway 的 `off/per-request/interval` 策略一致。`interval` 尚未过期会明确失败并要求稍后重验，不偷改生产配置加速。阶段之间不得修改 config 或替换 Key 文件，否则状态指纹检查会拒绝继续。

**阶段 D — 保留证据后清理**：

浏览器本机目录授权、资源压测/恢复等仍需继续测试时，先保留临时租户；不再需要时执行：

```bash
python3 /home/winger/work/ai_gateway/scripts/dshgw_host_acceptance.py cleanup \
  --run-dir /root/dshgw-e2e/run-001 --confirm-cleanup
```

只清理记录中仍然匹配 UID/创建时间/prefix/路径/unit 的本次租户，通过 `tenant remove --purge --yes` **先快照再删除**，快照路径记录在报告；状态漂移、未知的建户残留或 orphan user/data 都拒绝“清理成功”，交由人工检查。脚本不会直接 rm 工作区或更改原有租户身份。最后在 aigw 撤销测试 Key，并按秘密备份策略保存/处置快照。

**报告安全与剩余验收**：

- 只回传 `report.json` 和外部探针 JSON；它们不含 Key 或 browser token。
- **不要分享 `state.json` 或 `cli.log`**：前者含浏览器 bearer cookie，后者是 mode-0600 的本机诊断。两者均留在 root 私有目录。
- 报告的 `overall_complete` 始终为 false：基线通过不代表整个 M51 完成。10–30 worker 资源/压力命中、真实轮换后的计费归因、恢复演练、浏览器授权动作仍需补齐；单独读 cgroup 限额不是压力命中证明。
- 旧的 `DSHGW_E2E_ROOT=1 scripts/verify-dshgw.sh` 只保留单租户基本冒烟能力，不是本阶段双租户验收的替代。

新增热载 contract 使用真实一次性 DSH、假 loopback Responses 服务和 dummy Key：同一 session/PID 先观测 A，再经已发布的文件锁原子更新 refs、等待公开 `credentials/reference-updated` 事件，再观测 B。此自动化证明热载协议，不产生真实模型费用，也不替代真实主机的上述检查。


## 7. 隔离模式：每租户 OS 用户（默认）与 bwrap

`deploy.isolation` 决定租户 worker 的隔离方式。两种模式**不在同一部署内混用**：新租户一律按当前配置创建，已有租户按需迁移（§7.3）。

|  | `user`（默认，兼容旧行为） | `bwrap` |
|---|---|---|
| 每租户 OS 用户 | 是（`dsh-<t>`），真实 UID 边界 | **否**，所有 worker 用共享账号 `worker_user` |
| worker unit | `dsh-worker@.service`（`User=dsh-%i`，systemd 展开 `%i`） | `dsh-worker-bwrap@.service`（`User=dshgw`，`ExecStart=… sandbox-exec %i`） |
| 隔离边界 | UID + 文件权限 | bubblewrap mount namespace（空 tmpfs 根，只挂载租户自己的根与只读运行时） |
| 宿主前提 | 无 | 允许非特权 user namespace，且 `apparmor_restrict_unprivileged_userns=1` |

设计、实测事实与强度取舍见 `docs/design/m57-dshgw-strict-isolation.md`。

### 7.1 配置

```yaml
deploy:
  isolation: bwrap              # user（默认）| bwrap
  worker_user: dshgw            # 可省略，默认取 gateway_user；必须是已存在的非 root 账号
  bwrap_bin: /usr/bin/bwrap
  worker_unit_bwrap: dsh-worker-bwrap@.service
```

### 7.2 安装与切换

```bash
# 1) 安装（两个 worker unit 都会安装；不启动任何服务）
sudo env NODE_SOURCE=/home/winger/.local/node-v22.23.1-linux-x64 \
         DSH_SOURCE=/home/winger/.local/dsh-0.1.2-rc.1 \
         DSH_VERSION=0.1.2-rc.1 deploy/dshgw/install.sh
# 2) 编辑 /etc/dshgw/config.yaml，把 deploy.isolation 改成 bwrap
# 3) 重载 unit 并检查前置条件（bwrap 模式会多出 5 项检查，必须全绿）
sudo systemctl daemon-reload
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml doctor
# 4) 之后 tenant create 直接落在 bwrap 模式；已有租户按 §7.3 迁移
```

`worker_user` 或 `deploy.dshgw_binary` 改动后必须重装 unit 并再次 `doctor`：bwrap unit 的 `User`/`ExecStart` 是静态文本，`doctor` 的 `worker-unit-bwrap` 会交叉核对 unit 与配置，不一致会明确报错而不是静默以错误身份运行。

### 7.3 已有租户迁移（在线；会停一次该租户的 worker）

```bash
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant list      # 看 ISOLATION 列
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant re-isolate --to bwrap alice
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant re-isolate --to user  alice
```

迁移顺序：preflight（目标模式账号/运行时/租户叶权限）→ 停旧 unit → `chown -R` 到目标模式账号 →
更新 registry 的 `isolation` 并落盘 → `daemon-reload` → 启动新 unit → loopback 401 readiness → 恢复 enable 状态。
任一步失败会回滚：停新 unit、属主改回原账号、registry 恢复、重启原 unit。

两点必须知道：**user→bwrap 之后遗留的 `dsh-<t>` 账号不会被删除**（不可逆且无收益，确认无用后可自行 `userdel`）；
`bwrap→user` 会为该租户**新建**账号（共享 UID 无法"还回去"）。

### 7.4 doctor 在 bwrap 模式下的前置条件

配置为 bwrap 或任一租户记录为 bwrap 时追加：

| 检查 | 失败含义 |
|---|---|
| `bwrap-bin` | 配置的 bubblewrap 不存在或不可执行 |
| `bwrap-apparmor-userns` | `/proc/sys/kernel/apparmor_restrict_unprivileged_userns` 不是 `1` |
| `bwrap-sandbox-runtime` | profile 需要的绑定源（bwrap/node/bin.js/release 与 `/usr`、`/usr/lib`、`/usr/lib64`）不可用 |
| `bwrap-worker-account` | `worker_user` 为空、为 root、或账号不存在 |
| `worker-unit-bwrap` | unit 缺失，或 `User`/`Group`/`ExecStart` 与配置不一致 |
| `tenant-<t>-sandbox` | 该租户 profile 已无法构造，或租户叶权限出现 other 位 |

### 7.5 宿主验收（root 终端，单租户）

```bash
# a) profile 评审：确认没有 "--ro-bind / /" 之类宿主根挂载
#    （不需要 sudo：启动器按调用者身份运行，unit 里就是共享 worker 账号）
/opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml sandbox-exec --print alice | less

# b) 建临时租户并确认没有 per-tenant OS 用户
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml \
  tenant create --key-file /root/tcheck.key t-bwrap-check
getent passwd dsh-t-bwrap-check || echo "OK: 无 per-tenant 账号"
# worker 必须是 active（若宿主 systemd 拒绝 unit 的加固属性，这里会看到 218/CAPABILITIES）
systemctl show --property User --property MainPID --property ActiveState --value dsh-worker-bwrap@t-bwrap-check.service
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant list | grep t-bwrap-check

# c) worker 的 readiness 契约：未认证 /api 必须是 401
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:<worker_port>/api

# d) 人工不可见性/可写性确认（在该租户的 dsh 会话终端里执行）
ls -A /home /root /srv /var /etc          # 期望：空/仅白名单；/etc 无 nginx、无 systemd
ls /etc/dshgw 2>&1                        # 期望：空目录
test -e /var/lib/dshgw/registry.json && echo LEAK || echo "OK: gateway state 不可见"
mkdir -p ~/probe/a/b/c && touch ~/probe/a/b/c/f && echo "OK: workspace 可建子目录可写"

# e) 清理
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant remove --purge --yes t-bwrap-check
```

仓库内的自动化只做到真实 bubblewrap + 真实 dsh web 的启动与 401（`make dshgw-sandbox-test`）；上面 d) 的人工确认不能由它替代。

### 7.6 回滚

```bash
# 单个租户回到 UID 边界模式
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml tenant re-isolate --to user <t>
# 整体回到旧模式：把 deploy.isolation 改回 user，逐个迁移，然后复查
sudo systemctl daemon-reload
sudo /opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml doctor
```

### 7.7 强度与限制（必读）

- bwrap 模式**没有 UID 边界**：隔离来自 namespace 视图（空 tmpfs 根 + 逐路径绑定）、租户叶权限位、
  AppArmor 对嵌套 namespace 的限制，以及 dsh 内层 sandbox 的组合，而不是内核身份。
- root 与共享 worker 账号可以进入任何租户的数据；需要"连 root 都不能读租户数据"的场景不应使用本模式。
- 宿主 `apparmor_restrict_unprivileged_userns=1` 是前置条件，否则租户可在 profile 内再套一层 namespace（`doctor` 会失败）。
- 内层 dsh sandbox 在本宿主被 AppArmor 拒绝嵌套 bwrap，dsh 会回退 Landlock；实测仍能拦截工作区外写入，
  但"内外都是 bwrap"在本宿主不成立。
- 租户自己的 `/etc/dshgw/tenants/<t>` 目录**不挂载进沙箱**：`gateway.key` 与 `tenant.env` 由宿主侧 systemd 读取，
  租户在沙箱内看不到它们（与 user 模式一致）。
- 迁移会短暂停 worker（秒级到数十秒），不是零停机操作；请在维护窗口对测试租户先行验证。

## 8. 明确边界

- `user` 模式下 Linux UID/systemd 文件权限是租户安全边界；`bwrap` 模式下边界是 mount namespace 视图
  （没有 UID 边界，root 与共享 worker 账号可读租户数据，见 §7.7）。picker clamp 只是防误操作 UX。Node realpath 后仍有 tenant 自身并发 rename 的 TOCTOU，bind mount 也不表现为 symlink。
- tenant agent 可读取自己的 aigw Key；root 与 `dshgw` 账户可冒充租户。
- workers 共享主机网络 namespace；当前 unit 不声称网络强隔离。
- Cookie 不按端口隔离。所有 `chat.tirisen.hk` HTTPS 服务都会收到所有 `dshgw_s_*` cookie，因此同 hostname 上的服务与其访问日志必须全部可信。cookie overwrite 可造成跨租户 DoS，但 tenant 绑定阻止数据披露。
- 已建立的 WebSocket 不会因 logout/session TTL 即时断开；新请求/重连会被拒绝。upstream 最终 401 会在一次 CAS/singleflight re-handshake 后原样返回。
- `dsh-browser-fs` 会把用户在浏览器明确授权的本地文件传给 tenant dsh，内容可能进入模型请求；默认开启，可按租户 `--browser-fs off`。
