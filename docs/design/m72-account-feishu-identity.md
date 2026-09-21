# M72 设计文档：账号级飞书身份、组织页整合、多 Key 登录选择

> 状态：**设计（待确认后实现）**。
> 面向使用者的规格：[docs/feishu.md](../feishu.md) §1/§4/§5/§5c.4/§6/§8、[docs/org.md](../org.md) §5、
> [docs/dshgw.md](../dshgw.md) §3。
> 前序：M49 组织架构、M52-rev2 账号级 dsh 开关、M60 Key 级飞书绑定、M61 门户飞书登录、
> M66 控制台管理员扫码、M67 租户侧栏身份、M69 登录即同步、M70 通讯录同步、M71 宿主目录工作区。
>
> 需求原话（五条）：
> ① 「Key、账号、组织架构在管理后台界面整合」；
> ② 「飞书绑定到账号（不再只绑 Key）」；
> ③ 「只要配置文件开启了 dsh，所有激活账号都能用」；
> ④ 「绑定飞书不需要扫码，弹窗让管理员选择飞书人员」；
> ⑤ 「dshgw 登录时，账号有多个 Key 就弹选择框」。
>
> 用户确认的四项决策（2026-09-21）：整合以**组织架构为中心**，人员列表项上带账号操作、可展开看详情与
> Key 列表；多 Key 弹窗**出现在两种登录路径**上，选中**只影响本次会话归属与审计**；DSH **默认全开 +
> 首次登录按需建租户**，保留「停用」为显式例外；Key 级扫码绑定**替换**为账号级选人绑定，存量 Key 级
> 数据**迁移到账号级后清空**。

## 1. 目标

1. **身份真值上移**：DSH 门户飞书登录的判定从 `api_keys.feishu_open_id`（M60）改为
   `accounts.feishu_open_id`（M70 已存在的数据）。一个飞书身份对应**一个账号**，不再对应某一把 Key。
2. **绑定不再扫码**：管理员在控制台为账号**选择飞书人员**（弹窗 + 拼音过滤），全程不出现飞书授权页。
3. **配置开启即全量可用**：`dshgw.auto_enable: true` 时，任何 **active** 且**未被显式停用**的账号都
   能登录门户；租户在**首次登录时按需创建**，不再要求管理员逐个点「启用 DSH」。
4. **多 Key 账号先选 Key**：Key 登录与飞书登录在账号拥有 ≥2 把可用 Key 时，先显示选择页；
   选中结果进入本次会话的归属与审计（不改变 worker 的模型凭据）。
5. **控制台整合**：组织架构页成为人员/账号的主界面——人员列表每行可展开，展开处是该账号的详情、
   Key 列表与操作（新建/启停 Key、绑定飞书、启用停用 DSH、分配组织）。

### 非目标

- **不做**「选中的 Key 决定 worker 用哪把凭据跑模型」（用户选 A）：租户的模型凭据仍是账号级一把
  `dshgw-*` worker Key，登录永不轮换它。
- 不动控制台管理员登录（`admin_users.feishu_*`，M66）：它仍是**扫码/邀请链接**，与门户登录是两套命名空间。
- 不删 `/keys`、`/accounts` 两个页面：它们是全局总览（跨账号搜索、批量操作），组织页是人员视角的主入口。
- 不做飞书侧事件订阅、不做定时自动同步、不做组织人员与账号的双向同步（沿用 M70 口径）。
- 不改租户 worker 的隔离/端口/工作区形态。

## 2. 现状事实（代码级，2026-09-21 复核）

| 事实 | 位置 |
|---|---|
| 门户飞书登录按 **Key 级**绑定判定：`FindAPIKeyByFeishuOpenID` → account | `internal/httpapi/admin_feishu.go` `finishFeishuLogin` |
| 账号级身份（M70）只被当作"同步映射"，不授予登录 | `internal/store/accounts.go`、`docs/feishu.md` §5c.4 |
| dshgw 的授权契约：`POST /v1/dshgw/authorize` → `{allowed,tenant,account,feishu_name}`，`dsh_enabled=false` → 403 `dsh_disabled` | `internal/httpapi/v1.go` `handleDSHGWAuthorize` |
| `feishu_name` 只扫 **Key 级**姓名（worker Key 不带绑定，所以几乎总是空） | 同上 `accountFeishuName` |
| 门户登录：`POST /login`（明文 Key）→ 验 Key → authorize → resolveTenant → prepareLogin → 会话 | `internal/dshgw/proxy/proxy.go` `login` |
| 门户飞书登录：`GET /login/feishu` 兑换一次性票据 → 复核 → `prepareLogin(tenant,"")` → 会话；票据里的 `KeyID` **无消费方** | `internal/dshgw/proxy/feishu.go` |
| 票据契约：aigw 用 `internal/feishu` 签、dshgw 用 `internal/dshgw/feishu` 验，共享测试向量 | `internal/dshgw/contract/testdata/feishu_ticket_vectors.json` |
| 绑定入口（控制台）：「绑定飞书」= 302 到飞书授权页（即"扫码"） | `internal/webui/static/js/pages/keys.js`、`GET /admin/api/v1/keys/{id}/feishu/bind` |
| 「绑定账号」（M70 的人员→账号方向）已经是"账号选择弹窗 + 拼音过滤" | `internal/webui/static/js/pages/org_feishu.js` |
| 租户供应：铸 `dshgw-<tenant>-<hex4>` worker Key → `CreateTenant`/`SetTenantKey`+`StartTenant`，按**账号**一份 | `internal/httpapi/admin_catalog.go` `provisionAccountDSH` |
| 本机实测：30 个账号（28 active）、**7 个 dsh_enabled**、8 个已分配租户、54 把 Key、**5 把有 Key 级绑定**、**7 个账号有 ≥2 把 Key** | 只读查 `data/aigw-local.db` |
| 飞书通讯录实测 22 部门 / 90 人（M70 记录） | `docs/design/m70-feishu-org-sync.md` §11 |

## 3. 关键决策

### D1 登录真值：`accounts.feishu_open_id`

`finishFeishuLogin` 改为 `FindAccountByFeishuOpenID(open_id)`（store 方法 M70 已实现并在用）。
未命中 → `unbound`（文案改为"尚未绑定到任何账户：请联系管理员在控制台组织架构页选择你"）。
随后三道闸门保持不变：`account.status` 必须 active、DSH 有效（见 D3）、租户已分配。

**取舍**：身份绑定不再由本人扫码证明，而是管理员指认。这是需求 ④ 的直接后果——**登录时**"你是不是这个人"
仍由飞书证明（OAuth 拿到的 `open_id`），管理员改的只是"这个 open_id 对应哪个账号"。威胁模型写进
`docs/feishu.md` §6：**能改绑定的人 = 能把某个飞书身份登录到任意账号的人**，因此这是管理员权限，
控制台按 `role=admin` 收敛并全量审计。

### D2 显式停用需要一个新列，自动启用才安全

「配置开启 → 所有激活账号可用」与「管理员能停用某人的 DSH」是两条相反的要求，靠现有
`dsh_enabled` 无法共存：该列只有 true/false，无法区分"从未启用"与"管理员显式停用"（M62 的自动启用
就是为此只敢启用"从未启用过"的账号）。

- 新增 `accounts.dsh_disabled_at INTEGER`（迁移 `0025`）：**管理员显式停用 DSH 的时间**。
- 有效判定（唯一实现 `accountDSHEffective`）：
  `enabled = dsh_enabled || (cfg.Dshgw.AutoEnable && dsh_disabled_at IS NULL)`。
- 控制台「停用 DSH」写 `dsh_disabled_at = now`；「启用 DSH」清空它。
- 新配置 `dshgw.auto_enable`，**默认 false**（仓库惯例：升级不改行为，本机 `config.yaml` 由操作者打开）。

**取舍**：`dsh_enabled` 保持"最后一次操作的物理状态"，新列只表达"管理员是否否决"。这样
`/v1/dshgw/authorize` 的 403 `dsh_disabled` 语义、控制台的按钮文案、既有测试都还能对上，
且关掉开关就完全回到 M52 行为。

### D3 按需建租户落在 aigw 的 `/v1/dshgw/authorize`

只有 aigw 有 dshgw admin socket（`localdshgw`），dshgw 进程自己没有 provisioning 能力，所以"首次登录建租户"
只能在这一跳完成：

```
dshgw 登录 → POST /v1/dshgw/authorize
  有效 DSH 且 dsh_tenant != "" → 200（现状）
  有效 DSH 且 dsh_tenant == "" 且 auto_enable → provisionAccountDSH(actor="dshgw-auto") → 200（新）
  失败 → 403 {"allowed":false,"reason":"provision_failed"}（fail-closed，下一次登录重试）
```

- 只在"租户名为空"时触发：已存在租户名（即使 dshgw 里已丢失）**永不**自动重建——否则一次 dshgw 侧
  数据丢失会把所有账号悄悄重建成新租户（与原数据分叉）。
- 供应前先校验 worker Key 在 aigw 有可用模型：`default_grant: none` 的部署里"账号没有任何模型"
  是常见状态，而 `CreateTenant` 会因空模型列表拒绝。这种情况回答可操作的 `provision_failed` 原因
  （"该账号在当前网关没有可用模型"），而不是把 dshgw 的原始措辞丢给用户。
- 供应成功后 `provisionAccountDSH` 内部已 `reload()`（失效账号缓存），因此同一次登录的后续调用立刻看到新租户。

### D4 多 Key 选择不新建通道：复用票据（pick ticket）

门户侧**没有**能列出一个账号 Key 列表的凭据（浏览器只有明文 Key 或一次性票据），所以选择页必须由
**aigw 授权、dshgw 呈现**。三条候选路径：

| 方案 | 取舍 |
|---|---|
| aigw 回调页上选（`finishFeishuLogin` 前） | 会把选择页做在 aigw 的 origin 上，浏览器随后跨到门户；且 Key 登录路径根本没有 aigw 页面 |
| 新开一条 aigw↔dshgw 的内部通道 | 违反现有契约设计（两侧只共享签名票据与 authorize 调用），多一个要保密的接口 |
| **复用票据：新模式 `keypick`**（选它） | 门户自己的 origin 上渲染选择页，cookie/跨主机两种形态天然可用，aigw 只多一个签发点 |

`keypick` 票据：`{v:1, mode:"keypick", account_id, open_id, nonce, exp}`，aigw 用 `feishu.ticket_secret`
签（与登录票据同一密钥），dshgw 用同一 codec 验，TTL = 新配置 `feishu.pick_ttl_s`（默认 120s，
上限沿用 `MaxTicketTTL` 10 分钟）。**不在**票据里放 Key 明文或"选哪把"的答案：选择由浏览器提交，
服务器按 D5 校验。

### D5 选择只做归属与审计，且**不信任表单**

- Key 列表来自 aigw：`/v1/dshgw/authorize` 的 200 载荷新增
  `keys:[{id,name,key_prefix,last_used_at}]`——该账号 **active 且未过期** 的 Key，
  **过滤掉 `dshgw-` 前缀的 worker Key**（它是机器凭据，不是"哪把 Key"）。
- 门户提交 `key_id` 后，用它**刚取到的那份列表**校验（不读表单里的其它字段）；跨账号/已停用的 id 一律拒绝。
- 选中结果只用于：审计（新增动作 `login_key_selected`，记 `key_id`/`key_name`）与门户侧会话归属。
  `PrepareLogin` 仍收到**空** key（D6）。
- pick 票据的 nonce 在提交时消费一次：重放（后退、双击、分享链接）得到"已经使用过了"。

### D6 选中的 Key 不进 worker 凭据

`prepareLogin(r, tenant, submittedKey)` 的 `submittedKey` 只对**从未写入 key 的租户**有效
（`AdoptKey`：文件里已有 key 时明确不轮换）。按需建租户的路径总会先写入 worker Key，因此选择页走
`submittedKey=""` 是准确的，不会静默改掉任何一个租户的模型凭据。若将来要"用我的 Key 跑我的额度"，
那是另一个里程碑（涉及凭据热更新与并发会话冲突）。

### D7 控制台整合形态：组织页为中心

- 组织页右侧面板从"成员勾选列表"升级为**人员列表**（实体仍是账号）：账号名 + DSH 状态 + 飞书名 +
  Key 数 + `#id`，行可展开 → 账号详情 + **Key 列表** + 行内操作。
- 节点成员关系（勾选 + 保存成员）保留，但**只在"全部账户"范围下可编辑**：按部门/未归属过滤时的列表
  是视图，不是成员全集，此时让"保存成员"以整表替换语义落库会误删别人的归属。
- 合成行「未归属账户」：`GET /accounts` 是唯一能回答"哪些账号不在任何节点"的接口，因此未归属账号
  以一行 badge + 计数呈现，展开后可用账户页的 `PATCH /accounts/{id}`（`org_node_ids`）分配组织。
- `/keys` 降级为全局 Key 总览（飞书列变只读），`/accounts` 保留（组织筛选、批量、DSH 三态）。

### D8 绑定路由：新增账号优先的两个，原人员优先的两个保留

| 路由 | 用途 |
|---|---|
| `PUT /admin/api/v1/accounts/{id}/feishu`（新，`{open_id,union_id,name}`） | 控制台的绑定：由人员弹窗选出，**不要求**该人仍在通讯录里，也**不**顺带改组织归属 |
| `DELETE /admin/api/v1/accounts/{id}/feishu`（新，幂等） | 控制台解绑 |
| `PUT/DELETE /admin/api/v1/org/feishu/users/{open_id}/account`（M70，保留） | 人员优先方向：顺带把账号挂进其部门节点；控制台「同步飞书」弹窗与 MCP/脚本仍用 |
| `GET /admin/api/v1/keys/{id}/feishu/bind`（**废弃**，改 410） | 原扫码绑定入口；410 文案给出替代路径 |
| `DELETE /admin/api/v1/keys/{id}/feishu`（保留） | 清理存量 Key 级绑定；仍是 MCP 工具 `admin_unbind_key_feishu` |

### D9 存量迁移是一次性种子，之后 Key 级只读

`api_keys.feishu_*` 的 5 条存量绑定（本机实测）在启动时抄到对应账号：

- 账号未绑定 → 写账号级身份 + 审计 `feishu_bind`（`target_type=account`，changes 带 `from_key_id`，
  由启动钩子在写完后补记），然后清空 Key 行；
- 账号已绑**同一个** open_id → 只清 Key 行（不重写账号上的绑定人/时间）；
- 账号已绑**别人**，或同一账号的多把 Key 绑了**不同的人**（后者靠 `BindAPIKeyFeishu` 的唯一索引
  已经无法新建，但老库可能有）→ 保留 Key 行不动、计入 `conflicts` 并 `log.Warn`，
  由管理员在控制台处理（Key 行仍只读可见可解绑）；一律**不猜**哪个人才是对的。

迁移后 M70 合并通道②（"某把 Key 的 open_id 相同"）**删除**。理由：账号级身份已存在时它不再提供
新信息，却会在管理员把某人手工改绑到另一个账号之后，把这个人从旧 Key 数据里"复活"到旧账号上
（`planFeishuOrg` 每轮都从 `api_keys` 重算）。

## 4. 数据模型与配置

迁移 `internal/store/migrations/0025_account_dsh_auto.sql`：

```sql
-- M72: 自动启用 DSH（dshgw.auto_enable）需要区分「从未启用」与「管理员显式停用」。
-- NULL = 从未被显式停用过（因此可被自动启用），非 NULL = 管理员停用的时刻。
ALTER TABLE accounts ADD COLUMN dsh_disabled_at INTEGER;
```

- domain：`Account.DshDisabledAt *time.Time`。
- store：`accountCols` / `scanAccount` 增列；`SetAccountDSHDisabledAt(ctx, id, *time.Time)`。
- 配置（`internal/config/config.go`）：

```yaml
dshgw:
  auto_enable: false        # 新：激活账号无需逐个「启用 DSH」，首次登录按需建租户
feishu:
  pick_ttl_s: 120           # 新：多 Key 选择页的票据有效期（>0，上限 600）
```

## 5. 接口

### 5.1 `POST /v1/dshgw/authorize`（aigw，dshgw→aigw）

```jsonc
// 200
{"allowed":true,"tenant":"dsh-colin","account":"李智超(colin)","feishu_name":"李智超",
 "keys":[{"id":12,"name":"colin-laptop","key_prefix":"sk-gw-abc…","last_used_at":"2026-09-20T…Z"}]}
// 403（新增原因）
{"allowed":false,"reason":"provision_failed"}   // 自动建租户失败；详情只在 aigw 日志里
```

`keys` 缺省（空数组）表示"该账号没有可用 Key"；旧版 aigw 不返回该字段时 dshgw 视为"未知"——只跳过
选择页，登录照旧（向后兼容一个版本）。

### 5.2 门户（dshgw）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/login/pick` | 选择页：验 pick 票据（`VerifyPick`）→ 用该账号 worker Key 调 authorize 取 Key 列表 → 渲染单选表单；只有 1 把 Key 时直接登录 |
| POST | `/login/pick` | 提交 `key_id` + 票据 → 校验 id 在列表内 → 消费票据 → 签发会话（与其它两条登录路径完全相同） |

选择页与登录页共用同一模板与 CSP（`form-action` 已允许自身），无 JS：`<input type=radio name=key_id>`
+ 说明"选择只决定这次会话归属哪把 Key；模型额度按账号计算"。

### 5.3 控制台（aigw admin）

| 方法 | 路径 | 变化 |
|---|---|---|
| GET | `/admin/api/v1/accounts` | 每行增 `feishu`（形状同 Key 行）、`dsh_effective`、`key_count`、`active_key_count` |
| GET | `/admin/api/v1/accounts/{id}/dsh` | 增 `effective`（自动启用下的有效值）与 `disabled_at` |
| PUT/DELETE | `/admin/api/v1/accounts/{id}/feishu` | 新（D8） |
| GET | `/admin/api/v1/keys/{id}/feishu/bind` | 410 Gone（替代路径见文案） |
| GET | `/admin/api/v1/org/nodes/{id}/accounts` | 每行增 `dsh_enabled/dsh_tenant/feishu/active_key_count` |

## 6. 数据流

### 6.1 飞书登录（多 Key）

```
门户「飞书登录」→ aigw /feishu/login → 飞书授权 → /feishu/callback
  → open_id → accounts.feishu_open_id（D1）→ 闸门（active / 有效 DSH / 租户）
     单 Key：签 dsh 票据 → cookie/URL → 门户 /login/feishu → 建会话（现状）
     ≥2 Key：签 keypick 票据 → cookie/URL → 门户 /login/pick
                 → GET 渲染选择页（用 worker Key 调 authorize 取 keys[]）
                 → POST 选 key_id → 校验 → 审计 login_key_selected → 建会话
```

### 6.2 Key 登录（多 Key）

```
POST /login → GET /v1/models 验 Key → POST /v1/dshgw/authorize
  → keys[] ≥2：签 keypick 票据（account_id 来自 authorize）→ 303 /login/pick（不建会话）
  → 选择 → 建会话；keys[] ≤1：现状（直接建会话）
```

### 6.3 首次登录按需建租户

```
authorize：有效 DSH & dsh_tenant=="" & auto_enable
  → 校验 worker Key 有模型 → provisionAccountDSH("dshgw-auto")
      → 铸 worker Key → CreateTenant → 写 dsh_tenant/dsh_enabled → 审计 dsh_enable → reload
  → 200 {tenant}
失败 → 403 provision_failed（日志留原因；下一次登录重试）
```

### 6.4 启动迁移

```
store.Open → MigrateKeyFeishuToAccounts()（D9）→ 计数/冲突日志 → Bootstrap → 起服务
```

## 7. 异常与边界

| 情况 | 行为 |
|---|---|
| 飞书身份未绑定任何账号 | `unbound`：门户文案指向"管理员在控制台组织架构页选择你" |
| 账号 suspended/closed | `account_status`（现状） |
| 有效 DSH 为假（`dsh_disabled_at` 非空，或开关关且从未启用） | `dsh_disabled`（现状） |
| 租户未分配且 `auto_enable` 关 | `tenant_missing`（现状，提示管理员启用） |
| 自动建租户失败（通道不可用/无模型/网关拒绝） | 403 `provision_failed` + 日志；控制台可手动启用 |
| pick 票据过期/已用/签名错 | 门户回登录页："请重新点击「飞书登录」" |
| 提交的 `key_id` 不属于该账号或已停用 | 拒绝并重新渲染选择页（不泄露该 id 是否存在） |
| 选择页渲染时账号只剩 1 把 Key | 直接建会话（不显示已无意义的选择） |
| 账号没有任何可用 Key | 选择页显示空态："该账号没有可用的 Key，请联系管理员签发" |
| Key 登录时 authorize 不可达 | 503（现状 fail-closed），不进入选择页 |
| 管理员停用 DSH 后本人登录 | 被拒，且**不会**被自动重新启用（D2） |
| 迁移时同一账号两把 Key 绑了不同的人 | 都不迁移，WARN + 控制台可见（Key 行只读） |

## 8. 威胁模型与隐私（写进 `docs/feishu.md` §6）

- **管理员代绑的语义变化**（D1）：绑定不再证明"本人同意"，只证明"管理员指认"。因此：
  绑定/解绑是 `role=admin` 的写操作，全量审计（`feishu_bind` / `feishu_unbind`，
  `bound_by` 记管理员）；门户登录仍然每次都要求飞书完成一次 OAuth（身份不可伪造）。
- 选择页**不出现任何 Key 明文**：只显示名称/前缀/最近使用；`keys[]` 里没有 secret，
  也没有 hash。审计只记 `key_id`/`key_name`。
- `keypick` 票据与登录票据同密钥、不同 mode，两侧验证器按 mode 严格分流（一张票据不能跨用途兑换）。
- 迁移是数据搬运，不新增暴露面；`from_key_id` 只进审计，不进口户响应。

## 9. 测试策略

**Go**
- `internal/feishu/ticket_test.go` / `internal/dshgw/feishu`：keypick 签发/验证/模式互斥/过期/重放；
  契约向量新增一条 keypick（`cmd/gen-feishu-vectors` 重生成）。
- `internal/httpapi/dshgw_authorize_test.go`：账号级姓名进 `feishu_name`；`keys[]` 过滤 worker Key 与停用 Key；
  `auto_enable` 关→403 `dsh_disabled`；开→首登 200 且 `fakeDshgwAdmin` 只建一次租户（第二次登录不再建）；
  `dsh_disabled_at` 非空→403；无模型账号→403 `provision_failed`。
- `internal/httpapi/admin_feishu_test.go`：`finishFeishuLogin` 账号级命中/未命中；单 Key 发 dsh 票据、
  多 Key 发 keypick 票据并 303 到 `/login/pick`；`GET /keys/{id}/feishu/bind` 410。
- `internal/httpapi/admin_org_test.go` / `admin_feishu_test.go`：新 accounts feishu 路由（200/404/409/403/幂等）、
  节点成员行的新字段、DSH 三态字段。
- `internal/store/keys_feishu_migrate_test.go`：四种迁移情形（空库、正常、已绑同一人、冲突）+ 幂等。
- `internal/dshgw/proxy/pick_test.go`（新）：渲染、正确 id 建会话、跨账号 id 被拒、票据重放被拒、
  单 Key 直接登录、票据账号与 worker Key 账号不一致时拒绝。
- `internal/mcpsrv`：新/改路由的字段说明与 `admin_describe` 形状（`docs/mcp.md` §4.5）。

**控制台**
- `internal/webui/tests/account_feishu_test.mjs`（新）：人员弹窗读 `/org/feishu/directory`、已绑他人置灰、
  确认发 `PUT /accounts/{id}/feishu`、解绑发 DELETE。
- `internal/webui/tests/org_person_list_test.mjs`（新）：人员行渲染（DSH/飞书/Key 数）、展开拉
  `GET /keys?account_id=`、未归属合成行、只读角色无写操作。
- 更新 `keys_feishu_test.mjs`（无绑定按钮、解绑保留）、`org_tree_test.mjs`（成员行结构）。
- `scripts/ui-harness/org_person.page.html`（新视图）挂进 `make ui-check`；`ui-base` 目标登记两个新 mjs。

**命令**（本机）
- `NODE=/home/winger/.local/node-v22.23.1-linux-x64/bin/node`（`make` 用 `DSHGW_NODE`；`node` 不在 PATH）。
- `go vet ./internal/... ./cmd/...` + `go test ./internal/... ./cmd/...`（**不要**裸跑 `make verify`：
  `./...` 会扫 `./data` 的 8.5 GB 库与 dshgw 工作区副本而挂住）。
- `make ui-base`、`make dshgw-test`、`make dshgw-build`、`scripts/ui-harness/run.sh --views org-person`。

## 10. 依赖与部署

- 无新增外部依赖；飞书侧只需 M70 已有的两个只读通讯录权限（选人弹窗读的是同一份目录）。
- 部署：`make build` → `systemctl --user restart aigw-local`；`/version` 显示新 revision；
  `/admin/ui/js/pages/org.js` 与 `…/pages/account_feishu.js` 应 200。
- 打开自动启用：在 `config.yaml` 的 `dshgw` 块加 `auto_enable: true` 后重启（本机默认仍关闭，
  由操作者决定何时切）。

## 11. 主机验收（待执行，逐条回填结果）

1. 组织页给一个未绑定账号选一个飞书人员 → 账号行出现飞书名，全程无飞书授权页；
2. 该人走门户「飞书登录」（`auto_enable: true`）→ 首登即建租户并进入；控制台可见 `dsh_enable` 审计
   （actor=`dshgw-auto`）；
3. 控制台「停用 DSH」→ 同一身份再登被拒，且不会被自动重新启用；
4. 有 2 把 Key 的账号分别走 Key 登录与飞书登录 → 两次都出现选择页；审计里能看到所选 Key 名称；
5. 升级验证：迁移日志的 `migrated` 数 = 升级前 `api_keys.feishu_open_id <> ''` 的行数（冲突除外），
   迁移后 `api_keys.feishu_open_id` 为空，账号级身份可正常登录。

## 12. 实现与设计差异

（实现完成后回填。）
