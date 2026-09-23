package openaiimages

import "encoding/json"

// Schema returns the JSON Schema subset describing this kind's provider config and
// credentials. Descriptions are operator-facing Chinese text: the admin console renders
// them verbatim, which keeps the explanation next to the parser and the validation that
// enforce it. A reflective test compares the declared properties against Config, so a new
// field without a description fails the build.
func Schema() (config, credentials json.RawMessage) {
	return configSchema, credentialsSchema
}

// Note is the kind-level prose shown above the field table.
func Note() string { return kindNote }

// Template returns a config skeleton with the fields an operator must fill in.
func Template() json.RawMessage { return configTemplate }

const kindNote = "对接原生 OpenAI Images API（/images/generations、/images/edits）的上游，用来服务 gpt-image 这类" +
	"输出图片的模型。只讲 /chat/completions 或 /responses 的上游请用 openai-chat / openai-responses；" +
	"本类型的实例不服务文本补全，把文本请求发给它会得到 image_model_only 的 400。"

var configSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "base_url": {"type": "string", "x-required": true,
      "description": "上游根地址；/images/generations、/images/edits 与 /models 拼在其后（如 https://api.openai.com/v1）。"},
    "api_key": {"type": "string", "x-prefer-credential": "api_key",
      "description": "上游密钥，以 Authorization: Bearer <api_key> 发出。推荐填在「凭据」栏（AES-GCM 加密落库、永不回显）；写在这里是明文且管理面会回显，并且构建实例时优先于凭据通道——两处只填一处。"},
    "headers": {"type": "object",
      "description": "附加请求头。上游需要自定义头或非 Bearer 认证时用它，此时 api_key 留空。"},
    "timeout_s": {"type": "integer", "default": 300,
      "description": "HTTP 客户端超时（秒）。生图慢，默认 300；单次尝试的最终上限由路由的 per_attempt_timeout_s 决定。"},
    "models": {"type": "array",
      "description": "上游模型目录，只能声明不能猜。元素字段：public（对客模型名）、upstream（上游模型名，省略则用 public）、capabilities{image_generation,image}——图片模型必须声明 image_generation，要服务 /v1/images/edits（参考图）还要声明 image。声明会被 GET /v1/models 披露，并决定路由：没有声明 image_generation 的候选收不到图片请求。",
      "items": {"type": "object", "properties": {
        "public": {"type": "string"},
        "upstream": {"type": "string"},
        "capabilities": {"type": "object",
          "description": "能力申报：image_generation（本模型服务 Images API、输出图片，必填）、image（接受图片输入，参考图编辑需要）。"}
      }}}
  }
}`)

var credentialsSchema = json.RawMessage(`{
  "type": "object",
  "description": "凭据经 AES-256-GCM 加密落库（密钥来自 credentials_key，AAD 绑定 provider id），管理面只显示字段名。留空=保持不变，{} = 清空。",
  "required": ["api_key"],
  "properties": {
    "api_key": {"type": "string", "x-secret": true,
      "description": "上游密钥（sk-…）。401/403 归类为 fatal token_invalid：换供应商同样失败，属于凭据问题，不会故障切换。"}
  }
}`)

var configTemplate = json.RawMessage(`{
  "base_url": "https://api.openai.com/v1",
  "timeout_s": 300,
  "models": [
    {"public": "gpt-image-2", "upstream": "gpt-image-2",
     "capabilities": {"image_generation": true, "image": true}}
  ]
}`)
