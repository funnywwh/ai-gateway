package feishu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vectorFile is the shared contract with aigw's independent implementation: aigw signs
// (internal/feishu/ticket.go) and this package verifies. Both tests read the same file, so a
// change on one side that the other cannot reproduce fails here instead of at a user's login.
const vectorFile = "../contract/testdata/feishu_ticket_vectors.json"

type ticketVector struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	Now    string `json:"now"`
	Wire   string `json:"wire"`
}

func loadVectors(t *testing.T) []ticketVector {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(vectorFile))
	if err != nil {
		t.Fatalf("read shared ticket vectors: %v", err)
	}
	var doc struct {
		Comment string         `json:"_comment"`
		Vectors []ticketVector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode shared ticket vectors: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("shared ticket vectors are empty")
	}
	if doc.Comment == "" {
		t.Error("the vector file must say how to regenerate it")
	}
	return doc.Vectors
}

// Every vector the signer accepts must verify here, and every one it refuses must be refused
// with the same reason — that is what keeps two hand-written implementations honest.
func TestSharedTicketVectors(t *testing.T) {
	wantRejected := map[string]string{
		"expired":       "expired",
		"future-expiry": "expiry beyond the accepted window",
		"unknown-mode":  "unknown mode",
		"wrong-version": "unsupported version",
		"empty-tenant":  "incomplete ticket",
		"other-key":     "bad signature",
	}
	accepted := 0
	for _, vector := range loadVectors(t) {
		now, err := time.Parse(time.RFC3339, vector.Now)
		if err != nil {
			t.Fatalf("%s: bad now: %v", vector.Name, err)
		}
		verifier, err := New([]byte(vector.Secret))
		if err != nil {
			t.Fatalf("%s: %v", vector.Name, err)
		}
		verifier.Now = func() time.Time { return now }

		ticket, verifyErr := verifier.Verify(vector.Wire)
		if reason, rejected := wantRejected[vector.Name]; rejected {
			var refusal *Error
			if verifyErr == nil {
				t.Errorf("%s: accepted a ticket that must be refused", vector.Name)
				continue
			}
			if !asError(verifyErr, &refusal) || refusal.Reason != reason {
				t.Errorf("%s: refused with %v, want reason %q", vector.Name, verifyErr, reason)
			}
			continue
		}
		if verifyErr != nil {
			t.Errorf("%s: a valid vector was refused: %v", vector.Name, verifyErr)
			continue
		}
		// The payload must survive the trip with every field intact: the portal forwards the
		// tenant, and the audit records the identity.
		if ticket.Tenant == "" || ticket.OpenID == "" || ticket.Mode != "dsh" || ticket.Version != 1 {
			t.Errorf("%s: decoded ticket is incomplete: %+v", vector.Name, ticket)
		}
		accepted++
	}
	if accepted == 0 {
		t.Fatal("no valid vector was exercised")
	}
}

// A ticket is single use: the same one twice must fail the second time, and the failure must be
// distinguishable from a broken signature.
func TestTicketIsSingleUse(t *testing.T) {
	vector := loadVectors(t)[0]
	now, err := time.Parse(time.RFC3339, vector.Now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := New([]byte(vector.Secret))
	if err != nil {
		t.Fatal(err)
	}
	verifier.Now = func() time.Time { return now }
	if _, err := verifier.Verify(vector.Wire); err != nil {
		t.Fatalf("first redemption failed: %v", err)
	}
	var refusal *Error
	if _, err := verifier.Verify(vector.Wire); !asError(err, &refusal) || refusal.Reason != "already used" {
		t.Fatalf("second redemption = %v, want an already-used refusal", err)
	}
	// A second browser with its own ticket is of course still served.
	fresh := strings.Replace(vector.Wire, "0", "1", 1)
	if _, err := verifier.Verify(fresh); err == nil {
		t.Fatal("a tampered ticket was accepted")
	}
}

func TestMalformedTicketsAreRefused(t *testing.T) {
	verifier, err := New([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	verifier.Now = func() time.Time { return time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC) }
	cases := map[string]string{
		"empty":           "",
		"blank":           "   ",
		"no separator":    "abcdef",
		"empty payload":   ".signature",
		"empty signature": "payload.",
		"garbage":         "!!!.???",
	}
	for name, wire := range cases {
		var refusal *Error
		if _, err := verifier.Verify(wire); !asError(err, &refusal) {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(nil); err == nil {
		t.Error("a verifier without a secret was created")
	}
	// A disabled verifier refuses everything rather than opening the door.
	var disabled *Verifier
	if _, err := disabled.Verify("anything"); err == nil {
		t.Error("a nil verifier accepted a ticket")
	}
	if disabled.Enabled() {
		t.Error("a nil verifier reports itself enabled")
	}
}

// The consumed set is bounded, so a burst of logins cannot grow it without limit.
func TestConsumedSetIsBounded(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	verifier, err := New([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	verifier.Now = func() time.Time { return now }
	for i := 0; i < maxConsumed+20; i++ {
		nonce := "nonce-" + itoa(i)
		if !verifier.consume(nonce, now) {
			t.Fatalf("nonce %d was refused as a replay", i)
		}
		now = now.Add(time.Millisecond)
	}
	if len(verifier.consumed) > maxConsumed {
		t.Fatalf("consumed set grew to %d, want at most %d", len(verifier.consumed), maxConsumed)
	}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			if target != nil {
				*target = e
			}
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
