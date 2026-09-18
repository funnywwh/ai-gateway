// Package feishu holds aigw's Feishu (Lark) identity integration: the OAuth code flow
// against a self-built application, the signed state that carries an authorization
// attempt through the browser, and the short-lived ticket that hands a signed-in identity
// to the DSH gateway.
//
// Shape: one application, one registered redirect URL, and two flows that share it —
// an administrator binding an API key to a Feishu account, and a person signing in to the
// DSH portal. See docs/feishu.md and docs/design/m60-aigw-key-feishu-binding.md.
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
)

// State is one authorization attempt. It is signed rather than stored so that a gateway
// restart cannot strand a browser that is sitting on Feishu's consent page, and it is
// single-use so a leaked URL is worth one attempt at most.
type State struct {
	Version int    `json:"v"`
	Flow    Flow   `json:"flow"`
	Nonce   string `json:"nonce"`
	Expires int64  `json:"exp"`
	// KeyID is the key a binding targets; zero for a login flow.
	KeyID int64 `json:"key_id,omitempty"`
	// Actor is the administrator who started a binding, re-checked at the callback: a
	// demoted or deleted operator must not be able to finish a binding they started.
	Actor string `json:"actor,omitempty"`
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

// Sign returns the wire form of a fresh state for one flow.
func (c *StateCodec) Sign(flow Flow, keyID int64, actor, nonce string) (string, error) {
	if flow != FlowBind && flow != FlowDSHLogin {
		return "", fmt.Errorf("feishu: unknown flow %q", flow)
	}
	if strings.TrimSpace(nonce) == "" {
		return "", errors.New("feishu: state nonce must not be empty")
	}
	if flow == FlowBind && keyID == 0 {
		return "", errors.New("feishu: a binding state needs a key")
	}
	state := State{Version: 1, Flow: flow, Nonce: nonce, Expires: c.now().Add(c.TTL).Unix(), KeyID: keyID, Actor: actor}
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
	if state.Flow != FlowBind && state.Flow != FlowDSHLogin {
		return zero, stateErr("unknown flow")
	}
	if strings.TrimSpace(state.Nonce) == "" {
		return zero, stateErr("missing nonce")
	}
	if state.Flow == FlowBind && state.KeyID == 0 {
		return zero, stateErr("binding state without a key")
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
	if !c.consume(state.Nonce, now) {
		return zero, stateErr("replayed")
	}
	return state, nil
}

// consume records a nonce and reports whether it was fresh. The set is bounded and pruned
// on insert: a state only lives for one TTL, so anything older than that can be forgotten.
func (c *StateCodec) consume(nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used == nil {
		c.used = map[string]time.Time{}
	}
	if _, seen := c.used[nonce]; seen {
		return false
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
