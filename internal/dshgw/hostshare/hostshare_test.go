package hostshare

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newService(t *testing.T, declarations ...Declaration) *Service {
	t.Helper()
	service, err := New(Options{Subdir: "host", Declarations: declarations}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

// A share is visible to exactly the accounts it names, and to no other.
func TestSharesForFiltersByTenant(t *testing.T) {
	service := newService(t,
		Declaration{Name: "repo", Source: "/srv/repo", ReadOnly: true, Tenants: []string{"dsh-colin", "dsh-tenant"}},
		Declaration{Name: "private", Source: "/srv/private", Tenants: []string{"dsh-tenant"}},
	)
	workspace := "/state/workspaces/dsh-colin"
	shares := service.SharesFor("dsh-colin", workspace)
	if len(shares) != 1 || shares[0].Name != "repo" {
		t.Fatalf("dsh-colin shares = %+v, want only repo", shares)
	}
	if want := filepath.Join(workspace, "host", "repo"); shares[0].Target != want {
		t.Errorf("target = %q, want %q", shares[0].Target, want)
	}
	if !shares[0].ReadOnly {
		t.Error("declaration was read-only but the binding is not")
	}
	if got := service.SharesFor("dsh-yangmiao", workspace); len(got) != 0 {
		t.Errorf("an account the shares do not name got %+v", got)
	}
	// A write grant travels with the declaration.
	tenant := service.SharesFor("dsh-tenant", "/state/workspaces/dsh-tenant")
	if len(tenant) != 2 || tenant[1].Name != "private" || tenant[1].ReadOnly {
		t.Fatalf("dsh-tenant shares = %+v", tenant)
	}
}

// Ensure creates the container and every target at 0700 and records the account's list, so the
// tenant side can tell a bound directory from a plain one.
func TestEnsureCreatesTargetsAndMirror(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "shared")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "state", "workspaces", "dsh-colin")
	dshHome := filepath.Join(root, "state", "tenants", "dsh-colin", ".dsh")
	if err := os.MkdirAll(dshHome, 0o700); err != nil {
		t.Fatal(err)
	}
	service := newService(t,
		Declaration{Name: "shared", Source: source, ReadOnly: true, Tenants: []string{"dsh-colin"}},
		Declaration{Name: "gone", Source: filepath.Join(root, "vanished"), Tenants: []string{"dsh-colin"}},
	)
	if err := service.Ensure("dsh-colin", workspace, dshHome); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, dir := range []string{service.ContainerFor(workspace), filepath.Join(workspace, "host", "shared")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want a 0700 directory", dir, info.Mode())
		}
	}
	// The declaration whose directory vanished is skipped rather than fatal, and is left out of
	// the mirror: the panel must not offer a workspace that holds nothing.
	data, err := os.ReadFile(filepath.Join(dshHome, "host-shares.json"))
	if err != nil {
		t.Fatalf("reading the mirror: %v", err)
	}
	var document struct {
		Version int    `json:"version"`
		Tenant  string `json:"tenant"`
		Subdir  string `json:"subdir"`
		Shares  []struct {
			Name     string `json:"name"`
			Target   string `json:"target"`
			ReadOnly bool   `json:"read_only"`
		} `json:"shares"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parsing the mirror: %v", err)
	}
	if document.Version != 1 || document.Tenant != "dsh-colin" || document.Subdir != "host" {
		t.Errorf("mirror header = %+v", document)
	}
	if len(document.Shares) != 1 || document.Shares[0].Name != "shared" || !document.Shares[0].ReadOnly {
		t.Fatalf("mirror shares = %+v, want the live declaration alone", document.Shares)
	}
	if document.Shares[0].Target != filepath.Join(workspace, "host", "shared") {
		t.Errorf("mirror target = %q", document.Shares[0].Target)
	}
	// The host path is not in the mirror: the mapping is the operator's configuration.
	if got := string(data); strings.Contains(got, source) {
		t.Errorf("the mirror names the host directory: %s", got)
	}
	// A share for another account never reaches this account's mirror.
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	mixed := newService(t, Declaration{Name: "other", Source: other, Tenants: []string{"dsh-tenant"}})
	if err := mixed.Ensure("dsh-colin", workspace, dshHome); err != nil {
		t.Fatalf("Ensure for a foreign share: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(dshHome, "host-shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "other") {
		t.Errorf("a share declared for another account appears: %s", data)
	}
}

// A disabled deployment gets no service, which is what leaves the tenancy hook nil.
func TestNoDeclarationsMeansNoService(t *testing.T) {
	service := newService(t)
	if service.Enabled() {
		t.Error("a service with no declarations reports itself enabled")
	}
	if shares := service.SharesFor("dsh-colin", "/w"); shares != nil {
		t.Errorf("shares = %+v, want none", shares)
	}
	if err := service.Ensure("dsh-colin", "/nonexistent/workspace", "/nonexistent/home"); err != nil {
		t.Errorf("Ensure with no declarations did work: %v", err)
	}
}
