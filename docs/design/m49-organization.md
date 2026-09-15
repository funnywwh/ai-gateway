# M49 设计文档：组织架构（独立树 + 账号多归属 + 节点标签继承）

## 目标

在**不改动标签体系语义**的前提下，新增一套独立的组织架构，并交付一个可复用的树形控件：

1. **独立的组织树**：`org_nodes` 是一棵多根森林（`parent_id` 可空），与 `tags` 是两套互不隶属的实体，
   有自己的表、自己的页面、自己的管理路由组。删除/改名一个组织节点不会动到标签，反之亦然。
2. **账号多归属**：一个账号可同时挂在任意多个节点上（`org_node_accounts` 关联表）。
3. **节点标签继承**：节点可绑定标签（`tags_json`，与 `accounts.tags_json` 同格式）。账号下**所有 API Key**
   继承「其所有成员节点 + 这些节点的全部祖先」的标签，进入既有的授权并集与策略合并。
4. **路由性能不回退**：继承标签的解析**不在请求路径上遍历祖先链**，也不在请求路径上解码账号标签。
   本次把账号侧的标签解析从「每次请求」移到「快照构建」，并用基准与 `AllocsPerRun` 钉住：
   `BenchmarkPlan` 的 allocs/op 不上升，无 Key 自有标签时为 0 分配。
5. **可复用树形控件**：`internal/webui/static/js/tree.js` 是一个独立的、不知道组织架构也不知道 API 的树控件，
   同一份代码既能放**侧边菜单栏**（紧凑模式）也能放**工作区**（完整模式）。

## 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | 组织与标签是**两套独立实体**，只在「解析生效标签」这一个入口交汇 | 用户明确要求"独立"。交汇点选 `registry.ResolveTagRecords`：它是全部标签消费点（`v1` 模型列表、`v1` 配额 `limitsFor`、`routing.Plan`、`billing.ResolveMarkup`、`admin` 的 `effective_tags`、`admin_explain_router`、`mcpsrv` 的 `effective_tags`）的唯一上游，改一处即全部生效。**否决**在 httpapi 层层叠加：会漏掉 `Plan` 与 `limitsFor` 两条路 |
| D2 | **性能是本设计的硬约束**：祖先链只在 `registry.Build` 遍历一次，按账号**物化**成标签记录 | 见下方「性能」一节。若按"请求时向上走父亲"实现，每个请求要多 N 次 map 查找 + N 次 `json.Unmarshal(node.TagsJSON)`，而一个请求会调用 `ResolveTagRecords` **1–3 次**（模型列表 + `limitsFor` + `Plan`） |
| D3 | 无 Key 自有标签时**直接返回快照里预计算好的切片**，0 分配 0 解析 | 已核对全部调用方只读（`Authorize`/`mergePolicy`/`limitsFor`/`effective_tags` 都只 `for range`）。返回值因此是**只读契约**，写在函数注释与 `ResolveTagRecords` 的文档里，并由 `AllocsPerRun` 测试守住 |
| D4 | 账号侧预计算**只为「有标签或有组织归属」的账号**建条目 | 快照在每次后台写后重建；只为需要的账号建 map，内存上界是"真实用到的标签组合"，而不是账号总数 |
| D5 | 继承顺序：**成员节点（按节点 id 升序，各自根→自身）→ 账号自身标签 → Key 自身标签**，按名去重（首次出现优先），最后按 `tags.priority` 稳定排序 | 与既有契约一致（`registry/tags.go` 注释：账号名在前使等优先级顺序确定，Key 的策略最后生效）。多归属时"按节点 id 升序"让顺序可复现，不依赖 map 迭代顺序 |
| D6 | 多根森林 + **兄弟节点内名字唯一**（`UNIQUE(COALESCE(parent_id,0), name)`） | "两个分公司都有研发部"是常态，必须允许；同一层内重名几乎总是笔误，且会让 UI 的两行无法区分。**否决**全局唯一（挡住正常组织）与完全不唯一（错误无从发现） |
| D7 | 深度上限 `orgtree.MaxDepth = 16`，创建/移动时校验 | 祖先链展开成本 O(depth)；16 层足够任何真实组织，并让"物化"的内存上界可推理 |
| D8 | 节点写标签时**名字必须已存在**（复用 `unknownTagName`，未知名字 400） | 节点标签是"给整棵子树放权/收权"的开关。静默丢弃不存在的名字会让该账号**没有任何授权**，从而按 `default_grant` 回落成**通配全开**（`routing.Authorize` 的既有语义）——一个拼写错误不该有这个后果。账号/Key 的旧宽松行为**不改**（历史行为，改动会破坏既有导入流程） |
| D9 | 删除节点：默认**拒绝**有子节点的删除，`cascade=true` 才删整棵子树 | 误删代价高、没有回收站。**否决**默认级联（一次点击删掉一个部门）与"只删叶子"（无法删除中间层） |
| D10 | 两个写方向都做成**整表替换**：`PATCH /accounts/{id}` 的 `org_node_ids`、`PUT /org/nodes/{id}/accounts` 的 `account_ids` | 两条主用例（"这个账号属于哪些组织" / "这个组织有哪些账号"）各自幂等、可重放、易审计。**否决** add/remove 增量语义：要多一批接口与工具，且并发下语义更绕 |
| D11 | 树形控件是**独立模块**（`static/js/tree.js`），输入是扁平 `nodes` + 回调，**不知道组织架构、不 fetch、无全局状态** | 用户要求"独立可复用，可放侧边栏也可放工作区"。`mode` 只影响密度与元信息展示，不影响数据结构。**否决**把树写进 `org.js`（无法复用）与写进 `ui.js`（该文件已是通用组件集合，塞入一个 200 行的有状态控件会稀释它） |
| D12 | ~~侧边栏插槽是**通用机制**（`app.js` 的 `.sidebar-slot` + 页面上下文里的 `sidebar`）~~ **已撤销**：组织树只渲染在工作区，插槽与其 CSS 一并删除 | 当初为了"树可以放侧边栏"而给 shell 加了插槽。上线后产品判定侧边栏那份是冗余（同一棵树在同一屏出现两次，且挤占全局导航），于是撤掉。插槽的唯一使用者消失后它就是死代码，因此连插槽本身一起删——**控件仍然支持 `mode:'sidebar'`**（最初的需求是控件"可放两处"，不是页面必须放两处），那项能力由 harness 的 `tree` 视图继续守着 |
| D16 | 过滤支持拼音：表是**生成文件**（`scripts/gen-pinyin.py` → `js/pinyin.js`，数据来自 mozillazg/pinyin-data，MIT），**能力通过 `matcher` 回调注入树控件**，控件本身不依赖表 | 操作员记不住「张三」这两个字怎么打，但记得住 `zhangsan`/`zs`。控制台零构建、不能 import npm 包，所以表必须落成仓库文件。**否决**把表 import 进 `tree.js`：那会让每一个用树的地方都背上 137KB，也破坏"控件零依赖、可复用"这条最初的需求 |
| D17 | 成员面板拆成"固定过滤行 + 独立滚动列表"，勾选的成员置顶 | 过滤框跟着列表滚是错的：列表长到需要过滤时，过滤器正好被滚走。选中项置顶让"这个部门有谁"在一个长账号表里一眼可见。**否决**保持单块滚动区（更简单，但两个问题都还在） |
| D13 | `registry.NewSnapshot` 旧签名**保留**（转调 `Build`），新构造入口是 `registry.Build(Input)` | 仓库内 12 处 `NewSnapshot` 调用（多在测试），7 个位置参数再加 2 个会更不可读。两个入口的分工写进注释，并由"对拍测试"钉住无组织数据时行为一致 |
| D14 | `domain.Store`（registry 的读端口）只加 `ListOrgNodes` + `ListOrgMemberships` 两个读方法 | 快照只需要这两份数据；管理面的写方法走 `httpapi.OrgAdmin` 端口，不污染 registry 的读端口 |
| D15 | 写组织数据后 `reload(ctx, reason, true)`（`invalidateAll=true`） | 节点标签/成员/树形都会改变**既有 Key** 的生效授权，而 Key 在 verifier 里有 30s 正缓存；只 invalidate 单个前缀会留下最长 30s 的越权窗口。与标签写入同一约定 |

## 性能（本设计的核心约束）

### 改动前的实测（12th Gen i7-12700K，`-count=3`）

```
internal/registry  BenchmarkResolveTagRecordsWithoutKeyTags   512 ns/op   320 B/op   11 allocs/op
internal/registry  BenchmarkResolveTagRecordsWithKeyTags      800 ns/op   536 B/op   17 allocs/op
internal/routing   BenchmarkPlan                             3402 ns/op  4948 B/op   45 allocs/op
internal/routing   BenchmarkPlanParallel                     2696 ns/op  4954 B/op   45 allocs/op
```

`ResolveTagRecords` 每次调用都要 `json.Unmarshal(account.TagsJSON)`、`json.Unmarshal(key.TagsJSON)`、
建 `names`/`seen`/`tags` 三个容器并做一次 `sort.SliceStable`。而每个请求会走到它 1–3 次：
`v1.go:950`（`/v1/models` 的授权）、`v1.go:1118`（`limitsFor`，每次限速检查）、`routing.go:223`（`Plan`）。

### 改动后

- `registry.Build` 在一张快照里算一次：
  1. `orgtree.NewIndex(orgNodes).InheritedTagNames(orgMembers)` → 每账号的继承标签名（成员节点祖先链顺序 + 去重）；
  2. 账号侧名单 = 继承名 + `account.TagsJSON` 解码名（沿用 `appendTagNames` 的"解析失败即忽略"语义）→ 按名去重
     → 查 `TagByName` 丢弃不存在的名字 → 按 `priority` 稳定排序 → `accountTagRecords[accountID]`
     与 `accountTagNames[accountID]`（名字集合，用于 Key 侧去重）。
- `ResolveTagRecords` 请求路径：
  - Key 无自有标签且账号有条目 → **返回共享切片**（0 分配、0 解析）；
  - 否则 → 一次定容分配，先铺账号侧记录，再解码 Key 标签并**跳过名字已在账号侧集合里的项**，
    最后 `sort.SliceStable`。账号侧已预排序 ⇒ 等优先级仍是"账号在前"，与改动前逐项等价。
- **组织祖先链在请求路径上一次都不遍历**，快照里也没有惰性缓存（不加锁）。

行为等价性由「对拍测试」保证：无组织数据时，新实现对同一组 `(account, key)` 的输出与旧实现逐项相同。

## 接口

```go
// internal/domain/org.go
type OrgNode struct {
    ID        int64
    ParentID  *int64      // nil = 根
    Name      string
    Note      string
    TagsJSON  string      // 标签名数组，与 accounts.tags_json 同格式
    SortOrder int
    CreatedAt time.Time
    UpdatedAt time.Time
}
type OrgMembership struct {
    NodeID    int64
    AccountID int64
}

// internal/domain/org_name.go
const MaxOrgNodeNameRunes = 64
func NormalizeOrgNodeName(name string) (string, error) // 与 NormalizeTagName 同规则

// internal/orgtree —— 纯树算法，只 import internal/domain
const MaxDepth = 16
type Index struct{ /* ... */ }
func NewIndex(nodes []*domain.OrgNode) *Index      // 容忍乱序/孤立父 id
func (ix *Index) Node(id int64) *domain.OrgNode
func (ix *Index) Chain(id int64) []int64           // 根→自身（含自身），visited 防环
func (ix *Index) Depth(id int64) int
func (ix *Index) Descendants(id int64) []int64     // 含自身，防环
func (ix *Index) SubtreeHeight(id int64) int
func (ix *Index) WouldCreateCycle(id, newParent int64) bool
func (ix *Index) Ordered() []*domain.OrgNode       // DFS：sort_order, name, id
func (ix *Index) InheritedTagNames(members map[int64][]int64) map[int64][]string
func (ix *Index) Validate() error                  // 重复 id / 兄弟重名 / 成环 / 超深

// internal/store/org.go（*store.DB）
ListOrgNodes(ctx) ([]*domain.OrgNode, error)
GetOrgNode(ctx, id) (*domain.OrgNode, error)
CreateOrgNode(ctx, *domain.OrgNode) (int64, error)  // 兄弟重名 → ErrConflict
UpdateOrgNode(ctx, *domain.OrgNode) error
DeleteOrgNode(ctx, id int64, cascade bool) (int, error)
ListOrgMemberships(ctx) ([]domain.OrgMembership, error)
ListOrgNodeAccountIDs(ctx, nodeID int64) ([]int64, error)
SetOrgNodeMembers(ctx, nodeID int64, accountIDs []int64) error
SetAccountOrgNodes(ctx, accountID int64, nodeIDs []int64) error

// internal/registry
type Input struct {
    Accounts       []*domain.Account
    Providers      []*domain.Provider
    ProviderModels []*domain.ProviderModel
    Models         []*domain.Model
    Mappings       []*domain.ModelMapping
    Routes         []*domain.Route
    Tags           []*domain.Tag
    OrgNodes       []*domain.OrgNode
    OrgMembers     map[int64][]int64 // accountID → nodeIDs
}
func Build(in Input) *Snapshot
func NewSnapshot(accounts, providers, providerModels, models, mappings, routes, tags) *Snapshot // 旧签名，转调 Build
func (s *Snapshot) InheritedTagNames(accountID int64) []string

// internal/httpapi/admin_ports.go
type OrgAdmin interface {
    ListOrgNodes(ctx) ([]*domain.OrgNode, error)
    GetOrgNode(ctx, id int64) (*domain.OrgNode, error)
    CreateOrgNode(ctx, *domain.OrgNode) (int64, error)
    UpdateOrgNode(ctx, *domain.OrgNode) error
    DeleteOrgNode(ctx, id int64, cascade bool) (int, error)
    ListOrgMemberships(ctx) ([]domain.OrgMembership, error)
    ListOrgNodeAccountIDs(ctx, nodeID int64) ([]int64, error)
    SetOrgNodeMembers(ctx, nodeID int64, accountIDs []int64) error
    SetAccountOrgNodes(ctx, accountID int64, nodeIDs []int64) error
}
```

管理面路由（5 条新增 + 2 条改造，全部进 `admin_routes.go` 的同一张表，因此自动获得 MCP 工具）：

| Method | Path | 工具名 | 角色 |
|---|---|---|---|
| GET | `/admin/api/v1/org/nodes` | `admin_list_org_nodes` | viewer |
| POST | `/admin/api/v1/org/nodes` | `admin_create_org_node` | admin |
| PATCH | `/admin/api/v1/org/nodes/{id}` | `admin_update_org_node` | admin（Dangerous） |
| DELETE | `/admin/api/v1/org/nodes/{id}` | `admin_delete_org_node` | admin（Dangerous） |
| PUT | `/admin/api/v1/org/nodes/{id}/accounts` | `admin_set_org_node_accounts` | admin（Dangerous） |
| GET | `/admin/api/v1/accounts` | `admin_list_accounts` | viewer（增 `org_node_id` / `include_descendants`） |
| POST/PATCH | `/admin/api/v1/accounts` | — | admin（增 `org_node_ids`） |

## 数据流

**写路径（控制台/MCP → 数据面）**

```
PUT /org/nodes/{id}/accounts ─┐
PATCH /accounts/{id} ─────────┼→ store.SetOrgNodeMembers / SetAccountOrgNodes（事务内 DELETE+INSERT）
PATCH /org/nodes/{id} ────────┘                      │
                                                     ▼
                            s.reload(ctx, reason, invalidateAll=true)
                              ├─ verifier.InvalidateAll()          ← 丢掉 30s 的 Key 正缓存
                              └─ registry.Reload()
                                   ├─ ListOrgNodes / ListOrgMemberships / ListTags / ListAccounts …
                                   ├─ orgtree.NewIndex(...).InheritedTagNames(members)
                                   ├─ accountTagRecords / accountTagNames 物化
                                   └─ atomic 换入新快照
```

**读路径（请求热路径，零组织成本）**

```
请求 → verifier 命中缓存的 APIKey
     → registry.ResolveTagRecords(snap, key)
          ├─ key 无自有标签 且 账号有条目 → 返回 snap.accountTagRecords[key.AccountID]（共享、只读、0 分配）
          └─ 否则 → 定容拼装 + 跳过账号侧已含的名字 + 一次稳定排序
     → routing.Authorize(key, tags)  → 授权并集 + 策略合并（节点→账号→Key）
```

## 异常与边界

| 情形 | 行为 |
|---|---|
| 把节点移到自身或自己的子孙 | 400（`orgtree.WouldCreateCycle`）；数据层 `Chain`/`Descendants` 仍带 visited 兜底，永不挂死 |
| 移动后 `新父深度 + 子树高度 > MaxDepth` | 400 |
| 同一父节点下重名 | 409（`UNIQUE constraint failed` → `isUniqueViolation` → `ErrConflict`） |
| 不同父节点下同名 | 允许 |
| 节点标签名不存在 | 400，`WithParam("tags")` |
| 节点标签名对应的标签之后被删除 | 解析时丢弃该名字（既有语义）；若因此无任何授权则按 `default_grant` 回落通配——文档与控制台文案显式提示，本次**不改变**该全局语义 |
| `DELETE /org/nodes/{id}` 有子节点且无 `cascade` | 400，提示加 `?cascade=true` |
| `cascade=true` | 递归 CTE 取 `(id, depth)`，**一个事务内按 depth 倒序**删（父 FK 是 RESTRICT）；成员关系随 `ON DELETE CASCADE` 移除；**账号与 Key 不受影响** |
| `account_ids` / `org_node_ids` 里有不存在的 id | 404（插入前显式校验，避免 FK 报错变成 500，沿用 `admin.go:385` 的做法） |
| `org_node_ids: []` | 清空该账号的全部组织归属 |
| `Deps.Org == nil`（未启用） | 6 条新路由沿用既有 `portReady` 约定回答 400 `unsupported_parameter`（与 backups/invoices 等未接线端口一致，不引入新状态码）；账号 JSON 的 org 字段为 `[]`；控制台组织页提示"该部署未启用组织架构" |
| 并发写同一节点 | 沿用既有"读—改—写"模式（单写者 SQLite，WAL 并发读）；与标签路径同一取舍，注释写明 |
| 快照返回值被调用方修改 | 契约禁止；注释 + `AllocsPerRun` 测试 + 调用方只遍历的事实三重保障 |

## 测试策略

| 层 | 测试 |
|---|---|
| `internal/orgtree` | 祖先链/深度/子孙/环安全/DFS 顺序；继承名顺序与去重（多成员、祖先优先、乱序输入、孤立父 id）；`Validate` |
| `internal/store` | CRUD、兄弟重名 409、跨父同名允许、成员替换幂等、`SetAccountOrgNodes` 替换、子树删除（只删该子树、成员级联、账号保留）、`cascade=false` 有子节点报错、未知 id → `ErrNotFound` |
| `internal/registry` | **对拍**：无组织数据时新旧实现输出逐项相同；有组织数据时顺序与去重；`NewSnapshot` 旧签名语义；`InheritedTagNames`；**性能守卫**：`AllocsPerRun == 0`（无 Key 标签）+ 两个基准 |
| `internal/routing` | 节点标签参与 `Authorize` 并集与 `mergePolicy`（节点→账号→Key 覆盖顺序）；不存在的节点标签名与改动前同行为；`BenchmarkPlan` 增组织变体 |
| `internal/httpapi` | 5 条路由 CRUD / 角色（viewer 写 403）/ 环 400 / 超深 400 / 未知标签 400 / 未知 account_id 404 / `cascade` 语义 / 审计条目 / `Deps.Org==nil` 时按 `portReady` 约定 400 `unsupported_parameter` 且账号接口不受影响；`GET /accounts?org_node_id=` 含否子孙；`PATCH /accounts/{id}` 的 `org_node_ids` 替换与未知节点 404；既有 `admin_routes_test.go` 守卫（表覆盖、路径参数声明、body 形状与示例、描述可用性）自动覆盖新路由 |
| `internal/arch` | 新包 `internal/orgtree` 与新导入入白名单 |
| 前端静态 | `internal/webui/tests/org_tree_test.mjs`：`tree.js` 的导出与选项名、组织页把 `mode:'sidebar'` 挂到 `sidebar` 插槽、成员保存调 `PUT /org/nodes/{id}/accounts`、只读隐藏写按钮、账户页组织列与筛选参数、router 的 `/org` |
| 控制台走查 | `scripts/ui-harness/tree.page.html`（视图 `tree`）：同一控件挂**侧边栏**与**工作区**两处，断言两种 mode 的层级缩进、折叠/展开改变可见行数、键盘 ↑↓←→Enter、`renderMeta` 在 sidebar 下隐藏/工作区下显示、action 回调、`filter` 命中数、`aria-*` 与 roving tabindex；`scripts/ui-harness/org.page.html`（视图 `org`）：断言**侧边栏里不得出现树**（负向事实，两侧都钉：页面无侧边栏实例、shell 无插槽）、成员勾选发出**带查询串的原始 URL 与 body**、`cascade` 确认文案、viewer 下写按钮 disabled |

## 依赖

- 新包 `internal/orgtree` 只依赖 `internal/domain`（分层白名单新增一行）。
- `internal/registry` 新增对 `internal/orgtree` 的依赖（同层，允许）。
- `internal/httpapi` 新增对 `internal/orgtree` 的依赖（子树过滤要用 `Descendants`）。
- `internal/store` 新增 `internal/org.go`，不新增外部依赖。
- 前端 `static/js/tree.js` 只依赖 `./ui.js` 的 `el`/`clear`。
- 无 schema 破坏性变更（纯新增表），无配置项新增（组织架构不需要部署级开关）。

## 实现与设计差异

实现与设计基本一致（接口、继承顺序、拒绝路径、性能取舍都按上面的决策落地）。差异如下：

| # | 设计 | 实现 | 原因 |
|---|---|---|---|
| 1 | 「5 条新路由 + 2 条改造」 | **6 条新路由** + 2 条改造 | 设计漏了按节点分页读成员的 `GET /org/nodes/{id}/accounts`。成员勾选列表需要它（`include_accounts=true` 的内联列表是有上限的摘要，不是分页读），控制台成员区就用这一条 |
| 2 | `Deps.Org == nil` 时新路由回 501 | 沿用既有 `portReady` 约定回 **400 `unsupported_parameter`** | 501 是本设计的臆造：仓库里所有未接线端口（backups/invoices/…）都是这个既有约定，为组织单独引入一个状态码会让"未启用"有两种含义。已同步 `docs/org.md` |
| 3 | 节点/账号 JSON 的组织字段在无端口时为 `[]` | 同设计；另外 `accountJSON` 的签名从 1 个参数变成 3 个（`a, nodeIDs, orgs`） | 账号列表要为整页只读一次成员关系，不能每行查一次；调用方只有 3 处 |
| 4 | `Snapshot.InheritedTagNames` + 预计算 | 另加了 `Snapshot.OrgNodePath` | 控制台「所属组织」列要显示 `总部/研发部/平台组`，按 id 渲染路径需要祖先链，放在快照里比在 handler 里重建索引便宜 |
| 5 | `orgtree.Index.Validate()` 用「走链长度与记录深度不一致」判定环 | 改成**显式检测重访**（walk-up 时 revisited 即为环） | 原判据漏掉了"环上各节点深度恰好相等"的情形（实现时被 `TestValidateReportsACycle` 抓到）。同时 `computeDepth` 改为**只在父节点确实存在时**才加一，否则孤立节点的 depth 会与实际渲染位置不符 |
| 6 | `ResolveTagRecords` 一般路径"先解码 Key 标签再一次排序" | **原地过滤** Key 标签名（keep-idiom），并在"一个名字都没能加进来"时直接返回预计算切片 | 这是被 `BenchmarkPlan` 逼出来的：直接版每请求多 1 次分配（45 → 46 allocs/op）。原地过滤 + 短路回退把它压回 45，与改前**完全一致**。`appendTagNames` 因此仍被账号侧使用，Key 侧改为直接 `json.Unmarshal`（畸形输入同样返回账号侧） |
| 7 | 侧边栏插槽是"页面往 sidebar 放东西" | **已在 v0.14.0 后撤销**：组织树只留在工作区，`.sidebar-slot` 与 `clear(sidebar)` 及渲染上下文里的 `sidebar` 全部删除 | 唯一使用者消失后，插槽就是没有消费者的死代码。删除比留着更诚实：留着会让人以为"有页面在用"。控件的 `mode:'sidebar'` 能力保留并由 `tree` 视图覆盖 |
| 8 | `account_count` 的取数 | 实现初版把它挂在 `include_accounts` 分支里（**bug**），冒烟时抓到并修复：成员关系**总是**读，只有账号名按需解析 | 一个"存在但恒为 0"的计数比没有这个字段更糟：树上的「N 个账号」会对真有成员的部门显示 0。单测与 harness 都没抓到（harness fixture 直接给了这个字段），**真实网关是这条字段的裁判** |
| 9 | 树控件"只渲染展开的节点" | 同设计；另加"不可达节点按根显示"与"过滤时忽略折叠" | 前者让脏数据（孤儿父 id、环）**可见**而不是消失；后者因为"过滤把自己的命中藏在折叠里"等于过滤没生效 |

另外两处与设计无关、但由本轮走查暴露并修掉的真 bug（都记在 `docs/TODO.md` 的验收记录里）：
`visibleRows` 把折叠节点误判为不可达导致折叠失效；键盘展开/折叠后焦点掉到 `<body>` 导致键盘导航只能用一次。
两者都只有**真实浏览器**能发现，是 `#tree` 这个 harness 视图存在的意义。

## 性能落点（实测，详见 docs/TODO.md「M49 验收记录」）

| 基准 | 改前 | 改后 |
|---|---|---|
| `ResolveTagRecords`（无 Key 标签） | 512 ns / 320 B / 11 allocs | 3.5 ns / 0 B / **0 allocs** |
| `ResolveTagRecords`（有 Key 标签） | 800 ns / 536 B / 17 allocs | 370 ns / 288 B / 9 allocs |
| `routing.BenchmarkPlan` | 3402 ns / 4948 B / 45 allocs | 3459 ns / 4948 B / **45 allocs** |

结论：**组织继承在请求路径上是零成本**（一次 map 查表），路由的分配数与加这个功能之前完全相同。
