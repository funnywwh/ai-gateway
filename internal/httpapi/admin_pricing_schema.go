package httpapi

import "encoding/json"

// This file is the shape of the pricing documents an agent can write.
//
// It exists because "pricing_rules" used to reach an MCP client as {"type":"object"} with an
// example of {}: the model could see that a cost rule set was wanted, could not see a single
// field name, unit or dimension, and — correctly — refused to write one. A refused write is
// the honest outcome, but it is still a failed task, so the shape now travels with the field.
//
// It lives in the transport layer rather than next to pricing.RuleSet because of the layering
// rules (internal/arch: mcpsrv may not import pricing) and because this is metadata about what
// the *endpoints* accept, which is exactly what a route entry documents. The two are kept
// honest by tests rather than by proximity: the example below is fed to admin_validate_pricing
// and then really written through admin_upsert_provider_model / admin_upsert_model, so a shape
// that drifts from pricing.ParseRuleSet fails `make test` instead of misleading a model.
//
// Sources of truth for the field list: internal/pricing/rule.go (RuleSet, Rule, When, Tier,
// TimeWindow) and docs/pricing.md §1-§2. Fields that appear in the prose of docs/pricing.md
// but not in the struct — when.monthly_usage, when.region — are deliberately absent: the
// parser rejects unknown fields, so advertising them would produce documents that cannot be
// written.

// pricingDimensions are the usage dimensions a rule's rates may be keyed by. The engine
// prices any dimension name matching ^[a-z][a-z0-9_]{0,31}$, so these five are the ones the
// gateway actually meters today; the console's own rate editor offers the same five
// (internal/webui/static/js/pages/pricing.js).
var pricingDimensions = []string{"input", "input_cache_hit", "input_cache_miss", "output", "reasoning"}

// pricingRateUnit is the sentence every rate description needs: a rate without its unit is
// the single most expensive omission possible here, because micros look like plausible money.
const pricingRateUnit = "微单位/百万 token（整数，禁止浮点）：200000 = 0.2 个规则集币种/百万 token"

// pricingRuleSetSchema describes one rule set document: the cost side (pricing_rules) and
// the sale side (sale_pricing) are the same shape, as pricing.RuleSet is one struct.
func pricingRuleSetSchema() map[string]any {
	rates := map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "各计量维度的单价，" + pricingRateUnit +
			"。键就是维度名（见 properties 说明）；规则没有某维度费率时按引擎兜底计价：" +
			"input 借用 input_cache_miss、reasoning 借用 output；显式写 0 表示真的免费。" +
			"不需要的维度可以不写。",
		"propertyNames": map[string]any{"pattern": "^[a-z][a-z0-9_]{0,31}$"},
		"properties":    map[string]any{},
	}
	properties, _ := rates["properties"].(map[string]any)
	for _, dimension := range pricingDimensions {
		properties[dimension] = map[string]any{"type": "integer", "minimum": 0, "description": pricingDimensionDesc(dimension)}
	}

	tier := map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "按用量档位限定这条规则。basis=input 在准入阶段即可精确判定；basis=total 或 output 时准入按最贵档预留、结算按实际校正。" +
			"边界为半开区间 [gte, lt)，lt 省略或为 0 表示无上限。",
		"properties": map[string]any{
			"basis": map[string]any{"type": "string", "enum": []string{"input", "output", "total"},
				"description": "档位依据的用量口径"},
			"gte": map[string]any{"type": "integer", "minimum": 0, "description": "下界（含）"},
			"lt":  map[string]any{"type": "integer", "minimum": 0, "description": "上界（不含）；省略或 0 = 无上限"},
		},
		"required": []string{"basis", "gte"},
	}

	timeWindow := map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "一个生效时段。跨午夜用 end < start 表示（例如 16:30→00:30），命中 [start,24:00) ∪ [00:00,end)；星期按窗口起点所在日归属；边界半开。" +
			"所有时段都不匹配时这条规则不生效。",
		"properties": map[string]any{
			"days": map[string]any{"type": "array", "items": map[string]any{"type": "string",
				"enum": []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}},
				"description": "生效的星期；省略表示每天"},
			"start": map[string]any{"type": "string", "description": "开始时刻 HH:MM（零填充，24 小时制）"},
			"end":   map[string]any{"type": "string", "description": "结束时刻 HH:MM；end < start 表示跨午夜"},
			"tz":    map[string]any{"type": "string", "description": "时区：UTC、Local（=服务器本地时区）或固定偏移 +08:00（二进制内嵌 tzdata，不依赖主机 zoneinfo）"},
		},
		"required": []string{"start", "end"},
	}

	when := map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "这条规则的匹配条件。**空对象 {} 表示 catch-all**：每个规则集必须恰好有一条 catch-all（否则 400），" +
			"保证任何请求都有价。求值按 order 升序、首条全部条件满足的规则生效。",
		"properties": map[string]any{
			"time_windows": map[string]any{"type": "array", "items": timeWindow,
				"description": "生效时段；命中其中任一时段即可"},
			"valid_from": map[string]any{"type": "string", "description": "生效起点（RFC3339，例如 2026-01-01T00:00:00Z）"},
			"valid_to":   map[string]any{"type": "string", "description": "生效终点（RFC3339，不含）；省略表示长期有效"},
			"tier":       tier,
			"model_variant": map[string]any{"type": "string",
				"description": "限定上游模型变体（一般不需要）"},
		},
	}

	rule := map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "一条价格规则。规则集按 order 升序求值，第一条 when 全部满足的规则生效（首命中）。",
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "description": "规则标识（可省略；同一规则集内不能重复）。会写进计费快照，便于事后复算对账"},
			"title": map[string]any{"type": "string", "description": "可读标题（可省略），例如「标准时段」"},
			"order": map[string]any{"type": "integer",
				"description": "求值顺序，升序、**同一规则集内必须唯一**（重复即 400）。建议留间隔（10/20/30）便于插入"},
			"when":  when,
			"rates": rates,
			"per_request_fee_micros": map[string]any{"type": "integer", "minimum": 0,
				"description": "按次固定费（微单位）。是否每个 attempt 都收由 billing.per_request_fee_scope 决定（默认 attempt）"},
		},
		"required": []string{"order", "when"},
	}

	return map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "价格规则集（与 pricing.RuleSet 同形，成本侧与售价侧共用）。" +
			"**单位：rates 里每个数字都是微单位/百万 token 的整数**（200000 = 0.2 个规则集币种/百万 token，禁止浮点）。" +
			"字段名与结构必须完全按本 schema 写：服务端用严格解析，**未知字段会直接 400**。" +
			"拿不准时先用 admin_validate_pricing 校验（正文就是规则集本身，不写库、viewer 权限即可）。" +
			"成本侧（provider_models.pricing_rules_json）只认 currency 与 rules；" +
			"basis/markup_bp/dimension_markup_bp 只对售价侧（models.sale_pricing_json）有意义。",
		"properties": map[string]any{
			"currency": map[string]any{"type": "string", "pattern": "^[A-Z]{3}$",
				"description": "本规则集所有单价的计价币种（3 位大写码，如 USD/CNY）。省略 = 账本币种。" +
					"该币种必须已在 billing.fx_rates 里（账本币种本身除外），否则写入 400。" +
					"成本侧写上游真实报价币种，售价侧写对客报价币种；两者不同时先换算成本单价再乘倍率"},
			"basis": map[string]any{"type": "string", "enum": []string{"cost_follow", "absolute"},
				"description": "**仅售价侧**：cost_follow = 售价 = 该次成本 × markup_bp（上游调价时售价自动跟随）；" +
					"absolute = 用本规则集的绝对单价（不再乘倍率），此时必须至少有一条规则。空值按 cost_follow 处理"},
			"markup_bp": map[string]any{"type": "integer", "minimum": 0,
				"description": "**仅售价侧**：加价倍数，基点（10000 = 1.0 倍，15000 = 1.5 倍）。" +
					"dimension_markup_bp 可逐维度覆写。" +
					"**注意：在这里写 0 等于没写**——模型级售价的 0 与「未设置」不可区分，" +
					"写 0 会回落到 billing.default_markup_bp（默认 10000 = 原价）。" +
					"要把某个 Key 的加价真正降到 0（售价 = 成本），用该 Key 的 policy.margin_bp = 0；" +
					"要确认最终生效的倍率与来源，用 admin_pricing_targets 看 resolved_markup_bp/source"},
			"dimension_markup_bp": dimensionMarkupSchema(),
			"rules": map[string]any{"type": "array", "items": rule,
				"description": "规则数组。**售价侧 basis=cost_follow 时可以省略**（这时售价 = 成本 × markup_bp）；" +
					"cost_follow 之外的情况都必须给，且必须恰好包含一条 catch-all（when 为 {}）：" +
					"全部规则都不匹配的维度会按 0 计费（引擎会告警），所以不要把 catch-all 省掉"},
		},
		// rules is required unless basis=cost_follow: pricing.Validate accepts an empty rule array
		// on the cost_follow sale side because the mark-up does the work, and it is the sale side's
		// documented example (salePricingExample) that carries no rules.
		//
		// The exemption is written as an explicit marker rather than left implicit: the example
		// guard (TestGeneratedExamplesSatisfyTheirSchema) reads it, and the description of the rules
		// property states the same condition for the agent. JSON Schema's flat `required` cannot
		// express "required only when basis is absolute", so the condition is stated twice, once
		// for each reader.
		"required":                []string{"rules"},
		"x-rules-required-unless": []string{"basis=cost_follow"},
	}
}

// dimensionMarkupProperties documents the five metered dimensions for the per-dimension
// mark-up override, which uses the same names as the rate keys. It is consumed by
// dimensionMarkupSchema in admin_field_schemas.go.
func dimensionMarkupProperties() map[string]any {
	out := map[string]any{}
	for _, dimension := range pricingDimensions {
		out[dimension] = map[string]any{"type": "integer", "minimum": 0,
			"description": pricingDimensionDesc(dimension) + "；值为 bp（10000 = 1.0 倍）"}
	}
	return out
}

// pricingDimensionDesc is the one-line meaning of a rate key. A rate key that is not
// explained is a rate that gets set on the wrong dimension.
func pricingDimensionDesc(dimension string) string {
	switch dimension {
	case "input":
		return "未区分缓存的输入 token（上游没给缓存明细时按此维度计量）"
	case "input_cache_hit":
		return "缓存命中的输入 token（便宜的那部分）"
	case "input_cache_miss":
		return "缓存未命中的输入 token"
	case "output":
		return "输出 token"
	case "reasoning":
		return "思考 token（默认计入 output，单列时按本维度计价）"
	default:
		return dimension
	}
}

// pricingRuleSetJSONSchema is the rule set schema as a raw document, for endpoints whose
// whole request body *is* a rule set (admin_validate_pricing). It carries the example so a
// caller can copy a writable document instead of inventing one.
func pricingRuleSetJSONSchema(currency string) json.RawMessage {
	schema := pricingRuleSetSchema()
	schema["example"] = pricingRuleSetExample(currency)
	raw, err := json.Marshal(schema)
	if err != nil {
		// Unreachable: the schema is built from maps of strings, numbers and bools.
		panic("httpapi: encoding the pricing rule set schema failed: " + err.Error())
	}
	return raw
}

// pricingRuleSetExample is a writable cost rule set. The numbers are the shape of a real
// request: input/output/cache-hit prices per million tokens, converted to micros.
//
// The conversion is deliberately spelled out in the tool description as well, because it is
// the step a model gets wrong: "$0.20 per 1M tokens" is 200000 micros, not 0.2.
func pricingRuleSetExample(currency string) map[string]any {
	return map[string]any{
		"currency": currency,
		"rules": []any{map[string]any{
			"id": "cost", "title": "标准价", "order": 10, "when": map[string]any{},
			"rates": map[string]any{
				"input_cache_hit": 20000, "input_cache_miss": 200000, "output": 1200000,
			},
		}},
	}
}

// salePricingExample is the sale-side counterpart: follow the cost and multiply by 1.1
// (markup_bp 11000 = 10%).
//
// It deliberately does not use 0. A model-level markup_bp of 0 is indistinguishable from
// "unset" and falls back to billing.default_markup_bp, so an example built on 0 would teach an
// agent to write a document that silently does not do what it looks like it does. A true 0%
// markup is expressed on the key or tag policy (policy.margin_bp = 0), which is checked before
// the model's own rate and is the only place the zero is distinguishable.
//
// The mark-up belongs on the sale side only: a cost document with markup_bp is an unknown field
// and is rejected outright.
func salePricingExample(currency string) map[string]any {
	return map[string]any{"currency": currency, "basis": "cost_follow", "markup_bp": 11000}
}

// pricingRuleSchemaFields returns the body field an endpoint that accepts a cost rule set
// declares, plus the example value that makes it copyable.
//
// It takes the ledger currency because the example has to be a document this deployment can
// really write: a rule set whose currency has no fx rate is rejected, so a hard-coded "USD"
// example would be uncopyable on a CNY deployment.
func pricingRuleSchemaFields(currency string) []adminField {
	return []adminField{
		exampleField(schemaField(bodyOptional("pricing_rules", "object",
			"成本侧价格规则集：我们付给上游的真实计费，忠实还原上游的维度与单位（含缓存命中/未命中）。"+
				"**单价是微单位/百万 token**：$0.20/1M 写 200000，$1.20/1M 写 1200000，缓存命中 $0.02/1M 写 20000。"+
				"字段名与结构见本参数 schema，必须严格照写（未知字段 400）；不要放 basis/markup_bp（那是售价侧的事）。"+
				"拿不准先 admin_validate_pricing。"),
			pricingRuleSetSchema()), pricingRuleSetExample(currency)),
	}
}

// salePricingField is the model-side counterpart of the field above. The example is a
// cost_follow document with no mark-up, which is the "售价 = 成本" case.
func salePricingField(desc, currency string) adminField {
	return exampleField(schemaField(bodyOptional("sale_pricing", "object", desc),
		pricingRuleSetSchema()), salePricingExample(currency))
}
