# Images API 兼容面（gpt-image 系列）

> 状态：**规格（M84 实现）**。设计文档：`docs/design/m84-image-generation.md`。
> 相关：`docs/api-responses.md`（Responses 面）、`docs/api-providers.md`（`openai-chat`）、
> `docs/plugin-protocol-v1.md`（`provider.images`）、`docs/pricing.md` §1（图像 token 维度）、
> `docs/billing.md`（图片预留）、`docs/request-log.md`（图片请求的日志口径）。

## 1. 范围

网关额外提供 **OpenAI Images API** 的两个端点，用来服务 `gpt-image-*` 这类**输出图片**的模型：

| 端点 | 编码 | 用途 |
|---|---|---|
| `POST /v1/images/generations` | `application/json` | 提示词生图 |
| `POST /v1/images/edits` | `multipart/form-data` | 给参考图改图（≤16 张 + 可选 mask） |

两个端点的 `model` 字段就是网关的模型名：它参与模型映射、路由、候选过滤与计价，与
`/v1/responses` 完全同一套机制。图片模型必须是**上游目录里声明过**的（见 §6），
并在映射行上声明能力键 `image_generation`。

不做：`/v1/images/variations`、`dall-e` 专属参数（`style`）、Responses 的 `image_generation` 工具、
Azure 路径改写。需要 Azure 时用 `base_url` + `headers` 变通。

## 2. 认证与限速

与数据面一致：`Authorization: Bearer <api key>` 或 `X-API-Key`；限速与并发用该 Key 的
请求数/并发上限，余额在进入上游之前先按预留检查（见 §7）。请求体上限是
`server.images_max_body_bytes`（默认 32 MiB，`config.example.yaml`）；超过返回 413。

响应头（与 Responses 面同名）：`x-gateway-provider`、`x-gateway-model`；
能力降级时 `x-gateway-degraded`（图片请求不降级，见 §5）。

## 3. `POST /v1/images/generations`

```json
{
  "model": "gpt-image-2",
  "prompt": "a red fox reading a book, watercolor",
  "n": 1,
  "size": "1024x1024",
  "quality": "high",
  "background": "auto",
  "output_format": "png",
  "output_compression": 100,
  "moderation": "auto",
  "user": "u-1234",
  "stream": false,
  "partial_images": 0
}
```

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `model` | 是 | — | 网关模型名（映射到上游的 `upstream_model`） |
| `prompt` | 是 | — | 提示词 |
| `n` | 否 | 1 | 生成张数，1..10 |
| `size` | 否 | 上游默认 | 如 `1024x1024`、`1536x1024`、`1024x1536`、`auto` |
| `quality` | 否 | 上游默认 | `low` / `medium` / `high` / `auto` |
| `background` | 否 | 上游默认 | `transparent` / `opaque` / `auto` |
| `output_format` | 否 | 上游默认（png） | `png` / `jpeg` / `webp` |
| `output_compression` | 否 | 上游默认 | 0..100（webp/jpeg） |
| `moderation` | 否 | 上游默认 | `low` / `auto` |
| `response_format` | 否 | — | 仅 dall-e 兼容上游有意义；网关透传 |
| `user` | 否 | — | 透传 |
| `stream` | 否 | false | 见 §5 |
| `partial_images` | 否 | 0 | 0..3，仅流式有意义 |

**取值不做值域校验**（除了 `n`/`partial_images` 的边界与必填项）：上游是权威，网关拒绝一个上游
接受的取值只会把问题变成两次排障。未知顶层字段原样透传给供应商。

响应（非流式，HTTP 200）：

```json
{
  "created": 1767225600,
  "data": [{"b64_json": "iVBORw0KGgo…"}],
  "usage": {
    "input_tokens": 25,
    "input_tokens_details": {"text_tokens": 25, "image_tokens": 0},
    "output_tokens": 4160,
    "total_tokens": 4185
  },
  "size": "1024x1024", "quality": "high",
  "background": "auto", "output_format": "png"
}
```

- `data[].b64_json` 是 GPT image 模型的默认形态；上游返回 `url` 时原样放在 `data[].url`。
- `usage` 是 GPT image 模型才有的字段；上游没报时省略（网关仍按估算计量，见 §7）。
- `revised_prompt` 沿上游原样透传（dall-e-3 形态）。

## 4. `POST /v1/images/edits`

`multipart/form-data`：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `image` | file（可重复） | 是 | 参考图 1..16 张；网关同时接受 `image`、`image[]`、`image[N]` 三种写法 |
| `mask` | file | 否 | 透明区即待编辑区 |
| `prompt` | text | 是 | |
| `model` | text | 是 | |
| `n` | text | 否 | 1..10 |
| `size` / `quality` / `background` / `output_format` / `output_compression` | text | 否 | 同 generations |
| `input_fidelity` | text | 否 | `high` / `low`（部分模型支持） |
| `partial_images` / `stream` / `user` | text | 否 | 同 generations |

响应与 §3 相同；流式事件前缀是 `image_edit.`（见 §5）。

> 多张参考图的键名兼容三种写法是有意的：OpenAI 官方 Node SDK 发的是 `image[]`，
> Python SDK 与多数手写 curl 发的是重复的 `image`。

## 5. 流式（`stream: true` + `partial_images`）

`Content-Type: text/event-stream`。事件名与 OpenAI 一致（官方 SDK 的
`client.images.generate({stream: true})` 可直接消费）：

```
event: image_generation.partial_image
data: {"type":"image_generation.partial_image","b64_json":"…","partial_image_index":0,
       "created_at":1767225600,"size":"1024x1024","quality":"high",
       "background":"auto","output_format":"png"}

event: image_generation.completed
data: {"type":"image_generation.completed","b64_json":"…","created_at":1767225601,
       "size":"1024x1024","quality":"high","background":"auto","output_format":"png",
       "usage":{"input_tokens":25,"input_tokens_details":{"text_tokens":25,"image_tokens":0},
                "output_tokens":4160,"total_tokens":4185}}
```

edits 的两条事件名换成 `image_edit.partial_image` / `image_edit.completed`。

- `partial_images: 0` 时只有一条 completed。
- 上游在 completed 之前断流 → 该次尝试判失败（不会把半张图当成品）；有下一候选时自动故障切换
  （已经发过 partial 帧则不再切换，与流式聊天的口径一致：客户端已经看到内容了）。
- 客户端中途断开 → 上游调用被取消，该次尝试按 0 用量记失败（不做部分计费，见 §7）。

## 6. 供应商侧

### 6.1 内建类型 `openai-images`

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

| 字段 | 默认 | 说明 |
|---|---|---|
| `base_url` | 必填 | 上游根地址；`/images/generations`、`/images/edits`、`/models` 拼在其后 |
| `api_key` | 空 | 也可由「凭据」栏下发（加密落库）；写成 `Authorization: Bearer <api_key>` |
| `headers` | 空 | 附加请求头（自定义认证、额外的中转参数） |
| `timeout_s` | 300 | HTTP 客户端超时。生图慢，默认比 Responses 面（120）长 |
| `models[]` | 空 | 上游模型目录，只能声明不能猜；`capabilities` 里图片模型必须有 `image_generation` |

- 能力声明决定路由（§6.3），也会被 `GET /v1/models` 披露。
- 健康探测打 `GET {base}/models`：中转没有这个端点时探测会报错，但数据面不受影响。
- 控制台的「内建类型说明」「测试」「刷新模型」对新类型直接可用（声明即目录）。
- 一个供应商实例只讲一门协议：这个类型**不做**文本补全。把文本请求发给它，会得到
  `image_model_only` 的 400（文案指向 `/v1/images/generations`）。

### 6.2 插件供应商

插件在握手里声明 `capabilities.images = true` 并实现可选接口
`pluginapi.ImageProvider`（`Images` / `ImagesStream`），宿主就会把图片请求派给它；
详见 `docs/plugin-protocol-v1.md` §5、§6。未实现的插件收到图片请求时回
`unsupported_method`，不会崩溃。

### 6.3 能力键

| 键 | 含义 | 什么时候必须声明 |
|---|---|---|
| `image_generation` | 该模型服务 Images API、**输出**图片 | 所有图片模型的映射行 |
| `image` | 该模型接受图片**输入**（M68 语义） | 需要服务 `/v1/images/edits`（参考图）的模型 |

图片请求只落到声明了 `image_generation` 的候选上；edits 额外要求 `image`。
**图片请求不参与 `routing.degradation` 的 strip 降级**：没声明就是不可用（否则请求会打到只会聊天的
模型上白花一次额度），全部候选都不可用 → 400 `unsupported`。

## 7. 计量与计费

| 维度 | 来源 | 缺费率时回落 |
|---|---|---|
| `input` | 提示词文本 token（`usage.input_tokens_details.text_tokens`） | `input_cache_miss` |
| `image_input` | 参考图 token（`usage.input_tokens_details.image_tokens`） | `input_cache_miss` |
| `image_output` | 图像输出 token（`usage.output_tokens`） | `output` |

上游不报明细时：`input_tokens` 整体记入 `input`；完全不报 usage 时按提示词估算并标
`usage_estimated`（缺口如实标记，不假装精确）。

**预留**：进入上游之前按 `n × billing.images_reserve_tokens`（默认 8192，每张图的输出 token 上限）
加参考图数量估的输入侧预留；实际结算按上游真实 usage，多预留的部分自动释放。没有费率的部署回退
`billing.reserve_micros_default`。

**按请求收费**：`pricing` 规则集的 `per_request_fee_micros` 对图片请求按请求收一次（`n>1` 不乘）。

**已知缺口（按设计保留）**：图片流被客户端中断时上游 usage 未知，该次尝试按 0 用量记失败，
网关不会「估算」一笔补账；这类请求在请求日志里能看到（`terminated_reason`）。

## 8. 路由与模型披露

- 图片请求的特征是 `{"image_generation": true}`（edits 追加 `{"image": true}`）；映射、别名、优先级、
  权重、并发闸、熔断、会话粘性、`X-Gateway-Provider` 钉选全部照常。
- `GET /v1/models` 对图片模型多披露一个字段：

```json
{"id":"gpt-image-2","object":"model","owned_by":"aigw","input_modalities":["text","image"],
 "output_modalities":["image"],"capabilities":{"image_generation":true,"image":true}}
```

`output_modalities` 只在模型有声明时出现：普通文本模型是 `["text"]`，
图片模型是 `["image"]`。dshgw 渲染租户模型菜单、控制台智能问答的模型下拉都会**跳过**
输出模态不含文本的模型（DSH 与智能问答都只做文本对话）。

## 9. 请求日志

`Endpoint` 列是 `/v1/images/generations` 或 `/v1/images/edits`；记录的正文是 prompt 文本
（仍受 `recording` 档位与 `redact_paths` 约束）与参数（`n`/`size`/`quality`/`stream`/`partial_images`），
参考图只记 `{name, mime, bytes}` —— **图片字节永不入库**。用量、费用、供应商/上游模型/路由
与 Responses 面同一批列。

## 10. 排障

| 现象 | 多半原因 |
|---|---|
| 400 `unsupported`：没有供应商能服务图片生成 | 映射行没声明 `capabilities.image_generation`（edits 还要 `image`） |
| 400 `model_not_found` | 模型未启用 / 该 Key 的标签未授权 / 没有路由 |
| 400 `image_model_only` | 把 `/v1/responses` 的请求打到了图片模型上 |
| 413 | 请求体超过 `server.images_max_body_bytes`（参考图太大或太多） |
| 上游 400 且带 `Unknown parameter` | 参数只被部分模型支持（如 `input_fidelity` 对某些模型无效） |
| 上游 401/403 | 供应商凭据问题（fatal，不会故障切换） |
| 流中断但无 completed | 上游在生图过程中断开；看供应商日志与 `provider_busy`/`upstream_*` 错误码 |
| `usage_records` 里 `image_output` 为 0 | 上游没报 usage（部分中转），该行标 `usage_estimated` |

## 11. 本机验收命令（临时实例，不动运行态）

```bash
# 1) 起一个临时实例（临时数据根 + 临时端口），配置里只有一个 openai-images 供应商指向本地假上游
bin/aigw --config /tmp/m84/config.yaml

# 2) 非流式
curl -sS -X POST http://127.0.0.1:18099/v1/images/generations \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"a fox","n":1,"size":"1024x1024"}' | python3 -m json.tool

# 3) 流式
curl -sS -N -X POST http://127.0.0.1:18099/v1/images/generations \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"a fox","stream":true,"partial_images":2}'

# 4) edits
curl -sS -X POST http://127.0.0.1:18099/v1/images/edits \
  -H "Authorization: Bearer $KEY" \
  -F 'model=gpt-image-2' -F 'prompt=make it blue' -F 'image[]=@/tmp/m84/in.png'
```
