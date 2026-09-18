package edge

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// echoGateway reports the edge port header the gateway was given: that header is
// what routes a request to a tenant, so seeing it is seeing the routing decision.
func echoGateway() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-DSHGW-Port"))
	})
}

func edgeFixture(t *testing.T) (*Edge, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		PortalPort:     freePort(t),
		EdgePortHeader: "X-DSHGW-Port",
		MaxHeaderBytes: 64 << 10,
		Deploy:         config.DeployConfig{PublicListen: "127.0.0.1"},
	}
	return New(cfg, echoGateway(), nil), cfg
}

func tenantOnPort(name string, port int) registry.Tenant {
	return registry.Tenant{Name: name, PublicPort: port, WorkerPort: port + 1000, UID: 1000,
		DshHome: "/state/" + name + "/.dsh", Workspace: "/srv/" + name}
}

func fetch(t *testing.T, port int, header string) (string, bool) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	if header != "" {
		request.Header.Set("X-DSHGW-Port", header)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body)), true
}

func TestEdgeBindsOnlyExistingSurfacesAndRoutesToTheRightTenant(t *testing.T) {
	e, cfg := edgeFixture(t)
	alice := tenantOnPort("alice", freePort(t))
	bob := tenantOnPort("bob", freePort(t))
	if err := e.Reconcile([]registry.Tenant{alice, bob}); err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Ports() is sorted, so the expectation is compared as a set.
	want := []int{cfg.PortalPort, alice.PublicPort, bob.PublicPort}
	sort.Ints(want)
	got := e.Ports()
	if len(got) != len(want) {
		t.Fatalf("bound ports = %v, want %v", got, want)
	}
	for i, port := range want {
		if got[i] != port {
			t.Fatalf("bound ports = %v, want %v", got, want)
		}
	}
	// Each surface routes to itself: the portal port is the tenant port that
	// serves the login page, and each tenant port serves that tenant only.
	for _, port := range want {
		body, ok := fetch(t, port, "")
		if !ok {
			t.Fatalf("port %d did not answer", port)
		}
		if body != strconv.Itoa(port) {
			t.Fatalf("port %d routed with header %q", port, body)
		}
	}

	// A client cannot talk one tenant's port into serving another: the edge
	// overwrites whatever the request tried to claim.
	body, ok := fetch(t, alice.PublicPort, strconv.Itoa(bob.PublicPort))
	if !ok {
		t.Fatal("alice's port stopped answering")
	}
	if body != strconv.Itoa(alice.PublicPort) {
		t.Fatalf("client-supplied edge header survived: %q", body)
	}
}

func TestEdgeReconcileClosesRemovedSurfacesAndIsIdempotent(t *testing.T) {
	e, _ := edgeFixture(t)
	alice := tenantOnPort("alice", freePort(t))
	bob := tenantOnPort("bob", freePort(t))
	if err := e.Reconcile([]registry.Tenant{alice, bob}); err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Running reconcile again with the same input must not disturb live listeners.
	if err := e.Reconcile([]registry.Tenant{alice, bob}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fetch(t, bob.PublicPort, ""); !ok {
		t.Fatal("idempotent reconcile closed a live listener")
	}

	// Removing a tenant closes exactly its port and leaves the others alone.
	if err := e.Reconcile([]registry.Tenant{alice}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fetch(t, bob.PublicPort, ""); ok {
		t.Fatal("removed tenant's port is still bound")
	}
	if _, ok := fetch(t, alice.PublicPort, ""); !ok {
		t.Fatal("remaining tenant's port was closed")
	}

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if len(e.Ports()) != 0 {
		t.Fatalf("Close left listeners: %v", e.Ports())
	}
	if _, ok := fetch(t, alice.PublicPort, ""); ok {
		t.Fatal("port still answering after Close")
	}
}

// A port that cannot be bound must not stop the rest: one tenant's conflict is
// reported, and every other surface stays up.
func TestEdgeReportsUnbindablePortAndKeepsTheRest(t *testing.T) {
	e, cfg := edgeFixture(t)
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	conflicting := tenantOnPort("conflict", squatter.Addr().(*net.TCPAddr).Port)
	alice := tenantOnPort("alice", freePort(t))

	reconcileErr := e.Reconcile([]registry.Tenant{conflicting, alice})
	defer e.Close()
	if reconcileErr == nil {
		t.Fatal("a port conflict was reported as success")
	}
	if !strings.Contains(reconcileErr.Error(), "conflict") {
		t.Fatalf("error does not name the tenant: %v", reconcileErr)
	}
	if _, ok := fetch(t, cfg.PortalPort, ""); !ok {
		t.Fatal("portal port did not stay up after another port conflicted")
	}
	if _, ok := fetch(t, alice.PublicPort, ""); !ok {
		t.Fatal("healthy tenant port did not stay up after another port conflicted")
	}
}
