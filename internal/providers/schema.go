package providers

import (
	"encoding/json"
	"strings"

	"github.com/winger/ai-gateway/internal/providers/openaichat"
	"github.com/winger/ai-gateway/internal/providers/openairesponses"
	"github.com/winger/ai-gateway/internal/providers/testecho"
)

// Schema sources reported to the admin console. A builtin kind carries its own
// schema in this binary; a plugin's schema only exists inside its handshake.
const (
	SchemaSourceBuiltin = "builtin"
	SchemaSourcePlugin  = "plugin"
	SchemaSourceUnknown = "unknown"
)

// KindSchema is one provider kind's configuration surface, as the admin console
// renders it: a JSON Schema subset (plus the x-* extensions described in
// docs/provider-ui.md) for the config object, the same for the credential object,
// and a skeleton an operator can start from.
type KindSchema struct {
	Kind        string          `json:"kind"`
	Note        string          `json:"kind_note,omitempty"`
	Config      json.RawMessage `json:"config_schema,omitempty"`
	Credentials json.RawMessage `json:"credentials_schema,omitempty"`
	Template    json.RawMessage `json:"config_template,omitempty"`
	Source      string          `json:"schema_source"`
}

// Schemas returns the schema of every builtin kind, sorted by kind. Plugin kinds
// are not listed: they are not a closed set, and their schema is only known once
// the plugin process performs its handshake.
func Schemas() []KindSchema {
	kinds := BuiltinKinds()
	out := make([]KindSchema, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, SchemaFor(kind))
	}
	return out
}

// SchemaFor describes one kind. Unknown and plugin kinds do not fail: they return
// SchemaSourceUnknown / SchemaSourcePlugin with an explanatory note, because the
// caller (provider detail, create form) still has a usable answer to show.
func SchemaFor(kind string) KindSchema {
	var config, credentials, tmpl json.RawMessage
	var note string
	source := SchemaSourceBuiltin
	switch kind {
	case KindOpenAIChat:
		config, credentials = openaichat.Schema()
		note, tmpl = openaichat.Note(), openaichat.Template()
	case KindOpenAIResponses:
		config, credentials = openairesponses.Schema()
		note, tmpl = openairesponses.Note(), openairesponses.Template()
	case KindTestEcho:
		config, credentials = testecho.Schema()
		note, tmpl = testecho.Note(), testecho.Template()
	default:
		config, credentials, tmpl = nil, nil, nil
		if strings.HasPrefix(kind, "plugin:") {
			source = SchemaSourcePlugin
			note = "插件供应商：配置与凭据字段由插件在握手时声明，网关不预设。" +
				"点「读取插件声明」会连接（必要时启动）该插件进程后读取；插件未声明时只能按裸 JSON 配置。"
		} else {
			source = SchemaSourceUnknown
			note = "未知类型：既不是内建类型（" + strings.Join(BuiltinKinds(), " / ") +
				"），也不是 plugin:<名称> 形式。请检查拼写；实例会在构建时报错。"
		}
	}
	return KindSchema{
		Kind: kind, Note: note, Config: config, Credentials: credentials,
		Template: tmpl, Source: source,
	}
}
