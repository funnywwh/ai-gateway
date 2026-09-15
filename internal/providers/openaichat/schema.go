package openaichat

import "encoding/json"

// Schema returns the JSON Schema subset describing this kind's provider config and
// credentials. Descriptions are operator-facing Chinese text: the admin console
// renders them verbatim, which keeps the explanation in the same file as the parser
// and the validation that enforce it. A reflective test compares the declared
// properties against Config, so a new field without a description fails the build.
func Schema() (config, credentials json.RawMessage) {
	return configSchema, credentialsSchema
}

// Note is the kind-level prose shown above the field table.
func Note() string { return kindNote }

// Template returns a config skeleton with the fields an operator must fill in.
func Template() json.RawMessage { return configTemplate }

const kindNote = "对接 OpenAI 兼容的 /chat/completions 上游：DeepSeek、Qwen、Gemini（Google 官方兼容层）、Ollama、vLLM、LM Studio 以及任何自称 OpenAI 兼容的服务。" +
	"接不了 OpenAI Responses API —— 那是 openai-responses 类型的职责。" +
	"默认值等于通用 OpenAI 兼容行为，升级不会改变既有部署；上游从本机不可达时用 proxy 字段。" +
	"DeepSeek 的思考方言、Gemini 的接入差异与错误分类见 docs/api-providers.md。"

var configSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "base_url": {"type": "string", "x-required": true,
      "description": "上游根地址；/chat/completions 与 /models 拼在其后。https://api.deepseek.com/v1 与 https://api.deepseek.com 都可能正确，取决于该部署是否要求 /v1。"},
    "api_key": {"type": "string", "x-prefer-credential": "api_key",
      "description": "上游密钥，以 Authorization: Bearer <api_key> 发出。推荐填在「凭据」栏（AES-GCM 加密落库、永不回显）；写在这里是明文且管理面会回显，并且构建实例时优先于凭据通道——两处只填一处。"},
    "headers": {"type": "object",
      "description": "附加请求头。上游需要自定义头或非 Bearer 认证（例如 Azure 的 api-key 头）时用它，此时 api_key 留空。"},
    "timeout_s": {"type": "integer", "default": 120,
      "description": "HTTP 客户端超时（秒）。单次尝试的最终上限仍由路由的 per_attempt_timeout_s 决定。"},
    "proxy": {"type": "string", "x-advanced": true,
      "description": "出网代理：http://host:port、https://host:port、socks5://host:port（socks5h 同义），或字面量 env 表示跟随 HTTPS_PROXY/NO_PROXY。留空=直连，且**不读**环境变量——这样升级不会改变既有部署的走向；需要带账号密码的代理时把凭据写进 URL（配置明文存储且管理面回显）。代理写错会让供应商构建失败，而不是静默直连，否则症状与「上游不可达」无法区分。"},
    "models": {"type": "array",
      "description": "上游模型目录，只能声明不能猜。元素字段：public（对客模型名）、upstream（上游模型名，省略则用 public）、context_window、max_output_tokens、capabilities{stream,tools,reasoning}。",
      "items": {"type": "object", "properties": {
        "public": {"type": "string"},
        "upstream": {"type": "string"},
        "context_window": {"type": "integer"},
        "max_output_tokens": {"type": "integer"},
        "capabilities": {"type": "object",
          "description": "能力申报决定路由：客户端要 reasoning.effort 就要求 reasoning，要 text.format=json_object/json_schema 就要求同名的能力。/chat/completions 不支持 json_schema，别声明它。"}
      }}},
    "thinking": {"type": "object",
      "description": "思考（思维链）方言。默认一个 thinking 字段都不下发，即通用 OpenAI 兼容行为。",
      "properties": {
        "mode": {"type": "string", "enum": ["auto", "enabled", "disabled"], "default": "auto",
          "description": "auto=由客户端 reasoning.effort 决定；enabled=强制开启；disabled=强制关闭。注意 DeepSeek 默认开启思考，想省思考 token 只能显式关。"},
        "style": {"type": "string", "enum": ["none", "deepseek"], "default": "none",
          "description": "deepseek=下发 {\"thinking\":{\"type\":\"enabled|disabled\"}}；none=不下发（通用形态）。"},
        "replay_reasoning_content": {"type": "boolean", "default": false,
          "description": "把历史 reasoning 正文回传为 assistant.reasoning_content：带工具调用的多轮必需，上游缺失即 400。需要 style=deepseek。"}
      }},
    "response_format": {"type": "string", "enum": ["text", "json_object", "json_schema"], "default": "text",
      "description": "能力申报，不是下发开关：只说明该上游最高支持到哪一档，实际请求按客户端的 text.format 下发（没要 JSON 就不带该字段）。"},
    "default_max_output_tokens": {"type": "integer", "default": 0,
      "description": ">0 且客户端未给 max_output_tokens 时才补；只影响在途额度预留，不吃掉上游默认值（DeepSeek 思考模式默认 64K）。"}
  }
}`)

var credentialsSchema = json.RawMessage(`{
  "type": "object",
  "description": "凭据经 AES-256-GCM 加密落库（密钥来自 credentials_key，AAD 绑定 provider id），管理面只显示字段名。留空=保持不变，{} = 清空。",
  "required": ["api_key"],
  "properties": {
    "api_key": {"type": "string", "x-secret": true,
      "description": "上游密钥（sk-…）。上游返回 401/403 时归类为 fatal token_invalid，不会故障切换到别的供应商；402 视为余额耗尽并冷却该候选。"}
  }
}`)

var configTemplate = json.RawMessage(`{
  "base_url": "https://api.deepseek.com/v1",
  "timeout_s": 120,
  "models": [
    {"public": "deepseek-flash", "upstream": "deepseek-flash",
     "context_window": 1000000, "max_output_tokens": 65536,
     "capabilities": {"stream": true, "tools": true, "reasoning": true}}
  ]
}`)
