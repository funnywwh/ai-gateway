# M18 设计：控制台展示供应商配置说明（内建 kind 的字段文档）

> 触发：操作者反馈「没有配置说明，用户不知道怎么配置」。现状是控制台「模型供应商」只有两个裸 JSON 文本框
> （`config` / `credentials`）与一行 hint；内建 kind 支持哪些字段、默认值是什么、**密钥该填哪一栏**，
> 只能去读 `docs/api-providers.md` 或源码。本文把「说明」变成供应商包自带的数据，由管理面返回、控制台渲染。
> 本文在编码前输出，实现后回填第 10 节差异。

## 1. 目标与非目标

**目标**

1. 「模型供应商 → 详情」展示当前 kind 支持的**全部**配置项：字段路径、类型、默认值、取值范围、是否必填、说明、示例；
2. 同时展示**凭据**字段，并明确回答「密钥填哪里」：凭据通道是首选，`config.api_key` 是明文且在建实例时优先（陷阱）；
3. 「新建供应商」之前就能看到内建 kind 的说明与可复制的配置模板（不需要先建一个再删）；
4. 说明与实现**不可漂移**：新增/改名一个配置字段而忘记写说明，测试必须失败。

**非目标**

- 不做由 schema 生成的完整表单与前端字段校验（见 §9，单独立项）；
- 不改数据面、不改插件协议、不加数据库迁移、不加配置项、不新增依赖；
- 不为「看文档」启动插件子进程；
- 不把 `docs/api-providers.md` 的语义细节复制进代码：schema 只做**字段级事实**（名字/类型/默认/取值/一句话说明），
  DeepSeek 语义、错误分类、思考链路这些散文仍在规格文档里，schema 的 note 里给出指引。

## 2. 关键决策

### 2.1 说明与实现同源：单一事实来源是供应商包

字段在哪里解析、在哪里校验、默认值在哪里给，说明就写在哪里——`internal/providers/<kind>/schema.go`。
理由：仓库现状已经有「文档表格与代码脱节」的先例（本次投诉正是其结果）；把说明放进 `docs/` 或 `httpapi` 的常量表，
下一个新增字段的人不会想到去那里改。放在供应商包里，改 `Config` 的人就在同一个目录看到 schema。

### 2.2 复用插件已有的 schema 形状，而不是新造 DSL

插件 handshake 早已用 JSON Schema 子集描述自己的配置与凭据（`examples/provider-codex`：`type` / `properties` /
`description` / `enum` / `items` + `x-secret` / `x-advanced`），`pluginapi.Handshake` 与
`runtime.ProbeResult` 也已经把它带到管理面。内建 kind 用**同一种形状**，控制台一套渲染逻辑同时服务两类供应商，
将来插件作者补了 description，界面立刻受益。新增扩展只有四个，都是渲染需要、且插件侧将来可选的：

| 扩展 | 位置 | 含义 |
|---|---|---|
| `default` | 标准 JSON Schema | 字段默认值（渲染「默认」列，并作为模板骨架的来源） |
| `x-required` | 自定 | 必填（`base_url` 不填供应商构建直接失败） |
| `x-advanced` | 已有（插件侧） | 折叠到「高级」，默认不展开 |
| `x-prefer-credential` | 自定 | 该值经凭据通道下发更合适，值 = 凭据里的键名（如 `api_key`） |

不引入 `x-order`：JSON 对象键序在 Go 的 `json.RawMessage` 与 JS 对象里都按书写顺序保留，书写顺序即渲染顺序。

### 2.3 反射防漂移：`Config` 的 json tag 集合 == schema properties 集合

`internal/providers/schema_test.go` 用 `reflect` 取每个内建 `Config` 的 json tag（去掉 `,omitempty`），与 schema 的
`properties` 键做**双向差集**，任一方向非空即失败。这是本轮唯一能长期防住「没有配置说明」的机制：字段是代码的一部分，
说明必须是同一个提交的一部分。模板（`Template()`）的键同样断言 ⊆ schema 键，防止模板里写出不存在的字段。

### 2.4 内建 schema 是编译期常量，详情接口绝不启动插件

内建 kind 的 schema 直接内嵌在二进制里，`GET /providers/{id}` 返回它，零副作用。插件的 schema 只能由 handshake 提供，
而 handshake 需要拉起子进程（`runtime.ProbeResult` 里已有这两个字段）。因此：

- 内建 kind：详情页直接渲染；
- `plugin:<name>`：详情页显示「配置说明由插件在握手时声明」+ 一个显式按钮「读取插件声明（会启动/连接该插件进程）」，
  复用既有 `POST /providers/{id}/test?mode=info`；
- **不**在打开详情页时隐式探测——打开一个页面不应该有进程生命周期副作用。

### 2.5 新增 `GET /admin/api/v1/provider-kinds`

「新建」是操作者第一次面对空 JSON 框的时刻，说明必须在那之前可达。该端点返回全部内建 kind 的
note + config schema + credentials schema + 模板；同时它给未来的「kind 下拉框」与外部集成（脚本化建供应商）一个稳定的发现入口。
插件 kind 不进这个列表（它们随实例存在，不是一个封闭集合）。

### 2.6 文案语言

面向操作员的文案（`description`/`note`/模板注释）用**中文**，与既有控制台文案（「凭据（JSON，只写不回显）」等）一致；
标识符、代码注释、JSON 键保持英文。插件 schema 的文案由插件作者决定，界面原样展示、不翻译。

### 2.7 本轮只做「看得懂 + 可复制」

schema→表单生成（含 `models[]` 数组编辑器、凭据掩码输入、逐字段校验）是 M9 承诺过但未落地的大项，
它与本轮的痛点（**不知道有哪些字段、密钥填哪**）不是同一件事，且在没有前端测试环境（见 §7）的情况下风险不成比例。
本轮交付：字段表 + 备注 + 一键复制模板 + 现状 JSON 框。

## 3. 接口

### 3.1 每个内建供应商包（新增 `schema.go`）

```go
// Schema returns the JSON Schema subset describing this kind's provider config and
// credentials. Descriptions are operator-facing Chinese text: the admin console
// renders them verbatim.
func Schema() (config, credentials json.RawMessage)

// Note is the kind-level prose shown above the field table: what this kind talks to,
// what it cannot do, and which spec document explains the semantics.
func Note() string

// Template returns a config skeleton carrying every field an operator must fill in,
// so "create from template" produces a valid starting point.
func Template() json.RawMessage
```

（返回 `json.RawMessage` 而不是 `providers.KindSchema`，避免子包反向依赖父包。）

### 3.2 `internal/providers/registry.go`

```go
type KindSchema struct {
	Kind        string          `json:"kind"`
	Note        string          `json:"kind_note,omitempty"`
	Config      json.RawMessage `json:"config_schema,omitempty"`
	Credentials json.RawMessage `json:"credentials_schema,omitempty"`
	Template    json.RawMessage `json:"config_template,omitempty"`
	Source      string          `json:"schema_source"` // builtin | plugin | unknown
}

func Schemas() []KindSchema            // 内建 kind 全集，按 kind 排序
func SchemaFor(kind string) KindSchema // 未知/插件 kind → Source=unknown + 指向插件 handshake 的 note
```

### 3.3 管理面（`internal/httpapi`）

| 端点 | 变化 |
|---|---|
| `GET /admin/api/v1/provider-kinds` | **新增**，`{"data": [KindSchema…]}` |
| `GET /admin/api/v1/providers/{id}` | `providerJSON` **增** `config_schema` / `credentials_schema` / `kind_note` / `schema_source`（内建即有；插件为 null + `schema_source=plugin`） |
| `POST /admin/api/v1/providers/{id}/test` | 不变（插件 schema 的唯一来源，`probe.go` 已返回） |

`internal/httpapi` 已在分层表的允许依赖里包含 `internal/providers`（`internal/arch/layering_test.go`），**不新增分层边**。

## 4. 数据流

```
打开「模型供应商」
  └─ GET /providers                     列表（不变）
       └─ 点「内建类型说明」→ GET /provider-kinds → 说明弹窗（note + 字段表 + 模板 + 「用此模板新建」）

点「详情」
  └─ GET /providers/{id}  →  schema_source=builtin → 渲染「配置说明」区块
                          →  schema_source=plugin  → 提示 + 「读取插件声明」按钮
                                                        └─ POST /providers/{id}/test?mode=info
                                                             → handshake.config_schema / credentials_schema
```

## 5. 控制台渲染（`internal/webui/static/js/pages/providers.js`）

1. 列表卡工具栏新增「内建类型说明」按钮 → 自建弹窗（沿用 `detail()` 的 `el('div',{class:'modal-backdrop'})` 写法）：
   每个内建 kind 一个分区：`note` → 配置字段表 → 凭据字段表 → 配置模板（`jsonBlock`）+「用此模板新建」（带着 kind 与模板打开既有的新建弹窗）；
2. 详情弹窗在「配置」jsonBlock **之前**插入「配置说明」区块：
   - `builtin`：note + 字段表（基本字段）+ 折叠的「高级」字段 + 凭据字段表 + 「复制模板」；
   - `plugin`：提示文案 + 「读取插件声明」按钮（点击后把返回的两个 schema 就地渲染成同样的字段表）；
   - `unknown`：`kind` 不是内建也不是 `plugin:` 形式时的明确报错文案；
3. 字段表列：`字段`（等宽）/ `类型` / `默认值` / `说明`；`x-required` 加 `badge('必填','warn')`，
   `x-prefer-credential` 加 `badge('密钥')`，`x-advanced` 的行进「高级」子表；
4. 数组字段（`models[]`）一行显示 `models[]`，说明列写清元素字段（`public`/`upstream`/`context_window`/`max_output_tokens`/`capabilities`）；
5. 全部复用 `el` / `card` / `table` / `badge` / `jsonBlock` / `toast` 原语，不新增库、不加构建步骤（资源内嵌在二进制里，改完要重启网关才生效——既有事实，README 已述）。

## 6. 异常与边界

| 情况 | 行为 |
|---|---|
| `kind` 是 `plugin:<name>` | 详情页显示插件提示与按钮，`config_schema` 为 null；**不**隐式启动进程 |
| 插件未运行/启动失败 | 按钮点击后 `test` 返回 `ok:false` + `error`，用 `toast` 原文展示（沿用 `probe()` 的错误处理风格） |
| schema 不是合法 JSON | 后端测试兜底（不应发生）；前端 `JSON.parse` 失败时降级为「只显示 note」，不抛异常中断弹窗 |
| 模板键与 schema 不一致 | 单测失败（§7-5） |
| `navigator.clipboard` 不可用（非安全上下文/旧浏览器） | 「复制模板」回退为把模板写入配置框并提示「已填入，请手动复制」 |
| 超长说明 | 说明列 `white-space: pre-wrap` + 表格自身横向可滚（沿用现有样式） |
| 未知 kind（拼写错误） | 「配置说明」区块显示 `unknown` 提示；既有行为不变（构建实例时才报错） |

## 7. 测试策略

`internal/providers/schema_test.go`（新增）：

1. `Schemas()` 覆盖 `BuiltinKinds()` 全集（新增 kind 忘了 schema → 红）；
2. 每个 kind 的 note / config schema / credentials schema / template 均非空，且 schema 可被 `encoding/json` 解析；
3. **反射防漂移**：`openaichat.Config`、`openairesponses.Config`、`testecho.Config` 的 json tag 集合 == 对应 schema 的
   `properties` 键集合（双向差集为空）；
4. 字段合法性：`type` ∈ {object,string,integer,number,boolean,array}；有 `default` 时其 JSON 类型与 `type` 一致；
   有 `enum` 时 `default` ∈ `enum`；`x-required` 只允许出现在已声明的属性上；`x-secret` 只允许出现在 credentials schema；
5. template 键 ⊆ config schema 键；`models[]` 元素键 ⊆ 其 `items.properties` 键；
6. `SchemaFor("plugin:whatever")` → `Source=unknown` 且 note 提到插件 handshake；`SchemaFor("nope")` → 同样不 panic；
7. credentials schema 至少声明一个字段并含 `api_key`（内建三类都吃凭据通道的 `api_key`），
   且 `api_key` 在 config schema 里带 `x-prefer-credential: "api_key"`——这条把「密钥怎么配」固化成断言。

`internal/httpapi/admin_test.go`（新增 2–3 例）：

- `GET /admin/api/v1/provider-kinds` 200，`data` 含三个内建 kind 且 `config_schema` 非空；
- `GET /admin/api/v1/providers/{id}`（内建 testecho）返回非空 `config_schema` 与 `schema_source=builtin`，且响应里仍无凭据明文；
- 建一个 `plugin:does-not-exist` 供应商后取详情：`schema_source=plugin`、`config_schema` 为 null、**没有**子进程被启动
  （断言 `state_dir` 下未生成进程态文件/未报 handshake 错误）。

前端：本环境**无 node/npm**（`Makefile` 的 `test-race` 注释与 TODO 的 M9 条目都记录了这点），故不新增 JS 单测；
约束是「渲染函数保持纯数据 → DOM 映射 + 只用既有原语」，并在隔离实例（`:8099` + 库快照 + 独立 `GW_PLUGINS_STATE_DIR`，
沿用 M10c 的走查方式）对三个内建 kind 与一个插件 kind 各人工核对一次。

`make verify` 全绿（vet + 全量测试 + build + 分层断言）。

## 8. 依赖与假设

- 无新依赖、无迁移、无配置项变更；
- 假设控制台是唯一消费方（`provider-kinds` 也可以被脚本消费，但本轮不承诺对外稳定性）；
- 假设插件作者用英文写 description（`examples/provider-codex` 现状），界面原样展示；
- 假设 `docs/api-providers.md` 继续承载 openai-chat 的语义细节，schema 的 note 指向它而不是复制它。

## 9. 暂不纳入

- schema → 表单生成与服务端校验（M9 §6 承诺过「插件 schema → 表单」，实际只落地了 JSON 文本域；本轮补「说明」，表单单独立项）；
- `models[]` 的表格化编辑器（手写 JSON 仍是唯一入口）；
- 对外的配置文档页（`/docs`）与 `provider-kinds` 的公开版本；
- 服务端在保存时按 schema 校验配置（当前仍是构建期校验：`base_url` 必填、枚举非法即失败）；
- 把凭据字段做成掩码输入与「轮换」流程。

## 10. 实现与设计差异

1. **`KindSchema.Note` 的 JSON 名统一为 `kind_note`**（设计里写的是 `note`）：`/provider-kinds` 与
   `/providers/{id}` 的详情是同一份 schema 的两个出口，字段名必须一致，否则控制台两处要写两种读法。
2. **不做剪贴板按钮**：模板改为 `<details>` 展开 + `jsonBlock` 展示，再配一个「用此模板新建」直接预填创建表单。
   少一条依赖非安全上下文的分支（`navigator.clipboard` 在 http 下不可用），也少一条未测交互。
3. **插件 schema 的来源比设计更宽**：详情接口在 `discovered` 里已有上次握手的 schema 时**直接复用**
   （`providerDocsJSON`），因此探测过一次之后打开详情就能看到字段表；「读取插件声明」按钮只在没有任何记录时出现。
   仍然**不启动任何进程**（httpapi 测试断言了 `Prober.Probe` 调用次数不变）。
4. **新增未声明 schema 的兜底**：插件既没 schema 也没记录时，按该实例 `config` 里实际出现的键列一行键名
   （`keyList`），并写明这只是「当前配置」而不是「支持的字段」。设计讨论里把它列为可选项，本轮做了。
5. **凭据的必填来自 schema 级 `required` 数组**（不是逐字段的 `x-required`）：前端把 schema 级的 `required`
   折进对应的行再做标记，标准写法的插件也能被正确渲染。
6. **列表端点保持精简**：schema 只在详情 / 创建 / 更新三个响应里（`providerDetailJSON`），
   `GET /providers` 不带——否则列表响应会随字段说明膨胀。
7. **验证方式与原计划不同（本次最有价值的偏差）**：原计划是「无 node → 人工走查」。实现期发现本机有
   firefox，于是做成真实的浏览器自动化：`scripts/ui-harness/`（编译期产物 + API 快照 + headless firefox）
   与 `make ui-check`，5 个视图 40 项断言，覆盖字段表、模板预填、插件握手按钮。踩到三个坑并写进该目录 README：
   `--screenshot` 的产物在本机不可信（各视图 PNG 字节相同）、页面在截图后会被拆掉（等待必须是纯微任务）、
   同步 XHR 的回报会被丢弃（改用 `sendBeacon`）。
8. **测试落地**：`internal/providers/schema_test.go` 6 例（kind 全覆盖、反射双向防漂移、schema 合法性、
   模板键、凭据 schema、`x-secret` 只能在凭据里），其中「加字段不写说明」与「删 description」两处用**变异验证**
   过——各自精确失败；`internal/httpapi/admin_test.go` 增 2 例（`/provider-kinds` 契约、详情带 schema 且不启动插件、
   复用已记录 schema）。
9. **模板内容是通用起点**：`openai-chat` 的模板给了 DeepSeek 的 `base_url` + 一个 `deepseek-flash` 模型条目，
   但**不含 `thinking`**——通用模板不能默认替所有上游打开 DeepSeek 方言；DeepSeek 的完整片段在
   `docs/api-providers.md` §3 与 `config.example.yaml`，kind note 里已指向。
10. **`testecho` 的凭据 schema 声明「不使用凭据」而不是留空**：界面因此能明确告诉操作者「填了也会被忽略」，
    测试也断言了这句话存在。
