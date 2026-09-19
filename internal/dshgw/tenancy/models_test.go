package tenancy

import "github.com/winger/ai-gateway/internal/dshgw/aigw"

// models builds the disclosed-model list a test hands to the renderer: an id and nothing
// else, which is exactly what an aigw that predates the capability fields answers.
func models(ids ...string) []aigw.Model {
	out := make([]aigw.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, aigw.Model{ID: id})
	}
	return out
}

// reasoningSupport is the pointer a disclosed reasoning capability needs; the nil case (no
// capability set disclosed at all) is spelled by leaving the field alone.
func reasoningSupport(supported bool) *bool { return &supported }
