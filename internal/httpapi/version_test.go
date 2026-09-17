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
	s := New(Deps{Version: "1.2.3", Revision: "abc1234", UIAssets: "minified"})
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
	// The console asset shape rides along with the rest of the build identity: this is
	// the one call that answers "is the running instance serving readable front-end
	// source?" without `strings` and without the build log, which only ever existed on
	// the terminal of whoever ran the build. See docs/design/m54-console-asset-shape.md.
	if got["ui"] != "minified" {
		t.Errorf("ui = %v, want minified", got["ui"])
	}
}

// TestVersionDeclaresAnUnknownShapeRatherThanOmittingIt keeps "this deployment is not
// minified" distinguishable from "this binary predates the field": both are things an
// operator reads /version to find out, and an absent key would conflate them.
func TestVersionDeclaresAnUnknownShapeRatherThanOmittingIt(t *testing.T) {
	s := New(Deps{Version: "1.2.3", Revision: "abc1234"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("GET /version: body is not JSON: %v", err)
	}
	value, ok := got["ui"]
	if !ok {
		t.Fatal("ui key is missing; an unset shape must still be reported")
	}
	if value != "unknown" {
		t.Errorf("ui = %v, want unknown", value)
	}
}

// TestHealthzCarriesTheSameBuildIdentity keeps the probe and the dedicated endpoint from
// drifting: a deployment check reading /healthz must see the same revision an operator
// reading /version sees. The console shape is part of that identity for the same reason.
func TestHealthzCarriesTheSameBuildIdentity(t *testing.T) {
	s := New(Deps{Version: "1.2.3", Revision: "abc1234", UIAssets: "source"})
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
	if got["ui"] != "source" {
		t.Errorf("healthz ui = %v, want source", got["ui"])
	}

	// Both endpoints must agree on the shape, not merely each carry some value.
	vresp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer vresp.Body.Close()
	var versionBody map[string]any
	if err := json.NewDecoder(vresp.Body).Decode(&versionBody); err != nil {
		t.Fatalf("GET /version: body is not JSON: %v", err)
	}
	if versionBody["ui"] != got["ui"] {
		t.Errorf("ui disagrees: /version = %v, /healthz = %v", versionBody["ui"], got["ui"])
	}
}
