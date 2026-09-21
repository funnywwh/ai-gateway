package feishu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// vectorFile is the shared contract between the two implementations: aigw signs tickets
// here, the DSH gateway verifies them in internal/dshgw/feishu, and neither imports the
// other. Both read this file, so a change on one side that the other cannot reproduce
// fails a test instead of failing at a user's login.
const vectorFile = "../dshgw/contract/testdata/feishu_ticket_vectors.json"

type ticketVector struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	Now    string `json:"now"`
	Wire   string `json:"wire"`
	// Mode names the verifier entry point this vector belongs to: DSH tickets and key-pick
	// tickets (M72) are separate endpoints on both sides of the contract.
	Mode string `json:"mode"`
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

// The signing side must reproduce every vector byte for byte, and accepting exactly the
// vectors that are meant to be accepted is the other half of that contract.
func TestTicketVectorsAreReproducedAndEnforced(t *testing.T) {
	wantRejected := map[string]string{
		"expired":       "expired",
		"future-expiry": "expiry beyond the accepted window",
		"unknown-mode":  "unknown mode",
		"wrong-version": "unsupported version",
		"empty-tenant":  "incomplete ticket",
		"other-key":     "bad signature",
	}
	seenValid := false
	for _, vector := range loadVectors(t) {
		now, err := time.Parse(time.RFC3339, vector.Now)
		if err != nil {
			t.Fatalf("%s: bad now: %v", vector.Name, err)
		}
		codec, err := NewTicketCodec([]byte(vector.Secret), 120*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", vector.Name, err)
		}
		codec.Now = func() time.Time { return now }

		ticket, verifyErr := acceptVector(codec, vector)
		if reason, rejected := wantRejected[vector.Name]; rejected {
			var ticketErr *TicketError
			if verifyErr == nil {
				t.Errorf("%s: accepted a ticket that must be refused", vector.Name)
				continue
			}
			if !asTicketError(verifyErr, &ticketErr) || ticketErr.Reason != reason {
				t.Errorf("%s: refused with %v, want reason %q", vector.Name, verifyErr, reason)
			}
			continue
		}
		if verifyErr != nil {
			t.Errorf("%s: a valid vector was refused: %v", vector.Name, verifyErr)
			continue
		}
		// Re-encode from the payload and require the identical wire form.
		wire, err := TicketWire([]byte(vector.Secret), ticket)
		if err != nil {
			t.Fatalf("%s: %v", vector.Name, err)
		}
		if wire != vector.Wire {
			t.Errorf("%s: re-encoded ticket differs\n got %s\nwant %s", vector.Name, wire, vector.Wire)
		}
		if ticket.Mode != vector.Mode {
			t.Errorf("%s: the mode must survive signing: %+v", vector.Name, ticket)
		}
		seenValid = true
	}
	if !seenValid {
		t.Fatal("no valid vector was exercised")
	}
}

// acceptVector verifies through the entry point the vector's mode names. "unknown-mode" goes
// through the DSH verifier on purpose: an unrecognized mode must be refused there, not routed
// somewhere more permissive.
func acceptVector(codec *TicketCodec, vector ticketVector) (Ticket, error) {
	if vector.Mode == TicketModeKeyPick {
		return codec.VerifyKeyPick(vector.Wire)
	}
	return codec.VerifyTicket(vector.Wire)
}

func asTicketError(err error, target **TicketError) bool {
	for err != nil {
		if e, ok := err.(*TicketError); ok {
			*target = e
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
