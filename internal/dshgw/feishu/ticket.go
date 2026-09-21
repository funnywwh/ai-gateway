// Package feishu verifies the short-lived ticket aigw hands to this gateway after a person
// has proved their Feishu identity (M61).
//
// Why a ticket instead of its own OAuth client: the Feishu application, its secret and the
// registered redirect URL all belong to aigw, so that there is exactly one place to configure
// Feishu and exactly one callback URL to register. This package therefore never talks to
// Feishu at all — it only answers "aigw says this browser is this person, for this tenant".
//
// The signature, payload and header are a contract with an independent implementation in
// aigw (internal/feishu/ticket.go). The two must agree byte for byte, which is enforced by
// the shared vectors in internal/dshgw/contract/testdata/feishu_ticket_vectors.json read by
// both sides' tests — neither package imports the other (see internal/arch).
package feishu

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Ticket is one redeemed handoff.
type Ticket struct {
	Version   int    `json:"v"`
	Mode      string `json:"mode"`
	Tenant    string `json:"tenant"`
	KeyID     int64  `json:"key_id"`
	AccountID int64  `json:"account_id"`
	OpenID    string `json:"open_id"`
	Nonce     string `json:"nonce"`
	Expires   int64  `json:"exp"`
}

// maxTicketTTL bounds the expiry this side accepts, independently of what the issuer thinks
// its TTL is. A ticket is meant to cover one browser redirect.
const maxTicketTTL = 10 * time.Minute

// maxConsumed bounds the single-use set; entries are pruned by age, so a burst cannot grow it
// past this without dropping the oldest.
const maxConsumed = 4096

// Reason vocabulary for a refused ticket. It is small and stable because the portal turns it
// into a message for a person: "start again" versus "ask an administrator".
const (
	reasonMalformed   = "malformed"
	reasonSignature   = "bad signature"
	reasonVersion     = "unsupported version"
	reasonMode        = "unknown mode"
	reasonIncomplete  = "incomplete ticket"
	reasonExpired     = "expired"
	reasonFuture      = "expiry beyond the accepted window"
	reasonReplayed    = "already used"
	reasonUnavailable = "unavailable"
)

// Error is one refusal. Reason is safe to log and to translate; nothing else about the ticket
// is echoed.
type Error struct {
	Reason string
}

func (e *Error) Error() string { return "feishu ticket: " + e.Reason }

func ticketErr(reason string) error { return &Error{Reason: reason} }

// Verifier checks tickets and remembers which ones it has already redeemed.
type Verifier struct {
	key []byte
	// Now is injectable for tests.
	Now func() time.Time

	consumed map[string]time.Time
}

// New builds a verifier over the shared secret. An empty key is refused rather than accepted:
// a verifier that cannot verify would let anyone in.
func New(key []byte) (*Verifier, error) {
	if len(key) == 0 {
		return nil, errors.New("feishu: ticket secret must not be empty")
	}
	return &Verifier{key: append([]byte(nil), key...), consumed: map[string]time.Time{}}, nil
}

// Enabled reports whether a verifier is usable.
func (v *Verifier) Enabled() bool { return v != nil && len(v.key) > 0 }

func (v *Verifier) now() time.Time {
	if v != nil && v.Now != nil {
		return v.Now().UTC()
	}
	return time.Now().UTC()
}

// modeDSH and modeKeyPick are the ticket modes this verifier accepts. Each entry point below
// accepts exactly one of them, so a ticket minted for one purpose can never be redeemed for
// the other (M72 added the second mode for the multi-key selection step).
const (
	modeDSH     = "dsh"
	modeKeyPick = "keypick"
)

// Verify checks a DSH ticket's signature, version, mode, expiry and single use, and consumes
// its nonce on success.
func (v *Verifier) Verify(raw string) (Ticket, error) {
	return v.verify(raw, modeDSH)
}

// VerifyPick checks a key-pick ticket (M72): the step between "who you are" and a session,
// where the gateway asks which of the account's keys this login should be recorded against.
// It carries an account and no tenant — the picker runs before a tenant is entered — and it is
// consumed exactly like a DSH ticket, so a picker link is worth one submission.
func (v *Verifier) VerifyPick(raw string) (Ticket, error) {
	return v.verify(raw, modeKeyPick)
}

func (v *Verifier) verify(raw, mode string) (Ticket, error) {
	var zero Ticket
	if !v.Enabled() {
		return zero, ticketErr(reasonUnavailable)
	}
	encoded, signature, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok || encoded == "" || signature == "" {
		return zero, ticketErr(reasonMalformed)
	}
	if !hmac.Equal([]byte(signature), []byte(v.signature(encoded))) {
		return zero, ticketErr(reasonSignature)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return zero, ticketErr(reasonMalformed)
	}
	var ticket Ticket
	if err := json.Unmarshal(payload, &ticket); err != nil {
		return zero, ticketErr(reasonMalformed)
	}
	if ticket.Version != 1 {
		return zero, ticketErr(reasonVersion)
	}
	if ticket.Mode != mode {
		return zero, ticketErr(reasonMode)
	}
	if strings.TrimSpace(ticket.OpenID) == "" || strings.TrimSpace(ticket.Nonce) == "" {
		return zero, ticketErr(reasonIncomplete)
	}
	// Each mode must carry the one thing it is for: a DSH ticket is useless without a tenant,
	// a key-pick ticket without the account whose keys are being chosen between.
	if mode == modeDSH && strings.TrimSpace(ticket.Tenant) == "" {
		return zero, ticketErr(reasonIncomplete)
	}
	if mode == modeKeyPick && ticket.AccountID == 0 {
		return zero, ticketErr(reasonIncomplete)
	}
	now := v.now()
	expires := time.Unix(ticket.Expires, 0).UTC()
	if !expires.After(now) {
		return zero, ticketErr(reasonExpired)
	}
	if expires.After(now.Add(maxTicketTTL)) {
		return zero, ticketErr(reasonFuture)
	}
	if !v.consume(ticket.Nonce, now) {
		return zero, ticketErr(reasonReplayed)
	}
	return ticket, nil
}

func (v *Verifier) signature(encoded string) string {
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte("feishu-ticket:" + encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// consume records a nonce and reports whether it was fresh.
func (v *Verifier) consume(nonce string, now time.Time) bool {
	if v.consumed == nil {
		v.consumed = map[string]time.Time{}
	}
	if _, seen := v.consumed[nonce]; seen {
		return false
	}
	for key, at := range v.consumed {
		if now.Sub(at) >= maxTicketTTL {
			delete(v.consumed, key)
		}
	}
	if len(v.consumed) >= maxConsumed {
		oldest, oldestAt := "", now
		for key, at := range v.consumed {
			if at.Before(oldestAt) {
				oldest, oldestAt = key, at
			}
		}
		delete(v.consumed, oldest)
	}
	v.consumed[nonce] = now
	return true
}
