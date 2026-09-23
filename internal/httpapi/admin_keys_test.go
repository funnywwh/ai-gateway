package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/secret"
)

// M80: the batch key import and the ownership lookup. These tests pin the three properties
// the design document argues for (docs/design/m80-key-batch-import-and-lookup.md):
//
//   - a batch is all-or-nothing, and a failure names the item that needs fixing;
//   - a caller-supplied plaintext key is hashed here, and the plaintext is never stored,
//     returned or audited;
//   - the lookup answers "whose key is this" without deciding whether the key may be used.

// batchJSON renders a batch body. Items are maps so a test can build the malformed ones a
// struct would refuse to represent.
func batchJSON(items ...map[string]any) string {
	raw, err := json.Marshal(map[string]any{"keys": items})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func dryRunBatchJSON(items ...map[string]any) string {
	raw, err := json.Marshal(map[string]any{"keys": items, "dry_run": true})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func plaintextItem(name, account, key string) map[string]any {
	return map[string]any{"name": name, "account": account, "api_key": key}
}

// storedKeys lists every key row in the deployment.
func storedKeys(t *testing.T, f *adminFixture) []*domain.APIKey {
	t.Helper()
	keys, err := f.db.ListAPIKeys(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// importAuditRows returns the audit trail's import entries (newest first).
func importAuditRows(t *testing.T, f *adminFixture) []*AuditEntry {
	t.Helper()
	rows, err := f.db.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]*AuditEntry, 0, len(rows))
	for _, row := range rows {
		if row.Action == "import" && row.TargetType == "api_key" {
			out = append(out, row)
		}
	}
	return out
}

func assertNoKeyMaterial(t *testing.T, where string, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(text, value) {
			t.Fatalf("%s leaked the key itself: %s", where, text)
		}
		if strings.Contains(text, secret.Hash(value)) {
			t.Fatalf("%s leaked the hash: %s", where, text)
		}
	}
}

// errorEnvelope flattens the {"error":{message,param}} shape the management API answers with.
func errorEnvelope(t *testing.T, payload map[string]any) (message, param string) {
	t.Helper()
	inner, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %v", payload)
	}
	message, _ = inner["message"].(string)
	param, _ = inner["param"].(string)
	return message, param
}

// The point of the plaintext form: the caller keeps its existing key value, the gateway
// stores only what the data plane needs, and nothing secret comes back out.
func TestBatchImportPlaintextKeysAuthenticate(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	seedTags(t, f, "blue")

	laptop := "sk-live-laptop-0123456789abcdef"
	phone := "sk-gw-phone-0123456789abcdef01"
	minted := ids.APIKey()

	body := batchJSON(
		map[string]any{"name": "laptop", "account": "acme", "api_key": laptop, "tags": []any{"blue"}},
		map[string]any{"name": "phone", "account_id": 1, "api_key": phone},
		map[string]any{"name": "minted", "account": "acme", "api_key": minted},
	)
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch import status = %d: %v", resp.StatusCode, payload)
	}
	if payload["total"] != float64(3) || payload["created"] != float64(3) || payload["updated"] != float64(0) {
		t.Fatalf("batch counts = %v, want 3 created", payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	assertNoKeyMaterial(t, "the batch import response", string(raw), laptop, phone, minted)

	rows, _ := payload["keys"].([]any)
	if len(rows) != 3 {
		t.Fatalf("the response must report one row per item, got %v", payload["keys"])
	}
	for i, want := range []string{laptop, phone, minted} {
		row, _ := rows[i].(map[string]any)
		if row["key_prefix"] != secret.Prefix(want) {
			t.Errorf("row %d prefix = %v, want %s", i, row["key_prefix"], secret.Prefix(want))
		}
		if row["created"] != true {
			t.Errorf("row %d created = %v, want true", i, row["created"])
		}
	}

	// Each caller-chosen value is a working credential, and the prefix alone is not one.
	for _, token := range []string{laptop, phone, minted} {
		resp := bearerCall(t, f, token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the imported key did not authenticate: status = %d", resp.StatusCode)
		}
		resp = bearerCall(t, f, secret.Prefix(token))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a bare prefix authenticated: status = %d, want 401", resp.StatusCode)
		}
	}

	keys := storedKeys(t, f)
	if len(keys) != 3 {
		t.Fatalf("stored %d keys, want 3", len(keys))
	}
	hashes := map[string]bool{}
	for _, key := range keys {
		hashes[key.KeyHash] = true
		if key.CreatedBy != importedKeyPrefix+adminUser {
			t.Errorf("key %q created_by = %q, want the import marker", key.Name, key.CreatedBy)
		}
	}
	for _, token := range []string{laptop, phone, minted} {
		if hashes[token] {
			t.Fatalf("a plaintext value was stored as the hash: %s", token)
		}
		if !hashes[secret.Hash(token)] {
			t.Fatalf("the SHA-256 of %s is missing from the table", secret.Prefix(token))
		}
	}

	audit := importAuditRows(t, f)
	if len(audit) != 3 {
		t.Fatalf("import audit rows = %d, want one per item", len(audit))
	}
	for _, row := range audit {
		if !strings.Contains(row.ChangesJSON, `"batch":true`) {
			t.Errorf("audit row does not mark the batch: %s", row.ChangesJSON)
		}
		if !strings.Contains(row.ChangesJSON, `"credential":"plaintext"`) {
			t.Errorf("audit row does not say which credential form was used: %s", row.ChangesJSON)
		}
		assertNoKeyMaterial(t, "the audit trail", row.ChangesJSON, laptop, phone, minted)
	}
}

// The migration form and a mixed batch have to work the same way: the two credential forms
// differ only in who computes the SHA-256.
func TestBatchImportAcceptsHashFormAndMixedBatches(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	migrated := ids.APIKey()
	custom := "sk-live-custom-abcdef0123456789"
	body := batchJSON(
		map[string]any{"name": "migrated", "account": "acme",
			"key_prefix": secret.Prefix(migrated), "key_hash": secret.Hash(migrated)},
		plaintextItem("custom", "acme", custom),
	)
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK || payload["created"] != float64(2) {
		t.Fatalf("mixed batch status = %d payload = %v", resp.StatusCode, payload)
	}
	for _, token := range []string{migrated, custom} {
		resp := bearerCall(t, f, token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a key from the mixed batch did not authenticate: %d", resp.StatusCode)
		}
	}
	forms := []string{}
	for _, row := range importAuditRows(t, f) {
		forms = append(forms, row.ChangesJSON)
	}
	joined := strings.Join(forms, " ")
	if !strings.Contains(joined, `"credential":"hash"`) || !strings.Contains(joined, `"credential":"plaintext"`) {
		t.Fatalf("the audit trail must record both credential forms: %s", joined)
	}
}

// A batch that fails validation must leave the table exactly as it was, and the error has to
// name the item: "invalid request" on a 200-item call would be unusable.
func TestBatchImportIsAtomicAndNamesTheOffendingItem(t *testing.T) {
	good := "sk-live-good-0123456789abcdef"
	sharedA := "sk-shared-item-0123456789abcdef"
	sharedB := "sk-shared-item-ffffffffffffffffff"

	cases := []struct {
		name       string
		item       map[string]any
		extraItems []map[string]any
		seed       func(t *testing.T, f *adminFixture)
		wantStatus int
		wantParts  []string
	}{
		{
			name:       "unknown tag",
			item:       map[string]any{"name": "x", "account": "acme", "api_key": good, "tags": []any{"ghost"}},
			wantStatus: http.StatusBadRequest,
			wantParts:  []string{"tag"},
		},
		{
			name:       "unknown account",
			item:       plaintextItem("x", "ghost-account", good),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing credential",
			item:       map[string]any{"name": "x", "account": "acme"},
			wantStatus: http.StatusBadRequest,
			wantParts:  []string{"api_key"},
		},
		{
			name: "both credential forms",
			item: map[string]any{"name": "x", "account": "acme", "api_key": good,
				"key_prefix": "sk-live-abcd", "key_hash": strings.Repeat("b", 64)},
			wantStatus: http.StatusBadRequest,
			wantParts:  []string{"not both"},
		},
		{
			name:       "status outside the enum",
			item:       map[string]any{"name": "x", "account": "acme", "api_key": good, "status": "paused"},
			wantStatus: http.StatusBadRequest,
			wantParts:  []string{"status"},
		},
		{
			name:       "expires_at is not RFC3339",
			item:       map[string]any{"name": "x", "account": "acme", "api_key": good, "expires_at": "2026-09-23"},
			wantStatus: http.StatusBadRequest,
			wantParts:  []string{"expires_at"},
		},
		{
			name:       "policy with an unknown field",
			item:       map[string]any{"name": "x", "account": "acme", "api_key": good, "policy": map[string]any{"rate_limit": 5}},
			wantStatus: http.StatusBadRequest,
			wantParts:  []string{"policy"},
		},
		{
			name:       "prefix repeated inside the batch",
			item:       map[string]any{"name": "second", "account": "acme", "api_key": sharedB},
			extraItems: []map[string]any{{"name": "first", "account": "acme", "api_key": sharedA}},
			wantStatus: http.StatusBadRequest,
			// The offending item is the second one; the message points back at the first.
			wantParts: []string{"already used by item 1", secret.Prefix(sharedA)},
		},
		{
			name:       "prefix owned by a console-issued key",
			item:       map[string]any{"name": "clash", "account": "acme", "api_key": sharedB},
			wantStatus: http.StatusConflict,
			seed: func(t *testing.T, f *adminFixture) {
				t.Helper()
				if _, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
					AccountID: 1, Name: "console-key", KeyPrefix: secret.Prefix(sharedA),
					KeyHash: secret.Hash(sharedA), Status: "active", CreatedBy: adminUser,
					RecordInputMode: "inherit",
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantParts: []string{"console-key"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminFixture(t)
			cookie := f.login(t, adminUser, adminPassword)
			if tc.seed != nil {
				tc.seed(t, f)
			}
			before := len(storedKeys(t, f))
			beforeAudit := len(importAuditRows(t, f))

			items := []map[string]any{plaintextItem("anchor", "acme", good)}
			items = append(items, tc.extraItems...)
			items = append(items, tc.item)
			// The item under test is the last one, so its index depends on the extras.
			index := len(items) - 1
			resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", batchJSON(items...), cookie)
			payload := decodeJSONBody(t, resp)
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, tc.wantStatus, payload)
			}
			message, param := errorEnvelope(t, payload)
			if want := fmt.Sprintf("keys[%d]", index); !strings.Contains(message, want) || !strings.HasPrefix(param, want) {
				t.Errorf("error message=%q param=%q does not locate keys[%d]", message, param, index)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(message, part) {
					t.Errorf("error %q does not mention %q", message, part)
				}
			}
			// Nothing was written, and nothing was audited: the batch is all-or-nothing.
			if got := len(storedKeys(t, f)); got != before {
				t.Errorf("a refused batch wrote keys: %d rows, want %d", got, before)
			}
			if got := len(importAuditRows(t, f)); got != beforeAudit {
				t.Errorf("a refused batch wrote %d audit rows, want %d", got, beforeAudit)
			}
		})
	}
}

func TestBatchImportRejectsBadPlaintext(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"at the prefix boundary", "sk-live-abcd"},
		{"shorter than the floor", "sk-live-abcdef"},
		{"whitespace inside", "sk-live abcd-0123456789"},
		{"non printable byte", "sk-live-\x01-0123456789"},
		{"longer than the ceiling", "sk-live-" + strings.Repeat("a", 600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminFixture(t)
			cookie := f.login(t, adminUser, adminPassword)
			body := batchJSON(plaintextItem("bad", "acme", tc.key))
			resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, cookie)
			payload := decodeJSONBody(t, resp)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%v)", resp.StatusCode, payload)
			}
			message, param := errorEnvelope(t, payload)
			if !strings.Contains(message, "keys[0]") || param != "keys[0].api_key" {
				t.Errorf("error message=%q param=%q must locate keys[0].api_key", message, param)
			}
			if got := len(storedKeys(t, f)); got != 0 {
				t.Fatalf("a rejected key was written: %d rows", got)
			}
		})
	}
}

func TestBatchImportIsIdempotentOnRerun(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	seedTags(t, f, "blue")

	token := "sk-live-rerun-0123456789abcdef"
	first := batchJSON(map[string]any{"name": "first-name", "account": "acme", "api_key": token})
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", first, cookie)
	payload := decodeJSONBody(t, resp)
	resp.Body.Close()
	rows, _ := payload["keys"].([]any)
	firstRow, _ := rows[0].(map[string]any)
	firstID := firstRow["id"]

	// The same secret again, now with a new name and a tag: the row is refreshed, not doubled.
	second := batchJSON(map[string]any{"name": "second-name", "account": "acme", "api_key": token, "tags": []any{"blue"}})
	resp = f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", second, cookie)
	payload = decodeJSONBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-import status = %d: %v", resp.StatusCode, payload)
	}
	if payload["created"] != float64(0) || payload["updated"] != float64(1) {
		t.Fatalf("re-import must update in place, got %v", payload)
	}
	rows, _ = payload["keys"].([]any)
	secondRow, _ := rows[0].(map[string]any)
	if secondRow["id"] != firstID {
		t.Fatalf("re-import changed the id: %v → %v", firstID, secondRow["id"])
	}
	keys := storedKeys(t, f)
	if len(keys) != 1 || keys[0].Name != "second-name" {
		t.Fatalf("re-import left %d rows: %+v", len(keys), keys)
	}
	resp = bearerCall(t, f, token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the re-imported key stopped working: %d", resp.StatusCode)
	}
}

func TestBatchImportDryRunWritesNothing(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	token := "sk-live-dryrun-0123456789abcdef"
	body := dryRunBatchJSON(plaintextItem("dry", "acme", token))
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, cookie)
	payload := decodeJSONBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || payload["dry_run"] != true {
		t.Fatalf("dry run status = %d payload = %v", resp.StatusCode, payload)
	}
	if payload["created"] != float64(1) {
		t.Errorf("a dry run still reports what would happen: %v", payload)
	}
	rows, _ := payload["keys"].([]any)
	row, _ := rows[0].(map[string]any)
	if row["id"] != nil {
		t.Errorf("a dry run must not report an id: %v", row)
	}
	if got := len(storedKeys(t, f)); got != 0 {
		t.Fatalf("a dry run wrote %d keys", got)
	}
	if got := len(importAuditRows(t, f)); got != 0 {
		t.Fatalf("a dry run wrote %d audit rows", got)
	}
	// A dry run of a batch that cannot be written reports the same refusal.
	bad := dryRunBatchJSON(plaintextItem("dry", "ghost-account", token))
	resp = f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", bad, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a dry run must validate too: status = %d", resp.StatusCode)
	}
}

// A key that failed to authenticate a moment ago is cached as invalid for the negative TTL.
// Importing it has to lift that verdict immediately, or the client keeps seeing 401 after the
// operator did exactly what the error told them to do.
func TestBatchImportRefreshesTheVerificationCache(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	token := "sk-live-cache-0123456789abcdef"
	resp := bearerCall(t, f, token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unknown key must be refused first: %d", resp.StatusCode)
	}

	body := batchJSON(plaintextItem("late", "acme", token))
	resp = f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import status = %d", resp.StatusCode)
	}
	resp = bearerCall(t, f, token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the imported key stayed negative-cached: status = %d", resp.StatusCode)
	}
}

func TestBatchImportRequiresAnAdministrator(t *testing.T) {
	f := newAdminFixture(t)
	viewer := f.login(t, "reader", adminPassword)

	body := batchJSON(plaintextItem("refused", "acme", "sk-live-viewer-0123456789abcdef"))
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, viewer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer import status = %d, want 403", resp.StatusCode)
	}
	if got := len(storedKeys(t, f)); got != 0 {
		t.Fatalf("a refused batch wrote %d keys", got)
	}
}

// The lookup answers ownership and state. It deliberately does not decide admission.
func TestLookupKeyAnswersOwnership(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	token := "sk-live-lookup-0123456789abcdef"
	body := batchJSON(plaintextItem("laptop", "acme", token))
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", body, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seeding the key failed: %d", resp.StatusCode)
	}

	lookup := func(t *testing.T, payload string) map[string]any {
		t.Helper()
		resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/lookup", payload, cookie)
		decoded := decodeJSONBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("lookup status = %d: %v", resp.StatusCode, decoded)
		}
		return decoded
	}

	byKey := lookup(t, `{"api_key":"`+token+`"}`)
	if byKey["found"] != true || byKey["matched"] != "hash" {
		t.Fatalf("lookup by plaintext = %v", byKey)
	}
	key, _ := byKey["key"].(map[string]any)
	account, _ := byKey["account"].(map[string]any)
	if key["name"] != "laptop" || key["key_prefix"] != secret.Prefix(token) {
		t.Fatalf("lookup returned the wrong key: %v", key)
	}
	if account["name"] != "acme" || account["id"] != float64(1) {
		t.Fatalf("lookup returned the wrong account: %v", account)
	}
	assertNoKeyMaterial(t, "the lookup response", fmt.Sprint(byKey), token)

	byPrefix := lookup(t, `{"key_prefix":"`+secret.Prefix(token)+`"}`)
	if byPrefix["found"] != true || byPrefix["matched"] != "prefix" {
		t.Fatalf("lookup by prefix = %v", byPrefix)
	}

	unknown := lookup(t, `{"key_prefix":"sk-nobody000"}`)
	if unknown["found"] != false || unknown["reason"] != "unknown_prefix" {
		t.Fatalf("an unknown prefix = %v", unknown)
	}

	// A key sharing the first 12 characters but not the secret: the answer must not name the
	// row it collided with.
	clash := "sk-live-lookupX0123456789abcdef"
	if secret.Prefix(clash) != secret.Prefix(token) {
		t.Fatalf("the test key does not share a prefix: %s vs %s", secret.Prefix(clash), secret.Prefix(token))
	}
	mismatch := lookup(t, `{"api_key":"`+clash+`"}`)
	if mismatch["found"] != false || mismatch["reason"] != "hash_mismatch" {
		t.Fatalf("a hash mismatch = %v", mismatch)
	}
	if mismatch["key"] != nil || mismatch["account"] != nil {
		t.Fatalf("a hash mismatch must not describe the row: %v", mismatch)
	}

	// A disabled key still answers the ownership question, with its status.
	id := int64(key["id"].(float64))
	resp = f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/keys/%d", id), `{"status":"disabled"}`, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disabling the key failed: %d", resp.StatusCode)
	}
	disabled := lookup(t, `{"api_key":"`+token+`"}`)
	key, _ = disabled["key"].(map[string]any)
	if disabled["found"] != true || key["status"] != "disabled" {
		t.Fatalf("a disabled key must still report status: %v", disabled)
	}

	// An expired key reports expires_at rather than a verdict.
	expired := "sk-live-expired-0123456789abcde"
	past := "2020-01-01T00:00:00Z"
	resp = f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch", batchJSON(
		map[string]any{"name": "expired", "account": "acme", "api_key": expired, "expires_at": past}), cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seeding an expired key failed: %d", resp.StatusCode)
	}
	expiredPayload := lookup(t, `{"api_key":"`+expired+`"}`)
	key, _ = expiredPayload["key"].(map[string]any)
	if key["expires_at"] == nil {
		t.Fatalf("the lookup must report expires_at: %v", expiredPayload)
	}

	// Shape table for the request itself.
	for _, bad := range []string{`{}`, `{"api_key":"a","key_prefix":"` + secret.Prefix(token) + `"}`} {
		resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/lookup", bad, cookie)
		decoded := decodeJSONBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("lookup %s status = %d, want 400 (%v)", bad, resp.StatusCode, decoded)
		}
	}
	// A prefix that is not 12 characters is a 400, not a silent miss.
	resp = f.call(t, http.MethodPost, "/admin/api/v1/keys/lookup", `{"key_prefix":"too-short"}`, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed prefix status = %d, want 400", resp.StatusCode)
	}
}

func TestLookupKeyIsAvailableToViewers(t *testing.T) {
	f := newAdminFixture(t)
	admin := f.login(t, adminUser, adminPassword)
	viewer := f.login(t, "reader", adminPassword)

	token := "sk-live-viewer-0123456789abcdef"
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import-batch",
		batchJSON(plaintextItem("owned", "acme", token)), admin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seeding failed: %d", resp.StatusCode)
	}

	resp = f.call(t, http.MethodPost, "/admin/api/v1/keys/lookup", `{"api_key":"`+token+`"}`, viewer)
	payload := decodeJSONBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || payload["found"] != true {
		t.Fatalf("a viewer must be able to ask whose key this is: %d %v", resp.StatusCode, payload)
	}
}
