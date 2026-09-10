package pluginapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// FieldError is one validation failure, addressed by JSON path.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Schema is the supported subset of JSON Schema used to render and validate
// provider configuration forms. Unknown keywords are ignored.
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Default     json.RawMessage    `json:"default,omitempty"`
	Enum        []json.RawMessage  `json:"enum,omitempty"`
	Minimum     *float64           `json:"minimum,omitempty"`
	Maximum     *float64           `json:"maximum,omitempty"`
	Pattern     string             `json:"pattern,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	MinItems    *int               `json:"minItems,omitempty"`
	MaxItems    *int               `json:"maxItems,omitempty"`
	Format      string             `json:"format,omitempty"`

	// UI extensions (metadata only; not validated).
	XTitle       string `json:"x-title,omitempty"`
	XDescription string `json:"x-description,omitempty"`
	XGroup       string `json:"x-group,omitempty"`
	XOrder       int    `json:"x-order,omitempty"`
	XSecret      bool   `json:"x-secret,omitempty"`
	XAdvanced    bool   `json:"x-advanced,omitempty"`
	XPlaceholder string `json:"x-placeholder,omitempty"`
	XTextarea    bool   `json:"x-textarea,omitempty"`
	XUnit        string `json:"x-unit,omitempty"`
	XModelMap    bool   `json:"x-model-map,omitempty"`
}

// ParseSchema decodes a schema document (empty input yields a permissive schema).
func ParseSchema(raw json.RawMessage) (*Schema, error) {
	if len(raw) == 0 {
		return &Schema{}, nil
	}
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("pluginapi: parse schema: %w", err)
	}
	return &s, nil
}

// Validate checks value against the schema and returns every failure found.
func (s *Schema) Validate(value any) []FieldError {
	out := []FieldError{}
	s.validate(value, "", &out)
	return out
}

func (s *Schema) validate(value any, path string, out *[]FieldError) {
	if s == nil {
		return
	}
	switch s.Type {
	case "object", "":
		if len(s.Properties) == 0 && len(s.Required) == 0 {
			return
		}
		obj, ok := value.(map[string]any)
		if !ok {
			*out = append(*out, FieldError{Path: displayPath(path), Message: "must be an object"})
			return
		}
		for _, req := range s.Required {
			if _, exists := obj[req]; !exists {
				*out = append(*out, FieldError{Path: joinPath(path, req), Message: "is required"})
			}
		}
		for name, prop := range s.Properties {
			v, exists := obj[name]
			if !exists {
				continue
			}
			prop.validate(v, joinPath(path, name), out)
		}
	case "string":
		str, ok := value.(string)
		if !ok {
			*out = append(*out, FieldError{Path: displayPath(path), Message: "must be a string"})
			return
		}
		if s.Pattern != "" {
			re, err := regexp.Compile(s.Pattern)
			if err != nil {
				*out = append(*out, FieldError{Path: displayPath(path), Message: "schema pattern is invalid: " + err.Error()})
			} else if !re.MatchString(str) {
				*out = append(*out, FieldError{Path: displayPath(path), Message: "does not match pattern " + s.Pattern})
			}
		}
		switch s.Format {
		case "uri":
			if _, err := url.Parse(str); err != nil {
				*out = append(*out, FieldError{Path: displayPath(path), Message: "must be a valid URI"})
			}
		case "duration":
			if _, err := time.ParseDuration(str); err != nil {
				*out = append(*out, FieldError{Path: displayPath(path), Message: "must be a duration such as 30s"})
			}
		}
	case "number":
		num, ok := toFloat(value)
		if !ok {
			*out = append(*out, FieldError{Path: displayPath(path), Message: "must be a number"})
			return
		}
		s.checkRange(num, path, out)
	case "integer":
		num, ok := toFloat(value)
		if !ok || num != float64(int64(num)) {
			*out = append(*out, FieldError{Path: displayPath(path), Message: "must be an integer"})
			return
		}
		s.checkRange(num, path, out)
	case "boolean":
		if _, ok := value.(bool); !ok {
			*out = append(*out, FieldError{Path: displayPath(path), Message: "must be a boolean"})
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			*out = append(*out, FieldError{Path: displayPath(path), Message: "must be an array"})
			return
		}
		if s.MinItems != nil && len(items) < *s.MinItems {
			*out = append(*out, FieldError{Path: displayPath(path), Message: fmt.Sprintf("needs at least %d items", *s.MinItems)})
		}
		if s.MaxItems != nil && len(items) > *s.MaxItems {
			*out = append(*out, FieldError{Path: displayPath(path), Message: fmt.Sprintf("allows at most %d items", *s.MaxItems)})
		}
		if s.Items != nil {
			for i, item := range items {
				s.Items.validate(item, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}
	}

	if len(s.Enum) > 0 && !s.enumContains(value) {
		allowed := make([]string, 0, len(s.Enum))
		for _, e := range s.Enum {
			allowed = append(allowed, strings.TrimSpace(string(e)))
		}
		*out = append(*out, FieldError{
			Path:    displayPath(path),
			Message: "must be one of " + strings.Join(allowed, "|"),
		})
	}
}

func (s *Schema) checkRange(num float64, path string, out *[]FieldError) {
	if s.Minimum != nil && num < *s.Minimum {
		*out = append(*out, FieldError{Path: displayPath(path), Message: fmt.Sprintf("must be >= %v", *s.Minimum)})
	}
	if s.Maximum != nil && num > *s.Maximum {
		*out = append(*out, FieldError{Path: displayPath(path), Message: fmt.Sprintf("must be <= %v", *s.Maximum)})
	}
}

func (s *Schema) enumContains(value any) bool {
	want, err := json.Marshal(value)
	if err != nil {
		return false
	}
	for _, e := range s.Enum {
		if string(e) == string(want) {
			return true
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}

func displayPath(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}
