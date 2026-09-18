# M63 单一数据根：所有运行态数据默认落在 `./data`

## 1. 需求（用户原话与确认结论）

原话：

> 「这个部署路径太乱了，数据都应该默认放在 ./data 目录下」

逐项确认后的口径：

| 问题 | 用户选择 |
|---|---|
| 改动范围 | **A+B+C+D 全做**：代码默认值与示例、本机验证栈搬进 `./data`、下线遗留 root 形态、清理旧二进制与缓存 |
| 非数据配置文件（`dshgw.yaml`、`gwproxy.yaml`）放哪 | **部署根，与 `config.yaml` 并排**（配置归配置、数据归 `./data`） |
| 旧二进制副本 | **全部归拢到 `./data/prev/` 并保留**（不删回滚点） |
| 遗留 root 形态里的真实租户（E26Q） | **直接删除，只留归档**（`./data/prev/legacy-dshgw/`） |

## 2. 现状（已核实的事实，含证据）

| 事实 | 证据 |
|---|---|
| aigw 侧默认已在 `./data`，但字面量分散在三处 | `internal/config/config.go:733/741/808/853/858`、`internal/hook/dispatcher.go:41`、`internal/pluginhost/host.go:48` |
| dshgw 独立形态默认全是机器路径 | `internal/dshgw/config/config.go:255-266`：`/opt/dsh/node/bin/node`、`/opt/dsh/current`、`/srv/dsh`、`/var/lib/dshgw`、`/opt/dshgw/share/dsh-plugin/picker-clamp.js`、`/opt/dshgw/share/template-home`、`/home/winger/backups/dshgw`、`DefaultPath=/etc/dshgw/config.yaml` |
| 两个 CLI 的默认配置路径在 `/etc/dshgw` | `cmd/dshgw/main.go:33`、`cmd/gwproxy/main.go:37` |
| **监督形态今天用默认库路径会启动失败** | `cmd/aigw/dshgw_child.go:40-43`：`stateDir = filepath.Dir("./data/aigw.db") + "/dshgw"` = `data/dshgw`，非绝对 → 直接报错"must be an absolute path"。子进程侧同样要求"干净绝对路径"（`internal/dshgw/config/config.go:506-530`、`internal/dshgwsup/childconfig.go:307`） |
| **监督形态会静默继承 `/opt/dshgw/...` 默认** | `cmd/aigw/dshgw_child.go:96` 的 `template_home` 与 `:94` 的 `plugin_path` 未配置时为空，子配置 `omitempty` → 子进程回落到自身默认 `/opt/dshgw/share/template-home` 与 `/opt/dshgw/share/dsh-plugin/picker-clamp.js` |
| `DirectoryPicker=clamp` 与空 `plugin_path` 的组合没有任何校验 | `internal/dshgw/tenancy/render.go:270-273` 会用空路径拼出 `file://` URL |
| gwproxy 的 `registry_path` 必填且必须绝对 | `internal/frontproxy/config.go:230`；示例里写死了 `/home/winger/.local/share/dshgw/registry.json` |
| 示例配置里全是机器相关绝对路径 | `deploy/dshgw/config.example.yaml:37-52`（`/home/winger/...`）、`deploy/dshgw/frontproxy.example.yaml:35` |
| 本机有一个跑真实租户的 dshgw，数据在 `~/.local/share/dshgw-verify` | 该目录 15M：`dshgw.yaml`、`gwproxy.yaml`、`state/`、`template-home/`、`current`、日志；两个用户单元 `dshgw-verify`/`gwproxy-verify`。registry 里 5 个租户，其中 4 个对应真实 aigw 账号（`accounts.dsh_tenant` = dsh-colin / dsh-lianchangliang / dsh-ranqiliang / dsh-tenant） |
| 租户数据里嵌了旧绝对路径 | `state/registry.json`（`dsh_home`/`workspace`）、`tenants/*/.dsh/profiles/web/cordis.patch.yml`（picker `root:`）、`tenants/*/.dsh/storages/workspace.json`、`tenants/*/.dsh/storages/session_projcache/sessions/*.json`；而 `state/workspaces/**` 是租户自己的内容（含一份本仓库 clone） |
| registry 里的路径被强校验，必须与 root 推导一致 | `internal/dshgw/registry/registry.go:177`、`internal/dshgw/tenancy/backup.go:92`、`internal/dshgw/sandbox/profile.go:205-206` |
| 遗留 root 形态仍在运行 | `dshgw.service` + `dshgw-admin.service` + 6 个 `dsh-worker@`；`/opt/dshgw`、`/etc/dshgw`、`/var/lib/dshgw`；nginx `conf.d/dshgw/{01-portal,tenant-*}.conf` 在 `0.0.0.0:32600+` 终止 TLS 并转发 `127.0.0.1:3099` |
| `sudo` 需要交互密码 | `sudo -n true` → `sudo: interactive authentication is required` |

## 3. 目标布局

```
<部署根>/                        # 本机 = /home/winger/work/ai_gateway（unit 的 WorkingDirectory）
  config.yaml                    # aigw 配置（非数据，gitignore）
  dshgw.yaml                     # 独立形态 dshgw 配置（非数据）
  gwproxy.yaml                   # 入口反代配置（非数据）
  bin/                           # 构建产物：aigw / dshgw / gwproxy / aigw-provider-*
  plugins/                       # 供应商插件二进制（产物；aigw plugins.dir 扫描目录）
  data/                          # ★ 唯一数据根（gitignore）
    aigw-local.db{,-wal,-shm}    database.path
    aigw-local.log / .pid        scripts/local-run.sh 的日志与 pidfile
    backups/                     backup.dir
    plugin-state/                plugins.state_dir
    billing-fallback.jsonl       billing.fallback_file
    hooks-dead.jsonl             hooks.dead_letter
    dshgw/                       ★ 监督形态：<db 所在目录>/dshgw（aigw 生成）
    dshgw-verify/                ★ 本机独立 dshgw（历史命名；实为本机在用的 DSH 网关）
    prev/                        ★ 历史归档：README.md、bin/、legacy-dshgw/
```

**三分法**（`docs/deployment-layout.md` 的规格主体）：

| 类别 | 判据 | 归属 |
|---|---|---|
| 数据 | 运行期可写、可重建或可丢、随部署走 | `./data/...`（相对部署根） |
| 产物 | 构建或部署拷入、不是运行期状态 | `bin/`、`plugins/` |
| 运行时安装 | 机器相关、由运维安装（node、dsh release、bwrap、证书） | 绝对路径，显式配置或 `DSHGW_*` 环境变量 |

## 4. 关键决策（含取舍理由）

1. **相对路径以"进程工作目录"为基准**，部署根即 unit 的 `WorkingDirectory`（`scripts/local-run.sh` 已 `cd "$ROOT"`）。
   *取舍*：另一种做法是"相对配置文件所在目录"。选工作目录是因为它与现有 aigw 行为一致、改动最小，
   且两个部署入口（systemd 单元、local-run.sh）都显式设定了工作目录。代价是"从别处直接跑二进制"会把
   `./data` 建在别处 —— 用启动日志打印解析后的绝对数据根来消除这个不确定性（见决策 6）。
2. **`<db 所在目录>/dshgw` 这条推导保留**，不新增"默认 `./data/dshgw`"的字面量。数据根 = 库所在目录，
   运维把库放到别处时 dshgw 状态跟着走，不会出现"库在 A、状态在 B"的第二个根。
3. **配置文件放部署根**（`./dshgw.yaml`、`./gwproxy.yaml`），`./data` 只放数据。
   aigw 自动生成的子进程配置 `<state_dir>/config.yaml` 属"生成物"，留在数据根内（它是 aigw 的产物，
   运维不应手改）。
4. **补齐"必须显式"的两项，消除隐藏的 `/opt` 回落**：
   - `deploy.plugin_path` 改为**显式配置**（示例与 `config.yaml` 给相对路径 `./cmd/dshgw/plugin/picker-clamp.js`），
     并在 `directory_picker: clamp` 且为空时报错。*取舍*：另一条路是"派生 `<state_dir>/plugin/picker-clamp.js`
     并在启动时把仓库里的插件拷过去"——那是新机制（等于让二进制分发插件资产），超出"路径收敛"的范围。
   - `dsh.node_bin` / `bin_js` / `current_link` 默认改为空 + `DSHGW_NODE`/`DSHGW_DSH_ROOT` 兜底
     （与 aigw 侧 `dshPath()` 同规则），最终仍必须是绝对路径：它们是**运行时安装**，不是数据。
5. **不新增共享常量包**。`docs/architecture.md` 的分层表（`internal/arch/layering_test.go`）规定
   `internal/dshgw/**` 只能 import `internal/dshgw/**`、`internal/frontproxy` 不 import 任何 internal 包，
   因此共享一个"数据根常量"包会破坏既有分层断言。做法：aigw 侧在 `internal/config` 定义
   `DefaultDataDir = "./data"` 并由它派生，dshgw/frontproxy 各自保留本地字面量，
   用**跨包断言测试**钉住"三处默认值指向同一个数据根"。
6. **启动可观测**：`aigw starting` 日志新增 `data_dir` 与 `dshgw_state` 两个字段（解析后的绝对路径）。
   理由：把"数据落在哪"从"读代码/猜"变成"看一行日志"，这正是本次需求要消灭的模糊。
7. **C 按"直接删除，只留归档"执行**：不把遗留租户迁进新形态。E26Q 的 DSH 会停服（用户已确认），
   归档留在 `./data/prev/legacy-dshgw/`。*取舍*：迁移（`scripts/migrate_dshgw_to_supervised.sh --apply`）
   能保住那个租户，但它会把 `/var/lib/dshgw` 的树按旧位置继续用，与"单一数据根"相悖；用户选择归档 + 重建。
8. **不改本机拓扑**：`config.yaml` 的 `dshgw.enabled` 保持 `false`，4 个真实租户继续由独立 `dshgw-verify`
   （搬进 `./data` 后）服务。把本机 dshgw 收进 aigw 监督形态会让 aigw 重启波及 DSH 会话，是独立话题。
9. **不发版**：`VERSION` 与 gpt001 本次不动（gpt001 已是 `/opt/aigw/{config.yaml,data}` 同根形态，
   天然符合新规则）。需要发版时按 skill `release-version` 独立执行。

## 5. 接口与改动点

### 5.1 `internal/config`（aigw 配置）

```go
// DefaultDataDir is the single runtime data root: every stateful default below is
// derived from it, and nothing in it is a machine-specific path. Relative paths are
// resolved against the process working directory (the deployment root).
const DefaultDataDir = "./data"
```

`Default()` 由它派生：`Database.Path`、`Plugins.StateDir`、`Billing.FallbackFile`、`Hooks.DeadLetter`、
`Backup.Dir`。`Plugins.Dir` 保持 `./plugins`（产物目录）。

### 5.2 `internal/dshgw/config`（独立形态）

- `defaults()`：`StateDir=./data/dshgw`；`WorkspaceRoot`/`Deploy.BackupDir`/`Deploy.TemplateHome`
  由 `StateDir` 派生（`applyDerivedDefaults` 里与既有 `TenantRoot` 同处）；`Dsh.*` 为空；
  `Deploy.PluginPath` 为空；`DefaultPath = "./dshgw.yaml"`。
- 新增 `func (c *Config) resolvePaths() error`：把 `state_dir`、`registry_path`、`key_map_path`、
  `session_path`、`audit_path`、`activity_path`、`handshake_dir`、`tenant_root`、`workspace_root`、
  `deploy.{plugin_path,template_home,backup_dir,tenant_config_root,config_path}`、
  `dsh.{node_bin,bin_js,current_link}` 逐项 `filepath.Abs`（相对路径以工作目录为基准），
  在 `applyDerivedDefaults()` 之后、`Validate()` 之前调用；`Validate()` 的"干净绝对路径"不变量保持不变。
- 新增环境兜底：`dsh.node_bin`/`bin_js`/`current_link` 为空时读 `DSHGW_NODE`/`DSHGW_DSH_ROOT`
  （`bin_js` 由 `DSHGW_DSH_ROOT/lib/bin.js` 推导），与 `cmd/aigw/dshgw_child.go:dshPath()` 同规则。
- `Validate()` 新增：`directory_picker == "clamp"` 且 `deploy.plugin_path` 为空 → 报错（点名该键）。

### 5.3 `internal/frontproxy`（gwproxy）

- `registry_path` 默认 `./data/dshgw/registry.json`；`Validate()` 先 `filepath.Abs` 归一，再判干净绝对路径。

### 5.4 `cmd/aigw`（监督形态）

- `buildDshgwChild`：`stateDir`/`TenantRoot`/`WorkspaceRoot` 的显式值先 `filepath.Abs`（默认推导出的
  `<db 目录>/dshgw` 同样归一）；`TemplateHome` 默认 `<stateDir>/template-home`；
  `PluginPath` 透传，clamp + 空值在**启动期**报错（早于租户创建）。
- 启动日志增加 `data_dir`（解析后的数据根）与 `dshgw_state`。

### 5.5 CLI 默认值

`cmd/dshgw/main.go` 的 `-config` → `./dshgw.yaml`；`cmd/gwproxy/main.go` 的 `-config` → `./gwproxy.yaml`。

## 6. 数据流（本机最终形态）

```
aigw (user unit aigw-local, WorkingDirectory=<部署根>)
 ├─ :8088 数据面 + 控制台
 ├─ 库 ./data/aigw-local.db、日志/pid、备份、插件状态、兜底文件 —— 全在 ./data
 ├─ dshgw 子进程：默认关闭（dshgw.enabled=false）；若打开，状态在 ./data/dshgw
 └─ 不监督 gwproxy

dshgw (user unit dshgw-verify, --config ./dshgw.yaml)
 ├─ state ./data/dshgw-verify/state（registry/sessions/keys.map/handshake/tenants/workspaces/tenant-config/backups）
 ├─ 门户 18300、租户 18301+（每个租户独立 origin）、worker 18400+（bwrap）
 └─ template-home ./data/dshgw-verify/template-home

gwproxy (user unit gwproxy-verify, --config ./gwproxy.yaml)
 └─ :8090 前门：/ → aigw；/dshgw、/t/<t> → 302 到 dshgw 的端口；registry_path 指向上面的 state
```

## 7. 异常与边界

| 场景 | 行为 |
|---|---|
| 相对路径但工作目录不是部署根 | 数据落在该工作目录下的 `./data`；启动日志的 `data_dir` 让人一眼看出 |
| `dshgw.state_dir` 是相对路径 | 归一后通过（不再是"必须绝对"的失败）；用户手册写清基准 |
| `directory_picker: clamp` 未配 `plugin_path` | 配置加载失败，错误信息点名 `deploy.plugin_path`（不再静默用 `/opt` 默认） |
| `dsh.node_bin` 等既未配置也无环境变量 | 校验失败（必须显式给出运行时安装位置） |
| 搬移验证栈时租户仍在线 | 必须先 `systemctl --user stop`；未停就 mv 会让 worker 的绑定路径失效 |
| 租户文件里的旧前缀 | 只改写 dshgw 管理的 4 类文件，`workspaces/**`（租户内容）绝不改写；搬移后用 grep 断言 |
| 遗留 root 形态的删除 | 先归档并校验，再 `--apply`；脚本默认 dry-run；回滚 = 从归档拷回 + 恢复单元与 nginx conf |
| 旧 `./aigw`（根目录陈旧副本） | 归拢到 `./data/prev/bin/` 并在 README 里标注来源，不静默删除 |

## 8. 测试策略

| 层 | 断言 |
|---|---|
| `internal/config` | `Default()` 的全部数据路径以 `./data` 开头；与 `hook.DefaultConfig().DeadLetter`、`pluginhost` 默认一致（跨包钉住"一个数据根"） |
| `internal/dshgw/config` | 默认 state/workspace/template/backup/registry 都在 `./data/dshgw` 之下；相对路径归一后仍是"干净绝对路径"；clamp 缺 plugin_path 报错；`DSHGW_*` 环境兜底 |
| `internal/frontproxy` | 相对 `registry_path` 归一为绝对（并更新原来"必须绝对"的断言） |
| `cmd/aigw` | 相对库路径 + 未配 `dshgw.state_dir` → 状态根 = `<cwd>/data/dshgw`，且生成的子配置能被 `config.Load` 反向解析通过（今天这条必失败）；clamp 缺 `plugin_path` 在启动期报错 |
| `cmd/dshgw` | `deploy/dshgw/config.example.yaml` 改成相对路径后仍"按原样可加载"（现有测试） |
| `scripts` | `verify-dshgw.sh` 适配相对示例（把 `./data` 前缀与 node/dsh 重写进 `$TMP`）；新增下线脚本的计划测试（dry-run 零执行、关键步骤齐全） |
| 端到端 | `make verify`、`make dshgw-test`、`make dshgw-verify`；运行期 curl 清单见 §9 |

## 9. 验收（运行期）

1. `make verify` / `make dshgw-test` / `make dshgw-verify` 全绿。
2. `make build dshgw-build gwproxy-build` + `systemctl --user restart aigw-local`（宿主终端或本会话的
   `systemctl --user`；**不要**用 `scripts/local-run.sh start`，沙箱会回收进程）→ `:8088/version` 200、
   `/admin/ui/` 200、启动日志出现解析后的 `data_dir`/`dshgw_state`。
3. 搬移后的 dshgw：门户 200、4 个真实租户 `/api` 401、gwproxy `:8090` 路由不变、`doctor` 通过。
4. 遗留形态：`32600+` 不再监听、`systemctl list-units 'dsh*'` 无旧单元、`nginx -t` ok、
   `./data/prev/legacy-dshgw/` 归档完整（含 INDEX）。
5. `git grep` 断言：代码/示例/脚本里不再有作为**数据默认值**的机器路径（只剩运行时安装、合成夹具、历史文档）。
6. `~/.local/share/dshgw-verify`、`/opt/dshgw`、`/etc/dshgw`、`/var/lib/dshgw` 均不存在；
   `du -sh .cache` 明显下降（保留工具链缓存）。

## 10. 依赖与影响面

- 依赖：无新增第三方依赖；不改数据库 schema；不改对外 HTTP 契约。
- 行为变化（需写进规格文档）：
  1. dshgw 独立形态的**默认路径**从 `/var/lib/dshgw`、`/srv/dsh`、`/opt/dshgw/...` 变为 `./data/dshgw/...`；
  2. dshgw 配置里相对路径从"直接报错"变为"以工作目录为基准归一"；
  3. `directory_picker: clamp` 现在**要求** `deploy.plugin_path`（此前为空会静默拼出 `file://` 空路径）；
  4. CLI 的 `-config` 默认值从 `/etc/dshgw/*` 变为 `./dshgw.yaml`、`./gwproxy.yaml`；
  5. 监督形态在默认（相对）库路径下**从"启动失败"变为可用**，且不再回落到 `/opt/dshgw` 默认。
- 不受影响：`config.yaml` 现有内容（路径已是 `./data/...`）、`:8088` 数据面与控制台契约、gpt001 部署、
  `data/aigw-local.db` 与 `data/backups` 的内容。

## 11. 实现与设计差异

设计基本按原样落地，差异与"实现中发现的缺陷"如下。

### 11.1 实现时发现的既有缺陷（本里程碑顺带修掉）

| 缺陷 | 证据 | 处置 |
|---|---|---|
| 监督形态在默认（相对）库路径下**根本起不来** | `filepath.Dir("./data/aigw.db")+"/dshgw"` = `data/dshgw` 非绝对 → `must be an absolute path` | `resolveChildPath()` 归一后再判；新增回归测试（含子配置反向解析） |
| 监督形态静默继承 `/opt/dshgw/...` 默认 | `template_home`/`plugin_path` 为空 + 子配置 `omitempty` → 子进程用自身默认 | `template_home` 派生 `<state_dir>/template-home`；`plugin_path` 无默认且 clamp 时必填，启动期报错 |
| `deploy.tenant_config_root` 默认与 `tenant_root` 相同 | 两处都派生 `filepath.Join(StateDir, "tenants")`，而沙箱会绑定 `<tenant_root>/<t>` → `gateway.key` 落在绑定范围内 | 改为 `<state_dir>/tenant-config`，与监督形态一致；测试断言两者不相等 |
| `admin_socket` 在独立形态没有默认值 | `serve` 只在 `AdminSocket != ""` 时建立供应通道 → 不给就静默没有 | 派生 `<state_dir>/admin.sock`（与 aigw 侧推导同位置）；本机实测 `nc -U` 可用 |
| `dsh.*` 三个运行时字段为空时整份配置加载失败 | 空值会被"干净绝对路径"检查拒绝 → `doctor`（专为报告缺前置条件而生）无法运行 | 三项改为"可以空、非空则须干净绝对"；缺失交给 `doctor`/契约检查报告 |

### 11.2 设计外的补充

- **`resolvePaths` 对相对路径中的 `..` 直接报错**：`filepath.Abs` 会把 `./data/../x` 静默清理成
  部署根之外的位置，与"数据都在数据根下"的承诺冲突。拒绝比清理更诚实，三个解析点
  （aigw 子进程配置、dshgw 独立配置、gwproxy registry）口径一致。
- **CLI 默认配置路径**也一并收敛到部署根（`./dshgw.yaml`、`./gwproxy.yaml`）：设计只写了"配置放部署根"，
  `-config` 的默认值属于同一件事。
- **新增 `scripts/move_dshgw_state.sh`**：设计里 B 是"搬 + 改写"，实现把它固化成一个 dry-run 默认的脚本，
  因为要改写的文件类是有边界的（4 类）而搬移本身不可逆。
- **下线脚本读旧配置**（`state_dir`/`registry_path`/`nginx_include_path`）而不是假定默认路径：
  旧形态允许这些被改过，假定默认会把归档做错。另加 `--state-dir`/`--nginx-dir` 显式覆盖。
- **`data/prev/README.md` 由实现生成**（逐份二进制自报版本、归档时间与回滚命令）：设计只要求"归拢并保留"，
  但一份没有索引的回滚点等于没有回滚点。
- **`.gitignore` 增加 `/dshgw.yaml`、`/gwproxy.yaml`**：它们与 `config.yaml` 同级，且 `dshgw.yaml`
  带着飞书票据密钥（本机那份就是真实值）。
- **删掉了被误提交进 git 的 `./aigw`**（22MB 调试构建）：M63 把它当作"陈旧的根目录副本"归拢，
  实现时发现它在 git 里，于是连同工作树一起清掉，归档留在 `data/prev/bin/`。

### 11.3 上线实测暴露并修掉的脚本缺陷

首次在生产形态上跑 `sudo scripts/decommission_legacy_dshgw.sh --apply` 时，归档成功、删除被安全校验挡住
（**没有删任何东西**），原因是校验用了 `tar -tzf … | grep -qF …`：`grep -q` 命中即退出，`tar` 收到 SIGPIPE
（141），`set -o pipefail` 于是把成功的归档报成失败。这条缺陷恰好说明"dry-run 测不到校验代码"：
计划测试只断言计划文本，永远走不到校验分支。修法有三条，都进仓库：

1. 校验改为先把清单写进临时文件再 `grep -qxF`（无管道，无 SIGPIPE）；
2. 新增 `--archive-only`：只做归档与校验、不碰服务（也就意味着不需要 root），于是这条真实路径可被测试执行；
3. 计划测试新增用例，用夹具真的跑一遍"归档 + 校验 + 索引"，并断言它不动任何被归档的目录。

同时按实测发现补齐了覆盖面：旧配置里的 `workspace_root`（本机 `/srv/dsh`）与 `deploy.backup_dir`
此前不在归档里（也不在删除列表里，属于"没人记得的遗留数据"）；现在两者都进归档、都**不删**，
并在运行时逐条打印"保留"清单，归档目录里还写一份 `INDEX.md`（内容、旧租户、回滚命令）。

### 11.4 未按设计做的部分

- **未发版**：设计已声明不发版，实现保持一致（`VERSION` 仍 1.3.0）。代价是控制台角标与线上 revision
  在本次收尾时不变，运维无法仅凭版本号区分"含 M63 的 1.3.0"；已在 `docs/TODO.md` 记为待定项。
- **C 的 `--apply` 未执行**：需要交互式 `sudo`，本会话只做到"脚本 + 计划测试 + 本机 dry-run 复核"，
  执行步骤写在 `docs/TODO.md`（含"会停掉真实租户 E26Q"的提醒）。
- **本机 dshgw 仍是独立形态**（未收进 aigw 监督形态）：理由见决策 8，记为可选项而非遗漏。
