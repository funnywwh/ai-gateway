package feishu

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// bindAttempt and loginAttempt keep the tests readable now that Sign takes a struct.
func bindAttempt(keyID int64, actor, nonce string) Attempt {
	return Attempt{Flow: FlowBind, KeyID: keyID, Actor: actor, Nonce: nonce}
}

func loginAttempt(nonce string) Attempt {
	return Attempt{Flow: FlowDSHLogin, Nonce: nonce}
}

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
	wire, err := codec.Sign(bindAttempt(7, "admin", "nonce-1"))
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
	fresh, err := codec.Sign(bindAttempt(7, "admin", "nonce-2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(fresh); err != nil {
		t.Fatalf("a fresh state was refused: %v", err)
	}

	// Expiry is judged against the injected clock.
	*now = now.Add(11 * time.Minute)
	late, err := codec.Sign(bindAttempt(7, "admin", "nonce-3"))
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Minute)
	if _, err := codec.Verify(late); !isReason(err, "expired") {
		t.Fatalf("an expired state was accepted: %v", err)
	}
}

// The console login and the invitation flows carry their own targets, and the invitation
// handle survives the round trip: it is what the callback compares against the account row.
func TestStateCarriesAdminTargets(t *testing.T) {
	codec, _ := testCodec(t, time.Hour)
	invite, err := codec.Sign(Attempt{
		Flow: FlowAdminInvite, AdminUserID: 42, Actor: "boss", Nonce: "nonce-invite", Invite: "handle-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := codec.Verify(invite)
	if err != nil {
		t.Fatal(err)
	}
	if state.Flow != FlowAdminInvite || state.AdminUserID != 42 || state.Actor != "boss" || state.Invite != "handle-1" {
		t.Fatalf("invitation state round trip mismatch: %+v", state)
	}

	login, err := codec.Sign(Attempt{Flow: FlowAdminLogin, Nonce: "nonce-admin"})
	if err != nil {
		t.Fatal(err)
	}
	state, err = codec.Verify(login)
	if err != nil {
		t.Fatal(err)
	}
	if state.Flow != FlowAdminLogin || state.AdminUserID != 0 || state.KeyID != 0 {
		t.Fatalf("admin login state round trip mismatch: %+v", state)
	}
}

// Peek performs every check Verify does but leaves the state redeemable: the invitation
// entry point inspects a link, and the person may still cancel the consent screen.
func TestStatePeekDoesNotConsume(t *testing.T) {
	codec, _ := testCodec(t, time.Hour)
	wire, err := codec.Sign(Attempt{
		Flow: FlowAdminInvite, AdminUserID: 9, Actor: "boss", Nonce: "nonce-peek", Invite: "handle-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := codec.Peek(wire); err != nil {
			t.Fatalf("peek %d refused: %v", i, err)
		}
	}
	if _, err := codec.Verify(wire); err != nil {
		t.Fatalf("the state was not redeemable after peeking: %v", err)
	}
	// ...and once redeemed it is spent, for peeking as much as for verifying.
	if _, err := codec.Verify(wire); err == nil {
		t.Fatal("a replayed state was accepted")
	}
	if _, err := codec.Peek(wire); !isReason(err, "replayed") {
		t.Fatalf("a replayed state was peeked: %v", err)
	}
}

func TestStateRejectsTampering(t *testing.T) {
	codec, _ := testCodec(t, 10*time.Minute)
	wire, err := codec.Sign(loginAttempt("nonce-abc"))
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
		if _, err := codec.Peek(candidate); err == nil {
			t.Errorf("%s: accepted by peek", name)
		}
	}

	// A state signed with another key is refused even though it is well formed.
	other, err := NewStateCodec([]byte("other-key"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.Sign(loginAttempt("nonce-xyz"))
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
	wire, err := wide.Sign(bindAttempt(3, "admin", "long-lived"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Verify(wire); !isReason(err, "expiry beyond the configured window") {
		t.Fatalf("a far-future state was accepted: %v", err)
	}
	if _, err := codec.Peek(wire); !isReason(err, "expiry beyond the configured window") {
		t.Fatalf("a far-future state was peeked: %v", err)
	}
}

func TestStateSignRejectsIncompleteInputs(t *testing.T) {
	codec, _ := testCodec(t, 10*time.Minute)
	if _, err := codec.Sign(Attempt{Flow: Flow("nonsense"), KeyID: 1, Actor: "admin", Nonce: "n"}); err == nil {
		t.Error("an unknown flow was signed")
	}
	if _, err := codec.Sign(bindAttempt(0, "admin", "n")); err == nil {
		t.Error("a binding state without a key was signed")
	}
	if _, err := codec.Sign(loginAttempt("  ")); err == nil {
		t.Error("a state without a nonce was signed")
	}
	if _, err := codec.Sign(Attempt{Flow: FlowAdminInvite, Actor: "boss", Nonce: "n", Invite: "h"}); err == nil {
		t.Error("an invitation state without an account was signed")
	}
	if _, err := codec.Sign(Attempt{Flow: FlowAdminInvite, AdminUserID: 5, Actor: "boss", Nonce: "n"}); err == nil {
		t.Error("an invitation state without an invitation handle was signed")
	}
	if _, err := NewStateCodec(nil, time.Minute); err == nil {
		t.Error("a codec with no key was created")
	}
	if _, err := NewStateCodec([]byte("k"), 0); err == nil {
		t.Error("a codec with no ttl was created")
	}
}

// A payload that names a flow without carrying that flow's target is refused at
// verification too: a signature only proves who wrote it, not that it means anything.
func TestStateVerifyRejectsIncompletePayloads(t *testing.T) {
	codec, _ := testCodec(t, 10*time.Minute)
	cases := map[string]State{
		"binding without a key":     {Version: 1, Flow: FlowBind, Nonce: "n", Expires: codec.now().Add(time.Minute).Unix()},
		"invite without an account": {Version: 1, Flow: FlowAdminInvite, Nonce: "n", Invite: "h", Expires: codec.now().Add(time.Minute).Unix()},
		"invite without a handle":   {Version: 1, Flow: FlowAdminInvite, Nonce: "n", AdminUserID: 4, Expires: codec.now().Add(time.Minute).Unix()},
		"unknown flow":              {Version: 1, Flow: Flow("session"), Nonce: "n", Expires: codec.now().Add(time.Minute).Unix()},
		"unsupported version":       {Version: 2, Flow: FlowAdminLogin, Nonce: "n", Expires: codec.now().Add(time.Minute).Unix()},
	}
	for name, state := range cases {
		wire := signRaw(t, codec, state)
		if _, err := codec.Verify(wire); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The consumed set is bounded: a burst of attempts must not grow it without limit, and the
// oldest entries are the ones dropped.
func TestStateNonceSetIsBounded(t *testing.T) {
	codec, now := testCodec(t, time.Minute)
	for i := 0; i < maxUsedNonces+50; i++ {
		wire, err := codec.Sign(loginAttempt("nonce-" + itoa(i)))
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

// signRaw writes a payload the codec would never mint, so the verification-side shape
// checks can be exercised through a valid signature.
func signRaw(t *testing.T, codec *StateCodec, state State) string {
	t.Helper()
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + codec.sign(encoded)
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
