# M70 设计文档：组织架构页「同步飞书」（飞书通讯录 → 组织树与账户）

> 状态：**规格（M70 实现中）**。
> 面向使用者的规格：[docs/org.md](../org.md) §5、[docs/feishu.md](../feishu.md) §「飞书通讯录同步」。
> 前序：M49 组织架构（`docs/design/m49-organization.md`）、M60 API Key 飞书绑定、M66 控制台管理员扫码登录。
>
> 需求原话：「右上角添加一个"同步飞书"，弹出组织机构树和人员，人员可以有"创建用户""绑定账号"操作，
> 合并时同名合并，人员id相同合并」→「绑定账号时，弹出账号列表，可以拼音过滤」→「自动按人名匹配，不匹配的用户决定」。

## 1. 目标

1. 控制台组织架构页右上角新增「同步飞书」按钮，弹出**飞书组织结构树 + 人员列表**（数据来自飞书通讯录 API）。
2. 一次「同步」自动完成：补建缺失的部门节点（同名 / 部门 id 合并）+ 把**已匹配**人员合并到本地账户
   （写账户级飞书身份 + 挂到对应部门节点）。
3. 匹配不上的人员由操作员逐行决定：「创建用户」（新建同名账户）或「绑定账号」（弹账号列表，支持拼音过滤）。
4. 同步幂等：重复执行不产生新写入；已有数据（控制台手工建的节点/账户、M60 的 Key 级绑定）不被破坏。

### 非目标

- 批量创建账户（自动合并只覆盖已匹配人员；未匹配人员逐行处理）。
- 模糊/别名匹配（`周八(ba)` 不自动等于飞书「周八」，只给疑似提示）。
- 账户级飞书身份参与 DSH 门户登录（登录仍是 M60 的 Key 级绑定）。
- 定时自动同步、事件订阅、飞书侧删除的传播（飞书删部门/人员不影响本地）。
- 迁移既有 Key 级绑定数据（同步时自动固化属于正常写入路径）。

## 2. 实测事实（2026-09-21，作为设计依据）

| 事实 | 证据 |
|---|---|
| 部署在 `192.168.190.86:8088` 的就是本仓库进程（v2.7.3，`systemctl --user` 单元 `aigw-local.service`） | `/version` + `ps` |
| 应用 `cli_aa27b25392f91bdb` 可取 `tenant_access_token`，`contact/v3/departments`、`contact/v3/users` 均 code 0 | 直接调用实测 |
| 但部门/人员**不含 `name`**，用户**不含 `department_ids`**（缺「获取部门基础信息」「获取用户基本信息」） | 响应体实测 → 只能按部门枚举成员 |
| 飞书 `open_id` 与 `api_keys.feishu_open_id` 是同一套 id | 5 个已绑定 open_id 全部命中通讯录 |
| 本地：28 个账户（20 个中文人名），`org_nodes` 0 行 | 只读查库 |
| 根部门 `"0"` 的直接成员为 0；全目录 22 个部门 / 87 人 | 遍历实测 |

## 3. 关键决策

### D1 「创建用户」= 新建本地账户

组织树管的实体就是账户（M49），控制台没有独立的「用户」实体。创建时账户名默认飞书姓名（可改）、
写入飞书身份、并挂到该人所在部门对应的本地节点。

### D2 「绑定账号」= 把飞书身份写进本地账户

`accounts` 新增 5 列（与 `api_keys` 的 M60 五列同形）。它是**同步映射**，不是登录能力：数据面鉴权与
门户登录完全不读它。与 `api_keys.feishu_open_id` 是两张表上的两套唯一索引，互不影响。

### D3 人员匹配的三条自动通道（同步时落库）

优先级从高到低：

1. **按账户上的 id**：`accounts.feishu_open_id == open_id`（本功能或后续同步写入过的）；
2. **按 API Key 身份**（M60 遗产）：某个 Key 的 `feishu_open_id == open_id` → 命中该 Key 的账户，
   同步时把身份**固化到账户**（写 5 列，`matched_by="api_key"`）；
3. **按人名**：账户**尚未绑定**任何身份 **且** `accounts.name` 与飞书姓名（trim 后）完全相等 → 写入身份
   （`matched_by="name"`）。

都不命中 → 未匹配，由操作员「创建用户 / 绑定账号」。三条通道都**绝不覆盖**已绑定给他人的身份：
飞书侧两个不同 open_id 同名时先到先得，其余进 `skipped_users`（同名账户已被占用）。

### D4 部门匹配与补建

1. `org_nodes.feishu_department_id == open_department_id` → 已同步（改名也不丢）；
2. 否则**同一父节点下**的同名节点 → 视为同一节点并打上部门 id（`matched_by="name"`）；
3. 否则按需创建。遍历是 BFS（父先于子），所以父节点一定先存在；深度会超过 `orgtree.MaxDepth`(16) 的
   部门连同其子孙跳过并报告。
   节点已绑**另一个**部门 id 时同名规则不生效（不覆盖既有 id），只报 warning。

### D5 一次写、一次确认

主按钮「同步」= 补建缺失部门 + 自动合并已匹配人员（写身份 + 挂部门节点），一个动作、执行前一个确认框
（列出将建 N 个节点、自动合并 M 个账户、挂节点意味着继承这些节点的标签授权）。未匹配人员保持逐行手动，
不做批量创建。

### D6 服务端目录缓存 60 秒

一次目录遍历 ≈ 部门数 + 1 次列表调用（本机 24 次、1–2 秒）。成功快照在内存缓存 60 秒，`?refresh=true`
与任何写操作使其失效；失败不缓存。

### D7 同名是精确匹配

`domain.NormalizeAccountName` 归一后逐字节相等。带后缀的本地名（`周八(ba)`、`陈一-ios`）不自动合并；
前端用现有拼音表给出「疑似」提示，由人点「绑定账号」。

## 4. 数据模型

迁移 `internal/store/migrations/0024_feishu_directory_links.sql`：

```sql
ALTER TABLE org_nodes ADD COLUMN feishu_department_id TEXT NOT NULL DEFAULT '';
ALTER TABLE org_nodes ADD COLUMN feishu_synced_at INTEGER;
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_nodes_feishu_dept
    ON org_nodes(NULLIF(feishu_department_id, ''));

ALTER TABLE accounts ADD COLUMN feishu_open_id  TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN feishu_union_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN feishu_name     TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN feishu_bound_at INTEGER;
ALTER TABLE accounts ADD COLUMN feishu_bound_by TEXT    NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_feishu_open_id
    ON accounts(NULLIF(feishu_open_id, ''));
```

- domain：`OrgNode.FeishuDepartmentID string`、`OrgNode.FeishuSyncedAt *time.Time`；
  `Account.FeishuOpenID/FeishuUnionID/FeishuName string`、`Account.FeishuBoundAt *time.Time`、
  `Account.FeishuBoundBy string`；`domain.KeyFeishuIdentity{KeyID, AccountID int64; Binding FeishuBinding}`。
- store 读路径：`accountCols` / `orgNodeCols` 与对应 scan 函数增列（全仓各 3 处 SELECT）。
- store 写路径：`UpsertAccount` 与 `UpdateOrgNode` 的 UPDATE **不写**这些列 —— 控制台的普通编辑
  永远清不掉同步关系（与「余额不被 upsert 覆盖」同一口径）；`CreateOrgNode` 的 INSERT 带上两列，
  便于补建时一次落库。
- 新 store 方法（端口同增）：
  - `BindAccountFeishu(ctx, accountID, domain.FeishuBinding) error` / `UnbindAccountFeishu(ctx, accountID) (bool, error)`
    / `FindAccountByFeishuOpenID(ctx, openID) (*domain.Account, error)` —— 语义照抄 `BindAPIKeyFeishu`：
    trim、空 open_id → 400、唯一冲突 → 409、重新绑定覆盖旧值；
  - `ListAPIKeyFeishuIdentities(ctx) ([]domain.KeyFeishuIdentity, error)`（`feishu_open_id <> ''`）；
  - `SetOrgNodeFeishuDepartment(ctx, nodeID, deptID string) error`（空串 = 解绑；冲突 → 409）；
  - `AddAccountOrgNodes(ctx, accountID int64, nodeIDs []int64) error`（`INSERT OR IGNORE`，加性；
    账户/节点不存在 → 404）。

## 5. 飞书通讯录客户端（`internal/feishu/directory.go`）

```go
type Department struct { ID, ParentID, Name string; Depth int } // ID = open_department_id
type DirectoryUser struct { OpenID, UnionID, Name string; DepartmentIDs []string }
type Directory struct {
    Departments    []Department    // BFS 序：父一定在子之前
    Users          []DirectoryUser // 按 open_id 去重；DepartmentIDs 为出现过的全部部门
    NamesAvailable bool            // false = 调用成功但 name 全空（缺数据权限）
    Truncated      bool
    FetchedAt      time.Time
}
type DirectoryOptions struct { PageSize, MaxDepartments, MaxPages int }
func (c *Client) Directory(ctx context.Context, opts DirectoryOptions) (Directory, error)
```

- `tenantAccessToken(ctx)`：POST `TenantTokenURL`（`{app_id, app_secret}`），结果带过期时间缓存
  （提前 60 s 刷新，互斥锁保护）；调用返回 `99991663` 时先失效缓存再重试一次。
- 遍历：从根 `"0"` 起，`GET {ContactURL}/departments?parent_department_id=…&page_size=…&department_id_type=open_department_id`
  翻 `page_token`；每个部门（含根 `"0"`）再 `GET {ContactURL}/users?department_id=…&page_size=…&user_id_type=open_id`。
  默认 `PageSize=50`、`MaxDepartments=500`、`MaxPages=40`，超限置 `Truncated=true` 并停止。
- 名称缺失检测：拿到部门/人员但 `name` 全为空 → `NamesAvailable=false`（这是当前部署的真实状态）。
- 错误分类复用 `*feishu.Error`：token 拒绝（`99991661/99991663/99991668`）→ `KindCredentials`；
  其余非 0 code → `KindAppUnavailable`（message 只放数字码，不回显飞书原文）；HTTP 5xx/超时 →
  `KindUnreachable`；429 → `KindRateLimited`。复用 `do()` 的既有防线（不跟重定向、64 KB 上限、不走代理）。
- 配置：`Feishu.TenantTokenURL`（默认 `https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal`）、
  `Feishu.ContactURL`（默认 `https://open.feishu.cn/open-apis/contact/v3`）；env
  `GW_FEISHU_TENANT_TOKEN_URL` / `GW_FEISHU_CONTACT_URL`；进 `validateFeishu()` 的 https 校验。

## 6. 管理接口（`internal/httpapi/admin_org_feishu.go`，全部 `adminActor(w,r,true)`）

| Method | Path | MCP 工具 | 说明 |
|---|---|---|---|
| GET | `/admin/api/v1/org/feishu/directory` | `admin_list_feishu_directory` | 合并预览（含自动匹配结果），query `refresh=boolean` |
| POST | `/admin/api/v1/org/feishu/sync` | `admin_sync_feishu_org` | 补建缺失部门 + 自动合并已匹配人员 |
| POST | `/admin/api/v1/org/feishu/users/{open_id}/account` | `admin_create_account_from_feishu_user` | 创建用户；同名账户已存在 → 409 |
| PUT | `/admin/api/v1/org/feishu/users/{open_id}/account` | `admin_bind_account_feishu_user` | 绑定账号（body `{"account_id":7}`） |
| DELETE | `/admin/api/v1/org/feishu/users/{open_id}/account` | `admin_unbind_account_feishu_user` | 解绑（幂等） |

- 未启用飞书 → 501 `ErrUnsupported`；飞书调用失败 → `domain.ErrUpstream(502, 中文可读原因)`。
- 预览需要一次 `ListAccounts`（含飞书列）+ `ListOrgNodes` + `ListOrgMemberships` +
  `ListAPIKeyFeishuIdentities`，全部内存合并，不逐人查询。
- 同步写顺序：部门 `CreateOrgNode`（或 `SetOrgNodeFeishuDepartment`）；人 `BindAccountFeishu` →
  `AddAccountOrgNodes`。整体幂等：第二次同步 `created_count=0`、`linked_count=0`。
- 审计：每个新建节点一条 `create/org_node/{id}`（detail 带 `source:"feishu"`、`feishu_department_id`）、
  同步汇总一条 `sync_feishu/org`、自动合并每人一条 `feishu_bind/account`（带 `matched_by`）、
  手工创建/绑定/解绑各自一条。部门/账户写入后 `s.reload(ctx, "feishu org synced", true)` 一次
  （节点标签被子树继承，必须整树失效）；身份绑定不影响鉴权，不 reload。
- 任何写操作清空 D6 缓存。

## 7. 控制台（零构建原生 ES 模块）

- `pages/org.js`：actions 追加「同步飞书」（readonly → disabled，与「新建根节点」同一口径）。
- 新模块 `pages/org_feishu.js`（不占路由，仅被 org.js import）导出 `openFeishuSync({ onDone })`：
  手搓宽弹窗（`modalHead/modalBody/modalActions` + `closeButton`）。
  - 头部：统计 +「刷新」+「同步」（确认框写明将建 N 节点、自动合并 M 人、挂节点=继承标签授权）。
  - 左：`tree.js`（`mode:'workspace'`、拼音过滤），meta 显示 `N 人 · 已存在/未创建`。
  - 右：选中部门的人员列表（「包含子部门」勾选 + 拼音过滤框）；已匹配行显示通道并给「解绑」；
    未匹配行给「创建用户」（小表单）与「绑定账号」（嵌套弹窗：`/accounts?limit=1000` +
    `matchesQuery` 拼音过滤，同名候选置顶预选，已绑他人不可选）。
  - 写后 toast + `?refresh=true` 重绘 + `onDone()`。
- `app.css`：`.feishu-sync-dialog` / `.feishu-sync-layout` / `.feishu-user*` / `.feishu-picker*`，无内联样式。

## 8. 异常与边界

| 情况 | 行为 |
|---|---|
| 飞书两个不同 open_id 同名 | 先到先得；其余 `skipped_users` + warning，不覆盖已绑定身份 |
| 账户已绑定他人但姓名与某人相同 | 同名规则不生效（要求账户未绑定），落入未匹配 |
| 部门深度超 16 | 跳过该部门及其子孙，计入 `skipped_departments`，同步整体不失败 |
| 同名节点已绑另一部门 id | 不覆盖 id，报 `matched_by:"name"` + warning |
| 飞书返回无 name（当前真实状态） | 预览正常 + `names_unavailable` warning；树/人员用 `od-…`/`ou_…` 占位；创建账户表单要求手填名字 |
| token / 权限 / 网络失败 | 502 + 中文原因；日志留数字码 |
| 同步中途失败 | 保留已写入部分，响应报告成功部分与失败原因，重跑幂等 |
| 账号列表 >1000 | 与组织页一致：截断 + 提示 |

## 9. 测试策略

- `internal/feishu/directory_test.go`（httptest 桩）：翻页、BFS 父先于子、跨部门同人合一、token 缓存
  （两次 Directory 一次取 token）、`99991663` 后重取且只重试一次、上限截断、names 全空、错误分类。
- `internal/feishu/directory_live_test.go`：`FEISHU_LIVE_CONFIG=config.yaml` 才运行；真机 ≥1 部门、
  加权限后部门名非空（权限生效的机器可验证证据）。
- store：绑定唯一冲突 / 空 open_id / 覆盖 / 解绑幂等 / `UpsertAccount` 不清绑定；`SetOrgNodeFeishuDepartment`
  打标与冲突；`AddAccountOrgNodes` 幂等与 404；`0024` 已应用。
- `internal/httpapi/admin_org_feishu_test.go`（扩 `feishuStub` 支持 token/部门/人员三段）：三条匹配通道矩阵、
  同名自动落库、同名冲突不覆盖、同步幂等、父先建、深度跳过、创建用户 409 与挂节点、绑定/解绑、403、501。
- 前端：`internal/webui/tests/org_feishu_test.mjs`（文本断言）+ `scripts/ui-harness/org_feishu.page.html`
  （视图 `org-sync` / `org-sync-readonly`：按钮存在/禁用、弹窗、确认框 → `POST /org/feishu/sync`、
  绑定弹窗拼音过滤 → `PUT` 请求体）。

## 10. 依赖与部署

- 依赖：飞书应用需在开放平台添加「获取部门基础信息」`contact:department.base:readonly` 与
  「获取用户基本信息」`contact:user.base:readonly` 并发布新版本；「通讯录权限范围」覆盖要同步的部门。
  未加权限时功能可用但名字为空（有明确 warning，不静默显示空白行）。
- 部署：`make build` → `systemctl --user restart aigw-local`；`/version` 显示新 revision，
  `/admin/ui/js/pages/org_feishu.js` 应为 200。

## 11. 实现与设计差异

按实现顺序记下与上面设计不同的地方，以及为什么。

1. **里程碑号**：本文档原本编号 M69，开工时发现 `docs/design/m69-login-lifecycle-and-settings-merge.md`
   已占用 M69（已随 v2.9.1 发布），因此本功能改号 **M70**（文档已重命名为
   `m70-feishu-org-sync.md`，代码与规格文档里的引用一并改过）。迁移号 `0024` 未冲突，保持不动。
2. **未启用飞书的状态码**：设计写 501，实现用 `domain.ErrUnsupported`（**400
   `unsupported_parameter`**）。这是仓库既有口径——未接线的端口（如 `Deps.Org` 为空）都回答它，
   见 `docs/org.md` §6 最后一行；为一个"这台部署没接飞书"的答案新造一个 501 只会多一种要记的错。
3. **部门名的来源**：设计的遍历里每个部门要拉 `users` 与 `departments` 两个列表；实现就用
   `departments?parent_department_id=` 返回的 `name` 作为子部门的名称（该列表本身就带名字），
   因此**没有**逐部门的详情调用，一个部门的成本是 2 次请求而不是 3 次。这也让"缺名称权限"的表现
   与真机一致：调用成功、字段为空。
4. **缺名称时不建节点**：设计的边界表只写了"预览正常 + warning + 用编号占位"。实现进一步让
   「同步」**不创建**没有名称的部门（`skipped`，计数进 `departments_skipped`）——把 `od-xxx`
   当节点名写进组织架构是垃圾数据，而且它会成为子树标签的载体；缺名称时仍能合并按 id / Key
   命中的人员（这两条通道不需要名字）。`ensureUserDepartmentNodes` 同样跳过无名部门。
5. **单个人的部门补建会连祖先一起建**：设计只说"创建用户/绑定账号时挂入其部门节点"。实现发现
   只建"人员直属部门"会在父节点不存在时造出悬空的根节点（或干脆挂不上），所以
   `ensureUserDepartmentNodes` 会把该人员的部门**及其全部祖先**按 BFS 顺序补建，规则与整体同步
   完全相同（同名匹配 → 打标 → 否则创建；同名节点已属其它部门则不覆盖）。
6. **api_key 通道的名字来自 Key 绑定**：飞书没给名称时，目录里的人员没有 `name`，但 M60 写在
   `api_keys` 上的绑定有（当年扫码时拿到过）。实现里 `feishuUserPlan.BindName/BindUnionID` 对
   api_key 通道优先用 Key 绑定的值，因此"缺权限"的部署里固化到账户上的身份仍然带名字。
7. **预览多带一个合成根节点**：飞书把公司本身当作虚拟部门 `"0"`，它有直属人员但不是部门。控制台
   的树给它一个合成根节点「飞书根组织」，这样"直属公司的人"也有地方显示与操作；接口返回的
   `departments` 仍然只含真实部门（与设计一致）。
8. **响应里多了统计与跳过清单**：`stats`（含 `users_already_synced`）与
   `skipped_departments` / `skipped_users`（带 `reason`：`sibling_name_exists` /
   `node_pinned_to_other_department` / `identity_taken`）。设计只说"报告成功部分与失败原因"，
   落到具体形状是为了让控制台能逐条 toast，并且第二次同步能自证 `0` 写入。
9. **`?refresh=true` 的读取会刷新缓存**：设计只写"手动刷新绕过缓存"；实现让**任何**一次
   `refresh=true` 的读取把新快照写回缓存（否则界面刚刷新完，下一次打开又拿到 60 秒前的旧数据）。
10. **测试落点比设计多两处**：除设计列的三处（store / feishu / httpapi）之外，还加了
    `internal/webui/tests/org_feishu_test.mjs`（用 VM 模块跑真页面，断言发出去的 URL 与请求体：
    同步 POST、创建用户带 open_id、绑定 PUT 的 `account_id`、解绑 DELETE）并把它挂进 `make ui-base`；
    UI 夹层新增 `org-sync` / `org-sync-readonly` / `org-sync-nonames` 三个视图
    （`scripts/ui-harness/org_feishu.page.html`）。
11. **`ListAPIKeyFeishuIdentities` 进了 `AdminStore`**：设计只说加进 KeyStore 端口，但
    `dshgw` 的两条 provisioning 路径把 `AdminStore` 当 `KeyStore` 用，接口不扩就会编译失败；
    于是同一个方法在 `AdminStore` 上也声明了一次（实现仍是 store 的同一个方法）。
12. **`resolveUserDepartmentNodes` 在实现里叫 `ensureUserDepartmentNodes`**
    （`internal/httpapi/admin_org_feishu.go`），语义就是上面第 4、5 条。
13. **成员关系计数把"将创建的节点"算进去**：`stats.memberships_to_add` 原本只数已有节点上的新增归属，
    于是**第一次**同步（所有节点都还没建）会显示"新增成员关系 0 条"——恰恰是新增最多的一次。
    现在 `plannedJoins` 把"该人员所属、且本次将创建"的部门也计入；同步响应里的 `stats` 用**执行前**的计划
    （执行会就地改计划：建过的部门不再是"将创建"），真实写入量在 `created_nodes` / `linked_users` 里。
14. **真机测量（2026-09-21，本机 aigw-local，飞书应用已加两个数据权限）**：一次未命中缓存的目录读取
    耗时 **15.5 秒**（22 个部门 + 根 = 23 次人员列表 + 23 次子部门列表 + 1 次 token，约 320 ms/次；
    `feishu.timeout_s` 是**单次调用**的上限，不是整次走查的上限）。因此：弹窗打开时先显示"正在读取飞书通讯录…"，
    60 秒缓存是"再点一次不痛"的关键；如果要缩短首次等待，应做的是并发化（设计里为限流刻意串行）而不是调大超时。
    同一次真机预览的结果：`names_available: true`、22 个部门（全部 `将创建`）、90 人、
    **13 人自动匹配**（5 人走 `api_key` 通道——M60 绑过的那 5 个账号；8 人走同名通道）、
    77 人待操作员决定。此环境下 `departments_skipped = 0`（层级与名称都合法）。
