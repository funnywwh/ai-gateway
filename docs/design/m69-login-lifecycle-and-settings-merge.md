---
description: "M69：租户 settings 的『平台段/租户段』归属与合并、每次登录同步、退出即强制停掉该租户的 dsh worker。"
kind: "design"
---

# M69 设计文档：登录驱动的租户生命周期与「平台段 / 租户段」设置合并

> 状态：**设计已定稿，等确认后写代码**。
> 相关规格：[docs/dshgw.md](../dshgw.md) §3b（目标行为先行写出）。
> 需求来源：用户 2026-09-21 直接要求（原话见 §1）。

## 1. 目标与非目标

用户要求（2026-09-21）：

> 就是 dshgw 要合并租户手动设置，平台的模型限制使用平台的，其他用租户的，不要碰宿主机的，
> 同步要发生在用户每次登录时，用户点击退出，强制退出 dsh 服务。

拆成四条可验收的目标：

1. **设置的归属**：dshgw 写租户 `settings.yaml` 时只拥有「平台段」（平台的模型限制），
   其余一切键保持租户自己的值——即"合并"，不是整份覆盖，也不是整份让给租户。
2. **不碰宿主机的 settings**：`~/.dsh/settings.yaml`（操作者自己那份手写配置）既不读也不写，
   而且要落成可执行的断言，不只是口号。
3. **每次登录同步**：用户每次登录（门户 Key 登录、飞书登录）都重新把平台段从 aigw 同步一次。
4. **退出即停**：用户点击退出后，强制停掉该租户的 dsh 服务（worker 进程）。

非目标：

- **不做**「租户继承操作者宿主机 dsh 设置」的模板同步。宿主机那份是手写配置、不是 dshgw 租户
  （M68 设计 D5 已明确）。
- 不改 dsh 发行包、不引入 URL 前缀垫片（M51 的边界不变）。
- 不改 dshgw 重启后的启动策略（见 D8）。
- 不管「会话自然过期/TTL 到期」是否停 worker——本里程碑只覆盖"点退出"（见 §5 边界表）。

## 2. 现状与证据（2026-09-21 本机实测）

### 2.1 现状：合并语义已有一半，触发时机与生命周期缺失

- **合并已经存在**：`internal/dshgw/tenancy/render.go:111 renderSettings` 读入租户现有
  `settings.yaml`，只写 `llm-pi-ai.providers.aigw` 与「由 aigw 派生的 `agent-default-model` 纠正」，
  其它键（`permission`、`ui-theme`、`ui-onboarding`、租户自建的 provider）原样保留。
- **触发时机只有三处**：建户、`dshgw sync-models <tenant>`（`cmd/dshgw/ops.go:64`）、
  **worker 启动前**（`cmd/dshgw/modelrefresh.go` 的 hook，经 `tenancy/manager.go:577 startWorker`）。
- **登录不刷新**：`internal/dshgw/proxy/proxy.go:372` 只调 `KeyAdopter.AdoptKey`——租户已有 key 时
  直接返回 `false`，既不刷新模型，也不保证 worker 在跑。
- **退出不停服务**：`proxy.go:429`（门户 `POST /logout`）与 `proxy.go:700`（租户侧栏
  `POST /dshgw/logout/`）只删会话、清 cookie；worker 照旧运行到进程/主机重启。
- **凭据没有归属**：`.credentials.yaml` 的 `refs.AIGW_API_KEY` 只在建户与轮换时写；租户侧把它删掉后
  没有任何时机恢复。

### 2.2 今天线上两处偏离（正是"缺归属"的后果）

1. `data/dshgw-verify/state/tenants/dsh-tenant/.dsh/settings.yaml`（09:37:18）：
   `llm-pi-ai.providers: {}` + `agent-default-model: aigw/stealth/union-alpha` + `permission` +
   `ui-onboarding`；同一时刻 `.credentials.yaml` 的 `refs: {}`（**没有 `AIGW_API_KEY`**）。
   这个形状 dshgw 渲染不出来：渲染器不写 `permission`/`ui-onboarding`，且模型为空时是**删键**
   而不是写空的 `providers` map。即：平台段被租户侧（租户页面的设置保存）写坏，而当时没有任何
   时机把它修回来——该租户的模型页从此是空的。
2. 其余四户（`dsh-colin`/`dsh-lianchangliang`/`dsh-ranqiliang`/`dsh-yangmiao`）的 `settings.yaml`
   只剩 `ui-onboarding`：09:22 启动那轮同步拿到 `models=0`
   （`data/dshgw-verify/dshgw.log`：`tenant models refreshed before worker start tenant=dsh-colin models=0`），
   渲染器按"空列表 = 平台段为空"删掉了 aigw 段。而现在用它们各自的
   `data/dshgw-verify/state/tenant-config/<t>/gateway.key` 查 aigw 已经有 4 个模型
   （`curl -H "Authorization: Bearer …" http://127.0.0.1:8088/v1/models`）。
   只要没有下一次启动/登录，这个错误状态就一直留着。

> 结论：缺的不是"合并"本身，而是**归属的强制力**（谁写坏都能被下一次同步纠正）与
> **同步/生命周期的时机**（登录同步、退出停服务）。

## 3. 关键决策

### D1 归属的单位是「键」，不是「文件」

| 段 | 谁拥有 | 具体内容 |
|---|---|---|
| 平台段 | dshgw，每次同步重写 | `llm-pi-ai.providers.aigw`（模型清单 = 该租户 **worker key** 在 aigw 的授权结果）；由它派生的 `agent-default-model` 纠正；`.credentials.yaml` 的 `refs.AIGW_API_KEY` |
| 租户段 | 租户，dshgw 只做"读写时的原样保留" | `llm-pi-ai.providers.<其它 provider>`、`llm-deepseek`、`ui-theme`、`permission`、`ui-onboarding`、`agent-default-model` 指向非 aigw provider 时的值、`.credentials.yaml` 的其它 `refs` 与**全部** `records` |

- **为什么不整份文件归平台**：会抹掉租户的 UI 偏好与自建 provider，而 dsh 自己也在写同一个文件
  （`dsh-settings-file` 按 namespace 做叶子级 diff），双方互相抹除无法收敛。
- **为什么不整份文件归租户**：平台就收不回被删掉的 provider 与凭据，§2.2 的两处偏离永远修不回来，
  "平台的模型限制"也就只是建户那一刻的快照。
- **反例保护（有牙）**：租户（手工或经租户页面）增删 aigw 段、改它的 `baseURL`/`apiKeyEnv`/模型列表、
  或删掉 `AIGW_API_KEY` 引用，下一次登录同步都会被平台段覆盖回授权结果；租户自建的 provider 不受影响。
- **`agent-default-model` 的边界**：仅当它**当前指向 aigw** 且该模型已不在授权清单里时改写为清单首项；
  指向租户自有 provider 时一律不动（那是租户的选择），完全不写时按现有行为写入清单首项。

### D2 平台段的模型来源是「租户存储的 worker key」，不是登录提交的那把 key

`<tenant-config-root>/<tenant>/gateway.key` → `GET /v1/models`。理由：dsh 调用 aigw 用的是 worker key，
模型授权按它算；同一账号可以有多把 key 且授权不同（本机 `dsh-tenant` 的 key 现在 6 条模型，
其余四户各 4 条）。
登录提交的 key 只在**租户还没有 key** 时被采纳（沿用现有 `AdoptKey` 语义：不轮换已有 key，理由见
`cmd/dshgw/admin_serve.go:131` 的注释）。

### D3 同步时机 = 每次登录（两条登录路径共用同一段代码）

门户 `POST /login`（Key 登录）与门户 `/login/feishu`（飞书票据）都调用同一个 `LoginPrepare` hook；
既有的建户 / `sync-models` / worker 启动前三条路径保持不变。

失败策略沿用 `modelRefreshHook` 的取舍：aigw 401/403/不可达、读不到 key、worker 启不来 → **只告警，
不阻断登录**（已经验证过的身份不该因为一次配置刷新失败而被拒绝），但日志点名租户与原因。

### D4 登录必须确保 worker 在跑（同步启动 + readiness probe）

因为 D5 让退出会停掉 worker，登录必须把它拉回来，否则重定向落到 502。细化：

- worker 已在跑 → **不重启**（不打断在用的会话；模型由 dsh 的 settings-file 监听热更新，见 M68 §6）。
- 未在跑且**未被运维停用**（`registry.suspended == false`）→ `startWorker`（内部会再跑一遍模型 hook，
  幂等）+ probe。
- 运维停用（`suspended == true`）→ 不拉起，日志点名（运维意图优先，且这类账号在 aigw 侧已被
  `POST /v1/dshgw/authorize` 拒掉）。
- 启动失败不阻断登录（页面会 502，直到下次登录或运维介入），但日志与审计留下原因。

### D5 退出的停 = 该租户的 worker 无条件停掉（2026-09-21 部署当天修正）

- 门户 `POST /logout`：对本浏览器被撤销会话的每个租户逐个停；
  租户侧栏 `POST /dshgw/logout/`：只停该租户。
- 只对**这次退出真正撤销了会话的租户**动手：判定用 `Sessions.Get(cookie)` 必须成功且
  `session.Tenant` 等于该租户——否则一个伪造的 cookie 名就能把别人的 dsh 打掉。
- **为什么不是"最后一个会话退出才停"（初版设计，实现后被真机数据否掉）**：浏览器关掉标签页后
  会话在 TTL（本机 7 天）内依然有效，所以"这个租户已经没人了"用会话数判不出来。本机实测：
  `dsh-tenant` 有 **16 个存活会话**、其中 15 个是 09-18～09-20 的旧会话，TTL 还剩 4 天——
  "最后会话"规则等于用户点完退出后 dsh 还要跑好几天，直接违背需求。
  代价：同一个人**另一个窗口**的 dsh 也会被停掉（租户 = 账号 = 一个人），那个窗口重新登录即可；
  这个代价写进了规格（`docs/dshgw.md` §3b）与部署手册（§12b），并在审计里可见。
- 停的实现：`WorkerRunner.Stop`（SIGTERM → 超时 SIGKILL，含 scope 回收），**不写** `suspended`——
  写进去会（a）让 dshgw 重启后不再拉起该租户，（b）把租户生命周期与运维的"启用/停用"状态混在
  一个字段里。清理顺序与 `StopWorker` 一致：先停进程，再 `BrowserWorkspaces.DropTenant`（关掉
  browser-fs 工作区、卸掉挂载）。
- 结果：`session.Store.CountTenant` 这个初版为"最后会话"加的方法**没有消费者**，实现里删掉了
  （不留死接口）。

> **2026-09-22 追补（M76 修正上面的清理顺序）**：真机发现"先停进程、再卸载"在挂载卸不动时会把挂载留在
> 内核里——本机 `dsh-tenant/browser/ZT20Q` 连续一小时 `EBUSY` 重试刷屏，三次退出全记
> `logout_worker_stop_failed`（dsh 其实已经停了），`DropTenant` 的失败还会**短路**掉后续步骤，而 sshfs
> 挂载从来没被碰过。顺序改为**排除 → 强制卸载（浏览器 FUSE + sshfs）→ 最后强杀 dsh**，失败不再短路，
> 详见 [M76](m76-dsh-exit-force-teardown.md)。其余口径（无条件停、只动真的撤销了会话的租户、不写
> `suspended`）不变。

### D6 停发生在写响应之前，但有超时上界；停失败不影响退出成功

退出请求最坏会等到 `Runner.StopTimeout`（默认 20s）+ SIGKILL 后 5s 回收；正常情况 dsh 在 1s 内退出。
停失败只告警 + 审计（`logout_worker_stop_failed`），浏览器仍拿到 303 回门户。

> **2026-09-22 追补（M76）**：上界随"先卸载后停"变成 `logoutStopTimeout = 55s`（两段卸载各 ≤15s、
> 停 dsh ≤30s）；门户一次退出多租户另加 150s 整体预算（超出者审计 `logout_worker_stop_skipped`）。
> 审计的 `reason` 由错误**类型**改为错误**正文**（本次现场就是因为只记 `*fmt.wrapError` 而查不下去）。

### D7 平台段为空（该 key 在 aigw 没有任何模型）= 平台段为空

删除 `llm-pi-ai.providers.aigw` 与「由 aigw 派生的默认模型」，保留租户自有 provider 与文件其它内容
（沿用现有渲染器行为）。凭据 ref 仍然保留/恢复：key 有效，只是这一刻没有模型授权（授权恢复后，
下次登录同步即补回清单）。

### D8 不改 dshgw 重启后的启动策略

`Manager.StartWorkers` 仍按 registry 拉起"未被运维停用"的租户（M58 行为）。也就是「退出即停」是
**运行期**行为：dshgw/aigw 重启会把它们拉回来。若操作者要"没人登录就不起"，需另开一项
（在 `StartWorkers` 里按存活会话过滤会让重启后的行为依赖会话 TTL，要单独评估，不在本里程碑）。

### D9 「不碰宿主机 settings」要落成可执行的断言

现状即正确（dshgw 除测试外没有读取 `os.UserHomeDir` 的代码，也没有 `~/.dsh` 字面量），但本里程碑
把它固化成：新增测试在 `t.Setenv("HOME", tmp)` 下跑完整个登录同步，断言 `$HOME/.dsh` 既没有被创建、
也没有被修改；规格文档显式写明这条边界。

## 4. 接口（类型与签名）

```go
// internal/dshgw/session
type Store interface {
	// …（现有方法不变）
	// CountTenant reports how many unexpired sessions this tenant still holds. Logout reads it
	// to decide whether the last reader left.
	CountTenant(tenant string) (int, error)
}

// internal/dshgw/tenancy
// EnsureRunning starts the tenant's worker when it is not running and the operator has not
// suspended the tenant; it reports whether it started one.
func (m *Manager) EnsureRunning(ctx context.Context, t registry.Tenant) (started bool, err error)

// StopForLogout stops the tenant's worker without recording an operator suspension, with the
// same cleanup order as StopWorker.
func (m *Manager) StopForLogout(ctx context.Context, t registry.Tenant) error

// EnsureCredentialRef restores the platform's credential reference in a tenant's
// .credentials.yaml, preserving every other ref and every record.
func EnsureCredentialRef(path, key string) (changed bool, err error)

// internal/dshgw/proxy
// LoginPrepare is the login moment's lifecycle hook: it re-applies the platform slice of a
// tenant's dsh configuration from aigw and makes sure the tenant's worker is up.
type LoginPrepare interface {
	PrepareLogin(ctx context.Context, tenant, submittedKey string) error
}

// LogoutStop stops a tenant's dsh once its last session has signed out.
type LogoutStop interface {
	StopSignedOut(ctx context.Context, tenant string) error
}
```

`Proxy` 的 `KeyAdopter` 字段由 `LoginPrepare` 取代（`AdoptKey` 的语义保留在
`cmd/dshgw` 的 `managerOps.PrepareLogin` 内部，作为"租户还没有 key"的那一步），
这样"登录"只有一个接缝、只有一条顺序。

`submittedKey` 为空表示没有提交 key 的登录（飞书票据）：此时跳过"采纳 key"这一步，只用租户存储的 key。
`cmd/dshgw` 侧新增 `loginPrepare.go`（或并入 `modelrefresh.go`）实现该 hook，`serve.go` 接线
`gateway.LoginPrepare = ops`、`gateway.LogoutStop = ops`。

### 数据流

登录（`POST /login` 或 `/login/feishu`）：

```
校验 Key（aigw GET /v1/models）
  → authorizeDSH（账号是否启用 dsh）
  → resolveTenant / 票据里的租户
  → LoginPrepare.PrepareLogin(ctx, tenant, key)
        ├─ 租户无存储 key 且有 submittedKey → AdoptKey（落 gateway.key + 首次产物）
        ├─ 读 <tenant-config-root>/<tenant>/gateway.key
        ├─ GET /v1/models（该 key）
        ├─ EnsureProvisioned / SyncModels      ← 平台段：providers.aigw + agent-default-model 纠正
        ├─ tenancy.EnsureCredentialRef         ← 平台段：refs.AIGW_API_KEY
        └─ Manager.EnsureRunning + probe       ← worker 没跑就起来（suspended 则不）
  → Issue 会话 → Set-Cookie → 302 租户 origin
```

退出（门户 `POST /logout`、租户 `POST /dshgw/logout/`）：

```
校验 Origin / 方法
  → Sessions.Delete(cookie 对应的会话)
  → 对"这次真的撤销了会话"的每个租户（Sessions.Get 成功且租户匹配）：
        LogoutStop.StopSignedOut(ctx, tenant)
          → Manager.StopForLogout → Runner.Stop（SIGTERM→SIGKILL）+ BrowserWorkspaces.DropTenant
        （审计 logout_worker_stop；失败则 logout_worker_stop_failed，但不影响退出本身）
  → 清 cookie → 303 回门户
```

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| aigw 不可达 / 超时 / 5xx | 不改 settings，不阻断登录；worker 若没跑则尝试启动（同一原因失败→告警） |
| worker key 被 401/403 | 同上，日志点名租户与原因；不阻断登录 |
| 租户被运维停用（`suspended`） | 登录不拉起 worker；日志点名；账号级授权通常已在 aigw 侧拒绝 |
| 租户没有存储 key（手工/迁移） + 登录提交了 key | 采纳（现有 AdoptKey 路径），随后照常同步 |
| 租户没有存储 key + 飞书登录（无提交 key） | 记一条告警、跳过平台段同步；登录继续（实际到不了：`recheckFeishuAccess` 读不到 key 已经失败） |
| 授权模型列表为空 | D7：清空平台段，保留租户段与凭据 ref |
| `settings.yaml` / `.credentials.yaml` 不存在 | 走现有 `EnsureProvisioned` 从零渲染 |
| 退出时 worker 本来没跑 / 正在停 | 幂等，不算错误，不写审计失败 |
| 退出时该租户还有别的存活会话 | **仍然停**（D5 修正：会话数判不出"没人了"）；另一个窗口重新登录即恢复 |
| 退出时停 worker 失败/超时 | 告警 + 审计 `logout_worker_stop_failed`，浏览器仍 303（退出本身成功） |
| 会话 TTL 自然过期（不是点退出） | 不停 worker（本里程碑只覆盖"点退出"） |
| dshgw / aigw 重启 | D8：按 registry 拉起未停用租户（既有行为） |
| 租户手工改坏 aigw 段或凭据 | 下次登录同步覆盖回授权结果（D1 的反例保护） |

## 6. 测试策略

| 层 | 用例 |
|---|---|
| `session`（新） | `CountTenant` 只数未过期、只数该租户；空租户名报错；与 `DeleteTenant` 一致 |
| `tenancy/render`（新） | `EnsureCredentialRef`：缺失→补齐并保留 `records` 与其它 refs；相等→不写（mtime 不变）；版本非 1→报错；文件不存在→报错 |
| `tenancy/manager`（新） | `EnsureRunning`：未跑→启动+probe 返回 true；已在跑→false 且不重启（记录启动次数）；`suspended`→不启动；`StopForLogout`：不写 `suspended`、调用 `DropTenant`；`Suspended` 字段不变 |
| `proxy`（改/新） | 登录调 `LoginPrepare`（参数是租户名 + 提交的 key）；hook 报错时**会话仍然签发**并 302；飞书登录同样调用（`submittedKey == ""`）；门户退出：单会话租户→调 `StopSignedOut`；该租户还有别的会话→不调；租户侧栏退出同理；stopper 报错→仍 303 |
| `cmd/dshgw`（新） | `PrepareLogin`：用**存储的 worker key**（不是提交的 key）取模型并渲染；提交 key 与存储 key 不同时不动存储 key；恢复被删掉的 `providers.aigw` 与 `refs.AIGW_API_KEY`；租户段（`permission`/`ui-theme`/自建 provider）逐字节保留；aigw 失败时不改文件；`HOME` 指向临时目录时 `$HOME/.dsh` 全程不被创建 |

命令：`go test ./internal/dshgw/... ./cmd/dshgw ./internal/arch`（本机不能用 `./...`：`./data`
下有 GB 级 FUSE 挂载，`go list` 会挂住，见 `docs/TODO.md` 末节）。

## 7. 依赖与影响面

- 改动文件：`internal/dshgw/session/session.go`、`internal/dshgw/tenancy/manager.go`、
  `internal/dshgw/tenancy/render.go`、`internal/dshgw/proxy/proxy.go`、`internal/dshgw/proxy/feishu.go`、
  `cmd/dshgw/serve.go`、`cmd/dshgw/admin_serve.go|login_prepare.go`、以及文档。
- 不变式：dshgw 只 import `internal/dshgw/**`（M51）；不改 dsh 发行包；不需要 root；`.lock` 协议沿用
  dsh 的 `<file>.lock`（`@deepseek-ai/dsh-atomic-write` 同名约定，已核对）。
- 风险与代价：登录多一次 aigw `/v1/models`（冷启动时多 2–4s 的 worker 启动等待）；退出要等 worker 退出
  （正常 <1s，最坏十几秒）。两者都有上界且 fail-soft。
- 回归面：M52/M58 的 worker 生命周期、M67 的侧栏退出、M68 的模型渲染。

## 8. 复验计划（真机，本机三单元：aigw-local / dshgw-verify / gwproxy-verify）

1. **登录同步**：门户登录 → 日志出现准备成功；该租户 `settings.yaml` 的平台段 = aigw 当前授权模型；
   租户自有的 `permission`/`ui-theme` 原样保留。
2. **反例有牙**：手工删掉 `llm-pi-ai.providers.aigw` 与 `refs.AIGW_API_KEY` → 重新登录 → 两者恢复，
   其它键与 `records` 不动。
3. **退出即停**：侧栏退出 → 该租户的 dsh 进程消失、worker 端口无监听、`registry.json` 的
   `suspended` 仍为 `false`；再登录 → 进程与端口回来。
4. **多会话**：两个浏览器同时登录同一租户 → 其中一个退出 → worker 仍在（审计 `logout_worker_kept`）；
   两个都退出 → 停。
5. **不碰宿主机**：整个过程 `~/.dsh/settings.yaml` 的 mtime 与内容不变。

## 9. 实现与设计差异

实现（2026-09-21）与设计的差异，逐条记录：

| 项 | 设计 | 实现 | 原因 |
|---|---|---|---|
| `EnsureCredentialRef` 签名 | `EnsureCredentialRef(path, key)` | `EnsureCredentialRef(path, ref, value)` + 常量 `AIGWAPIKeyRef` | 渲染器与轮换里本来就有字面量 `"AIGW_API_KEY"`，抽成常量后三处（渲染、轮换、登录恢复）同源；多一个 ref 参数不增加调用方负担，却让函数名不撒谎 |
| **退出的停** | "该租户无其它存活会话时停"（D5 初版） | **无条件停**（仍只动"这次真的撤销了会话"的租户） | 部署当天真机数据否掉了初版：`dsh-tenant` 有 16 个存活会话（15 个是几天前的旧会话），"最后会话"规则等于退出后 dsh 还跑好几天。同时补上"伪造 cookie 名不能停别人的 dsh"（`Sessions.Get` + 租户匹配） |
| `managerOps.validator` 类型 | 未提 | `*aigw.Client` → `keyValidator` 接口 | 登录钩子的策略（用哪把 key 取模型、失败后文件是否原样）必须能在没有 aigw HTTP 服务的情况下测；接口本来就在 `modelrefresh.go` 里为同一目的存在 |
| 挂起租户 | "不拉起，日志点名" | `EnsureRunning` 返回错误（含 `suspended` 字样），由调用方告警 | 让"跳过"与"失败"在日志里可区分，且不引入第三个返回值 |
| 超时 | "有上界" | 具名常量 `loginPrepareTimeout = 45s`、`logoutStopTimeout = 30s` | 上界要能被审阅；两个值分别覆盖"冷启动 + probe"与"worker 慢死 + SIGKILL 回收" |
| 审计事件 | 未提 | 新增 `login_prepare_failed`、`logout_worker_stop`、`logout_worker_kept`、`logout_worker_stop_failed` | 退出的停/不停是运维要能事后查清的事（"谁把 dsh 停了"） |
| `EnsureRunning` 的返回 | `(started bool, err error)` | 同设计 | — |
| 未 provisioned 租户 | 沿用 `AdoptKey` | 同设计：`PrepareLogin` 内部调 `AdoptKey`，但**仅当登录提交了 key**（飞书登录提交为空则跳过） | 飞书票据登录没有 key 可采纳 |
| `KeyAdopter` 字段 | 由 `LoginPrepare` 取代 | 同设计（`AdoptKey` 方法保留在 `managerOps` 上，作为内部一步） | 登录只有一个接缝、一条顺序 |

测试（新增，`go test ./internal/dshgw/... ./cmd/dshgw ./internal/arch` 全绿）：

- `tenancy`：`TestEnsureCredentialRefRestoresThePlatformReferenceOnly`（补齐、幂等不写、轮换跟随、版本非 1 报错）、
  `TestEnsureRunningStartsAStoppedTenantAndLeavesARunningOneAlone`、`TestEnsureRunningRefusesASuspendedTenant`、
  `TestStopForLogoutStopsWithoutSuspending`（幂等 + 再登录能起来）
- `proxy`：`TestLoginPreparesTheTenantAndSurvivesPreparationFailure`、
  `TestFeishuLoginPreparesTheTenantWithoutAKey`（提交 key 为空）、
  `TestLogoutStopsTheTenantEvenWithOtherLiveSessions`（有过期风险的旧会话也照停）、
  `TestLogoutLeavesTenantsItDidNotSignOutAlone`（伪造 cookie 名不停别人的 dsh）、
  `TestLogoutSurvivesAWorkerThatWillNotStop`、`TestTenantLogoutStopsTheTenantWhenItsLastSessionLeaves`
- `cmd/dshgw`：`TestPrepareLoginAppliesThePlatformSliceFromTheStoredKey`（平台段来自存储 key、
  租户段逐键保留、凭据恢复、worker 起来）、`TestPrepareLoginLeavesTheFilesAloneWhenAigwFails`、
  `TestPrepareLoginDoesNotResurrectASuspendedTenant`、
  `TestPrepareLoginNeverTouchesTheHostsDshHome`（`HOME` 指向临时目录，`$HOME/.dsh` 不被创建/修改）、
  `TestStopSignedOutStopsWithoutSuspending`、`TestPrepareLoginReportsAMissingStoredKey`

真机验收（本机三单元）与发布记录见 `docs/todo_done.md` 的 M69 小节。

## 10. 回滚

改动基本是加法（新方法、新接缝、新测试）；回滚 = `git revert` 该里程碑提交，租户产物无需迁移。
"退出即停"是运行期行为，回滚后 worker 恢复常驻，没有残留状态需要清理。
