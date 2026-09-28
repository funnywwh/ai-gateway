package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/ids"
	"github.com/funnywwh/ai-gateway/internal/secret"
)

// importBody builds the import payload for a synthetic token. The plaintext is used here
// only to compute the prefix and the hash, exactly as a migration script on the source
// system would: the gateway never receives it.
func importBody(token, name, tags string) string {
	payload := map[string]any{
		"name":       name,
		"account_id": 1,
		"key_prefix": secret.Prefix(token),
		"key_hash":   secret.Hash(token),
	}
	if tags != "" {
		payload["tags"] = strings.Split(tags, ",")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func seedTags(t *testing.T, f *adminFixture, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		tag := &domain.Tag{Name: name, GrantsJSON: `{"providers":["provider-` + name + `"]}`}
		if _, err := f.db.UpsertTag(ctx, tag); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
}

// bearerCall hits the data plane with a token instead of a console cookie.
func bearerCall(t *testing.T, f *adminFixture, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The point of the import path: a key that already exists elsewhere authenticates here
// without anybody ever sending its plaintext to the gateway.
func TestImportedKeyAuthenticatesWithoutThePlaintext(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	seedTags(t, f, "blue")

	token := ids.APIKey()
	hash := secret.Hash(token)
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import", importBody(token, "migrated", "blue"), cookie)
	created := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import status = %d: %v", resp.StatusCode, created)
	}
	if created["created"] != true {
		t.Fatalf("created = %v, want true for a new prefix", created["created"])
	}
	if created["key_prefix"] != secret.Prefix(token) {
		t.Fatalf("key_prefix = %v, want %s", created["key_prefix"], secret.Prefix(token))
	}
	assertJSONStrings(t, created["tags"], `["blue"]`)
	// Neither the plaintext nor the hash may travel back out.
	if _, ok := created["key_hash"]; ok {
		t.Fatal("the import response must not echo the hash")
	}
	if raw, _ := json.Marshal(created); strings.Contains(string(raw), token) || strings.Contains(string(raw), hash) {
		t.Fatalf("the import response leaked key material: %s", raw)
	}

	if resp := bearerCall(t, f, token); resp.StatusCode != http.StatusOK {
		t.Fatalf("the imported key did not authenticate: status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// The prefix alone is not the secret: it indexes the row, it does not open it.
	if resp := bearerCall(t, f, secret.Prefix(token)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bare prefix authenticated: status = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := bearerCall(t, f, token[:len(token)-1]); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a truncated key authenticated: status = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The audit trail records the import and keeps the hash out of it.
	rows, err := f.db.ListAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.Action != "import" || row.TargetType != "api_key" {
			continue
		}
		found = true
		if strings.Contains(row.ChangesJSON, hash) {
			t.Fatalf("audit entry carries the key hash: %s", row.ChangesJSON)
		}
		if !strings.Contains(row.ChangesJSON, secret.Prefix(token)) {
			t.Fatalf("audit entry should name the prefix: %s", row.ChangesJSON)
		}
	}
	if !found {
		t.Fatal("no import entry in the audit trail")
	}
}

func TestImportKeyValidatesItsInput(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	seedTags(t, f, "blue")

	validToken := ids.APIKey()
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"short prefix is a label, not a lookup key", `{"name":"k","account_id":1,"key_prefix":"sk-123","key_hash":"` + secret.Hash(validToken) + `"}`, http.StatusOK},
		{"prefix with whitespace", `{"name":"k","account_id":1,"key_prefix":"sk-123 45678","key_hash":"` + secret.Hash(validToken) + `"}`, http.StatusBadRequest},
		{"prefix longer than the label cap", `{"name":"k","account_id":1,"key_prefix":"` + strings.Repeat("a", 65) + `","key_hash":"` + secret.Hash(validToken) + `"}`, http.StatusBadRequest},
		{"no prefix at all is allowed", `{"name":"k","account_id":1,"key_hash":"` + secret.Hash(validToken) + `"}`, http.StatusOK},
		{"short hash", `{"name":"k","account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"abcd"}`, http.StatusBadRequest},
		{"non hex hash", `{"name":"k","account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + strings.Repeat("z", 64) + `"}`, http.StatusBadRequest},
		{"missing name", `{"account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + secret.Hash(validToken) + `"}`, http.StatusBadRequest},
		{"unknown account", `{"name":"k","account_id":999,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + secret.Hash(validToken) + `"}`, http.StatusNotFound},
		{"unknown tag", `{"name":"k","account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + secret.Hash(validToken) + `","tags":["nope"]}`, http.StatusBadRequest},
		{"bad status", `{"name":"k","account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + secret.Hash(validToken) + `","status":"paused"}`, http.StatusBadRequest},
		{"bad expiry", `{"name":"k","account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + secret.Hash(validToken) + `","expires_at":"tomorrow"}`, http.StatusBadRequest},
		{"unknown policy field", `{"name":"k","account_id":1,"key_prefix":"` + secret.Prefix(validToken) + `","key_hash":"` + secret.Hash(validToken) + `","policy":{"rate_limit":{"rpm":5}}}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import", tc.body, cookie)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestImportKeyIsIdempotentAndASharedLabelIsNotATakeover(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	seedTags(t, f, "blue", "green")
	ctx := context.Background()

	token := ids.APIKey()
	first := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/keys/import", importBody(token, "migrated", "blue"), cookie))
	if first["created"] != true {
		t.Fatalf("first import created = %v, want true", first["created"])
	}
	// Re-running a migration must update its own row, not fail and not duplicate it.
	again := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/keys/import", importBody(token, "migrated", "green"), cookie))
	if again["created"] != false {
		t.Fatalf("second import created = %v, want false", again["created"])
	}
	if again["id"] != first["id"] {
		t.Fatalf("re-import changed the key id: %v -> %v", first["id"], again["id"])
	}
	assertJSONStrings(t, again["tags"], `["green"]`)
	keys, err := f.db.ListAPIKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("stored keys = %d, want 1", len(keys))
	}

	// A *different* secret that happens to share the console key's display prefix is its own key,
	// not a takeover (M87): a row is identified by its hash, and a label two people share is
	// exactly what a sub2api-style source produces. Both keys must keep working.
	console := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/keys", `{"name":"console","account_id":1,"tags":["blue"]}`, cookie))
	consoleToken, _ := console["key"].(string)
	if consoleToken == "" {
		t.Fatalf("console key creation returned no token: %v", console)
	}
	clash := map[string]any{
		"name": "same-label", "account_id": 1,
		"key_prefix": secret.Prefix(consoleToken),
		"key_hash":   secret.Hash(consoleToken + "different"),
	}
	raw, _ := json.Marshal(clash)
	imported := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/keys/import", string(raw), cookie))
	if imported["created"] != true {
		t.Fatalf("a key wearing somebody else's label must be created: %v", imported)
	}
	if imported["key_prefix"] != secret.Prefix(consoleToken) {
		t.Fatalf("the label must be stored verbatim: %v", imported["key_prefix"])
	}
	if resp := bearerCall(t, f, consoleToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("the console key stopped working after an import wore its label: status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := bearerCall(t, f, consoleToken+"different"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the imported key (same label, own secret) must authenticate: status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// The same secret again is not a conflict: it is the same key.
	same := map[string]any{
		"name": "console", "account_id": 1,
		"key_prefix": secret.Prefix(consoleToken),
		"key_hash":   secret.Hash(consoleToken),
	}
	raw, _ = json.Marshal(same)
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import", string(raw), cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-importing the same secret = %d, want 200", resp.StatusCode)
	}
	if resp := bearerCall(t, f, consoleToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("the console key stopped working: status = %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestImportKeyRequiresAnAdministrator(t *testing.T) {
	f := newAdminFixture(t)
	seedTags(t, f, "blue")
	viewer := f.login(t, "reader", adminPassword)

	token := ids.APIKey()
	resp := f.call(t, http.MethodPost, "/admin/api/v1/keys/import", importBody(token, "migrated", "blue"), viewer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer import status = %d, want 403", resp.StatusCode)
	}
	keys, err := f.db.ListAPIKeys(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("a refused import still wrote %d keys", len(keys))
	}
}

func TestFindAPIKeyByPrefixReportsAMissingRowAsAbsent(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()

	missing, err := f.db.FindAPIKeyByPrefix(ctx, "sk-missing00")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("missing prefix = %#v, want nil", missing)
	}
	token := ids.APIKey()
	if _, err := f.db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: 1, Name: "k", KeyPrefix: secret.Prefix(token), KeyHash: secret.Hash(token), Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	found, err := f.db.FindAPIKeyByPrefix(ctx, secret.Prefix(token))
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.KeyHash != secret.Hash(token) {
		t.Fatalf("found = %#v, want the stored key", found)
	}
}
