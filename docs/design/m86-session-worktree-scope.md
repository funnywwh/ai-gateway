# M86 「变更」插件跟随当前会话的 git 工作区

## 0. 需求原话

> 变更 插件 应该和当前会话的工作区关联
> 跟随当前会话的 git 工作区
> 不能准确的到当前会话对应的 git 工作区吗？

三句合起来是一件事：会话主区的「变更」View 必须作用在**当前会话真正对应的那个 git 工作区**上，
而不是账号工作区根那一片。第三句是对判定方式的追问——所以本设计的核心是把"哪个工作区"交给
**git 自己**回答，而不是由插件猜目录内容或猜会话活动。

## 1. 现状（实测）

| 事实 | 证据 |
|---|---|
| 行 root 由网关写死为账号工作区（或 M79 短路径） | `internal/dshgw/tenancy/patch.go` 的 `gitDiffRow()`；本机租户 `profiles/web/cordis.patch.yml` 里 `root: /…/state/workspaces/dsh-tenant` |
| 现场表现是"全账号仓库" | `plugin-state/git-diff.trace.jsonl` 最后一条 `discover`：`root=…/state/workspaces/dsh-tenant`，`repos=[work/ai-gateway, work/ai-gateway-m84, work/deepseek-harness, ssh/gw-d/home/operator/ZT20Q]` |
| **git 能给出精确答案** | 只读实测：`work/ai-gateway` → 自身；`work/ai-gateway/internal` → `work/ai-gateway`；`…/work` 与账号根 → `fatal: not a git repository`；`ssh/…/ZT20Q`（sshfs）→ 该挂载路径 |
| 会话 cwd 的 work tree 就是它真正在改的仓库 | 3 个 `workspace = …/work/ai-gateway` 会话的工具调用里 595 处绝对路径引用逐个判 work tree：**100% 落在该仓库内，0 处落别处** |
| "会话真正改过哪个仓库"没有一等事实 | 会话日志只有 header `cwd` + `tool/call` 的 arguments；这些会话以 `bash` 为主（抽查 149/166/171 次，`edit`/`write` 0~35）。DSH 记录"改过哪些文件"的唯一口径是 mutation 工具的 `locations`（`dsh-client-ui-deliverables` 用它渲染 deliverables 行），bash 改动不在其中；抽查的 `…/work` 会话实际在 `/tmp/ssh-probe2`（账号工作区之外）干活 |
| 会话工作区在浏览器侧可精确读到 | `conversation.view` 的 standard props 含 `sessionId` 与根级 `useSessions`（`dsh-client-ui-renderer` 的 `standardKit()`；`dsh-client-ui-session` 的 `BUILTIN_SOURCE`）；同包 `ConversationRoot` 正是 `useSessions((s) => s.byId[sessionId]?.cwd)`；`SessionSummary.cwd` 由宿主 `listFields(header)` 填 |
| RPC 通道不带会话身份 | `dsh-client-connection`：`handler(endpoint, payload, signal)` ⇒ 工作区路径只能由浏览器随请求报送 |
| 宿主半改动必须重启租户 worker | 插件在 `<部署根>/cmd/dshgw/plugin/git-diff/`（租户沙箱内只读绑定，见 `docs/todo_done.md` M75）；宿主半走 Node ESM 缓存，不重启不重载 |

## 2. 设计

### 2.1 作用域判定：交给 git，判不出就不猜

浏览器每次调用带上当前会话的工作区路径 `workspace`（宿主自己发布的 `SessionSummary.cwd`）；
宿主半 `GitService.scopeOf(workspace)` 分三步：

1. **夹紧**：`realpath(workspace)` 必须存在、是目录、且在账号工作区（`config.root`）之内。
   不合格 → `root = config.root`、`anchor='fallback'`，trace `scope-fallback`（这也是"会话没有 cwd"
   的老行为：客户端不发 `workspace` ⇒ 与今天逐字节一致）。
2. **问 git**：`git -C <W> rev-parse --show-toplevel`（沿用插件既有 `BASE_ARGS`：`--no-optional-locks`
   / `core.checkStat=minimal` / `NO_OPTIONAL_LOCKS=0`，只读）。成功且 `realpath(T)` 仍在账号工作区内
   → `root = T`、`anchor='repo'`。这一步覆盖仓库根、仓库子目录、`.git` 为文件的 linked worktree /
   submodule、sshfs 挂载上的仓库——都是 git 自己的判定，插件不做目录猜测。
3. **判不出**（git 说 not a repository，或 `T` 落在账号工作区之外）：`root = W`、`anchor='workspace'`。
   此时只做"该目录下的仓库发现"（既有 BFS 语义），**不解析会话活动反推仓库**——依据见 §1 第 5 行：
   没有一等事实，且样本显示会话可能整个工作在账号工作区之外（`/tmp/ssh-probe2`），从命令行文本猜
   仓库只会给出"看着准、其实可能错"的答案。

`scopeOf` 的结果按 `W` 缓存 30 秒（`Map`，上限 64，超出淘汰最旧），所以每个会话工作区最多起一次
git 进程。

### 2.2 接口（客户端 ↔ 宿主）与宿主内部

请求（所有仓库作用域端点）：`workspace?: string` —— 会话工作区绝对路径。

响应增量（`hello` / `repos`）：

| 字段 | 含义 |
|---|---|
| `workspace` | 被接受的会话工作区（realpath），无/不合格时为 `null` |
| `root` | 有效作用域：`repo` = git 工作区，`workspace` = 会话工作区本身，`fallback` = 账号工作区根 |
| `anchor` | `'repo' \| 'workspace' \| 'fallback'` |

`GitService`：

- `async scopeOf(workspace)` → `{ workspace, root, anchor, reason }`（新）
- `discover({ root = this.root, refresh = false })`：发现缓存改 per-root（`Map<root,{at,repos}>`，
  30s TTL、上限 8），BFS 规则不变
- `resolveRepo(input, { root = this.root } = {})`：相对路径按 `root` 解析，夹紧仍按 `this.root`；
  `repoCache` 键从仓库 realpath 改为 `` `${root}\0${real}` ``（同一仓库在不同作用域下 `rel` 不同）
- `hello(root, anchor, workspace)` / `diag()` 报 `root`、`anchor`、`roots`

### 2.3 浏览器半：跟随会话，切换即重来

- View 从 standard props 读 `sessionId` + `useSessions((s) => s.byId[sessionId]?.cwd)`；两者缺失
  （老 shell、测试夹具）⇒ 不发 `workspace`，行为与今天一致。
- `call()` 唯一入口给每个 payload 加 `workspace`（信封必须带 `payload` 键这条规则保持）；
  `scanStatus`/`scanCancel` 只带 `jobId`。客户端不做任何 git 判断。
- 会话工作区变化 ⇒ `switchWorkspace()`：停轮询 → 若在扫描则 `scanCancel` → 清空
  `repos/repo/meta/staged/scan/rows/selected/diff/error/notice` → `bootstrap()`。插件内 `state` 是
  整页共享的，不重置就会把上一个作用域的仓库/行/差异画到新作用域下。
- `generation` 计数守卫：切换后到达的旧响应一律丢弃。
- 记忆按会话工作区：`localStorage` 的 `repoByWorkspace: { [workspace]: path }`（v1 的单个 `repo` 丢弃）。
- 界面：`anchor='repo'` 标题旁显示仓库名；`'workspace'` 显示「该目录下的仓库」；`'fallback'` 且
  有请求过工作区时显示「会话工作区不可用，已回退到账号工作区」；`'workspace'` 且没有仓库时是**空态**
  「当前会话不在某个 git 工作区里」+ 目录路径，并把该目录下确实存在的仓库列成可点的一行（点一下
  即查看该仓库，标注保持「该目录下的仓库」）。

## 3. 数据流

```
浏览器 变更 View（props.sessionId + useSessions → 会话 workspace）
   └─ 每次调用带 workspace
        └─ 宿主 scopeOf(workspace)
             ├─ 夹紧（realpath + isInside(config.root)）→ 不合格：root=config.root  anchor=fallback
             └─ git -C W rev-parse --show-toplevel（30s 缓存）
                  ├─ T 且 T 在 config.root 内 → root=T      anchor=repo
                  └─ 否则                    → root=W      anchor=workspace
                       └─ discover(root)（per-root 缓存）
                            ├─ anchor=repo：单条目 rel='.'，label=仓库名
                            ├─ anchor=workspace：该目录下的仓库（显式标注、可点选）
                            └─ 空：空态「当前会话不在某个 git 工作区里」
```

安全口不变：仓库与 diff 路径仍 `realpath` 后必须落在 `config.root` 内；`.git/index` 仍不被写。

## 4. 异常与边界

| 场景 | 行为 |
|---|---|
| 会话工作区本身是仓库 | `anchor='repo'`，单条目 `rel='.'` |
| 会话工作区是仓库子目录 | `--show-toplevel` → 所属仓库，`anchor='repo'`，列该仓库全部改动 |
| 会话工作区是仓库父目录（`…/work`、账号根） | git 明确 not a repository → `anchor='workspace'`：空态 + 可点仓库名 |
| 所在仓库的 top 在账号工作区之外（工作区嵌在更大的仓库里） | 不发 `anchor='repo'`（不把账号工作区之外的东西当作用域）→ `anchor='workspace'` |
| 会话工作区被删 / 不可解析 / 被夹紧拒绝 | `anchor='fallback'`，回退账号工作区根 + 界面提示 + trace |
| 会话无 cwd（老会话）或 props 缺失 | 不发 `workspace` ⇒ 配置根（今天的行为） |
| M79 已开、老会话是长路径 cwd | 夹紧拒绝 → 回退（长/短是 bind 的两个视图，`realpath` 不统一；不做别名猜测） |
| 配置了 `repo`（pin；网关行未用） | 忽略会话作用域，pin 说了算 |
| git 不可用 / `rev-parse` 超时（10s） | 退 `anchor='workspace'`（面板本就会因 git 不可用给 fatal 提示） |
| 两个会话/浏览器并发 | 发现缓存按根、jobs/results 按仓库路径；切作用域时旧扫描被 cancel |
| 只读保证 | 不变（index 字节/mtime 测试继续跑） |

## 5. 测试策略

宿主 `test/host.test.mjs`：`scopeOf` 三分支与四种回退、子目录→仓库根、`.git` 文件型 worktree、
top 落在账号工作区之外、同仓库两作用域的 `rel`、per-root 发现缓存、`rev-parse` 结果被缓存。

客户端 `test/client.test.mjs`：每个 payload 都带同一个 `workspace`、切换 `cwd` 触发重新引导
（旧行清空 + 旧扫描 cancel + 新作用域行渲染）、不传 props 时不带 `workspace`、`anchor` 三种标注与
空态可点仓库名。

关键回归（既有 34 条必须全绿）：`.git/index` 字节不变、replaced-inode、两条 wire envelope 守卫。

## 6. 依赖与非目标

- **零 Go 改动**：行渲染（`root` = 夹紧+回退根）与 `tenant_plugins` 开关不变。
- 不改 `web-tty`（终端）与 `workspace-files`（文件）：它们是侧栏面板，作用域仍是账号工作区。
- 不做：长/短路径别名匹配；从会话工具调用反推"改过的仓库"；把列表缩到"会话工作区子目录内"的文件
  （git 改动是仓库级的）。
- 部署：宿主半变了 ⇒ 同步 `<部署根>/cmd/dshgw/plugin/git-diff/` 后重启该租户 worker；只换 `client.js`
  时刷新页面即可。

## 7. 实现与设计差异

- **`anchor='workspace'` 不自动选中仓库**（设计里只写了"空态 + 可点仓库名"）：`loadRepos` 只在
  `anchor='repo'`、`anchor='fallback'`，或该会话工作区此前已记住某个仓库时才打开一个仓库；
  `anchor='workspace'` 且没有记忆时**不猜**，空态列出可点仓库名（点一下即用 `loadRepo` 打开，
  并写进 `repoByWorkspace`）。`'fallback'` 保留自动选中，是为了与 M86 之前的行为逐字节一致。
- **wire 字段最终形态**：请求统一为一个可选 `workspace`；响应为 `{ root, workspace, anchor, reason }`
  （`reason ∈ blank|not-found|outside-root|not-a-directory|not-a-repo|top-outside-root`），
  比设计稿里的"requested/followed"更贴近实际判据，界面文案直接由 `anchor` + `reason` 决定。
- **`scopeOf` 只缓存成功解析过的字符串键**（30s / 64 条，LRU 淘汰最旧），且 trace 只在
  `anchor !== 'repo'` 时写一行 `{"event":"scope"}` —— 正常情形（会话就在仓库里）不刷 trace。
- **`diag` 的口径**：原来的单对象 `discovery` 变成按根缓存，`diag` 改报 `roots`（各发现基准）与
  `scopeCache`（已解析的会话工作区），`repos` 汇总各根的发现结果。
- **测试落地数**：宿主 25 → **34**（新增 9 条：仓库/子目录/非工作区目录/四种回退/仓库 top 在夹紧之外/
  `.git` 文件的 linked worktree/同一仓库两个作用域的 `rel`/按根发现缓存与 `hello` 口径/一次解析的缓存），
  客户端 9 → **14**（新增 5 条：每次调用带会话工作区、无 props 时不带、换工作区重新引导、
  非工作区目录不自动选仓库且可点选、回退提示），`npm test` **48/48**。
- 设计里"零 Go 改动、不加开关"照原样落地：`internal/dshgw/tenancy/patch.go` 的行渲染与
  `tenant_plugins` 开关一字未动。
