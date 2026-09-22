package feishu

import (
	"strings"
	"testing"
	"time"
)

func consoleCodec(t *testing.T) (*TicketCodec, *time.Time) {
	t.Helper()
	codec, err := NewTicketCodec([]byte("ticket-key"), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	codec.Now = func() time.Time { return now }
	return codec, &now
}

// A console ticket names one administrator and nothing else: the tenant and key fields the
// DSH ticket needs stay empty, which is also what keeps the shared vectors byte-identical.
func TestConsoleTicketRoundTrip(t *testing.T) {
	codec, _ := consoleCodec(t)
	wire, ticket, err := codec.IssueConsole(7, "ou_admin", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Mode != TicketModeConsole || ticket.AdminUserID != 7 || ticket.OpenID != "ou_admin" {
		t.Fatalf("issued ticket = %+v", ticket)
	}
	if ticket.Tenant != "" || ticket.KeyID != 0 {
		t.Fatalf("a console ticket carries tenant/key fields: %+v", ticket)
	}
	// The wire form must not carry an admin id for DSH tickets, which is what lets the
	// child keep verifying the very same vectors it verified before M66.
	if !strings.Contains(wire, ".") {
		t.Fatalf("wire = %q", wire)
	}
	verified, err := codec.VerifyConsoleTicket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if verified.AdminUserID != 7 || verified.OpenID != "ou_admin" || verified.Nonce != "nonce-1" {
		t.Fatalf("verified ticket = %+v", verified)
	}
}

// The two modes are separate capabilities: each verifier refuses the other's tickets even
// though both are signed with the same key.
func TestTicketModesDoNotCross(t *testing.T) {
	codec, _ := consoleCodec(t)
	dsh, _, err := codec.Issue("dsh-tenant", 3, 9, "ou_person", "nonce-dsh")
	if err != nil {
		t.Fatal(err)
	}
	console, _, err := codec.IssueConsole(7, "ou_admin", "nonce-console")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.VerifyConsoleTicket(dsh); !isReason(err, "unknown mode") {
		t.Fatalf("a DSH ticket was accepted by the console verifier: %v", err)
	}
	if _, err := codec.VerifyTicket(console); !isReason(err, "unknown mode") {
		t.Fatalf("a console ticket was accepted by the DSH verifier: %v", err)
	}
}

func TestConsoleTicketRefusesBadInput(t *testing.T) {
	codec, now := consoleCodec(t)
	if _, _, err := codec.IssueConsole(0, "ou_admin", "nonce"); err == nil {
		t.Error("a console ticket without an administrator was issued")
	}
	if _, _, err := codec.IssueConsole(7, "  ", "nonce"); err == nil {
		t.Error("a console ticket without an identity was issued")
	}
	if _, _, err := codec.IssueConsole(7, "ou_admin", ""); err == nil {
		t.Error("a console ticket without a nonce was issued")
	}

	wire, _, err := codec.IssueConsole(7, "ou_admin", "nonce-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.VerifyConsoleTicket(wire + "x"); err == nil {
		t.Error("a tampered console ticket was accepted")
	}
	// Unmarshalable-but-signed payloads are refused on the shape, not only on the signature.
	signed := signRawTicket(t, codec, Ticket{
		Version: 1, Mode: TicketModeConsole, OpenID: "ou_admin", Nonce: "n",
		Expires: codec.now().Add(time.Minute).Unix(),
	})
	if _, err := codec.VerifyConsoleTicket(signed); !isReason(err, "console ticket without an administrator") {
		t.Fatalf("a console ticket without an administrator verified: %v", err)
	}

	// Expiry is judged against the injected clock, like every other codec here.
	*now = now.Add(3 * time.Minute)
	if _, err := codec.VerifyConsoleTicket(wire); !isReason(err, "expired") {
		t.Fatalf("an expired console ticket verified: %v", err)
	}
}

// The consumed set is what makes a ticket a single-use capability; losing it to a restart
// reopens a window bounded by the TTL, which is why the TTL is short.
func TestConsumedTicketsRejectReplay(t *testing.T) {
	consumed := &ConsumedTickets{}
	if !consumed.Consume("nonce-1") {
		t.Fatal("the first redemption was refused")
	}
	if consumed.Consume("nonce-1") {
		t.Fatal("the same ticket was redeemed twice")
	}
	if !consumed.Consume("nonce-2") {
		t.Fatal("a different ticket was refused")
	}
	if consumed.Consume("  ") {
		t.Fatal("an empty ticket was redeemed")
	}
}

// signRawTicket signs a payload the codec would never mint, so the verification-side shape
// checks can be exercised through a valid signature.
func signRawTicket(t *testing.T, codec *TicketCodec, ticket Ticket) string {
	t.Helper()
	wire, err := TicketWire(codec.Key, ticket)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// M72: the gateway signs its own key-pick ticket for a key login, and aigw's codec has to accept
// it — the two sides share one secret and one wire format, so a divergence here would show up as
// "unknown mode" at a person's login. The shared vectors cover the format; this covers the minting
// side that has no vector (aigw never mints this ticket in production).
func TestKeyPickTicketRoundTrip(t *testing.T) {
	codec, now := consoleCodec(t)
	wire, ticket, err := codec.IssueKeyPick(42, "ou_alice", "nonce-pick-1")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Mode != TicketModeKeyPick || ticket.AccountID != 42 || ticket.OpenID != "ou_alice" {
		t.Fatalf("issued ticket = %+v", ticket)
	}
	if ticket.Tenant != "" || ticket.KeyID != 0 || ticket.AdminUserID != 0 {
		t.Fatalf("a key-pick ticket carries another flow's fields: %+v", ticket)
	}
	verified, err := codec.VerifyKeyPick(wire)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.AccountID != 42 || verified.Expires != now.Add(2*time.Minute).Unix() {
		t.Fatalf("verified ticket = %+v", verified)
	}
	// A key-pick ticket is not a DSH ticket, and the other way round.
	if _, err := codec.VerifyTicket(wire); err == nil {
		t.Fatal("a key-pick ticket was accepted as a DSH ticket")
	}
	if _, _, err := codec.IssueKeyPick(0, "ou_alice", "nonce-pick-2"); err == nil {
		t.Fatal("a key-pick ticket without an account was issued")
	}
}
