package feishu

import (
	"errors"
	"testing"
	"time"
)

func testCodec(t *testing.T, ttl time.Duration) (*StateCodec, *time.Time) {
	t.Helper()
	codec, err := NewStateCodec([]byte("state-key"), ttl)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	codec.Now = func() time.Time { return now }
	return codec, &now
}

func TestStateRoundTripAndReplay(t *testing.T) {
	codec, now := testCodec(t, 10*time.Minute)
	wire, err := codec.Sign(FlowBind, 7, "admin", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	state, err := codec.Verify(wire)
	if err != nil {
		t.Fatal(err)
	}
	if state.Flow != FlowBind || state.KeyID != 7 || state.Actor != "admin" || state.Nonce != "nonce-1" {
		t.Fatalf("state round trip mismatch: %+v", state)
	}
	// A second redemption of the same state is refused: the nonce is consumed on success.
	if _, err := codec.Verify(wire); err == nil {
		t.Fatal("a replayed state was accepted")
	}

	// The same codec accepts a fresh attempt, so consuming one state does not lock the
	// flow out.
	fresh, err := codec.Sign(FlowBind, 7, "admin", "nonce-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(fresh); err != nil {
		t.Fatalf("a fresh state was refused: %v", err)
	}

	// Expiry is judged against the injected clock.
	*now = now.Add(11 * time.Minute)
	late, err := codec.Sign(FlowBind, 7, "admin", "nonce-3")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Minute)
	if _, err := codec.Verify(late); !isReason(err, "expired") {
		t.Fatalf("an expired state was accepted: %v", err)
	}
}

func TestStateRejectsTampering(t *testing.T) {
	codec, _ := testCodec(t, 10*time.Minute)
	wire, err := codec.Sign(FlowDSHLogin, 0, "", "nonce-abc")
	if err != nil {
		t.Fatal(err)
	}
	encoded, signature, _ := cut(wire)
	cases := map[string]string{
		"empty":              "",
		"no separator":       encoded,
		"empty signature":    encoded + ".",
		"tampered payload":   flipLast(encoded) + "." + signature,
		"tampered signature": encoded + "." + flipLast(signature),
		"garbage":            "not-a-state",
	}
	for name, candidate := range cases {
		if _, err := codec.Verify(candidate); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A state signed with another key is refused even though it is well formed.
	other, err := NewStateCodec([]byte("other-key"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.Sign(FlowDSHLogin, 0, "", "nonce-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(foreign); !isReason(err, "bad signature") {
		t.Fatalf("a foreign state was accepted: %v", err)
	}
}

// A state may not carry an expiry beyond the configured window: the window is what keeps a
// leaked URL from being a long-lived capability.
func TestStateRejectsADistantExpiry(t *testing.T) {
	codec, _ := testCodec(t, 10*time.Minute)
	// Sign with a wide TTL codec, verify with the narrow one: the payload is valid, the
	// expiry is not.
	wide, err := NewStateCodec([]byte("state-key"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wide.Now = codec.Now
	wire, err := wide.Sign(FlowBind, 3, "admin", "long-lived")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(wire); !isReason(err, "expiry beyond the configured window") {
		t.Fatalf("a far-future state was accepted: %v", err)
	}
}

func TestStateSignRejectsIncompleteInputs(t *testing.T) {
	codec, _ := testCodec(t, 10*time.Minute)
	if _, err := codec.Sign(Flow("nonsense"), 1, "admin", "n"); err == nil {
		t.Error("an unknown flow was signed")
	}
	if _, err := codec.Sign(FlowBind, 0, "admin", "n"); err == nil {
		t.Error("a binding state without a key was signed")
	}
	if _, err := codec.Sign(FlowDSHLogin, 0, "", "  "); err == nil {
		t.Error("a state without a nonce was signed")
	}
	if _, err := NewStateCodec(nil, time.Minute); err == nil {
		t.Error("a codec with no key was created")
	}
	if _, err := NewStateCodec([]byte("k"), 0); err == nil {
		t.Error("a codec with no ttl was created")
	}
}

// The consumed set is bounded: a burst of attempts must not grow it without limit, and the
// oldest entries are the ones dropped.
func TestStateNonceSetIsBounded(t *testing.T) {
	codec, now := testCodec(t, time.Minute)
	for i := 0; i < maxUsedNonces+50; i++ {
		wire, err := codec.Sign(FlowDSHLogin, 0, "", "nonce-"+itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := codec.Verify(wire); err != nil {
			t.Fatalf("attempt %d refused: %v", i, err)
		}
		*now = now.Add(time.Millisecond)
	}
	codec.mu.Lock()
	size := len(codec.used)
	codec.mu.Unlock()
	if size > maxUsedNonces {
		t.Fatalf("consumed set grew to %d, want at most %d", size, maxUsedNonces)
	}
}

func TestDeriveSecretIsPurposeBound(t *testing.T) {
	first := DeriveSecret("material", "feishu-state")
	second := DeriveSecret("material", "feishu-ticket")
	if string(first) == string(second) {
		t.Fatal("two purposes derived the same key")
	}
	if string(DeriveSecret("material", "feishu-state")) != string(first) {
		t.Fatal("derivation is not deterministic")
	}
	if len(first) != 32 {
		t.Fatalf("derived key length = %d, want 32", len(first))
	}
}

func isReason(err error, reason string) bool {
	var stateErr *StateError
	if errors.As(err, &stateErr) {
		return stateErr.Reason == reason
	}
	var ticketErr *TicketError
	if errors.As(err, &ticketErr) {
		return ticketErr.Reason == reason
	}
	return false
}

func cut(raw string) (string, string, bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '.' {
			return raw[:i], raw[i+1:], true
		}
	}
	return raw, "", false
}

// flipLast changes the final character of a base64url string, keeping it decodable.
func flipLast(value string) string {
	if value == "" {
		return value
	}
	last := value[len(value)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return value[:len(value)-1] + string(replacement)
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
