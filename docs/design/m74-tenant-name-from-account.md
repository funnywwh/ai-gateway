# M74 设计文档：租户名自动使用 `dsh-<账号拼音>-<账号ID>`

> 状态：**已实现（M74，代码、单测与浏览器 harness 完成；真机走查见 docs/TODO.md）**。
> 上游：启用流程见 [M52](m52-dsh-enable.md)，飞书首登自动开通见 [M62](m62-feishu-auto-enable-dsh.md)；
> 规格：[docs/dshgw.md](../dshgw.md)、[docs/org.md](../org.md)。
>
> 需求原话：「租户名自动用 `dsh-<账号>` 格式」（对话中进一步确认：`dsh-账号拼音-id`，弹窗预填但可手改）。

## 1. 目标

控制台对一个**中文名账号**点「启用 DSH」时，租户名应该是可读、可辨认、天然唯一的
`dsh-<账号名拼音>-<账号ID>`：

| 账号（name / id） | 现在的候选 | 本里程碑的候选 |
|---|---|---|
| 陈景峰 / 10 | `dsh-tenant`（中文全被丢弃，落进共享兜底名） | `dsh-chenjingfeng-10` |
| 杨妙 / 36 | `dsh-tenant` | `dsh-yangmiao-36` |
| 李智超(colin) / 8 | `dsh-colin` | `dsh-lizhichao-colin-8` |
| acme / 7 | `dsh-acme` | `dsh-acme-7` |
| !!! / 5 | `dsh-tenant` | `dsh-tenant-5` |

现状（M52）`dshTenantSlug(accountName)` 只保留 ASCII、其余折叠成 `-`，于是**所有中文名账号的候选
都退化成同一个 `dsh-tenant`**；真实部署里 `dsh-chengjinfeng` / `dsh-yangmiao` / `dsh-lianchangliang`
都是操作员在弹窗里手敲的拼音。本里程碑把这件事变成自动的，并且把"重名"从"靠人去重"变成"靠 id 天然不重"。

## 2. 关键决策

### D1 生成规则只有一份实现：服务端

控制台不再自己拼名字（现状 `accounts.js` 与 `org.js` **各有一份** `slugFromAccount`），改为服务端在
账号行的 JSON 里下发候选名 `dsh_tenant_suggested`，弹窗只是预填它。取舍：

* 收益：拼音表只在服务端被用到（控制台不必为一个预填加载 112KB 的 `pinyin.js`）；不存在"JS 与 Go
  规则漂移"这一整类缺陷；飞书首登自动开通（没有浏览器参与）与按钮路径**必然**同名。
* 代价：管理 API 多一个只读字段（`/accounts` 与 `/org/nodes/{id}/accounts`）。两个字段都是纯新增，
  旧控制台对新服务端、新控制台对旧服务端都只是少一个预填值，不会报错。

### D2 拼音表生成两份，出自同一个脚本

`scripts/gen-pinyin.py` 现在同时输出：

* `internal/webui/static/js/pinyin.js`（既有，控制台过滤用，**输出必须逐字节不变**）；
* `internal/pinyin/table_gen.go`（新增，Go 侧取名用）。

两份是**同一个 blob**（同一个来源文件、同一个 SHA-256、同一行序），`internal/pinyin` 的测试断言两者
逐字节相等——只改一边就红。取舍与 M50 的压缩混淆有关：发布二进制里的 JS 副本会被 esbuild 压缩，
从产物里读回表既脆弱又依赖构建形状；编译期常量没有这个问题。

### D3 控制台自己的租户名校验放宽到与 dshgw **完全相同**

`internal/httpapi/admin_catalog.go` 的 `dshTenantNameRE` 一直自称"mirrors dshgw's
`config.ValidTenantName`"，实际比它严：dshgw 允许以数字结尾（`^[a-z][a-z0-9-]{0,25}[a-z0-9]$`），
控制台要求以字母结尾。新规则的名字**以 id 结尾**，用现状表达式会被自己拒掉；因此把控制台表达式改成
与 dshgw 逐字符相同，并用交叉断言钉住（每个生成名都要通过 `config.ValidTenantName`）。副作用：操作员
现在可以手填以数字结尾的租户名——那本来就是 dshgw 接受的名字。

### D4 多音字取表里的第一个读音

`长` 有 `chang`/`zhang`，`曾` 有 `ceng`/`zeng`。取名是**确定性**需求，不是搜索需求：取表里第一个读音
（与 `pinyin.js` 的约定一致），不引入姓名专用词典。`pinyin.js` 的多音字枚举是给过滤框用的，取名不用它。

### D5 既有租户名一律不改

租户名是 dsh 主目录、工作区、registry 与端口映射的键，改名等于换一个租户。因此：

* `accounts.dsh_tenant` 非空时仍然优先（粘住）；
* 弹窗里显式填的名字仍然优先（这是既有语义，也是运维改名/纠正的唯一入口）；
* 本里程碑**不做**重命名、不做历史回填。`dsh-tenant`、`dsh-colin` 这些老租户保持原样。

### D6 截断保 id、按音节截断

`ValidTenantName` 的实际长度上限是 **27 字符**（`[a-z]` + 最多 25 + `[a-z0-9]`）。名字预算是
`27 - len("dsh-") - len("-<id>")`；超长时按**整音节**截断（放不下下一个读音/字符就停，再 `TrimRight("-")`），
**id 后缀永不截断**。这样名字永远唯一，也永远不会出现半个拼音。

## 3. 接口

```go
// internal/pinyin —— 叶子包，只依赖标准库；表是生成文件（勿手改）。
//
// FirstReading 返回一个汉字的 tone-stripped 首读音（ü 写成 v），表外字符返回 ""。
func FirstReading(r rune) string

// internal/httpapi/admin_catalog.go
//
// dshTenantNameForAccount 返回"操作员没给名字时"该账号的租户名候选：
// "dsh-" + 账号名的拼音/ASCII slug + "-" + 账号 id。
func dshTenantNameForAccount(a *domain.Account) string

// dshTenantNameRE 与 dshgw 的 config.ValidTenantName 是同一个表达式。
```

slug 规则（逐 rune 处理 `strings.ToLower(a.Name)`）：

| 输入字符 | 输出 |
|---|---|
| `a-z` / `0-9` | 原样 |
| CJK（表内有读音） | 该字的首读音（可能是多个字母，或 `v`） |
| 其它任何字符（标点、空白、表情、表外汉字） | 一个 `-`（连续折叠，不做前导 `-`），最后 `Trim("-")` |
| slug 为空 | `"tenant"`（继承今天的兜底词，于是 `!!!` → `dsh-tenant-5`） |

管理 API 新增字段：

| 端点 | 字段 | 值 |
|---|---|---|
| `GET /admin/api/v1/accounts` | `dsh_tenant_suggested` | 规则名（**总是**有值，已启用账号也给规则名） |
| `GET /admin/api/v1/org/nodes/{id}/accounts` | 同上 | 同上 |

控制台取值顺序不变：`dsh_tenant || dsh_tenant_suggested || ''`——已有映射照旧预填，让人一眼看到"不会改名"。

## 4. 数据流

```
操作员点「启用 DSH」
  → 控制台页面从账号行读 dsh_tenant（已有映射）或 dsh_tenant_suggested（规则名）预填弹窗
  → POST /admin/api/v1/accounts/{id}/dsh {enabled:true, tenant?}
      → provisionAccountDSH(requested)
          requested 非空 ? 用它 : (a.DshTenant 非空 ? 用它 : dshTenantNameForAccount(a))
          → 校验 dshTenantNameRE → 唯一性 → 铸 worker Key → tenant-create/set-key+start
          → 写 accounts.dsh_tenant/dsh_enabled → 审计 dsh_enable{tenant} → reload

飞书首登（无人参与）
  → dshgw authorize → aigw 自动开通 → provisionAccountDSH(requested = nil)
  → 同一条规则 → 同一个名字
```

## 5. 异常与边界

| 情况 | 行为 |
|---|---|
| 账号名为空/纯符号/纯表情 | slug = `tenant` → `dsh-tenant-<id>` |
| 拼音 slug 超预算 | 按整音节截断，id 后缀保留，总长 ≤27 |
| 账号 id 极大（预算 <4） | slug 退化为 `t` → `dsh-t<id>`，仍在 27 内 |
| 生成名不合规（理论上不可达） | 退化为 `dsh-tenant-<id>`，并仍由 `provisionAccountDSH` 的校验兜底 |
| 生成名被另一个账号的 `dsh_tenant` 占用 | 沿用现状：400 `tenant name is already used by another account`（id 后缀让这条路基本不可达） |
| dshgw 里已存在同名租户 | 沿用现状：按"重新启用"处理（`SetTenantKey` + `StartTenant`） |
| 账号被停用/关闭 | 沿用现状：400 `suspended or closed accounts cannot enable dsh` |
| 在弹窗里手改成**另一个**名字 | 现状不变（新建租户，旧租户留在 dshgw、其 worker Key 被吊销，旧数据仍在旧租户目录）——**已知限制，本里程碑不修** |
| 表外汉字的账号名（CJK 扩展区） | 该字当分隔符（`pinyin.js` 的同一条已文档化限制） |

## 6. 测试策略

* `internal/pinyin/pinyin_test.go`：行数 20924；抽查读音；**与 `pinyin.js` 的 `READINGS` 逐字节相等**。
* `internal/httpapi`：规则表（含上表 §3 的五个例子 + 超长名 + 纯符号名）、每个结果都通过
  `config.ValidTenantName`（交叉断言）、启用流程（中文名 → `dsh-<拼音>-<id>`）、飞书自动开通同名、
  既有映射粘住、显式指定仍生效、两个 JSON 端点都带 `dsh_tenant_suggested`。
* `internal/webui/tests/*.mjs`：文本断言两个页面不再自带 `slugFromAccount`，且都用 `dsh_tenant_suggested`。
* `scripts/ui-harness`（视图 `org-person`）：中文名 fixture 打开「启用 DSH」后，租户名输入框的初值等于
  该账号行给的 `dsh_tenant_suggested`（浏览器里真走一遍预填；拼音规则本身由 Go 测试负责）。
* 真机：对某个未启用的中文名账号启用一次，核对徽标、审计 `dsh_enable.tenant` 与 `tenant-list`。

## 7. 依赖

* `scripts/gen-pinyin.py` 与它 pinned 的 mozillazg/pinyin-data（MIT，版本与 SHA-256 写在两个生成文件的头部）。
* `internal/dshgw/config.ValidTenantName`（dshgw 侧的同一个约束，测试里交叉引用）。
* 无新第三方依赖；无新环境变量/配置项。

## 8. 实现与设计差异

- **表里没有 `v`**（实现时才发现）：生成脚本用 NFD 剥离声调，变音符号一并被去掉，所以 `ü` 落在 `u` 上
  （女 → `nu`，与 路 同串；全表 20992 行里一个 `v` 都没有）。设计稿最初跟着旧注释写成"ü 写成 v"，
  已改正：**行为不动**（那是控制台过滤的既有数据），只把 `scripts/gen-pinyin.py` 的注释、两个生成文件的
  头注释与 `docs/org.md` 的说法改成与数据一致。同音同形由账号 ID 兜底，这正是本规则要 ID 的原因之一。
- **Go 表是离线生成的**：`scripts/gen-pinyin.py` 的默认路径要下载 pinned 的 `pinyin.txt`，本机取不到
  （超时）。实现时用一次性脚本从**已提交的** `pinyin.js` 里反解出同一个 blob 交给生成器新增的
  `go_body()` 写出 `table_gen.go`——生成器仍是唯一出处，两侧逐字节相等由 `internal/pinyin` 的测试钉住；
  下次有网时正常跑一次脚本，产物应当逐字节不变（`pinyin.js` 本次只改了头注释两行，已核对 diff）。
- **长度上限按 27 取**（`^[a-z][a-z0-9-]{0,25}[a-z0-9]$` 的实际边界）：正数 int64 的 ID 最多 19 位，
  于是留给拼音的预算恒 ≥3，`dsh-` + stem + `-<id>` 永远落在正则内——这条推理写进了注释，并有
  `math.MaxInt64` 的用例钉住。
- **多加了两条断言**：控制台的 `dshTenantNameRE` 与 dshgw 的 `config.ValidTenantName` 交叉验证；
  "操作员手填一个以数字结尾的名字"现在必须被接受（现状是控制台自己 400，而 dshgw 接受）。
- **控制台提示文案**两页都写了规则与例子（`dsh-chenjingfeng-10`）以及"已存在的租户名不会被改动"，
  由 `internal/webui/tests/tenant_name_test.mjs` 的文本断言守住。
- 设计里的"去重循环不动"确实没动：ID 让同音重名基本不可达，保留它只是防御人工建的 dshgw 同名租户。

## 9. 验证

- `go vet` + `go test`（显式四棵树 `./cmd/... ./internal/... ./pkg/... ./examples/...`，本机不能跑
  `./...`，见 TODO 的 M66 小节）：全绿。
- `internal/pinyin`：表与 `pinyin.js` 的 `READINGS` 逐字节相等、行数 20992、读音抽样（含多音字取首读音）、
  每个首读音都是 `[a-z]+`。
- `internal/httpapi`：规则表 12 例、与 `config.ValidTenantName` 的交叉断言、超长名按音节截断且 ID 保留、
  启用流程（中文名 → `dsh-<拼音>-<id>`）、飞书自动开通同名、既有映射粘住、显式名仍优先、以数字结尾被接受、
  两个 JSON 端点都带 `dsh_tenant_suggested`。
- 控制台：`node internal/webui/tests/tenant_name_test.mjs`（新，已挂进 `make ui-base`）+
  `scripts/ui-harness/run.sh` **全 32 个视图通过**，其中 `org-person` 48 项（新增 3 项：弹窗给出启用入口、
  预填值等于 `dsh_tenant_suggested`、提示写明规则）；同一组视图对**压缩镜像**
  （`UI_STATIC_DIR=.cache/ui-dist/static`）复跑也通过。
- `make build`（控制台压缩内嵌）通过。
