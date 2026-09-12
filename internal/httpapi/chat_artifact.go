package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
)

// Previewing a model-authored HTML page or SVG needs a URL an iframe can load. Two designs
// were rejected before this one:
//
//   - inline `srcdoc`/Blob: the document inherits the console's CSP, which has no
//     'unsafe-inline' for scripts, so the page's own JavaScript never runs;
//   - a permanent unguessable URL: it is a bearer credential that outlives the session that
//     created it, so a leaked screenshot of a URL would keep working forever.
//
// What is left is a short-lived ticket bound to the artifact *and* to the administrator
// session that asked for it. The frame is sandboxed without allow-same-origin, so its
// requests carry an opaque origin and no cookie is sent — the ticket, not a cookie, is what
// authorizes the fetch. Because it is a bearer credential for one payload with a five
// minute life, leaking it is worth strictly less than leaking the conversation.
//
// The payload itself is served with a `sandbox` CSP directive, which keeps the document in
// an opaque origin even when it is opened as a top-level page, and with `default-src 'none'`
// so the page cannot load external resources or open network connections unless the operator
// explicitly turns that on.

// ChatArtifactStore is the persistence the preview endpoints need. It is separate from
// chat.Store on purpose: the chat service never reads or writes a preview payload, and
// widening its port would suggest otherwise.
type ChatArtifactStore interface {
	UpsertChatArtifact(ctx context.Context, a *domain.ChatArtifact) error
	GetChatArtifact(ctx context.Context, id string, ownerUserID int64) (*domain.ChatArtifact, error)
	PruneChatArtifacts(ctx context.Context, sessionID string, keep int) error
	AdminSessionUser(ctx context.Context, sessionID string) (int64, error)
}

// chatArtifacts resolves the artifact store from the chat store when it implements it.
func (s *Server) chatArtifacts() ChatArtifactStore {
	if store, ok := s.deps.ChatStore.(ChatArtifactStore); ok {
		return store
	}
	return nil
}

// chatTicketSigner signs preview tickets. The key is generated per process: a restart
// invalidates every outstanding ticket, which is the right default for a five-minute URL.
type chatTicketSigner struct{ key []byte }

func newChatTicketSigner() *chatTicketSigner {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// Without randomness there is no safe ticket; refuse to hand out previews rather
		// than sign with a predictable key.
		return &chatTicketSigner{}
	}
	return &chatTicketSigner{key: key}
}

func (s *chatTicketSigner) ready() bool { return s != nil && len(s.key) == 32 }

// sign returns the ticket for one (artifact, session, expiry) triple.
func (s *chatTicketSigner) sign(artifactID, adminSessionID string, expires time.Time) string {
	if !s.ready() {
		return ""
	}
	payload := artifactID + "|" + adminSessionID + "|" + strconv.FormatInt(expires.UnixNano(), 10)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		hex.EncodeToString(mac.Sum(nil))
}

// verify checks a ticket and returns the administrator session it was issued to.
func (s *chatTicketSigner) verify(ticket, artifactID string, now time.Time) (string, bool) {
	if !s.ready() || ticket == "" {
		return "", false
	}
	parts := strings.SplitN(ticket, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	fields := strings.Split(string(decoded), "|")
	if len(fields) != 3 || fields[0] != artifactID {
		return "", false
	}
	expires, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || now.UnixNano() > expires {
		return "", false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(decoded)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return "", false
	}
	return fields[1], true
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

type chatArtifactRequest struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Format string `json:"format"`
	Body   string `json:"body"`
}

// handleAdminChatPutArtifact stores a preview payload. The console uploads the exact bytes
// it is about to preview, which is what lets streaming answers be previewed before they are
// stored and keeps the server from re-parsing markdown to find code fences.
func (s *Server) handleAdminChatPutArtifact(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	artifacts := s.chatArtifacts()
	if artifacts == nil {
		writeAPIError(w, domain.ErrUnsupported("the console chat is disabled on this deployment"))
		return
	}
	sessionID := r.PathValue("id")
	// Ownership is checked through the chat service's own read path, so an artifact can
	// never be attached to somebody else's conversation.
	if _, err := s.chat.Session(r.Context(), user.ID, sessionID, nil); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body chatArtifactRequest
	if !decodeChatBody(w, r, &body) {
		return
	}
	format := strings.ToLower(strings.TrimSpace(body.Format))
	if format == "" {
		format = domain.ChatArtifactHTML
	}
	if format != domain.ChatArtifactHTML && format != domain.ChatArtifactSVG {
		writeAPIError(w, domain.ErrInvalidRequest("format must be html or svg"))
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		writeAPIError(w, domain.ErrInvalidRequest("key is required: it identifies the code block being previewed"))
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeAPIError(w, domain.ErrInvalidRequest("there is nothing to preview"))
		return
	}
	limit := s.chatArtifactLimit()
	if len(body.Body) > limit {
		writeAPIError(w, domain.ErrInvalidRequest(fmt.Sprintf("this preview is larger than the %d byte limit", limit)))
		return
	}
	artifact := &domain.ChatArtifact{
		ID:          ids.ChatArtifact(),
		OwnerUserID: user.ID,
		SessionID:   sessionID,
		Key:         key,
		Title:       truncateForTitle(body.Title),
		Format:      format,
		Body:        body.Body,
		SizeBytes:   len(body.Body),
		CreatedAt:   time.Now().UTC(),
	}
	if err := artifacts.UpsertChatArtifact(r.Context(), artifact); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// Re-previews of the same block reuse its row, so the table stays bounded by the number
	// of distinct blocks rather than the number of clicks.
	if err := artifacts.PruneChatArtifacts(r.Context(), sessionID, s.chatArtifactKeep()); err != nil {
		s.deps.Log.Warn("pruning chat artifacts failed", "err", err, "session", sessionID)
	}
	// The stored id may differ from the one we generated when the row already existed.
	stored, err := artifacts.GetChatArtifact(r.Context(), artifact.ID, user.ID)
	if err != nil || stored == nil {
		stored = artifact
	}
	ticket, expires := s.issueChatTicket(r, stored.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": stored.ID, "format": stored.Format, "url": "/admin/chat-artifact/" + stored.ID,
		"ticket": ticket, "expires_at": expires,
	})
}

// handleAdminChatTicket issues a fresh ticket for an existing artifact, so reopening a
// preview after the first ticket expired does not need the payload to be re-uploaded.
func (s *Server) handleAdminChatTicket(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	artifacts := s.chatArtifacts()
	if artifacts == nil {
		writeAPIError(w, domain.ErrUnsupported("the console chat is disabled on this deployment"))
		return
	}
	id := r.PathValue("art")
	if _, err := artifacts.GetChatArtifact(r.Context(), id, user.ID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	ticket, expires := s.issueChatTicket(r, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "url": "/admin/chat-artifact/" + id, "ticket": ticket, "expires_at": expires,
	})
}

// handleAdminChatArtifact serves one preview payload. It is authorized by the ticket, not
// by a cookie: a sandboxed frame without allow-same-origin sends no cookie at all, and
// pretending otherwise would make previews fail intermittently depending on the browser's
// SameSite treatment of an opaque origin.
func (s *Server) handleAdminChatArtifact(w http.ResponseWriter, r *http.Request) {
	artifacts := s.chatArtifacts()
	if artifacts == nil || s.chatSigner == nil || !s.chatSigner.ready() {
		writeAPIError(w, domain.ErrUnsupported("previews are unavailable on this deployment"))
		return
	}
	id := r.PathValue("id")
	adminSession, ok := s.chatSigner.verify(r.URL.Query().Get("ticket"), id, time.Now().UTC())
	if !ok {
		// No hint about whether the artifact exists: the ticket check comes first.
		writeAPIError(w, domain.ErrNotFound("this preview link has expired"))
		return
	}
	// The ticket names the administrator session it was issued to; that session must still
	// be valid, so logging out revokes outstanding previews immediately.
	ownerID, err := artifacts.AdminSessionUser(r.Context(), adminSession)
	if err != nil {
		writeAPIError(w, domain.ErrNotFound("this preview link has expired"))
		return
	}
	artifact, err := artifacts.GetChatArtifact(r.Context(), id, ownerID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	if artifact.Format == domain.ChatArtifactSVG {
		header.Set("Content-Type", "image/svg+xml; charset=utf-8")
	}
	header.Set("Content-Security-Policy", s.chatArtifactCSP(artifact.Format))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(artifact.Body))
}

// chatArtifactCSP is the sandbox a previewed page lives in.
//
// `sandbox allow-scripts` (without allow-same-origin) keeps the document in an opaque
// origin even when someone opens the URL directly in a tab, so it cannot read cookies, use
// the console's storage or call the management API with the operator's credentials.
// `default-src 'none'` blocks every external resource; `connect-src 'none'` blocks fetch
// and WebSocket. What this does *not* claim to do is stop every possible navigation — a
// nested frame can still navigate itself — which is why the console says "external
// resources are blocked by default" rather than "this page has no network".
func (s *Server) chatArtifactCSP(format string) string {
	allowNetwork := s.deps.Config != nil && s.deps.Config.Chat.ArtifactAllowNetwork
	script := "'unsafe-inline'"
	style := "'unsafe-inline'"
	img := "data: blob:"
	font := "data:"
	media := "data: blob:"
	connect := "'none'"
	if allowNetwork {
		script += " https:"
		style += " https:"
		img += " https:"
		font += " https:"
		connect = "https:"
	}
	directives := []string{
		"default-src 'none'",
		"img-src " + img,
		"media-src " + media,
		"font-src " + font,
		"style-src " + style,
		"script-src " + script,
		"connect-src " + connect,
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors 'self'",
	}
	sandbox := "sandbox allow-scripts"
	if format == domain.ChatArtifactSVG {
		// An SVG chart needs no scripting at all, so it gets none.
		sandbox = "sandbox"
	}
	return sandbox + "; " + strings.Join(directives, "; ")
}

// issueChatTicket signs a fresh ticket for one artifact, bound to the administrator session
// that is asking for it.
func (s *Server) issueChatTicket(r *http.Request, artifactID string) (string, time.Time) {
	ttl := 5 * time.Minute
	if s.deps.Config != nil && s.deps.Config.Chat.ArtifactTicketTTL > 0 {
		ttl = s.deps.Config.Chat.ArtifactTicketTTL
	}
	expires := time.Now().UTC().Add(ttl)
	sessionID := adminSessionFromRequest(r)
	return s.chatSigner.sign(artifactID, sessionID, expires), expires
}

// adminSessionFromRequest reads the console session id out of the request's cookie. The
// ticket only needs to name the session (the signature makes it unforgeable); the token half
// of the cookie is verified by adminActor before any of this runs.
func adminSessionFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	id, _, ok := admin.ParseCookie(cookie.Value)
	if !ok {
		return ""
	}
	return id
}

func (s *Server) chatArtifactLimit() int {
	if s.deps.Config != nil && s.deps.Config.Chat.ArtifactMaxBytes > 0 {
		return s.deps.Config.Chat.ArtifactMaxBytes
	}
	return 256 * 1024
}

func (s *Server) chatArtifactKeep() int {
	if s.deps.Config != nil && s.deps.Config.Chat.ArtifactMaxPerSession > 0 {
		return s.deps.Config.Chat.ArtifactMaxPerSession
	}
	return 50
}

func truncateForTitle(value string) string {
	trimmed := strings.TrimSpace(value)
	if len([]rune(trimmed)) <= 80 {
		return trimmed
	}
	return string([]rune(trimmed)[:80])
}
