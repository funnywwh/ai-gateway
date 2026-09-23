package nodeup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func testTenant(node string) registry.Tenant {
	return registry.Tenant{
		Name: "alice", UID: 1000, PublicPort: 32601, WorkerPort: 32900,
		DshHome: "/node/tenants/alice/.dsh", Workspace: "/node/srv/alice",
		CreatedAt: time.Now().UTC(), Handshake: registry.HandshakeOK, Node: node,
	}
}

// handshakeNode answers the one operation this package drives.
func handshakeNode(t *testing.T, respond func(authority string) (int, any), token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(nodeproto.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		nodeproto.WriteValue(w, http.StatusOK, nodeproto.Health{Name: "node-a", Protocol: nodeproto.Version})
	})
	mux.HandleFunc(nodeproto.ControlPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "bad token")
			return
		}
		var request nodeproto.TenantHandshakeRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		status, value := respond(request.Authority)
		if protoErr, ok := value.(*nodeproto.Error); ok {
			nodeproto.WriteError(w, status, protoErr.Code, protoErr.Message)
			return
		}
		nodeproto.WriteValue(w, status, value)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestNodeForDistinguishesLocalFromRemote(t *testing.T) {
	server := handshakeNode(t, func(string) (int, any) { return http.StatusOK, nil }, "tok")
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: server.URL, Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	service := New(&config.Config{}, set, nil)

	if _, remote := service.NodeFor(testTenant("")); remote {
		t.Fatal("a tenant with no placement is local")
	}
	if _, remote := service.NodeFor(testTenant(config.LocalNodeName)); remote {
		t.Fatal("the literal local is local")
	}
	ref, remote := service.NodeFor(testTenant("node-a"))
	if !remote || ref.Name != "node-a" || ref.BaseURL != server.URL || ref.Token != "tok" {
		t.Fatalf("ref = %+v remote = %t", ref, remote)
	}
	if _, remote := service.NodeFor(testTenant("node-z")); remote {
		t.Fatal("an undefined node must not resolve: the proxy refuses it instead")
	}
	// A single-machine deployment has no clients at all.
	single := New(&config.Config{}, nil, nil)
	if _, remote := single.NodeFor(testTenant("node-a")); remote {
		t.Fatal("a deployment without node clients must report no remote tenants")
	}
	if !single.ServesBrowserWorkspaces(testTenant("")) {
		t.Fatal("a local tenant's mounts are served here")
	}
	if service.ServesBrowserWorkspaces(testTenant("node-a")) {
		t.Fatal("a remote tenant's mounts live on its node")
	}
}

func TestHandshakeReturnsTheNodesCookie(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	server := handshakeNode(t, func(authority string) (int, any) {
		return http.StatusOK, nodeproto.TenantHandshakeResult{
			Name: "dsh-auth-test", Value: "cookie-value", Authority: authority, ExpiresAt: expires,
		}
	}, "tok")
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: server.URL, Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	service := New(&config.Config{}, set, nil)
	authority := "127.0.0.1:32900"
	upstream, err := service.Handshake(context.Background(), testTenant("node-a"), authority)
	if err != nil {
		t.Fatal(err)
	}
	if upstream.Name != "dsh-auth-test" || upstream.Value != "cookie-value" || upstream.Authority != authority {
		t.Fatalf("upstream = %+v", upstream)
	}
	if !upstream.ExpiresAt.Equal(expires) {
		t.Fatalf("expiry = %s, want %s", upstream.ExpiresAt, expires)
	}
}

// TestHandshakeRejectsAMismatchedAuthority is the failure this check exists for: dsh binds the
// cookie to the authority, so a node that handshook under another one would hand back a credential
// the forwarded requests could never use.
func TestHandshakeRejectsAMismatchedAuthority(t *testing.T) {
	server := handshakeNode(t, func(string) (int, any) {
		return http.StatusOK, nodeproto.TenantHandshakeResult{
			Name: "dsh-auth-test", Value: "cookie-value", Authority: "127.0.0.1:9999",
		}
	}, "tok")
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: server.URL, Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(&config.Config{}, set, nil).Handshake(context.Background(), testTenant("node-a"), "127.0.0.1:32900")
	if !nodeproto.IsCode(err, nodeproto.CodeProtocolMismatch) {
		t.Fatalf("err = %v (code %q)", err, nodeproto.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Fatalf("the message must name what the node used: %v", err)
	}
}

func TestHandshakeRejectsAnEmptyCredential(t *testing.T) {
	server := handshakeNode(t, func(authority string) (int, any) {
		return http.StatusOK, nodeproto.TenantHandshakeResult{Authority: authority}
	}, "tok")
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: server.URL, Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(&config.Config{}, set, nil).Handshake(context.Background(), testTenant("node-a"), "127.0.0.1:32900")
	if !nodeproto.IsCode(err, nodeproto.CodeInternal) {
		t.Fatalf("err = %v", err)
	}
}

func TestHandshakeForAnUndefinedNode(t *testing.T) {
	set, err := nodeclient.NewSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(&config.Config{}, set, nil).Handshake(context.Background(), testTenant("node-z"), "127.0.0.1:32900")
	if !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "node-z") {
		t.Fatalf("the message must name the node: %v", err)
	}
}

// TestHandshakePropagatesNodeRefusal keeps the failure classification the proxy branches on: a
// node whose worker is gone reports worker_not_running, an unreachable one node_unreachable.
func TestHandshakePropagatesNodeRefusal(t *testing.T) {
	server := handshakeNode(t, func(string) (int, any) {
		return http.StatusServiceUnavailable, nodeproto.Errorf(nodeproto.CodeWorkerNotRunning, "no startup token")
	}, "tok")
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: server.URL, Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(&config.Config{}, set, nil).Handshake(context.Background(), testTenant("node-a"), "127.0.0.1:32900"); !nodeproto.IsCode(err, nodeproto.CodeWorkerNotRunning) {
		t.Fatalf("err = %v", err)
	}
	server.Close()
	if _, err := New(&config.Config{}, set, nil).Handshake(context.Background(), testTenant("node-a"), "127.0.0.1:32900"); !nodeproto.IsCode(err, nodeproto.CodeUnreachable) {
		t.Fatalf("err = %v", err)
	}
}
