# M50 设计文档：控制台前端资源混淆压缩

> 前置：`docs/design/m9-web-console.md`（控制台零构建、原生 ES 模块、`go:embed` 随二进制发布）。
> 本文档在编码前输出，实现后回填第 8 节差异。

## 1. 目标

控制台（`internal/webui/static`，37 个文件、559,442 B）在**发布二进制里**以**压缩 + 标识符重命名**的
形态存在：拿到二进制的人反解 `go:embed` 出来的资源时，看到的是去掉了注释、换行和局部命名的代码，
而不是带完整中文注释与语义化函数名的源码。

同时**不能**牺牲已有的工程性质：

1. 仓库里**只有源码**：没有第二份需要同步的生成产物，也就没有"改了 `static/` 忘了重新生成"的漂移；
2. 现有验证手段继续读得到可读源码：`internal/webui/embed_test.go` 的静态合约、
   `internal/webui/tests/*.mjs`、`scripts/ui-harness`（无 node 环境下的唯一 UI 验证手段）；
3. 控制台的运行时行为逐条不变：挂载前缀推导（`base.js` 用 `import.meta.url`）、CSP、SPA 回落、缓存头；
4. 不引入 npm 工具链：本仓库构建环境无 npm（node 也不在 `PATH` 上），工具必须是 Go。

## 2. 关键决策

| # | 决策 | 理由 / 被否决的备选 |
|---|---|---|
| D1 | 混淆强度 = **esbuild 压缩 + 局部标识符重命名**（去注释、去换行、`minifySyntax`、`minifyIdentifiers`），**导出名一律保留** | 这是"体积与可读性"的最优性价比点：体积 −41%，语义零风险。**否决**（用户确认）控制流扁平化/字符串加密：需要 npm 的 javascript-obfuscator，体积反而变大、运行变慢，且会让 `embed_test.go` 与 harness 的一大批静态断言失效——那些断言是控制台与后端合约的唯一守卫 |
| D2 | 工具用 **esbuild 的 Go API**（`github.com/evanw/esbuild/pkg/api`），只调 `api.Transform`（**逐文件、不 bundle、不 splitting**） | 逐文件转译保持"同名文件 + 同相对 import 说明符 + 原生 ESM"这一形态，于是 CSP、缓存头、SPA 回落、`base.js` 的挂载推导全部无需改动。**否决** bundle：`router.js` 的 `import(route.module)` 是变量，esbuild 无法静态解析，页面要么丢失、要么必须改成静态导入（丢掉懒加载），收益仅是省掉同源小请求 |
| D3 | 交由 esbuild 的**局部**重命名，**绝不**碰属性名/字符串 | `--mangle-props` 会改到 `el('button', {class:…})` 这类属性键、`{value:'inherit'}` 这类服务端枚举、以及 DOM id（`#form_`/`#f_`/`#b_` 是模型协议的一部分）。收益有限、风险致命，不做 |
| D4 | 产物**不进仓库**：`make build` 先把资源压缩到 `.cache/ui-dist/`，再用 **`go build -overlay`** 把它嵌进二进制 | 已实测：`-overlay` 对 `//go:embed` 生效、支持相对路径、多余条目无害，`go test`/`go vet` 不带 overlay 时读的仍是源码。于是**工作区一个字节都不被改写**，`git status` 干净，回滚就是"不带 overlay 再编译一次"。**否决**（用户确认）在仓库里提交 `static-dist/`：多 330 KB 生成代码、每次改前端都要重新生成，且要靠一条测试防漂移 |
| D5 | **不做**传输层预压缩（.br/.gz + `Content-Encoding`） | 控制台是同源控制面、内网/同机访问，收益远小于它所牵动的面：embed 形态、`Vary` 头、缓存策略、`Accept-Encoding` 协商。用户确认本次不做 |
| D6 | 服务层 `embed.go` **逻辑零改动**，只改包注释 | 镜像与源码同名同路径、同为原生 ESM，`Handler()` 的 `fs.Sub`/SPA 回落/`setCacheHeaders`/CSP 全部照旧。改动面越小越好 |
| D7 | 压缩失败**必须让构建失败**，绝不回落成"悄悄用源码" | 回落的后果是"发出去的是可读代码而 nobody knows"，与本次目的直接相反。宁可 `make build` 红 |
| D8 | 镜像**原子落盘**（临时目录 → rename），overlay JSON 最后写 | 中断的构建（^C、磁盘满）不能给编译器留下半个镜像——那会编出一个"少几个页面"的控制台 |
| D9 | 除 `.js` / `.css` 外**逐字节复制** | `index.html` 是外壳（`embed_test` 断言其中 `<div id="app">` 等字面量）、`favicon.svg` 是 SVG。任何"顺手也压一下 HTML"的想法都要重新论证，不在本次范围 |
| D10 | 仍然提供 `make build-src`（不混淆）作为显式的调试/对照入口 | 见第 6 节脚枪。可读版是排查线上问题的唯一手段（不发布 sourcemap，那会抵消混淆本身） |

## 3. 接口

### 3.1 `internal/webui/minify`（新包）

```go
package minify

// Options 描述一次镜像生成：源目录、镜像目录、overlay 文件。
type Options struct {
	SourceDir   string // 例如 internal/webui/static
	OutputDir   string // 例如 .cache/ui-dist/static
	OverlayPath string // 例如 .cache/ui-dist/overlay.json（空则不写）
}

// FileResult 是一个文件的压缩结果。
type FileResult struct {
	Path        string // 相对路径，斜杠分隔
	SourceBytes int
	OutputBytes int
	Changed     bool // 内容与源不同（.js/.css 之外恒为 false）
}

// Result 是一次镜像生成的汇总，供 CLI 打印与测试断言。
type Result struct {
	Files       []FileResult // 按 Path 排序，遍历顺序稳定
	SourceBytes int
	OutputBytes int
	Elapsed     time.Duration
}

// Run 生成镜像并写出 overlay。
func Run(opts Options) (Result, error)

// Transform 暴露单文件转译（测试用；esbuild 的选项集中在这里，别处不再重复）。
func Transform(source []byte, isCSS bool) ([]byte, error)
```

- `.js`：`LoaderJS` + `FormatESModule` + `TargetES2020` + `MinifyWhitespace/Identifiers/Syntax` +
  `LegalCommentsNone` + `CharsetUTF8`。
- `.css`：`LoaderCSS` + `MinifyWhitespace` + `MinifySyntax`（**不** `MinifyIdentifiers`），
  保留全部自定义属性与 `@media`。
- 目标定 `ES2020` 而非 `ESNext`：源码里已用到 `?.` 与 `??`，再新的语法目前没有；定在 ES2020 让
  "未来有人写了 ES2022+ 语法"变成一次显式的选项复核，而不是悄悄产出老浏览器跑不了的代码。
- overlay JSON 形如 `{"Replace": {"<绝对源路径>": "<绝对镜像路径>"}}`，只列 `Changed` 的文件。

### 3.2 `cmd/minifyui`（新命令）

```
minifyui -src internal/webui/static -out .cache/ui-dist/static -overlay .cache/ui-dist/overlay.json
```

逐文件打印 `路径  源体积 → 产物体积`，末行打印
`TOTAL 37 files 559442 -> 329702 (-41%)`；失败时打印文件名与 esbuild 的诊断并退非 0。

它**不是** `cmd/aigw` 的子命令：esbuild 的 Go API 会给二进制加约 10 MB，绝不能进发布产物。
产物与工具都落在 `.cache/`（`.gitignore` 已覆盖 `/.cache/`），`bin/` 只放对外产物。

### 3.3 `Makefile`

```make
UIDIST     ?= $(CURDIR)/.cache/ui-dist
UI_OVERLAY ?= $(UIDIST)/overlay.json

ui-dist:                       # 生成压缩镜像 + overlay（并打印体积汇总）
build: version-check ui-dist    # go build -overlay $(UI_OVERLAY) -o bin/aigw ./cmd/aigw
build-src: version-check        # 不混淆，并显式打印 "ui: source assets (not minified)"
clean:                          # rm -rf bin $(UIDIST)
```

`build` 是 `release.sh`（skill `release-version`）调用的入口，所以发版路径自动包含混淆，`scripts/` 无需改动。
`verify = vet + test + ui-base + build` 不变，`build` 依赖 `ui-dist` 后自动覆盖这条路径。

## 4. 数据流

```
internal/webui/static/            ← 唯一真值（源码，进 git）
        │  go build ./cmd/minifyui （工具，落 .cache/ui-dist/minifyui）
        ▼
cmd/minifyui ── api.Transform（逐文件）──► .cache/ui-dist/static/   （同名同结构的原生 ESM）
        └────────────────────────────────► .cache/ui-dist/overlay.json
                                                     │
                    make build: go build -overlay=$UI_OVERLAY ./cmd/aigw
                                                     ▼
                              bin/aigw（go:embed static → 实际读到压缩镜像）
```

`go vet` / `go test ./...` / `make ui-base` / `make ui-check` 都**不带** overlay，读源码。

## 5. 异常与边界

| 情况 | 处理 |
|---|---|
| 某个 `.js` 语法错误（esbuild 报错） | `Run` 返回 error（含文件路径），`make build` 失败；不写镜像、不写 overlay、不留临时目录 |
| 构建被 ^C | 镜像目录用 `os.Rename` 原子替换，中断只会留下 `static.tmp-<pid>`，由下次运行清理 |
| 源目录新增文件/子目录 | `filepath.WalkDir` 全量遍历，1:1 复制→新文件自动进入镜像与 overlay；新增的非 `.js/.css` 按原样复制 |
| 空文件 / 只有注释的 `.js` | esbuild 产出空文件，镜像仍保留该文件（1:1 覆盖是硬约束） |
| 未来引入打包器语法（如 `.ts`、`.json` import 断言） | 不在本次范围：`.ts` 会被当作未知后缀**原样复制**，页面上线时会立刻报错，不会静默降级 |
| 直接 `go build ./cmd/aigw` | 得到**未混淆**版（脚枪，见第 6 节） |
| esbuild 升级 | 输出字节会变（体积/百分比随之变化），但测试断言的是**性质**（导出名、import 图、选择器、变小）而非固定字节，无需重刷基线 |
| 首次构建需要依赖 | `github.com/evanw/esbuild`，一次下载后进 `$HOME/go/pkg/mod`；`scripts/goenv.sh` 的 GOPROXY 已优先 `file://` 缓存，之后离线可构建 |

## 6. 脚枪与它的缓解（本设计最需要知情接受的一点）

`make build` 才会带 overlay，`go build ./cmd/aigw` 不会。缓解手段只有三条，都不完美，因此写在这里：

1. `make build` 是 README 与 skill 里**唯一**记录在案的构建入口，`scripts/release.sh` 也走它；
2. `make build-src` 把"我就要源码版"变成一句显式的话，而不是一次忘记；
3. `make build` 的输出里有一行 `ui: minified 37 files …(-41%)`，`make build-src` 打印
   `ui: source assets (not minified)`——看日志就知道刚才是哪条路。

不采用"把嵌入目录改成只有构建后才存在"这种硬护栏：那会让 `go test ./...` 无法编译 `internal/webui`，
把整个测试矩阵一起赔进去，代价大于收益。

## 7. 测试策略

**新增 `internal/webui/minify/minify_test.go`**（`go test` 直接跑，输出到 `t.TempDir()`，对真实 `../static` 树操作）：

1. **1:1 覆盖**：文件集完全相同；非 `.js/.css` 逐字节相同；每个 `.js/.css` 都变小且非空。
2. **导出名保留**：按 `embed_test.go` 同款正则抽取每个源文件的导出名，断言都出现在镜像里
   （`export function render` / `export{… as render}` 两种形态都算命中）。这是页面间合约与静态测试的前提。
3. **import 图闭合**：逐文件 import 说明符集合与源码一致，且每个相对说明符在镜像里都能解析到实体文件
   ——"少复制一个文件"必须变成一次测试失败，而不是线上某页 404。
4. **CSS 契约**：归一化后的选择器集合、11 个自定义属性、`@media` 数量全部保留。
5. **overlay 完整**：每个 `Changed` 文件都有条目，且 `Changed` 集合非空（防"什么都没压"却报成功）。
6. **确定性与原子性**：连续两次生成逐字节相同；预置一个残留文件的旧 `-out` 目录，生成后无残留。
7. **失败路径**：临时目录里放一个语法坏掉的 `.js` → 返回 error（含路径），且不留下输出目录。
8. **变异验证**（仓库惯例）：让遍历跳过 `js/pages/*.js`，测试必须变红。

**端到端验收**：

| 步骤 | 期望 |
|---|---|
| `make ui-dist` | `37 files 559442 -> 329702 (-41%)`；overlay 35 条 |
| `make build` / `make build-src` | 两个二进制体积差 ≈ −229 KB；`strings bin/aigw \| grep -c renderShell` 为 0，`record_output_text` 仍 >0 |
| `go vet ./...`、`go test ./...` | 与基线一致（干净 / 37 ok） |
| 走查比对 | `make ui-check` 与 `UI_STATIC_DIR=$PWD/.cache/ui-dist/static make ui-check` 逐视图一致（同为 18 ok / 3 FAIL，check 数相同） |
| 隔离端口冒烟 | `/admin/ui/js/app.js` 200 且 `Content-Type: text/javascript`、无注释、`export const` 计数为 0、< 4 KB；`/js/pages/keys.js` 含 `record_output_text`；`/admin/ui/` 仍带 CSP；端口随后释放 |
| 回读 | `make build-src` 的二进制回到源码形态（证明 overlay 流程无副作用） |

`scripts/ui-harness/run.sh` 增加 `UI_STATIC_DIR` 环境变量（默认仍是 `internal/webui/static`），
于是同一套浏览器走查既能跑源码也能跑压缩产物。**这是"压缩没有改变行为"的唯一硬证据**：
Go 侧的静态合约测试读的是源码，天然看不到"只有压缩后才发生"的回归，这条分工要写清楚。

## 8. 依赖

- 新增 Go 依赖：`github.com/evanw/esbuild v0.28.2`（纯 Go、无需 CGO，只用于 `cmd/minifyui` 与 `internal/webui/minify`）。
- `internal/arch/layering_test.go` 的 allowed 表新增 `"internal/webui/minify": nil`（该包不 import 任何模块内包）；
  `cmd/*` 本来就被跳过。`internal/webui` 自身仍是 `nil`（`minify` 不被它 import）。
- 不新增 npm/node 依赖。

## 9. 实现与设计差异

按设计实现，以下 5 处是写代码时才发现、需要记下来的调整：

1. **百分比做四舍五入**（设计的算式是整除）。`100-100*329702/559442` 整除得 **42**，而真实降幅是 **41.07%**。
   这个数字要写进设计文档与发布记录，**多报一个百分点**是不能接受的，于是抽了 `percentSaved`（`math.Round`），
   `Result.Percent()` 与逐文件的 `Result.PercentOf()` 共用它。
2. **CSS 选择器比对要先归一化组合符周围的空格**。esbuild 会把 `a > b` 压成 `a>b`，第一版测试直接
   字符串比对，于是 8 条规则被同时报成"丢失"+"凭空出现"。改成只折叠空白与组合符两侧空格
   （`a b` 后代选择器仍与 `a>b` 区分）后，265 条选择器逐条相等。这是测试写错，不是压缩出错。
3. **动态 import 的断言不能匹配字面量**。`router.js` 是 `import(route.module)`——说明符是个变量
   （这正是不能 bundle 的原因）。测试改成断言"存在 `import(` 调用形状"，另加"路由器仍引用
   `./pages/*.js`"两条，才真正钉住懒加载页面表。
4. **`Run` 的返回值多了一个 `Warnings`**：esbuild 的 warning 要能出现在 `make build` 的输出里
   （`minifyui` 打到 stderr），否则"压缩成功了但有个可疑写法"就没人看见。
5. **`make ui-dist` 每次都重建 `minifyui` 到 `.cache/ui-dist/minifyui`**，而不是写进 `bin/`：
   `bin/` 只放对外产物（二进制与 `aigw-provider-*`），工具是 11 MB 的中间物。

另外验证方式本身也修正过一次，值得记下来：`scripts/ui-harness/run.sh` 的 `UI_STATIC_DIR` **先写在文档里、
后写进脚本**，于是第一次"对压缩产物跑走查"实际上复制的仍是源码树（`run.sh` 还没有这个变量），
两边结果当然一致——**一次没有咬合力的验证**。补上变量后重跑，工作目录里的 `js/app.js` 是 3007 B
（源码是 6097 B），这时"逐视图一致"才是真证据。教训与 `docs/PROCESS.md` 里"先证伪再修"同源：
**拿到一致结果时要先确认被测对象真的是被测的那一个**。

## 10. 验收记录（实测）

环境：本机 12th Gen i7-12700K，`source scripts/goenv.sh`，esbuild **v0.28.2**（Go 模块，2026-09-15 引入）。

### 压缩本身

```
$ make ui-dist
ui: minified 37 files 559442 -> 329702 bytes (-41%) in 820ms
ui: overlay -> /home/winger/work/ai_gateway/.cache/ui-dist/overlay.json
```

| 文件（抽样） | 源 | 镜像 |
|---|---|---|
| `js/pages/chat.js` | 65,093 | 27,872 |
| `js/ui.js` | 21,403 | 9,237 |
| `js/app.js` | 6,097 | 3,007 |
| `app.css` | 29,884 | 20,655 |
| `js/pinyin.js` | 137,477 | 112,658 |
| `index.html` / `favicon.svg` | 669 / 349 | 不变（逐字节） |

overlay 35 条（= 被改写文件数）；`index.html` 与 `favicon.svg` 不在其中。

### 二进制

| 构建 | 体积 | 说明 |
|---|---|---|
| `make build-src` | 21,915,749 B | 日志 `ui: source assets (not minified)` |
| `make build` | 21,686,381 B | 日志 `ui: minified 37 files …(-41%)`；**−229,368 B（−1.05%）** |

`strings` 抽查：`renderShell` / `const STATS_SORTS` / `api.post('/routes'` 在源码版为 3/1/1，
在压缩版**全部为 0**；`record_output_text` 两边都是 21（服务端枚举这类字符串合约必须留下）。

### 服务行为 A/B（同一份配置、隔离端口 `:8093` 压缩版 / `:8094` 源码版、各自空库）

| 请求 | 压缩版 | 源码版 |
|---|---|---|
| `/admin/ui/`、`/admin/ui/providers`、`/admin/ui/app.css`、`/admin/ui/js/*` | 200 | 200 |
| `/admin/ui/index.html` | 301（`http.FileServer` 的规范跳转，改动前也是） | 301 |
| `/admin/ui/js/pages/missing.js` | 404 | 404 |
| `js/app.js` / `ui.js` / `pages/chat.js` / `app.css` 字节 | 3007 / 9237 / 27872 / 20655 | 6097 / 21403 / 65093 / 29884 |
| 响应头 | `Content-Type: text/javascript; charset=utf-8`、`Cache-Control: public, max-age=300`、`nosniff`；`/` 带 CSP | 同 |
| `index.html` 内容 | 与源码逐字节相同 | 同 |

两个实例启动日志 `level=ERROR` 均为 **0**，验证后端口释放；在跑的 `:8088`（`0.14.0 / 6dc9082`）全程未受影响。

### 测试

| 检查 | 结果 |
|---|---|
| `go vet ./...` | 干净（改动前也干净） |
| `go test ./...` | **38 ok**（改动前 37 ok；多的是 `internal/webui/minify`） |
| `go test ./internal/webui/minify/ -v` | 7 项全过 |
| 变异验证 | 让 `writeMirror` 跳过 `js/pages/*.js` → `TestMirrorCoversEverySourceFile` 等 5 项变红（"mirror file set differs"），确认断言有咬合力 |
| `make ui-base`（node 在 PATH 上时） | 与改动前**同样 3 过 2 红**（`tags_binding_test.mjs`、`org_tree_test.mjs` 是既有红项；`models_test.mjs` 需 `--experimental-vm-modules`，未挂进 `ui-base`）——**本改动没有引入新失败** |
| `make ui-check`（源码树） | **21 个视图全绿**（docs 26 / requests 97 / chat 110 / tree 57 / org 52 …） |
| `UI_STATIC_DIR=.cache/ui-dist/static make ui-check`（压缩镜像） | **21 个视图全绿，逐视图检查项数量与源码树完全相同**（`diff` 判定 IDENTICAL VERDICTS） |

最后一行是本里程碑的核心证据：同一套真实浏览器断言，跑在压缩产物上与跑在源码上给出同样的结论。

