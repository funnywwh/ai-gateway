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

// Ticket is what aigw hands to the DSH gateway after it has asked Feishu who the person
// is. It is not a session: it is the answer to "this browser just proved this identity,
// and that identity belongs to this tenant", valid for one redirect.
//
// The DSH gateway verifies it with its own copy of the same code (internal/dshgw/feishu):
// the two implementations must agree bit for bit, which is what the shared test vectors in
// internal/dshgw/contract/testdata/feishu_ticket_vectors.json exist to enforce. aigw does
// not import dshgw's packages and dshgw does not import aigw's.
type Ticket struct {
	Version   int    `json:"v"`
	Mode      string `json:"mode"`
	Tenant    string `json:"tenant"`
	KeyID     int64  `json:"key_id"`
	AccountID int64  `json:"account_id"`
	OpenID    string `json:"open_id"`
	Nonce     string `json:"nonce"`
	Expires   int64  `json:"exp"`
	// AdminUserID is the console administrator a console ticket names. It is omitted for a
	// DSH ticket, so the shared test vectors and the child's verifier see exactly the bytes
	// they saw before this field existed (M66).
	AdminUserID int64 `json:"admin_user_id,omitempty"`
}

// Ticket modes. The mode lives inside the signed payload, and each verifier accepts exactly
// one of them, so a ticket minted for one purpose cannot be redeemed for the other.
const (
	// TicketMode is a DSH portal login: aigw mints it, the dshgw child redeems it.
	TicketMode = "dsh"
	// TicketModeConsole is a console administrator login (M66): aigw mints it on its
	// registered callback origin and redeems it on whatever origin the operator's console is
	// served from. It exists because a session cookie is scoped to a host name: when the
	// console and the Feishu callback are reached under different names, the callback can
	// prove who somebody is but cannot hand that browser a cookie for the console's host.
	// A one-time ticket in the URL can.
	TicketModeConsole = "console"
)

// MaxTicketTTL bounds a ticket's lifetime from the verifier's side. It is deliberately
// larger than any sane TTL so that a clock-skewed or forged-but-signed far-future expiry
// can still be refused.
const MaxTicketTTL = 10 * time.Minute

// ErrTicketInvalid is returned for every refusal. Callers distinguish the cases through
// Reason so an operator can see why, while the browser only ever gets "please retry".
var ErrTicketInvalid = errors.New("feishu ticket: invalid")

// TicketError carries a machine-readable reason for one refusal.
type TicketError struct {
	Reason string
}

func (e *TicketError) Error() string { return "feishu ticket: " + e.Reason }
func (e *TicketError) Unwrap() error { return ErrTicketInvalid }

func ticketErr(reason string) error { return &TicketError{Reason: reason} }

// TicketCodec signs and verifies tickets.
type TicketCodec struct {
	Key []byte
	TTL time.Duration
	Now func() time.Time
}

// NewTicketCodec builds a codec over a signing key.
func NewTicketCodec(key []byte, ttl time.Duration) (*TicketCodec, error) {
	if len(key) == 0 {
		return nil, errors.New("feishu: ticket signing key must not be empty")
	}
	if ttl <= 0 || ttl > MaxTicketTTL {
		return nil, fmt.Errorf("feishu: ticket ttl must be within (0, %s]", MaxTicketTTL)
	}
	return &TicketCodec{Key: append([]byte(nil), key...), TTL: ttl}, nil
}

func (c *TicketCodec) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// TicketWire encodes and signs a ticket. It is exported so the contract test can build
// vectors from the same code the server uses.
func TicketWire(key []byte, ticket Ticket) (string, error) {
	payload, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + ticketSignature(key, encoded), nil
}

// TicketSignature is the detached signature form, for tests that need it.
func TicketSignature(key []byte, encoded string) string { return ticketSignature(key, encoded) }

func ticketSignature(key []byte, encoded string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("feishu-ticket:" + encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Issue mints a ticket for one identity and tenant.
func (c *TicketCodec) Issue(tenant string, keyID, accountID int64, openID, nonce string) (string, Ticket, error) {
	if strings.TrimSpace(tenant) == "" {
		return "", Ticket{}, errors.New("feishu: a ticket needs a tenant")
	}
	return c.issue(Ticket{
		Version: 1, Mode: TicketMode, Tenant: tenant, KeyID: keyID, AccountID: accountID, OpenID: openID,
	}, nonce)
}

// IssueConsole mints a ticket that lets one browser open the console as one administrator.
//
// It is the console's half of the handoff the DSH portal has had since M61: the callback
// runs on the origin registered with Feishu, and the console may be served under another host
// name, where a cookie set by the callback would never arrive. The ticket is the capability,
// it is redeemed once on the console's own origin, and it is what sets the cookie there.
func (c *TicketCodec) IssueConsole(adminUserID int64, openID, nonce string) (string, Ticket, error) {
	if adminUserID == 0 {
		return "", Ticket{}, errors.New("feishu: a console ticket needs an administrator")
	}
	return c.issue(Ticket{
		Version: 1, Mode: TicketModeConsole, AdminUserID: adminUserID, OpenID: openID,
	}, nonce)
}

// issue fills in the shared checks and signs one ticket.
func (c *TicketCodec) issue(ticket Ticket, nonce string) (string, Ticket, error) {
	if strings.TrimSpace(ticket.OpenID) == "" {
		return "", Ticket{}, errors.New("feishu: a ticket needs an identity")
	}
	if strings.TrimSpace(nonce) == "" {
		return "", Ticket{}, errors.New("feishu: a ticket needs a nonce")
	}
	ticket.Nonce = nonce
	ticket.Expires = c.now().Add(c.TTL).Unix()
	wire, err := TicketWire(c.Key, ticket)
	return wire, ticket, err
}

// VerifyTicket checks a DSH ticket's signature, version, mode and expiry. It deliberately
// does not track single use: the DSH gateway keeps that set, because it is the side that
// consumes the ticket, and a consumed-ticket set on the issuing side would need a callback
// to be authoritative.
func (c *TicketCodec) VerifyTicket(raw string) (Ticket, error) {
	return c.verify(raw, TicketMode)
}

// VerifyConsoleTicket is VerifyTicket for the console mode. The two are separate entry points
// rather than one mode-parameterised call so that neither side can accidentally accept the
// other's tickets: a DSH ticket presented to the console handler fails on the mode check, and
// the child's verifier keeps rejecting anything that is not a DSH ticket.
func (c *TicketCodec) VerifyConsoleTicket(raw string) (Ticket, error) {
	ticket, err := c.verify(raw, TicketModeConsole)
	if err != nil {
		return Ticket{}, err
	}
	if ticket.AdminUserID == 0 {
		return Ticket{}, ticketErr("console ticket without an administrator")
	}
	return ticket, nil
}

func (c *TicketCodec) verify(raw, mode string) (Ticket, error) {
	var zero Ticket
	encoded, signature, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok || encoded == "" || signature == "" {
		return zero, ticketErr("malformed")
	}
	if !hmac.Equal([]byte(signature), []byte(ticketSignature(c.Key, encoded))) {
		return zero, ticketErr("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return zero, ticketErr("malformed payload")
	}
	var ticket Ticket
	if err := json.Unmarshal(payload, &ticket); err != nil {
		return zero, ticketErr("malformed payload")
	}
	if ticket.Version != 1 {
		return zero, ticketErr("unsupported version")
	}
	if ticket.Mode != mode {
		return zero, ticketErr("unknown mode")
	}
	if mode == TicketMode && strings.TrimSpace(ticket.Tenant) == "" {
		return zero, ticketErr("incomplete ticket")
	}
	if strings.TrimSpace(ticket.OpenID) == "" || strings.TrimSpace(ticket.Nonce) == "" {
		return zero, ticketErr("incomplete ticket")
	}
	now := c.now()
	expires := time.Unix(ticket.Expires, 0).UTC()
	if !expires.After(now) {
		return zero, ticketErr("expired")
	}
	if expires.After(now.Add(MaxTicketTTL)) {
		return zero, ticketErr("expiry beyond the accepted window")
	}
	return ticket, nil
}

// ConsumedTickets remembers which ticket nonces a gateway has already redeemed, so a
// replayed link (browser history, a shared URL, a retry) cannot sign in twice. The set is
// bounded and pruned by expiry; losing it to a restart only reopens a window bounded by
// the ticket TTL.
type ConsumedTickets struct {
	Now func() time.Time

	mu   sync.Mutex
	used map[string]time.Time
}

const maxConsumedTickets = 4096

// Consume records a nonce and reports whether it was fresh.
func (c *ConsumedTickets) Consume(nonce string) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used == nil {
		c.used = map[string]time.Time{}
	}
	if _, seen := c.used[nonce]; seen {
		return false
	}
	for key, at := range c.used {
		if now.Sub(at) >= MaxTicketTTL {
			delete(c.used, key)
		}
	}
	if len(c.used) >= maxConsumedTickets {
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
