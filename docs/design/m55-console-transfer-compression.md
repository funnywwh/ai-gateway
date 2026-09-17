# M55 设计文档：控制台资源的传输层压缩（gzip sidecar + Content-Encoding 协商）

> 前置：`docs/design/m50-frontend-minify.md`（发布二进制里的控制台资源是构建期 esbuild 压缩 +
> 局部标识符重命名的镜像，经 `go build -overlay` 嵌入，仓库只留源码）与
> `docs/design/m54-console-asset-shape.md`（形态可被 `/version` 断言、调试产物与部署产物隔离）。
> 本文档在编码前输出，实现后回填第 10 节差异。

## 0. 起因与实测现状

用户要求「添加前端压缩」。实勘后发现**混淆压缩已经做完并在线**，缺的是**传输层压缩**：

| 检查 | 实测（2026-09-17，在跑的 `0.16.0 / 9dc4ed2`） |
|---|---|
| 混淆 | 已完成：`/admin/ui/js/pages/chat.js` 服务出 27,872 B（源码 65,093 B），二进制 `grep -c renderShell` = 0 |
| 传输层 | **完全没有**：带 `Accept-Encoding: gzip, br` 请求，响应只有 `Content-Length`，无 `Content-Encoding`、无 `Vary` |
| 资源规模 | 源码 37 文件 / 566,807 B；镜像 37 文件 / 333,162 B（js 311,489 + css 20,655 + html 669 + svg 349） |
| gzip 收益（`gzip -9` 预估 / 构建实测） | 镜像 js/css **332,144 → 130,902 B（−61%）**（预估）；实际构建产出 **32 个 sidecar，330,801 → 131,763 B（−60%）**；chat 页 27,872 → 10,268 B（−63%）；`pinyin.js` 112,658 → 42,895；`app.css` 20,655 → 4,509 |

M50 §2 D5 当时**明确缓做**这一项（"控制台是同源控制面、内网/同机访问，收益远小于它所牵动的面"），
本次把它做掉的直接理由有两条：

1. 控制台不再只是"内网同机"——`deploy/nginx-aigw-8446.conf` 已经把它挂到 8446/TLS 上给别的机器访问，
   单页最大 112 KB 的 `pinyin.js` 是真实链路成本；
2. M50 已经建立了"构建期生成镜像 + `go build -overlay` 嵌入"这条通路，**加一个 gzip sidecar 是同一
   机制的复用**，而不是新引入一层（见第 1 节 D2 的实测前提）。

## 1. 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | **构建期预压缩**：镜像里为可压缩资产生成 `<name>.gz` sidecar | 零运行期成本、压缩级别可以取最高、产物可被审计（`ls .cache/ui-dist/static/js/*.gz` 就是全部真相）。**否决**运行期 `gzip.Writer`：省下二进制里那 131 KB，但每个响应都要现压（或在启动期压一遍并常驻内存），且"到底压了没有"变成无法离线核对的行为 |
| D2 | sidecar 走**同一条 overlay 通路**（新增文件由 `-overlay` 引入嵌入树），不落进仓库 | 已实测：`//go:embed static` 的目录模式经 `fsys.WalkDir → fsys.ReadDir` 展开，而 `fsys.ReadDir` 会**把 overlay 里映射到本目录的新文件合并进磁盘清单**；`go list -f '{{.EmbedFiles}}'` 能列出源目录里并不存在的 `static/js/app.js.gz`，编译出的二进制也确实读到了它。于是仓库仍然只有一个真值（源码），`git status` 干净，`go test`/`go vet` 不带 overlay 时读到的仍是源码 |
| D3 | 只压 **`.js/.css/.html/.svg`**，且**省不到 256 B 就不压** | 白名单避免给已是二进制/已压缩的格式（png/woff2/…）套一层壳；256 B 阈值让"压完更大"的极小文件（`favicon.svg` 349→226、`index.html` 669→512、`js/base.js` 475→318 这类）不产生无意义请求面。规则写在**一个地方**（`minify.gzipEligible`），服务端只认"有没有 sidecar"，不重复判断 |
| D4 | 服务端**只做协商**，压缩逻辑全在构建期 | 服务端判断"有没有 `.gz`"用 `fs.Stat`，是文件存在性而不是规则；因此服务端与生成侧不会各持一套阈值而漂移 |
| D5 | 协商接受 `gzip` 的显式 `q>0`，以及**未显式列出 gzip 时的 `*;q>0`** | RFC 9110 的通配语义；**否决**一律发 gzip：那会把压缩字节发给不认它的客户端。**否决**看 `User-Agent` 之类的启发式：判断依据必须是客户端自己说的话 |
| D6 | 有 sidecar 时**两个分支都发 `Vary: Accept-Encoding`** | 否则共享缓存可能把 gzip 表示发给只接受 identity 的客户端（这正是 `Vary` 存在的理由）。没有 sidecar 的树（源码构建）**不发** `Vary`：那时的响应与改动前逐字节一致 |
| D7 | 压缩分支显式设 `Content-Type`，复用 `http.FileServer` 服务 `.gz` 文件 | `FileServer` 只在响应头没设时才按扩展名探测，不显式设置就会把 `.gz` 报成 `application/gzip`。其余（`Content-Length`、HEAD、错误）交给 `FileServer` 而不是手搓 `io.Copy`——少一处能写错的地方 |
| D8 | 压缩分支**复用原路径的缓存与安全头**（`Cache-Control`、`nosniff`、CSP） | `setCacheHeaders(w, path)` 用**原始**路径调用：`strings.HasSuffix(path, ".html")` 的 CSP 判定不能被 `.gz` 后缀带偏 |
| D9 | 形态也要能运行态自问：`-X main.uiEncoding`、`/version`、`/healthz` 增 `ui_encoding` | 沿用 M54 D1/D4/D5 的既有模式（`-X` 与 `-overlay` 同行、默认值取安全侧、五处同源）。不做的话，下一轮"为什么还是没压缩"又会回到 `curl -D-` 考古 |
| D10 | **不做** brotli/zstd | 需要新增 Go 依赖（本仓库至今零 npm、Go 依赖也很克制）。brotli 相对 gzip 只再省约 15%，但会让 sidecar 集合与协商表都翻倍。要做另立里程碑：那时应当先论证"边缘终止 TLS 的部署里压缩该不该由网关自己做" |
| D11 | `AssetCount()` 显式跳过 `.gz` | 它描述的是"控制台有多少资产"，overlay 构建下不能从 37 变 69。它是 `embed_test.go` 的第一条断言，语义不能被 sidecar 污染 |

## 2. 接口

### 2.1 `internal/webui/minify`（生成侧）

```go
type Options struct {
	SourceDir   string
	OutputDir   string
	OverlayPath string
	Gzip        bool // 生成 .gz sidecar；make build 走 true，对照实验用 false
}

type FileResult struct {
	Path        string
	SourceBytes int
	OutputBytes int
	Changed     bool
	GzipBytes   int // sidecar 字节数；0 = 该文件没有 sidecar
}

type Result struct {
	Files       []FileResult
	SourceBytes int
	OutputBytes int
	GzipFiles   int // sidecar 数量
	GzipRaw     int // 有 sidecar 的文件的镜像字节合计
	GzipBytes   int // 上述文件的 gzip 字节合计
	Warnings    []string
	Elapsed     time.Duration
}

func (r Result) PercentGzip() int // 复用 percentSaved（math.Round）
func gzipEligible(path string) bool
func compress(data []byte) ([]byte, error) // BestCompression，头部不带 Name/ModTime → 确定性
```

- 白名单：`.js .css .html .svg`；阈值 `gzipSavingsFloor = 256`。
- 每个 sidecar 在 overlay 里追加一条：源侧键是**源文件路径 + `.gz`**（源目录里并不存在），值是镜像里的
  `.gz` 路径。
- `.gz` 写失败 → 返回 error，构建失败（绝不静默产出"一半有 sidecar"的镜像，M50 D7 的同一原则）。

### 2.2 `cmd/minifyui`

新增 `-gzip`（默认 true）。汇总行：

```
ui: minified 37 files 566807 -> 333162 bytes (-41%); gzip 32 files 330801 -> 131763 bytes (-60%) in 62ms
ui: overlay -> …/overlay.json (67 entries)
```

`-gzip=false` 时打印 `; gzip: disabled`——它是 A/B 对照（"压缩到底改变了什么"）的入口。

### 2.3 `internal/webui`（服务侧）

```go
// Handler 保持既有签名；为可测性抽出 handler(root fs.FS)：go test 不带 overlay，永远看不到 .gz，
// 压缩分支若只能靠真实嵌入树触发，就永远不会被执行（M50 §9 的教训）。
func Handler() http.Handler
func handler(root fs.FS) http.Handler

func acceptsGzip(r *http.Request) bool // 显式 gzip q>0，或未显式列 gzip 时 * q>0
func hasSidecar(root fs.FS, path string) bool
func contentTypeFor(path string) string // mime.TypeByExtension，与 FileServer 同源
func withPath(r *http.Request, path string) *http.Request
```

请求路径（`path` 已做既有处理：空 → `index.html`，目录/缺失 → SPA 回落或 404）：

```
setCacheHeaders(w, path)              // 原语义，用的是原始路径
if hasSidecar(root, path) {
    w.Header().Add("Vary", "Accept-Encoding")
    if acceptsGzip(r) {
        w.Header().Set("Content-Encoding", "gzip")
        w.Header().Set("Content-Type", contentTypeFor(path)) // FileServer 只在未设置时才探测
        files.ServeHTTP(w, withPath(r, path+".gz"))
        return
    }
}
files.ServeHTTP(w, r)
```

`serveIndex`（`/` 与 SPA 回落）走同一套判断。

### 2.4 `cmd/aigw` / `internal/httpapi` / 脚本

- `var uiEncoding = "identity"`（默认取安全侧：只有同时传了 `-overlay` 的那条 `make build` 行才设
  `gzip`）；`versionLine()` → `aigw 0.16.0 (revision …, built …, console minified, transfer gzip)`；
  启动日志加 `"ui_encoding"`。
- `Deps.UIEncoding string`；`/version`、`/healthz` 增 `"ui_encoding"`（空 → `"unknown"`，与 `ui` 同规则）。
- `Makefile`：`build` 行与 `-overlay` **同行**加 `-X main.uiEncoding=gzip`；`build-src` 加
  `-X main.uiEncoding=identity`。
- `scripts/local-run.sh status`：`console: minified · transfer: gzip`；缺字段 → `unknown` 并说明原因，不报错。
- `scripts/ui-harness/server.py`：`UI_HARNESS_GZIP=1` 时按与 Go 端相同规则发 `.gz`；
  `run.sh` 在该模式下自检（见第 6 节）——**让浏览器真的跑在压缩传输上**。

## 3. 数据流

```
internal/webui/static/            唯一真值（源码，37 文件 / 566,807 B，进 git）
      │  cmd/minifyui：esbuild 压缩 → gzip sidecar
      ▼
.cache/ui-dist/static/            镜像 333,162 B + 32 个 *.gz（130,902 B）
.cache/ui-dist/overlay.json       67 条（35 个被改写文件 + 32 个 sidecar）
      │  make build: go build -overlay=$(UI_OVERLAY) -X main.uiAssets=minified -X main.uiEncoding=gzip
      ▼
bin/aigw ── GET /admin/ui/js/pages/chat.js (Accept-Encoding: gzip) ──► 10,065 B + Content-Encoding: gzip + Vary
         └─ Accept-Encoding: identity ─────────────────────────────► 27,872 B（仍带 Vary）
```

## 4. 异常与边界

| 情况 | 处理 |
|---|---|
| esbuild 或 gzip 出错 | `make build` 失败，不写镜像/overlay，不留临时目录（M50 D7/D8 不变） |
| 无 `Accept-Encoding` / `identity` / `gzip;q=0` / 只有 `br` | 发原字节；有 sidecar 时**仍带 `Vary`**；正文与改动前逐字节一致 |
| `*;q=1` 且未显式列 `br`/`gzip` | 视为接受 gzip（RFC 9110） |
| `make build-src` / 手写 `go build` | 镜像里没有 `.gz` → 无 `Content-Encoding`、无 `Vary`；`/version` 报 `identity` |
| 直接请求 `/js/app.js.gz` | 协商只看 `path+".gz"`；该请求落到 `FileServer`，按 `application/gzip` 原样返回（控制台不请求它） |
| HEAD / Range | 交给 `FileServer`：HEAD 无正文且 `Content-Length` 正确；Range 作用于所选表示（即 gzip 表示） |
| SPA 回落（`/admin/ui/requests`） | 同一套协商；当前 `index.html` 低于阈值，实际仍是原字节（用 MapFS 测试钉住"阈值之上会压缩"） |
| 上游 nginx（8446） | 该配置未开 gzip；即便开了也不会对已带 `Content-Encoding` 的响应二次压缩 |
| 共享缓存 | 两个分支都带 `Vary: Accept-Encoding` |
| 二进制体积 | sidecar 数据本身不可再压，**+≈132 KB**；但镜像的混淆节省（−229 KB）仍然更大，所以 `make build` 的产物**比 `make build-src` 还小 98 KB**（21,918,362 vs 22,016,610 B）。当时担心的"体积代价"没有出现 |
| 确定性 | gzip 头不带 Name/ModTime → 两次 `make ui-dist` 逐字节相同 |

## 5. 测试策略

**`internal/webui/minify/minify_test.go`（改 + 增）**

1. 文件集契约：`源 ⊆ 镜像`，且 `镜像 − 源 == {f+".gz" | f 有 sidecar}`（精确相等）。
2. 每个 sidecar 解压后与对应镜像文件**逐字节相同**（对真实 `../static` 树跑）。
3. 规则边界：`index.html`/`favicon.svg`/小 `js` 无 sidecar；非白名单扩展名（构造 `.png` 源文件）无 sidecar。
4. overlay：每个 `Changed` 文件与每个 sidecar 都有条目；条目数 = 被改写文件数 + sidecar 数。
5. 确定性：两次运行（含 sidecar）逐字节相同；`-out` 目录无残留。
6. 失败路径与开关：坏语法 → error 且不留输出目录；`Gzip=false` 时镜像与 M50 时代逐字节相同。
7. **变异验证**：跳过写 sidecar / 让 overlay 漏掉 sidecar → 测试必须变红。

**`internal/webui/encoding_test.go`（新增，`fstest.MapFS` 注入带 sidecar 的树）**

8. 表驱动协商：`gzip`/`gzip, deflate, br, zstd` → `Content-Encoding: gzip` + `Vary` + `Content-Length` = gz 大小
   + 解压后等于原文件；`identity`/`gzip;q=0`/`br`/空 → 无 `Content-Encoding`、正文 = 原文件、**仍带 `Vary`**；
   无 sidecar 的树 → 无 `Content-Encoding` **且无 `Vary`**。
9. `Content-Type` 两个分支一致；`Cache-Control`/`nosniff`/CSP 不变。
10. HEAD 压缩分支同头空正文；`/js/missing.js` 仍 404；`/index.html` 301 语义不变（由 `FileServer` 决定）。
11. `AssetCount()` 不数 `.gz`。
12. **变异验证**：`acceptsGzip` 恒真 / 忽略 `q=0` → 测试必须变红。

**`internal/httpapi/version_test.go` / `cmd/aigw/version_test.go`**：`ui_encoding` 与 `ui` 同规则（空→`unknown`），
两个端点一致；`versionLine()` 覆盖 `uiAssets`×`uiEncoding` 与默认值。

## 6. 验收（隔离端口，全程不动在跑的 8088）

| 步骤 | 期望 |
|---|---|
| `make ui-dist` | `gzip 32 files 332144 -> 130902 bytes (-61%)`；overlay 67 条；`js/pages/chat.js.gz` 存在 |
| `make build` / `make build-src` | `-version` 分别为 `console minified, transfer gzip` / `console source, transfer identity`；两个二进制仍相差 −98 KB（release 更小） |
| 隔离端口两实例（`make build` / `make build-src`） | `/version`、`/healthz` 的 `ui`/`ui_encoding` 分别为 `minified/gzip`、`source/identity`；`level=ERROR` = 0 |
| `curl -H 'Accept-Encoding: gzip'` 取 chat.js（压缩版实例） | 200、`Content-Encoding: gzip`、`Vary: Accept-Encoding`、`Content-Length` ≈ 10,065、`Content-Type: text/javascript; charset=utf-8`；`gzip -dc` 与镜像文件 `cmp` 相同 |
| 同路径不带 `Accept-Encoding` | `:8111` 回 27,872 B（仍带 `Vary`）；`:8112` 回 65,093 B（无 `Vary`） |
| `/`、`/admin/ui/requests`、`app.css`、`missing.js`、`index.html` | CSP/SPA 回落/404/301 逐条与改动前一致 |
| `UI_HARNESS_GZIP=1` + `UI_STATIC_DIR=.cache/ui-dist/static` 的 `make ui-check` | 21 个视图全绿；链路自检证明响应确实是 gzip（体积对比 + `gzip -dc` 逐字节比对） |
| `make ui-check`（源码树）与 `UI_STATIC_DIR=…`（镜像，不 gzip） | 与改动前基线一致 |
| `go vet ./...` / `go test ./...` | 干净 / 51 包全 ok（新增 `internal/webui/encoding_test.go`） |
| `scripts/load.sh` | 跑完 `bin/aigw` sha256 不变 |
| 在跑的 `:8088` | 全程 sha256 与 `/version` 不变 |

## 7. 依赖

无新增依赖：`compress/gzip` 是标准库（`internal/dshgw/tenancy/backup.go` 已在用），
`mime.TypeByExtension` 亦同。不新增 npm/node 依赖。

## 8. 与 M50 §2 D5 的关系

M50 D5 的原文是"**不做**传输层预压缩（.br/.gz + Content-Encoding），用户确认本次不做"。
本期做的正是它，且把当时的两条顾虑各自给了处理：

| M50 的顾虑 | M55 的处理 |
|---|---|
| "牵动 embed 形态" | 已实测 overlay 能把新增文件喂给 `//go:embed`，所以 embed 形态**不需要**变（仓库仍然只有源码） |
| "`Vary` 头、缓存策略" | 决策 D6：有 sidecar 时两个分支都发 `Vary`；缓存策略一字未改（`no-cache` / `max-age=300` 原样） |
| "`Accept-Encoding` 协商" | 决策 D5：显式 `gzip` 或通配 `*` 才发，`q=0` 尊重；有 12 条测试钉住 |
| 收益"远小于面" | 现在控制台经 nginx 8446 给别的机器访问，全量 js/css −61%、单页最大 −64%，收益已可测 |

## 9. 非目标

- 不做 brotli/zstd（D10）；不做运行期压缩（D1）；不改混淆强度与 `.js/.css` 的压缩语义；
- 不改 `index.html`/`favicon.svg` 的"不混淆"策略（M50 D9 维持，本期连它们也不压：低于阈值）；
- 不发布 sourcemap；不做 CDN/边缘压缩；不动上游 nginx 配置（它是否需要开 gzip 是部署侧的另一件事）。

## 10. 实现与设计差异

1. **`.gz` 的嵌入不需要任何 embed 侧改动，但需要 overlay 里有一条"源侧不存在"的条目**——设计把这点写成
   前提，实现时把它变成了写死的行为：`writeOverlay` 为每个 sidecar 追加
   `<源路径>+".gz" → <镜像路径>+".gz"`。这条键指向一个**磁盘上不存在的文件**，正是它让目录模式的
   `//go:embed static` 把新文件合并进嵌入清单（实测：`go list -f '{{.EmbedFiles}}'` 里出现
   `static/js/app.js.gz`）。测试专门断言这个 key 在源码树里不存在——否则"镜像往源码树写文件"这种
   更糟的事会以一副正常的样子通过。
2. **`Content-Length` 必须显式设置**（设计 §2.3 未提）。`http.FileServer` 对带 `Content-Encoding` 的
   响应**拒绝**设置 `Content-Length`（它无法知道未编码时的长度），于是第一版所有压缩响应都变成了
   chunked——而改动前每个资源都带长度。修法是在压缩分支用 `fs.Stat` 取 sidecar 大小写死；`serveIndex`
   同样显式设置（它自己 `Write` 一次，长度不该依赖编码方式）。测试用 `HEAD` 钉住了这一点：
   第一版就是被这条测试抓住的。
3. **`serveIndex` 也要协商**，而且要让 `Content-Type: text/html` 覆盖掉由 `.gz` 推出的类型：
   外壳今天低于 256 B 阈值所以实际不会被压缩，但分支必须存在，否则"外壳长大到阈值之上"会变成
   一次静默的回归。测试用 `fstest.MapFS` 造了一个**带 sidecar 的 shell** 来跑这条路径。
4. **`AssetCount()` 拆出 `countAssets(fsys, root)`**：原实现直接走嵌入树，而 `go test` 的嵌入树里
   永远没有 `.gz`，于是"跳过 sidecar"这条规则无法被测试。拆出后测试用注入的树验证（4 个资产 + 3 个
   sidecar → 仍然数 4）。
5. **二进制体积的预期是错的，而且错在好消息的方向**：设计 §4 预估 sidecar 会让发布产物 **+≈132 KB**。
   实测 `make build` = 21,918,362 B 对 `make build-src` = 22,016,610 B，**发布产物仍小 98 KB**——镜像的
   混淆节省（约 229 KB）大于 gzip 数据本身（131,763 B）。D1"构建期预压缩"的代价比预期更低，无需重新权衡。
6. **`local-run.sh` 的一处编辑事故**：把 `console_shape()` 改成 `console_fields()` 时，`edit` 的匹配
   吞掉了后面的 `do_start()` 函数头，`bash -n` 立刻报语法错。修好后再跑，`status` 输出形如
   `console: minified · transfer: gzip`；对早于 M54/M55 的二进制打印两行 `unknown` 并说明原因。
   教训与仓库既有习惯一致：**改脚本之后先 `bash -n` 再谈行为**。
7. 走查侧多了一层设计没写死的东西：`run.sh` 在 gzip 模式下**自检**（`Content-Encoding: gzip`、
   压缩后更小、`gzip -dc` 与磁盘文件逐字节相同），不满足直接非 0 退出。设计只写了"让浏览器跑在压缩传输上"，
   而"跑在压缩传输上"必须由脚本自己证明，否则会退化成 M50 §9 记录过的那种没有咬合力的验证。

## 11. 验收记录（实测）

环境：本机 `winger`，`source scripts/goenv.sh`，Go 1.25.5，revision `3c01859`（含 M54），版本 `0.16.0`。

### 生成侧

```
$ make ui-dist
ui: minified 37 files 566807 -> 333162 bytes (-41%); gzip 32 files 330801 -> 131763 bytes (-60%) in 62ms
ui: overlay -> /home/winger/work/ai_gateway/.cache/ui-dist/overlay.json (67 entries)
```

- overlay 67 条 = 35 个被改写文件 + 32 个 sidecar；`index.html`、`favicon.svg`、`js/base.js`、
  `js/brand.js`、`js/pages/placeholder.js` 因低于 256 B 阈值没有 sidecar。
- 32/32 个 sidecar `gzip -dc` 后与镜像文件逐字节相同；连续两次运行镜像与 overlay 逐字节相同。
- 被压得最多的文件：`js/pinyin.js` 112,658 → 42,895、`js/pages/chat.js` 27,872 → 10,268、
  `js/pages/requests.js` 16,868 → 6,350、`js/pages/providers.js` 16,688 → 6,341、`app.css` 20,655 → 4,509。

### 二进制

| 构建 | 体积 | `-version` |
|---|---|---|
| `make build` | 21,918,362 B | `aigw 0.16.0 (revision 3c01859, built …, console minified, transfer gzip)` |
| `make build-src` | 22,016,610 B | `…console source, transfer identity` |

发布产物**比源码版小 98,248 B**：混淆省下的约 229 KB 仍然大于内嵌的 131,763 B gzip 数据。

### 服务行为 A/B（隔离端口 `:8111` 带 sidecar / `:8112` 源码构建，各自空库）

| 请求 | `:8111` | `:8112` |
|---|---|---|
| `/version` | `{"revision":"3c01859","ui":"minified","ui_encoding":"gzip","version":"0.16.0"}` | `…"ui":"source","ui_encoding":"identity"…` |
| `chat.js`（`Accept-Encoding: gzip`） | 200，**10,268 B**，`Content-Encoding: gzip`，`Vary: Accept-Encoding`，`text/javascript; charset=utf-8` | 200，65,093 B，无 `Content-Encoding`、**无 `Vary`** |
| `chat.js`（不带 `Accept-Encoding`） | 200，27,872 B（镜像原文），仍带 `Vary` | 200，65,093 B，无 `Vary` |
| `app.css`（gzip） | 20,655 → **4,509 B**，`text/css; charset=utf-8` | 29,884 B 原文 |
| `/`、SPA 回落 `/admin/ui/requests` | 200 / 669 B（带 CSP；shell 低于阈值故未压缩） | 同 |
| `/admin/ui/index.html` | 301（`FileServer` 规范跳转，改动前也是） | 301 |
| `/admin/ui/js/pages/missing.js` | 404 | 404 |
| `Cache-Control` | `public, max-age=300`（资源）/ `no-cache`（shell） | 同 |
| `level=ERROR` | 0 | 0 |

`Accept-Encoding` 协商逐条实测（`:8111`）：`gzip` → gzip；`deflate, gzip` → gzip；`br, *;q=1` → gzip；
`identity` / `gzip;q=0` / `br` / 不带头 → 原字节。`HEAD` 压缩分支：无正文、长度 10,268、类型不变。
`gzip -dc` 后的正文与 `.cache/ui-dist/static/js/pages/chat.js` 逐字节相同；源码版实例的正文与
`internal/webui/static/js/pages/chat.js` 逐字节相同。验证后两个端口释放，在跑的 `:8088` 未受影响。

### 浏览器走查

```
$ UI_STATIC_DIR=$PWD/.cache/ui-dist/static UI_HARNESS_GZIP=1 scripts/ui-harness/run.sh
assets: /home/winger/work/ai_gateway/.cache/ui-dist/static
gzip self-check: js/app.js 3007 -> 1593 B, Content-Encoding: gzip, Vary: Accept-Encoding
…
all views passed
```

**21 个视图全绿**，逐视图检查项与源码树基线完全相同（docs 26 / detail 23 / capacity 8 / models 17 /
create 6 / plugin 20 / plugin-cached 19 / currency 16 / keys 17 / requests 106 / paging 30 / chat 110 /
noSkills 7 / skills 10 / form 78 / bridge 17 / brand 10 / tree 57 / org 52 / org-readonly 8 / org-accounts 10）。
自检行是"这次真的跑了压缩路径"的证据：`js/app.js` 3007 → 1593 B 且带 `Content-Encoding: gzip`。

第一次跑这一轮时 `form` 报了 `no report (the page was torn down before the assertions ran)`——单个视图重跑
78 项全过，随后整套重跑 21/21 全过，判定为 headless firefox 的拆页抖动而非本改动的回归。如实记录在此，
因为"走查偶发红"与"走查有咬合力"是两件事：这次的判断依据是同一视图在**同一模式、同一份产物**下重跑通过，
而不是换个模式跑绿。

不带 sidecar 的那条路径同样回归过：`scripts/ui-harness/run.sh`（源码树、无 `UI_HARNESS_GZIP`）
**21 个视图全绿**，逐视图检查项与上表相同——即"没有 `Content-Encoding`、没有 `Vary`"的分支
在真实浏览器下与改动前完全一致。

### 测试

| 检查 | 结果 |
|---|---|
| `go vet ./...` | 干净 |
| `go test ./...` | 全绿（51 个包，0 FAIL） |
| 变异验证 1：`writeMirror` 不写 sidecar | 3 条 FAIL（sidecar 覆盖、解压一致、overlay 完备） |
| 变异验证 2：`writeOverlay` 丢掉 sidecar 条目 | 3 条 FAIL（overlay 完备） |
| 变异验证 3：`acceptsGzip` 忽略 `q=0` | 2 条 FAIL（协商表） |
| 变异验证 4：`Vary` 只加在压缩分支 | 3 条 FAIL（协商表） |
