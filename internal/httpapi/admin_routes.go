package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/winger/ai-gateway/internal/store"
)

// This file is the single source of truth for the management surface: every
// /admin/api/v1 route is declared here with the handler that serves it plus the
// metadata an MCP client needs (a stable tool name, a one-line summary, the path
// and query parameters, the request body schema and whether the call is
// destructive). routes() registers whatever the table contains, and the MCP admin
// bridge dispatches into the very same handlers, so the agent surface cannot drift
// from the console surface.
//
// Adding an endpoint means adding one entry here; admin_routes_test.go fails when a
// registered pattern is missing from the table, when a tool name repeats, when a
// {param} placeholder has no declaration, or when an entry carries no summary.

// Roles an entry requires. They mirror adminActor(requireAdmin): roleAdmin entries
// call adminActor(w, r, true) and therefore answer 403 to a read-only caller.
const (
	roleViewer = "viewer"
	roleAdmin  = "admin"
)

// Resource families used to group the catalogue.
const (
	groupSystem   = "system"
	groupKeys     = "keys"
	groupRequests = "requests"
	groupAudit    = "audit"
	groupAccounts = "accounts"
	// groupOrg is the organization structure. It is its own group rather than a corner of
	// accounts because it is an independent entity: an operator can run the organization tree
	// without touching tags, and the tree organizes the accounts it does not own.
	groupOrg       = "org"
	groupModels    = "models"
	groupProviders = "providers"
	groupBilling   = "billing"
	groupBackups   = "backups"
	groupPortal    = "portal"
	groupPricing   = "pricing"
	groupMCP       = "mcp"
	groupHooks     = "hooks"
	groupSettings  = "settings"
	// groupChat lives in admin_chat_routes.go, next to the routes it groups.
)

// adminField documents one path parameter, query parameter or body field.
//
// Desc is not decoration: it is the only thing an agent reads about a parameter. Say what
// the value means, its unit, its default and (for closed sets) use Enum. See
// docs/mcp.md §4.5 — a field described as a bare noun ("成本侧计价规则") is how an agent was
// once left unable to write a cost rule at all.
//
// Schema and Example exist because Type alone is not a contract for anything structured:
// before them, an object-typed field reached the model as {"type":"object"} with an example
// of {}, so the model saw a field with no fields and refused to write it. A field whose Type
// is "object" must carry a shape (Schema, or the whole body through RawBody) — bodySchema
// panics otherwise, deliberately: the silent version of this is what broke.
type adminField struct {
	Name     string
	Type     string // string|integer|number|boolean|object|array
	Desc     string
	Required bool
	Enum     []string
	// Schema replaces the generated {"type": Type} in the endpoint's body schema. It is how
	// a nested document states its own field names, units and constraints.
	Schema map[string]any
	// Example replaces the placeholder sampleBody/sampleForType would invent for this field:
	// for an object that placeholder is {}, which teaches an agent nothing and can even be
	// rejected by a strict parser. It must be a document the endpoint really accepts.
	Example any
}

func pathParam(name, desc string) adminField {
	return adminField{Name: name, Type: "string", Desc: desc, Required: true}
}

func queryParam(name, typ, desc string) adminField {
	return adminField{Name: name, Type: typ, Desc: desc}
}

func bodyRequired(name, typ, desc string) adminField {
	return adminField{Name: name, Type: typ, Desc: desc, Required: true}
}

func bodyOptional(name, typ, desc string) adminField {
	return adminField{Name: name, Type: typ, Desc: desc}
}

// schemaField gives a field its real shape. Use it for anything structured, and always for
// a field whose Type is "object".
func schemaField(field adminField, schema map[string]any) adminField {
	field.Schema = schema
	return field
}

// exampleField gives a field a copyable example value. The example is a promise: it must be
// accepted by the endpoint (see TestMCPPricingExampleIsWritable).
//
// The value is mirrored into the schema it belongs to, in two places that matter:
//
//   - schema["example"], so an agent reading only body_schema (not the example arguments) still
//     sees a value to copy;
//   - field.Example, which is what sampleBody and the guard test read.
//
// A field may carry both a shape and an example, in either order; this is the one place that
// keeps the two views of the same value in step.
func exampleField(field adminField, value any) adminField {
	field.Example = value
	if field.Schema != nil {
		field.Schema["example"] = value
	}
	return field
}

// structuredField is the pair the two helpers above are used in for an object body field: a
// shape plus a writable example. It is named for what it produces (a field with a shape)
// rather than for its JSON type, because the same helper documents array fields too — what
// matters is that a structured field never reaches an agent without both.
func structuredField(name, typ, desc string, schema map[string]any, example any) adminField {
	field := schemaField(bodyOptional(name, typ, desc), schema)
	if example != nil {
		field = exampleField(field, example)
	}
	return field
}

func enumField(field adminField, values ...string) adminField {
	field.Enum = values
	return field
}

// numericField constrains a numeric field. The bounds are part of the interface an agent reads:
// a weight documented only as "同层内的权重" leaves it to guess the range, and a guessed 0 or a
// negative is either rejected or meaningless. Bounds are declared here only when the handler or
// the storage really enforces them — a bound that is not true is worse than none, because a model
// will trust it.
func numericField(field adminField, minimum, maximum int) adminField {
	schema := map[string]any{"type": field.Type, "minimum": minimum, "maximum": maximum,
		"description": field.Desc}
	return schemaField(field, schema)
}

// intRange is numericField for a field with a known upper bound.
func intRange(field adminField, minimum, maximum int) adminField {
	return numericField(field, minimum, maximum)
}

// nonNegative constrains a field to 0 and above, the bound most of these fields really have.
func nonNegative(field adminField) adminField {
	return numericField(field, 0, maxSaneInteger)
}

// maxSaneInteger is the ceiling used for fields whose handler enforces no upper bound: it is
// listed so the schema communicates "a plain non-negative integer" without pretending to know a
// domain limit. It stays inside int32 so a value copied from the schema cannot overflow a
// downstream column.
const maxSaneInteger = 1 << 31

// stringExample pins the example for a required name/id field. A required field has no default to
// fall back on, so the example has to carry a value an agent can recognize as its own to fill in.
func stringExample(field adminField, value string) adminField {
	return schemaField(field, map[string]any{"type": "string", "example": value, "description": field.Desc})
}

// dimensionQueryFields are the filters every request-log read accepts: the account and API
// key the request authenticated with, plus the six identity dimensions. They are declared
// once so the console, MCP's admin_describe and the store's filter cannot drift apart.
func dimensionQueryFields() []adminField {
	return []adminField{
		queryParam("account_id", "integer", "按账户（用户）过滤；非数字返回 400"),
		queryParam("api_key_id", "integer", "按 API Key 过滤（凭据 id，见 GET /keys）；非数字返回 400"),
		queryParam("client", "string", "按客户端过滤：dsh | codex | unknown"),
		queryParam("model", "string", "按请求的模型名过滤（账单口径，与发票分组一致）"),
		queryParam("resolved_model", "string", "按路由后的规范模型名过滤"),
		queryParam("workspace", "string", "按工作区根路径过滤"),
		queryParam("session_id", "string", "按会话 id 过滤（显式会话标识优先，缺失时使用 prompt_cache_key）"),
		queryParam("call_kind", "string", "按调用类型过滤：agent | title"),
	}
}

// prop describes one property of a hand-written body schema.
func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// objectSchema builds a JSON Schema object from hand-written properties.
func objectSchema(properties map[string]any, required ...string) json.RawMessage {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	return raw
}

// freeFormSchema documents a body whose shape is not enumerable as fields.
func freeFormSchema(desc string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{"description": desc})
	if err != nil {
		return nil
	}
	return raw
}

// adminRoute is one management endpoint.
type adminRoute struct {
	Method  string
	Path    string // ServeMux pattern, e.g. /admin/api/v1/providers/{id}
	Handler http.HandlerFunc

	// Name is the MCP tool name passed to admin_request and admin_describe.
	Name    string
	Group   string
	Summary string
	Role    string

	// Dangerous marks a call that deletes data, moves money or touches
	// credentials; admin_request refuses it unless confirm is true.
	Dangerous     bool
	ConfirmReason string

	Params  []adminField // path parameters
	Query   []adminField
	Body    []adminField    // flat request bodies
	RawBody json.RawMessage // bodies that are not flat (JSON Schema)

	Notes string

	// NoTool is non-empty for endpoints that are registered but not offered to MCP
	// clients; the value explains why.
	NoTool string
}

func (r adminRoute) pattern() string { return r.Method + " " + r.Path }

func (r adminRoute) exposed() bool { return r.NoTool == "" }

func (r adminRoute) hasBody() bool { return len(r.Body) > 0 || len(r.RawBody) > 0 }

// bodySchema renders the request body as a JSON Schema object, or nil when the
// endpoint takes no body.
//
// A field declared as an object with no Schema panics instead of degrading to
// {"type":"object"}: that degradation is not a smaller answer, it is a wrong one. It told
// an agent "pricing_rules is an object" and nothing else, and the agent — correctly —
// refused to write a document whose field names it could not know. Failing at
// construction (startup, first tools/list, or any test run) is strictly better than
// shipping a field description no model can act on. See docs/mcp.md §4.5.
func (r adminRoute) bodySchema() map[string]any {
	if len(r.RawBody) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(r.RawBody, &schema); err == nil {
			return schema
		}
	}
	if len(r.Body) == 0 {
		return nil
	}
	return schemaForFields(r.Name, r.Body)
}

// schemaForFields turns declared fields into a JSON Schema object. It takes the endpoint
// name so the panic above can say which route to fix.
func schemaForFields(where string, fields []adminField) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(fields))
	for _, field := range fields {
		if field.Schema != nil {
			// A hand-written shape carries its own type and description.
			properties[field.Name] = field.Schema
		} else {
			if field.Type == "object" {
				panic(fmt.Sprintf(
					"httpapi: %s field %q is declared as an object with no Schema: an agent would "+
						"see {\"type\":\"object\"} and cannot write it; give it a shape with "+
						"schemaField(...) or declare the whole body with RawBody "+
						"(standard: docs/mcp.md §4.5 工具说明标准)",
					where, field.Name))
			}
			property := map[string]any{"type": field.Type, "description": field.Desc}
			if len(field.Enum) > 0 {
				property["enum"] = field.Enum
			}
			properties[field.Name] = property
		}
		if field.Required {
			required = append(required, field.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// summaryRow is the compact catalogue entry returned by admin_endpoints.
//
// body_fields names the body's top-level fields. It is here because "has_body: true" alone
// forces an agent to spend an admin_describe call just to learn whether an endpoint it is
// looking at could be the one it needs.
func (r adminRoute) summaryRow() map[string]any {
	row := map[string]any{
		"name": r.Name, "method": r.Method, "path": r.Path,
		"summary": r.Summary, "group": r.Group, "role": r.Role,
		"params": fieldNames(r.Params), "query": fieldNames(r.Query),
		"has_body": r.hasBody(), "body_fields": fieldNames(r.Body), "dangerous": r.Dangerous,
	}
	if r.exposed() {
		row["tool"] = r.Name
	} else {
		row["tool"] = nil
		row["reason"] = r.NoTool
	}
	return row
}

// detail is the full description returned by admin_describe.
func (r adminRoute) detail() map[string]any {
	payload := map[string]any{
		"name": r.Name, "method": r.Method, "path": r.Path, "group": r.Group,
		"summary": r.Summary, "role": r.Role,
		"dangerous": r.Dangerous, "exposed": r.exposed(),
	}
	if r.Notes != "" {
		payload["notes"] = r.Notes
	}
	if r.NoTool != "" {
		payload["not_exposed_reason"] = r.NoTool
	}
	if r.ConfirmReason != "" {
		payload["confirm_reason"] = r.ConfirmReason
	}
	if len(r.Params) > 0 {
		payload["params"] = fieldDocs(r.Params)
	}
	if len(r.Query) > 0 {
		payload["query"] = fieldDocs(r.Query)
	}
	if schema := r.bodySchema(); schema != nil {
		payload["body_schema"] = schema
	}
	payload["example"] = r.example()
	return payload
}

// example is a ready-to-copy admin_request argument object.
func (r adminRoute) example() map[string]any {
	arguments := map[string]any{"name": r.Name}
	if len(r.Params) > 0 {
		params := map[string]any{}
		for _, field := range r.Params {
			params[field.Name] = sampleValue(field)
		}
		arguments["params"] = params
	}
	if len(r.Query) > 0 {
		query := map[string]any{}
		for _, field := range r.Query {
			query[field.Name] = sampleValue(field)
		}
		arguments["query"] = query
	}
	if body := r.sampleBody(); body != nil {
		arguments["body"] = body
	}
	if r.Dangerous {
		arguments["confirm"] = true
	}
	return map[string]any{"tool": "admin_request", "arguments": arguments}
}

// sampleBody renders the example body: a declared Example wins over anything invented from
// the schema, because an invented object is {} and an agent that copies {} gets a 400.
func (r adminRoute) sampleBody() map[string]any {
	if len(r.Body) == 0 {
		if schema := r.bodySchema(); schema != nil {
			return sampleBody(schema)
		}
		return nil
	}
	out := map[string]any{}
	for _, field := range r.Body {
		out[field.Name] = sampleValue(field)
	}
	return out
}

func fieldNames(fields []adminField) []string {
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, field.Name)
	}
	return out
}

func fieldDocs(fields []adminField) []map[string]any {
	out := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		doc := map[string]any{
			"name": field.Name, "type": field.Type,
			"description": field.Desc, "required": field.Required,
		}
		if len(field.Enum) > 0 {
			doc["enum"] = field.Enum
		}
		out = append(out, doc)
	}
	return out
}

func sampleValue(field adminField) any {
	if field.Example != nil {
		return field.Example
	}
	if field.Schema != nil {
		// A declared shape produces a value that satisfies itself: a model copies the example
		// verbatim, so an example that violates its own schema teaches a 400.
		return sampleFromSchema(field.Schema, field.Name)
	}
	if len(field.Enum) > 0 {
		return field.Enum[0]
	}
	return sampleForType(field.Type, field.Name)
}

func sampleForType(typ, name string) any {
	switch typ {
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		return "<" + name + ">"
	}
}

// sampleBody renders a placeholder body from a schema, for the routes that declare their whole
// body with RawBody. It is schema-aware on purpose: inventing {} for a nested object, or 0 for a
// field whose minimum is 1, produces an example the endpoint then rejects — the same class of
// defect as describing a field as a bare object. TestGeneratedExamplesSatisfyTheirSchema checks
// this for every route.
func sampleBody(schema map[string]any) map[string]any {
	value, _ := sampleFromSchema(schema, "").(map[string]any)
	if value == nil {
		return map[string]any{}
	}
	return value
}

// sampleFromSchema builds one value that satisfies the schema it is given.
//
// It is deliberately conservative: when it cannot produce a satisfiable value (an unparseable
// regex pattern, say) it returns a recognizable placeholder rather than a plausible-looking
// wrong value, so the failure shows up in the guard test instead of in an agent's call.
func sampleFromSchema(schema map[string]any, name string) any {
	if len(schema) == 0 {
		return sampleForType("", name)
	}
	if example, ok := schema["example"]; ok {
		return example
	}
	if enum, ok := schema["enum"].([]string); ok && len(enum) > 0 {
		return enum[0]
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch typ, _ := schema["type"].(string); typ {
	case "object":
		properties, _ := schema["properties"].(map[string]any)
		out := map[string]any{}
		names := make([]string, 0, len(properties))
		for property := range properties {
			names = append(names, property)
		}
		sort.Strings(names)
		for _, property := range names {
			sub, _ := properties[property].(map[string]any)
			out[property] = sampleFromSchema(sub, property)
		}
		// An object with no declared properties renders as an explicitly empty document: it is
		// honest about there being nothing to fill in.
		return out
	case "array":
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return []any{}
		}
		return []any{sampleFromSchema(items, name)}
	case "integer", "number":
		return sampleNumber(schema)
	case "boolean":
		return false
	case "string":
		return sampleString(schema, name)
	default:
		return sampleForType(typ, name)
	}
}

// sampleNumber picks the smallest value the schema allows, because that is the only choice that
// cannot exceed a maximum the author wrote down.
func sampleNumber(schema map[string]any) any {
	value := 0.0
	if minimum, ok := numeric(schema["minimum"]); ok && minimum > value {
		value = minimum
	}
	if maximum, ok := numeric(schema["maximum"]); ok && value > maximum {
		value = maximum
	}
	switch schema["type"] {
	case "integer":
		return int(value)
	default:
		return value
	}
}

// sampleString satisfies minLength when one is declared, and otherwise falls back to a
// placeholder an agent is meant to replace.
func sampleString(schema map[string]any, name string) any {
	// A declared format or pattern means the value has meaning the generator cannot guess;
	// the placeholder keeps that visible instead of inventing a well-formed wrong value.
	if schema["pattern"] != nil || schema["format"] != nil {
		return "<" + name + ">"
	}
	placeholder := "<" + name + ">"
	if minimum, ok := numeric(schema["minLength"]); ok {
		for float64(len(placeholder)) < minimum {
			placeholder += "x"
		}
	}
	return placeholder
}

// numeric reads a JSON number as it arrives from a map[string]any literal.
func numeric(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// adminRoutes assembles the whole table.
func (s *Server) adminRoutes() []adminRoute {
	out := make([]adminRoute, 0, 96)
	out = append(out, s.systemAdminRoutes()...)
	out = append(out, s.catalogAdminRoutes()...)
	out = append(out, s.providerAdminRoutes()...)
	out = append(out, s.billingAdminRoutes()...)
	out = append(out, s.invoiceAdminRoutes()...)
	out = append(out, s.backupAdminRoutes()...)
	out = append(out, s.portalAdminRoutes()...)
	out = append(out, s.pricingAdminRoutes()...)
	out = append(out, s.chatAdminRoutes()...)
	return out
}

// systemAdminRoutes covers authentication, statistics, API keys, recorded requests
// and the audit trail.
func (s *Server) systemAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "POST", Path: "/admin/api/v1/auth/login", Handler: s.handleAdminLogin,
			Name: "admin_login", Group: groupSystem, Role: roleViewer,
			Summary: "管理员登录并签发会话 Cookie",
			NoTool:  "登录返回的是浏览器 Cookie 会话，MCP 客户端用令牌鉴权，此接口在 MCP 下没有意义",
		},
		{
			Method: "POST", Path: "/admin/api/v1/auth/logout", Handler: s.handleAdminLogout,
			Name: "admin_logout", Group: groupSystem, Role: roleViewer,
			Summary: "退出登录并清除会话 Cookie",
			NoTool:  "MCP 调用没有 Cookie 会话可退出；要收回 MCP 权限请用 admin_revoke_mcp_token",
		},
		{
			Method: "GET", Path: "/admin/api/v1/auth/me", Handler: s.handleAdminMe,
			Name: "admin_whoami", Group: groupSystem, Role: roleViewer,
			Summary: "当前调用者的身份与角色（MCP 调用返回 mcp:<令牌名>#<id>）",
		},
		{
			Method: "GET", Path: "/admin/api/v1/stats", Handler: s.handleAdminStats,
			Name: "admin_stats", Group: groupSystem, Role: roleViewer,
			Summary: "运行概览：注册表计数（模型/供应商/路由/账户）、负载均衡指标、冷却中的目标、供应商并发与排队（含排队策略）、版本",
		},
		{
			Method: "GET", Path: "/admin/api/v1/keys", Handler: s.handleAdminListKeys,
			Name: "admin_list_keys", Group: groupKeys, Role: roleViewer,
			Summary: "列出 API Key（自有/账号继承/生效标签、前缀、状态与内容录制开关）",
			Query: append(pageConfig.fields(),
				queryParam("account_id", "integer", "只看某个账户的 Key，省略表示全部")),
		},
		{
			Method: "POST", Path: "/admin/api/v1/keys", Handler: s.handleAdminCreateKey,
			Name: "admin_create_key", Group: groupKeys, Role: roleAdmin,
			Summary:   "签发一个新的 API Key（明文只在响应里出现一次）",
			Dangerous: true, ConfirmReason: "会签发一个可以立即消费额度的新密钥，且明文只返回一次",
			Body: []adminField{
				bodyRequired("name", "string", "Key 名称"),
				nonNegative(bodyOptional("account_id", "integer", "所属账户 id（与 account 二选一）")),
				bodyOptional("account", "string", "所属账户名（与 account_id 二选一）"),
				structuredField("tags", "array", "Key 自有标签名数组；与账号标签取并集并决定授权与策略合并；标签必须在 admin_list_tags 里存在", arrayOfStrings("标签名列表"), []any{}),
				structuredField("grants", "object", "这个 Key 能调用哪些模型与供应商（与 tag 的授权取并集）",
					grantsSchema("Key"), grantsExample()),
				exampleField(schemaField(bodyOptional("policy", "object",
					"限速与配额策略：顶层扁平字段，只有 rpm/tpm/concurrency（已强制执行）、monthly_*（只解析未执行）、"+
						"strategy/provider_order/margin_bp（影响路由与加价）被接受，其它键一律 400。字段说明见本参数 schema"),
					policySchema()), keyPolicyExample()),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/keys/import", Handler: s.handleAdminImportKey,
			Name: "admin_import_key", Group: groupKeys, Role: roleAdmin,
			Summary:   "导入一把已经存在的密钥：只接收明文的前 12 字符与它的 SHA-256，网关不接触明文（迁移用）",
			Dangerous: true, ConfirmReason: "会把一份已存在的明文密钥接入网关：知道该明文的人立刻可以消费额度",
			Notes: "用途是把别的网关/平台里已经在用的 key 搬过来，让客户端不用改 key。" +
				"key_prefix 必须是明文的前 12 个字符（网关按它建索引），key_hash 必须是同一明文的 SHA-256 十六进制。" +
				"接口无法校验两者是否同源，写错只会得到一把认证不通过的 key，不会泄露任何东西。" +
				"明文不落库、不返回、不进审计；同一前缀重复导入是幂等的，但该前缀已被控制台签发的 key 占用时返回 409。",
			Body: []adminField{
				bodyRequired("key_prefix", "string", "明文密钥的前 12 个字符（网关据此前缀索引查找）"),
				bodyRequired("key_hash", "string", "同一明文的 SHA-256 十六进制（64 位小写）"),
				bodyRequired("name", "string", "Key 名称（迁移时沿用源系统名称便于对账）"),
				nonNegative(bodyOptional("account_id", "integer", "所属账户 id（与 account 二选一）")),
				bodyOptional("account", "string", "所属账户名（与 account_id 二选一）"),
				structuredField("tags", "array", "Key 自有标签名数组；这些名字必须已经存在，否则 400（不存在的名字会被解析丢弃，授权随即回落到默认通配）", arrayOfStrings("标签名列表"), []any{}),
				structuredField("grants", "object", "这个 Key 能调用哪些模型与供应商（与 tag 的授权取并集）",
					grantsSchema("Key"), grantsExample()),
				enumField(bodyOptional("status", "string", "导入后的状态，省略为 active；disabled 表示先记录、暂不使用"), "active", "disabled"),
				bodyOptional("expires_at", "string", "RFC3339 过期时间，省略表示不过期"),
				exampleField(schemaField(bodyOptional("policy", "object",
					"限速与配额策略，字段与创建 Key 完全相同（见本参数 schema）"),
					policySchema()), keyPolicyExample()),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/keys/{id}", Handler: s.handleAdminPatchKey,
			Name: "admin_update_key", Group: groupKeys, Role: roleAdmin,
			Summary:   "改 Key 的标签、状态、配额策略与内容录制开关（输入/思考/最终输出）",
			Dangerous: true, ConfirmReason: "可停用或启用密钥，也能改动内容录制开关（影响隐私）",
			Params: []adminField{pathParam("id", "API Key 的数字 id")},
			Body: []adminField{
				structuredField("tags", "array", "替换 Key 自有标签；空数组清空；与账号标签取并集", arrayOfStrings("标签名列表"), []any{}),
				enumField(bodyOptional("status", "string", "状态"), "active", "disabled"),
				bodyOptional("record_reasoning", "boolean", "是否保存思考文本"),
				bodyOptional("record_output_text", "boolean", "是否保存最终输出文本"),
				enumField(bodyOptional("record_input_mode", "string", "输入文本录制级别：user 只记用户输入（默认），full 整份请求正文，metadata 只记元数据，off 不记"), "inherit", "user", "full", "metadata", "off"),
				exampleField(schemaField(bodyOptional("policy", "object",
					"限速与配额策略，字段与创建 Key 完全相同（见本参数 schema）；省略则不改，写 null 表示清空"),
					policySchema()), keyPolicyExample()),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/requests", Handler: s.handleAdminRequests,
			Name: "admin_list_requests", Group: groupRequests, Role: roleViewer,
			Summary: "全部账户的请求日志（跨账户视图，可按账户/API Key、天数与身份维度过滤；带该请求的用户与 Key 名字、token 与成本）",
			Query: append(append(pageRequests.fields(),
				queryParam("days", "integer", "回溯天数，默认 7，最大 365")),
				dimensionQueryFields()...),
		},
		{
			// The literal path wins over /requests/{id} in Go's ServeMux, so a request
			// whose x-request-id is literally "dimensions" has no reachable detail page.
			// Request ids are req_* (internal/ids), so that is accepted rather than
			// worked around; /requests/prune has the same shape.
			Method: "GET", Path: "/admin/api/v1/requests/dimensions", Handler: s.handleAdminRequestDimensions,
			Name: "admin_request_dimensions", Group: groupRequests, Role: roleViewer,
			Summary: "请求日志的维度统计：按客户端/模型/工作区/会话/调用类型/账户（用户）/API Key 分组；请求数和已计量数按请求去重，token 与成本累计全部上游尝试；默认按最近一次请求时间降序，可分页（total 是分组数）",
			Query: append(append([]adminField{
				queryParam("days", "integer", "回溯天数，默认 7，最大 365"),
				enumField(queryParam("group_by", "string", "分组维度"), append([]string{"client"}, store.RequestLogDimensionNames...)...),
				enumField(queryParam("sort", "string",
					"排序键（均为降序，同数按分组键升序）：last_seen 最近一次请求时间（默认）| requests 请求数 | charge 对客成本（与列表「成本」列同源 charge_micros）"),
					store.RequestLogDimensionSorts...),
			}, pageDimensions.fields()...), dimensionQueryFields()...),
		},
		{
			Method: "GET", Path: "/admin/api/v1/requests/{id}", Handler: s.handleAdminRequestDetail,
			Name: "admin_get_request", Group: groupRequests, Role: roleViewer,
			Summary: "单条请求详情（输入/思考/输出，按录制开关决定是否可见；含身份维度、用户与 API Key 名字、token/成本）",
			Params:  []adminField{pathParam("id", "请求 id（x-request-id）")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/requests/prune", Handler: s.handleAdminPruneRequests,
			Name: "admin_prune_requests", Group: groupRequests, Role: roleAdmin,
			Summary:   "立即清理超过保留期的请求日志与已过期的存储响应（保留期取 recording.retention_days）",
			Dangerous: true, ConfirmReason: "会永久删除超过保留期的请求日志与已过期的存储响应；计费（usage/ledger）与审计记录不受影响",
		},
		{
			Method: "GET", Path: "/admin/api/v1/audit-logs", Handler: s.handleAdminAuditLogs,
			Name: "admin_list_audit_logs", Group: groupAudit, Role: roleViewer,
			Summary: "管理面审计流水（谁在什么时候改了什么）",
			Query:   pageAudit.fields(),
		},
		{
			Method: "GET", Path: "/admin/api/v1/router/explain", Handler: s.handleAdminExplainRouter,
			Name: "admin_explain_router", Group: groupSystem, Role: roleViewer,
			Summary: "解释一次路由决策：解析结果、有序候选与每个候选被排除的原因",
			Query: []adminField{
				{Name: "model", Type: "string", Desc: "对客模型名（必填）", Required: true},
				queryParam("account_id", "integer", "按某个账户的授权计算候选"),
				queryParam("api_key_id", "integer", "按某个 Key 的授权与策略计算候选"),
				queryParam("provider", "string", "钉死供应商名"),
				queryParam("strategy", "string", "层内负载均衡策略"),
				queryParam("features", "string", "需要的能力，逗号分隔，例如 stream,tools"),
			},
		},
	}
}

// catalogAdminRoutes covers accounts, models, name mappings, routes, tags, MCP
// tokens, hooks and settings.
func (s *Server) catalogAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/accounts", Handler: s.handleAdminListAccounts,
			Name: "admin_list_accounts", Group: groupAccounts, Role: roleViewer,
			Summary: "列出全部账户（计费模式、状态、标签、所属组织、授信与低余额阈值）",
			Query: append(pageConfig.fields(),
				queryParam("org_node_id", "integer",
					"只看某个组织节点下的账户（默认连同子节点，见 include_descendants）；不传则不按组织过滤。节点 id 来自 admin_list_org_nodes"),
				queryParam("include_descendants", "boolean",
					"配合 org_node_id：默认 true 表示连同该节点的所有子孙节点一起返回，false 表示只返回直接挂在该节点上的账户")),
		},
		{
			Method: "POST", Path: "/admin/api/v1/accounts", Handler: s.handleAdminCreateAccount,
			Name: "admin_create_account", Group: groupAccounts, Role: roleAdmin,
			Summary: "新建账户（按名字 upsert）",
			Body: []adminField{
				bodyRequired("name", "string",
					"账户名；去除首尾空白后须非空，最多 64 个 Unicode 字符，可用邮箱、中文等任意 Unicode（同名按名字 upsert）"),
				enumField(bodyOptional("billing_mode", "string", "计费模式"), "postpaid", "prepaid"),
				bodyOptional("note", "string", "备注"),
				structuredField("tags", "array", "账号级标签名数组；账号下所有 API Key 自动继承，并与 Key 自有标签取并集；空数组清空", arrayOfStrings("标签名列表"), []any{}),
				structuredField("org_node_ids", "array",
					"该账号所属的组织节点 id 列表（多归属：账号可同时属于多个节点）。账号下所有 API Key 会继承这些节点及其祖先节点上的标签。"+
						"任意 id 不存在会 404，且不会改动已有归属",
					idArraySchema(), []any{}),
				nonNegative(bodyOptional("credit_limit_micros", "integer", "后付授信上限（微美元，整数，负数会被拒）")),
				nonNegative(bodyOptional("low_balance_threshold_micros", "integer", "低余额告警阈值（微美元）")),
				bodyOptional("overdraft_limit_micros", "integer", "允许的透支额度（微美元）"),
				nonNegative(bodyOptional("markup_override_bp", "integer", "账户级加价倍数（bp，0 表示免费；10000 = 1.0 倍）")),
				bodyOptional("auto_suspend", "boolean", "欠费自动暂停"),
				bodyOptional("auto_resume", "boolean", "充值后自动恢复"),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/accounts/{id}", Handler: s.handleAdminPatchAccount,
			Name: "admin_update_account", Group: groupAccounts, Role: roleAdmin,
			Summary:   "改账户的计费模式、状态、标签、授信、加价倍数与在途策略",
			Dangerous: true, ConfirmReason: "可暂停或恢复账户，并改动授信额度、加价倍数与在途策略（直接影响出账）",
			Params: []adminField{pathParam("id", "账户数字 id")},
			Body: []adminField{
				enumField(bodyOptional("billing_mode", "string", "计费模式"), "postpaid", "prepaid"),
				enumField(bodyOptional("status", "string", "账户状态"), "active", "suspended"),
				bodyOptional("note", "string", "备注"),
				structuredField("tags", "array", "替换账号级标签；空数组清空；账号下所有 API Key 自动继承并与 Key 标签取并集", arrayOfStrings("标签名列表"), []any{}),
				structuredField("org_node_ids", "array",
					"整表替换该账号所属的组织节点（空数组表示移出全部组织）。账号下所有 API Key 会立即失去/获得相应节点及其祖先节点的标签授权",
					idArraySchema(), []any{}),
				nonNegative(bodyOptional("credit_limit_micros", "integer", "后付授信上限（微美元，整数，负数会被拒）")),
				nonNegative(bodyOptional("low_balance_threshold_micros", "integer", "低余额告警阈值（微美元）")),
				bodyOptional("overdraft_limit_micros", "integer", "允许的透支额度（微美元）"),
				bodyOptional("markup_override_bp", "integer", "账户级加价倍数（bp）"),
				bodyOptional("auto_suspend", "boolean", "欠费自动暂停"),
				bodyOptional("auto_resume", "boolean", "充值后自动恢复"),
				bodyOptional("inflight_policy_override", "string", "在途超额策略覆写"),
				exampleField(schemaField(bodyOptional("price_overrides", "object",
					"价格覆写文档。字段说明见本参数 schema"), accountPriceOverridesSchema()),
					map[string]any{}),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/models", Handler: s.handleAdminListModels,
			Name: "admin_list_models", Group: groupModels, Role: roleViewer,
			Summary: "列出对客模型（含别名、启用状态与售价文档）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/models", Handler: s.handleAdminUpsertModel,
			Name: "admin_upsert_model", Group: groupModels, Role: roleAdmin,
			Summary: "新建或更新对客模型（按 public_name upsert）",
			Notes:   "sale_pricing 直接决定对客售价，改动会立刻影响计费",
			Body: []adminField{
				bodyRequired("public_name", "string", "对客模型名"),
				bodyOptional("display_name", "string", "展示名"),
				structuredField("aliases", "array", "别名数组：客户端用别名请求时也能路由到这个模型", arrayOfStrings("别名列表"), []any{}),
				bodyOptional("enabled", "boolean", "是否对客可用"),
				salePricingField("对客售价规则集：客户按它计费（成本侧在 provider_models 的 pricing_rules）。"+
					"只想「售价 = 成本」时写 {\"basis\":\"cost_follow\",\"markup_bp\":10000}；"+
					"要独立定价写 {\"basis\":\"absolute\",\"rules\":[…]}（此时必须至少一条规则）。"+
					"字段名与结构必须严格按 schema 写（未知字段 400），单位是微单位/百万 token；拿不准先 admin_validate_pricing。",
					s.ledgerCurrency()),
				exampleField(schemaField(bodyOptional("policy", "object", "模型级策略。字段说明见本参数 schema"), modelPolicySchema()),
					map[string]any{}),
				exampleField(schemaField(bodyOptional("reasoning", "object", "模型级推理覆写；省略不改，写 null 清空并继承。字段说明见本参数 schema"), modelReasoningSchema()),
					map[string]any{"mode": "force", "effort": "high"}),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/models/{name}", Handler: s.handleAdminUpsertModel,
			Name: "admin_update_model", Group: groupModels, Role: roleAdmin,
			Summary: "按名更新对客模型（字段同 admin_upsert_model，模型名走路径）",
			Params:  []adminField{pathParam("name", "对客模型名")},
			Body: []adminField{
				bodyOptional("display_name", "string", "展示名"),
				structuredField("aliases", "array", "别名数组：客户端用别名请求时也能路由到这个模型", arrayOfStrings("别名列表"), []any{}),
				bodyOptional("enabled", "boolean", "是否对客可用"),
				salePricingField("对客售价规则集，与 admin_upsert_model 同一形状；省略则不改。"+
					"只改加价倍数请用 admin_update_markup（它保留规则数组，避免并发编辑互相覆盖）",
					s.ledgerCurrency()),
				exampleField(schemaField(bodyOptional("policy", "object", "模型级策略。字段说明见本参数 schema"), modelPolicySchema()),
					map[string]any{}),
				exampleField(schemaField(bodyOptional("reasoning", "object", "模型级推理覆写；省略不改，写 null 清空并继承。字段说明见本参数 schema"), modelReasoningSchema()),
					map[string]any{"mode": "force", "effort": "high"}),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/model-mappings", Handler: s.handleAdminListMappings,
			Name: "admin_list_model_mappings", Group: groupModels, Role: roleViewer,
			Summary: "列出模型名映射规则（exact/prefix/glob/regex）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/model-mappings", Handler: s.handleAdminUpsertMapping,
			Name: "admin_upsert_model_mapping", Group: groupModels, Role: roleAdmin,
			Summary: "新建或更新一条模型名映射规则",
			Body: []adminField{
				enumField(bodyRequired("kind", "string", "匹配方式"), "exact", "prefix", "glob", "regex"),
				bodyRequired("pattern", "string", "匹配模式"),
				bodyOptional("target_model", "string", "改写成的模型名（可用 {model} 与捕获组）"),
				nonNegative(bodyOptional("target_provider_id", "integer", "钉死供应商 id")),
				bodyOptional("target_provider", "string", "钉死供应商名"),
				bodyOptional("target_upstream_model", "string", "改写成的上游模型名"),
				nonNegative(bodyOptional("priority", "integer", "优先级，数字越大越先匹配（0 及以上）")),
				bodyOptional("enabled", "boolean", "是否启用"),
				bodyOptional("note", "string", "备注"),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/model-mappings/{id}", Handler: s.handleAdminDeleteMapping,
			Name: "admin_delete_model_mapping", Group: groupModels, Role: roleAdmin,
			Summary:   "删除一条模型名映射规则",
			Dangerous: true, ConfirmReason: "删除后该映射立即失效（不可撤销）",
			Params: []adminField{pathParam("id", "映射规则数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/routes", Handler: s.handleAdminListRoutes,
			Name: "admin_list_routes", Group: groupModels, Role: roleViewer,
			Summary: "列出模型到供应商的路由（优先级、权重、启用状态）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/routes", Handler: s.handleAdminUpsertRoute,
			Name: "admin_upsert_route", Group: groupModels, Role: roleAdmin,
			Summary: "新建或更新一条路由（对客模型到供应商上游模型）",
			Body: []adminField{
				nonNegative(bodyOptional("model_id", "integer", "对客模型 id（与 model 二选一）")),
				bodyOptional("model", "string", "对客模型名（与 model_id 二选一）"),
				nonNegative(bodyOptional("provider_id", "integer", "供应商 id（与 provider 二选一）")),
				bodyOptional("provider", "string", "供应商名（与 provider_id 二选一）"),
				bodyOptional("upstream_model", "string", "上游模型名，省略表示同名"),
				nonNegative(bodyOptional("priority", "integer", "优先级，数字越大越先选（0 及以上）")),
				nonNegative(bodyOptional("weight", "integer", "同层内的权重（0 及以上；0 表示不参与抽选）")),
				bodyOptional("enabled", "boolean", "是否启用"),
				exampleField(schemaField(bodyOptional("policy", "object", "路由级策略。字段说明见本参数 schema"), routePolicySchema()), map[string]any{}),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/routes/{id}", Handler: s.handleAdminPatchRoute,
			Name: "admin_update_route", Group: groupModels, Role: roleAdmin,
			Summary: "改一条路由的权重、启用状态、策略，或清掉它的冷却",
			Params:  []adminField{pathParam("id", "路由数字 id")},
			Body: []adminField{
				bodyOptional("upstream_model", "string", "上游模型名"),
				bodyOptional("priority", "integer", "优先级"),
				bodyOptional("weight", "integer", "同层内权重"),
				bodyOptional("enabled", "boolean", "是否启用"),
				exampleField(schemaField(bodyOptional("policy", "object", "路由级策略。字段说明见本参数 schema"), routePolicySchema()), map[string]any{}),
				bodyOptional("reset_cooldown", "boolean", "是否清掉该目标的冷却与熔断状态"),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/routes/{id}", Handler: s.handleAdminDeleteRoute,
			Name: "admin_delete_route", Group: groupModels, Role: roleAdmin,
			Summary:   "删除一条路由",
			Dangerous: true, ConfirmReason: "删除后该模型可能没有可用上游（不可撤销）",
			Params: []adminField{pathParam("id", "路由数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/tags", Handler: s.handleAdminListTags,
			Name: "admin_list_tags", Group: groupModels, Role: roleViewer,
			Summary: "列出标签（授权并集与策略合并的来源）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/tags", Handler: s.handleAdminUpsertTag,
			Name: "admin_upsert_tag", Group: groupModels, Role: roleAdmin,
			Summary: "新建或更新一个标签（按名字 upsert；写标签名即可改它的授权与策略）",
			Body: []adminField{
				bodyRequired("name", "string", "标签名（人类可读标签：去除首尾空白后须非空，最多 64 个 Unicode 字符，中文/邮箱/标点均可）。这个名字就是绑定点：账号与 API Key 用名字引用它，改不了名"),
				bodyOptional("description", "string", "说明"),
				structuredField("grants", "object", "这个标签授予的模型与供应商访问权（绑定该标签的账号和 Key 都会获得）", grantsSchema("标签"), grantsExample()),
				exampleField(schemaField(bodyOptional("policy", "object",
					"该标签的限速与配额策略，字段与 Key 的 policy 相同（见本参数 schema）。"+
						"多个标签按 priority 依次合并、最后 Key 自己的 policy 覆盖"),
					policySchema()), keyPolicyExample()),
				nonNegative(bodyOptional("priority", "integer", "策略合并优先级（0 及以上，数字大者先合并）")),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/tags/{id}", Handler: s.handleAdminPatchTag,
			Name: "admin_update_tag", Group: groupModels, Role: roleAdmin,
			Summary:   "按 id 改一个标签的授权/策略/说明（只改传入的字段；不能改名）",
			Dangerous: true, ConfirmReason: "label 的授权是账号与 API Key 生效权限的一部分：加宽 grants 等于给所有绑定它的凭据放权",
			Params: []adminField{pathParam("id", "标签数字 id（admin_list_tags 给出）")},
			Body: []adminField{
				bodyOptional("name", "string", "只允许回传标签当前的名字（幂等）。改名会被拒绝（400）：标签是按名字绑定在 accounts.tags_json / api_keys.tags_json 上的，改名会静默丢掉全部绑定。要换名字就新建标签、搬绑定、再删旧的"),
				bodyOptional("description", "string", "说明"),
				structuredField("grants", "object", "新的授权（整体替换；传 null 清空）。与 admin_upsert_tag 的 grants 同形", grantsSchema("标签"), grantsExample()),
				exampleField(schemaField(bodyOptional("policy", "object",
					"新的限速与配额策略（整体替换；传 null 清空）。字段与 Key 的 policy 相同"),
					policySchema()), keyPolicyExample()),
				nonNegative(bodyOptional("priority", "integer", "策略合并优先级（0 及以上，数字大者先合并）")),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/tags/{id}", Handler: s.handleAdminDeleteTag,
			Name: "admin_delete_tag", Group: groupModels, Role: roleAdmin,
			Summary:   "删除一个标签",
			Dangerous: true, ConfirmReason: "删除后挂在它上面的授权与策略立即消失（不可撤销）；组织架构节点上的同名绑定同样失效",
			Params: []adminField{pathParam("id", "标签数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/org/nodes", Handler: s.handleAdminListOrgNodes,
			Name: "admin_list_org_nodes", Group: groupOrg, Role: roleViewer,
			Summary: "列出组织架构的节点（扁平列表 + parent_id/depth/path，前端不必自己算层级）",
			Query: append(pageConfig.fields(),
				queryParam("include_accounts", "boolean",
					"是否在每个节点内联成员账号（默认 false）。true 时每行多出 accounts[{id,name}] 与 accounts_truncated；"+
						"成员很多时用 admin_list_org_node_accounts 分页读")),
		},
		{
			Method: "POST", Path: "/admin/api/v1/org/nodes", Handler: s.handleAdminCreateOrgNode,
			Name: "admin_create_org_node", Group: groupOrg, Role: roleAdmin,
			Summary:   "新建组织节点（parent_id 为空即根节点；同一父节点下名字唯一）",
			Dangerous: true, ConfirmReason: "节点上绑定的标签会被整棵子树继承：一个带 grants 的标签等于给该子树下所有账号的全部 API Key 放权",
			Body: []adminField{
				bodyRequired("name", "string",
					"节点名；去除首尾空白后须非空，最多 64 个 Unicode 字符（中文/标点均可）。同一父节点下不能重名，不同父节点下可以同名"),
				bodyOptional("parent_id", "integer",
					"父节点数字 id（来自 admin_list_org_nodes）；省略或传 0 表示这是一个根节点。指向不存在的节点会 404"),
				bodyOptional("note", "string", "备注"),
				structuredField("tags", "array",
					"该节点绑定的标签名数组：整棵子树（本节点及所有子孙）下的账号都会继承这些标签，进而获得它们的授权与限速策略。"+
						"名字必须已经存在（admin_list_tags 可查），未知名字会 400：不存在的名字会被解析丢弃，"+
						"一旦该凭据因此没有任何授权，就会回落到 routing.default_grant（可能是通配全开）",
					arrayOfStrings("标签名列表"), []any{}),
				nonNegative(bodyOptional("sort_order", "integer", "同级排序，数字小者靠前（默认 100）")),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/org/nodes/{id}", Handler: s.handleAdminPatchOrgNode,
			Name: "admin_update_org_node", Group: groupOrg, Role: roleAdmin,
			Summary:   "改组织节点：改名 / 换父节点 / 改标签 / 改排序（只改传入的字段）",
			Dangerous: true, ConfirmReason: "改标签会即时改变该子树下所有账号与 API Key 的生效授权；换父节点会改变哪些账号继承到这些标签",
			Params: []adminField{pathParam("id", "组织节点数字 id（admin_list_org_nodes 给出）")},
			Body: []adminField{
				bodyOptional("name", "string", "新名字（最多 64 个字符）。可以改名：节点是按 id 被引用的，改名不会丢绑定"),
				bodyOptional("parent_id", "integer",
					"新父节点 id；传 0 表示把它变成根节点。移到自身或自己的子孙会 400（会让子树脱离所有根），"+
						"移动后使某个节点超过 16 层也会 400。注意：整棵子树会跟着移动，被移动的账号继承的标签随之改变"),
				bodyOptional("note", "string", "备注"),
				structuredField("tags", "array",
					"替换该节点的标签名数组（空数组清空）。整棵子树下的账号都继承这些标签；名字必须已存在，未知名字会 400",
					arrayOfStrings("标签名列表"), []any{}),
				nonNegative(bodyOptional("sort_order", "integer", "同级排序，数字小者靠前")),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/org/nodes/{id}", Handler: s.handleAdminDeleteOrgNode,
			Name: "admin_delete_org_node", Group: groupOrg, Role: roleAdmin,
			Summary:   "删除组织节点（有子节点时必须显式 cascade=true，会删掉整棵子树）",
			Dangerous: true, ConfirmReason: "删除会移除该节点（及 cascade 时的整棵子树）上的成员关系，账号与 API Key 本身不受影响，但继承来的标签授权会立即消失（不可撤销）",
			Params: []adminField{pathParam("id", "组织节点数字 id")},
			Query: []adminField{
				queryParam("cascade", "boolean",
					"默认 false：节点还有子节点时返回 409 并说明原因。传 true 表示确认删除整棵子树（本节点及全部子孙）"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/org/nodes/{id}/accounts", Handler: s.handleAdminListOrgNodeAccounts,
			Name: "admin_list_org_node_accounts", Group: groupOrg, Role: roleViewer,
			Summary: "列出某个组织节点下的成员账号（分页，含账号名与状态）",
			Params:  []adminField{pathParam("id", "组织节点数字 id")},
			Query:   pageConfig.fields(),
		},
		{
			Method: "PUT", Path: "/admin/api/v1/org/nodes/{id}/accounts", Handler: s.handleAdminSetOrgNodeAccounts,
			Name: "admin_set_org_node_accounts", Group: groupOrg, Role: roleAdmin,
			Summary:   "整表替换某个组织节点的成员账号（幂等：重复调用结果相同）",
			Dangerous: true, ConfirmReason: "整表替换：不在 account_ids 里的账号会被移出该组织，从而失去从该节点继承的标签与授权",
			Params: []adminField{pathParam("id", "组织节点数字 id")},
			Body: []adminField{
				structuredField("account_ids", "array",
					"该节点最终的成员账号 id 列表（整表替换，不是增量）。传空数组即清空该节点。"+
						"任意一个 id 不存在会 404，且不会改动已有成员。账号可以同时属于多个节点",
					idArraySchema(), []any{}),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/mcp-tokens", Handler: s.handleAdminListMCPTokens,
			Name: "admin_list_mcp_tokens", Group: groupMCP, Role: roleViewer,
			Summary: "列出 MCP 令牌（前缀、scope、状态、最近使用、过期时间）",
			Query: append(pageConfig.fields(),
				queryParam("account_id", "integer", "只看某个账户的令牌")),
		},
		{
			Method: "POST", Path: "/admin/api/v1/mcp-tokens", Handler: s.handleAdminCreateMCPToken,
			Name: "admin_create_mcp_token", Group: groupMCP, Role: roleAdmin,
			Summary:   "签发 MCP 令牌（明文只返回一次；scope 决定能否执行后台接口）",
			Dangerous: true, ConfirmReason: "会签发一个 MCP 凭据；scope=admin 的令牌等同于管理员权限",
			Body: []adminField{
				bodyRequired("name", "string", "令牌名称（审计里会带着它）"),
				bodyOptional("account_id", "integer", "绑定账户 id（与 account 二选一）"),
				bodyOptional("account", "string", "绑定账户名（与 account_id 二选一）"),
				enumField(bodyOptional("scope", "string", "权限级别：query 只查本账户数据，admin_read 可读后台接口，admin 可执行全部后台接口"), "query", "admin_read", "admin"),
				bodyOptional("note", "string", "备注"),
				bodyOptional("expires_at", "string", "过期时间（RFC3339，必须晚于现在）"),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/mcp-tokens/{id}", Handler: s.handleAdminPatchMCPToken,
			Name: "admin_update_mcp_token", Group: groupMCP, Role: roleAdmin,
			Summary:   "改 MCP 令牌的 scope 或状态（降级无需重新签发）",
			Dangerous: true, ConfirmReason: "scope 决定该令牌能否执行全部后台接口；提升权限等于发放管理员凭据",
			Params: []adminField{pathParam("id", "MCP 令牌数字 id")},
			Body: []adminField{
				enumField(bodyOptional("scope", "string", "新的权限级别"), "query", "admin_read", "admin"),
				enumField(bodyOptional("status", "string", "新的状态"), "active", "revoked"),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/mcp-tokens/{id}", Handler: s.handleAdminRevokeMCPToken,
			Name: "admin_revoke_mcp_token", Group: groupMCP, Role: roleAdmin,
			Summary:   "吊销一个 MCP 令牌（使用它的客户端立即 401）",
			Dangerous: true, ConfirmReason: "吊销不可撤销，正在使用该令牌的客户端会立即失去访问",
			Params: []adminField{pathParam("id", "MCP 令牌数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/hooks", Handler: s.handleAdminListHooks,
			Name: "admin_list_hooks", Group: groupHooks, Role: roleViewer,
			Summary: "列出事件投递目标（webhook / jsonl）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/hooks", Handler: s.handleAdminUpsertHook,
			Name: "admin_upsert_hook", Group: groupHooks, Role: roleAdmin,
			Summary: "新建或更新一个事件投递目标（按名字 upsert）",
			Body: []adminField{
				bodyRequired("name", "string", "hook 名称"),
				enumField(bodyOptional("type", "string", "投递类型"), "webhook", "jsonl"),
				bodyOptional("url", "string", "webhook 地址（必须是 https，除非配置放开）"),
				bodyOptional("secret", "string", "签名密钥（只写，不回显）"),
				structuredField("events", "array", "订阅的事件名，支持通配（如 request.*、ledger.*）；空数组表示不订阅任何事件", arrayOfStrings("事件名列表"), []any{"request.completed"}),
				bodyOptional("include_content", "boolean", "是否附带请求/响应内容"),
				nonNegative(bodyOptional("max_bytes", "integer", "单条投递的正文上限（字节）")),
				numericField(bodyOptional("sample_rate", "number", "采样率，取值区间 [0,1]；1 表示全量"), 0, 1),
				bodyOptional("enabled", "boolean", "是否启用"),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/hooks/{id}", Handler: s.handleAdminDeleteHook,
			Name: "admin_delete_hook", Group: groupHooks, Role: roleAdmin,
			Summary:   "删除一个事件投递目标",
			Dangerous: true, ConfirmReason: "删除后该目标不再收到任何事件（不可撤销）",
			Params: []adminField{pathParam("id", "hook 数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/settings", Handler: s.handleAdminGetSettings,
			Name: "admin_get_settings", Group: groupSettings, Role: roleViewer,
			Summary: "按 key 读取运行期设置（key 可重复出现以一次读多个）",
			Query:   []adminField{{Name: "key", Type: "string", Desc: "设置项 key，可重复；至少一个", Required: true}},
		},
		{
			Method: "PUT", Path: "/admin/api/v1/settings/{key}", Handler: s.handleAdminPutSetting,
			Name: "admin_put_setting", Group: groupSettings, Role: roleAdmin,
			Summary:   "写入一个运行期设置（值为任意合法 JSON）",
			Dangerous: true, ConfirmReason: "会改动正在运行的网关配置",
			Params:  []adminField{pathParam("key", "设置项 key")},
			RawBody: freeFormSchema("任意合法 JSON 值；也接受带 value 字段的包装对象，例如 true 或对象形式的配置"),
		},
	}
}

// maxInflightDesc documents providers.max_inflight on the create and update bodies. It is
// one constant because the two endpoints must not drift: a model reads one of them and
// writes the value, and the *behaviour* (queueing, then 429 provider_busy) is what makes
// the field usable rather than merely writable.
const maxInflightDesc = "供应商最大并发：该供应商**同时在途的上游调用数**，0 = 不限（默认）。" +
	"正整数时超出并发的新请求会**排队等待**名额（FIFO），等待上限由部署配置 routing.provider_queue_wait_s 决定" +
	"（默认 30 秒，0 = 不排队、超限直接失败）；等待超时或队列已满时该次尝试按可重试失败换下一个候选，" +
	"全部候选耗尽返回 HTTP 429 provider_busy（带 Retry-After）。探测/重启等后台动作不占名额。" +
	"写完后 admin_get_provider / admin_list_providers 的 capacity 字段给出实时 limit/inflight/waiting（读回只需 admin_read）"

// providerAdminRoutes covers provider instances, their upstream models and the
// out-of-band operations (probe, restart, actions, logs).
func (s *Server) providerAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/providers", Handler: s.handleAdminListProviders,
			Name: "admin_list_providers", Group: groupProviders, Role: roleViewer,
			Summary: "列出供应商（状态、优先级、权重、健康、最近错误、实时在途与排队 capacity）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "GET", Path: "/admin/api/v1/provider-kinds", Handler: s.handleAdminListProviderKinds,
			Name: "admin_list_provider_kinds", Group: groupProviders, Role: roleViewer,
			Summary: "列出内建供应商类型及其配置/凭据字段说明（新建供应商前先看这个）",
		},
		{
			Method: "POST", Path: "/admin/api/v1/providers", Handler: s.handleAdminCreateProvider,
			Name: "admin_create_provider", Group: groupProviders, Role: roleAdmin,
			Summary:   "新建或用配置覆盖一个供应商（按 name upsert）",
			Dangerous: true, ConfirmReason: "可写入供应商凭据与出网配置，并会重启该插件进程",
			RawBody: objectSchema(map[string]any{
				"name":              prop("string", "供应商名（唯一，upsert 依据）"),
				"kind":              prop("string", "类型：testecho / openai-chat / openai-responses / plugin:<name>"),
				"display_name":      prop("string", "展示名"),
				"state_dir":         prop("string", "插件状态目录"),
				"config":            providerConfigSchema(),
				"credentials":       providerCredentialsSchema(),
				"meta":              providerMetaSchema(),
				"timeout_overrides": providerTimeoutSchema(),
				"enabled":           prop("boolean", "是否启用"),
				"draining":          prop("boolean", "是否排空（不再接新流量）"),
				"priority":          prop("integer", "优先级"),
				"weight":            prop("integer", "同层权重"),
				"max_inflight":      prop("integer", maxInflightDesc),
				"degradation":       prop("string", "能力缺失时的降级策略：none/strip/fail_fast/best_effort"),
				"reset_cooldown":    prop("boolean", "写完后是否清掉冷却"),
			}, "name"),
		},
		{
			Method: "GET", Path: "/admin/api/v1/providers/{id}", Handler: s.handleAdminGetProvider,
			Name: "admin_get_provider", Group: groupProviders, Role: roleViewer,
			Summary: "单个供应商详情（配置、字段说明、已配置的凭据字段名、健康与最近错误、实时并发 capacity）",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/providers/{id}", Handler: s.handleAdminPatchProvider,
			Name: "admin_update_provider", Group: groupProviders, Role: roleAdmin,
			Summary:   "更新供应商（字段同 admin_create_provider，凭据省略即保持不变）",
			Dangerous: true, ConfirmReason: "可覆盖供应商凭据与配置，并会重启该插件进程",
			Params: []adminField{pathParam("id", "供应商数字 id")},
			RawBody: objectSchema(map[string]any{
				"name":           prop("string", "改名（一般不用）"),
				"kind":           prop("string", "类型"),
				"display_name":   prop("string", "展示名"),
				"state_dir":      prop("string", "插件状态目录"),
				"config":         providerConfigSchema(),
				"credentials":    providerCredentialsSchema(),
				"enabled":        prop("boolean", "是否启用"),
				"draining":       prop("boolean", "是否排空"),
				"priority":       prop("integer", "优先级"),
				"weight":         prop("integer", "同层权重"),
				"max_inflight":   prop("integer", maxInflightDesc),
				"degradation":    prop("string", "降级策略：none/strip/fail_fast/best_effort"),
				"reset_cooldown": prop("boolean", "写完后是否清掉冷却"),
			}),
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/providers/{id}", Handler: s.handleAdminDeleteProvider,
			Name: "admin_delete_provider", Group: groupProviders, Role: roleAdmin,
			Summary:   "删除供应商（被路由引用时返回 409，force=true 级联删除）",
			Dangerous: true, ConfirmReason: "删除会级联移除该供应商的模型与路由（不可撤销）",
			Params: []adminField{pathParam("id", "供应商数字 id")},
			Query:  []adminField{queryParam("force", "boolean", "true 表示连同引用它的模型与路由一起删除")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/providers/{id}/test", Handler: s.handleAdminProbeProvider,
			Name: "admin_test_provider", Group: groupProviders, Role: roleAdmin,
			Summary: "真实探测供应商是否可用（结果落库）",
			Notes:   "会向真实上游发起一次流式补全，可能耗时数十秒",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
			Query:   []adminField{enumField(queryParam("mode", "string", "探测模式"), "auto", "plugin", "builtin")},
			Body:    []adminField{enumField(bodyOptional("mode", "string", "探测模式（与 query 二选一）"), "auto", "plugin", "builtin")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/providers/{id}/restart", Handler: s.handleAdminRestartProvider,
			Name: "admin_restart_provider", Group: groupProviders, Role: roleAdmin,
			Summary: "重启供应商插件进程",
			Notes:   "会中断该供应商正在进行的请求（新请求会重新拉起进程）",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/providers/{id}/logs", Handler: s.handleAdminProviderLogs,
			Name: "admin_provider_logs", Group: groupProviders, Role: roleViewer,
			Summary: "读取插件进程的 stderr 环形缓冲（排障用）",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
			Query:   []adminField{queryParam("limit", "integer", "返回行数，默认 200，最大 2000")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/providers/{id}/actions", Handler: s.handleAdminProviderActions,
			Name: "admin_provider_actions", Group: groupProviders, Role: roleViewer,
			Summary: "列出该插件声明的动作（如 whoami / refresh_session）",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/providers/{id}/actions/{name}", Handler: s.handleAdminRunProviderAction,
			Name: "admin_run_provider_action", Group: groupProviders, Role: roleAdmin,
			Summary: "执行一个插件动作并返回它的 JSON 结果",
			Params: []adminField{
				pathParam("id", "供应商数字 id"),
				pathParam("name", "动作名（见 admin_provider_actions）"),
			},
			Query: []adminField{queryParam("params", "string", "动作入参的 JSON 字符串")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/providers/{id}/models", Handler: s.handleAdminListProviderModels,
			Name: "admin_list_provider_models", Group: groupProviders, Role: roleViewer,
			Summary: "列出某个供应商声明的上游模型",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
			Query:   pageConfig.fields(),
		},
		{
			Method: "GET", Path: "/admin/api/v1/provider-models", Handler: s.handleAdminListProviderModels,
			Name: "admin_list_all_provider_models", Group: groupProviders, Role: roleViewer,
			Summary: "列出全部供应商模型（跨供应商视图）",
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/providers/{id}/models", Handler: s.handleAdminUpsertProviderModel,
			Name: "admin_upsert_provider_model", Group: groupProviders, Role: roleAdmin,
			Summary: "新增或更新一个上游模型（能力、档位、成本规则）",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
			Body: []adminField{
				bodyRequired("public_model", "string", "上游模型名"),
				bodyOptional("upstream_model", "string", "实际发给上游的模型名，省略表示同名"),
				bodyOptional("enabled", "boolean", "是否启用"),
				bodyOptional("priority", "integer", "优先级"),
				bodyOptional("weight", "integer", "同层权重"),
				nonNegative(bodyOptional("context_window", "integer", "上下文窗口（token，0 = 未知）")),
				nonNegative(bodyOptional("max_output_tokens", "integer", "最大输出（token，0 = 未知）")),
				exampleField(schemaField(bodyOptional("capabilities", "object",
					"能力声明：这个上游模型支持什么（流式、工具、思考、用量维度……），字段说明见本参数 schema。省略的键按未知处理"),
					capabilitiesSchema()),
					map[string]any{"complete": true, "stream": true, "usage_dimensions": true}),
				capabilityOverrideField(),
				// The cost rule set: the field whose missing shape made an operator's request fail.
				pricingRuleSchemaFields(s.ledgerCurrency())[0],
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/providers/{id}/models/refresh", Handler: s.handleAdminRefreshProviderModels,
			Name: "admin_refresh_provider_models", Group: groupProviders, Role: roleAdmin,
			Summary: "从上游发现模型并补齐本地的空值",
			Notes:   "会调用上游模型目录接口，可能耗时",
			Params:  []adminField{pathParam("id", "供应商数字 id")},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/provider-models/{id}", Handler: s.handleAdminDeleteProviderModel,
			Name: "admin_delete_provider_model", Group: groupProviders, Role: roleAdmin,
			Summary:   "删除一个上游模型",
			Dangerous: true, ConfirmReason: "删除后引用它的路由会失去上游目标（不可撤销）",
			Params: []adminField{pathParam("id", "供应商模型数字 id")},
		},
	}
}

// billingAdminRoutes covers invariants, billing status, the ledger rebuild and the
// per-account balance/ledger views.
func (s *Server) billingAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/billing/currency", Handler: s.handleAdminGetBillingCurrency,
			Name: "admin_billing_currency", Group: groupBilling, Role: roleViewer,
			Summary: "账本币种、默认展示币种、可用汇率表与缺汇率的币种（控制台显示币种选择器用）",
		},
		{
			Method: "GET", Path: "/admin/api/v1/billing/invariants", Handler: s.handleAdminInvariants,
			Name: "admin_billing_invariants", Group: groupBilling, Role: roleViewer,
			Summary: "巡检计费不变量（余额一致、账本可复算、用量与账本对齐等）",
		},
		{
			Method: "GET", Path: "/admin/api/v1/billing/status", Handler: s.handleAdminBillingStatus,
			Name: "admin_billing_status", Group: groupBilling, Role: roleViewer,
			Summary: "计费运行状态（写入器队列、兜底文件、失败重放积压）",
		},
		{
			Method: "POST", Path: "/admin/api/v1/billing/rebuild-ledger", Handler: s.handleAdminRebuildLedger,
			Name: "admin_rebuild_ledger", Group: groupBilling, Role: roleAdmin,
			Summary:   "按用量重算账本与余额（apply=false 为 dry-run）",
			Dangerous: true, ConfirmReason: "apply=true 会在单个事务里重写账本分录并重算余额",
			Notes: "先跑 apply=false 看差异，确认后再 apply=true",
			Body: []adminField{
				bodyOptional("account_id", "integer", "只重建某个账户（与 account 二选一；都省略为全量）"),
				bodyOptional("account", "string", "只重建某个账户名"),
				bodyOptional("apply", "boolean", "true 才真正写库，默认 false"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/accounts/{id}/dsh", Handler: s.handleAdminGetAccountDSH,
			Name: "admin_get_account_dsh", Group: groupAccounts, Role: roleViewer,
			Summary: "查看账户的 dsh 多租户网关启用状态（M52）",
			Params:  []adminField{pathParam("id", "账户数字 id")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/accounts/{id}/dsh", Handler: s.handleAdminSetAccountDSH,
			Name: "admin_set_account_dsh", Group: groupAccounts, Role: roleAdmin,
			Summary:   "启用或停用该账户的 dsh 网关入口（M52）",
			Dangerous: true, ConfirmReason: "停用后 dsh 门户拒绝该账号新登录，既有会话按 dshgw 的 dsh_enforce 档位失效；启用恢复登录。不影响账户的 Key、余额或 dsh 租户数据",
			Params: []adminField{pathParam("id", "账户数字 id")},
			Body: []adminField{
				bodyRequired("enabled", "boolean", "true=启用 dsh 入口（自动铸造 worker 专用 Key 并建立/启动租户）；false=停用（停止 worker 并吊销 worker Key，数据保留）"),
				bodyOptional("tenant", "string",
					"启用时可选指定 dshgw 租户名（须匹配 [a-z][a-z0-9-]{0,25}[a-z]）；留空则沿用既有映射或按账户名自动生成。停用时忽略"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/accounts/{id}/balance", Handler: s.handleAdminAccountBalance,
			Name: "admin_account_balance", Group: groupBilling, Role: roleViewer,
			Summary: "账户余额与信用视图",
			Params:  []adminField{pathParam("id", "账户数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/accounts/{id}/ledger", Handler: s.handleAdminAccountLedger,
			Name: "admin_account_ledger", Group: groupBilling, Role: roleViewer,
			Summary: "账户账本流水（充值、消费、调整、退款、过期）",
			Params:  []adminField{pathParam("id", "账户数字 id")},
			Query: append(pageLedger.fields(),
				queryParam("days", "integer", "回溯天数，默认 7，最大 365")),
		},
	}
}

// invoiceAdminRoutes covers invoices, credits, redemption codes and reconciliation.
func (s *Server) invoiceAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/invoices", Handler: s.handleAdminListInvoices,
			Name: "admin_list_invoices", Group: groupBilling, Role: roleViewer,
			Summary: "列出账单（可按账户过滤）",
			Query: append(pageInvoices.fields(),
				queryParam("account_id", "integer", "只看某个账户")),
		},
		{
			Method: "GET", Path: "/admin/api/v1/accounts/{id}/invoices", Handler: s.handleAdminListInvoices,
			Name: "admin_list_account_invoices", Group: groupBilling, Role: roleViewer,
			Summary: "列出某个账户的账单",
			Params:  []adminField{pathParam("id", "账户数字 id")},
			Query:   pageInvoices.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/accounts/{id}/invoices", Handler: s.handleAdminBuildInvoice,
			Name: "admin_build_invoice", Group: groupBilling, Role: roleAdmin,
			Summary: "为一个账期生成（或重算）草稿账单",
			Params:  []adminField{pathParam("id", "账户数字 id")},
			Body: []adminField{
				bodyOptional("period", "string", "账期：current / previous / last30，或自然月如 2026-08"),
				bodyOptional("group_by", "string", "明细分组维度，默认 model"),
				bodyOptional("force", "boolean", "草稿已存在时是否重算"),
				bodyOptional("note", "string", "备注"),
				bodyOptional("period_start", "string", "自定义账期起点（RFC3339，与 period 二选一）"),
				bodyOptional("period_end", "string", "自定义账期终点（RFC3339）"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/invoices/{id}", Handler: s.handleAdminGetInvoice,
			Name: "admin_get_invoice", Group: groupBilling, Role: roleViewer,
			Summary: "账单详情与明细（query format=csv 时返回 CSV 文本）",
			Params:  []adminField{pathParam("id", "账单数字 id")},
			Query:   []adminField{enumField(queryParam("format", "string", "返回格式，csv 时是文本表格"), "json", "csv")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/invoices/{id}/{action}", Handler: s.handleAdminInvoiceAction,
			Name: "admin_invoice_action", Group: groupBilling, Role: roleAdmin,
			Summary:   "流转账单状态：issue 签发 / void 作废 / pay 标记已付",
			Dangerous: true, ConfirmReason: "pay 会写账本（后付还款），void 会让账单永久失效",
			Params: []adminField{
				pathParam("id", "账单数字 id"),
				enumField(pathParam("action", "动作"), "issue", "void", "pay"),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/accounts/{id}/credits", Handler: s.handleAdminAccountCredits,
			Name: "admin_grant_credits", Group: groupBilling, Role: roleAdmin,
			Summary:   "给账户充值/赠送/调整/退款（幂等键 kind:ref_id）",
			Dangerous: true, ConfirmReason: "直接往账户余额里加钱或扣钱，会写账本并可能自动恢复账户",
			Params: []adminField{pathParam("id", "账户数字 id")},
			Body: []adminField{
				enumField(bodyRequired("kind", "string", "充值类型"), "topup", "credit_grant", "adjustment", "refund"),
				bodyOptional("amount_micros", "integer", "金额（微美元整数，正数；与 amount_usd 二选一）"),
				bodyOptional("amount_usd", "string", "金额（美元字符串，例如 10.00）"),
				bodyOptional("ref_id", "string", "外部单据号，用于幂等"),
				bodyOptional("note", "string", "备注"),
				bodyOptional("expires_at", "string", "赠送额度的到期时间（RFC3339）"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/accounts/{id}/credits", Handler: s.handleAdminAccountCreditList,
			Name: "admin_list_credits", Group: groupBilling, Role: roleViewer,
			Summary: "账户的充值/赠送记录",
			Params:  []adminField{pathParam("id", "账户数字 id")},
			Query: append(pageLedger.fields(),
				queryParam("days", "integer", "回溯天数，默认 7，最大 365")),
		},
		{
			Method: "POST", Path: "/admin/api/v1/redemption-codes", Handler: s.handleAdminGenerateCodes,
			Name: "admin_generate_redemption_codes", Group: groupBilling, Role: roleAdmin,
			Summary:   "批量生成兑换码（明文只在响应里出现一次）",
			Dangerous: true, ConfirmReason: "生成的每个码都可以被兑换成余额，明文只返回一次",
			Body: []adminField{
				intRange(bodyOptional("count", "integer", "生成数量"), 1, 1000),
				intRange(bodyOptional("amount_micros", "integer", "每张面额（微美元）"), 0, maxSaneInteger),
				bodyOptional("amount_usd", "string", "每张面额（美元字符串）"),
				bodyOptional("expires_at", "string", "过期时间（RFC3339）"),
				bodyOptional("batch_id", "string", "批次号"),
				bodyOptional("note", "string", "备注"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/redemption-codes", Handler: s.handleAdminListCodes,
			Name: "admin_list_redemption_codes", Group: groupBilling, Role: roleViewer,
			Summary: "列出兑换码（不返回明文）",
			Query: append(pageCodes.fields(),
				queryParam("batch_id", "string", "只看某个批次")),
		},
		{
			Method: "POST", Path: "/admin/api/v1/redemption-codes/redeem", Handler: s.handleAdminRedeemCode,
			Name: "admin_redeem_code", Group: groupBilling, Role: roleAdmin,
			Summary:   "为账户核销一张兑换码（并发下只成功一次）",
			Dangerous: true, ConfirmReason: "会把兑换码的面额充进账户余额，码随即失效",
			Body: []adminField{
				bodyRequired("code", "string", "兑换码明文"),
				bodyOptional("account_id", "integer", "目标账户 id（与 account 二选一）"),
				bodyOptional("account", "string", "目标账户名"),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/billing/reconcile", Handler: s.handleAdminReconcile,
			Name: "admin_reconcile_billing", Group: groupBilling, Role: roleAdmin,
			Summary: "对账：用量汇总对比账本汇总，给出差异样例与不变量结果",
			Body: []adminField{
				intRange(bodyOptional("days", "integer", "回溯天数（1–365）"), 1, 365),
				bodyOptional("account_id", "integer", "只对某个账户"),
				bodyOptional("from", "string", "起点（RFC3339）"),
				bodyOptional("to", "string", "终点（RFC3339）"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/billing/reconciliations", Handler: s.handleAdminReconciliations,
			Name: "admin_list_reconciliations", Group: groupBilling, Role: roleViewer,
			Summary: "历史对账记录",
			Query:   pageReconciliations.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/billing/failures/replay", Handler: s.handleAdminReplayFailures,
			Name: "admin_replay_billing_failures", Group: groupBilling, Role: roleAdmin,
			Summary:   "重放落在兜底文件/表里的结算失败记录",
			Dangerous: true, ConfirmReason: "会把之前失败的用量重新结算进账本（幂等，但会改动余额）",
		},
		{
			Method: "POST", Path: "/admin/api/v1/billing/expire-credit", Handler: s.handleAdminExpireCredit,
			Name: "admin_expire_credit", Group: groupBilling, Role: roleAdmin,
			Summary:   "手动执行赠送额度到期冲销",
			Dangerous: true, ConfirmReason: "会把到期未用的赠送额度从余额里冲销掉",
		},
	}
}

// backupAdminRoutes covers the backup lifecycle.
func (s *Server) backupAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/backups", Handler: s.handleAdminListBackups,
			Name: "admin_list_backups", Group: groupBackups, Role: roleViewer,
			Summary: "列出备份作业（时间、大小、校验结果、路径）",
			Query:   pageBackups.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/backups", Handler: s.handleAdminRunBackup,
			Name: "admin_run_backup", Group: groupBackups, Role: roleAdmin,
			Summary: "立即执行一次一致点备份",
			Notes:   "VACUUM INTO 需要复制整个库文件，库大时耗时较长",
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/backups/{id}", Handler: s.handleAdminDeleteBackup,
			Name: "admin_delete_backup", Group: groupBackups, Role: roleAdmin,
			Summary:   "删除一个备份文件",
			Dangerous: true, ConfirmReason: "删除后无法再从这个备份恢复（不可撤销）",
			Params: []adminField{pathParam("id", "备份作业数字 id")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/backups/{id}/download", Handler: s.handleAdminDownloadBackup,
			Name: "admin_download_backup", Group: groupBackups, Role: roleAdmin,
			Summary: "下载备份文件（二进制）",
			NoTool:  "返回的是整个数据库的二进制文件，MCP 文本通道无法承载；请在控制台或直接用 curl 下载",
			Params:  []adminField{pathParam("id", "备份作业数字 id")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/backups/{id}/restore", Handler: s.handleAdminRestoreBackup,
			Name: "admin_restore_backup", Group: groupBackups, Role: roleAdmin,
			Summary:   "从备份恢复数据库（需 confirm=true 且 body.confirm=true）",
			Dangerous: true, ConfirmReason: "会用备份覆盖当前数据库，期间网关不可用，之后写入的数据会丢失",
			Params: []adminField{pathParam("id", "备份作业数字 id")},
			Body:   []adminField{bodyRequired("confirm", "boolean", "必须为 true，表示确认覆盖当前数据库")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/backups/prune", Handler: s.handleAdminPruneBackups,
			Name: "admin_prune_backups", Group: groupBackups, Role: roleAdmin,
			Summary:   "按保留策略清理过期备份",
			Dangerous: true, ConfirmReason: "会永久删除超出保留策略的备份文件",
		},
	}
}

// portalAdminRoutes covers customer self-service logins.
func (s *Server) portalAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/portal-users", Handler: s.handleAdminListPortalUsers,
			Name: "admin_list_portal_users", Group: groupPortal, Role: roleViewer,
			Summary: "列出门户用户（可按账户过滤）",
			Query: append(pageConfig.fields(),
				queryParam("account_id", "integer", "只看某个账户")),
		},
		{
			Method: "GET", Path: "/admin/api/v1/accounts/{id}/portal-users", Handler: s.handleAdminListPortalUsers,
			Name: "admin_list_account_portal_users", Group: groupPortal, Role: roleViewer,
			Summary: "列出某个账户的门户用户",
			Params:  []adminField{pathParam("id", "账户数字 id")},
			Query:   pageConfig.fields(),
		},
		{
			Method: "POST", Path: "/admin/api/v1/accounts/{id}/portal-users", Handler: s.handleAdminCreatePortalUser,
			Name: "admin_create_portal_user", Group: groupPortal, Role: roleAdmin,
			Summary:   "为一个账户创建门户登录（一次性口令只返回一次）",
			Dangerous: true, ConfirmReason: "会签发一个可以自助查询与兑换的登录凭据",
			Params: []adminField{pathParam("id", "账户数字 id")},
			Body:   []adminField{bodyRequired("username", "string", "门户用户名（全局唯一）")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/portal-users/{id}/password", Handler: s.handleAdminResetPortalPassword,
			Name: "admin_reset_portal_password", Group: groupPortal, Role: roleAdmin,
			Summary:   "重置门户用户口令（新口令只返回一次）",
			Dangerous: true, ConfirmReason: "旧口令立即失效，新口令只显示这一次",
			Params: []adminField{pathParam("id", "门户用户数字 id")},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/portal-users/{id}", Handler: s.handleAdminDisablePortalUser,
			Name: "admin_disable_portal_user", Group: groupPortal, Role: roleAdmin,
			Summary:   "停用一个门户用户",
			Dangerous: true, ConfirmReason: "停用后该用户立即无法登录门户",
			Params: []adminField{pathParam("id", "门户用户数字 id")},
		},
	}
}

// pricingAdminRoutes covers the pricing simulator, the rule validator and the
// markup controls.
func (s *Server) pricingAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "POST", Path: "/admin/api/v1/pricing/simulate", Handler: s.handleAdminSimulatePricing,
			Name: "admin_simulate_pricing", Group: groupPricing, Role: roleViewer,
			Summary: "用数据面同一条纯函数试算一次计费（可内联规则做改了会怎样的预览）",
			Body: []adminField{
				bodyRequired("model", "string", "对客模型名"),
				bodyOptional("at", "string", "计费时刻（RFC3339），省略为现在"),
				bodyOptional("variant", "string", "计价变体"),
				exampleField(schemaField(bodyOptional("dimensions", "object",
					"本次试算的用量：键是计量维度名（input/output/input_cache_hit/input_cache_miss/reasoning），"+
						"不是 input_tokens。数值为整数 token 数；省略的维度按 0 计"),
					simulateDimensionsSchema()), simulateDimensionsExample()),
				nonNegative(bodyOptional("markup_bp", "integer", "临时加价倍数（bp，10000 = 1.0 倍）")),
				nonNegative(bodyOptional("min_charge_micros", "integer", "最低收费（微美元）")),
				exampleField(schemaField(bodyOptional("cost_rules", "object",
					"内联成本规则集（只用于本次试算，不写库）：覆盖该模型路由到的真实成本规则，形状与 pricing_rules 相同"),
					pricingRuleSetSchema()), pricingRuleSetExample(s.ledgerCurrency())),
				exampleField(schemaField(bodyOptional("sale_rules", "object",
					"内联售价规则集（只用于本次试算，不写库），形状与 sale_pricing 相同"),
					pricingRuleSetSchema()), salePricingExample(s.ledgerCurrency())),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/pricing/validate", Handler: s.handleAdminValidatePricing,
			Name: "admin_validate_pricing", Group: groupPricing, Role: roleViewer,
			Summary: "校验一份价格规则集是否合法，并报告被遮蔽的规则（不写库；viewer scope 即可调用）",
			Notes: "请求体就是规则集文档本身（不是 {\"rules\": …} 之外的包装），形状见 admin_describe 的 body_schema。" +
				"改价前的推荐做法：先 validate，再用 admin_upsert_provider_model / admin_update_model 落库",
			RawBody: pricingRuleSetJSONSchema(s.ledgerCurrency()),
		},
		{
			Method: "GET", Path: "/admin/api/v1/pricing/targets", Handler: s.handleAdminPricingTargets,
			Name: "admin_pricing_targets", Group: groupPricing, Role: roleViewer,
			Summary: "一次拉全定价目标：每个模型的成本规则、售价、生效来源与缺失项",
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/pricing/markup", Handler: s.handleAdminPatchMarkup,
			Name: "admin_update_markup", Group: groupPricing, Role: roleAdmin,
			Summary: "只改模型的加价倍数（保留规则数组与其它字段）",
			Notes:   "改动立刻影响该模型的对客售价",
			Body: []adminField{
				bodyRequired("model", "string", "对客模型名"),
				enumField(bodyOptional("basis", "string", "计价基准"), "cost_follow", "absolute"),
				intRange(bodyRequired("markup_bp", "integer", "加价倍数（bp，10000 表示 1.0 倍，最大 1000000）"), 0, 1000000),
				exampleField(schemaField(bodyOptional("dimension_markup_bp", "object",
					"按维度覆写的加价倍数（bp，10000 = 1.0 倍），键是计量维度名；省略的维度用 markup_bp"),
					dimensionMarkupSchema()), dimensionMarkupExample()),
			},
		},
	}
}

// adminEndpointIndex answers "which endpoint is this?" for the MCP bridge.
type adminEndpointIndex struct {
	routes []adminRoute
	byName map[string]adminRoute
}

func newAdminEndpointIndex(routes []adminRoute) *adminEndpointIndex {
	index := &adminEndpointIndex{routes: routes, byName: make(map[string]adminRoute, len(routes))}
	for _, route := range routes {
		index.byName[route.Name] = route
	}
	return index
}

func (i *adminEndpointIndex) lookup(name string) (adminRoute, bool) {
	route, ok := i.byName[name]
	return route, ok
}

// names lists every entry name in table order (used by diagnostics and tests).
func (i *adminEndpointIndex) names() []string {
	out := make([]string, 0, len(i.routes))
	for _, route := range i.routes {
		out = append(out, route.Name)
	}
	return out
}
