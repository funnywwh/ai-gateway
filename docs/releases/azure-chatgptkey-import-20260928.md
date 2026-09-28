# gw-b sub2api `azureChatGPTkey` → gw-c:8088 导入报告

日期：2026-09-28 · 目标实例：gw-c（192.0.2.101）`http://127.0.0.1:8088`（ai-gateway v4.7.2）
源：gw-b（198.51.100.101）sub2api `accounts.id=10`「azureChatGPTkey」

## 1. 结果

按你的要求导入了**供应商、模型、价格**，**未动**任何用户 / API Key / 标签 / 计费历史。

| 对象 | id | kind | 行数 |
|---|---|---|---|
| 供应商 `azure` | 23 | `openai-responses` | 4 个文本模型 |
| 供应商 `azure-images` | 25 | `openai-images` | 5 个图像模型 |

对数客模型目录新增 8 项，供应商模型行 9 条，路由 9 条；**每一条行都当场带上了官方成本价**
（`pricing_rules`，USD 微美元/百万 token），不存在「先建行、后补价」的零成本窗口。

### 模型与价格（public → 上游部署，官方 Standard 档 USD/百万）

| 供应商 | public 名 | 上游部署 | 输入 | 缓存命中 | 输出 |
|---|---|---|---|---|---|
| azure | `gpt-5.6-luna` | `gpt-5.6-luna` | $0.20 | $0.02 | $1.20 |
| azure | `gpt-5.6-sol` | `gpt-5.6-sol` | $4.00 | $0.40 | $20.00 |
| azure | `gpt-5.6-terra` | `gpt-5.6-terra` | $2.00 | $0.20 | $12.00 |
| azure | `gpt-4o` | **`gpt-5.6-luna`** | $0.20 | $0.02 | $1.20 |
| azure-images | `gpt-image-1.5` | `gpt-image-1.5` | $5.00 | $1.25 | $32.00（图像输出） |
| azure-images | `gpt-image-2` | `gpt-image-2` | $5.00 | $1.25 | $30.00 |
| azure-images | `gpt-image-2.5` | **`gpt-image-2.5-sunburst`** | $5.00 | $1.25 | $30.00 |
| azure-images | `gpt-image-2.5-flare` | `gpt-image-2.5-flare` | $5.00 | $1.25 | $30.00 |
| azure-images | `gpt-image-2.5-sunburst` | `gpt-image-2.5-sunburst` | $5.00 | $1.25 | $30.00 |

三个文本模型另有 `>272K` 长上下文档（输入/缓存 2×、输出 1.5×）。

## 2. 实测验证（不是推断）

| 层次 | 做法 | 结果 |
|---|---|---|
| 上游部署存在性 | 对 `model_mapping` 的 11 个目标各真发一次 `POST /responses` | 3× 200、4× 400 `unsupported`（图像模型存在）、2× 404（不存在，不建） |
| 文本数据面 | 4 个文本模型各发真实 `/v1/responses` | **全部 HTTP 200**，usage 正常（`input 13 / output 5`） |
| 图像数据面 | 3 个图像模型各发真实 `/v1/images/generations` | **全部 HTTP 200**，返回真实图片（b64 240–363 KB） |
| 计费落账 | 读 `usage_records` | cost 与官方价**逐分对得上**，例如 `gpt-image-2`：9×$5/M + 196×$30/M = **5925 µ$**，库里正是 5925 |
| 价格回读 | 逐行比对 `pricing_rules_json` 与价表原文 | **9/9 逐字一致** |
| 路由映射 | `router/explain` | 9/9 解析出 canonical 且映射通（排除原因是 `not_granted`＝未授权，与既有供应商一致） |
| 无回归 | 与导入前基线逐条比对 | 既有 6 供应商 / 10 模型 / 14 行 / 16 路由 **0 差异** |
| 保密 | 扫报告、脚本、`aigw-local.log` | `sk-…` 命中 **0**；凭据 AES-GCM 加密落库（126 B），`config_json` 内无任何密钥 |

`gpt-4o` 走**重命名**：该 azure 资源的 `model_mapping` 把 `gpt-4o` 指向 `gpt-5.6-luna` 部署，
已按真实部署写进 `upstream_model`（不是照 public 名硬写，否则请求时会 404）。

## 2.1 与 Azure 官方定价页的逐项核对（2026-09-28）

来源：<https://azure.microsoft.com/en-us/pricing/details/cognitive-services/openai-service/>
（Global 档，USD / 百万 token；页面把每格价格以 JSON 内嵌在 `data-amount` 属性里，逐条读出，
不是二手转述）。**9 条行的三个维度全部一致**：

| 我们导入的行 | 输入 | 缓存命中 | 输出 | 判定 |
|---|---|---|---|---|
| `azure/gpt-5.6-luna` | $0.20 | $0.02 | $1.20 | ✓ |
| `azure/gpt-5.6-sol` | $4.00 | $0.40 | $20.00 | ✓ |
| `azure/gpt-5.6-terra` | $2.00 | $0.20 | $12.00 | ✓ |
| `azure/gpt-4o`（部署 `gpt-5.6-luna`） | $0.20 | $0.02 | $1.20 | ✓ |
| `azure-images/gpt-image-1.5` | $5.00 | $1.25 | $32.00 | ✓ |
| `azure-images/gpt-image-2` | $5.00 | $1.25 | $30.00 | ✓ |
| `azure-images/gpt-image-2.5`（部署 `-sunburst`） | $5.00 | $1.25 | $30.00 | ✓ |

长上下文档也对得上：官方 `>272K` 分别是 luna `$0.40/$0.04/$1.80`、sol `$8/$0.8/$30`、
terra `$4/$0.4/$18`，即**输入/缓存 2×、输出 1.5×**，与我们规则里写的倍数完全一致。

两处需注意的口径差异：

1. **官方页面没有 `gpt-image-2.5` 这个 id**（图像家族只有 `GPT-Image-1`、`-1-mini`、`-1.5`、`-2`）。
   这印证了 `official-pricing.sh` 里的注释：裸 `gpt-image-2.5` 不是官方 id。我们给 `-flare` /
   `-sunburst` 按 `gpt-image-2` 同档计价（$5/$1.25/$30），是「2.5 家族与 2 同价」这一既有口径的沿用，
   官方没有可直接对标的公开价——这一条属于**按家族推定**，不是官方逐字核对。
2. **图像输入（reference image）这一维度我们偏低**。官方 `gpt-image-2` 的图像输入是 $8/M
   （缓存 $2），我们没写它的费率，计价引擎按 `image_input → input` 回落，即按文本输入 $5/M 计，
   偏低约 37.5%。这与 `scripts/official-pricing.sh` 文件头「唯一一处刻意的低估」一致，且目前
   **无法通过写入消除**——管理面价格 schema 只接受 5 个维度（`internal/httpapi/admin_pricing_schema.go:29`：
   `input / input_cache_hit / input_cache_miss / output / reasoning`），没有 `image_input` /
   `image_output` 的位置。图像**输出**不受影响：回落链是 `image_output → output`，而我们的
   `output` 里放的正是图像输出价，实测 `gpt-image-2` 的 196 图像输出 token × $30/M = 5880 µ$
   **精确**（`pricing_snapshot_json` 的 `bucketed_dimensions` 记为 `image_output->output`）。


## 3. 两处必须说明的取舍

### 3.1 图像模型单独开了一个供应商 `azure-images`

第一版把 5 个 `gpt-image-*` 建在了 `azure`（`openai-responses`）上，图片请求得到 400
`provider kind openai-responses does not serve image models`。查源码确认这不是配置问题而是设计：
`internal/runtime/dispatcher.go:291` 要求该 kind 实现 `pluginapi.ImageProvider`，而
`openai-responses` 没有。`docs/api-images.md` §6.1 的口径是「一个供应商实例只讲一门协议」。

所以按你的选择拆成两个实例（同一套 azure 凭据）：`azure` 讲 `/responses`，`azure-images`
讲 `/images`。拆分后 3 个图像模型实测全部 200。

### 3.2 两个模型**故意没有建**（上游部署不存在）

- `gpt-6-astra` → 404 `DeploymentNotFound`
- `gpt-image-1` → 其 `model_mapping` 目标是 `gpt-image-2.5`，该部署 404

我没有把这两个名字写进目录去凑数——写进去只会让客户端选中一个必然失败的模型。它们已记入
`data/azure-import/azure-import-plan-*.json` 的 `skipped`，并附上游原始错误。

对比 `docs/sub2api-migration.md` §9.5 记的 2026-09-23 gw-b 结论（当时 `gpt-5.5`、`gpt-6-astra`
服务不了）：本次 `gpt-6-astra` 依旧 404，与该记录一致。

## 4. 工具与产物

- 新增 `scripts/sub2api-azure-import.py`（`plan` / `apply` / `verify` / `patch` / `split` /
  `tags` / `verify-tags`）：与 `sub2api-reimport.py` 的关键区别是**认 `model_mapping`**
  （reimport 忽略它）且**只导一个上游账号**。
- `scripts/official-pricing.sh`：补了 `gpt-image-2.5-flare` / `-sunburst` 两条（价表此前缺这两个
  真实 id，会让整批写入中止），并把它们纳入自检断言。价格仍只在这一处维护。
- gw-c 上（`data/azure-import/`，0600）：
  - `azure-import-plan-20260928-122548.json`、`azure-import-report-20260928-122623.json`（无密钥材料）
  - `baseline-20260928-121011/`（导入前基线，用于无回归比对）
  - `pricing-plan.json`（价表原文）
- 回滚点：`data/backups/pre-azure-import-20260928-122533.sql`（0600，25 KB，
  已实测可还原成 6/10/14/16）。**注意**：先尝试的整库 `.backup`（12 GB）在 900s 超时被杀、
  产物不完整，已删除；本机已有的完整全量备份是 `data/backups/aigw-20260928-033011.db`。

## 5. 遗留与后续建议

1. **没有把 `azure` / `azure-image` 加进 K7QX 或任何真实账号**（按「只挂验证 key」的决定）。
   现有标签 `K7QX` 已含 `providers:["azure"]`，但它**不含 `azure-images`**——注意 key 36 的名字
   就叫「图像 (替换原 sub2api key #24)」，可它现在**用不了图像模型**；要让 K7QX 人群能用图像，
   需给该标签补 `azure-images`（或把 `azure-image` 挂到该账户）。
2. 本次只导入账号 #10。账号 2/3/4/6/7/8/9/11（codex / deepseek 系）**未动**——按 `docs/sub2api-migration.md`
   §9.3 的警告，导入 codex 供应商会涉及 OAuth 凭据搬运与刷新抢占，须先确认 gw-b 侧已停用该账号。
3. gw-c 的 checkout 落后（`VERSION` 记 4.0.0，跑着的二进制是 4.7.2，缺
   `probe-azure-models.py` / `add-provider-models.py` / `sub2api-reimport.py`）。本次刻意**没有**
   整仓更新，只投放了本次需要的文件到 `data/azure-import/`。
4. 自检钥匙留下 4 把 `disabled` 的 `sk-gw_*`：`K7QX` 账户下 id 130/132/134
   （`zz-azure-import-selftest`）、`local` 账户下 id 136（`zz-tag-azure-verify`）。网关没有
   删除 key 的路由，只能置 `disabled`；如需清理请在控制台按名字处理。

## 6. 标签 `azure` / `azure-image` 与全模型验证（2026-09-28）

按你的要求建了两个标签，并用一把**只带这两个标签**的 key 把 9 个模型逐个真跑了一遍。

| 标签 | id | grants |
|---|---|---|
| `azure` | 13 | `{"providers":["azure"],"models":["*"]}` |
| `azure-image` | 14 | `{"providers":["azure-images"],"models":["*"]}` |

> 标签名与供应商名**故意不同**：标签叫 `azure-image`（单数，按你的原话），grants 里写的是供应商
> 真名 `azure-images`（复数）。两者是独立命名空间；且 grants 存的是原始 JSON 字符串，服务端
> **不校验供应商是否存在**（`routing.go:151` 只做 `json.Unmarshal`），写错只会静默不加权——
> 所以下面的真实请求才是唯一证据。另：**标签建后不能改名**（`admin_routes.go:1062`），
> 要换名字只能删掉重建。

### 验证结果：9/9 全部通过

| 检查 | 结果 |
|---|---|
| 负向对照（key 未打标签时请求 `gpt-5.6-luna`） | **HTTP 403 `permission_denied`** —— 证明该 key/账户没有别的 azure 授权，访问权确实来自标签 |
| 打标签后 `effective_tags` | `['azure','azure-image']` |
| `GET /v1/models` | 9 个模型全部可见：`gpt-4o`、`gpt-5.6-luna/sol/terra`、`gpt-image-1.5/2/2.5/2.5-flare/2.5-sunburst` |
| 4 个文本模型（`/v1/responses`） | 全部 **HTTP 200**，usage `input 13 / output 5` |
| 5 个图像模型（`/v1/images/generations`） | 全部 **HTTP 200**，返回真实图片 226–314 KB |

**归属与计费**（读 `usage_records`，`api_key_id=136`）：4 条文本记在 `azure`(23)、5 条图像记在
`azure-images`(25)，状态全 `completed`、成本全 > 0（合计 ≈ $0.164）。
`gpt-4o` 的上游仍如实记为 `gpt-5.6-luna`（重命名映射经标签路径同样生效）。

值得一提：`gpt-image-2.5` 与 `gpt-image-2.5-flare` 是**本次才第一次真出图**（上次只探到 400
「部署存在」），两个都成功——原先担心的 `-flare` 失败没有发生。

### 约束与卫生

- 验证 key（id 136，账户 `local`(1)）**不带自有 grants**，权限只从两个标签来——否则证明不了标签有效。
- 账户选 `local`(1) 而非 `dshgw-e2e-a`(99)：图像每张按 8192 token 预留 ≈$0.25，
  99 只有 $1.00 预付，会撞上 `insufficient_quota`（`docs/design/m51-dshgw.md:485` 有先例），
  那样就分不清「额度不足」还是「授权失败」。`m51-test-a`(98) 按约定保持无密钥，也没用。
- key 最终 **disabled**，明文文件（0600）用完即抹除；报告里扫不到任何 `sk-` 材料。
- 既有 7 个标签与 `local` 账户原有 3 把 key **改动 0 处**（与导入前基线逐条比对）。
- 校验产生的 9 条 `usage_records` 是审计记录，按设计保留。

复现：`python3 data/azure-import/sub2api-azure-import.py tags` 然后 `verify-tags`
（报告落在 `data/azure-import/tag-verify-*.json`，0600）。

