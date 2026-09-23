# M84 设计文档：图片生成（gpt-image 系列 · Images API · `openai-images` 供应商 · `provider.images` 协议）

> 状态：**实现中（M84）**。
> 规格：[docs/api-images.md](../api-images.md)（对客面）、[docs/plugin-protocol-v1.md](../plugin-protocol-v1.md)（协议新增）、
> [docs/routing.md](../routing.md)、[docs/pricing.md](../pricing.md) §1、[docs/billing.md](../billing.md)、
> [docs/request-log.md](../request-log.md)、[docs/dshgw.md](../dshgw.md)。
> 官方口径：OpenAI Images API（`POST /v1/images/generations`、`POST /v1/images/edits`、`stream` +
> `partial_images` 事件 `image_generation.*` / `image_edit.*`、GPT image 模型的 `usage.input_tokens_details`）；
> 本地核对样本是已安装 dsh 0.1.2-rc.1 自带的 `openai` SDK 类型定义（`node_modules/openai/resources/images.d.ts`）。
>
> 需求原话：「aigw 支持 gpt-image 系列模型，支持这类模型的 provider 接口」。
> 现场：`scripts/official-pricing.sh` 已经把 `gpt-image-1 / 1.5 / 2 / 2.5` 的官方价写进 gptjp 的
> `provider_models.pricing_rules_json`（output 取图像输出价），但网关只有 `/v1/responses` 一个数据面入口，
> 这些模型**无法被调用**。

## 1. 目标与非目标

**目标**：aigw 用与 `/v1/responses` 同源的机制服务 **OpenAI Images API**：

| # | 能力 | 落点 |
|---|---|---|
| 1 | `POST /v1/images/generations`（JSON） | 新数据面端点，认证/限速/额度/计费/请求日志与 Responses 面共路径 |
| 2 | `POST /v1/images/edits`（multipart，≤16 张参考图 + 可选 mask） | 同上 |
| 3 | `stream:true` + `partial_images` → SSE（`image_generation.*` / `image_edit.*`） | 事件名与官方一致，官方 SDK 可直接消费 |
| 4 | 内建供应商类型 `openai-images` 对接上游 `/images/*` | `internal/providers/openaiimages` |
| 5 | 插件协议 `provider.images` / `provider.images.stream` + `images` 能力 | 插件类供应商也能服务图片模型（加法式，不破坏既有插件） |
| 6 | 能力键 `image_generation` 参与路由与 `/v1/models` 披露 | 图片请求只落到声明了的候选；文本请求不会落到图片模型 |
| 7 | 计量维度 `input` / `image_input` / `image_output` + 计费回落 | 参考图输入不再只能按文本价估（`official-pricing.sh` 记的「唯一一处低估」被补齐） |

**验收**：

1. 配好 `openai-images`（上游为官方或中转的 `/v1/images/*`）后，用 aigw 的 Key 调
   `POST /v1/images/generations {"model":"gpt-image-2","prompt":"…"}` 返回 200，body 是
   `{created,data:[{b64_json}],usage,size,quality,background,output_format}`。
2. `"stream":true,"partial_images":2` 时返回 `text/event-stream`，先若干
   `image_generation.partial_image`（带 `partial_image_index`），最后一条
   `image_generation.completed`（带最终 `b64_json` 与 `usage`）；edits 对应的前缀是 `image_edit.`。
3. edits 用 multipart 提交（`image[]` 多张、`mask` 可选）成功；`image` / `image[]` / `image[N]` 三种键都收。
4. `usage_records` 里该次尝试的 `dimensions_json` 含 `input`（提示词文本 token）、`image_input`（参考图 token）、
   `image_output`（图像输出 token）；用 gptjp 现有图像价规则集（`output`=图像输出价、`input`=文本输入价）
   复算，`cost_micros` 与 `scripts/official-pricing.sh` 的预期一致。
5. 没有候选声明 `image_generation` 时返回 400（不是 500、也不是把请求发给文本模型）。
6. 图片模型不出现在租户 DSH 的模型菜单、也不出现在控制台智能问答的模型下拉里。
7. `make verify` 全绿；本机临时环境下用 curl 实测非流式/流式/edits/错误路径全部符合预期。

**非目标**（明确不做，避免范围蔓延）：

- **不做** `/v1/responses` 的 `image_generation` 工具。该工具自带 `model` 字段（默认 `gpt-image-1`），
  请求的 `model` 是文本模型，aigw 的路由会按文本模型走 —— 与「aigw 支持 gpt-image 系列模型」想要的
  「按模型名路由、按模型计价」不是一回事（见 D2）。
- **不做** `dall-e-2` / `dall-e-3` 专属参数（`style`、variations 端点）；`response_format` 若客户端发来则透传。
- **不做** Azure 风格的 `deployments/<name>/images/...?api-version=` 路径改写（需要时用 `base_url` + `headers` 变通）。
- **不做** 图片落盘 / 对象存储 / CDN；网关只做 base64 或 url 的搬运。
- **不做** 图片流被中断后的部分计费（见 §9 边界）。
- **不改** `internal/responses`（Responses 面的 assembler、compaction、会话续接一律不参与图片路径）。

## 2. 现状与证据

- 数据面只有三个入口：`POST /v1/responses`、`GET /v1/responses/{id}`、`DELETE /v1/responses/{id}`，
  外加 `GET /v1/models`（`internal/httpapi/server.go` 的 `routes()`）；`admin_routes_test.go:206` 把
  「数据面路由数 = 管理面路由数 + 11」写成断言，加两个端点必须同步改成 +13。
- 供应商只有 `openai-chat` / `openai-responses` / `testecho` 三个内建类型 + `plugin:<name>`
  （`internal/providers/registry.go`），三者的请求形状都是 Responses（`pluginapi.Request`）。
- 计量维度只有 `input` / `input_cache_hit` / `input_cache_miss` / `output` / `reasoning` 五个有采集方；
  `docs/pricing.md` §1 的「扩展」行（`image`、`audio_second`、`tool_call`）至今无人产出。
  `scripts/official-pricing.sh` 因此把 gpt-image 的**图像输出价写进 `output`**、把参考图输入 token
  按文本价计，并在标题里标注这是「唯一一处低估」。
- 计费引擎本身不关心维度单位（`pricing.RateScale` 的注释：tokens、images、seconds 同一套），
  缺费率的兜底只有两条：`input → input_cache_miss`、`reasoning → output`（`dimensionFallbacks`）。
- 图片能力键已存在但是**输入**语义：`capabilities.image` = 支持 `input_image`（M68）。
  重新定义它会破坏 gptjp 上四条 deepseek 映射行的含义，因此新增独立键（见 D3）。

## 3. 关键决策

### D1 对客面用 Images API，两个端点都做

客户端拿 `model` 名直接调用，路由、模型映射、计价都按这个名字走，与既有模型中心天然一致；
生成与编辑都要（编辑是 gpt-image 的常用形态：给参考图改图）。OpenAI SDK 的
`client.images.generate` / `client.images.edit` 是两条路径、两种编码（JSON / multipart），
所以网关侧也是两个处理器 + 一份共享的 dispatch/settle 逻辑。

*取舍*：不做 Responses 的 `image_generation` 工具，代价是「在文本对话里顺手生图」这条路不通；
收益是路由/计价语义干净，且不需要动 Responses 的 assembler 与请求日志口径。

### D2 图片模型是「模型」，不是「工具」

`provider_models` 行 + 路由 + 候选 + 在途额度这套机制原样复用，`model` 名就是 gpt-image 名。
图片请求的 `Features` 是 `{"image_generation": true}`（edits 再加 `{"image": true}`），
`checkCapabilities` 不需要改一行。

### D3 新能力键 `image_generation`；`image` 保持「支持图片输入」

- `image_generation`：该模型服务 Images API、**输出**图片。
- `image`：该模型接受图片**输入**（M68 语义不变）。edits 需要两者同时声明（参考图是真输入）。

### D4 图片请求的能力门禁强制 `reject`

`routing.degradation` 默认 `strip`：能力缺失只标记 `Degraded`、仍会派发。对聊天请求这是合理的
（降级总比失败好），对图片请求则是灾难 —— 请求会打到只会聊天的模型上，上游必然 400，还白花一次额度。
所以图片处理路径在 `Plan` 之后**丢弃** `Degraded` 含 `image_generation` / `image` 的候选，
等价于这两个键强制 `reject`。`checkCapabilities` 与其配置语义保持不动（避免影响既有行为）。

### D5 规范化契约新增图片请求/响应，而不是塞进 `pluginapi.Request`

`Request` 是 Responses 形状（instructions/input/tools/…），图片请求的字段集合几乎不重叠；
硬塞会让每个 provider 都要判「这次到底是哪种请求」。新增 `ImageRequest` / `ImageResponse` 与两个协议方法，
`Request`/`Response` 一个字节不改（既有插件照常编译、照常工作）。

### D6 插件协议是加法式扩展，帧上限提高是唯一的兼容性事项

- 新方法 `provider.images`（一元）/ `provider.images.stream`（事件流）；新能力 `images`。
- `Provider` 接口**不动**：新增可选接口 `ImageProvider`，`Serve` 用类型断言探测，未实现则回
  `unsupported_method`（与 `SchemaProvider` 同一手法）。
- `MaxFrameBytes` 8 MiB → 64 MiB：base64 图片单帧十几 MB 很正常；`MaxFrameBytes` 同时是编解码两侧的
  缓冲上限，旧插件只会写 ≤8 MiB（新宿主照读），**新插件写 >8 MiB 时旧宿主会读不了** —— 两侧同批发布，
  写进协议文档的兼容性说明。

### D7 计量拆成 `image_input` / `image_output`，并加回落

上游 GPT image 模型报的是 `usage.input_tokens_details{text_tokens,image_tokens}` 与
`usage.output_tokens`（图像 token）。映射：

| 上游字段 | 网关维度 | 依据 |
|---|---|---|
| `input_tokens_details.text_tokens` | `input` | 提示词文本 token，与文本模型同维度 |
| `input_tokens_details.image_tokens` | `image_input` | 参考图 token（官方另有图像输入价） |
| `output_tokens` | `image_output` | 图像输出 token（官方图像输出价） |
| 缺 `input_tokens_details` 时 `input_tokens` | `input` | 中转可能不报明细，按文本输入价计（保守向：图像输入价 ≥ 文本输入价） |

`dimensionFallbacks` 加 `image_input → input_cache_miss`、`image_output → output`，
于是 gptjp 现有规则集**不改一个数**就能算出今天的预期值，而想精确计价的操作者可以补两条费率。
**不能**写成 `image_input → input`：那会与既有 `input → input_cache_miss` 形成两跳链，
而 `WorstCaseRates` 的注释明确要求回落图无链、一次遍历即收敛。

不新增 `image`（张数）维度：`per_request_fee_micros` 已经能表达「按请求一笔」，
再造计数维度只会给没配费率的部署刷 `unpriced_dimensions` WARN。

### D8 预留按「张数 × 每张输出 token 上限」估

图片没有 `max_output_tokens` 可用，`reservation_mode: max_tokens` 会因为 `MaxOutputTokens=0`
只预留一个提示词的钱，预付费账户能被一次性打穿。新配置 `billing.images_reserve_tokens`（默认 8192）
给出「每张图的输出 token 预留」，据此：

```
ImageOutputTokens = n × images_reserve_tokens      （按 image_output 最贵费率）
ImageInputTokens  = len(参考图) × images_reserve_tokens （按 image_input 最贵费率）
EstInputTokens    = 提示词估算 token                （按 input 最贵费率）
```

多预留的部分结算时释放（沿用既定「宁可多预留」口径，`docs/billing.md`）。

### D9 图片模型在 `/v1/models` 上用 `output_modalities` 自我声明

`output_modalities` 与既有 `input_modalities` 对称：无能力声明时**省略**（客户端按文本模型处理，
老客户端行为不变）；有声明时给 `["text"]`，`image_generation` 为真时给 `["image"]`。
dshgw 渲染租户 settings 与控制台智能问答据此**跳过**非文本输出的模型（DSH 只做文本对话，
把它列进模型菜单等于给用户一个必错的选项）。

### D10 请求日志记参数与图片元数据，永不记图片字节

`request_logs.Endpoint` 记 `/v1/images/generations` 或 `/v1/images/edits`；正文只记 prompt 文本
（仍走 `recordInput` 的档位与 `redact_paths`）与参数，参考图只记 `{name,mime,bytes}`。
一次 edits 的 base64 正文可达几十 MB，进日志既撑爆库也毫无审计价值。

### D11 流式：上游事件翻译成对客事件，usage 在 completed 帧

- provider → host 的规范化事件：`image.partial`（`Image.B64JSON` + `partial_image_index`）与
  `image.completed`（最终图 + 回显参数），随后照既有约定发 `usage` + `finish`（`finish` 是终止声明）。
- host → 客户端：`image_generation.partial_image` / `image_generation.completed`
  （edits 用 `image_edit.*`），字段名与官方一致（`b64_json`、`created_at`、`size`、`quality`、
  `background`、`output_format`、`partial_image_index`、`usage`）。
- 上游缺终态（连接断了、没发 completed）→ 与聊天流同样判失败，绝不把半张图当成品。

## 4. 契约一：对客 Images API

### 4.1 `POST /v1/images/generations`

```json
{
  "model": "gpt-image-2",
  "prompt": "a red fox reading a book",
  "n": 1,
  "size": "1024x1024",
  "quality": "high",
  "background": "auto",
  "output_format": "png",
  "output_compression": 100,
  "moderation": "auto",
  "partial_images": 0,
  "stream": false,
  "user": null
}
```

- `model` / `prompt` 必填；`n` ∈ [1,10]；`partial_images` ∈ [0,3]（仅 `stream:true` 时有意义）。
- 其余参数不做值域校验，原样透传给上游（上游是权威；网关拒绝一个上游接受的取值只会造成困惑）。
- 未知顶层字段进 `Extra` 透传（与 Responses 面同风格），但**不**参与日志正文的展示。

响应（非流式）是官方形状：

```json
{
  "created": 1767225600,
  "data": [{"b64_json": "iVBORw0KG..."}],
  "usage": {"input_tokens": 25, "input_tokens_details": {"text_tokens": 25, "image_tokens": 0},
            "output_tokens": 4160, "total_tokens": 4185},
  "size": "1024x1024", "quality": "high", "background": "auto", "output_format": "png"
}
```

### 4.2 `POST /v1/images/edits`（multipart/form-data）

| 字段 | 类型 | 说明 |
|---|---|---|
| `image` | file ×N | 参考图，1..16 张；同时接受 `image[]`、`image[N]` 三种键名 |
| `mask` | file | 可选，PNG |
| `prompt` | text | 必填 |
| `model` | text | 必填 |
| `n` / `size` / `quality` / `background` / `output_format` / `output_compression` / `input_fidelity` / `partial_images` / `stream` / `user` | text | 同 generations |

响应与 generations 相同；流式事件名前缀换成 `image_edit.`。

### 4.3 认证、限速、额度、错误

- 认证：`Authorization: Bearer <key>` 或 `X-API-Key`，与数据面同一验证器（`s.authenticate`）。
- 限速：`Limiter.Reserve`（请求数/并发/额度），结束时按实际 token 用量 `Settle`。
- 余额：进入上游之前先 `Billing.Admit`（D8 的预留），失败走既有 `rejectForQuota`。
- 错误体与 Responses 面同一封装（`{error:{message,type,code,param}}`），错误码沿用
  `invalid_request` / `model_not_found` / `unsupported` / `rate_limited` / `insufficient_quota` /
  `upstream_error` / `provider_busy` 等既有集合。
- 请求体上限：`server.images_max_body_bytes`（默认 32 MiB）只作用于这两个端点，
  超限返回 413（`invalid_request` + 说明上限）。`server.max_body_bytes`（10 MiB）保持给 Responses 面。

## 5. 契约二：插件协议新增（`pkg/pluginapi`）

```go
// 方法
const (
    MethodImages       = "provider.images"        // 一元：返回 ImageResponse
    MethodImagesStream = "provider.images.stream" // 事件流：image.partial* → image.completed → usage → finish
)

// 能力
type Capabilities struct { /* … */ Images bool `json:"images,omitempty"` }

// 可选接口：实现它并在握手里声明 images=true，宿主才会把图片请求派给你
type ImageProvider interface {
    Images(ctx context.Context, req *ImageRequest) (*ImageResponse, error)
    ImagesStream(ctx context.Context, req *ImageRequest, emit func(Event) error) error
}

type ImageRequest struct {
    Model  string `json:"model"`
    Op     string `json:"op"`     // ImageOpGenerate | ImageOpEdit
    Prompt string `json:"prompt"`
    N int `json:"n,omitempty"`
    Size, Quality, Background, OutputFormat, Moderation, InputFidelity, ResponseFormat, User string
    OutputCompression *int  `json:"output_compression,omitempty"`
    PartialImages     int   `json:"partial_images,omitempty"`
    Stream            bool  `json:"stream,omitempty"`
    Images            []ImageInput `json:"images,omitempty"`
    Mask              *ImageInput  `json:"mask,omitempty"`
    Extra             map[string]json.RawMessage `json:"-"`
}

type ImageInput struct { Name, MIME string; Data []byte } // Data 走 JSON base64
type Image     struct { URL, B64JSON, RevisedPrompt string }
type ImageResponse struct {
    Created int64; Data []Image; Usage Usage
    Size, Quality, Background, OutputFormat string
}

// 事件：payload 收在一个子对象里，不往 Event 上摊七个字段
type ImageEvent struct {
    B64JSON string; PartialIndex int; Created int64
    Size, Quality, Background, OutputFormat string
}
type Event struct { /* …既有字段… */ Image *ImageEvent `json:"image,omitempty"` }
const (
    EventImagePartial   = "image.partial"
    EventImageCompleted = "image.completed"
)
```

- `MaxFrameBytes`：8 MiB → **64 MiB**。
- 宿主侧派发：`runtime.Dispatcher.Images` / `ImagesStream`，容量闸、熔断观测、等待时长统计与
  `Complete` / `Stream` 完全一致；内建供应商走 `pluginapi.ImageProvider` 断言（不实现即 `unsupported`），
  插件走 `client.Images` / `client.ImagesStream`。

## 6. 契约三：内建供应商 `openai-images`（`internal/providers/openaiimages`）

配置形状与 `openai-responses` 对齐（少一份认知负担）：

```json
{
  "base_url": "https://api.openai.com/v1",
  "api_key": "",
  "headers": {},
  "timeout_s": 300,
  "models": [
    {"public": "gpt-image-2", "upstream": "gpt-image-2",
     "capabilities": {"image_generation": true, "image": true}}
  ]
}
```

- `timeout_s` 默认 **300**（生图数十秒到数分钟；Responses 面默认 120 不够）。注意
  `routing.per_attempt_timeout_s` 目前是**死配置**（全仓库只出现在 config.go），真正生效的是这里。
- `ListModels` 返回声明的目录（能力一起带出去，供 `refresh provider models` 落成映射行）；
  因此「控制台新建实例 → 测试/刷新模型」这条既有链路对新类型直接可用。
- `Health`：`GET {base}/models`。中转没有这个端点时探测会报错，但数据面不受影响（文档写明）。
- `Images`：`generate` → `POST {base}/images/generations`（JSON）；`edit` → `POST {base}/images/edits`
  （multipart，参考图逐张写 `image[]`、mask 写 `mask`）。
- `ImagesStream`：请求体带 `stream:true` + `partial_images`，用 `providerkit.NewSSEReader` 解析上游事件
  （`image_generation.*` 与 `image_edit.*` 两族都认），翻译成规范化事件；缺终态 → 可重试错误。
- `Complete` / `Stream`（Responses 形状）返回**非可重试**错误 `image_model_only`，文案指向
  `POST /v1/images/generations` —— 文本请求误打到图片模型时，报错要能自解释。
- 错误分类沿用 `httpx.ErrorFromResponse`（401/403 fatal、429 → quota 冷却、5xx/网络 → 可重试）。

注册点：`internal/providers/{registry.go,schema.go}`、`internal/providers/schema_test.go` 的 `kindConfigs()`、
`internal/arch/layering_test.go` 的允许导入表。控制台「内建类型说明」由 `providers.Schemas()` 驱动，自动出现。

## 7. 契约四：路由与披露

- `featuresOf` 的图片版（图片处理器内）：`{"image_generation": true}`，edits 追加 `{"image": true}`。
- `Plan` 之后丢弃 `Degraded` 含这两个键的候选（D4）；全部被丢弃 → 400 `unsupported`。
- `routing.ModelFacts` 增 `ImageGeneration bool`（候选声明并集）。
- `/v1/models` 增 `output_modalities`（D9）。
- **文本请求打到图片模型**：路由不预过滤，由供应商返回 `image_model_only`（非可重试，
  带可读文案）。这是刻意的取舍：加「输出模态判定」要把模型类型概念引入路由，
  而收益只是把一次 400 提前成另一次 400。

## 8. 数据流

```
client ──POST /v1/images/generations|edits──► httpapi.images
   │  parse(JSON|multipart) → ImageRequest(规范化)
   │  authenticate → limitsFor/Limiter.Reserve → Router.Plan(Features{image_generation[,image]})
   │  → 丢弃 degraded 候选 → Billing.Admit(预留含图像 token 估算)
   ▼
Dispatcher.Images / ImagesStream(providerID, *ImageRequest)
   ├─ 内建 openai-images ─► POST {upstream}/images/generations|edits
   └─ 插件 provider.images(.stream) ─► NDJSON 帧
   ▼
ImageResponse / image.* 事件 ──► httpapi
   │  usage → dims{input,image_input,image_output} → settleAttempt（usage_records + 账本）
   │  → request_logs 行（Endpoint + prompt + 参数 + 图片元数据）
   ▼
client ◄── JSON {created,data[],usage,…} 或 SSE image_generation.* / image_edit.*
```

## 9. 异常与边界

| 场景 | 行为 |
|---|---|
| body 超 `images_max_body_bytes` | 413 + `invalid_request`（文案含上限） |
| 缺 `model`/`prompt`、`n` 越界、`partial_images` 越界 | 400 `invalid_request`，进 `recordDenied` |
| 模型不存在 / 无路由 | 400 `model_not_found`（沿用 `Plan` 语义） |
| Key 未授权 / 余额不足 / 限速 | 403 / 402(或 429) / 429，与 Responses 面同源 |
| 候选未声明 `image_generation`（含 `strip` 模式） | 400 `unsupported`：没有供应商能服务图片生成 |
| 上游 4xx | fatal，不故障切换（既有分类：客户端错误换供应商也一样） |
| 上游 5xx / 超时 / 断流 / 缺 completed 帧 | 可重试 → 换下一候选，最多 `routing.max_attempts` |
| 单帧 > 64 MiB（如 n=10 大图经插件供应商） | 明确报错（协议层），文档建议降 `n` 或换 `jpeg` |
| 客户端中途断开 | 取消上游、usage 记 0、该次尝试标失败；**不**做部分计费（缺口，文档写明） |
| 上游只返回 `url`（部分中转的 dall-e 兼容行为） | 原样透传 `data[].url` |
| 文本请求打到图片模型 | 供应商 `image_model_only`（400，非可重试） |
| 图片模型出现在 `/v1/models` | 带 `output_modalities:["image"]`；dshgw 与控制台智能问答过滤 |

## 10. 测试策略

- `pkg/pluginapi`：`provider.images` / `provider.images.stream` 的 client↔serve 往返（含 `ImageRequest`
  的 base64 参考图）、未实现 `ImageProvider` 时报 `unsupported_method`、事件 payload 的 JSON 形状。
- `internal/providers/openaiimages`（`httptest` 上游）：
  - generations JSON 全字段透传与响应解析；
  - edits multipart 的键名（`image[]`、`mask`）与文本字段；
  - usage → 维度映射（含缺 `input_tokens_details` 的退化路径与完全无 usage 的估算路径）；
  - SSE 翻译（partial ×k → completed）与缺终态判失败；
  - 上游 400/401/429/5xx 的分类与可重试性；
  - 文本请求（`Complete`/`Stream`）返回 `image_model_only`。
- `internal/providers`：`schema_test` 反射校验覆盖新类型（Config 字段必须有说明）；
  `registry_test` 覆盖新 kind 的构建与未知 kind 报错。
- `internal/routing`：`image_generation` / `image` 的门禁（含 `strip` 模式下仍被图片路径拒绝）、
  `ModelFacts.ImageGeneration` 并集、未知能力不拦。
- `internal/pricing`：两条新回落的计价、`WorstCaseRates` 单趟收敛（回落图无链）。
- `internal/billing`：带图像 token 的预留大于只算提示词的预留；缺费率回退默认预留。
- `internal/httpapi`：两端点端到端 —— 认证、限速、额度拒绝、超限 413、能力缺失 400、
  非流式响应形状、流式事件名与顺序、usage 维度落库与 `cost_micros`、请求日志行
  （Endpoint、prompt、图片元数据、**不含 base64**）；`admin_routes_test.go` 的 +11 → +13。
- `internal/dshgw/tenancy`：图片模型不进租户 settings.yaml（golden）。
- `internal/arch`：新包的分层边。

验收命令（本机）见 `docs/api-images.md` §9。

## 11. 依赖

- 无新增第三方依赖（multipart 用标准库 `mime/multipart`）。
- 上游需实现 `/v1/images/generations`（以及 edits 时的 `/v1/images/edits`）。
- 与 M68 的边界：`image` 键语义不变，deepseek 四条映射行不受影响；M68 的 `input_modalities` 逻辑不动。

## 12. 实现与设计差异

（实现完成后回填。）

## 13. 回滚

- 代码级：镜像/二进制回退到上一版本即可；两个新端点在旧二进制上直接 404，算子侧无残留状态。
- 数据级：新增的配置键（`server.images_max_body_bytes`、`billing.images_reserve_tokens`）都有默认值，
  旧配置无需改动；新供应商类型只是多一个 kind，不建实例就不参与路由。
- 计量维度：旧版本不产生 `image_input` / `image_output`，历史 `usage_records` 不受影响
  （计价引擎按维度取名，回落规则对新旧版本都成立）。
