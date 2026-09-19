// Package feishu holds aigw's Feishu (Lark) identity integration: the OAuth code flow
// against a self-built application, the signed state that carries an authorization
// attempt through the browser, and the short-lived ticket that hands a signed-in identity
// to the DSH gateway.
//
// Shape: one application, one registered redirect URL, and four flows that share it —
// an administrator binding an API key to a Feishu account, a person signing in to the
// DSH portal, an administrator signing in to the console, and an invited administrator
// binding their identity to their account. See docs/feishu.md,
// docs/design/m60-aigw-key-feishu-binding.md and
// docs/design/m66-console-admin-feishu-login.md.
//
// Nothing here touches the store or the HTTP transport: the package is a client and a
// pair of codecs, which is what makes the whole flow testable against a local stub.
package feishu

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Flow is the purpose of one authorization attempt. It travels inside the signed state so
// a single callback can serve both.
type Flow string

const (
	// FlowBind binds a Feishu identity to an API key. It is started by an administrator
	// from the console.
	FlowBind Flow = "bind"
	// FlowDSHLogin signs a person in to the DSH portal. It needs no session: the signed
	// state is the capability.
	FlowDSHLogin Flow = "dsh"
	// FlowAdminLogin signs an administrator in to the management console. Like the portal
	// login it needs no session, and it carries no target: the identity that comes back
	// decides which administrator it is (M66).
	FlowAdminLogin Flow = "admin"
	// FlowAdminInvite binds a Feishu identity to an administrator account that was created
	// without a password (M66). It is the only flow that carries both a target account and
	// an Invite handle, and the handle is what makes an outstanding invitation revocable:
	// the account row keeps the newest handle, so regenerating the link retires the old one.
	FlowAdminInvite Flow = "admin_invite"
)

// State is one authorization attempt. It is signed rather than stored so that a gateway
// restart cannot strand a browser that is sitting on Feishu's consent page, and it is
// single-use so a leaked URL is worth one attempt at most.
type State struct {
	Version int    `json:"v"`
	Flow    Flow   `json:"flow"`
	Nonce   string `json:"nonce"`
	Expires int64  `json:"exp"`
	// KeyID is the key a binding targets; zero for the other flows.
	KeyID int64 `json:"key_id,omitempty"`
	// AdminUserID is the administrator account an invitation targets; zero for the other
	// flows.
	AdminUserID int64 `json:"admin_user_id,omitempty"`
	// Actor is the administrator who started a binding or an invitation, re-checked at the
	// callback: a demoted or deleted operator must not be able to finish what they started.
	Actor string `json:"actor,omitempty"`
	// Invite is the invitation handle the target account must still hold at redemption.
	// Regenerating an invitation replaces that handle, which is what retires the old link.
	Invite string `json:"invite,omitempty"`
}

// Attempt is one authorization attempt as the caller describes it. It is a struct rather
// than a parameter list because the flows now differ in what they must carry — a key, an
// administrator account, an invitation handle — and positional arguments would let a
// caller satisfy the wrong flow's rule by accident.
type Attempt struct {
	Flow        Flow
	KeyID       int64
	AdminUserID int64
	Actor       string
	Nonce       string
	Invite      string
}

// validate reports whether the attempt carries everything its flow needs. It is a
// programming error when it does not, so Sign refuses rather than minting a state that the
// callback would have to reject.
func (a Attempt) validate() error {
	switch a.Flow {
	case FlowBind:
		if a.KeyID == 0 {
			return errors.New("feishu: a binding state needs a key")
		}
	case FlowDSHLogin, FlowAdminLogin:
	case FlowAdminInvite:
		if a.AdminUserID == 0 {
			return errors.New("feishu: an invitation state needs an administrator account")
		}
		if strings.TrimSpace(a.Invite) == "" {
			return errors.New("feishu: an invitation state needs an invitation handle")
		}
	default:
		return fmt.Errorf("feishu: unknown flow %q", a.Flow)
	}
	if strings.TrimSpace(a.Nonce) == "" {
		return errors.New("feishu: state nonce must not be empty")
	}
	return nil
}

// StateError describes why a state was refused, in words that are safe to log.
type StateError struct {
	Reason string
}

func (e *StateError) Error() string { return "feishu state: " + e.Reason }

func stateErr(reason string) error { return &StateError{Reason: reason} }

// usedNonceTTL is how long a consumed nonce is remembered. It only has to outlive the
// state itself, because an expired state is refused regardless of its nonce.
const maxUsedNonces = 4096

// StateCodec signs and verifies authorization attempts.
type StateCodec struct {
	Key []byte
	TTL time.Duration
	Now func() time.Time

	mu   sync.Mutex
	used map[string]time.Time
}

// NewStateCodec builds a codec over a signing key. An empty key is a programming error:
// a codec that cannot sign would silently accept anything.
func NewStateCodec(key []byte, ttl time.Duration) (*StateCodec, error) {
	if len(key) == 0 {
		return nil, errors.New("feishu: state signing key must not be empty")
	}
	if ttl <= 0 {
		return nil, errors.New("feishu: state ttl must be positive")
	}
	return &StateCodec{Key: append([]byte(nil), key...), TTL: ttl, used: map[string]time.Time{}}, nil
}

func (c *StateCodec) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// Sign returns the wire form of a fresh state for one attempt. A state may not outlive the
// codec's TTL, so the caller that wants a longer-lived value (an invitation link, say) uses
// a codec built with that TTL rather than stretching this one.
func (c *StateCodec) Sign(attempt Attempt) (string, error) {
	if err := attempt.validate(); err != nil {
		return "", err
	}
	state := State{
		Version: 1, Flow: attempt.Flow, Nonce: attempt.Nonce,
		Expires: c.now().Add(c.TTL).Unix(),
		KeyID:   attempt.KeyID, AdminUserID: attempt.AdminUserID,
		Actor: attempt.Actor, Invite: attempt.Invite,
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + c.sign(encoded), nil
}

// Verify checks a state's signature, expiry and nonce, and marks the nonce used. A state
// that fails any check is refused, and nothing else about it is trusted afterwards.
func (c *StateCodec) Verify(raw string) (State, error) {
	return c.verify(raw, true)
}

// Peek performs every check Verify does but does not mark the nonce used, so the same value
// can still be redeemed afterwards. It exists for the one caller that must inspect a state
// before deciding whether to spend it: the invitation entry point, where a link stays usable
// until the invitation is actually redeemed (docs/design/m66-console-admin-feishu-login.md
// §D3). Everything else uses Verify.
func (c *StateCodec) Peek(raw string) (State, error) {
	return c.verify(raw, false)
}

func (c *StateCodec) verify(raw string, consume bool) (State, error) {
	var zero State
	encoded, signature, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok || encoded == "" || signature == "" {
		return zero, stateErr("malformed")
	}
	if !hmac.Equal([]byte(signature), []byte(c.sign(encoded))) {
		return zero, stateErr("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return zero, stateErr("malformed payload")
	}
	var state State
	if err := json.Unmarshal(payload, &state); err != nil {
		return zero, stateErr("malformed payload")
	}
	if state.Version != 1 {
		return zero, stateErr("unsupported version")
	}
	if err := checkFlowShape(state); err != nil {
		return zero, err
	}
	if strings.TrimSpace(state.Nonce) == "" {
		return zero, stateErr("missing nonce")
	}
	now := c.now()
	expires := time.Unix(state.Expires, 0).UTC()
	if !expires.After(now) {
		return zero, stateErr("expired")
	}
	// A state may not carry an arbitrarily distant expiry: the window is the configured
	// TTL plus a minute of clock skew, so a forged-but-signed long-lived state (from a
	// previous key, say) cannot be used as a permanent capability.
	if expires.After(now.Add(c.TTL + time.Minute)) {
		return zero, stateErr("expiry beyond the configured window")
	}
	if consume && !c.reserve(state.Nonce, now, true) {
		return zero, stateErr("replayed")
	}
	if !consume && !c.reserve(state.Nonce, now, false) {
		// Peeking does not spend the state, but it still refuses one that was already spent:
		// "is this link still good?" must not answer yes for a link that has been redeemed.
		return zero, stateErr("replayed")
	}
	return state, nil
}

// checkFlowShape enforces what each flow must carry. A state that names a flow but does not
// carry that flow's target is refused rather than handed to a handler that would have to
// guess what it meant.
func checkFlowShape(state State) error {
	switch state.Flow {
	case FlowBind:
		if state.KeyID == 0 {
			return stateErr("binding state without a key")
		}
	case FlowDSHLogin, FlowAdminLogin:
	case FlowAdminInvite:
		if state.AdminUserID == 0 {
			return stateErr("invitation state without an administrator account")
		}
		if strings.TrimSpace(state.Invite) == "" {
			return stateErr("invitation state without an invitation handle")
		}
	default:
		return stateErr("unknown flow")
	}
	return nil
}

// reserve reports whether a nonce is fresh, and records it when record is set. Peeking asks
// the same question without recording the answer, so a state inspected by an entry point is
// still redeemable afterwards (M66's invitation links rely on exactly that).
func (c *StateCodec) reserve(nonce string, now time.Time, record bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used == nil {
		c.used = map[string]time.Time{}
	}
	if _, seen := c.used[nonce]; seen {
		return false
	}
	if !record {
		return true
	}
	for key, at := range c.used {
		if now.Sub(at) >= c.TTL+time.Minute {
			delete(c.used, key)
		}
	}
	if len(c.used) >= maxUsedNonces {
		// The map is full of live nonces. Dropping the oldest keeps memory bounded; the
		// alternative — refusing every new attempt — turns a burst into an outage.
		oldest, oldestAt := "", now
		for key, at := range c.used {
			if at.Before(oldestAt) {
				oldest, oldestAt = key, at
			}
		}
		delete(c.used, oldest)
	}
	c.used[nonce] = now
	return true
}

func (c *StateCodec) sign(encoded string) string {
	mac := hmac.New(sha256.New, c.Key)
	mac.Write([]byte("feishu-state:" + encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// DeriveSecret turns one piece of configured key material into a purpose-bound key, so a
// value reused as a signing key somewhere else does not sign the same bytes here.
func DeriveSecret(material, purpose string) []byte {
	mac := hmac.New(sha256.New, []byte(material))
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}
