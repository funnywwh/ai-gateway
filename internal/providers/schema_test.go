package providers

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/providers/openaichat"
	"github.com/winger/ai-gateway/internal/providers/openairesponses"
	"github.com/winger/ai-gateway/internal/providers/testecho"
)

// kindConfigs maps every builtin kind to the Go config struct whose fields the admin
// console documents. The reflective check below is the point of this file: a field
// added to one of these structs without a description in the matching schema fails
// here, which is the only durable defence against "no configuration docs".
func kindConfigs() map[string]any {
	return map[string]any{
		KindOpenAIChat:      openaichat.Config{},
		KindOpenAIResponses: openairesponses.Config{},
		KindTestEcho:        testecho.Config{},
	}
}

func TestSchemasCoverEveryBuiltinKind(t *testing.T) {
	schemas := Schemas()
	kinds := BuiltinKinds()
	if len(schemas) != len(kinds) {
		t.Fatalf("Schemas() returned %d entries, want %d (one per builtin kind)", len(schemas), len(kinds))
	}
	for i, ks := range schemas {
		if ks.Kind != kinds[i] {
			t.Fatalf("Schemas()[%d].Kind = %q, want %q (sorted by kind)", i, ks.Kind, kinds[i])
		}
		if ks.Source != SchemaSourceBuiltin {
			t.Fatalf("%s: source = %q, want %q", ks.Kind, ks.Source, SchemaSourceBuiltin)
		}
		if strings.TrimSpace(ks.Note) == "" {
			t.Fatalf("%s: the kind note is empty; operators need to know what this kind talks to", ks.Kind)
		}
		if len(ks.Config) == 0 || len(ks.Credentials) == 0 || len(ks.Template) == 0 {
			t.Fatalf("%s: config schema / credentials schema / template must all be present", ks.Kind)
		}
	}
}

func TestSchemaForUnknownAndPluginKinds(t *testing.T) {
	plugin := SchemaFor("plugin:whatever")
	if plugin.Source != SchemaSourcePlugin {
		t.Fatalf("plugin kind source = %q, want %q", plugin.Source, SchemaSourcePlugin)
	}
	if !strings.Contains(plugin.Note, "握手") {
		t.Fatalf("plugin note must explain that the schema comes from the handshake, got %q", plugin.Note)
	}
	if len(plugin.Config) != 0 {
		t.Fatal("a plugin kind must not carry a builtin config schema")
	}

	unknown := SchemaFor("nope")
	if unknown.Source != SchemaSourceUnknown {
		t.Fatalf("unknown kind source = %q, want %q", unknown.Source, SchemaSourceUnknown)
	}
	for _, kind := range BuiltinKinds() {
		if !strings.Contains(unknown.Note, kind) {
			t.Fatalf("unknown-kind note must list the builtin kinds, missing %q in %q", kind, unknown.Note)
		}
	}

	if SchemaFor("").Source != SchemaSourceUnknown {
		t.Fatal("the empty kind must not be reported as a builtin schema")
	}
}

func TestSchemaPropertiesMatchConfigStructs(t *testing.T) {
	configs := kindConfigs()
	for kind, cfg := range configs {
		ks := SchemaFor(kind)
		props := mustProps(t, kind, "config", ks.Config)
		compareStruct(t, kind+".config", reflect.TypeOf(cfg), props)

		// Every declared property must be explained: an undocumented field is the
		// exact failure this milestone exists to prevent.
		for _, name := range sortedKeys(props) {
			checkProperty(t, kind+".config."+name, props[name], false)
		}
	}
}

func TestCredentialsSchemaIsExplainedAndSealed(t *testing.T) {
	// Kinds that authenticate upstream must document the api_key credential, and the
	// config field of the same name must point at the credential channel.
	for _, kind := range []string{KindOpenAIChat, KindOpenAIResponses} {
		ks := SchemaFor(kind)
		creds := mustProps(t, kind, "credentials", ks.Credentials)
		apiKey, ok := creds["api_key"]
		if !ok {
			t.Fatalf("%s: the credentials schema must declare api_key", kind)
		}
		if apiKey["x-secret"] != true {
			t.Fatalf("%s: credentials.api_key must be marked x-secret", kind)
		}
		checkProperty(t, kind+".credentials.api_key", apiKey, true)

		cfg := mustProps(t, kind, "config", ks.Config)
		if cfg["api_key"]["x-prefer-credential"] != "api_key" {
			t.Fatalf("%s: config.api_key must carry x-prefer-credential=api_key so the console points at the credential field", kind)
		}
		var doc struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(ks.Credentials, &doc); err != nil {
			t.Fatalf("%s: credentials schema is not valid JSON: %v", kind, err)
		}
		if len(doc.Required) == 0 || doc.Required[0] != "api_key" {
			t.Fatalf("%s: credentials.required = %v, want api_key first", kind, doc.Required)
		}
	}

	// testecho takes no credentials: say so instead of leaving the field unexplained.
	creds := mustProps(t, KindTestEcho, "credentials", SchemaFor(KindTestEcho).Credentials)
	if len(creds) != 0 {
		t.Fatalf("testecho credentials = %v, want no fields", sortedKeys(creds))
	}
	var doc map[string]any
	if err := json.Unmarshal(SchemaFor(KindTestEcho).Credentials, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(asString(doc["description"])) == "" {
		t.Fatal("testecho credentials schema must explain that credentials are unused")
	}
}

func TestCredentialPlacementIsNeverSecretInConfig(t *testing.T) {
	// x-secret belongs to credentials only: a config field is stored and echoed in
	// clear text, so marking it secret there would be a lie.
	for _, ks := range Schemas() {
		for name, prop := range mustProps(t, ks.Kind, "config", ks.Config) {
			if prop["x-secret"] == true {
				t.Fatalf("%s: config.%s must not be marked x-secret (config is stored in clear text)", ks.Kind, name)
			}
		}
	}
}

func TestTemplatesOnlyUseDeclaredFields(t *testing.T) {
	for _, ks := range Schemas() {
		props := mustProps(t, ks.Kind, "config", ks.Config)
		var tmpl map[string]json.RawMessage
		if err := json.Unmarshal(ks.Template, &tmpl); err != nil {
			t.Fatalf("%s: template is not a JSON object: %v", ks.Kind, err)
		}
		if len(tmpl) == 0 {
			t.Fatalf("%s: template is empty; it must be a usable starting point", ks.Kind)
		}
		for name := range tmpl {
			prop, ok := props[name]
			if !ok {
				t.Fatalf("%s: template uses undeclared field %q", ks.Kind, name)
			}
			// Nested objects/arrays in the template must stick to declared element
			// fields too, so "create from template" never produces an unknown key.
			switch prop["type"] {
			case "array":
				items, _ := prop["items"].(map[string]any)
				nested, _ := items["properties"].(map[string]any)
				var rows []map[string]json.RawMessage
				if err := json.Unmarshal(tmpl[name], &rows); err != nil {
					t.Fatalf("%s: template.%s is not an array of objects: %v", ks.Kind, name, err)
				}
				for _, row := range rows {
					for key := range row {
						if _, ok := nested[key]; !ok {
							t.Fatalf("%s: template.%s[] uses undeclared field %q", ks.Kind, name, key)
						}
					}
				}
			case "object":
				nested, _ := prop["properties"].(map[string]any)
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(tmpl[name], &obj); err != nil {
					t.Fatalf("%s: template.%s is not an object: %v", ks.Kind, name, err)
				}
				for key := range obj {
					if _, ok := nested[key]; !ok {
						t.Fatalf("%s: template.%s uses undeclared field %q", ks.Kind, name, key)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustProps(t *testing.T, kind, which string, schema json.RawMessage) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		t.Fatalf("%s: %s schema is not valid JSON: %v", kind, which, err)
	}
	if doc.Type != "object" {
		t.Fatalf("%s: %s schema type = %q, want object", kind, which, doc.Type)
	}
	return doc.Properties
}

// compareStruct asserts a struct's json tags and a schema's properties describe the
// same set of fields, in both directions, and recurses into nested structs.
func compareStruct(t *testing.T, path string, typ reflect.Type, props map[string]map[string]any) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("%s: %s is not a struct", path, typ)
	}

	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" { // unexported
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		seen[name] = true
		prop, ok := props[name]
		if !ok {
			t.Fatalf("%s: field %q has no description in the schema; every configuration field must be documented", path, name)
		}
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		switch fieldType.Kind() {
		case reflect.Struct:
			// A struct field must document its own fields, otherwise operators see a
			// field with no explanation of what goes inside it.
			nested, ok := prop["properties"].(map[string]any)
			if !ok {
				t.Fatalf("%s.%s is an object in Go but declares no properties", path, name)
			}
			compareStruct(t, path+"."+name, fieldType, toPropsMap(t, path+"."+name, nested))
		case reflect.Slice:
			elem := fieldType.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() != reflect.Struct {
				continue
			}
			items, ok := prop["items"].(map[string]any)
			if !ok {
				t.Fatalf("%s.%s is an array of objects in Go but declares no items", path, name)
			}
			nested, ok := items["properties"].(map[string]any)
			if !ok {
				t.Fatalf("%s.%s[] declares no properties", path, name)
			}
			compareStruct(t, path+"."+name+"[]", elem, toPropsMap(t, path+"."+name+"[]", nested))
		}
	}

	for _, name := range sortedKeys(props) {
		if !seen[name] {
			t.Fatalf("%s: the schema documents %q but the config struct has no such field", path, name)
		}
	}
}

func toPropsMap(t *testing.T, path string, raw map[string]any) map[string]map[string]any {
	t.Helper()
	out := make(map[string]map[string]any, len(raw))
	for name, value := range raw {
		prop, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s.%s: property is not an object", path, name)
		}
		out[name] = prop
	}
	return out
}

// checkProperty validates one property's own consistency: a known type, a non-empty
// description, and a default that actually matches the declared type and enum.
func checkProperty(t *testing.T, path string, prop map[string]any, credentials bool) {
	t.Helper()
	kind := asString(prop["type"])
	switch kind {
	case "object", "string", "integer", "number", "boolean", "array":
	default:
		t.Fatalf("%s: type = %q is not one of the supported JSON Schema types", path, kind)
	}
	if strings.TrimSpace(asString(prop["description"])) == "" {
		t.Fatalf("%s: every field needs a description; that is the whole point of this schema", path)
	}
	if secret, ok := prop["x-secret"]; ok && secret == true && !credentials {
		t.Fatalf("%s: x-secret is only meaningful in a credentials schema", path)
	}
	if required, ok := prop["x-required"]; ok {
		if _, isBool := required.(bool); !isBool {
			t.Fatalf("%s: x-required must be a boolean", path)
		}
	}
	if pref, ok := prop["x-prefer-credential"]; ok {
		key, isString := pref.(string)
		if !isString || key == "" {
			t.Fatalf("%s: x-prefer-credential must name the credential key", path)
		}
	}

	def, hasDefault := prop["default"]
	if hasDefault && !defaultMatchesType(kind, def) {
		t.Fatalf("%s: default %v does not match the declared type %q", path, def, kind)
	}
	if rawEnum, ok := prop["enum"]; ok {
		list, isList := rawEnum.([]any)
		if !isList || len(list) == 0 {
			t.Fatalf("%s: enum must be a non-empty array", path)
		}
		if hasDefault && !containsValue(list, def) {
			t.Fatalf("%s: default %v is not one of the declared enum values %v", path, def, list)
		}
	}
}

func defaultMatchesType(kind string, def any) bool {
	switch kind {
	case "string":
		_, ok := def.(string)
		return ok
	case "boolean":
		_, ok := def.(bool)
		return ok
	case "integer":
		number, isNumber := def.(float64)
		return isNumber && number == float64(int64(number))
	case "number":
		_, ok := def.(float64)
		return ok
	case "array":
		_, ok := def.([]any)
		return ok
	case "object":
		_, ok := def.(map[string]any)
		return ok
	default:
		return false
	}
}

func containsValue(list []any, want any) bool {
	for _, item := range list {
		if reflect.DeepEqual(item, want) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
