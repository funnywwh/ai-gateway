package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
)

// nodeStub answers like a node agent: the token gate, the health endpoint and one control op.
func nodeStub(t *testing.T, token string, health func() any, control func(op string, body []byte) (int, any)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(nodeproto.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "bad token")
			return
		}
		nodeproto.WriteValue(w, http.StatusOK, health())
	})
	mux.HandleFunc(nodeproto.ControlPath, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "bad token")
			return
		}
		op := strings.TrimPrefix(r.URL.Path, nodeproto.ControlPath)
		body, _ := readAll(r)
		status, value := control(op, body)
		switch typed := value.(type) {
		case *nodeproto.Error:
			nodeproto.WriteError(w, status, typed.Code, typed.Message)
		default:
			nodeproto.WriteValue(w, status, value)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func authorized(r *http.Request, token string) bool {
	presented, ok := nodeproto.BearerToken(r.Header.Get("Authorization"))
	return ok && nodeproto.TokenMatches(token, presented)
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

func TestHealthAndProbe(t *testing.T) {
	server := nodeStub(t, "secret", func() any {
		return nodeproto.Health{Name: "node-a", Protocol: nodeproto.Version, Revision: "abc1234", Version: "9.9.9"}
	}, func(string, []byte) (int, any) { return http.StatusOK, map[string]any{"ok": true} })

	client := New("node-a", server.URL, "secret")
	health, err := client.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Name != "node-a" || health.Revision != "abc1234" || health.Protocol != nodeproto.Version {
		t.Fatalf("health = %+v", health)
	}
	if client.BaseAddress() != server.URL {
		t.Fatalf("BaseAddress = %q", client.BaseAddress())
	}
}

func TestAuthFailure(t *testing.T) {
	server := nodeStub(t, "secret", func() any { return nodeproto.Health{} }, func(string, []byte) (int, any) {
		return http.StatusOK, map[string]any{}
	})
	client := New("node-a", server.URL, "wrong")
	_, err := client.Probe(context.Background())
	if !nodeproto.IsCode(err, nodeproto.CodeAuthFailed) {
		t.Fatalf("err = %v (code %q)", err, nodeproto.CodeOf(err))
	}
	if err := client.Ping(context.Background()); !nodeproto.IsCode(err, nodeproto.CodeAuthFailed) {
		t.Fatalf("ping err = %v", err)
	}
}

// TestProtocolMismatch covers the three ways a version disagreement shows up: a node that
// announces another protocol, a node that speaks another major version (404 on the versioned
// path) and a node that answers 426.
func TestProtocolMismatch(t *testing.T) {
	t.Run("health announces another protocol", func(t *testing.T) {
		server := nodeStub(t, "secret", func() any {
			return nodeproto.Health{Name: "node-a", Protocol: nodeproto.Version + 1}
		}, func(string, []byte) (int, any) { return http.StatusOK, map[string]any{} })
		_, err := New("node-a", server.URL, "secret").Probe(context.Background())
		if !nodeproto.IsCode(err, nodeproto.CodeProtocolMismatch) {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(err.Error(), "deploy") {
			t.Fatalf("the message should say what to do: %v", err)
		}
	})
	t.Run("404 on the versioned path", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<html>not a node</html>"))
		}))
		t.Cleanup(server.Close)
		_, err := New("node-a", server.URL, "secret").Probe(context.Background())
		if !nodeproto.IsCode(err, nodeproto.CodeProtocolMismatch) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("426 from the gate", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nodeproto.WriteError(w, http.StatusUpgradeRequired, nodeproto.CodeProtocolMismatch, "another version")
		}))
		t.Cleanup(server.Close)
		_, err := New("node-a", server.URL, "secret").Probe(context.Background())
		if !nodeproto.IsCode(err, nodeproto.CodeProtocolMismatch) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNameMismatchIsReported(t *testing.T) {
	server := nodeStub(t, "secret", func() any {
		return nodeproto.Health{Name: "node-b", Protocol: nodeproto.Version}
	}, func(string, []byte) (int, any) { return http.StatusOK, map[string]any{} })
	_, err := New("node-a", server.URL, "secret").Probe(context.Background())
	if !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("the message must name both sides: %v", err)
	}
}

func TestUnreachableAndTimeout(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := server.URL
		server.Close()
		_, err := New("node-a", url, "secret").Probe(context.Background())
		if !nodeproto.IsCode(err, nodeproto.CodeUnreachable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("slow node", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		t.Cleanup(func() { close(release); server.Close() })
		client := New("node-a", server.URL, "secret")
		client.HealthTimeout = 50 * time.Millisecond
		_, err := client.Probe(context.Background())
		if !nodeproto.IsCode(err, nodeproto.CodeUnreachable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unusable address", func(t *testing.T) {
		_, err := New("node-a", "192.168.190.87:18400", "secret").Probe(context.Background())
		if !nodeproto.IsCode(err, nodeproto.CodeUnreachable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCallMapsNodeErrorsAndDecodesValues(t *testing.T) {
	server := nodeStub(t, "secret", func() any { return nodeproto.Health{Name: "node-a"} },
		func(op string, body []byte) (int, any) {
			switch op {
			case "boom":
				return http.StatusServiceUnavailable, nodeproto.Errorf(nodeproto.CodeWorkerNotRunning, "tenant %s has no worker", "alice")
			case "unreadable":
				return http.StatusOK, "not-an-object"
			case "status":
				var request map[string]string
				if len(body) > 0 {
					_ = json.Unmarshal(body, &request)
				}
				return http.StatusOK, nodeproto.Status{
					Health:  nodeproto.Health{Name: "node-a", Protocol: nodeproto.Version, Tenants: 2, Running: 1},
					Tenants: []nodeproto.TenantState{{Name: "alice", WorkerPort: 32100, Running: true}},
				}
			default:
				return http.StatusOK, map[string]any{"op": op, "body": string(body)}
			}
		})

	client := New("node-a", server.URL, "secret")
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	err := client.Call(context.Background(), "boom", nil, nil)
	if !nodeproto.IsCode(err, nodeproto.CodeWorkerNotRunning) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "node-a") || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("the message must name the node and the tenant: %v", err)
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Health.Tenants != 2 || status.Health.Running != 1 || len(status.Tenants) != 1 || status.Tenants[0].Name != "alice" {
		t.Fatalf("status = %+v", status)
	}
	// A value that is not the shape the caller asked for must be an error, not a zero value.
	var out nodeproto.Status
	if err := client.Call(context.Background(), "unreadable", nil, &out); err == nil {
		t.Fatal("expected a decode error")
	}
	// The request body must reach the node unchanged.
	var echoed struct {
		Op   string `json:"op"`
		Body string `json:"body"`
	}
	if err := client.Call(context.Background(), "echo", map[string]any{"name": "alice"}, &echoed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(echoed.Body, `"name":"alice"`) {
		t.Fatalf("echo = %+v", echoed)
	}
}

func TestCallRejectsUnencodableRequest(t *testing.T) {
	client := New("node-a", "http://127.0.0.1:1", "secret")
	err := client.Call(context.Background(), "create", make(chan int), nil)
	if !nodeproto.IsCode(err, nodeproto.CodeInternal) {
		t.Fatalf("err = %v", err)
	}
}

func TestServerWithoutEnvelopeIsInternal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("plain text"))
	}))
	t.Cleanup(server.Close)
	_, err := New("node-a", server.URL, "secret").Probe(context.Background())
	if !nodeproto.IsCode(err, nodeproto.CodeInternal) {
		t.Fatalf("err = %v", err)
	}
}
