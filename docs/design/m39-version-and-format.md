# M39：对外版本号（`/version` + 控制台角标）与 `response_format` 语义修正

## 1. 目标

两件事被排在同一个里程碑里，因为它们由同一次线上反馈带出来：

1. **版本可读**：现在判断线上跑的是哪个版本，只能去 `journalctl` 翻启动日志里的 `version=56df9b5`
   ——一个 commit 前缀。要回答「线上是 0.3.0 吗」「这个后端是不是我发布的那个 revision」，
   必须有一个**机器可读的端点**和一个**人眼可见的位置**。
2. **`response_format` 的语义修正**：线上 gpt001 的 `deepseek` 供应商配了
   `config.response_format="json_object"`，而 `openai-chat` 把这个值**无条件下发**给每个上游请求
   （`openaichat.go:477`），于是**所有**流量都变成 JSON 模式。DeepSeek 对不含 "json" 字样的提示词
   直接 400：`Prompt must contain the word 'json' in some form to use 'response_format' of type
   'json_object'`。真实后果：DSH 选 `deepseek-flash` 后第一轮就 `upstream_400` 失败
   （`docs/TODO.md` 在 M17/M10d 两处已把这笔账记下，但代码没改语义、配置又被加回去了）。

### 成功标准

| # | 标准 |
|---|---|
| 1 | `GET /version` 返回 `{"version":"<a.b.c>","revision":"<短 sha>"}`，**无需鉴权**、带 `base_path` 前缀 |
| 2 | `GET /healthz` 同时带上 `version` 与 `revision`（既有字段不变） |
| 3 | 控制台侧边栏左上角 `AI Gateway` 旁显示 `v<a.b.c>` 与短 revision（两格分开显示） |
| 4 | `make build` 产出的二进制 `-version` 输出 `a.b.c` 形态的版本号（不再回落成 commit 前缀） |
| 5 | `response_format` 只在**请求真的要 JSON** 时下发；普通流量（无 `text.format`）不带该字段 |
| 6 | 客户端声明 `text.format` 且该模型未申报对应能力 → 路由层拒绝（不再静默 400/降级） |
| 7 | 线上 deepseek 供应商去掉该配置后，DSH 选 `deepseek-flash` 能正常出字 |

## 2. 关键决策

| # | 决策 | 理由与取舍 |
|---|---|---|
| D1 | 版本号格式定为 **`a.b.c` 语义化版本**，真值放仓库根的 `VERSION` 文件 | 版本号是**发布事实**，不是 git 派生物：`git describe` 在没有 tag 的仓库里只会给出 commit 前缀（线上现状 `version=d637209`）。一个被跟踪的文件让「这个二进制是什么版本」在 `git log` 里可追溯，也让 `VERSION` 的改动成为发布那一步的显式动作 |
| D2 | revision 取 `git rev-parse --short HEAD`（7 位），**单独一个字段** | 版本号回答「发布了几次」，revision 回答「是不是我提交的那份代码」。两者混成一个字符串就没法比较 |
| D3 | `/version` 与 `/healthz` 都**不鉴权** | 探针与运维脚本要能读到；且版本号不是秘密（二进制里本来就有）。revision 会暴露一个 sha，但仓库一旦开源它就是公开信息——这是刻意的取舍 |
| D4 | 控制台的版本角标在**登录后的外壳**里渲染，不走 `/stats` | 角标属于"这一份前端是什么版本、连的是哪个后端"，与账户无关；`/stats` 要管理员凭据，登录页拿不到，角标就会缺一块 |
| D5 | 记不住 `a.b.c` 的构建**不假装**有版本号：`VERSION` 缺失/非法 → 构建时回落到 `0.0.0` 并告警 | 「沉默地给出一个假版本」比「显式地说没有版本」更坏（运维会照着假版本回滚） |
| D6 | `config.response_format` 保留，但语义**只降级为能力申报** | 删掉这个键会让老部署的配置静默失效；保留它并在真实请求里生效的能力门槛上使用，语义就与 `docs/api-providers.md` 的表述一致了（那里早就写明它是"上游真实支持到哪一档"） |
| D7 | `text.format` 的档位**按级校验**：`json_object` 要 `json_object` 能力，`json_schema` 要 `json_schema` 能力 | 现在 `featuresOf` 把任何 `text.format` 都算成 `json_schema`，而 deepseek 申报的是 `json_object` → 一个合法请求会被判成"没有可用供应商"。按级校验后，申报了什么就能服务什么 |
| D8 | 非法 `text.format.type` 在**解析层**就 400，不静默吞掉 | 静默忽略等于把客户端的结构化输出请求变成普通文本返回，客户端只能自己二次解析；早失败更便宜 |

### 2.1 为什么版本号不能只靠 git tag

`git describe --tags --always` 在没有 tag 时退化成短 sha（线上正是这样），而"有 tag"依赖发布者记得打 tag。
把真值落在 `VERSION` 文件后：

- `make build` 永远有 `a.b.c`（本地开发也是），revision 仍然如实来自 git；
- 发布 = 改 `VERSION` + 提交 + `git tag v$(cat VERSION)`，tag 只是**索引**，不是真值；
- `VERSION` 与 tag 不一致时以 `VERSION` 为准（它跟着 commit 走，tag 可能被打在别的 commit 上）。

## 3. 接口

### 3.1 端点

```
GET /version            （带 base_path 前缀，例如 /aigw/version）
200 {"version":"0.1.0","revision":"56df9b5"}
```

```
GET /healthz
200 {"status":"ok","version":"0.1.0","revision":"56df9b5"}
```

两个端点都不鉴权、不读库、不读注册表——它们是探针，必须在数据库还没就绪时也能答。

### 3.2 构建注入

```make
VERSION ?= $(shell cat VERSION 2>/dev/null || echo 0.0.0)
REVISION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -X main.version=$(VERSION) -X main.revision=$(REVISION) -X main.date=$(DATE)
```

`main.commit` 重命名为 `main.revision`：字段叫 commit 时，控制台要显示"revision"就得做一次翻译，
而这条信息在两个地方出现（端点与控制台），翻译只会带来不一致。`-version` 的输出同步改成
`aigw 0.1.0 (revision 56df9b5, built …)`。

### 3.3 控制台

`renderShell()` 里 `el('div', {class:'brand'}, [名字, 版本角标])`：

```
AI Gateway  v0.1.0 56df9b5
```

- 版本角标是**一个模块级 Promise 缓存**（`versionBadge()`），只请求一次 `/version`；
- 请求失败时角标留空（不弹错、不阻塞外壳）——版本号读不到不该让控制台不能用；
- 角标用等宽字体 + muted 色，避免与品牌名抢注意力。

### 3.4 `response_format` 的下发规则

| 请求 `text.format` | 下发到上游的 `response_format` | 能力门槛 |
|---|---|---|
| 缺省 | 不下发 | 无 |
| `{"type":"text"}` | 不下发 | 无 |
| `{"type":"json_object"}` | `{"type":"json_object"}` | `json_object` |
| `{"type":"json_schema", ...}` | 原样透传整个对象（含 `schema`/`name`/`strict`） | `json_schema` |

`config.response_format` 不再进入请求体，只在构造时校验取值合法（`text|json_object|json_schema`），
并作为**该供应商能力的上限**记录在配置里供人阅读。这样 `docs/api-providers.md:42` 的表述
（"上游真实支持到哪一档：`text`（不下发）/`json_object`/`json_schema`"）与实现终于一致。

## 4. 数据流

```
客户端 ──POST /v1/responses {"text":{"format":{"type":"json_object"}}}──▶ responses.Parse
        （D8：非法 type 在此 400）
   └─▶ httpapi.featuresOf  → features{json_object:true}（D7）
        └─▶ routing.Plan 按候选模型 capabilities 过滤（未申报 → 排除/降级）
             └─▶ pluginapi.Request.Text.Format（原样 JSON）
                  └─▶ openaichat.renderBody → response_format（§3.4 表）
                       └─▶ 上游 /chat/completions
```

## 5. 异常与边界

| 情形 | 行为 |
|---|---|
| `VERSION` 文件缺失 | `make build` 用 `0.0.0`，二进制 `-version` 打印 `0.0.0`（D5） |
| `VERSION` 内容非法（`1.2`、`v1.2.3`、空行） | Makefile 拒绝构建并说明原因，不静默替换 |
| 仓库没有 git（源码包解压） | revision = `none`，版本号仍可用 |
| `text.format` 不是对象 / `type` 未知 | 400 `invalid_request`，`param=text.format` |
| `text.format.type=json_schema` 但上游是 `/chat/completions` | 由能力申报挡住（该模型不申报 `json_schema` 就不会被选中） |
| 老部署仍配着 `response_format: json_object` | **不再影响普通流量**（这正是本里程碑要修的），只有显式要 JSON 的请求才下发 |

## 6. 测试策略

| 层 | 用例 |
|---|---|
| `responses.Parse` | 非法 `text.format.type` 400；合法三档原样保留 |
| `httpapi.featuresOf` | 无 `text` → 无 feature；`json_object` → 要求 `json_object`；`json_schema` → 要求 `json_schema` |
| `openaichat.renderBody` | 无 `text.format` → 请求体**不含** `response_format`（回归：修的就是这条）；`json_object` → 带；`json_schema` → 原样透传；`config.response_format=json_object` 不再强制下发 |
| `httpapi` | `GET /version` 200 + 两个字段；`/healthz` 带 revision；带 base_path 前缀时同样可达 |
| 控制台 | `make ui-check`（headless firefox）断言角标出现版本号与 revision |

**变异验证**（确认测试不是空转）：把 `renderBody` 里的"按下发档位"改回"按配置下发"，上表第 2、4 行的
用例必须失败。

## 7. 依赖

- 无新依赖。`VERSION` 是新增的仓库文件，`Makefile` 与 `scripts/release.sh`（发布 skill 调用）读它。
- 规格文档同步：`docs/api-providers.md`（§2 表格 + §3 片段）、`README.md`（入口表 + 状态）。

## 8. 实现与设计差异

（实现完成后回填）
