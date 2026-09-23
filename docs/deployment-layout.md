# 部署布局与单一数据根

> 状态：**已实现（M63）**。设计文档：`docs/design/m63-data-root.md`。

本文件规定 ai-gateway 的部署只有**一个数据根**：`<部署根>/data`。
代码里的默认值、示例配置与脚本一律使用相对路径，**不出现任何机器相关的绝对路径**；
"数据落在哪"由部署根决定，而不是由某个人的家目录或某台机器的 `/var/lib` 决定。

## 1. 三类路径（先分清，再谈默认值）

| 类别 | 判据 | 归属 | 例 |
|---|---|---|---|
| **数据** | 运行期可写；丢了要重建，或者本来就该丢 | `<部署根>/data/...` | 数据库、日志、pid、备份、插件状态、租户状态、工作区 |
| **产物** | 构建或部署时拷入；不是运行期状态 | `<部署根>/bin/`、`<部署根>/plugins/` | `bin/aigw`、`bin/dshgw`、`bin/gwproxy`、`plugins/aigw-provider-*` |
| **运行时安装** | 机器相关，由运维安装 | 绝对路径，显式配置或 `DSHGW_*` 环境变量 | Node、dsh release、`bwrap`、TLS 证书 |

**配置文件不属于数据**：`config.yaml`（aigw）、`dshgw.yaml`（独立形态 dshgw）、`gwproxy.yaml`（入口反代）
放在**部署根**，与数据根并排。

## 2. 目标布局

```
<部署根>/                        # 部署根 = 进程的工作目录（unit 的 WorkingDirectory）
  config.yaml                    # aigw 配置（含密钥，勿入 git）
  dshgw.yaml                     # 独立形态 dshgw 配置
  gwproxy.yaml                   # 入口反代配置
  bin/                           # 产物：aigw / dshgw / gwproxy / aigw-provider-*
  plugins/                       # 产物：供应商插件（aigw plugins.dir 扫描这里）
  data/                          # ★ 唯一数据根
    aigw.db | aigw-local.db      database.path
    aigw-local.log / .pid        scripts/local-run.sh 的日志与 pidfile
    backups/                     backup.dir（数据库备份）
    plugin-state/                plugins.state_dir（插件凭据与会话）
    billing-fallback.jsonl       billing.fallback_file（结算兜底）
    hooks-dead.jsonl             hooks.dead_letter（hook 死信）
    dshgw/                       监督形态：aigw 生成的子进程配置与租户状态
      state/ssh-mounts.json      SSH 工作区的挂载记录（M64，0600）
      state/workspaces/<账号>/ssh/  SSH 工作区挂载点（0700，在该账号 workspace 之内）
      state/workspaces/<账号>/.ssh/ 该账号的 ssh 身份（id_rsa 0600）、known_hosts 与别名清单 config（0644）；
                                  另有可选的 identity-managed（0600）：表示该身份归账号自己管，网关不再种密钥
      ssh-configs/<账号>         该账号别名清单的**初始种子**（运维资产，0644；由 ssh_config_dir 指定）
      nodes.json                 多机形态（M77）：控制面登记的节点（0600；含令牌与部署状态）
      node-ssh/<节点>/           多机形态：该节点的部署私钥（id_ed25519 0600）与固定指纹（known_hosts）
      node-deploy/<节点>.log     多机形态：一键部署的分阶段日志（有界；管理面只暴露尾部）
    dshgw-verify/                本机独立 dshgw 的 state / template-home / 日志
      ssh-configs/<账号>         同上（本机形态的种子目录，`scripts/ssh_config_adopt.sh` 默认写入这里）
      ssh-keys/<账号>            该账号密钥的预置来源（运维资产，0600；由 identity_dir 指定，一账号一把）
    prev/                        历史归档（回滚二进制、下线形态的归档），非活动数据
```

多机形态（M77）在**工作节点机器**上另有一棵自己独立的根，默认即目标机上的部署根
（一键部署写在 `node.deploy.dir` 指定的目录，示例用 `/srv/dshgw-node`）：

```
<srv/dshgw-node>/                # 节点机器的部署根（在目标机本地盘上，与控制面互不共享）
  dshgw-node.yaml                # 该节点的配置（一键部署生成，或运维手写）
  node-a.token                   # 节点令牌（0600；位置由 node.token_file 指定）
  bin/dshgw                      # 该节点的二进制（与控制面同版本同 revision）
  plugins/                       # picker-clamp.js 与三块租户插件（一键部署推送）
  template-home/                 # 该节点自己的租户模板（默认由控制面推送，或本机 prepare-template.sh 产出）
  state/                         # state_dir：registry.json（该机分配表）、handshake/、tenants/、tenant-config/、backups/
  workspaces/                    # workspace_root：该节点承载租户的工作区
  .prev/                         # 上一版本（一键部署回滚用）
```

## 3. 相对路径规则

- 所有数据默认值都是相对路径（`./data/...`），**以进程的工作目录为基准**。
  两个部署入口都显式设定工作目录：systemd 用户单元的 `WorkingDirectory=<部署根>`、
  `scripts/local-run.sh` 的 `cd "$ROOT"`。
- 从别处直接运行二进制（工作目录不是部署根）时，`./data` 会建在那个工作目录下。
  为避免"数据到底落在哪"靠猜，aigw 启动日志会打印解析后的绝对数据根：

  ```
  msg="aigw starting" … data_dir=/home/winger/work/ai_gateway/data dshgw_state=/home/winger/work/ai_gateway/data/dshgw
  ```

- 配置里写相对路径是允许的（dshgw 与 gwproxy 同样在加载时归一为绝对路径），
  但**运行时安装**（`dsh.node_bin`、`dsh.bin_js`、`dsh.current_link`、`deploy.bwrap_bin`、TLS 证书）
  必须是绝对路径——它们描述的是"这台机器上装在哪"，不是数据。

## 4. 默认值一览

### 4.1 aigw（`config.yaml`）

| 配置键 | 默认值 |
|---|---|
| `database.path` | `./data/aigw.db` |
| `plugins.dir` | `./plugins`（产物目录） |
| `plugins.state_dir` | `./data/plugin-state` |
| `backup.dir` | `./data/backups` |
| `billing.fallback_file` | `./data/billing-fallback.jsonl` |
| `hooks.dead_letter` | `./data/hooks-dead.jsonl` |
| `dshgw.state_dir` | 未设置时 = `<database.path 所在目录>/dshgw`（默认即 `./data/dshgw`） |
| `dshgw.template_home` | 未设置时 = `<dshgw.state_dir>/template-home` |
| `dshgw.tenant_root` / `workspace_root` | 未设置时 = `<dshgw.state_dir>/{tenants,workspaces}` |

### 4.2 dshgw 独立形态（`dshgw.yaml`）

| 配置键 | 默认值 |
|---|---|
| `-config` | `./dshgw.yaml` |
| `state_dir` | `./data/dshgw` |
| `tenant_root` / `workspace_root` | `<state_dir>/{tenants,workspaces}` |
| `registry_path` / `key_map_path` | `<state_dir>/{registry.json,keys.map}` |
| `session_path` / `audit_path` / `activity_path` | `<state_dir>/gateway/{sessions.json,audit.jsonl,activity.json}` |
| `handshake_dir` | `<state_dir>/handshake` |
| `admin_socket` | `<state_dir>/admin.sock` |
| `deploy.tenant_config_root` | `<state_dir>/tenant-config` |
| `deploy.backup_dir` | `<state_dir>/backups` |
| `deploy.template_home` | `<state_dir>/template-home` |
| `deploy.plugin_path` | **无默认值**：`directory_picker: clamp` 时必须显式配置（否则配置加载失败） |
| `deploy.config_path` | `./dshgw.yaml` |
| `dsh.node_bin` / `bin_js` / `current_link` | 空 → `DSHGW_NODE` / `DSHGW_DSH_ROOT` 兜底；仍须为绝对路径 |
| `deploy.bwrap_bin` | `/usr/bin/bwrap`（运行时安装） |
| `deploy.sandbox_workspace` | 空 = 关闭（M79）：工作区在沙箱里只有宿主长路径一个视图；配 `/workspace` 时另绑一个短路径，`HOME`/`~`、目录选择器、终端与两个工作区面板都用它 |

### 4.3 gwproxy（`gwproxy.yaml`）

| 配置键 | 默认值 |
|---|---|
| `-config` | `./gwproxy.yaml` |
| `registry_path` | `./data/dshgw/registry.json`（指向 dshgw 的 registry；相对路径同样以工作目录为基准） |

### 4.4 工作节点（`dshgw-node.yaml`，M77）

节点机器上的默认值与上面同一套派生规则（同一份配置类型），**只有 `node:` 段与运行时安装路径是本机特有的**：

| 配置键 | 默认值 / 要求 |
|---|---|
| `-config` | `./dshgw-node.yaml`（一键部署写在 `node.deploy.dir` 里） |
| `node.name` / `node.listen` / `node.token_file` | **必填**；名字必须与控制面记录一致，`listen` 必须显式地址（拒绝 `0.0.0.0` 与空值） |
| `state_dir` | `./data/dshgw-node`（一键部署写 `<deploy.dir>/state`） |
| `tenant_root` / `workspace_root` | `<state_dir>/{tenants,workspaces}` |
| `registry_path` / `key_map_path` | `<state_dir>/{registry.json,keys.map}`（该节点的**分配表**，由控制面推送） |
| `handshake_dir` | `<state_dir>/handshake` |
| `deploy.template_home` / `template_home` | `<state_dir>/template-home`（一键部署推送，或本机 `prepare-template.sh` 产出） |
| `deploy.plugin_path` | **无默认值**（与独立形态同一门禁） |
| `dsh.node_bin` / `bin_js` / `current_link`、`deploy.bwrap_bin` | 本机绝对路径（运行时安装，预检逐项校验） |
| `deploy.sandbox_workspace` | 空 = 关闭（M79）；多节点部署给同一短路径（如 `/workspace`）租户体验才一致 |
| `worker_port_lo` / `worker_port_hi` | 由节点记录给出；必须是本机未被占用的段 |
| `aigw_base_url` | **必填**（worker 直连 aigw 取模型，不经过控制面） |
| `admin_socket`、门户/公开端口、会话、飞书 | 节点模式**不使用**（不绑定门户与公开端口，不写会话） |


## 5. 本机布局实例

`<部署根> = /home/winger/work/ai_gateway`：

| 组件 | 单元 | 配置 | 数据 |
|---|---|---|---|
| aigw（`:8088` + 控制台） | `aigw-local.service`（用户单元，enabled） | `./config.yaml` | `./data/aigw-local.db`、`./data/{backups,plugin-state}` |
| dshgw（门户 `:18300`、租户 `:18301+`） | `dshgw-verify.service`（用户单元，enabled） | `./dshgw.yaml` | `./data/dshgw-verify/state`、`./data/dshgw-verify/template-home` |
| gwproxy（前门 `:8090`） | `gwproxy-verify.service`（用户单元，enabled） | `./gwproxy.yaml` | 无（只读 dshgw 的 registry） |

> 名字里的 `verify` 是历史命名：它现在是**本机在用的** DSH 网关，承载真实租户。

宿主安装 dsh 与 Node 的位置（`dsh.node_bin`、`dsh.bin_js`、`dsh.current_link`）是**运行时安装**，
按本机实际路径写绝对路径或用 `DSHGW_NODE`/`DSHGW_DSH_ROOT`。

## 6. 常用运维命令

```bash
# dshgw 独立形态
bin/dshgw --config ./dshgw.yaml doctor                    # 只读前置条件检查（含路径与权限）
bin/dshgw --config ./dshgw.yaml sandbox-exec --print <租户>
printf '%s\n' '{"id":1,"op":"tenant-list"}' | nc -U ./data/dshgw-verify/state/admin.sock

# 准备租户模板（产物落在数据根内）
DSHGW_NODE=… DSHGW_DSH_ROOT=… DSHGW_TEMPLATE_HOME=./data/dshgw/template-home \
  deploy/dshgw/prepare-template.sh

# aigw（本机用户单元；不要在 DSH 沙箱里用 local-run.sh start，进程会被回收）
systemctl --user restart aigw-local && curl -s localhost:8088/version
```

## 7. 迁移与下线

- **搬移 dshgw 状态根**：先停 dshgw（`systemctl --user stop dshgw-verify`），`mv` 目录树，
  改写 `registry.json` 的 `dsh_home`/`workspace`、租户的 `profiles/web/cordis.patch.yml`、
  `storages/workspace.json` 与 `storages/session_projcache/sessions/*.json` 里的旧前缀，
  再改配置与单元、启动、逐个租户验收。
  **绝不要改写 `state/workspaces/**`**：那是租户自己的文件。
  工具：`scripts/move_dshgw_state.sh`（默认 dry-run）。
  **别忘了状态树之外的消费者**：上面的脚本只改写树内部的文件，而 aigw 侧 `config.yaml` 的
  `dshgw.admin_socket` 指的是 `<state_dir>/admin.sock`（本机为
  `./data/dshgw-verify/state/admin.sock`）。搬完家它还指着旧前缀时，控制台的「启用/停用 DSH」
  与飞书首次登录的自动开通都会拨一个空地址（`dshgw admin channel unavailable at …`）；
  这个值必须跟着 `dshgw.yaml` 的 `state_dir` 一起改（aigw 启动时会为此打一条 `WARN`）。
- **下线遗留 root 形态**（`/opt/dshgw` + `/etc/dshgw` + `/var/lib/dshgw` + nginx `conf.d/dshgw`）：
  `scripts/decommission_legacy_dshgw.sh`（默认 dry-run 打印计划，`--apply` 才执行，需要 root）。
  它**先归档再删**：`/var/lib/dshgw`、`/etc/dshgw`、nginx 的 `conf.d/dshgw`、旧单元文件，
  以及旧配置里写明的 workspace 根与备份目录（后两者只归档、**不删除**），归档落在
  `<部署根>/data/prev/legacy-dshgw/` 并附一份 `INDEX.md`（内容、旧租户清单、回滚命令）；
  校验每个 tar 都能列回、且 state 归档里含 `registry.json` 之后，才停单元、删 nginx 转发、删三处目录。
  **归档是唯一的回滚来源**。
  `--archive-only --apply` 只做归档与校验、不碰任何服务（想在动手前先拿到回滚源时用）；
  `--etc-dir`/`--state-dir`/`--nginx-dir` 用于非默认位置。
- **历史二进制**：`./data/prev/bin/` 保存历次回滚点（含根目录那份陈旧 `aigw`），
  `./data/prev/README.md` 记录来源与用途；回滚 = `cp data/prev/bin/<file> bin/aigw` +
  `systemctl --user restart aigw-local`。

## 8. 排障

| 现象 | 原因与处置 |
|---|---|
| 数据出现在意料之外的位置 | 看启动日志的 `data_dir=`；确认进程工作目录（unit 的 `WorkingDirectory`）就是部署根 |
| dshgw 报 `<键> must be a clean absolute path` | 该项属"运行时安装"或含 `..`/空段；数据类键写 `./data/...` 即可（会被归一） |
| 配置加载报 `deploy.plugin_path` 缺失 | `directory_picker: clamp` 需要目录选择器插件：给出仓库里的 `./cmd/dshgw/plugin/picker-clamp.js` |
| 搬移后目录选择器/工作区列表指向不存在的路径 | 租户文件里的旧前缀没改全：按 §7 的 4 类文件重写（`registry.json`、`cordis.patch.yml`、`workspace.json`、session cache） |
| 租户 `/api` 从 401 变成 502/无响应 | worker 起不来：`bin/dshgw --config ./dshgw.yaml doctor` 看路径与权限，或看 dshgw 日志里的 bwrap profile |
