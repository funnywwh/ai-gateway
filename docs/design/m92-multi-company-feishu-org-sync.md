# M92 设计文档：导入多家公司的组织架构（每家公司一个飞书自建应用）

> 状态：**已实现（M92）**。
> 前序：[M49 组织架构](m49-organization.md)、[M70 飞书通讯录同步](m70-feishu-org-sync.md)、[M72 账号级飞书身份](m72-account-feishu-identity.md)。
> 面向使用者的规格：[docs/org.md](../org.md) §1/§3/§5/§6、[docs/feishu.md](../feishu.md) §2/§2b/§3/§5c.6、[docs/mcp.md](../mcp.md) §4。
>
> 需求原话：「如何实现导入不同公司的组织架构？」
> 评审确认（2026-09-24）：① 数据来源 = **各家公司的飞书企业，每家公司一个自建应用 App ID/Secret**；
> ② 导入范围 = **部门节点（每家一棵根）+ 人员 → 账号（自动合并已存在账号、缺失的按需创建）**。

## 1. 目标

1. 一台 aigw 能同时维护 **N 家公司**的飞书企业：每家公司用自己的自建应用凭据同步，互不干扰。
2. 每家公司在本地组织树里有**自己的根节点**（公司节点）：该公司的顶层部门挂在其下，人员挂到部门节点。
3. **公司之间强隔离**：两家公司都有「研发部」→ 两个节点；两家公司都有「张三」→ 不会互相抢占账号。
4. **人员 → 账号**：
   - 身份应用（本公司）的同步**逐字沿用今天的行为**（写 `accounts.feishu_*`，它就是 M72 的门户登录身份）；
   - 其它公司的人员只写**公司级映射**（新表 `feishu_person_links`），用于同步的幂等与组织归属，
     **不产生任何登录能力**（他们的飞书租户里没有我们的应用）。
5. 升级安全：旧数据（只有 `feishu_department_id`、没有公司维度）在启动时被认领给身份应用；
   旧顶层部门在**预预览里显式预告**后移入公司节点；同步保持幂等（第二次运行写 0）。
6. 请求热路径零成本：`registry`/`routing`/快照一行不改，`BenchmarkPlan` 的 allocs/op 不变。

### 非目标

- 非飞书身份源（钉钉 / 企业微信 / Azure AD / LDAP）与文件（CSV/Excel）导入。公司来源抽象成
  `[]feishu.Company`，将来换来源不动同步算法。
- 控制台里增删改公司（公司来自配置文件；新增一家公司 = 改配置 + 重启，**同步本身永不重启**）。
- 批量建 API Key / DSH 租户、定时同步、飞书侧删除的传播、跨公司的人员去重（同一个人出现在两家公司时
  是两个独立映射）。
- 非身份应用的人员用飞书登录 DSH 门户（见 §5「与登录的关系」）。

## 2. 实测与代码事实（2026-09-24，本仓库核对）

| 事实 | 位置 |
|---|---|
| 组织树已是**多根森林**，兄弟内重名唯一、跨父同名允许 | `internal/store/migrations/0018_org_structure.sql`、`docs/org.md` §1 |
| 同步只认**一个**应用：`feishu.app_id/app_secret` → `feishu.New(cfg.Feishu)` → `FeishuDeps.Client` | `cmd/aigw/feishu.go`、`internal/feishu/client.go` |
| 目录读取只有一个客户端 + **单条** 60 秒缓存 | `internal/httpapi/admin_org_feishu.go`（`fetchFeishuDirectory`）、`internal/httpapi/server.go`（`feishuDirCache`） |
| 部门 pin 是**全局唯一**：`UNIQUE(NULLIF(feishu_department_id,''))` | `internal/store/migrations/0024_feishu_directory_links.sql` |
| 顶层部门（飞书父为空）的目标父是**森林根**（`parentKey = "root|"`） | `admin_org_feishu.go` 的 `planFeishuOrg` / `siblingKey` |
| 人员合并 = 账号级 `open_id` 通道 ∪ 同名通道（同名只认**没有飞书身份**的账号，先到先得） | `admin_org_feishu.go` 的 `planFeishuOrg` |
| 企业层人员（不属于任何部门）**不落任何节点**，虚拟根 `"0"` 只是范围开关 | `planFeishuOrg`（`in_scope`）、`ensureUserDepartmentNodes` 首行 return |
| 账号身份只有一处：`accounts.feishu_*`，**就是** DSH 门户登录身份（M72） | `internal/store/accounts.go`、`internal/httpapi/admin_feishu.go` |
| 启动期一次性回填的先例：日志 + 审计，失败不致命 | `cmd/aigw/feishu_migrate.go` |
| 后台路由表是唯一真源，进表即成为 MCP 工具；body/query 字段有守卫测试 | `internal/httpapi/admin_routes.go`、`docs/mcp.md` §4.5、`docs/PROCESS.md` |
| 组织树深度上限 16 | `internal/orgtree.MaxDepth` |

**缺口**：换一家公司只能改 `feishu.app_id` 重启（上一家的节点变成"无主"，且无法同时同步两家）；
顶层部门跨公司按名字互相合并；一个账号只能有一个飞书身份且与登录身份同源。

## 3. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | 公司是**配置实体**（`feishu.companies`），不进数据库 | 与现有 `feishu` 块同源同信任模型：密钥不落库、不进备份、不必引入 AES-at-rest、CRUD 路由与控制台页面；每家一个 `*feishu.Client` 天然隔离 tenant token 缓存与限流。代价是新增一家公司要改配置 + 重启（组织同步本身不需要）。**否决** DB 管理 + 控制台公司页 |
| D2 | 公司标识 = **app_id**（`org_nodes.feishu_app_id`、映射表键），公司名只用于展示与根节点默认名 | app_id 稳定、唯一、与凭据同源；改名不影响数据；换 app_id 语义上就是换了一家公司 |
| D3 | 每家一个**公司根节点**，标记 = `(feishu_app_id, feishu_department_id='0')` | 复用既有 pin 机制：改名不丢；"公司本身"用飞书的虚拟根 `"0"` 表达，与既有的"企业层人员开关"含义一致；不需要新表 |
| D4 | 旧部署升级：启动时把「有部门 id 但没有 app_id」的节点认领给身份应用；**顶层部门节点在同步时移入公司根**，预览里显式预告 | 不迁移会留下"一半在根层、一半在公司节点下"的双份树；只迁移被 pin 的公司部门节点，绝不移动操作员手工建的节点。**否决**静默迁移（必须在确认框里看得见）与"不迁移" |
| D5 | 人员映射分两处、各一处真源：**身份应用**写 `accounts.feishu_*`（登录身份，M72 语义一字不改）；**其它公司**写新表 `feishu_person_links`（唯一 `(app_id, open_id)` + 唯一 `(app_id, account_id)`） | 门户登录是安全关键路径，少改 = 少风险；映射表让"同一账号是两家公司的同一个人"成为可能。**否决**给 `accounts` 加 `feishu_app_id`：那会把登录查询改成带 app 作用域，并让一个账号只能有一个飞书身份 |
| D6 | 企业层人员（不属于任何部门）挂到**公司根节点**（仅当虚拟根 `"0"` 在范围内） | 今天他们不落任何节点，"这家公司的直属人员"无处可见。**有意的行为变化**：公司根节点是新建的（无标签），因此不改变任何既有授权 |
| D7 | 公司参数统一叫 `company`，接受 **app_id 或公司名**；省略 = 身份应用 | 向后兼容：M70/M72 的脚本与 MCP 调用一行不改；人可读。未知值 400 并列出已知公司 |
| D8 | 目录缓存按公司分桶（`map[app_id]*entry`），任何写操作清空全部桶 | 缓存的意义是省飞书配额，按公司分桶后每家公司各自计数；写操作稀少，整体失效最简单也最安全 |
| D9 | 唯一索引 `(NULLIF(feishu_department_id,''))` → `(feishu_app_id, NULLIF(feishu_department_id,''))` | 部门 id 是**租户内**标识；跨公司同名部门必须能共存；公司根标记也走这条索引 |
| D10 | 自动同名合并的"已认领"判定推广为：**在任何公司都没有飞书映射、且没有账号级身份** | 今天只查 `accounts.feishu_open_id`；跨公司同名人员不得互相抢占账号。手工绑定允许跨公司（操作员显式选择），自动合并永不 |
| D11 | 公司同步保证一条**不变量**：该公司的顶层部门节点挂在该公司根节点下（手动移到根层会在下次同步被移回） | 否则"部门在公司子树内"这个前提被破坏，标签继承与组织页筛选都会出现"看得见但不在公司里"的节点。超过 16 层的跳过并告警，不阻塞其余部门 |
| D12 | 公司退场只提供**删映射**（`DELETE /org/feishu/companies/{app_id}/links`），节点与成员关系不动 | 与"删节点"是两个不同后果，分成两条接口；公司从配置里移除后数据仍在，操作员可选择保留或清理 |
| D13 | 不做定时同步、不做飞书侧删除传播、不做跨公司人员合并 | 与 M70 的取舍一致：范围由操作员在弹窗里决定，删除必须是人做的决定 |

## 4. 数据模型

### 4.1 migration `internal/store/migrations/0030_feishu_company_scope.sql`

```sql
ALTER TABLE org_nodes ADD COLUMN feishu_app_id TEXT NOT NULL DEFAULT '';

DROP INDEX IF EXISTS idx_org_nodes_feishu_dept;
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_nodes_feishu_dept_app
    ON org_nodes(feishu_app_id, NULLIF(feishu_department_id, ''));
CREATE INDEX IF NOT EXISTS idx_org_nodes_feishu_app ON org_nodes(feishu_app_id);

CREATE TABLE IF NOT EXISTS feishu_person_links (
    feishu_app_id TEXT    NOT NULL,
    open_id       TEXT    NOT NULL,
    union_id      TEXT    NOT NULL DEFAULT '',
    name          TEXT    NOT NULL DEFAULT '',
    account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    bound_by      TEXT    NOT NULL DEFAULT '',
    bound_at      INTEGER,
    created_at    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (feishu_app_id, open_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_person_links_account
    ON feishu_person_links(feishu_app_id, account_id);
```

`PRIMARY KEY (feishu_app_id, open_id)`：open_id 只在**应用内**唯一，跨应用可能撞串 —— 复合键从根上消除该问题。
`UNIQUE (feishu_app_id, account_id)`：一家公司里一个账号只对应一个人（跨公司允许，是同一个人的情况）。

### 4.2 domain

```go
// internal/domain/org.go
type OrgNode struct {
    // …既有字段…
    // FeishuAppID 是把这个节点带进来的那家公司的应用 id（空 = 手工节点或未被任何公司同步）。
    // 它与 FeishuDepartmentID 一起构成公司作用域的 pin：同一部门 id 属于不同公司时互不冲突。
    FeishuAppID string
}

// FeishuPersonLink 是"某家公司通讯录里的某个人 = 本地某个账号"的映射（M92）。
// 只有非身份应用写它：身份应用的映射在 accounts.feishu_* 上，因为那同时是登录身份。
type FeishuPersonLink struct {
    AppID     string
    OpenID    string
    UnionID   string
    Name      string
    AccountID int64
    BoundBy   string     // "sync" 或管理员用户名
    BoundAt   *time.Time
}
```

### 4.3 store（`internal/store/org.go` + 新文件 `internal/store/org_feishu.go`）

```go
ListFeishuPersonLinks(ctx) ([]domain.FeishuPersonLink, error)          // 全表，按 app_id, open_id 排序
UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink) error             // (app_id,open_id) 幂等更新；(app_id,account_id) 冲突 → ErrConflict
DeleteFeishuPersonLink(ctx, appID, openID string) (bool, error)
DeleteFeishuPersonLinksByApp(ctx, appID string) (int, error)

// 签名变更：公司作用域
SetOrgNodeFeishuDepartment(ctx, nodeID int64, appID, departmentID string) error

// 启动认领（幂等）：把旧的"有部门 id 但没有公司"的节点标记给身份应用。不动 accounts。
AdoptLegacyFeishuScope(ctx, identityAppID string) (updated int, err error)
```

### 4.4 端口（`internal/httpapi/admin_ports.go`）

```go
// FeishuPersonAdmin 是公司级人员映射的独立窄端口，不进 OrgAdmin：测试替身不必为它实现全部节点方法。
type FeishuPersonAdmin interface {
    ListFeishuPersonLinks(ctx) ([]domain.FeishuPersonLink, error)
    UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink) error
    DeleteFeishuPersonLink(ctx, appID, openID string) (bool, error)
    DeleteFeishuPersonLinksByApp(ctx, appID string) (int, error)
}
```

`Deps` 增 `FeishuPeople FeishuPersonAdmin`；`feishuDirectoryGate` 增 `People FeishuPersonAdmin`，
未接线时沿用既有 `portReady` 约定 400 `unsupported_parameter`。`OrgAdmin.SetOrgNodeFeishuDepartment`
的签名随之变化（两个测试替身同步更新）。`internal/orgtree`、`internal/registry`、`internal/routing` **不改**。

### 4.5 启动认领（`cmd/aigw/feishu_migrate.go`）

```go
// adoptLegacyFeishuScope 在 migrateKeyFeishuBindings 之后调用（仅 feishu.enabled && app_id != ""）。
// 与它同一约定：失败只记日志（旧数据保持原样），成功打一行 Info 让运维在升级日志里看到数字。
func adoptLegacyFeishuScope(ctx context.Context, db *store.DB, appID string, log *slog.Logger)
```

## 5. 配置（`internal/config/config.go` + `config.example.yaml`）

```yaml
feishu:
  enabled: true
  app_id: "cli_aaa"            # 本部署的身份应用（本公司）
  app_secret: "..."
  company_name: "本公司"        # 新增：同步弹窗里的展示名 + 本公司根节点默认名（空 = "本公司"）
  companies:                   # 新增：只用于组织架构导入的其它公司
    - name: "某某科技"          # 唯一；同步弹窗里的公司名，也是根节点默认名
      app_id: "cli_bbb"
      app_secret_env: "GW_FEISHU_ACME_SECRET"   # 与 app_secret 二选一；env 优先
      app_secret: ""
      root_node: ""            # 可选：本地根节点名（默认 = name；可指向已建好的同名根节点，同步会认领它）
```

```go
type Feishu struct {
    // …既有字段…
    CompanyName string          `yaml:"company_name"`
    Companies   []FeishuCompany `yaml:"companies"`
}

type FeishuCompany struct {
    Name         string `yaml:"name"`
    AppID        string `yaml:"app_id"`
    AppSecret    string `yaml:"app_secret"`
    AppSecretEnv string `yaml:"app_secret_env"`
    RootNode     string `yaml:"root_node"`
}
```

校验（错误信息带 `feishu.companies[i].字段` 下标，启动即失败）：

| 规则 | 说明 |
|---|---|
| `companies` 非空 ⇒ `enabled: true` | 没有身份应用就没有通讯录端点的配置来源 |
| `name` trim 后非空、≤64 rune、不以 `cli_` 开头 | 与 `domain.MaxOrgNodeNameRunes` 同界（config 不 import domain，注释指向该常量）；名字会成为节点名 |
| `app_id` 匹配既有 `feishuAppIDRE`；公司间互不重复且 ≠ `feishu.app_id` | 身份应用不能既是本公司又是别家公司 |
| `app_secret` / `app_secret_env` 至少一个非空；`app_secret_env` 在 `applyEnv` 解析，空值 = 启动报错（写明变量名） | 与 `GW_FEISHU_APP_SECRET` 同一套"密钥不进 YAML"的做法 |
| 有效根名（`root_node || name`；本公司 = `company_name || "本公司"`）互不重复 | 两个公司根同名会在同层撞车（唯一索引 409） |
| 公司数 ≤ 32 | 有界，避免配置手滑把对话框撑爆 |

## 6. 运行时装配（`internal/feishu/company.go` + `cmd/aigw/feishu.go`）

```go
// internal/feishu/company.go
type Company struct {
    AppID    string
    Name     string // 配置里的公司名（展示）
    RootName string // 公司根节点的默认名（root_node || name）
    Identity bool   // 身份应用：只有它能写 accounts.feishu_*，也只有它参与登录
    Client   *Client
}

// NewCompanyClient 复用配置里的 contact/tenant-token 端点与 timeout，只换 AppID/AppSecret。
func NewCompanyClient(base config.Feishu, appID, secret string) *Client

// FindCompany 解析 company 参数：app_id 或公司名，trim 后精确匹配；未知时返回列出全部已知公司的错误。
func FindCompany(list []Company, token string) (*Company, error)
```

`httpapi.FeishuDeps` 增 `Companies []Company`，**索引 0 恒为身份应用**（`Client` 字段保留并指向同一个客户端，
因此 OAuth / 绑定 / 控制台登录三条流程一行不改）。`cmd/aigw/feishu.go` 负责构造；启动日志打印
公司数与名字，**绝不打印密钥**。

## 7. 同步算法（`internal/httpapi/admin_org_feishu.go`）

### 7.1 取目录

```go
func (s *Server) fetchFeishuDirectory(w, r, company *feishu.Company, refresh bool) (*feishu.Directory, bool, bool)
```

`s.feishuDirCache` 从单条改成 `map[string]*feishuDirectoryEntry`（键 = app_id）；`invalidateFeishuDirectory()`
清空全表（写操作稀少，整体失效最安全）。

### 7.2 公司根解析（preview 与 sync 共用）

```go
type companyRootPlan struct {
    NodeID   int64  // 0 = 本次要创建
    Name     string
    Matched  string // "marker" | "name" | ""
    WillCreate bool
    WillAdopt  bool
    Reparent   []*domain.OrgNode // 该公司现有的顶层部门节点（见 D11）
    Warnings   []string          // root_not_at_top / reparent_depth_exceeded …
}
```

解析顺序：

| # | 条件 | 结果 |
|---|---|---|
| 1 | 存在 `FeishuAppID == app && FeishuDepartmentID == "0"` 的节点 | `matched:"marker"`；若它不在根层则告警 `root_not_at_top`（仍然用它，位置由操作员决定） |
| 2 | 根层存在 `name == RootName` 且 `FeishuDepartmentID == ""`（未被任何公司/部门认领）的节点 | `matched:"name"`、`will_adopt:true`（写 app_id + `"0"`） |
| 3 | 否则 | `will_create:true`（可被同级重名 409 挡住 → `root_name_conflict` 警告，提示用 `root_node` 换名） |

### 7.3 部门匹配（公司作用域）

- `pinnedByDept` 只为 `node.FeishuAppID == company.AppID` 的节点建条目；
- 顶层部门（飞书父为空）的**目标父 = 公司根节点**，于是 `parentKey` 从 `"root|"` 变成 `"p<公司根 id>|"`：
  两家公司的同名顶层部门天然分叉；同名合并规则（同父下同名、已被 pin 到别的部门时不覆盖）保持不变；
- 公司根解析/创建发生在这个循环之前，因此子部门的父 id 一定已经存在。

### 7.4 认领旧顶层部门（D4/D11）

`FeishuAppID == app && FeishuDepartmentID ∉ {"", "0"} && parent == nil` 的节点在本次运行里移入公司根下：
- 高度守卫：`index.SubtreeHeight(id) + 1 > orgtree.MaxDepth` → 跳过 + `reparent_depth_exceeded` 警告；
- 预览与响应都给 `reparent_node_ids`，确认框显示「将把 N 个顶层部门移入公司节点」；
- 搬移发生在节点创建/认领之后、人员挂载之前；成员关系与节点标签都不动。

### 7.5 人员合并

| 通道 | 身份应用 | 其它公司 |
|---|---|---|
| 按 id | `accounts.feishu_open_id == open_id` | `feishu_person_links(app_id, open_id)` 命中 |
| 同名 | 账户名完全相等 **且** 该账号在任何公司都没有飞书映射、也没有账号级身份 | 同左 |
| 写回 | `BindAccountFeishu`（M72 原样） | `UpsertFeishuPersonLink`（`bound_by:"sync"`） |
| 冲突 | `identity_taken`（先到先得，绝不覆盖） | 同左（`identity_taken`），另有 `(app_id, account_id)` 冲突 → 409 报告 |

**企业层人员**（`DepartmentIDs` 为空且虚拟根 `"0"` 在范围内）：加入**公司根节点**
（本次新建的公司根在 `justCreated` 之后补挂）。这是 M70 行为的一处有意变化（D6）。

### 7.6 单个人操作

`ensureUserDepartmentNodes(ctx, company, gate, dir, person)` 复用同一套公司根解析 + 顶层部门归一，
供三个 per-person 路由使用。非身份公司的「创建用户」写账号 + 公司映射（**不写** `accounts.feishu_*`），
「绑定账号」写映射（身份应用则是写账号身份，行为不变），「解绑」只删该公司的映射。

### 7.7 审计与 JSON

- 审计：既有 `sync_feishu` / `create(org_node)` / `feishu_bind` 的 changes 增 `"company": <app_id>`；
  新增动作 `purge_feishu_links`。
- `orgNodeJSON` 增 `feishu_app_id` 与 `company`（配置里的公司名；未知名为空串）。
- `accountJSON` 的 `feishu` 增 `"links":[{app_id,company,open_id,name,union_id,bound_by,bound_at}]`
  （恒存在，可为空数组），签名变 `accountJSON(a, nodeIDs, orgs, links)`（调用方 3 处）；
  账号列表按页读一次映射表，与成员关系一样一次性传入。

## 8. 接口（进 `admin_routes.go` 同一张表 ⇒ 自动成为 MCP 工具）

| Method | Path | 变化 | 工具名 | 角色 |
|---|---|---|---|---|
| GET | `/admin/api/v1/org/feishu/companies` | **新增** | `admin_list_feishu_companies` | viewer |
| GET | `/admin/api/v1/org/feishu/directory` | 增 query `company`；响应增 `company{}` | `admin_list_feishu_directory` | admin |
| POST | `/admin/api/v1/org/feishu/sync` | 增 body `company`；响应增 `root_node_id` / `reparented_nodes` | `admin_sync_feishu_org` | admin（危险） |
| POST/PUT/DELETE | `/admin/api/v1/org/feishu/users/{open_id}/account` | 增 query `company` | 名称不变 | admin（危险） |
| DELETE | `/admin/api/v1/org/feishu/companies/{app_id}/links` | **新增**（退场：只删映射） | `admin_purge_feishu_company_links` | admin（危险） |

`company` 字段说明（MCP 描述与守卫测试要求）：

> 公司标识：app_id（`cli_…`）或配置里的公司名。省略 = 本部署的身份应用（本公司）。
> 未知值 400，消息里列出已知的 app_id 与公司名。

响应形状：

```json
// GET /org/feishu/companies
{"data":[{"app_id":"cli_aaa","name":"本公司","identity":true,
          "root_node_id":12,"root_node_name":"本公司","company_nodes":23,"linked_accounts":87}],
 "count":2,"identity_app_id":"cli_aaa"}

// directory / sync 里的公司块
"company":{"app_id":"cli_bbb","name":"某某科技","identity":false,
           "root":{"node_id":41,"name":"某某科技","matched":"marker","will_create":false,"will_adopt":false},
           "reparent_node_ids":[3,4]}
```

`stats` 增 `root_will_create` / `root_will_adopt` / `reparent_nodes`；同步响应增 `root_node_id` 与
`reparented_nodes:[{id,name,department_id}]`。`DELETE …/companies/{app_id}/links` 对身份应用 400
（指向 `DELETE /org/feishu/users/{open_id}/account`）。

## 9. 数据流

```
控制台「同步飞书」（选公司 X）
   └─ GET /org/feishu/companies        → 公司下拉（名字 + 是否身份应用 + 根节点）
   └─ GET /org/feishu/directory?company=X&departments=…
        ├─ company.Client.Directory()（按公司缓存的 60s 快照）
        └─ planFeishuOrg(company, …)
             ├─ planCompanyRoot()：marker / 同名认领 / 新建 + 待搬移的顶层部门
             ├─ 部门按 (app_id, feishu_department_id) 或"公司根下同名"匹配
             └─ 人员按 (app_id, open_id) 或"同名且未被任何公司认领"匹配
   └─ POST /org/feishu/sync {company:X, department_ids:[…]}
        ├─ 公司根：创建 / 认领 / 补标记
        ├─ 顶层部门搬移（高度守卫）
        ├─ 部门节点：创建 / 补 pin
        ├─ 人员：写映射（非身份公司）或账号身份（身份公司）+ 挂部门节点（含企业层人员 → 公司根）
        ├─ audit(sync_feishu, {company}) + reload(invalidateAll=true)
        └─ 失效目录缓存

请求热路径：与 M92 之前逐字节相同（本里程碑不碰 registry / routing / 快照）
```

## 10. 异常与边界

| 情形 | 行为 |
|---|---|
| `company` 未知 / 空串 | 400，列出已知 app_id 与公司名 |
| 公司根名与已认领节点同名 | 不认领；新建撞同级唯一索引 → 409 → `root_name_conflict` 警告（用 `root_node` 换名） |
| 公司根被删（cascade） | 下次同步重建；树内成员关系已随之消失（与删节点同一语义） |
| 公司从配置移除 | 节点、映射、成员关系全部保留；同步不再可达；`feishu.links` 里 `company:""`（app_id 仍在） |
| 同名人员已被其它公司认领 | 不自动合并，留在"未匹配"，由操作员「绑定账号」显式选择 |
| 同一账号在同一家公司对应两个人 | `(app_id, account_id)` 唯一索引 → 409 并说明原因 |
| 旧顶层部门子树高度 + 1 > 16 | 跳过搬移 + `reparent_depth_exceeded`，其余部门照常 |
| 公司缺两个只读权限（无名字） | 沿用 `names_available:false`；无名部门跳过、人员逐行手工 |
| 目录被截断（>500 部门 / >40 页） | 沿用 `directory_truncated`，按公司独立计数 |
| 飞书侧失败 / 限流 | 沿用 502 + `feishuUpstreamError` 的中文原因，日志留数字码 |
| `Deps.FeishuPeople == nil` | 沿用 `portReady`：400 `unsupported_parameter` |
| 只读角色 | 公司列表 viewer 可读；目录/同步/per-person/purge 403 |
| 并发同步同一公司 | 沿用"读—改—写 + 唯一索引兜底"（单写者 SQLite、WAL） |

## 11. 与登录的关系（口径）

- **只有身份应用（本公司）的映射是登录身份**：门户登录查 `accounts.feishu_open_id`（M72），本里程碑
  一字未改。
- **其它公司的人员只建立组织映射**：他们的飞书租户里没有本部署的应用，不可能完成 OAuth；因此
  「绑定」这些人是"这家公司的这个人 = 本地这个账号"，用于组织归属与后续放权，**不产生登录能力**。
  控制台与 `docs/feishu.md` 都按这个口径写，避免运维把它误判为故障。

## 12. 测试策略

| 层 | 测试 |
|---|---|
| `internal/config` | 公司列表校验矩阵：缺 name / 名字超长 / `cli_` 前缀 / app_id 形状 / 重复 app_id / 与身份应用同 app_id / 密钥双空 / `app_secret_env` 空值 / 根名重复 / 未 enabled 却配 companies / 超过 32 家 |
| `internal/store` | 0030 索引行为（同部门 id 两个 app 可共存、同 app 重复 409）；`SetOrgNodeFeishuDepartment` 公司作用域；映射表 upsert / `(app_id,account_id)` 冲突 / 账号删除级联 / 按 app 清理；`AdoptLegacyFeishuScope` 幂等（两次调用只生效一次） |
| `internal/httpapi` | 复用 `orgFeishuFixture` + 第二个 `dirStub`（真 store）：① 两家公司同名顶层部门 → 各自建在公司根下；② 第二次同步写 0；③ 同名人员不跨公司抢占；④ 公司 B 的绑定**不改** `accounts.feishu_*`；⑤ 企业层人员挂公司根；⑥ 旧顶层部门搬移与超深跳过；⑦ `?company=` 省略 / 名字 / app_id / 未知；⑧ per-person 三路由带 company；⑨ `DELETE …/links` 只删映射；⑩ `feishu.links` 与节点 `feishu_app_id`/`company` 形状；⑪ 角色（viewer 403）与审计字段 |
| `internal/httpapi` 守卫 | 既有 `admin_routes_test.go` 的表覆盖 / 路径参数 / body 形状与示例守卫自动覆盖新路由（需按 §4.5 补齐字段） |
| 前端静态 | `internal/webui/tests/org_feishu_test.mjs`：读 `/org/feishu/companies`；directory/sync/per-person 请求都带 `company`；切换公司重新读取；单公司不渲染下拉 |
| harness | `scripts/ui-harness/org_feishu.page.html` + `fixtures.json` 增第二家公司；视图 `org-sync` / `org-sync-readonly` / `org-sync-nonames` 断言下拉存在、切换后请求带 company |
| 性能 | 无热路径改动：`go test -bench BenchmarkPlan -run '^$' ./internal/routing ./internal/registry` 与改前逐项相同 |

**真机验收（宿主执行）**：升级 → 启动日志出现 `legacy feishu org nodes adopted` → 同步本公司
（预览显示"新建公司节点 + 移入 N 个顶层部门"，确认后执行，再同步一次 0 写入）→ 配第二家公司重启 →
同步 → 校对两家公司树互不干扰、账号 `feishu.links` 正确、门户飞书登录无回归。

## 13. 依赖

- 无新外部依赖；`internal/store`、`internal/config`、`internal/httpapi` 的分层白名单不变
  （`internal/feishu` 已有的 `internal/config`、`internal/domain` 依赖足够）。
- 新增文件：`internal/store/org_feishu.go`、`internal/feishu/company.go`、
  `internal/store/migrations/0030_feishu_company_scope.sql`。
- 控制台仍是零构建原生 ES 模块：只改 `pages/org_feishu.js`、`pages/org.js`（口径不变）。

## 14. 实现与设计差异

实现与设计基本一致（配置模型、数据模型、`company` 参数、公司节点解析顺序、人员映射的两处真源、控制台
公司下拉都按上面的决策落地）。差异与补充如下：

| # | 设计 | 实现 | 原因 |
|---|---|---|---|
| 1 | 公司根名冲突时"警告并继续" | 计划阶段就判定 `Blocked`，`POST /sync` **写任何数据之前 409**（消息点名 `feishu.companies[].root_node`） | 继续执行会让顶层部门落到森林根——正好是本设计要消除的形状。预览里也回 `company.root.blocked:true`，控制台据此禁用「同步」按钮 |
| 2 | 同名根节点的冲突只覆盖"另一家公司的公司节点" | 还覆盖"根层同名节点已被某个飞书部门占用"（`FeishuAppID != ""` 或 `FeishuDepartmentID != ""`） | 认领一个部门节点当公司节点会把公司接在别人的部门上，比新建同名节点更糟 |
| 3 | 认领旧顶层部门（reparent）只改父 | 父被移的同时，若该节点的 `feishu_app_id` 还是空（M92 之前写的），顺手补上 `(公司, 部门 id)` | 同一次写就能把这对键补齐，避免它继续依赖"空 app = 身份应用"的兜底；也让"公司同步后每个公司节点都带公司"这条不变量对这次动过的节点成立 |
| 4 | 旧数据的认领只在启动时做（`AdoptLegacyFeishuScope`） | 同步路径也有两层自愈：按部门 id 命中的旧行会补打公司（`WillStampScope`），reparent 时同样补打 | 启动认领**失败只记日志**（沿用 M72 回填的约定），所以同步不能假设它一定跑过 |
| 5 | 人员映射：身份应用写 `accounts.feishu_*`、其它公司写映射表 | 一致；另外 `feishu_person_links` 还多了 `(app_id, account_id)` 唯一索引，并把冲突报成 `skipped_users.reason = "account_taken"`（身份应用的冲突仍是 `identity_taken`） | 一句话说清两种冲突：同一家公司两个人抢一个账号 vs. 一个飞书身份被两个账号抢 |
| 6 | 预览里的公司块 | 除 `app_id/name/identity/root/reparent_node_ids/warnings` 外，另给 `company.root.blocked`；部门行另给 `local.local_depth`、`local.will_reparent`、`local.will_stamp_scope`，人员行另给 `join_company_root` | 运营要能一眼看出"这次会不会建公司节点/搬哪些部门"，而不是只看汇总数字 |
| 7 | 控制台：公司下拉 + 请求带 company | 一致；补充：切换公司会重置勾选与选中节点并重新读取（`seeded=false`），目录读取仍走服务端缓存（缓存按公司分桶），单公司部署不渲染下拉且请求不带 `company` | 沿用旧公司的勾选会导致跨公司的范围；单公司部署保持 M70 的请求形状不变 |
| 8 | 账号「飞书」列显示公司级映射 | 一致，`accountJSON.feishu.links` 恒存在（可为空数组），人员过滤也匹配这些姓名 | 与 `feishu.bound` 的既有形状约定一致（字段恒在，页面不必猜版本） |
| 9 | `DELETE /org/feishu/companies/{app_id}/links` | 一致；身份应用 400 并指向逐人解绑 | 身份应用的映射就是登录身份，批量删会让一批人**无法登录**（而不是少一条组织关系），必须是逐人的决定 |
| 10 | 企业层人员挂到公司节点 | 一致；这正是 M70 的一处行为变化：以前他们不落任何节点。系统同步测试相应更新（`linked_users` 多出这条） | 公司层人员需要一个可见的落点，"这家公司的直属人员"在树里才看得见 |
| 11 | 配置校验：名字不得以 `cli_` 开头、根名唯一、公司数 ≤ 32 | 一致；`app_secret_env` 解析放在 `applyEnv`（未设置的环境变量由校验报错点名变量） | 与 `GW_FEISHU_APP_SECRET` 同一套"密钥不进 YAML"的做法 |
| 12 | 单元/接口测试 | 另有三个既有测试按新形状更新：组织同步测试的 `created_nodes` 多出公司节点、顶层部门挂在公司节点下、无名同步会建公司节点、企业层人员获得公司节点成员关系 | 这些是**预期的行为变化**，不是测试放宽；每一处都在这里点名 |

性能落点（实测，本机，2026-09-28）：

| 基准 | 改前（M49 记录） | 改后 |
|---|---|---|
| `routing.BenchmarkPlan` | 4948 B / 45 allocs | 4948 B / **45 allocs** |
| `registry.BenchmarkResolveTagRecords`（无 Key 标签） | 0 B / 0 allocs | 0 B / **0 allocs** |
| `registry.BenchmarkResolveTagRecords`（有 Key 标签） | 288 B / 9 allocs | 288 B / **9 allocs** |

本里程碑没有改请求路径上的任何代码（只改管理面/同步面），分配数与改前逐项相同。
