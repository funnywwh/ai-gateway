# M97 设计：`uya-agent` 作为一个独立的客户端维度取值

> 状态：**已实现（M97）**。
> 面向使用者的规格：[docs/request-log.md](../request-log.md) §2（身份维度表）。
> 需求原话（2026-10-07）：「识别出来的是 dsh 客户端，要是 uya-agent」。

## 1. 问题

M27 的客户端词表是 `dsh / codex / console / unknown`，**封闭**。`uya-agent`
（本机另一个编码 agent，与 DSH 同方言但**不是** DSH）在这个词表里没有位置：

| 现状 | 结果 |
|---|---|
| 它的正文不是 DSH 的 persona（`You are an AI agent powered by DeepSeek Harness.`） | 正文规则全不命中 |
| 它的 User-Agent 是 `uya-agent/0.1 (deepseek-harness-compatible)` | 含 `deepseek-harness` 子串 ⇒ 被认成 **`dsh`** |
| 若 UA 不含那个子串 | 落 `unknown`，而 `workspace` 只在 dsh/codex 两个分支里取值 ⇒ **工作区一起丢**（M27 §3 的既有形状） |

也就是「客户端列看不出是谁」与「工作区为空」是同一件事的两面。本轮只解决前半：让它
**如实显示成 `uya-agent`**。工作区归主工作区那一半已在 uya-agent 侧（P71）完成 —— 它发
`session workspace: "<归类键>"`，网关照读。

### 成功标准

| # | 标准 |
|---|---|
| 1 | `client=uya-agent` 的请求，工作区、会话、标题识别**与 dsh 同一条路**（读 `session workspace:` 与两个会话头） |
| 2 | 控制台客户端列与筛选下拉都有 `uya-agent`，与 dsh/codex 并列 |
| 3 | 我们的标题调用也记成 `uya-agent`（现在被「标题调用 = DSH」那条兜底吞掉） |
| 4 | 真 DSH 与 Codex 的识别**逐字节不变**（含 `deepseek-harness/…` 那种真 DSH UA） |

## 2. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | **新增第五个取值 `uya-agent`**，不做「把 dsh 改名」也不做「配置化词表」 | 「改名」会把真 DSH 一起改掉（两个客户端本来就不该同桶）；配置化词表是为一个取值引入一套配置面与校验，收益不成比例。取值本身用**连字符**（`uya-agent`）而不是下划线/驼峰：与客户端的自称逐字一致，且它已经出现在 UA 里，运维一眼能对上 |
| D2 | UA 判定**放在 dsh 之前**，并只认 `uya-agent` 这个子串 | 顺序是**载荷相关**的：我们的 UA 里**同时**含 `uya-agent` 与 `deepseek-harness`（后者是给不认 `uya-agent` 的老网关留的兜底），谁在前谁赢。判定用 `uya-agent` 而不是 `deepseek-harness-compatible`：后者是我们自己发明的措辞，前者是客户端的名字 |
| D3 | 工作区/会话取值按**客户端无关**实现：把 `session workspace:`（以及会话头）提到一个共用分支，`dsh` 与 `uya-agent` 都走它 | 这两家在同一套方言上（uya-agent 的设计目标就是「与 DSH 同方言」）。写成 `case ClientDSH, ClientUya:` 而不是复制一段，是因为复制会让「改了一处忘了另一处」成为默认结果 —— 与 M27 §3「提取规则只有一份」同源 |
| D4 | 标题调用：`CallKind==title` 且**正文里没有 DSH 的 persona 前缀**时归 `uya-agent`；有前缀仍是 DSH | 那条兜底的原文是「标题提示词是 DSH 自己的，所以标题调用就是 DSH 调用」——在 uya-agent 逐字复用同一提示词之后，这个推理断了：**提示词相同不代表调用方相同**。用「有没有 DSH persona」当区分判据，是因为它正是 D1 里那条正文规则，且 uya-agent 的标题请求**只带 system + 一条 user**（不含运行时上下文），两者不会混淆 |
| D5 | 不做数据迁移、不回填历史行 | 历史行是既成事实（当时网关确实认成了 dsh/unknown）；改写历史等于伪造当时的判定。`docs/request-log.md` 的既有口径就是「历史行不回填，界面显示未知」 |
| D6 | 不改 `docs/request-log.md` 之外的规格面 | `client` 是自由字符串（store 侧无白名单、管理面无枚举校验，已核对），所以新增取值不需要迁移、不需要新路由、不需要 MCP 工具说明变更（`admin_describe` 的 client 说明要跟着改一行文案，见 §4） |

## 3. 接口

```go
// internal/responses/dimensions.go
const (
    ClientDSH   = "dsh"
    ClientCodex = "codex"
    // ClientUya is uya-agent: a coding agent that speaks DSH's dialect on purpose
    // (same session headers, same `session workspace:` line, same title prompts) but
    // is not DSH. It identifies itself in the User-Agent; nothing in its body is a
    // marker we could match on, which is why adding it here was necessary at all.
    ClientUya     = "uya-agent"
    ClientConsole = "console"
    ClientUnknown = "unknown"
)

func clientFromHint(hint string) string   // 新增一条 case，放在 deepseek-harness 之前
func (r *Request) Dimensions(clientHint string) Dimensions  // 工作区分支改成 dsh|uya 共用
```

客户端取值只在这两处产生（已核对：全仓没有任何把请求里的字符串抄进该列的路径），
所以改动面就是「一个常量 + 两条判定 + 一个分支」。

## 4. 数据流

```
POST /v1/responses
  └─ clientHintFromRequest(r)          UA 前 64 字节（不落库）
       └─ Dimensions(clientHint)
            ├─ 正文规则（codex / dsh persona / title）      ← 与改动前逐字节相同
            └─ clientFromHint 兜底： uya-agent > deepseek-harness > codex > console
  └─ workspace： dsh | uya-agent 都读 `session workspace: "<JSON 路径>"`
       （uya-agent 发的是**归类键**：linked worktree 折回主工作区，见 uya-agent 仓 P71）
```

控制台：请求日志页的客户端下拉加一项 `uya-agent`；`admin_describe` 的 `client` 过滤
说明文案从 `dsh | codex | unknown` 改成四个取值（`console` 本来就没写进去，本次一并补齐）。

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| 真 DSH（UA `deepseek-harness/0.1.2 (+…)`） | 仍 `dsh`（不含 `uya-agent`）——有断言钉住 |
| 我们的旧版本（UA 只有 `uya-agent/0.1`） | `uya-agent`（含子串即可）—— 向后兼容，不必同步升级 |
| 第三方 UA 恰好含 `uya-agent` | 记成 `uya-agent`。这是 UA 兜底的固有性质（任何人可自称 DSH），**不是安全边界**：没有任何权限/计费决定依赖该列 |
| 会话压缩后 runtime context 被替换 | `workspace` 可能空，`session_id` 仍可归组（与 dsh 同一条既有边界） |
| 历史行 | 不回填；那批行仍是 `dsh`/`unknown` |

## 6. 测试策略

- `internal/responses/dimensions_test.go`：
  - `TestDimensionsFallBackToUserAgentHint` 增两条：我们的 UA ⇒ `uya-agent`；真 DSH 的 UA ⇒ 仍 `dsh`；
  - 新增 `TestDimensionsUyaAgentBody`：带 `session workspace:` 的正文 + 我们的 UA ⇒
    `uya-agent` **且 workspace 取到**（这条钉的正是「工作区与客户端同一个 bug」那件事）；
  - 新增 `TestDimensionsUyaTitleCallIsNotDSH`：标题形状的正文（无 DSH persona）+ 我们的 UA
    ⇒ `uya-agent`；**带** DSH persona 的标题形状 ⇒ 仍是 `dsh`（反向断言，钉住 D4）。
- `internal/webui/tests/requests_test.mjs`（`make ui-base` 的一条，**不需要浏览器**）：钉住下拉
  里有 `uya-agent` 这一项。
- `scripts/ui-harness`（真浏览器走查，本机无 firefox 时自动跳过）：`keys.page.html` 的
  `clientShown` 断言把 `uya-agent` 一起算上；fixtures 增一行 `client=uya-agent` 的数据。
- `make verify` 全绿。

## 7. 依赖

无新增包、无数据库迁移、无路由变化。改动落在 `internal/responses`、`internal/httpapi`
（一行文档字符串）、`internal/webui/static/js`、`internal/webui/tests`、`scripts/ui-harness`
与 `docs/request-log.md`。

## 8. 实现与设计差异

1. **§2 D4 的判据比设计更保守**：设计写「`CallKind==title` 且无 DSH persona ⇒ uya-agent」，
   实现改成「先按 DSH（沿用旧默认），**UA 给出了非 unknown 的判定时**才覆盖」。理由是把
   `TestDimensionsReadsDSHTitleCallInBothRoleVariants` 跑红发现的：它传的 hint 是空串，
   按设计那样写会让「DSH 的标题调用 + 自定义 provider 不发 UA」退化成 `unknown` —— 那是把
   一个现有行为改坏。改成「显式 hint 才覆盖」之后，两种情形都对。
2. **§4 说控制台下拉「加一项」，实际连断言一起改了**：`scripts/ui-harness/keys.page.html` 的
   `clientFilter` 原来只查「有没有 `codex` 这一项」，现在逐项查五个取值 —— 只加 option 而不加
   断言，等于这条检查项对新增取值永远为真（本仓踩坑 94 同族：全绿只覆盖被断言过的东西）。
3. **没有往 `scripts/ui-harness/fixtures.json` 加 `uya-agent` 数据行**（设计 §6 提过）。
   那份夹具是 2 行，走查里有若干只认「共 2 行」的断言；本沙箱没有 firefox（`make ui-check`
   会自己跳过），加行之后我**无法**验证那些断言，所以按「改不了就别改」留成 TODO 里的
   待宿主项，并已把「不需要浏览器的那条」改成真正会跑的 `requests_test.mjs` 断言。
4. **`vet` 与 `desensitize-check` 各有一处既有失败**（都不是本里程碑引入）：
   `internal/dshgw/config/sandboxview_test.go` 三处 `copylocks`（该文件最后一改在 `6a99f63`）；
   `desensitize-check` 46 处 finding（在未改动的 `main` 检出上逐字相同，且本里程碑的 diff
   一个都没新增）。`make verify` 因此在 `vet` 这一步失败 —— 本包与受影响包的 `go vet` 与
   `go test` 都单独跑过且全绿。
