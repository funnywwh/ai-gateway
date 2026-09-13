package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRelayRoundTrip(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request headers")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "notifications/") {
			w.WriteHeader(204)
			return
		}
		if string(body) != `{"jsonrpc":"2.0","id":"hello","method":"ping"}` {
			t.Errorf("body=%s", body)
		}
		io.WriteString(w, "{\n\"jsonrpc\":\"2.0\",\"id\":\"hello\",\"result\":{}}")
	}))
	defer server.Close()
	var out bytes.Buffer
	err := serveMCPRelay(context.Background(), strings.NewReader("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":\"hello\",\"method\":\"ping\"}"), &out, server.URL, "secret", server.Client())
	if err != nil || calls != 2 || out.String() != `{"jsonrpc":"2.0","id":"hello","result":{}}`+"\n" {
		t.Fatalf("calls=%d out=%q err=%v", calls, out.String(), err)
	}
}
func TestRelayRejectsEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/mcp", "https://u:secret@example.com/mcp", "https://example.com/mcp?token=secret", "https://example.com/mcp#x", "file:///tmp/x", "https:///mcp"} {
		if validateMCPEndpoint(endpoint) == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	for _, endpoint := range []string{"https://example.com/aigw/mcp", "http://127.0.0.1:8088/mcp", "http://[::1]:8088/mcp", "http://localhost/mcp"} {
		if err := validateMCPEndpoint(endpoint); err != nil {
			t.Error(err)
		}
	}
}
func TestRelayErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"http", "secret", 401}, {"invalid", "secret", 200}, {"array", "[]", 200}, {"empty", "", 200}, {"wrong_id", `{"jsonrpc":"2.0","id":2,"result":{}}`, 200}, {"oversize", strings.Repeat("x", mcpRelayMaxBytes+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer s.Close()
			var out bytes.Buffer
			err := serveMCPRelay(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), &out, s.URL, "secret", s.Client())
			if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("out=%q err=%v", out.String(), err)
			}
		})
	}
}
func TestRelayRedirectAndOversizedInput(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer s.Close()
	for _, input := range []string{`{"jsonrpc":"2.0","id":1,"method":"ping"}`, strings.Repeat("x", mcpRelayMaxBytes+1)} {
		if err := serveMCPRelay(context.Background(), strings.NewReader(input), io.Discard, s.URL, "secret", s.Client()); err == nil {
			t.Fatal("expected rejection")
		}
	}
	if calls != 0 {
		t.Fatal("followed redirect")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func TestRelayCancellationAndOutputError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer s.Close()
	input := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if err := serveMCPRelay(context.Background(), strings.NewReader(input), brokenWriter{}, s.URL, "secret", s.Client()); err == nil {
		t.Fatal("ignored write failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serveMCPRelay(ctx, strings.NewReader(input), io.Discard, s.URL, "secret", s.Client()); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestRelayCLIValidation(t *testing.T) {
	t.Setenv("GW_MCP_TOKEN", "")
	for _, args := range [][]string{{"--endpoint", "http://localhost/mcp", "--account", "a"}, {"--endpoint", "http://localhost/mcp"}, {"--endpoint", "http://example.com/mcp"}, {"unexpected"}} {
		if code := runMCPServe(args); code != 2 {
			t.Errorf("args=%v code=%d", args, code)
		}
	}
}
