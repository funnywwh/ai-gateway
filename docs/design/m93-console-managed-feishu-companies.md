# M93 设计文档：在控制台管理公司的飞书应用（新增/编辑/停用/删除 + 测试连接）

> 状态：**已实现（M93）**。
> 前序：[M92 多公司组织架构导入](m92-multi-company-feishu-org-sync.md)、[M60 飞书身份](m60-aigw-key-feishu-binding.md)、
> [M44 供应商并发](m44-provider-concurrency-queue.md)（凭据加密落库的先例：`internal/creds` + provider 凭据）。
> 面向使用者的规格：[docs/feishu.md](../feishu.md) §2b/§3/§5c.6、[docs/org.md](../org.md) §3/§5/§6、[docs/mcp.md](../mcp.md) §4。
>
> 需求原话（2026-09-28）：「用户可以在管理后台添加每个公司的应用吗？」→ 评审选择：
> **做成控制台可管理**（新表 + `credentials_key` 加密存密钥 + admin 路由 + 控制台页面，含「测试连接」）。

## 1. 目标

M92 把"多公司导入"做出来了，但公司只能写在配置文件里：**加一家客户公司 = 改 YAML + 重启网关**。
本里程碑把它变成控制台里的一次操作：

1. 控制台新增独立页面「公司」（`#/companies`）：列出身份应用、配置里的公司、以及控制台登记的公司；
2. **新增 / 编辑 / 停用 / 删除**客户公司（App ID、App Secret、根节点名、备注、启停）；
3. **「测试连接」**：不落库先验证——用给定或已存的密钥铸 `tenant_access_token` 并读一页部门，
   当场回答"密钥对不对、两个只读权限有没有、名字读不读得到"；
4. 密钥**加密落库**（AES-256-GCM，`credentials_key`），永不回显、不进日志、不进审计；
5. 不重启网关即可上线一家新客户；同步、目录、逐人操作与 M92 完全一致（它们只多了一个数据来源）。

### 非目标

- **身份应用（本公司）仍然只在配置里**：它还要回调地址、签名密钥、门户/控制台登录，属于部署身份，
  不是"目录源"。页面上它是一行只读记录，并指向 `feishu.app_id/app_secret`。
- 不做多应用登录（一家公司的飞书身份登进 DSH 门户）、不做按公司隔离控制台权限、
  不做密钥历史版本/自动轮换、不接外部 KMS/Vault、不做公司级配额。
- 不改请求热路径（数据面鉴权与 M92 之前逐字节相同）。

## 2. 现状与缺口（M92 已落地的形状）

| 事实 | 位置 |
|---|---|
| 公司来自配置：`feishu.company_name` + `feishu.companies[]`（name/app_id/app_secret(_env)/root_node） | `internal/config/config.go`、`cmd/aigw/feishu.go`（`CompaniesFromConfig`） |
| 控制台只有**只读**列表（同步弹窗的公司下拉） | `GET /admin/api/v1/org/feishu/companies`、`internal/webui/static/js/pages/org_feishu.js` |
| 停用/删除/新增公司：无 | 路由表里只有 `GET` 与 `DELETE …/links`（只删映射） |
| 密钥加密落库的先例 | `internal/creds`（AES-256-GCM，AAD = 8 字节 provider id）+ `store.providers.credentials_enc` |
| 目录读取是**有界**的：`DirectoryOptions.MaxDepartments` 会在到达上界时提前返回 | `internal/feishu/directory.go`（`Truncated`） |

## 3. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | **身份应用与客户公司分治**：身份应用只在配置；控制台只管理"客户公司"。配置里的客户公司保留但**只读**（页面标"由配置提供"） | 身份应用带着回调 URL 与三个签名密钥，属于部署身份，搬进数据库只会制造两份真源。配置里的公司保留是**向后兼容**：M92 刚部署的部署不用改任何东西；想搬进控制台就"在控制台新建 + 删掉配置里那家"（文档写明） |
| D2 | 密钥加密落库：新表 `feishu_apps.secret_enc`，AES-256-GCM，AAD 用**新的作用域名** `feishu_app` + 行 id | provider 凭据的 AAD 是裸的 8 字节 id，两家表如果共用同一个 AAD 空间，一条 provider 密文能被塞进公司行解密（id 相同即可）。加作用域前缀让两套密文**互不可用**，也不动既有 provider 行（它们继续用旧 AAD） |
| D3 | `credentials_key` 未配置时**拒绝**在控制台保存公司（400 + 指向配置方式） | 没有密钥就不该落库；静默存明文是绝对不能做的。错误信息给出两条出路：配 `credentials_key`，或继续用 `feishu.companies` |
| D4 | `app_id` **创建后不可改**（PATCH 拒绝）；可改的是名称、根节点名、备注、启停、密钥 | app_id 是存储在数据里的公司身份（`org_nodes.feishu_app_id`、`feishu_person_links.feishu_app_id`、审计）。改它等于换一家公司，会让已导入的节点/映射失去归属——那是"新建一家 + 清理旧的"，不是编辑 |
| D5 | 删除**只删登记**，节点/映射/成员关系都不动（响应与确认框给出计数与清理路径） | 与 M92 的 `DELETE …/links`（只删映射）同一条取舍：删节点、删映射、删登记是三种后果，分成三条接口 |
| D6 | 启用/停用（`enabled`）：停用的公司**不出现在同步下拉**，按 app_id/名字解析时 400「该公司已停用」，但节点、映射与登记保留 | 客户暂停服务时最需要的是"先别同步了"，而不是"删掉再重建" |
| D7 | 「测试连接」是一等公民：`POST /org/feishu/companies/probe`，接受 `id`（用已存密钥）或 `app_id + app_secret`（还没保存） | 控制台里最常见的失败是"密钥抄错"与"两个只读权限没发版本"，同步弹窗 15 秒后才报错。探针用**有界**目录读（`MaxDepartments≈6`）在 2–3 次调用内回答，并区分 凭据被拒 / 权限不足（没名字）/ 能读。密钥既不落库（未保存时）也不回显、不进日志 |
| D8 | 合并规则：**以 app_id 为合并键，配置优先**；重名（不同 app_id）时配置方赢得"按名解析"，控制台行标警告；新建/改名**不接受**与其它公司重名 | `company` 参数接受 app_id 或名字，所以名字必须唯一才有意义。同 app_id 出现在配置与库里 = 同一家公司的两条记录，配置优先（显式且可审计），库里的那条成为冗余行（页面标注），不会在列表里出现两次 |
| D9 | 控制台是**新页面** `#/companies`（不是弹窗里的二级对话框） | 这是"公司注册表"，不是某一次同步的一部分：要有列表、状态、每行三个动作（编辑/测试/删除），与「供应商」「API Keys」页同形。同步弹窗的下拉加一个「管理公司…」入口跳过去，页面每行加「同步这家公司」跳回来并预选 |
| D10 | 4 条新路由进 `admin_routes.go` 同一张表（自动成为 MCP 工具）：create/update/delete 是危险接口带 `confirm_reason`，probe 不是（不改状态） | 与 M49/M80/M92 同一约定；`app_secret` 字段在描述里写明"加密落库、永不回显、不进日志与审计" |
| D11 | 审计：`create`/`update`/`delete` on `target_type=feishu_company`，changes 里只写 `secret_changed: true`，绝不写密钥本身或它的前缀 | 审计要能回答"谁在什么时候改了这家公司的密钥"，但不能因此留下一份可用的秘密 |
| D12 | 不做"把配置里的公司一键导入控制台" | 需要迁移的是少数部署，手工新建一次即可；自动导入会制造"配置与库两条记录指向同一家公司"的模糊态（D8 已经要处理一种，不再制造第二种） |

## 4. 数据模型

### 4.1 migration `internal/store/migrations/0031_feishu_apps.sql`

```sql
-- 客户公司的飞书应用登记（M93）：身份应用仍在配置里，这张表只装"控制台管理的目录源"。
CREATE TABLE IF NOT EXISTS feishu_apps (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    app_id     TEXT    NOT NULL,
    -- AES-256-GCM（internal/creds，AAD = "feishu_app" + id）。明文只在请求期存在于内存。
    secret_enc BLOB,
    root_node  TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_by TEXT    NOT NULL DEFAULT '',
    updated_by TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_apps_app_id ON feishu_apps(app_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_apps_name ON feishu_apps(name);
```

### 4.2 domain

```go
// internal/domain/feishu_app.go
// FeishuApp is one client company's self-built Feishu application as the console manages it
// (M93): the row that turns "add a customer company" into a console action. The identity
// application is never one of these — it stays in the configuration.
type FeishuApp struct {
    ID        int64
    Name      string
    AppID     string
    SecretEnc []byte // sealed; the plaintext never leaves the request that wrote or reads it
    RootNode  string // optional override of the company node's name
    Note      string
    Enabled   bool
    CreatedAt, UpdatedAt time.Time
    CreatedBy, UpdatedBy string
}
```

### 4.3 creds：作用域 AAD + 公司密钥封口器

```go
// internal/creds/creds.go（新增；旧 Encrypt/Decrypt 保持"无作用域"，既有 provider 密文不动）
func EncryptScoped(key []byte, scope string, id int64, plaintext []byte) ([]byte, error)
func DecryptScoped(key []byte, scope string, id int64, ciphertext []byte) ([]byte, error)
// AAD = scope + 0x00 + big-endian uint64(id)

// internal/creds/sealer.go（新增类型，与 Sealer 并列）
type CompanySealer struct{ key []byte }
func NewCompanySealer(key []byte) *CompanySealer
func (s *CompanySealer) Ready() bool
func (s *CompanySealer) Seal(appRowID int64, secret string) ([]byte, error)
func (s *CompanySealer) Open(appRowID int64, ciphertext []byte) (string, error)
```

### 4.4 store（`internal/store/feishu_apps.go`）

```go
ListFeishuApps(ctx) ([]*domain.FeishuApp, error)          // 全表，按 name 排序
GetFeishuApp(ctx, id int64) (*domain.FeishuApp, error)
GetFeishuAppByAppID(ctx, appID string) (*domain.FeishuApp, error)
UpsertFeishuApp(ctx, *domain.FeishuApp) (int64, error)    // (name)/(app_id) 唯一冲突 → ErrConflict
SetFeishuAppEnabled(ctx, id int64, enabled bool, by string) error
DeleteFeishuApp(ctx, id int64) (bool, error)              // 只删登记行
CountFeishuAppData(ctx, appID string) (nodes, links int, err error) // 删除前给操作员看的计数
```

### 4.5 端口（`internal/httpapi/admin_ports.go`）

```go
type FeishuCompanyAdmin interface {
    ListFeishuApps(ctx) ([]*domain.FeishuApp, error)
    GetFeishuApp(ctx, id int64) (*domain.FeishuApp, error)
    GetFeishuAppByAppID(ctx, appID string) (*domain.FeishuApp, error)
    UpsertFeishuApp(ctx, *domain.FeishuApp) (int64, error)
    SetFeishuAppEnabled(ctx, id int64, enabled bool, by string) error
    DeleteFeishuApp(ctx, id int64) (bool, error)
    CountFeishuAppData(ctx, appID string) (nodes, links int, err error)
}

// FeishuCompanySecrets 加解密公司密钥。它只被"构造客户端"的路径调用，返回值永不进 JSON。
type FeishuCompanySecrets interface {
    Ready() bool
    Seal(appRowID int64, secret string) ([]byte, error)
    Open(appRowID int64, ciphertext []byte) (string, error)
}
```

`Deps` 增 `FeishuApps FeishuCompanyAdmin`、`FeishuAppSecrets FeishuCompanySecrets`；`cmd/aigw` 传 `db` 与
`creds.NewCompanySealer(credKey)`（`credentials_key` 为空时是未就绪的 sealer，写路径据此 400）。

## 5. 公司的合并与解析（`internal/httpapi/admin_org_feishu_companies.go`）

```go
// feishuCompanyRow 是"一家可以被同步的公司"的统一视图：身份应用 / 配置文件 / 控制台登记。
type feishuCompanyRow struct {
    feishu.Company                 // AppID/Name/RootName/Identity/Client（Client 可能为 nil）
    ID      int64                  // 控制台登记的库行 id（身份应用与纯配置公司为 0）
    Source  string                 // "identity" | "config" | "console"
    Enabled bool
    Note    string
    SecretConfigured bool
    ClientErr string               // 密钥解不开（credentials_key 轮换过）时的可读原因
    Warnings []string              // duplicate_name / shadowed_by_config ...
}

func (s *Server) feishuCompanyRows(ctx) ([]feishuCompanyRow, error)  // 合并：身份应用 → 配置 → 控制台
func (s *Server) feishuCompanies(ctx) []feishu.Company                // 只含 enabled 的、可解析的行（M92 的调用方不变）
```

规则：以 **app_id** 为合并键、**配置优先**（同 app_id 的库行成为冗余行并在页面标注）；
不同 app_id 的名字冲突时配置方赢得"按名解析"，库行标 `duplicate_name` 警告；
身份应用恒为第 0 行且不可编辑/删除/停用。

## 6. 接口（`admin_routes.go` 同一张表 ⇒ 自动成为 MCP 工具）

| Method | Path | 工具名 | 角色 | 说明 |
|---|---|---|---|---|
| GET | `/org/feishu/companies` | `admin_list_feishu_companies` | viewer | **扩展**：每行加 `id`/`source`/`enabled`/`note`/`secret_configured`/`client_error`/`warnings`；`counts` 沿用 `company_nodes`/`linked_accounts` |
| POST | `/org/feishu/companies` | `admin_create_feishu_company` | admin（危险） | `{name, app_id, app_secret, root_node?, note?, enabled?}`；app_id 与配置/库内其它公司重复 → 409；名字重复 → 409 |
| PATCH | `/org/feishu/companies/{id}` | `admin_update_feishu_company` | admin（危险） | `{name?, root_node?, note?, enabled?, app_secret?}`；`app_id` 不在字段里（不可改）；`app_secret` 省略 = 不动，给了就替换 |
| DELETE | `/org/feishu/companies/{id}` | `admin_delete_feishu_company` | admin（危险） | 只删登记；响应带 `company_nodes`/`linked_accounts` 与清理指引 |
| POST | `/org/feishu/companies/probe` | `admin_probe_feishu_company` | admin | `{id}` 或 `{app_id, app_secret}`；200 + `{ok, stage, message, departments_seen, names_available, samples[]}` |

字段描述（要点）：
- `app_secret`：`"该公司的 App Secret。加密落库（AES-256-GCM），永不回显、不进日志与审计；PATCH 省略则保持不变"`；
- `probe` 的 `app_secret`：`"只用于这次探测，不落库、不回显"`；
- `confirm_reason`：create/update 写"会用它去读这家公司的通讯录（组织架构导入的凭据）"；
  delete 写"只删登记，已导入的节点与人员映射保留；重新登记同一 app_id 会重新认领它们"。

`POST /org/feishu/companies/probe` 的判定（三种结果都 200，网络/超时仍是 502 `feishuUpstreamError`）：

| 结果 | 判据 | stage | message |
|---|---|---|---|
| 凭据被拒 | `KindCredentials` / `KindAppUnavailable` | `credentials` / `permission` | 「App ID/Secret 被飞书拒绝」/「权限或可用范围不对（需要两个只读权限并发布版本）」 |
| 能读但没名字 | `names_available=false` | `names` | 「能读到部门，但缺『获取部门基础信息/获取用户基本信息』：加上并发布版本」 |
| 正常 | 有名字 | `ok` | 「可读：读到 N 个部门（示例 3 个）」+ `samples` |

## 7. 控制台（新页面 `#/companies`）

- 路由与侧边栏：`app.js` 的 router 增 `#/companies`，放在「访问控制」组，紧跟「组织架构」；
- 页面形状（`pages/companies.js`，新建）：标题 + 「新建公司」按钮；表格列
  **公司**（名字 + 来源徽标：身份应用 / 配置 / 控制台）/ **App ID** / **状态**（启用/停用）/
  **公司节点**（名字或"未建"）/ **节点数** / **已映射账号** / **操作**；
- 操作：`编辑`（名称/根节点/备注/启用/替换密钥）、`测试连接`、`删除`（危险确认，写明计数与"数据保留"）、
  `同步`（打开同步弹窗并预选这家公司）；
- 新建/编辑对话框：名称、App ID（编辑时只读）、App Secret（`type=password`，永不回显；编辑时留空 = 不动）、
  根节点名、备注、启用；对话框里有**「测试连接」**按钮（未保存也能测：把当前输入发到 probe）；
- 只读角色：写按钮 disabled（与其它页面同一约定）；
- 同步弹窗的下拉加一行「管理公司…」入口，跳 `#/companies`（关闭弹窗）；
- 静态测试 `internal/webui/tests/companies_test.mjs`：新建请求体、编辑省略密钥时不发 `app_secret`、
  删除确认文案含"数据保留"、viewer 下写按钮 disabled、probe 请求体不带已存密钥（只传 id）；
- harness 新视图 `companies` / `companies-readonly`（`scripts/ui-harness/companies.page.html` + `fixtures.json` 快照）。

## 8. 异常与边界

| 情形 | 行为 |
|---|---|
| `credentials_key` 未配置 | POST/PATCH 带 `app_secret` → 400，消息指向"配 `credentials_key` 或用 `feishu.companies`"；不带密钥的改名/停用仍允许 |
| 密钥解密失败（`credentials_key` 轮换过/行被手改） | 该行 `client_error` 标出，列表与同步下拉都提示"请重新填写密钥"；同步这家公司 → 400 |
| app_id 与配置里的公司或库内其它公司重复 | 409，消息点名是哪一处 |
| 名字与其它公司重复 | 409；若**后来**配置里新增了同名公司，则库行标 `duplicate_name` 警告且"按名解析"归配置方（app_id 仍可用） |
| 同 app_id 同时出现在配置与库里 | 合并为一行（配置优先），库行标注"被配置覆盖"；不报错、不重复出现在下拉 |
| 编辑身份应用 / 删除身份应用 / 停用身份应用 | 400，指向 `feishu.app_id`（身份应用属于部署身份） |
| 编辑或删除配置里的公司 | 400，提示"改配置，或在控制台新建一家（并删掉配置里那家）" |
| 删除时公司还有节点/映射 | 允许（只删登记），响应与确认框给出计数与两条清理路径（`…/links`、删节点） |
| 停用后仍被脚本按 app_id 同步 | 400「该公司已停用」 |
| probe：网络不通/超时 | 502 + `feishuUpstreamError` 的可读原因（与目录读取一致的错误面） |
| probe：系统层 panic 兜底 | 不新增；沿用全局 recover（与其它管理路由一致） |
| 只读角色 | 列表 viewer 可读（含 `secret_configured` 布尔值，**不含**任何密钥材料）；写与 probe 403 |
| 并发编辑同一公司 | 沿用"读—改—写 + 唯一索引兜底"（单写者 SQLite） |

## 9. 测试策略

| 层 | 测试 |
|---|---|
| `internal/creds` | `EncryptScoped/DecryptScoped` 往返；**跨作用域不可解**（`feishu_app` 密文用 provider 的 AAD 解失败，反之亦然）；`CompanySealer` 未就绪时 `Seal` 报错 |
| `internal/store` | `feishu_apps` CRUD、`(app_id)`/`(name)` 唯一 409、`secret_enc` 落库后**明文不在库里**（`SELECT` 原始行断言）、启停、删除只删登记（节点/映射仍在）、`CountFeishuAppData` 计数 |
| `internal/httpapi` | 新建 → 列表 → 同步这家公司（部门建在它自己的公司节点下）→ 第二次同步 0 写入；改名/换密钥/停用/删除的守卫（身份应用 → 400、配置来源 → 400、app_id 改不动 → 400/不识别该字段）；重名与重复 app_id → 409；`credentials_key` 空 → 400；probe 三态（凭据被拒 / 缺权限 / 正常）用 stub 目录；合并规则（同 app_id 配置优先、同名警告）；viewer 403 且响应无密钥材料；审计 changes 不含密钥 |
| `internal/mcpsrv` + 守卫 | 新路由的表覆盖/字段形状与示例由既有守卫测试自动覆盖（`admin_routes_test.go` 计数与 `admin_describe` 形状） |
| 前端静态 | `companies_test.mjs`（见 §7）+ `org_feishu_test.mjs` 的「管理公司…」入口断言 |
| harness | 视图 `companies` / `companies-readonly`：列表渲染、来源徽标、停用行、新建对话框的测试连接按钮、viewer 只读 |
| 性能 | 不碰请求路径；`BenchmarkPlan` 与 `BenchmarkResolveTagRecords` 与 M92 逐项相同（作为回归证据） |

## 10. 依赖

- 无新外部依赖；复用 `internal/creds`（AES-256-GCM）与 `internal/feishu`（目录读）。
- 新增文件：`internal/store/migrations/0031_feishu_apps.sql`、`internal/store/feishu_apps.go`、
  `internal/domain/feishu_app.go`、`internal/creds`（作用域 AAD + `CompanySealer`）、
  `internal/webui/static/js/pages/companies.js`、`scripts/ui-harness/companies.page.html`。
- 分层白名单：`internal/creds` 只依赖标准库；`internal/httpapi` 已有 `internal/feishu`/`internal/creds`
  的依赖边（后者经端口），不新增边。

## 11. 实现与设计差异

实现与设计一致（数据模型、合并键与优先级、`app_id` 不可改、只删登记、停用语义、probe 的三态、
4 条路由与控制台新页面都按上面的决策落地）。差异与补充：

| # | 设计 | 实现 | 原因 |
|---|---|---|---|
| 1 | 创建公司 = 一次写 | **两步**：先插登记行（拿到 id），再用该 id 封口密钥并回写；封口失败则删掉刚建的行 | 作用域 AAD 绑行 id，行必须先存在。留一个"有登记但没密钥"的行会让下一次列表多出一条坏行，所以失败即回滚那行 |
| 2 | `credentials_key` 未配置 → 拒绝带密钥的写 | 一致；不带密钥的操作（改名/换根节点/备注/停用）仍然允许 | 停用/改名不需要密钥，拒绝它们没有理由 |
| 3 | 合并规则：app_id 为键、配置优先 | 一致；另外**同一 app_id 的库行不会从列表里消失**，而是把配置那一行标上 `shadowed_by_config` 与警告（含库行 id），并且**禁止编辑**被覆盖的库行（409）、允许**删除**它 | 库行如果完全不可见，操作员就永远清不掉它；而"编辑了却不生效"比"拒绝并说明"更糟 |
| 4 | 名字冲突时配置方赢得按名解析 | 一致；创建/改名时直接 409（不能造出冲突），只有"先控制台登记、后写进配置"这种顺序会留下冲突，此时库行带 `duplicate_name` 警告 | 唯一索引 + 合并键让创建期就能拦住最常见的错误 |
| 5 | probe 判定 4 态 | 一致；另外 `GET /companies` 多了 `secrets_ready`（控制台据此禁用「新建公司」并说明原因），probe 在密钥解不开时也给出 `stage:"credentials"` 的可读原因 | 界面要能解释"为什么按钮是灰的"，而不是让操作员点一次才知道 |
| 6 | 控制台对话框里有「测试连接」 | `ui.modal` 新增可选 `extraActions`（收到与提交相同的字段值、不关窗、失败显示在对话框的错误行），公司对话框是它的第一个使用者 | 原设计假定 modal 支持额外按钮，实际没有；给共用组件加一个加法式的选项比在页面里手搓一个对话框更小更稳 |
| 7 | 公司页每行「同步」打开弹窗并预选 | 实现为**深链接** `#/org?company=<app_id>`：组织架构页读 `route.params` 并把公司传给弹窗；弹窗加载公司列表后若预选值不在（或已停用）则回落到身份应用 | 弹窗由组织页拥有，深链接让"从公司页发起同步"不需要跨页状态；回退规则避免选到一家已停用的公司 |
| 8 | 停用公司"不出现在同步下拉" | 一致（下拉只列 `enabled` 的公司）；另外**弹窗的公司下拉只有 ≥2 家时渲染**（M92 的既有行为），此时旁边多一个「管理公司…」入口 | 单公司部署的请求形状与 M70/M92 逐字相同，不能因为多了注册表就改变它 |
| 9 | 审计只写 `secret_changed` | 一致；测试直接查 `audit` 表断言明文密钥不在任何 `feishu_company` 条目里 | 这是本里程碑最容易悄悄写错的一处 |
| 10 | 分层：`internal/creds` 只依赖标准库 | 一致；`internal/httpapi` 通过两个新端口（`FeishuCompanyAdmin`/`FeishuCompanySecrets`）使用 store 与封口器，没有新的包依赖边 | `internal/arch` 的分层白名单未变 |

**性能**：`BenchmarkPlan` 4948 B/45 allocs、`BenchmarkResolveTagRecords` 0 B/0 allocs 与 288 B/9 allocs
与 M92 逐项相同（本里程碑只动管理面）。

**新增测试**：`internal/creds/company_test.go`（作用域往返 + 跨作用域/跨表不可解 + 空密钥拒绝）、
`internal/store/feishu_apps_test.go`（CRUD、唯一冲突、密文落库、只删登记、计数）、
`internal/httpapi/admin_org_feishu_company_admin_test.go`（注册→同步→停用→删除全流程、守卫、
shadowed/duplicate 两条合并分支、probe 四态、密钥不可解密、映射清理仍可用）、
`internal/webui/tests/companies_test.mjs`（页面发出的请求形状与只读角色）、
harness 视图 `companies` / `companies-readonly`（**本沙箱没有 firefox，`make ui-check` 跑不了**，
已记入 `docs/TODO.md` 的宿主待办）。
