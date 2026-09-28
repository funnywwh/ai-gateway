# M94 设计文档：在控制台改公司名（含身份应用与配置里的公司）

> 状态：**已实现（M94）**。
> 前序：[M92 多公司组织架构导入](m92-multi-company-feishu-org-sync.md)、[M93 控制台管理公司](m93-console-managed-feishu-companies.md)。
> 面向使用者的规格：[docs/org.md](../org.md) §5、[docs/feishu.md](../feishu.md) §5c.6、[docs/mcp.md](../mcp.md) §4。
>
> 需求原话（2026-09-28）：「`…/admin/ui/#/companies` 要支持能修改公司名」。

## 1. 问题

M93 的公司页只能编辑**控制台登记的**公司（有库行 id 的那种）。身份应用（本公司）与
`feishu.companies` 登记的公司是**只读行**，连「编辑」按钮都没有——它们的名字分别来自
`feishu.company_name` 与 `feishu.companies[].name`，要改只能改配置 + 重启。

真实部署上的第一个需求正是这一条：rag-server 现在只有身份应用一家，名字默认「本公司」，
而公司的真名（智天成）只出现在组织树的一个根节点上。运营要的是**在页面上把公司名改成真名**。

## 2. 目标

1. 「公司」页的每一行都有「编辑」；**公司名**对任何来源的公司都可改（身份应用、配置里的公司、
   控制台登记的），改完立即生效，**不改配置、不重启**；
2. 改名同时把**公司节点**的名字跟着改（当它存在、且名字还等于旧名时），让树与公司页不打架；
3. 改名与组织树冲突时**提前说清楚**并给出出路，而不是等到下一次同步才报 `root_name_conflict`；
4. 除名字之外，配置来源的公司的字段（密钥、根节点名、备注、启停）**仍然由配置管理**——
   这一条沿用 M93 的 D1，本次只为「名字」开一个口子。

### 非目标

- 不在控制台管理身份应用的**密钥/回调/签名密钥**（它是部署身份，仍只在配置里）；
- 不改 `feishu.companies` 的其它字段（root_node/secret/note/enabled 仍归配置）；
- 不做公司重命名历史/审计回放（审计照旧记一条 `update`，changes 里写新旧名字）。

## 3. 现状（已核对的事实）

| 事实 | 位置 |
|---|---|
| 公司页只给 `source === 'console'` 的行渲染「编辑」，且 PATCH 用数字 `row.id`；身份应用/配置公司的 `id` 是 `null` | `internal/webui/static/js/pages/companies.js`、`GET /org/feishu/companies` |
| PATCH 只接受数字 id，且只服务库行 | `internal/httpapi/admin_org_feishu_company_writes.go` |
| 公司名来源：身份应用 = `feishu.company_name`（空则「本公司」）；配置公司 = `companies[].name`；控制台公司 = 库行 `name` | `cmd/aigw/feishu.go`、`internal/httpapi/admin_org_feishu_companies.go` |
| 公司节点名只在**创建/认领**时取一次，之后改公司名不影响已有节点 | `planCompanyRoot`（M92） |
| 线上（rag-server）现状：`feishu_apps` 0 行；9 个部门节点已认领给身份应用；**根层已有一个名为「智天成」的部门节点**，公司节点尚未创建 | 只读查库，2026-09-28 |

## 4. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | 新增一张**名称覆盖表** `feishu_company_names(app_id PK, name, updated_by, updated_at)`，只服务"名字来自配置"的公司（身份应用 + `feishu.companies`） | 名字是唯一一个运营需要就地改、而配置又不方便改的字段。**否决**给身份应用在 `feishu_apps` 里造一行（它会以"控制台公司"的身份出现在列表与同步下拉里，语义错）；**否决**改写配置文件（网关不写自己的配置，且与 M63 的"配置在部署根、数据在 data/"分层冲突） |
| D2 | 合并优先级：**控制台行自己的 name > 名称覆盖 > 配置里的名字** | 三者各自有真源：库行的名字就在行里；覆盖是"运营在控制台上做的决定"，比配置文件更新；配置是兜底。覆盖只对配置来源的行生效，库行改名仍然改它自己那一行 |
| D3 | `PATCH /org/feishu/companies/{id}` 的 `{id}` **同时接受数字 id 与 app_id**（`cli_…`） | 页面对每一行都要能编辑，而配置来源的行没有数字 id。app_id 与数字 id 形态不重叠（`cli_` 前缀），一个工具一个 handler 就能覆盖三种来源。**否决**再加一条 `…/by-app/{app_id}`（同一动作两条路由）；也**否决**把覆盖行的 id 编成负数 |
| D4 | 配置来源的公司用这条路由时**只接受 `name`**；`app_secret`/`root_node`/`note`/`enabled` 一律 400 并点名配置项 | 与 M93 的 D1 一致：密钥属于部署身份，`root_node`/`enabled` 是配置即真源；"只开一个口子"比"看似都能改、实际只有一半生效"诚实 |
| D5 | 改名时**连带改公司节点名**，条件是：该公司节点存在、且它的名字等于**改名前的有效根名** | 否则运营改了公司名，树里还挂着旧名字，下一次同步又会用新名字去建第二个节点（或撞名被拒）。条件收窄（只动"还是旧名"的节点）是为了不动运营手工改过名的节点 |
| D6 | 根层已有同名节点时，**改名照做**（它只是名字），响应回一条 `warnings: ["root_name_taken"]` 与具体指引；下一次同步仍按 M92 的规则拒绝（`root_name_conflict`）。**不**再发明"认领"机制 | 名字写在哪都不冲突（覆盖表按 app_id）；真正的冲突发生在**同步建公司节点**那一步，M92 已经有确定的判定与报错，这里只需把"下一步会怎样、怎么解"提前说清楚 |
| D7 | 冲突的**两条实际出路**写进响应、文档与控制台提示：①先在组织树里给那个节点改名或移走，再同步；②**先同步一次（用旧名字建出公司节点），再改名** | ②对线上这台部署最省事：一次「同步飞书」+ 一次改名，就得到 `智天成 → 智天成(部门) → 软件部/硬件部/…`，且**不动任何既有节点**（部门节点的父子关系保持） |
| D8 | ~~`adopt_root_node`：把同名根节点认领为公司节点~~ **不做** | 想清楚后发现它对真实的树是**更差**的结果：线上名为「智天成」的根节点（部门 `od-a446…`）承载着 `软件部/硬件部/…`，它是一个**真实部门**、不是公司的虚拟根。认领要清掉它的部门 id，于是同步会把 `od-a446…` 当成"本地没有"新建一个**空**的同名子节点，而 `软件部` 等仍挂在被认领的节点上——树变成 `智天成(公司) → {软件部, …, 智天成(空)}`，比不认领更难理解。**否决** |
| D9 | 控制台：每一行都有「编辑」；配置来源的对话框里只有**公司名**可改，其余字段只读并注明「由配置提供」；若根层已有同名节点，对话框里直接写出两条出路（先同步再改名 / 先处理那个节点） | "能不能改"一眼可见，比点进去再发现灰着好；冲突的解法要写在操作发生的地方 |
| D10 | 审计：`update` on `feishu_company`，changes 里写 `{"name": {"from": …, "to": …}}`（`from`/`to` 都是名字，不是秘密）；名字不是秘密，可以照实记 | 与 M93 的 `secret_changed` 口径一致：能记的记清楚，不能记的（密钥）绝不记 |
| D11 | 名称覆盖表**不**出现在 `GET /org/feishu/companies` 的响应里（响应里就是合并后的 `name`）；另加一个只读字段 `name_source`（`"row"`/`"override"`/`"config"`）供页面显示"这个名字是在控制台改过"的标记 | 页面不需要知道实现，但运营需要知道"这个名字是我在控制台改的"还是"配置里写的" |
| D12 | 不做"把配置里的公司搬进控制台"（M93 已否决） | 本次只解决名字 |

## 5. 数据模型

### 5.1 migration `internal/store/migrations/0032_feishu_company_names.sql`

```sql
-- 公司名的控制台覆盖（M94）：只服务"名字来自配置"的公司（身份应用与 feishu.companies）。
-- 控制台登记的公司（feishu_apps）改自己的 name 列，不写这张表。
CREATE TABLE IF NOT EXISTS feishu_company_names (
    app_id     TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);
```

### 5.2 store（`internal/store/feishu_apps.go` 追加）

```go
ListFeishuCompanyNames(ctx) (map[string]string, error)      // app_id → 覆盖名
SetFeishuCompanyName(ctx, appID, name, by string) error      // upsert
DeleteFeishuCompanyName(ctx, appID string) (bool, error)     // 删掉覆盖 = 回到配置里的名字
```

端口 `FeishuCompanyAdmin` 追加同名三个方法（M93 的端口继续是唯一入口）。

## 6. 合并与解析

`feishuCompanyRows(ctx)` 的配置那一段变成三步：

```
身份应用 / feishu.companies  ← 行（配置）
      ↓ 若 feishu_company_names 里有覆盖 → 用覆盖名（NameSource = "override"）
      ↓ 控制台行（feishu_apps）保留自己的 name（NameSource = "row"）
```

`RootName` 也随之一致：优先 `feishu_apps.root_node`（控制台行）、其次（配置来源）用有效名字，
最后才是 `feishu.companies[].root_node`……**注意**：配置里的 `root_node` 是显式写的根节点名，
覆盖名不应该悄悄把它盖掉——规则定为：`root_node` 非空时它优先（显式 > 覆盖 > name）。

## 7. 接口（只扩一条路由，不新增）

| Method | Path | 变化 |
|---|---|---|
| PATCH | `/admin/api/v1/org/feishu/companies/{id}` | `{id}` 可为数字 id（控制台行）或 `cli_…`（身份应用/配置公司）；对后者只接受 `name`，其余字段 400 并点名配置项 |

响应（两条路径一致）：`{ok, id|app_id, company:{…}, node_renamed:{id,name}|null, name_source, warnings[]}`。

新增/变化的字段（`GET /org/feishu/companies`）：
- `name_source`：`"row"` / `"override"` / `"config"`；
- `root_name_taken`：`{node_id, name}` —— 根层存在同名节点时给出（M92 的 `root_blocked` 继续表示"同步会被拒"），
  页面据此把"下一步会怎样、怎么解"直接写在编辑框里。

边界与错误（都发生在写之前）：

| 情形 | 行为 |
|---|---|
| `name` 不合法（空、>64 字符、`cli_` 开头） | 400（复用 `config.NormalizeCompanyName`） |
| 名字与**其它**公司重名（库行或配置行） | 409，点名是哪一家 |
| 公司节点存在、名字≠旧名 | 只改公司名，返回 `warnings: ["node_name_kept"]`（不覆盖手工改过的节点名） |
| 公司节点改名会撞同层同名 | 409，消息给出"改名或先处理那个节点" |
| 根层有同名节点（公司节点尚未创建） | **改名成功** + `warnings: ["root_name_taken"]`；响应与页面写明下一步：先同步一次再改名，或先处理那个节点 |
| 传了 `app_secret`/`root_node`/`note`/`enabled` 给配置来源的公司 | 400，消息点名 `feishu.app_id` / `feishu.companies[].…` |
| 未知 `{id}`/app_id | 404 |
| viewer | 403 |

## 8. 控制台

- 每一行都渲染「编辑」（M93 只给控制台行）；
- 配置来源的对话框：**公司名**可编辑；`App ID`、`App Secret`、根节点名、备注、启用都只读，并各带一句
  「由 feishu.app_id / feishu.companies 管理」；当 `root_name_taken` 存在时，对话框里直接给出两条出路
  （先同步一次再改名 / 先把那个节点改名或移走），不提供"认领"；
- 提交成功后的 toast 区分两种结果：只改了名字 / 顺带改了公司节点名（并说明同步的下一步）；
- 页面上把 `name_source === 'override'` 的行标一个小徽标「名字已在控制台改过」（可再次编辑，清空输入回到配置名）。

## 9. 测试策略

| 层 | 测试 |
|---|---|
| `internal/store` | 覆盖表 upsert/list/delete；覆盖不碰 `feishu_apps` |
| `internal/httpapi` | 身份应用改名（覆盖落库、`name_source=override`）；配置公司改名；控制台行改名仍走原路径；冲突矩阵（与其它公司重名 409、公司节点改名撞同层 409、根层同名 → 改名成功 + `root_name_taken` 警告、给配置公司传密钥 400、未知 id 404、viewer 403）；**两条出路的端到端**：①先同步（旧名）再改名 → 公司节点被改名、同名部门节点在其下不动；②根层同名未处理时同步仍 409 且消息给出指引 |
| 前端静态 | `companies_test.mjs`：身份应用行也有「编辑」；配置行对话框只有名字可改；`root_name_taken` 存在时对话框里出现两条出路提示 |
| harness | `companies` 视图补：身份应用行的编辑入口、配置行的只读字段、认领勾选（本沙箱无 firefox，宿主走查） |
| 回归 | M92/M93 的组织同步测试与 `companies_test.mjs` 全绿；性能基准不变（不碰请求路径） |

## 10. 依赖

无新外部依赖；新增 `0032_feishu_company_names.sql`，其余都在既有文件里扩展。

## 11. 实现与设计差异

实现与设计一致（覆盖表、合并优先级、app-id 句柄、只接受 `name`、连带改公司节点名、两条出路、
控制台每行可改名）。差异与补充：

| # | 设计 | 实现 | 原因 |
|---|---|---|---|
| 1 | 覆盖只服务"名字来自配置"的公司 | 一致；另外**控制台登记的公司**用 app_id 访问时**委托**给按 id 的那条路径（同一次写、两种句柄），覆盖表里不会出现库行 | 否则同一个动作会有两份实现，迟早分叉 |
| 2 | 写顺序：先改公司节点名、再写覆盖 | 一致（公司节点改名是唯一可能冲突的写；失败时名字不变，不会出现"名字改了、节点没改"的中间态） | — |
| 3 | root 名规则：显式 `root_node` 优先 | 一致：`feishu.companies[].root_node` 非空时，改名只改**显示名**，根节点名与目标节点名都不动 | `root_node` 指的是一个本地节点，不是标签 |
| 4 | "根层同名"的提示位置 | 列表行里给 `root_name_taken` + `root_blocked`；改名**响应**里给 `warnings:["root_name_taken"]`（保存后立刻 toast 提示）；对话框在行已带这两个字段时把两条出路写在名字字段的 hint 里 | 对话框在输入前拿不到"你想输入的名字是否已占用"（那是服务端的事实）；能提前说的说、剩下的保存后立刻说，不留到下一次同步 |
| 5 | 公司节点名"还在用旧名字才跟着改" | 一致；另外**清空名字**（回到配置名）也走同一条跟随逻辑 | 否则清空后节点会停在覆盖名上 |
| 6 | 审计 `{name:{from,to}}` | 一致；清空时 `to` 为空字符串（表示回到配置名） | — |
| 7 | 测试 | 另有 `TestFeishuRenameReportsRootNameTaken` 把"两条出路"做成端到端：①未处理时改名成功但同步 409（且消息给指引）；②先同步再改名后，同一棵树里两个「智天成」（公司节点与其下的部门节点）并存且互不干扰 | 这条路径是本里程碑最容易写错、也最需要在真机上先走一遍的 |

**性能**：`BenchmarkPlan` 4948 B/45 allocs 不变（只动管理面与公司列表的构造）。

**新增测试**：`internal/store/feishu_apps_test.go` 的覆盖表用例、`internal/httpapi/admin_org_feishu_company_rename_test.go`
（身份应用/配置公司/控制台行三条改名路径、跟随与手工改名保护、根层同名的两条出路、守卫矩阵、
viewer 403）、`internal/webui/tests/companies_test.mjs` 的 M94 断言（改名按钮、只读字段、
只发 `name` 的请求体、冲突提示）、harness `companies` 视图的新检查项（本沙箱无 firefox，宿主走查）。
