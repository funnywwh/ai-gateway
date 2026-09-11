# 插件协议 v1（Provider Plugin Protocol）

> 状态：**已实现**（`pkg/pluginapi`，M2）。协议主版本 `protocol = 1`，破坏性变更只升主版本。

## 1. 进程模型

- 网关把每个供应商实例作为**独立子进程**启动：`<binary> --aigw-plugin`。
- 通信：**stdin/stdout 上的 NDJSON**（每行一个 JSON 对象，单行上限 8 MiB）。
- **stdout 只允许协议帧**；插件日志必须写 **stderr**（宿主按 `provider=<instance>` 前缀转写，
  并保留最近 200 行供管理界面排障）。
- 进程属性：独立进程组（`Setpgid`）+ `Pdeathsig=SIGTERM`（Linux），避免孤儿插件堆积。

## 2. 环境变量

| 变量 | 说明 |
|---|---|
| `GW_PLUGIN_PROTOCOL` | 固定 `1` |
| `GW_PLUGIN_INSTANCE` | 实例名（与 provider 记录同名） |
| `GW_PLUGIN_CONFIG` | 插件配置 JSON（非密） |
| `GW_PLUGIN_STATE_DIR` | 插件可写状态目录（0700），OAuth token / cookie / 缓存放这里 |
| `GW_PLUGIN_CREDENTIALS` | **兼容回退**：小凭据 JSON；大凭据或含 token 时请用下面的文件 |

**凭据传递（推荐）**：宿主在启动前把凭据写入 `$GW_PLUGIN_STATE_DIR/credentials.json`（**0600**），
插件启动时读取并可自行删除。这样避免 `/proc/<pid>/environ` 同 UID 可读与 env ~128KB 上限。

插件若需要**出网代理**（境外上游受地区限制等），约定写在插件自己的配置/凭据里，而非宿主进程环境变量——见 §12。

## 3. 握手

插件 stdout 的**第一行**必须是握手帧：

```json
{
  "type": "handshake",
  "protocol": 1,
  "name": "codex-subscription",
  "version": "0.1.0",
  "capabilities": {
    "complete": true, "stream": true, "list_models": true, "health": true,
    "usage_estimated": true, "usage_delta": true, "usage_dimensions": true,
    "needs_login": true,
    "actions": [{"name": "login", "title": "登录 ChatGPT 账号"}]
  },
  "config_schema": { "type": "object", "properties": { "base_url": {"type": "string", "format": "uri"} } },
  "credentials_schema": { "type": "object", "properties": { "access_token": {"type": "string", "x-secret": true} } }
}
```

- 宿主 **3 秒**（可配）内未收到握手、或 `protocol` 不匹配 → 判定不可用、kill 进程并记录 `last_error`。
- `config_schema` / `credentials_schema` 用于管理界面**通用表单渲染**（见 `docs/provider-ui`）。

## 4. 帧格式

宿主 → 插件（请求）：

```json
{"id":"42","method":"provider.stream","params":{ ... }}
```

插件 → 宿主（四种响应帧 + 通知）：

| type | 含义 | 载荷 |
|---|---|---|
| `result` | 一元成功 | `result` 字段 |
| `event` | 流式增量（可多次） | `event` 字段 |
| `end` | 流结束 | `result` = `{usage, finish_reason, partial}` |
| `error` | 失败 | `error` = `{code, message, retryable, http_status, kind, reset_at}` |
| `notify` | 插件主动通知（如凭据回写） | `method` + `params` |
| `pong` | 心跳应答 | 无 |

## 5. 方法

| method | 方向 | 说明 |
|---|---|---|
| `provider.info` | 宿主→插件 | 返回 `Info`（通常握手已足够） |
| `provider.list_models` | 宿主→插件 | 返回 `[]ModelInfo`（上游模型目录与能力） |
| `provider.complete` | 宿主→插件 | 非流式：返回 `Response` |
| `provider.stream` | 宿主→插件 | 流式：多次 `event` 后 `end` |
| `provider.health` | 宿主→插件 | 连通性探测 |
| `provider.action` | 宿主→插件 | 交互动作（设备码登录等），返回 `ActionStatus` |
| `provider.cancel` | 宿主→插件 | 取消某次调用（见 §7） |
| `ping` / `pong` | 双向 | 心跳（默认 15s；连续 3 次失败判死重启） |
| `shutdown` | 宿主→插件 | 优雅退出 |

## 6. 事件类型

| event.type | 含义 |
|---|---|
| `text.delta` | 最终输出文本增量（→ `response.output_text.delta`） |
| `reasoning.delta` | 思考文本增量（→ `response.reasoning_summary_text.delta`） |
| `refusal.delta` | 拒答增量 |
| `tool_call.start` | 工具调用开始（带 `call_id`/`name`） |
| `tool_call.arguments.delta` | 工具参数增量 |
| `usage` | **最终**用量 |
| `usage.delta` | **增量**用量（在途计量用；可带 `estimated:true`） |

## 7. 取消与部分结算

宿主发出：

```json
{"method":"provider.cancel","params":{"id":"42","reason":"insufficient_quota"}}
```

插件**必须**取消上游请求（context cancel），并尽快以 `end{partial:true}` 或 `error` 收尾。
宿主在 `cancel_grace_ms`（默认 1500ms）后强制丢弃该流。
结算口径：**只为已计量部分计费**，中继期间上游继续产生的 `overshoot` 记成本但不计费（`overshoot_policy=absorb`）。

## 8. 用量维度

`Usage.Dimensions` 是统一的计量维度表（整数）：

| 维度 | 含义 |
|---|---|
| `input` | 未区分缓存的输入 token |
| `input_cache_hit` | 缓存命中的输入 token（如 DeepSeek `prompt_cache_hit_tokens`） |
| `input_cache_miss` | 缓存未命中的输入 token |
| `output` | 输出 token |
| `reasoning` | 思考 token（默认计入 output，可单列） |

扩展维度（同机制）：`image`、`audio_second`、`tool_call`。

## 9. 错误分类

| kind | 路由行为 |
|---|---|
| `retryable` | 允许故障切换到下一个候选供应商 |
| `quota_exhausted` | 冷却该候选至 `reset_at`（缺省 1800s） |
| `fatal` | 直接返回客户端，不切换 |

HTTP 映射：`retryable` 依据 `http_status`；`quota_exhausted` → 429；其余 → 502/400。

## 10. 背压

宿主对每个插件使用**单 reader goroutine + 有界缓冲（8 帧）**。缓冲填满时插件的 `emit` 阻塞——
这就是"暂停读取上游"（throttle 策略）的实现基础。**插件必须能承受背压**：不得丢弃事件，也不得死锁。

## 11. 编写一个插件（最小示例）

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

type provider struct{}

func (p *provider) Info() pluginapi.Info {
	return pluginapi.Info{Name: "my-provider", Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{Complete: true, Stream: true}}
}
func (p *provider) ListModels(context.Context) ([]pluginapi.ModelInfo, error) { return nil, nil }
func (p *provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	return &pluginapi.Response{Status: "completed"}, nil
}
func (p *provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	return emit(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: "hi"})
}
func (p *provider) Health(context.Context) error { return nil }
func (p *provider) Actions() []pluginapi.Action  { return nil }
func (p *provider) RunAction(context.Context, string, []byte) ([]byte, error) { return nil, nil }
func (p *provider) StateDir() string             { return os.Getenv(pluginapi.EnvStateDir) }
func (p *provider) SetCredentials(map[string]string) {}

func main() {
	if err := pluginapi.Serve(&provider{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

参考实现：`examples/provider-replay`（含可控慢流、增量用量、可控失败，用于 E2E 与压测）。

## 12. 出网与代理

需要访问境外上游的插件（地区限制、链路质量），**约定**把代理声明在自己的配置里，而不是依赖宿主进程的
`HTTPS_PROXY`：环境变量要求重启网关才生效，而配置/凭据变更会让宿主停掉插件进程、下次请求以新配置拉起。

| 位置 | 字段 | 说明 |
|---|---|---|
| 配置（非密，明文存储、管理面回显） | `proxy` | 代理 URL，例如 `http://127.0.0.1:2334` |
| 凭据（AES-GCM 封装，写后不回显） | `proxy` | 覆盖配置值；URL 含 `user:pass` 时填这里 |

规则：

1. **优先级**：`credentials.proxy` > `config.proxy` > 进程环境变量（`HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`）。
2. **留空即回退环境变量**，因此不配置代理的插件与不支持代理的插件行为一致，无需开关。
3. **支持的 scheme**：`http`、`https`、`socks5`、`socks5h`（`net/http` 原生支持，`socks5` 等价于 `socks5h`）。
   URL 内的 userinfo 由标准库自动转为 `Proxy-Authorization`，插件无需自行实现认证。
4. **只作用于该插件自己的上游请求**，不影响宿主，也不影响同宿主下的其他插件。
5. **日志与诊断输出必须脱敏**：只允许出现 `scheme://host[:port]`，绝不包含 userinfo。
   建议在 `whoami` 之类的只读动作里回报生效代理与来源（`credentials`/`config`/`env`）以便排障。
6. **错误分类**：代理不可达属**瞬时故障** → `retryable`（允许故障切换）；代理**配置非法**属 `fatal`，不重试。
7. 代理 URL 的校验由**插件自己**完成：宿主当前不对插件配置执行 schema 校验（`pluginapi.Schema` 尚未接线），
   因此不得依赖 `config_schema` 里的 `format` 声明来拦截非法值。

参考实现：`examples/provider-codex`（设计与取舍见 `docs/design/m10b-codex-egress-proxy.md`）。
