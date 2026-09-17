# M54 控制台资源形态的运行态自述与部署产物隔离

> 前置：`docs/design/m50-frontend-minify.md`（控制台资源在发布二进制里以 esbuild 压缩 + 局部标识符
> 重命名的形态存在，构建期生成、`go build -overlay` 嵌入、仓库只留源码）。
> 本文档在编码前输出，实现后回填第 9 节差异。

## 0. 起因与结论

用户报告：**「本机的 8088 实例的前端 js 没有混淆」**。

2026-09-17 16:00 在本机实测，**这个现象复现不出来**——在跑的实例是混淆的：

| 检查 | 实测 |
|---|---|
| 运行进程 | `/proc/1360138/exe` → `/home/winger/work/ai_gateway/bin/aigw`，监听 `*:8088` |
| `/version` | `{"revision":"9dc4ed2","version":"0.16.0"}`（进程 15:38:02 启动，二进制 15:32:42 构建） |
| 逐文件内容 | 控制台 **37 个资产逐个 sha256 与 `.cache/ui-dist/static/` 的压缩镜像全部相同**（`js/app.js` 服务出 3007 B，源码 6097 B；`js/pages/chat.js` 27872 B，源码 65093 B） |
| overlay | 35 条 = 37 − `index.html` − `favicon.svg`，与 M50 §10 记录一致 |
| 二进制指纹 | `strings bin/aigw \| grep -c renderShell` = **0**，`const STATS_SORTS` = **0**（源码形态分别为 3 与 1） |
| 缓存与服务端 | 资产响应只有 `Cache-Control: public, max-age=300`，无 `ETag`/`Last-Modified`；控制台没有 service worker，不存在"浏览器长期吃旧的可读版" |

但**"曾经不是"是可证的**，磁盘上留着的回滚点就是证据：

| 二进制 | 构建时间 | `renderShell` | `const STATS_SORTS` | 形态 |
|---|---|---|---|---|
| `bin/aigw.prev-running-0.14.0-6dc9082`（M50 上线前 8088 的**真身**） | 09-15 21:00 | 3 | 1 | **源码（未混淆）** |
| `bin/aigw.prev-0.14.0-6bf8dce`（从 tag `v0.14.0` 重建） | 09-15 21:00 | 3 | 1 | **源码（未混淆）** |
| `/tmp/aigw-src`（M50 当时的 A/B 对照，源码版） | 09-15 20:47 | 3 | 1 | 源码 |
| `/tmp/aigw-dist`（M50 当时的 A/B 对照，压缩版） | 09-15 20:48 | 0 | 0 | 混淆 |
| `bin/aigw`（当前在跑） | 09-17 15:32 | 0 | 0 | 混淆 |

M50 随 **0.14.1**（2026-09-15 21:00 部署）上线，在那之前本机 8088 上的控制台一直是可读源码。
所以"看到可读的前端 js"在时间上完全成立。

**真正的问题不是这次看到的字节，而是"看到可读字节之后，没有任何运行态信号能回答为什么"**：

1. **运行态不自述形态**。构建日志（`ui: minified 37 files …` / `ui: source assets (not minified)`）
   只打在**构建者当时的终端**上；运行中的实例、`/version`、`/healthz`、控制台角标，一个都不提。
   判定形态只能靠 `strings bin/aigw | grep -c renderShell` 这种专家手法。
2. **有两条路径会静默把未混淆版换上 8088**，都不带 overlay、都不留持久痕迹：
   - `make build-src` 直接写 **`bin/aigw`** —— 而那正是 `scripts/local-run.sh` 的默认 `BIN`。
     "我只是想调试一下控制台" + 之后任意一次 `local-run.sh restart` = 可读前端上了 8088。
     M50 §6 把这个脚枪写进了文档，并承认三条缓解"都不完美"。
   - `scripts/load.sh:46` 的 `go build -o bin/aigw ./cmd/aigw` —— 一个**压测脚本**在就地覆盖部署产物。
     这条比上一条更隐蔽：它不是"我要源码版"的显式表达，而是顺手发生的。
3. `/admin/ui/` 的外壳 `index.html`（669 B）与 `favicon.svg`（349 B）按 M50 D9 **不压缩**，
   是控制台里仅有的两份可读资产。这是有意的，本里程碑不改。

本里程碑把"形态"从一个只能靠专家手法考古的隐含事实，变成**运行态自述的、可被运维脚本断言的属性**，
并把上面两条覆盖路径从"需要记住别这么干"改成"结构上做不到"。

## 1. 目标与非目标

**目标**

1. 在任何时刻，一条 `curl /version` 就能问出"这个实例的前端是混淆的还是可读的"。
2. `make build-src` 与 `scripts/load.sh` **不可能**再覆盖 `bin/aigw`。
3. 发布记录自动带上"这一版是混淆的"的证据，不需要任何额外步骤。
4. 源码形态的本地实例仍然可用（M50 D10 明确保留的调试用法），但要**响亮**。

**非目标**

- 不改压缩语义：仍是 esbuild 压缩 + 局部标识符重命名，仍是 37 个文件、35 条 overlay、-41%。
- 不改任何资源字节、CSP、缓存头、SPA 回落、挂载前缀推导。
- 不做传输层预压缩（M50 D5 不变）。
- **不做**"检测到源码形态就拒绝启动"：源码版实例是合法用法，拒绝会破坏它。
  护栏放在产物落点上（第 2 节 D2/D3），不放在运行门槛上。

## 2. 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | 形态由**构建期声明**承载：`-X main.uiAssets=minified\|source`，代码里的默认值是 **`source`** | 声明与 `-overlay` 写在 Makefile 的**同一行**，两者无法单方面漂移。默认值取"响亮的那一侧"是刻意的不对称：`minified` 只有同时传了 `-overlay` 的那一行会设置，所以**"自称已混淆、实际是源码"这个危险方向在构造上不可能**；反过来（自称源码、实际已混淆）只是一次误报，会被人看见。**否决**运行期哨兵注释（要往业务源码里塞一条只为探测存在的魔法注释）与启发式（"压缩后是单行"这类性质会随 esbuild 升级改变，且判据本身判错的方向不可控） |
| D2 | `make build-src` 的产物改到 **`bin/aigw-src`**，不再写 `bin/aigw` | 把脚枪从"纪律问题"变成"结构问题"：调试构建碰不到部署产物，就不存在"忘了重新 `make build`"。**否决**"仅打印告警"：告警会被忽略，而 M50 §6 已经承认现有三条缓解都不完美 |
| D3 | `scripts/load.sh` 不再写 `bin/aigw`：在自己的 `$WORK` 里构建并运行 | 一个压测脚本就地覆盖部署产物是纯粹的事故源；它与它的临时库、临时配置同生命周期，产物也应如此 |
| D4 | `/version` 与 `/healthz` 都新增 `"ui"` 字段（`minified`/`source`/`unknown`） | 与既有 `version`/`revision` 同属"构建身份"，运维一条 curl 就能问。沿用 `version_test.go` 里既有的约束——探针与专门端点读到的身份不得漂移，所以两处一起加，不是只加 `/version` |
| D5 | `aigw -version` 打印形态（`aigw 0.16.0 (revision 9dc4ed2, built …, console minified)`） | `scripts/release.sh:88` 本来就把 `-version` 的输出写进发布记录，于是**每次发版自动留下形态证据**，无需改发布流程 |
| D6 | `scripts/local-run.sh` 的 `status` 打印形态，`start` 在非 `minified` 时**显著告警但不拒绝** | 源码版实例仍是合法调试用法（D10）。这里的告警是最后一道"看见"，真正的护栏是 D2/D3 |
| D7 | `ui` 字段描述的是 **JS/CSS 的形态**；`index.html`（D9 不压缩）与 `favicon.svg` 不参与判定 | 维持 M50 D9。字段语义必须在文档里写死，否则"外壳是可读的"会被误读成"整个控制台没混淆" |
| D8 | 不新增依赖，不改 `cmd/minifyui` 与 `internal/webui/minify` 的接口 | 本里程碑只动"形态的可见性"与"产物的落点"；压缩工具本身已被 M50 的 7 项契约测试锁住，无必要触碰 |

## 3. 接口

### 3.1 `cmd/aigw`

```go
// uiAssets is the console asset shape this binary carries: "minified" when the build
// compiled with -overlay against the mirror cmd/minifyui produced, "source" otherwise.
// `make build` sets it next to -overlay, on the same line, so the two cannot drift;
// the default is the loud side. See docs/design/m54-console-asset-shape.md.
var uiAssets = "source"

// versionLine renders the one-line build identity that `-version` prints and
// scripts/release.sh copies into the release record.
func versionLine() string
// 例：aigw 0.16.0 (revision 9dc4ed2, built 2026-09-17T07:32:40Z, console minified)
```

启动日志的 `aigw starting` 追加一个字段：`"ui"`。

### 3.2 `internal/httpapi`

```go
type Deps struct {
	// …既有字段…
	Version  string
	Revision string
	// UIAssets is the console asset shape ("minified"/"source"). Empty is reported as
	// "unknown" rather than omitted: an operator reading /version must never have to
	// guess whether a missing field means source or means an older binary.
	// 字段名不是 UI：Deps 里已经有一个 UI（控制台 handler），见第 9 节第 1 条。
	UIAssets string
}
```

`handleVersion` 与 `handleHealthz` 都输出 `"ui": s.deps.UIAssets`（空 → `"unknown"`）。

### 3.3 `Makefile`

```make
build: version-check ui-dist
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS) -X main.uiAssets=minified" \
		-overlay $(UI_OVERLAY) -o bin/aigw ./cmd/aigw

build-src: version-check
	@echo "ui: source assets (not minified); wrote bin/aigw-src — bin/aigw is untouched"
	@$(GOENV) go build -trimpath -ldflags "$(LDFLAGS) -X main.uiAssets=source" -o bin/aigw-src ./cmd/aigw
```

`clean` 已是 `rm -rf bin $(UIDIST)`，`bin/aigw-src` 自动被覆盖；`/bin/` 在 `.gitignore` 里。

### 3.4 `scripts/local-run.sh` 与 `scripts/load.sh`

- `local-run.sh status`：健康检查后追加一行 `console: minified (js/css minified)`，
  数据来自 `GET /version` 的 `ui` 字段；字段缺失（0.16.0 及更早的二进制）显示
  `console: unknown（该二进制早于 M54，没有 ui 字段）`，**不得报错退出**。
- `local-run.sh start`：形态不是 `minified` 时打印醒目告警（含"这是源码形态，仅用于调试"与
  如何回到混淆版的命令），然后照常启动。
- `load.sh`：`go build -o bin/aigw ./cmd/aigw` → 在 `$WORK` 下构建并以 `$WORK/aigw` 启动。

## 4. 数据流

```
Makefile build ──┬─ ui-dist ──► .cache/ui-dist/static/（压缩镜像）
                 └─ go build -overlay=… -X main.uiAssets=minified ──► bin/aigw
                                                                            │
              local-run.sh start（BIN 默认 bin/aigw）─► 进程 ─► GET /version
                                                                            ▼
                                                        {"version":…,"revision":…,"ui":"minified"}
                                                                            │
                                              local-run.sh status / 发布脚本 / 人工 curl 读到同一事实

Makefile build-src ─────────────────────────► bin/aigw-src（bin/aigw 不变）
scripts/load.sh    ──► $WORK/aigw（用完即弃，bin/aigw 不变）
```

## 5. 异常与边界

| 情况 | 处理 |
|---|---|
| 手写 `go build -overlay … -o bin/aigw`（带 overlay、不带 `-X`） | 二进制自称 `source` 而实际是压缩资源：**方向安全**（不会谎称"已混淆"），且 `local-run.sh` 会据此告警，把不一致暴露给人 |
| `BIN=bin/aigw-src scripts/local-run.sh restart`（有意跑源码版） | 启动日志与 `status` 都写 `source`，`start` 打告警，行为不拒绝 |
| 老二进制（≤ 0.16.0）没有 `ui` 字段 | `/version` 里没有该键 → 脚本按 `unknown` 处理并说明原因；不因为解析失败而中断 `status` |
| `Deps.UI` 为空（测试 fixture、库调用方） | 输出 `"ui":"unknown"`，不省略字段——缺字段与 unknown 必须在线上可区分 |
| `go test` / `go vet` / `ui-check` | 不带 overlay，读到源码树；`uiAssets` 保持默认 `source`，测试里由 fixture 显式给定期望值 |
| esbuild 升级 | 与本里程碑无关：形态声明不依赖任何"压缩后长什么样"的性质 |
| 有人删掉 Makefile 里的 `-X main.uiAssets=minified` | 发布脚本与验收会读到 `source` 并告警——`make build` 的验收项失败，而不是静默通过 |

## 6. 与 M50 §6「脚枪」的关系

M50 §6 坦承脚枪存在，并给了三条缓解：`make build` 是唯一记录在案的入口、`build-src` 让它成为显式的话、
日志行会写明走了哪条路。三条都成立，也都依赖"人记得住"。M54 用两条替换它们：

| M50 缓解 | M54 之后 |
|---|---|
| 依赖"大家都用 `make build`" | **产物隔离**：调试构建写 `bin/aigw-src`，`bin/aigw` 只可能来自 `make build` |
| 靠构建日志区分形态（只在构建者终端） | **运行态自述**：`/version`、`/healthz`、启动日志、`-version`、`local-run.sh status` 五处同一事实 |
| 靠 `strings \| grep renderShell` 抽查 | 同一条 curl 可断言，可直接写进运维与发布脚本 |

M50 §3.3 与 §6 里 `build-src` 的产物路径在实现后需要同步修订，并在 §6 末尾指向本文档。

## 7. 测试策略

1. **`internal/httpapi/version_test.go`**：`/version` 与 `/healthz` 都带 `ui`，fixture 给 `minified`；
   另测 `Deps.UI` 为空时是 `"unknown"`（保证"缺字段"与"unknown"可区分）。
2. **`cmd/aigw`**：新增 `versionLine()` 的单元测试，覆盖 `uiAssets` 的两种取值与默认值——
   `scripts/release.sh` 依赖这行文本，它不能是拼在 `fmt.Println` 里的裸字符串。
3. **变异验证**（仓库惯例）：删掉 `handleVersion` 里的 `ui` 字段 → 测试必须红；
   删掉 Makefile `build` 行的 `-X main.uiAssets=minified` → 第 4 节的验收项必须红。
4. **端到端验收（隔离端口，全程不动在跑的 8088）**：

| 步骤 | 期望 |
|---|---|
| `make build` | 日志 `ui: minified 37 files …`；`./bin/aigw -version` 含 `console minified`；`strings bin/aigw \| grep -c renderShell` = 0 |
| `make build-src` | 写 `bin/aigw-src`；**`bin/aigw` 的 sha256 与 mtime 逐字节不变**；`./bin/aigw-src -version` 含 `console source` |
| 起 `:8097`（`bin/aigw`）与 `:8098`（`bin/aigw-src`），各自空库 | `/version` 分别为 `"ui":"minified"` 与 `"ui":"source"`；启动日志同一事实；`/healthz` 一致；两实例 `level=ERROR` 为 0；验证后端口释放 |
| `scripts/load.sh`（小并发短跑） | 跑完后 `bin/aigw` 的 sha256 不变 |
| `local-run.sh status` | 打印 `console: minified`；对 `bin/aigw-src` 实例打印告警 |
| 在跑的 `:8088` | 构造与冒烟期间 sha256 与 `/version` 均不变 |

5. **`go vet ./...` / `go test ./...`** 与基线一致（38 个测试包全过）。

## 8. 依赖

无新增依赖。脚本侧解析 `/version` 用本仓库既有的做法（`python3` 一行解析），
不引入 `jq` 这类新前置。

## 9. 实现与设计差异

1. **`Deps.UI string` 与既有的 `Deps.UI http.Handler` 重名，改为 `Deps.UIAssets`**。设计稿按 wire 上的键名
   给字段命名，但 `Deps` 里 `UI` 早就是控制台 handler（`server.go` 的 `ui := s.deps.UI`、
   `internal/httpapi/basepath_test.go` 的 fixture 都指着它），一个 struct 里不能有两个 `UI`。改名的是**本次
   新增的字符串字段**而不是既有 handler：改动面更小，语义也更准（它描述的是"资产形态"）。wire 上的键仍是 `"ui"`。
   这是"M54 把形态变成可断言属性"落地时踩到的第一件事——**接线之前先看这个名字有没有人用**。
2. **补写了 `cmd/aigw/version_test.go`**（设计 §7.2 要求，实现时漏了）。它钉住 `versionLine()`：两种形态各一条
   断言，外加"默认值必须是 `source`"。默认值取错方向（自称 minified 而实际是源码）是这套机制里唯一危险的
   错误，值得一条专门的测试而不是一句注释。
3. `scripts/local-run.sh status` 对**早于 M54 的二进制**（当时在跑的 `0.16.0 / 9dc4ed2`）打印
   `console: unknown（该二进制没有 ui 字段，早于 M54；不等于未混淆，请按需确认）`——按设计 §5 只提示不报错，
   实测确认（见第 10 节）。
4. 其余按设计实现：`-X main.uiAssets` 与 `-overlay` 同行、`build-src` 改写 `bin/aigw-src`、`load.sh` 全部
   构建到 `$WORK`、`/version` 与 `/healthz` 同增 `ui`、五处同源（`/version`、`/healthz`、启动日志、
   `-version`、`local-run.sh status`）。

## 10. 验收记录（实测）

环境：本机 `winger`，`source scripts/goenv.sh`，Go 1.25.5，版本 `0.16.0 / 9dc4ed2`。

### 构建与产物形态

```
$ make build
ui: minified 37 files 566807 -> 333162 bytes (-41%) in 50ms
$ ./bin/aigw -version
aigw 0.16.0 (revision 9dc4ed2, built 2026-09-17T08:12:53Z, console minified)
$ make build-src
ui: source assets (not minified); wrote bin/aigw-src — bin/aigw is untouched
$ ./bin/aigw-src -version
aigw 0.16.0 (revision 9dc4ed2, built 2026-09-17T08:12:54Z, console source)
```

| 二进制 | 体积 | `renderShell` | `const STATS_SORTS` | `record_output_text` |
|---|---|---|---|---|
| `bin/aigw`（`make build`） | 21,773,193 B | 0 | 0 | 21 |
| `bin/aigw-src`（`make build-src`） | 22,006,633 B | 3 | 1 | 21 |

`make build-src` 前后 `bin/aigw` 的 sha256 与 mtime **逐字节不变**。

### 运行态自述（隔离端口 `:8111` 压缩版 / `:8112` 源码版，各自空库）

| 检查 | 压缩版 | 源码版 |
|---|---|---|
| `GET /version` | `{"revision":"9dc4ed2","ui":"minified","version":"0.16.0"}` | `…"ui":"source"…` |
| `GET /healthz` | `…"status":"ok","ui":"minified"…` | `…"ui":"source"…` |
| 启动日志 | `ui=minified` | `ui=source` |
| `js/app.js` / `js/pages/keys.js` / `app.css` | 3007 / 5420 / 20655 B | 6097 / 8461 / 29884 B |
| `/admin/ui/`、SPA 回落 `/admin/ui/providers` | 200 / 669 B（带 CSP） | 200 / 669 B（同） |
| `/admin/ui/index.html` | 301（FileServer 规范跳转，改动前也是） | 301 |
| `/admin/ui/js/pages/missing.js` | 404 | 404 |
| `level=ERROR` 条数 | 0 | 0 |

`local-run.sh status`：对 `:8111` 打印 `console: minified（控制台 js/css 已压缩混淆）`；对当时在跑的
`:8088`（`0.16.0`，早于 M54）打印 `console: unknown（…早于 M54…）` 且**不报错退出**。
`scripts/load.sh 4 3s` 跑完（4663 请求、rps 1554）后 `bin/aigw` 的 sha256 不变。验证后两个端口均已释放，
在跑的 `:8088` 的 `/version` 与二进制 sha 全程未变。

### 测试

| 检查 | 结果 |
|---|---|
| `go vet ./...` | 干净 |
| `go test ./...` | **51 个包全 ok、0 个 FAIL**（新增 `cmd/aigw/version_test.go`） |
