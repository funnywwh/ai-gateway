package main

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guard exists because a stale absolute socket path is silent until a person's login
// fails (M62/M63); these tests pin what it says and, just as importantly, when it stays
// quiet.
func TestWarnIfAdminSocketMissing(t *testing.T) {
	dir := t.TempDir()

	live := filepath.Join(dir, "admin.sock")
	listener, err := net.Listen("unix", live)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	regular := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cases := []struct {
		name       string
		socket     string
		supervised bool
		wantWarn   string
	}{
		{"live socket is silent", live, false, ""},
		{"missing socket warns", filepath.Join(dir, "gone.sock"), false, "dshgw admin channel is missing"},
		{"non-socket in the way warns", regular, false, "not a socket"},
		{"empty path is silent", "", false, ""},
		// The child binds the socket only after aigw serves, so silence is correct here.
		{"supervised shape is silent", filepath.Join(dir, "gone.sock"), true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			warnIfAdminSocketMissing(log, tc.socket, tc.supervised)

			got := buf.String()
			if tc.wantWarn == "" {
				if got != "" {
					t.Fatalf("expected no log output, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantWarn) {
				t.Fatalf("expected a warning containing %q, got %q", tc.wantWarn, got)
			}
			if !strings.Contains(got, "level=WARN") {
				t.Fatalf("expected WARN level, got %q", got)
			}
		})
	}
}
