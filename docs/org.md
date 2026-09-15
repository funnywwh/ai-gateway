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

- **组织架构页**（`#/org`，侧边栏「访问控制」组）：
  - 左/右两栏：组织树 + 选中节点详情（名称、备注、父节点、排序、标签、成员账号）。
  - 树同时出现在**侧边栏**（紧凑模式，用于快速跳转）与**工作区**（完整模式，带行内操作）；
    两处的选中状态同步。
  - 树支持：点击选中、三角折叠/展开、展开全部/折叠全部、键盘 ↑↓←→Enter、
    工作区模式的搜索框。
  - 成员分配：在详情卡里勾选账号后「保存成员」（整表替换该节点的成员）。
- **账户页**：新增「所属组织」列与**按组织筛选**（可选是否包含子节点）；编辑账户时可设置
  `org_node_ids`（逗号分隔的节点 id，整表替换该账号的归属）。

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

## 6. 排障

| 症状 | 原因与处理 |
|---|---|
| 账号挂上节点后仍然不能访问某模型 | 节点的标签里没有覆盖该模型的 grants；用 `admin_explain_router`（或控制台「模型与路由」的模拟）看解析结果里的标签与候选 |
| 移出节点后权限没有立刻回收 | 检查是否走了接口（写路径会清空 Key 缓存）；正常情况下是**立即**生效，最长不超过一次快照换入 |
| 明明没有标签却能用全部模型 | 该凭据**没有任何**生效授权，回落到 `default_grant` 通配。给节点/账号/Key 补上正确的标签名，或把 `routing.default_grant` 设为 `none` |
| 组织树上少了一个节点 | 兄弟重名会让写入 409；检查是否是同名节点建在了同一父节点下 |
| 删不掉节点 | 它是中间节点，需要 `cascade=true`（控制台会提示将删除整棵子树） |
| 组织页显示"该部署未启用组织架构" | `Deps.Org` 端口为空（该构建/测试环境未接存储），此时 org 接口回答 400 `unsupported_parameter`（与其它未接线端口一致）；正式部署不会出现 |
