package testecho

import "encoding/json"

// Schema returns the JSON Schema subset describing this kind's provider config and
// credentials. Descriptions are operator-facing Chinese text: the admin console
// renders them verbatim, which keeps the explanation next to the parser and the
// validation that enforce it. A reflective test compares the declared properties
// against Config, so a new field without a description fails the build.
func Schema() (config, credentials json.RawMessage) {
	return configSchema, credentialsSchema
}

// Note is the kind-level prose shown above the field table.
func Note() string { return kindNote }

// Template returns a config skeleton with the fields an operator must fill in.
func Template() json.RawMessage { return configTemplate }

const kindNote = "测试回声供应商：不访问任何上游，把最后一条用户输入原样回显，并可注入慢流与各类失败。" +
	"用于集成测试、健康探测与压测基线（scripts/load.sh 默认用它）。生产路由不要指向它。"

var configSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "prefix": {"type": "string", "default": "echo:",
      "description": "回显文本的前缀，便于确认请求确实打到了这个供应商。"},
    "chunks": {"type": "integer", "default": 1,
      "description": "流式响应切成几个增量；<=0 按 1 处理。用来验证客户端的增量拼装。"},
    "delay_ms": {"type": "integer", "default": 0,
      "description": "每个增量之间的延迟（毫秒），用来模拟慢上游与 TTFB 超时。"},
    "fail_mode": {"type": "string", "enum": ["", "retryable", "quota", "fatal"], "default": "",
      "description": "强制失败：空=正常；retryable=可重试（触发故障切换）；quota=配额耗尽（冷却该候选）；fatal=致命错误（不切换，直接回客户端）。"},
    "fail_after": {"type": "integer", "default": 0,
      "description": "先发出 N 个增量再按 fail_mode 失败，用于测试流中途失败与部分用量结算。"},
    "finish_reason": {"type": "string", "default": "stop",
      "description": "上报给宿主的终止原因。填 length/content_filter 可模拟“流正常结束但答案被截断”，此时网关会以 response.incomplete 收尾而不是 response.completed。"},
    "cut_stream": {"type": "boolean", "default": false,
      "description": "流不回终止事件就结束，模拟上游连接中途断开：文本已经发出、结尾永远没来。宿主必须把它当作被截断的流而不是完整回答。"}
  }
}`)

var credentialsSchema = json.RawMessage(`{
  "type": "object",
  "description": "该类型不访问上游，因此不使用凭据；填了也会被忽略。",
  "properties": {}
}`)

var configTemplate = json.RawMessage(`{
  "prefix": "echo:",
  "chunks": 1,
  "delay_ms": 0,
  "fail_mode": ""
}`)
