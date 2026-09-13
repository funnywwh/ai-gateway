package responses

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SessionHeaders contains only the identity headers understood by the log extractor.
// It is not serialized or forwarded: transport metadata must not change model input.
type SessionHeaders struct {
	TurnMetadata string
	SessionID    string
	ThreadID     string
}

func (r *Request) SetSessionHeaders(headers http.Header) {
	r.SessionHeaders = SessionHeaders{
		TurnMetadata: headers.Get("x-codex-turn-metadata"),
		SessionID:    headers.Get("session-id"),
		ThreadID:     headers.Get("thread-id"),
	}
	// DSH's OpenAI adapter uses session_id (underscore) when enabled.
	if r.SessionHeaders.SessionID == "" {
		r.SessionHeaders.SessionID = headers.Get("session_id")
	}
}

type sessionMetadata struct {
	SessionID   string `json:"session_id"`
	ThreadID    string `json:"thread_id"`
	TurnTrigger string `json:"turn_trigger"`
}

type clientSessionMetadata struct {
	sessionMetadata
	TurnMetadata string `json:"x-codex-turn-metadata"`
}

// LogSessionKey uses Codex's root session identity rather than its cache affinity.
// Codex 0.153.4: responses_metadata.rs defines the canonical JSON snapshot and its
// flat/header projections; session/session.rs makes session_id the live tree root.
// A parent_thread_id is only the immediate parent, not necessarily the root.
func (r *Request) LogSessionKey() string {
	key, _ := r.logSession()
	return key
}

func (r *Request) logSession() (string, bool) {
	if len(r.Extra["client_metadata"]) == 0 && r.SessionHeaders == (SessionHeaders{}) &&
		r.Metadata["session_id"] == "" && r.Metadata["thread_id"] == "" {
		return r.SessionKey(), false
	}
	var flat clientSessionMetadata
	// Metadata is best effort. Bad field types must not reject an otherwise valid request.
	if raw := r.Extra["client_metadata"]; len(raw) > 0 && json.Unmarshal(raw, &flat) != nil {
		flat = clientSessionMetadata{}
	}
	var canonical, header sessionMetadata
	if flat.TurnMetadata != "" && json.Unmarshal([]byte(flat.TurnMetadata), &canonical) != nil {
		canonical = sessionMetadata{}
	}
	// Body metadata is canonical; most Codex requests also repeat it in a header.
	// Decode that compatibility copy only if it can supply missing root identity or
	// a title marker absent from body metadata.
	needHeader := strings.TrimSpace(canonical.SessionID) == "" && strings.TrimSpace(flat.SessionID) == "" || flat.TurnMetadata == ""
	if needHeader && r.SessionHeaders.TurnMetadata != "" && json.Unmarshal([]byte(r.SessionHeaders.TurnMetadata), &header) != nil {
		header = sessionMetadata{}
	}
	sources := [...]sessionMetadata{
		canonical, flat.sessionMetadata, header,
		{SessionID: r.Metadata["session_id"], ThreadID: r.Metadata["thread_id"]},
		{SessionID: r.SessionHeaders.SessionID, ThreadID: r.SessionHeaders.ThreadID},
	}
	title := canonical.TurnTrigger == "thread_title" || header.TurnTrigger == "thread_title"
	// Prefer a root id from any explicit projection over a per-thread id.
	for _, source := range sources {
		if value := clampBytes(source.SessionID, maxSessionIDBytes); value != "" {
			return value, title
		}
	}
	for _, source := range sources {
		if value := clampBytes(source.ThreadID, maxSessionIDBytes); value != "" {
			return value, title
		}
	}
	return r.SessionKey(), title
}
