package mcpsrv

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

// This file is the mechanical half of the tool-description standard in docs/mcp.md §4.5.
//
// The standard exists because of a real failure: an agent asked to configure a cost price
// found pricing_rules described as an object with no fields, and refused to write anything —
// correctly, since guessing a field name was a 400. A description is not decoration, it is the
// interface document the model reads, and the only way to keep eleven of them complete is to
// check them. These tests read the same Tools() the client reads, so a new tool cannot be added
// without a description that answers the four questions the standard requires.

// useCaseMarkers are the ways a description states when the tool applies. The check is for one
// of them rather than for a fixed sentence, because the standard is about the information being
// there, not about a phrasing — but "there is no marker at all" is a description that only says
// what the tool is, which is the half a model cannot plan with.
var useCaseMarkers = []string{"用在", "用于", "适合", "要改", "要的是", "只看"}

func TestQueryToolDescriptionsAreComplete(t *testing.T) {
	service, _, _ := newMCPFixture(t)
	tools := service.Tools()
	if len(tools) == 0 {
		t.Fatal("no query tools")
	}
	otherNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		otherNames = append(otherNames, tool.Name)
	}
	for _, tool := range tools {
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			t.Errorf("tool %s has no description (standard: docs/mcp.md §4.5)", tool.Name)
			continue
		}
		if !hasCJK(description) {
			t.Errorf("tool %s describes itself in a language the console does not use; the tool "+
				"surface is Chinese like the route summaries (standard: docs/mcp.md §4.5)", tool.Name)
		}
		if !strings.Contains(description, "返回：") {
			t.Errorf("tool %s: no 返回： section, so an agent cannot know what the payload holds "+
				"(standard: docs/mcp.md §4.5)", tool.Name)
			continue
		}
		if _, returned, found := strings.Cut(description, "返回："); !found || strings.TrimSpace(returned) == "" {
			t.Errorf("tool %s: the 返回： section is empty (standard: docs/mcp.md §4.5)", tool.Name)
		}
		if !containsAny(description, useCaseMarkers) {
			t.Errorf("tool %s: the description says what the tool is but not when to use it "+
				"(one of %v); a model that cannot tell two tools apart picks one at random "+
				"(standard: docs/mcp.md §4.5)", tool.Name, useCaseMarkers)
		}
		// Naming a neighbour is what makes "when to use this" actionable instead of aspirational.
		if !namesAnotherTool(description, tool.Name, otherNames) {
			t.Errorf("tool %s: the description names no other tool, so a model cannot tell which "+
				"of the overlapping tools to reach for (standard: docs/mcp.md §4.5)", tool.Name)
		}
	}
}

// namesAnotherTool reports whether a description refers to a sibling query tool.
func namesAnotherTool(description, self string, candidates []string) bool {
	for _, name := range candidates {
		if name != self && strings.Contains(description, name) {
			return true
		}
	}
	// Referring to the write-side entry points counts too: several query tools say "to change
	// this, use admin_update_model".
	return strings.Contains(description, "admin_")
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// TestQueryToolDescriptionsCoverTheirSchema is the direction that actually bit: a parameter
// existed in the schema, the description never named it, and the model had to guess whether it
// existed.
func TestQueryToolDescriptionsCoverTheirSchema(t *testing.T) {
	service, _, _ := newMCPFixture(t)
	for _, tool := range service.Tools() {
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
				Enum        []string
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("tool %s: input schema is not valid JSON: %v", tool.Name, err)
			continue
		}
		for name, property := range schema.Properties {
			if strings.TrimSpace(property.Description) == "" {
				t.Errorf("tool %s: parameter %q has no description; say what it means, its unit and "+
					"its default (standard: docs/mcp.md §4.5)", tool.Name, name)
			}
			if !strings.Contains(tool.Description, name) {
				t.Errorf("tool %s: the description never names its own parameter %q, so an agent "+
					"reading the description cannot know the parameter exists "+
					"(standard: docs/mcp.md §4.5)", tool.Name, name)
			}
		}
	}
}

// TestPeriodWindowsAreTheSameEverywhere keeps the one parameter that decides "which numbers am
// I looking at" identical across tools: a tool that accepts a period but does not enumerate the
// values invites a model to invent one, and silently defaults to a window it never mentioned.
func TestPeriodWindowsAreTheSameEverywhere(t *testing.T) {
	service, _, _ := newMCPFixture(t)
	withPeriod := 0
	for _, tool := range service.Tools() {
		var schema struct {
			Properties map[string]struct {
				Type        string   `json:"type"`
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %s: %v", tool.Name, err)
		}
		period, ok := schema.Properties["period"]
		if !ok {
			continue
		}
		withPeriod++
		if strings.Join(period.Enum, ",") != strings.Join(periodEnum, ",") {
			t.Errorf("tool %s: period accepts %v, expected %v", tool.Name, period.Enum, periodEnum)
		}
		if !strings.Contains(period.Description, "last_7_days") {
			t.Errorf("tool %s: the period description must state the default window: %s",
				tool.Name, period.Description)
		}
	}
	if withPeriod < 5 {
		t.Fatalf("only %d tools declare a period; the extraction is wrong, not the code", withPeriod)
	}
}

// TestQueryToolNamesAreDerivedFromTheDeclarations is the guard that keeps the closed set and the
// served list from becoming two lists. The chat routes on IsQueryTool, so a name that is served
// but not in the set gets folded into admin_request and answered with a bogus unknown-endpoint
// error — a failure that already happened once in production.
func TestQueryToolNamesAreDerivedFromTheDeclarations(t *testing.T) {
	service, _, _ := newMCPFixture(t)
	declared := map[string]bool{}
	for _, tool := range service.Tools() {
		declared[tool.Name] = true
	}
	if len(declared) != len(queryToolNames) {
		t.Fatalf("Tools() declares %d tools but queryToolNames has %d", len(declared), len(queryToolNames))
	}
	for name := range declared {
		if !IsQueryTool(name) {
			t.Errorf("tool %q is declared but IsQueryTool reports false", name)
		}
	}
}

// TestEveryQueryToolIsDispatched keeps a tool from being advertised and then failing at call
// time: the surface a client sees must be the surface callRead implements.
func TestEveryQueryToolIsDispatched(t *testing.T) {
	service, _, _ := newMCPFixture(t)
	for _, tool := range service.Tools() {
		if _, _, known := service.callRead(t.Context(), 0, tool.Name, nil); !known {
			t.Errorf("tool %q is served by Tools() but callRead does not dispatch it", tool.Name)
		}
	}
}

// hasCJK reports whether a string contains a Han character, which is how these tests tell the
// Chinese operator-facing text from a stray English sentence.
func hasCJK(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
