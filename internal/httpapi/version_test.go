package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVersionEndpointIsPublicAndComplete pins the endpoint operators script against: it
// answers with the build identity, needs no session, and does not depend on the database
// being open (the fixture has no store at all, which is the point).
func TestVersionEndpointIsPublicAndComplete(t *testing.T) {
	s := New(Deps{Version: "1.2.3", Revision: "abc1234"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /version: status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("GET /version: body is not JSON: %v", err)
	}
	if got["version"] != "1.2.3" {
		t.Errorf("version = %v, want 1.2.3", got["version"])
	}
	if got["revision"] != "abc1234" {
		t.Errorf("revision = %v, want abc1234", got["revision"])
	}
}

// TestHealthzCarriesTheSameBuildIdentity keeps the probe and the dedicated endpoint from
// drifting: a deployment check reading /healthz must see the same revision an operator
// reading /version sees.
func TestHealthzCarriesTheSameBuildIdentity(t *testing.T) {
	s := New(Deps{Version: "1.2.3", Revision: "abc1234"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("GET /healthz: body is not JSON: %v", err)
	}
	if got["status"] != "ok" || got["version"] != "1.2.3" || got["revision"] != "abc1234" {
		t.Errorf("healthz = %v, want status=ok version=1.2.3 revision=abc1234", got)
	}
}
