# M75 设计文档：把 dsh-tenant 的 3 个插件纳入项目并默认下发

> 状态：**已实现（代码、单测、配置面与文档）；真机下发记录见本文 §10**。
> 上游：租户 profile 的渲染与刷新见 [M64](m64-ssh-workspace.md)（ssh 行）、[M67](m67-dshgw-account-card.md)
> （账号行）；规格：[docs/dshgw.md](../dshgw.md) §7f、[deploy/dshgw/README.md](../../deploy/dshgw/README.md)。
>
> 需求原话：「将 dsh-tenant 的 3 个插件加入项目，并让 dsh 默认安装启动」。

## 1. 目标

`dsh-tenant` 这个租户的 DSH 里有三块很有用的面板，但它们只存在于**该租户自己的 DSH home**：

```
state/tenants/dsh-tenant/.dsh/
  plugins/{web-tty,workspace-files,git-diff}/     # 插件本体（手写，不在仓库里）
  cordis.patch.yml                                # 手写的用户级 patch，三段 insert 引用上面三个目录
```

后果有三个：**（1）**它们不在版本控制里，只有这一台机器上有；**（2）**别的租户没有，要用就得手抄一遍
（8 个租户就要抄 8 次）；**（3）**它们引用的路径在租户自己可写的目录里，与仓库里另外三个网关插件的
做法（root 拥有的共享目录 + 网关渲染的行）不一致。

本里程碑把这三件事一次收口：**源码入库**（成为项目产物）、**网关默认渲染给每个租户**（「默认安装
启动」）、**运行期状态按租户隔离**。

## 2. 这 3 个插件是什么

| 插件 | 目录 | 在租户 DSH 里的表现 | 宿主半 | 浏览器半 |
|---|---|---|---|---|
| 终端 | `cmd/dshgw/plugin/web-tty/` | 侧栏「终端」：浮动终端面板，每个标签一个真 PTY（`/bin/bash -i`），xterm.js 渲染；面板可拖动/缩放/最大化，`Ctrl+\`` 开关 | `index.js`：用 `node-pty`（从 dsh 发行版解析）起会话，长轮询式 `read`，环形回放缓冲 | `client.js`（`vendor/` 里的 xterm.js 6.0.0 拼装，无打包器） |
| 工作区文件 | `cmd/dshgw/plugin/workspace-files/` | 侧栏「文件」：浏览/预览/编辑/上传/下载/改名/删除 | `index.js` + `fs-service.js`：一切路径夹紧在 `config.root` 子树内，符号链接越界即拒 | `client.js` |
| 变更 | `cmd/dshgw/plugin/git-diff/` | 会话主区的「变更」View：左列改动文件，右侧两栏 diff | `index.js` + `git-service.js`：只读，每次 git 调用都带 `--no-optional-locks`，`.git/index` 字节不变 | `client.js` |

三者的宿主半**只用 `node:` 内建与相对导入**（没有裸包依赖），所以放在哪里都能加载；`node-pty` 是唯一
的外部依赖，按 `DSHGW_DSH_ANCHOR`（runner 给每个 worker 注入的 dsh 锚点）解析。

## 3. 已核实的事实（决定了方案形状）

| # | 事实 | 证据 |
|---|---|---|
| F1 | 网关已有同构范式：`cmd/dshgw/plugin/{ssh-workspace,browser-workspace,account-card}/`，行由 `renderPatch` 建、`Ensure*Row` 每次启动刷新 | `internal/dshgw/tenancy/render.go`、`patch.go`、`manager.go` 的 `startWorker` |
| F2 | 行里的 `name` 指向 `filepath.Dir(cfg.Deploy.PluginPath)` 的兄弟目录，而**该目录已被只读绑进每个租户沙箱** | `patch.go`、`sandbox/profile.go`（`pluginDirectory` + `--ro-bind`），运行中 worker 的 bwrap argv 实测可见 |
| F3 | 三个插件各自把状态写在自己目录里：`TRACE_FILE=<pluginDir>/trace.jsonl`，git-diff 另有 `cache.json`；写失败被 try/catch 吞掉 | 三个 `index.js` 的 `TRACE_FILE`/`CACHE_FILE`/`createTracer` |
| F4 | `web-tty` 在租户沙箱里**实测可用**（trace 有 `open`/`write`/`read`，`nodePty` 有版本号）；沙箱是 `--dev /dev --proc /proc` | `…/plugins/web-tty/trace.jsonl`、`sandbox/profile.go` |
| F5 | 只有 `dsh-tenant` 有这三个插件与手写 patch；其余 7 个租户的 `.dsh/plugins/` 为空 | 逐租户列举 |
| F6 | 一个坏行会拖垮**整棵**插件树，不只是那一块面板 | M67 实测（把行指向 `client.js` → `window is not defined`） |
| F7 | 部署手册已有「把 `plugin_path` 同级的插件目录一起部署」的先例 | `deploy/dshgw/README.md`（M67/M65 两节） |

## 4. 决策

1. **引用共享插件目录**（不做按租户拷贝）。插件目录随仓库/部署发到 `plugin_path` 同级，网关只渲染行。
   理由：与既有三个插件完全同构（F1/F2），不新增「安装/升级/漂移」机制，沙箱零改动；而按租户拷贝会让
   每个租户都有一份**自己可改**的副本，反而弱化「网关决定租户看到什么」这条边界。
2. **三个都默认开启**（`tenant_plugins.*.enabled: true`）。「默认安装启动」就是本里程碑的目的；想收窄
   面就显式关掉某一项，关掉即从每个租户的 profile 移除该行。
3. **运行期状态外置到该账号自己的 DSH home**：`<DshHome>/plugin-state/`。共享目录在生产可能 root 拥有
   （写不进去），而且一份共享的 `cache.json` 会把 A 账号的仓库路径喂给 B 账号（F3）。目录由插件自己
   首次写入时创建 —— 它是以**租户账号**身份运行的，只有它才能在自己的 DSH home 下建出属主正确的目录
   （分账号形态下若由 root 预建 0700 目录，租户反而写不进去）。
4. **行 id 沿用租户手写时用的值**：`dshgw-web-tty`、`dshgw-workspace-files`、`dshgw-git-diff`。接管接线
   不改变浏览器半注册的东西。
5. **不新开数值旋钮**：租户 patch 里当初写的 `maxSessions: 8`、`maxRepoDepth: 6`、`chunkTargetFiles: 25000`、
   `chunkTimeoutMs: 90000`、`autoScan: true` **正是插件自身默认值**，渲染时不重复暴露。只暴露总开关与
   `root_label`（默认「工作区」，也是当初手写的值）。
6. **未部署的插件不渲染行**：建户/轮换密钥时**报错**（preflight 语境，半部署的主机应当被点名），
   既有租户启动时**跳过并告警**（一个可选面板不该让账号起不来）—— 依据 F6，坏行的代价是整棵树。

## 5. 配置面

独立形态（`dshgw.yaml`）与监督形态（aigw 的 `config.yaml` → `dshgw.tenant_plugins`）字段同名：

```yaml
tenant_plugins:
  web_tty:
    enabled: true          # 终端
  workspace_files:
    enabled: true          # 工作区文件
  git_diff:
    enabled: true          # 变更
  root_label: 工作区        # 两个工作区面板对 root 的显示名；留空 = 该默认值
```

- 校验：任一开关打开而 `deploy.plugin_path` 为空 → 配置加载失败（行是 `plugin_path` 兄弟目录里的文件）。
  文件**是否存在**不在加载期检查：那是 `dshgw doctor`（三条 `<name>-plugin`）与建户期 preflight 的事。
- **升级注意**：三个插件默认开 ⇒ `directory_picker: browse` 且没有 `plugin_path` 的老配置现在会在加载期
  被拒（它没有目录可放这三个插件，而「声明了默认值却什么都不发生」就是静默降级）。二选一：命名一个
  `plugin_path` 目录并部署三个插件目录，或显式关掉三项。仓库里那类只测别的行为、临时树里没有插件目录的
  夹具也按这条改了口径。
- 监督形态下这块**总是**写进生成的子进程配置，即使三项全关：子进程自己的默认值是**开**，父进程沉默
  等于把运维写的 `false` 又翻回去。这条有测试钉住（`cmd/aigw/dshgw_child_test.go`）。

## 6. 渲染与刷新

| 时机 | 行为 |
|---|---|
| 建户 / 轮换密钥（`renderPatch`） | 三个开关打开且插件已部署 → 三行追加到 profile patch 的那个 `insert` 列表（排在 account-card 之后）；**开着但没部署 → 报错**，错误里带缺失路径 |
| 每次 worker 启动（`EnsureTenantPlugins`） | 逐插件「先删同 id 行、再按当前开关追加」，所以开关翻转、`root_label` 变更都对**既有租户**生效；开着但没部署 → 该行被删掉并返回一条 warning |

三行的配置（`root`/`cwd` 都是**该租户自己的 workspace**）：

| 行 id | 插件目录 | config |
|---|---|---|
| `dshgw-web-tty` | `web-tty/index.js` | `cwd` = workspace、`cwdRoot` = workspace、`traceFile` = `<DshHome>/plugin-state/web-tty.trace.jsonl`、`trace: true` |
| `dshgw-workspace-files` | `workspace-files/index.js` | `root` = workspace、`rootLabel` = `root_label`、`traceFile` = `…/workspace-files.trace.jsonl`、`trace: true` |
| `dshgw-git-diff` | `git-diff/index.js` | `root` = workspace、`rootLabel`、`traceFile` = `…/git-diff.trace.jsonl`、`cacheFile` = `…/git-diff.cache.json`、`trace: true` |

插件侧新增的能力很小：`readConfig` 接受行配置里的绝对 `traceFile`/`cacheFile`，`createTracer` 写前
`mkdir -p`；**不给就还是插件自己目录**（保持它们「可放在 `$DSH_HOME/plugins/` 独立使用」的既有承诺）。
`web-tty` 顺带把 `readConfig` 导出，与另外两个插件一致，好让测试直接断言回退规则。

## 7. 隔离与安全

- **不新增端口、不新增令牌、不新增 capability**：三块面板都走 `ctx.connection.rpc`（租户 origin 下、
  既有鉴权链）与 dsh 自己的槽位注册。
- **终端不等于新权限**：PTY 在租户自己的 bwrap 沙箱里起，能力与该账号 agent 的 bash 工具完全一致。
- **工作区面板的边界是 `root`**：每个路径请求都相对 root 解析并夹紧，符号链接越界即拒。
- **变更是只读的**：没有 stage/checkout/discard 端点，git 一律 `--no-optional-locks`。
- **共享目录里不留状态**：trace/cache 全部按账号落在 DSH home 内；测试直接断言三行 config 里没有任何
  字符串落在插件目录下。
- **租户可改的东西没有变多**：插件代码在共享目录（生产为 root 拥有）；租户能写的只有自己 DSH home 里
  的诊断文件。相对地，共享模型意味着**一处改动影响所有租户**——这是与既有三个插件相同的代价。

## 8. 失败模式与处置

| 现象 | 处置 |
|---|---|
| `plugin_path` 同级缺插件目录 | 建户/轮换报错（含路径）；既有租户启动丢那一行并 warning |
| 共享目录不可写 | 只损失诊断（trace 静默失败），功能不受影响 |
| 某租户手写行未删净（迁移遗留） | 同 id 双份行 → 删掉手写行后重启该租户 worker |
| 某插件激活即失败（如 dsh 升级后 node-pty 缺失） | 配置里单独关掉该插件 → 重启 → 该行被移除，其余两个照常 |
| 整体回滚 | 三个开关置 `false`（或回旧 `bin/dshgw`）→ 重启 → 行被移除 |

## 9. 测试与验收

- **Go**：`internal/dshgw/config/tenant_plugins_test.go`（默认全开、显式关、`root_label`、缺
  `plugin_path` 报错且全关时不再要求）；`internal/dshgw/tenancy/tenant_plugins_test.go`（三个包两半齐备、
  行命名为 `index.js`、`root`/`cwd` 是该租户 workspace、状态按租户且不落在插件目录、开关翻转与幂等、
  未部署时建户报错但启动只告警）；`cmd/aigw/dshgw_child_test.go`（开关原样过河，全关也写块）。
- **JS**：三个插件自己的 `test/*.test.mjs`（14 + 18 + 33 条）已挂进 `make dshgw-test`；web-tty 的测试
  用临时目录的 `traceFile`，不再往仓库里写运行期文件。
- **配置样例**：`config.example.yaml` 与 `deploy/dshgw/config.example.yaml` 都写清了默认值与部署前置。
- **验收命令**：`make dshgw-test`、`make dshgw-verify`。

## 10. 本机下发记录

下发到本机现网（`dshgw-verify.service`，8 个租户）的步骤与证据见 `docs/todo_done.md` 的 M75 小节；
摘要：先把 `dsh-tenant` 手写的用户级 patch 备份成 `.pre-m75-*` 并删掉三段 insert（避免同 id 双份行），
再重建 `bin/dshgw` 并重启 dshgw，然后逐项复核「8 个租户 profile 都有三行 / 各租户
`plugin-state/*.trace.jsonl` 有 `activated` / 共享插件目录里没有运行期文件 / 浏览器里三块面板可用」。
