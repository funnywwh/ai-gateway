# M95 设计文档：公司页的「编辑」——配置来源的公司也能改字段

> 状态：**已实现（M95）**。
> 前序：[M92 多公司组织架构导入](m92-multi-company-feishu-org-sync.md)、[M93 控制台管理公司](m93-console-managed-feishu-companies.md)、
> [M94 可改公司名](m94-editable-company-names.md)。
> 面向使用者的规格：[docs/org.md](../org.md) §5、[docs/feishu.md](../feishu.md) §5c.6、[docs/mcp.md](../mcp.md) §4。
>
> 需求原话（2026-09-28）：「"改名"应该改成"编辑"，编辑可编辑字段」。

## 1. 问题

M94 给身份应用与 `feishu.companies` 登记的公司开了「改名」这一个按钮：名字可改，其余字段（根节点名、备注、
启用、密钥）在对话框里是只读的。运营看到的是**一个半残的编辑框**——按钮叫「改名」，五个字段里四个灰着。

## 2. 目标

1. 按钮统一叫 **「编辑」**（与控制台登记的公司一致）；
2. 配置来源的公司，编辑框里 **公司名 / 公司根节点名 / 备注 / 启用** 四项都能改，改完立即生效（同步、
   公司节点名、同步下拉、可见性都跟着变），不必改配置重启；
3. 一键 **「恢复配置值」**：把这家公司上所有控制台覆盖清掉，回到配置文件里的样子；
4. 仍然只有**一处**真源：配置文件；控制台写的是**覆盖**，页面明确标出哪些字段被覆盖过。

### 非目标（本次）

- **身份应用的密钥仍不可改**（见 D3）：本公司的 `feishu.app_secret` 同时服务于登录/绑定/扫码，覆盖它
  会造成"通讯录读得通、扫码登录失败"的不一致；它的其余四项字段（名/根节点/备注/启用）照常可改。
- 不做字段级的历史/回放（审计记一条 `update`，changes 里写改了哪些字段）。
- 不改控制台登记的公司（`feishu_apps`）的行为——它们的字段本来就都可编辑。

## 3. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | **一个覆盖机制覆盖五个字段**：M94 的 `feishu_company_names` 重建为 `feishu_company_overrides`（每字段可空 = "未覆盖"；密钥是 `secret_enc` BLOB，NULL = 不覆盖），而不是每字段一张表或"编辑即升级成控制台登记" | 一张表一个语义（"这家公司在控制台上被改过什么"）。**否决**"编辑即升级"（把配置里的公司偷偷变成库行，配置与库两份真源，还得处理密钥拷贝与登录应用的例外）；**否决**每字段一张表 |
| D2 | 覆盖的优先级：**覆盖 > 配置**（对这四个字段）；密钥仍是配置 > 无 | 覆盖就是"运营在控制台上做的决定"，比文件新。`root_node` 也适用：在手改过根节点名之后，配置文件里的 `root_node` 不再压过它（M94 的"显式 root_node 优先"只对**名字覆盖**成立，本次收回，见差异节） |
| D3 | **密钥覆盖分两种情况**（2026-09-28 按用户决定）：「用户说：编辑可编辑字段」→ 评审时明确"密钥也要能改"：<br>① `feishu.companies` 登记的公司 —— **可覆盖**，覆盖值加密落库（`secret_enc`）、留空 = 用配置里的、`reset` 可清掉；<br>② **身份应用（本公司）—— 不可覆盖**，字段只读并写明"密钥同时用于登录流程" | 身份应用的密钥同时用于**登录流程**（`feishu.New(cfg.Feishu)` 在启动时构造，OAuth/绑定/扫码都用它）：让目录读取用库里的新值会造成"通讯录读得通、扫码登录失败"这种最难查的不一致。<br>对客户公司，覆盖的代价是**客户端要按请求重建**（丢掉启动时那份 tenant token 缓存）——但只在"这家公司有密钥覆盖"时发生，且目录本身有 60 秒缓存，代价是每家公司每分钟至多一次铸 token，可接受；换来的是运营不必为了换一把密钥去改配置重启。<br>**并存两份真源的坑**：控制台覆盖之后配置文件里的那把会**静默失效**，所以列表必须标出 `secret_overridden`，页面徽标写清"已在控制台编辑"，并提供「恢复配置值」回到配置 |
| D4 | 「恢复配置值」= 删掉这家公司的覆盖行（`PATCH … {"reset": true}`），并**同时**把公司节点名改回配置里的名字（走 M94 那条跟随逻辑：只动"还在用覆盖名"的节点） | 与 M94 的"清空名字 = 回到配置名"是同一件事，扩展成"清空全部覆盖" |
| D5 | 列表每行给 `overridden: ["name","root_node","note","enabled"]`（以及沿用 M94 的 `name_source`），页面用它画「已在控制台编辑」徽标与「恢复配置值」按钮（只有覆盖非空时才出现） | 运营必须能一眼看出"这个值不是配置文件里的"，否则下次改配置文件时会困惑 |
| D6 | `enabled` 的覆盖是**纯加法**：配置文件里没有这个字段（配置来源的公司默认启用），覆盖 `false` = 暂停同步；覆盖 `true` 与不覆盖等价 | 让"停用一家客户公司"不需要改配置；又不需要在配置文件里加一个只为覆盖存在的字段 |
| D7 | 备注（note）也进覆盖 | 它本来就是本地展示字段（库行有自己的 note），配置来源的行没有 note 可写，覆盖是唯一的落点 |
| D8 | 不做"把配置里的公司搬进控制台"，也不做密钥覆盖 | 沿用 M93/M94 的取舍；真要全字段管理就在控制台新建一家（那是既有能力） |

## 4. 数据模型

### 4.1 migration `internal/store/migrations/0033_feishu_company_overrides.sql`

```sql
-- M94 的 feishu_company_names 只有一列名字，而且 name NOT NULL（"存在即改了名"）；
-- M95 要表达"某几个字段被覆盖"，于是重建为 feishu_company_overrides：
-- 每列 NULL = 该字段未被覆盖（用配置里的值），非 NULL = 覆盖值
-- （空字符串也是合法覆盖值：root_node 覆盖成空 = "用公司名当根节点名"）。
-- 旧行（M94 改过的名字）原样搬过去，不丢。
CREATE TABLE feishu_company_overrides (
    app_id     TEXT PRIMARY KEY,
    name       TEXT,
    root_node  TEXT,
    note       TEXT,
    enabled    INTEGER,
    -- 客户公司的密钥覆盖（AES-256-GCM，AAD 用 app_id 而不是行 id，见 internal/creds 的 ScopedKey）。
    secret_enc BLOB,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO feishu_company_overrides(app_id, name, updated_by, updated_at)
    SELECT app_id, name, updated_by, updated_at FROM feishu_company_names;
DROP TABLE feishu_company_names;
```

（重建而不是加列：旧表的 `name NOT NULL` 让"只改了备注"这种行写不进去；表名也要如实说它装的是"覆盖"，
留着 `…names` 会误导后来人。SQLite 的 DDL 在迁移事务里跑，失败会整体回滚。）

### 4.2 domain 与 store

```go
// internal/domain/feishu_app.go
// FeishuCompanyOverride 是控制台对"名字来自配置"的公司的字段覆盖（M95）。nil 字段 = 未覆盖。
type FeishuCompanyOverride struct {
    AppID    string
    Name     *string
    RootNode *string
    Note     *string
    Enabled  *bool
    // SecretEnc 非空 = 这家客户公司的密钥被控制台覆盖过（身份应用永远不会写进来）。
    SecretEnc []byte
    UpdatedBy string
    UpdatedAt time.Time
}

// internal/creds：按 app_id 加封（身份应用的密钥用行 id 加封，两套 AAD 互不可解）
func EncryptScopedKey(key []byte, scope, handle string, plaintext []byte) ([]byte, error)
func DecryptScopedKey(key []byte, scope, handle string, ciphertext []byte) ([]byte, error)
func (s *CompanySealer) SealByApp(appID, secret string) ([]byte, error)
func (s *CompanySealer) OpenByApp(appID string, ciphertext []byte) (string, error)

// internal/store/feishu_apps.go（替换 M94 的三个方法）
ListFeishuCompanyOverrides(ctx) (map[string]domain.FeishuCompanyOverride, error)
SetFeishuCompanyOverride(ctx, domain.FeishuCompanyOverride) error   // upsert，全空则删行
DeleteFeishuCompanyOverride(ctx, appID string) (bool, error)       // 「恢复配置值」
```

端口 `FeishuCompanyAdmin` 用这三个方法替掉 M94 的 `List/Set/DeleteFeishuCompanyName`。

### 4.3 合并（`feishuCompanyRows`）

```
配置来源的行：Name / RootName / Note / Enabled 依次取
    覆盖（非 nil）  > 配置文件里的值（RootName 再按 root_node > name 解析）
Identity 行同规则（它的 Enabled 覆盖同样生效：停用本公司 = 不参与同步，数据保留）
密钥：有 secret_enc 覆盖的客户公司 → 用它**按请求构造客户端**（拿到 ClientErr 就走 400/列表标错）；
      其余沿用启动时那份客户端（带 tenant token 缓存）
控制台行：不变（自己的库行就是真源），Overridden 为空
```

`RootName` 的解析顺序（M95 后）：**覆盖的 root_node（非 nil）> 配置的 root_node（非空）> 有效名字**。

## 5. 接口（把 M94 的"只接受 name"放宽）

| Method | Path | 变化 |
|---|---|---|
| PATCH | `/admin/api/v1/org/feishu/companies/{app_id}` | 接受 `name` / `root_node` / `note` / `enabled` / **`app_secret`**（都可选；**省略 = 不动**；`app_secret: ""` = 清除密钥覆盖，回到配置里的那把），新增 `{"reset": true}` 清除全部覆盖。**身份应用给 `app_secret` → 400**（密钥用于登录流程）；`app_id` 永远不可改 |
| GET | `/admin/api/v1/org/feishu/companies` | 每行加 `overridden: [...]`（含 `"secret"`；**永不含密钥材料**）、`overridden_by`/`overridden_at`、`secret_overridden` 布尔 |

- 覆盖值的校验与配置来源同一套：名字 `config.NormalizeCompanyName`、根节点名 `domain.NormalizeOrgNodeName`、
  备注 ≤ 512 字符（与库行同一上限）、enabled 布尔、密钥非空且无空白（与 `feishu.companies[].app_secret` 同级要求）；
- 密钥覆盖需要 `credentials_key`（未配置 → 400 并指向配置方式），与 M93 的库行同一约定；
- 覆盖 `root_node` 后，**下一次同步**会用这个名字找/建公司节点（`planCompanyRoot` 自然生效）；
  若改名会撞上根层已有节点，沿用 M94 的 `root_name_taken` 警告与两条出路；
- `reset` 时把公司节点名改回配置名（M94 的跟随逻辑，只动还在用覆盖名的节点）。

## 6. 控制台

- 按钮统一 **「编辑」**（身份应用/配置公司/控制台行都一样）；
- 配置来源的对话框：**公司名 / 公司根节点名 / 备注 / 启用** 全部可编辑；**App Secret** 对**客户公司**同样
  可编辑（留空 = 用配置里的，填了就覆盖；已覆盖时提示「已覆盖配置里的密钥」），对**身份应用（本公司）**只读
  并写明「密钥同时用于登录流程（feishu.app_secret）」；
- 覆盖非空时：行上徽标 **「已在控制台编辑」**（含字段列表），对话框底部多一个 **「恢复配置值」**；
- 「先测试连接」按钮只在有待填密钥时出现（配置来源的公司没有，保持不变）。

## 7. 边界

| 情形 | 行为 |
|---|---|
| 只改备注 | 允许（覆盖行 name 为空 = 不改名） |
| 清空根节点名（覆盖成空串） | 允许：表示"用公司名当根节点名"，忽略配置里的 root_node |
| 恢复配置值后该名字与其它公司重名 | 409（沿用重名检查），覆盖保持原样 |
| 把 enabled 覆盖成 true 而配置文件里没有 enabled | 等价于不覆盖（配置来源默认启用） |
| 停用身份应用（本公司） | 允许：同步下拉与按名解析都拒绝，节点与映射保留；不影响登录流程 |
| 给身份应用传 `app_secret` | 400，消息说明"密钥同时用于登录流程，请改 feishu.app_secret" |
| 给客户公司传 `app_secret` 但部署没配 `credentials_key` | 400，指向"配 credentials_key 或继续用配置里的密钥" |
| 密钥覆盖后配置里的密钥被改 | 控制台覆盖优先，配置里的那把静默不生效——列表的 `secret_overridden` 与页面徽标就是为此存在；「恢复配置值」回到配置 |
| viewer | 403 |

## 8. 测试策略

| 层 | 测试 |
|---|---|
| `internal/creds` | `EncryptScopedKey` 往返；与按行 id 的密钥**互不可解**（同一 app_id 的两种封法、provider 的旧 AAD 都解不开） |
| `internal/store` | 覆盖行 upsert（增量字段）/ 全空删行 / delete 幂等 / 迁移后 M94 的旧行仍在（名字覆盖没丢）/ 密文原样落库 |  |
| `internal/httpapi` | 改名以外的三字段：改 `root_node` → 下一次同步用新名字建公司节点；改 `note`/`enabled` → 列表与解析生效（停用后同步 400）；`reset` → 回到配置值（含公司节点名跟随回去、密钥覆盖一并清掉）；省略字段 = 不动；**密钥覆盖真的被用上**（stub 校验凭据：配置里的密钥被拒 → 覆盖后目录读成功 → reset 后再次被拒）；身份应用给密钥 400；`app_id` 仍 400；重名 409；viewer 403 |
| 前端静态 | `companies_test.mjs`：按钮文案「编辑」、配置行的四个字段可编辑 + 密钥只读、覆盖徽标、「恢复配置值」只在有覆盖时出现、提交体只带这四个字段 |
| harness | `companies` 视图更新按钮名与新字段断言（宿主走查） |
| 回归 | M92/M93/M94 的组织同步与公司管理测试全绿；性能基准不变 |

## 9. 依赖

无新外部依赖；新增 `0033_feishu_company_overrides.sql`，其余为既有文件扩展。

## 10. 实现与设计差异

实现与设计一致（覆盖表重建、覆盖 > 配置、四字段 + 客户公司密钥、`reset`、身份应用密钥只读、控制台统一「编辑」）。
差异与补充：

| # | 设计 | 实现 | 原因 |
|---|---|---|---|
| 1 | 空请求（一个字段都不给） | 400 `nothing to update`（与库行路径同一句话） | 与 M93 的既有行为一致；MCP/脚本得到的是可读的错误而不是"成功但什么也没发生" |
| 2 | 覆盖行的写入是**整行替换** | 一致：先读出已存覆盖，把本次请求带的字段合进去，再整行写 | "省略 = 不动"必须靠它实现；也让"只改备注"不会把名字覆盖冲掉 |
| 3 | 密钥覆盖的客户端 | 放在**合并阶段**（`feishuClientForOverride`）按请求构造，失败原因写进 `client_error`；只有存在密钥覆盖的客户公司才会走这条路 | 其余公司沿用启动时那份客户端（保住 tenant token 缓存）；有覆盖的公司每分钟至多一次铸 token（目录缓存 60 秒），代价可控 |
| 4 | App Secret 的 AAD | 新增 `EncryptScopedKey/DecryptScopedKey`（AAD = scope + app_id）与 `CompanySealer.SealByApp/OpenByApp`（scope `feishu_app_override`），与按行 id 的 `feishu_app` 作用域**互不可解** | 行 id 与 app id 是两种句柄，混用会让一条密文在另一种语境下被解开 |
| 5 | 身份应用的停用 | 与其它公司同一套守卫（`usableCompany`）：停用后按 app_id/名字解析 **和不带 company 的同步**都 400；节点与映射保留，登录流程不受影响 | 实现时发现旧代码的"省略 company = 本公司"那条快捷路径**跳过了停用/凭据检查**（M94 的覆盖语义让它第一次可达），已一并修掉 |
| 6 | 测试 | 除设计的矩阵外，加了 `TestFeishuClientSecretOverrideIsUsed`：stub 现在**校验凭据**（Feishu 的 99991663），所以"覆盖的密钥真的被用上"是对着线上请求形状断言的，而不是看一个布尔字段 | 密钥覆盖最容易写成"存了但没用"；这条测试是它的反向证据 |
| 7 | 静态/harness | `companies_test.mjs` 与 `companies` 视图的断言改成 M95 口径（四字段可编辑、客户公司密钥可编辑、本公司密钥只读、覆盖徽标、恢复配置值） | — |

**性能**：`BenchmarkPlan` 4948 B/45 allocs 不变（只动管理面）。

**已知的、与本次无关的抖动**：全量测试跑第一遍时 `internal/dshgw/nodeaudit` 的 `TestRunStopsWithContext`
报过一次 `Run never drained`，单独重跑 5 次全绿、全量重跑也全绿——是既有测试的时序抖动，未改该包。
