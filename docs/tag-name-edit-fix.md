# 中文标签名可编辑修复（2026-09-14）

gptjp 上给三个标签（蓝精灵1/2/3）加 `deepseek` 供应商授权时发现：**控制台「标签 → 编辑」对它们必然失败**，
名字是中文的标签**无法通过管理 API 更新**，只能直写 SQLite。

`docs/TODO.md` 的 M43 迁移记录里有同一条先例：迁移时补 `"models":["*"]` 也是"按既有先例直写 SQLite"。
这次把它按缺陷修掉，而不是再写一次 SQL。

## 原因与复现

- 标签的唯一写路径是 `POST /admin/api/v1/tags`，语义是**按名字 upsert**；它的名字校验用
  `admin_json.go` 的 `resourceNameRE = ^[A-Za-z0-9._-]{1,64}$`（ASCII 标识符规则，与 provider / model /
  portal 用户名共用）。
- 库里**已经存在**非 ASCII 名字的行（导入器建的 蓝精灵1/2/3）。于是这些行进入死锁状态：
  按名字 upsert 被名字校验拦住（400），而"改名"只会新建一行、旧行与它的绑定原样留下。
- 复现（修复前，线上实测）：

  ```
  POST /admin/api/v1/tags  {"name":"蓝精灵1", ...}
  → 400 {"code":"invalid_request",
         "message":"tag name must match [A-Za-z0-9._-] and be at most 64 characters"}
  ```

- 这条规则对标签本身也没有收益：标签名是**被精确比较的标签**（`tags_json` 数组、
  `grants.providers` 的值），永远不会成为路径段、主机名或文件名。控制台表格里它还只是段文本。

## 变更

1. **`PATCH /admin/api/v1/tags/{id}`（工具名 `admin_update_tag`）**：按 id 局部更新
   `grants` / `policy` / `description` / `priority`，**只改传入的字段**（省略即保持原值，
   显式 `null` 才清空）。名字是身份，不接受改名：`name` 只允许回传当前名字（幂等，照顾控制台表单的
   回显）；真正的改名返回 400 并说明原因——标签是按**名字**绑定在 `accounts.tags_json` /
   `api_keys.tags_json` 上的，改名会静默丢掉全部绑定。要换名字就新建标签、搬绑定、再删旧的。
2. **标签名改用与账户名同一条规则**：新增 `domain.NormalizeTagName`（`internal/domain/tag_name.go`），
   与 `NormalizeAccountName` 共用 `normalizeLabel`：去除首尾空白、非空、合法 UTF-8、最多 64 个 **rune**
   （中文按一个字算）。理由有两条：账户名在上一个提交（`29cb087` "support unicode account names"）里
   已经确立"人类可读标签接受任意 Unicode"的口径，标签名不能与它漂移——**一个写入方接受、另一个拒绝的名字，
   正是让某一行变得不可编辑的原因**；而 `resourceNameRE`（ASCII 标识符）继续用于 provider / model /
   portal 用户名等真标识符。`internal/domain/tag_name_test.go` 逐例断言两条规则同意。
3. **store**：新增 `GetTagByID`（未命中 → 404）与 `UpdateTag`（按 id 写全部可变列；重名 →
   409 `ErrConflict`）。`UpsertTag` 保持原样，创建路径继续按名字幂等。
4. **控制台标签页**：编辑走 `PATCH /tags/{id}`（原先是 `POST /tags`，既会因名字被拒、又会在改名时
   分叉出第二个标签）；编辑态下名称字段只读（`ui.js` 的 `readonly` 约定：仍提交值，但身份不可改）。

## 测试与验收

- `internal/httpapi/admin_tags_test.go`（新增）：
  - 中文名创建成功（回归）：`蓝精灵3` 通过 `POST /tags`；
  - 名字校验边界：空/仅空格 → 400；64 个字符 → 200、65 → 400；`team.a-b`、含空格与全角括号的中文名 → 200；
    64 个**中文字**同样算 64 个字符（rune 计数，不是字节）；
  - **按 id 更新中文名标签**：`grants` 加上 `deepseek` 后 200，名字不变，且 `registry` 重新加载后的快照里已含新授权（数据面读的是快照）；
  - 局部更新：只传 `priority` 时 `description`/`grants` 保持原值；
  - 改名 → 400 且错误信息含 `cannot be renamed`；未知 id → 404；被拒的写入不动行（回读校验）；
  - body 校验：`grants` 非对象/非法 JSON、policy 含网关不读的嵌套字段、负 priority、空名字 → 400；`{"grants":null,"policy":null}` → 清空；
  - viewer 角色 → 403。
- `internal/domain/tag_name_test.go`（新增）：标签名与账户名两条规则逐例同意（中文/邮箱式/标点/空白裁剪/
  64 字上限/65 字拒绝/非法 UTF-8）。
- `internal/store/tags_test.go`（新增）：按 id 往返（含中文名）、更新后 id/名字不变、未知 id 404、
  无 id 拒绝、改名撞名 → 409。
- 路由表守卫：`admin_routes_test.go` 的 `expectedAdminPatterns` 增 `PATCH /admin/api/v1/tags/{id}`
  （漏登记会让 `TestAdminRouteTableCoversEveryEndpoint` 直接失败）；新条目的 body 形状/示例按
  `docs/mcp.md` §4.5 写全（MCP `admin` scope 自动可见，`dangerous` 带理由）。
- 控制台静态断言：`internal/webui/tests/tags_binding_test.mjs` 增 PATCH-by-id 与 `readonly: !!row` 两条。
- `make verify` 等价执行：`go vet ./...`、`go test ./...`、`go build ./...` 全绿
  （`pkg/pluginapi` 的 `TestClientCredentialsNotification` 在全量并发跑时偶发 1s 超时，
  单独重跑 3 次均通过；该包与本次改动无关）。
  本沙箱没有 node，`make ui-base` 的两个 JS 断言用等价 Python 正则复核过匹配。

## 部署状态

- 本修复**尚未部署**：gptjp 上仍是 0.10.0（`9368b07`），本次授权变更是在旧二进制上通过
  SQLite + 管理接口 reload 完成的。下次发版带上本修复后，这类标签改动可以直接在控制台做。
- 遗留（不属本仓库）：DSH 客户端把任意 401/403 一律显示成「API 密钥无效」
  （`dsh-llm-deepseek` / `dsh-llm-pi-ai` 的 `AUTH` 归类），而网关返回的是
  `permission_error: model or provider not allowed for this API key`，文案与事实不符。
