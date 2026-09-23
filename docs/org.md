# 组织架构（规格）

> 状态：**已实现（M49）**。
> 设计见 `docs/design/m49-organization.md`。
> 相关：`docs/routing.md`（授权并集与策略合并）、`docs/mcp.md`（后台工具）、`docs/billing.md`（账号即计费主体）。

组织架构回答「这家公司/这个部门有哪些账号」，并让**节点上绑定的标签被整棵子树继承**。
它是一套**独立于标签**的实体：组织树管归属，标签管权限，两者在自己的页面与自己的接口里各自维护，
只在「解析生效标签」这一处交汇。

## 1. 形状

- **多根森林**：`org_nodes.parent_id` 为空即根节点。可以有多个根（多家公司、多个事业部）。
- **兄弟节点内名字唯一**，不同父节点下可以同名（两个分公司都可以有「研发部」）。
- 名字是**人类可读标签**：去除首尾空白后非空、最多 64 个 Unicode 字符，中文/标点/邮箱形式均可，
  与账号名、标签名同一套规则（`domain.normalizeLabel`）。名字**可以改**（组织节点按 id 寻址，
  不像标签那样按名字绑定）。
- **深度上限 16 层**：超过时创建/移动被拒绝（400）。
- **账号多归属**：一个账号可同时挂在多个节点上；一个节点下可有任意多个账号。

## 2. 继承的标签（授权与策略）

账号下**所有 API Key** 自动继承这些标签：

```
成员节点的祖先链（根 → … → 成员节点）
  ├─ 多个成员节点时按「节点 id 升序」依次展开，每个节点各自从根到自身
  ├─ 按名字去重（首次出现优先）
  └─ 然后是：账号自身标签（账号页/接口设置的 tags）
       然后是：Key 自身标签
最后按 tags.priority 稳定排序（等优先级时保持上面的先后）
```

- 生效授权 = Key 自有 grants ∪ 上述全部标签的 grants（`*` 通配，见 `docs/routing.md`）。
- 生效策略 = 上述标签按 `priority` 依次合并，**最后 Key 自己的 policy 覆盖**。
- 限速（rpm/tpm/并发）同样取「最严合并」，所以把节点挂上一个带 `rpm` 的标签会给整棵子树限速。

**为什么继承顺序是这样**：账号名在前使等优先级下的顺序确定（与标签体系既有契约一致），
Key 的策略最后生效，所以 Key 永远能覆盖组织与账号层的设置。

### ⚠️ 拼错的标签名是有代价的

写节点/账号/Key 的标签名时如果名字不存在，解析会**丢弃**这个名字；如果因此导致该凭据
**没有任何授权**，授权会回落到 `default_grant`（部署配置，默认可能是**通配全开**）。
因此：

- **组织节点**写标签时名字必须已存在（未知名字 → 400），这是刻意的严格；
- 账号与 Key 的写路径保留了历史宽松行为（名字不存在会被存储但解析时丢弃），
  **迁移/导入请先用 `admin_list_tags` 确认名字**；
- 标签被删除后，挂在它上面的组织节点/账号/Key 会立刻失去该标签带来的授权。

## 3. 管理接口

| Method | Path | MCP 工具 | 角色 |
|---|---|---|---|
| GET | `/admin/api/v1/org/nodes` | `admin_list_org_nodes` | viewer |
| POST | `/admin/api/v1/org/nodes` | `admin_create_org_node` | admin |
| PATCH | `/admin/api/v1/org/nodes/{id}` | `admin_update_org_node` | admin |
| DELETE | `/admin/api/v1/org/nodes/{id}` | `admin_delete_org_node` | admin |
| PUT | `/admin/api/v1/org/nodes/{id}/accounts` | `admin_set_org_node_accounts` | admin |
| GET | `/admin/api/v1/org/feishu/directory` | `admin_list_feishu_directory` | admin |
| POST | `/admin/api/v1/org/feishu/sync` | `admin_sync_feishu_org` | admin |
| POST/PUT/DELETE | `/admin/api/v1/org/feishu/users/{open_id}/account` | `admin_create_account_from_feishu_user` / `admin_bind_account_feishu_user` / `admin_unbind_account_feishu_user` | admin |
| GET | `/admin/api/v1/accounts?org_node_id=&include_descendants=` | `admin_list_accounts` | viewer |
| POST/PATCH | `/admin/api/v1/accounts`（body `org_node_ids`） | — | admin |

字段语义、形状与示例见 `admin_describe`（MCP）或控制台组织架构页；两者是同一张路由表。

**列表返回的形状**（扁平 + 层级字段，前端不再自己算层级）：

```json
{"data":[
  {"id":1,"parent_id":null,"name":"总部","path":"总部","depth":0,"sort_order":100,
   "note":"","tags":["internal"],"account_count":3,
   "accounts":[{"id":7,"name":"研发-张三"}],"accounts_truncated":false,
   "created_at":"2026-09-15T10:00:00Z","updated_at":"2026-09-15T10:00:00Z"},
  {"id":2,"parent_id":1,"name":"研发部","path":"总部/研发部","depth":1,"sort_order":100,
   "note":"","tags":["internal","vip"],"account_count":2,"accounts":[],
   "created_at":"…","updated_at":"…"}
], "count":2, "total":2, "limit":200, "offset":0, "has_more":false}
```

- `path` 是「根/…/自身」的名字路径，用于下拉框与账户页的「所属组织」列。
- `depth` 与 `parent_id` 让控制台一次性拿到可渲染的标量列表。
- `accounts` 只在 `include_accounts=true` 时返回；超过上限会截断并置 `accounts_truncated: true`。

## 4. 删除与移动

- **删除**：默认只允许删除**叶子**；节点有子节点时返回 400 并提示可加 `cascade=true`。
  `cascade=true` 删除整棵子树。
  - 被删节点上的**成员关系一并消失**（账号本身不被删除，账号与其 API Key 不受影响）。
  - 删除是单事务、按深度倒序执行。
- **移动**：`PATCH /org/nodes/{id}` 的 `parent_id` 可以改父（`null` 表示变成根节点）。
  移动到自身或自己的子孙 → 400；移动后使某节点超过 16 层 → 400。
- **改名**：直接改，按 id 寻址，不会丢任何绑定。

## 5. 控制台

- **组织架构页**（`#/org`，侧边栏「访问控制」组）——M72 起它同时是**账号（人员）管理的主界面**：
  - 左/右两栏：组织树 + 选中节点的**人员列表**。
  - 人员列表是**多列表格**（表头随列表滚动固定在顶部，列在窄屏下换行而不横向溢出）：
    **账号**（名字 + `#id`）/ **DSH** / **飞书** / **Key** / **所属组织** / **操作**；行可**展开**成该账号的详情：
    - 账号字段（状态、计费模式、授信、标签、所属组织路径）；
    - **Key 列表**（名称、前缀、状态、最近使用）与操作：新建 Key（明文只显示一次）、编辑/启停 Key；
    - **绑定飞书 / 解绑飞书**：绑定是弹出**飞书人员列表**（支持拼音过滤，已被别人绑定的置灰）→
      `PUT /admin/api/v1/accounts/{id}/feishu`，**不扫码**（见 [feishu.md §4](feishu.md)）；
    - **启用/停用 DSH**：见下一条；
    - **分配组织**：整表替换该账号的 `org_node_ids`（与账户页的编辑是同一件事，共用
      `pages/org_assign.js` 的勾选树）。
  - 树只渲染在**工作区**（完整模式，带行内操作）。它一度同时挂一份到左侧栏（紧凑模式）并让两处
    选中互相镜像，产品上判定为冗余（同一棵树在同一屏出现两次，还挤占左侧栏的全局导航），已去掉。
  - 树支持：点击选中、三角折叠/展开、展开全部/折叠全部、键盘 ↑↓←→Enter、
    工作区模式的搜索框。
  - **逐人动作**：摘要行上是「编辑」（账号字段：状态/计费/授信/标签/所属组织，与账户页同一个表单）
    与「展开」；展开区是 Key 列表、绑定飞书、启停 DSH、分配组织。
  - **新建成员**（节点详情工具条，在「保存成员」左边）：直接在这个节点下建一个账号——建号与挂到
    本节点是**同一个请求**（`POST /accounts` 带 `org_node_ids`），所以不会出现"人建好了但还不属于
    任何部门"的中间状态。建完只重绘本表、**不重读成员**：操作员还没保存的勾选不会被冲掉，新账号按
    「勾选置顶」规则立刻出现在第一行且已勾选。
  - **分配组织**：弹出**组织树勾选**（支持拼音过滤），勾选即把这账号挂到该节点**本身**——
    账号可同时属于多个节点，勾父节点不会连带勾选子节点（子树继承的是节点**标签**，不是成员）。
    保存是整表替换；不勾任何节点保存 = 移出全部组织（弹窗里有明确警告），失败（例如节点刚被删）
    留在弹窗内报错、不关窗。账户页的新建/编辑账户用的是同一棵树。
  - **未归属账户**：人员列表顶部有一行合成行「未归属账户 N 个」（不在任何节点下的账号）。
    它是视图不是节点：点开只列出这些账号，**该列表不渲染勾选列**（没有节点可写），但每行仍可展开，
    用「分配组织」把它们挂到节点上。
  - 成员分配：在详情卡里勾选账号后「保存成员」（整表替换该节点的成员）。**搜索框里有关键词时不能
    新增勾选**——那张列表是过滤后的视图而不是成员全集，此时整表替换会误删看不见的归属；已勾选的
    仍可取消（那是操作员看得见的一次移除）。
    - **过滤支持拼音与英文**：中文名可以打全拼（`zhangsan`）、首字母（`zs`）、中英混合（`devzs`
      能搜到 `dev-张三`）；英文名按子串匹配（`acme`）；多音字按全部读音匹配（`changwei` 与
      `zhangwei` 都能搜到 `长伟`）。节点树的过滤框同样支持。人员列表的过滤同时匹配账号名与飞书姓名。
    - **过滤框固定在列表上方**，不随成员列表滚动；
    - **勾选的成员自动排到最前**（含"已选 N 个"提示），取消勾选即回到原位。
  - **同步飞书**（右上角，`role=admin`；M70）：弹出飞书组织结构树 + 人员列表，**勾选要同步的部门**
    （含合成根「飞书根组织」＝公司层人员；勾选/取消父部门会**连同其子部门**一起，半选表示"这一行与它的
    子树不一致"，另有「全选 / 清空」），一键「同步」补建缺失部门节点并自动合并**范围内**已匹配的人员；
    勾选部门的**上级**会自动补建（树里标「为层级补建」，其人员不在范围内）；范围外的人员在右侧置灰；
    匹配不上的人逐行「创建用户 / 绑定账号」。
    合并与范围规则、飞书后台需要的权限与排障见
    [docs/feishu.md §5c](feishu.md#5c-通讯录同步组织架构页同步飞书m70)。
- **账户页**：「所属组织」列与**按组织筛选**（可选是否包含子节点）；新建/编辑账户里的
  「所属组织」是**勾选树字段**（一行当前归属路径 + 「分配组织…」按钮），提交时整表替换
  `org_node_ids`（空数组 = 移出全部组织）。账号表单本身由 `pages/account_actions.js` 提供，
  与组织页共用。DSH 列在 M72 起区分三态
  （已启用 / 被管理员停用 / 未启用），并显示账号的飞书身份。它是跨组织的全局总览；
  逐人操作在组织页做。
- **API Keys 页**：仍是全部 Key 的全局总览（跨账号搜索、配额与录制开关）。它的「飞书」列自 M72 起
  是**只读**的 Key 级遗产（登录与同步都按账号级身份判定），绑定入口已移到组织页的账号上。

### 拼音过滤的数据来源与限制

拼音表是**生成文件** `internal/webui/static/js/pinyin.js`（约 137KB / 2 万余字），来源
[mozillazg/pinyin-data](https://github.com/mozillazg/pinyin-data)（**MIT** 许可），文件头记录了
版本与输入文件的 SHA-256。重新生成：

```sh
python3 scripts/gen-pinyin.py                 # 从上游下载（记录版本与校验和）
python3 scripts/gen-pinyin.py --source FILE   # 或用本地副本
```

- 控制台是**零构建**的原生 ES 模块，不能 import npm 包，所以表必须是仓库里的文件；
- **覆盖范围**：CJK 统一汉字基本区 U+4E00–U+9FFF。表外字符（生僻字/扩展区）**不做拼音匹配**，
  只按字面子串匹配——这是有意的取舍，不是静默失败；
- 声调与变音符号一并剥离，所以 **ü 落在 u 上**（女 → `nu`，与 路 同串；表里没有 `v`）；
  操作员打的是 `zhangsan`，不会打 `zhāngsān`；
- 这张表还有第二个消费者：租户名的自动生成（M74，`dsh-<账号拼音>-<账号ID>`）读的是**同一张表**，
  由生成脚本同时输出 `internal/webui/static/js/pinyin.js` 与 `internal/pinyin/table_gen.go` 两份，
  两边的 blob 由 `internal/pinyin` 的测试断言逐字节相等；
- 树形控件本身**不依赖**这张表：拼音匹配是由调用方通过 `matcher` 注入的能力，所以任何一个用
  这棵树的地方都不必被迫背上 137KB 的表。

### 可复用树形控件

树是一个独立模块 `internal/webui/static/js/tree.js`，**不知道组织架构、不发起任何请求**，
输入是扁平节点数组加回调，因此任何层级数据（组织、会话、模型→路由）都能直接复用：

```js
import { tree } from '../tree.js';

const view = tree({
  nodes,                  // [{id, parent_id, name, sort_order, ...任意 meta}]
  selectedId,
  mode: 'sidebar',        // 'sidebar' 紧凑 | 'workspace' 完整
  expandDepth: 1,         // 默认 Infinity（全展开）
  renderLabel: (n) => n.name,
  renderMeta: (n) => n.account_count + ' 个账号',
  actions: (n) => [ ...按钮 ],       // sidebar 模式仅在选中/悬停时显示
  onSelect: (n) => {}, onToggle: (n, expanded) => {}, onAction: (name, n) => {},
});
view.refresh(nodes); view.setSelected(id); view.expandAll(); view.collapseAll();
```

两处放置方式的差别**只有密度与元信息展示**，数据结构与交互完全一致。

**缩进的实现有一处硬约束**：每层 22px 的缩进是行内样式，而控制台的 CSP 是 `style-src 'self'`
（无 `'unsafe-inline'`）——浏览器会丢弃 style **属性**，所以这段样式必须由 `el()` 走 CSSOM
（`node.style.cssText`）写入，不能退回 `setAttribute`。症状极其隐蔽：DOM 里 row 上写着
`style="padding-left:52px"`，`getComputedStyle(row).paddingLeft` 却是 `0px`，整棵树贴着左边缘。
约束由 `internal/webui/tests/style_csp_test.mjs`（源码不变式）与 harness 的 `csp` 视图（真实策略下量几何）
一起守住，见 `docs/todo_done.md`「组织树没有缩进」。

## 6. 排障

| 症状 | 原因与处理 |
|---|---|
| 账号挂上节点后仍然不能访问某模型 | 节点的标签里没有覆盖该模型的 grants；用 `admin_explain_router`（或控制台「模型与路由」的模拟）看解析结果里的标签与候选 |
| 移出节点后权限没有立刻回收 | 检查是否走了接口（写路径会清空 Key 缓存）；正常情况下是**立即**生效，最长不超过一次快照换入 |
| 明明没有标签却能用全部模型 | 该凭据**没有任何**生效授权，回落到 `default_grant` 通配。给节点/账号/Key 补上正确的标签名，或把 `routing.default_grant` 设为 `none` |
| 组织树上少了一个节点 | 兄弟重名会让写入 409；检查是否是同名节点建在了同一父节点下 |
| 删不掉节点 | 它是中间节点，需要 `cascade=true`（控制台会提示将删除整棵子树） |
| 组织树每层都贴着左边缘（"没有缩进"） | 行内样式被控制台 CSP 丢弃了：控制台是 `style-src 'self'`，浏览器会丢掉 style **属性**（DOM 里写着 `padding-left:52px`，计算值是 `0px`）。行内样式必须走 CSSOM，即 `ui.js` 的 `el()` 的 `style` 键；排查时看 `getComputedStyle(row).paddingLeft`，不要看 `row.style`/属性。见 `docs/todo_done.md`「组织树没有缩进」 |
| 组织页显示"该部署未启用组织架构" | `Deps.Org` 端口为空（该构建/测试环境未接存储），此时 org 接口回答 400 `unsupported_parameter`（与其它未接线端口一致）；正式部署不会出现 |
| 「同步飞书」提示部门/人员没有名称 | 飞书应用缺两个只读数据权限；加权限并发布版本，见 [docs/feishu.md §5c](feishu.md#5c-通讯录同步组织架构页同步飞书m70) |
| 「同步飞书」报错 502 | 飞书侧失败（凭据/权限/网络/限流），弹窗给出中文原因；详细数字码在 aigw 日志 |
| 同步后有人没被合并 | 名字不完全一致或账户已绑定其它飞书身份：用行内「绑定账号」人工绑定（支持拼音过滤），自动合并绝不覆盖已绑定身份 |
| 同步建的节点删不掉/不想保留 | 与手工节点一样：`DELETE /org/nodes/{id}?cascade=true`；节点上的 `feishu_department_id` 会随节点一起删除 |
| 组织页里"保存成员"是灰的 | 成员还没读回来（按钮要等这一次读完成才能落库）；搜索框有过滤关键词时勾选框也只能取消、不能新增——过滤后的列表是视图而非成员全集 |
| 点「收起」收不起来 | 已修：详情行默认 `display:none`，只有 `.open` 才成为一行（`app.css` 的 `.org-person-detail` 门控）。这条规则曾缺失，于是"收起"只是把 `open` 类摘掉、内容照旧可见 |
| 合成行「未归属账户」里的账号点不进任何节点 | 它不是一个节点：展开这些账号后用「分配组织」（`PATCH /accounts/{id}` 的 `org_node_ids`）把它们挂到某个节点下 |
| 账号行的飞书操作看不见 | 只读角色（`role=viewer`）看不到绑定/解绑/停用等写操作；飞书未配置的部署也不会出现这些元素 |
