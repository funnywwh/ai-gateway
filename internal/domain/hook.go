package domain

import "time"

// Hook is one outbound event sink (webhook or local JSONL file).
type Hook struct {
	ID             int64
	Name           string
	Type           string // webhook|jsonl
	URL            string // webhook target, or file path for jsonl
	Secret         string
	EventsJSON     string // JSON array of event names; empty means "all"
	IncludeContent bool
	MaxBytes       int
	SampleRate     float64
	Enabled        bool
	CreatedAt      time.Time
}

// MatchesEvent reports whether the hook subscribes to the given event name.
func (h *Hook) MatchesEvent(event string) bool {
	if h == nil {
		return false
	}
	if h.EventsJSON == "" || h.EventsJSON == "[]" {
		return true
	}
	var names []string
	if err := jsonUnmarshal(h.EventsJSON, &names); err != nil {
		return true // misconfigured filter: fail open for observability
	}
	if len(names) == 0 {
		return true
	}
	for _, name := range names {
		if name == event || name == "*" {
			return true
		}
	}
	return false
}
