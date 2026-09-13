package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ModelReasoning controls the reasoning effort requested for a canonical model.
// Mode determines whether the configured effort is a default or is forced onto a
// request; Effort is the provider-supported reasoning level.
type ModelReasoning struct {
	Mode   string `json:"mode" yaml:"mode"`
	Effort string `json:"effort" yaml:"effort"`
}

// Validate checks that a model reasoning configuration can be applied.
func (r ModelReasoning) Validate() error {
	switch r.Mode {
	case "default", "force":
	default:
		return fmt.Errorf("reasoning mode must be default|force (got %q)", r.Mode)
	}
	switch r.Effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	default:
		return fmt.Errorf("reasoning effort must be none|minimal|low|medium|high|xhigh|max (got %q)", r.Effort)
	}
}

// ParseModelReasoning decodes one model reasoning JSON document. Empty input and
// JSON null mean no model-level reasoning configuration. Unknown fields are
// rejected so an operator cannot persist a setting the gateway does not read.
func ParseModelReasoning(raw string) (*ModelReasoning, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	var reasoning ModelReasoning
	decoder := json.NewDecoder(bytes.NewBufferString(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reasoning); err != nil {
		return nil, fmt.Errorf("model reasoning must be a JSON object with mode and effort: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("model reasoning must be a single JSON object")
	}
	if err := reasoning.Validate(); err != nil {
		return nil, err
	}
	return &reasoning, nil
}
