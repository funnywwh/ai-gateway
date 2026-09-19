package httpapi

// This file documents the small structured documents the management API accepts beyond the
// pricing rule sets (see admin_pricing_schema.go).
//
// They are here for the same reason those are: a body field declared as "object" with no
// shape reaches an agent as {"type":"object"} with an example of {}, which is not a smaller
// answer but a wrong one — the agent cannot know a field name and either guesses (400, since
// several of these are parsed strictly) or refuses. See docs/mcp.md §4.5.
//
// The honest half of this file matters as much as the helpful half: a field the gateway
// stores but never reads is described as such, because teaching an agent to write a setting
// that does nothing is worse than telling it the setting does nothing.

// objectDocument renders a JSON Schema object from one map of properties, with the strictness
// the matching parser actually applies.
//
// "strict" mirrors json.Decoder.DisallowUnknownFields / an allow-list check: unknown keys are
// rejected at write time, so a permissive schema here would describe a document that cannot be
// saved. When strict is false the description is still rendered verbatim, because that is what
// a reader needs to know before assuming a typo was ignored.
func objectDocument(properties map[string]any, strict bool, desc string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if strict {
		schema["additionalProperties"] = false
	}
	if desc != "" {
		schema["description"] = desc
	}
	return schema
}

// arrayOfStrings is the schema for a list of names (tags, aliases, provider order …).
func arrayOfStrings(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// idArraySchema is the schema for a list of numeric ids. It is separate from arrayOfStrings so
// an agent is not left guessing whether it should send 3 or "3".
func idArraySchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "integer"},
		"description": "数字 id 列表"}
}

// boolProp and intProp are the two property shapes that repeat everywhere; both require a
// description, because a boolean with no explanation is a coin flip.
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// stringEnumProp renders a closed set of string values.
func stringEnumProp(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

// wildcardListProp renders a list of model or provider names that accepts "*" as "everything".
func wildcardListProp(desc string) map[string]any {
	return map[string]any{
		"type": "array", "items": map[string]any{"type": "string"},
		"description": desc + "；\"*\" 表示全部。键名保持英文（接口契约），含义随说明",
	}
}

// policySchema is the API key / tag policy document.
//
// This one is strict in the strongest sense: domain.ParsePolicy compares the document's
// top-level keys against domain.PolicyFields and the key endpoints answer 400 for anything
// else. An agent that writes {"rate_limit":{"rpm":60}} — the shape the console itself used to
// advertise, and which nothing ever read — must be told, not silently ignored.
func policySchema() map[string]any {
	properties := map[string]any{
		"rpm":                 intProp("每分钟请求数上限（已强制执行）"),
		"tpm":                 intProp("每分钟 token 数上限（已强制执行）"),
		"concurrency":         intProp("并发请求上限（已强制执行）"),
		"monthly_requests":    intProp("每月请求数上限（**只解析、未执行**，会在 get_rate_limits 的 not_enforced 里列出）"),
		"monthly_tokens":      intProp("每月 token 上限（**只解析、未执行**）"),
		"monthly_cost_micros": intProp("每月费用上限，微单位，按账本币种（**只解析、未执行**）"),
		"strategy": stringEnumProp("层内负载均衡策略（影响路由；留空用路由表配置）",
			"weighted_random", "round_robin", "least_inflight", "least_latency", "strict_order"),
		"provider_order": arrayOfStrings("优先尝试的供应商顺序；不在列表里的供应商排在后面。元素是供应商名"),
		"margin_bp":      intProp("对客加价倍数，基点（10000 = 1.0 倍）；设置后覆盖账户与模型级售价倍率"),
	}
	return objectDocument(properties, true,
		"限速与配额策略。**顶层扁平字段，只有上面列出的键被接受**（其它键一律 400）："+
			"键名保持英文（接口契约），含义见各属性说明。嵌套写法（如 rate_limit 对象）从不存在有效实现，会被拒绝。"+
			"tag 策略与 key 策略按优先级合并，key 覆盖 tag。")
}

// keyPolicyExample is a policy that is entirely in force (no monthly_* field, which would be
// rejected by nobody but enforced by nothing).
func keyPolicyExample() map[string]any {
	return map[string]any{"rpm": 60, "tpm": 100000, "concurrency": 8}
}

// grantsSchema is the model/provider authorization document.
func grantsSchema(subject string) map[string]any {
	return objectDocument(map[string]any{
		"models":    wildcardListProp("允许访问的对客模型名（" + subject + "）"),
		"providers": wildcardListProp("允许使用的供应商名（" + subject + "）"),
	}, false,
		"授权对象：决定这个"+subject+"能调用哪些模型与供应商。两个数组取并集；"+
			"key 的授权与它所有 tag 的授权取并集，都没有任何授权时按 router 的 default_grant 兜底。")
}

// grantsExample is the "everything" grant, which is what an empty grant means by default.
func grantsExample() map[string]any {
	return map[string]any{"models": []any{"*"}, "providers": []any{"*"}}
}

// unenforcedObjectSchema documents a field the API stores but no code reads.
//
// It exists so the shape is not a lie without inventing semantics: the schema says "any JSON
// object is accepted", and the description says plainly that nothing consumes it yet. A model
// that sees this will not spend a turn configuring it expecting an effect.
func unenforcedObjectSchema(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc}
}

// capabilitiesSchema is a provider model's capability declaration. Two vocabularies share
// this one object: the provider-level keys of pkg/pluginapi.Capabilities, and the
// request-feature keys routing matches a request against (`tools`, `reasoning`, `image`, …).
// All of them are optional booleans, and an omitted key is unknown — which for a request
// feature means "do not block the candidate", not "unsupported".
func capabilitiesSchema() map[string]any {
	return objectDocument(map[string]any{
		"complete":         boolProp("是否支持非流式补全"),
		"stream":           boolProp("是否支持流式（SSE）"),
		"list_models":      boolProp("是否支持模型目录查询"),
		"health":           boolProp("是否支持健康探测"),
		"usage_estimated":  boolProp("用量是否为估算值（上游不报精确 token）"),
		"usage_delta":      boolProp("用量是否以增量形式上报"),
		"usage_dimensions": boolProp("是否上报分维度用量（缓存命中/未命中、思考等）"),
		"needs_login":      boolProp("是否需要交互式登录（如设备码）"),
		"tools":            boolProp("是否支持工具调用（请求带 function 工具时按它筛候选）"),
		"reasoning":        boolProp("是否支持思考（请求带 reasoning.effort 时按它筛候选，也是 /v1/models 披露推理档位的依据）"),
		"image":            boolProp("是否支持图片输入（请求含 input_image 时按它筛候选，也是 /v1/models 的 input_modalities 依据）"),
		"json_object":      boolProp("是否支持 text.format=json_object"),
		"json_schema":      boolProp("是否支持 text.format=json_schema"),
		"parallel_tools":   boolProp("是否支持并行工具调用"),
	}, false,
		"能力声明：上游模型支持什么。省略的键按未知处理，模型会按供应商声明的能力参与路由与降级判断。")
}

// capabilityOverrideSchema is the per-model override mode of the declared capabilities.
func capabilityOverrideSchema() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"inherit", "strip", "reject"},
		"description": "能力缺失时怎么办：inherit = 用供应商声明（默认）；strip = 剥掉不支持的能力继续发；reject = 直接判失败。空字符串等于 inherit"}
}

// capabilityOverrideField is the body field for that mode.
func capabilityOverrideField() adminField {
	return schemaField(bodyOptional("capabilities_override", "string",
		"能力覆写模式：inherit（默认，用供应商声明）/ strip（剥掉不支持的能力继续发）/ reject（直接判失败）；空字符串等于 inherit"),
		capabilityOverrideSchema())
}

// providerOverridesSchema documents the provider-level knobs that are objects.
func providerMetaSchema() map[string]any {
	return map[string]any{"type": "object",
		"description": "自定义元数据：网关不解释，只原样保存并在读取供应商时返回，可放备注、内部编号等（任意 JSON 对象）"}
}

func providerCredentialsSchema() map[string]any {
	return map[string]any{"type": "object",
		"description": "凭据字段。字段名由供应商类型决定，先 admin_list_provider_kinds 或 admin_get_provider 看 credentials 字段清单；" +
			"常见为 api_key。空对象 {} 表示清空已存凭据，**省略**表示保持不变。凭据值只写不回显"}
}

func providerConfigSchema() map[string]any {
	return map[string]any{"type": "object",
		"description": "供应商配置。字段随类型（kind）不同，用 admin_list_provider_kinds 查该类型的配置字段说明（含必填与默认），" +
			"再用 admin_get_provider 看当前值。写错的键由该类型的校验决定是否被拒"}
}

func providerTimeoutSchema() map[string]any {
	return map[string]any{"type": "object",
		"description": "超时覆写（毫秒），键如 connect_ms/read_ms/total_ms；省略表示用全局默认"}
}

// accountPriceOverridesSchema documents the account-level price override field that no code
// reads.
//
// The name is deliberately about the account: the precedence list in docs/pricing.md §3 names
// account.price_overrides, while the code resolves an account's mark-up from
// accounts.markup_override_bp (internal/billing/markup.go). Both the field and the doc line
// predate this milestone; what matters here is that the tool description does not invite an
// agent to configure a document that changes nothing.
func accountPriceOverridesSchema() map[string]any {
	return unenforcedObjectSchema("账户级价格覆写文档。注意：该字段目前**只保存不生效**（售价来源优先级里没有读取方）。" +
		"要调某个账户的价格，请用 markup_override_bp（账户级加价倍数），或模型的 sale_pricing 与 pricing/markup")
}

// modelPolicySchema and routePolicySchema describe the two policy fields that are stored but
// not read.
//
// The message is different from the key/tag policy above on purpose: those are enforced and
// their unknown keys are rejected, while these are accepted as any object and then ignored by
// every code path. Saying so is the point — a model asked to "set the model policy" should
// report that it has no effect instead of configuring it and claiming success.
func modelPolicySchema() map[string]any {
	return unenforcedObjectSchema("模型级策略。**当前不生效**：网关把该文档原样保存，但没有任何读取方" +
		"（限速与路由策略读的是 key/tag 的 policy）。要配置限速与路由请用 admin_create_key / admin_update_key / admin_upsert_tag 的 policy")
}

// modelReasoningSchema is the independently stored per-model reasoning override.
// null clears it so the model inherits its provider/default behavior. The object
// form is intentionally strict because domain.ParseModelReasoning rejects unknown
// keys and unsupported values.
func modelReasoningSchema() map[string]any {
	object := objectDocument(map[string]any{
		"mode": stringEnumProp(
			"default = 仅在客户端未提供 reasoning.effort 时补入下方 effort；force = 无论客户端是否提供都使用下方 effort",
			"default", "force"),
		"effort": stringEnumProp("推理强度；支持 none、minimal、low、medium、high、xhigh、max", "none", "minimal", "low", "medium", "high", "xhigh", "max"),
	}, true, "模型级推理覆写。")
	object["required"] = []string{"mode", "effort"}
	return map[string]any{
		// Do not set a top-level type here: schemaForFields uses this map verbatim,
		// so the alternatives genuinely allow either the strict object or JSON null.
		"oneOf": []any{
			object,
			map[string]any{"type": "null", "description": "清空模型级覆写并继承默认行为"},
		},
		"description": "模型级推理配置；省略不改，null 清空并继承。对象必须包含且只包含 mode 与 effort。",
	}
}

func routePolicySchema() map[string]any {
	return unenforcedObjectSchema("路由级策略。**当前不生效**：网关把该文档原样保存，但没有任何读取方。" +
		"要调整某个模型的路由，请改路由的 priority/weight/enabled（admin_update_route）或加路由（admin_upsert_route），" +
		"要调负载均衡策略请用 key/tag 的 policy.strategy")
}

// simulateDimensionsSchema documents the usage dimensions accepted by the pricing simulator.
//
// It used to be documented as "input_tokens 与 output_tokens 两个整数", which is not a key the
// engine reads: the simulator hands this map straight to pricing.Evaluate, whose dimension
// names are the ones below. An agent following the old text priced every simulation as zero.
func simulateDimensionsSchema() map[string]any {
	properties := map[string]any{}
	for _, dimension := range pricingDimensions {
		properties[dimension] = map[string]any{"type": "integer", "minimum": 0,
			"description": pricingDimensionDesc(dimension)}
	}
	return objectDocument(properties, false,
		"本次试算的用量，键就是计量维度名（与价格规则 rates 的键完全一致，不是 input_tokens）："+
			"例如 {\"input\":1000,\"output\":500}。省略的维度按 0 计。")
}

// simulateDimensionsExample is a small readable request.
func simulateDimensionsExample() map[string]any {
	return map[string]any{"input": 1000, "output": 500}
}

// dimensionMarkupSchema is the per-dimension mark-up override (sale side only).
func dimensionMarkupSchema() map[string]any {
	schema := objectDocument(dimensionMarkupProperties(), false,
		"按维度覆写的加价倍数（bp，10000 = 1.0 倍），键是计量维度名。省略的维度用模型级 markup_bp")
	schema["propertyNames"] = map[string]any{"pattern": "^[a-z][a-z0-9_]{0,31}$"}
	return schema
}

// dimensionMarkupExample overrides output only, which is the common case (sell output dearer).
func dimensionMarkupExample() map[string]any {
	return map[string]any{"output": 15000}
}
