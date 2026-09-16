package aigw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateKeySemantics(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    []string
		invalid bool
	}{{"invalid", 401, `{"error":"x"}`, nil, true}, {"empty is valid", 200, `{"data":[]}`, []string{}, false}, {"models", 200, `{"data":[{"id":"a"},{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}, false}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer sk-abcdefghijkl" {
					t.Errorf("authorization=%q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			got, err := (&Client{BaseURL: srv.URL, HTTP: srv.Client()}).ValidateKey(context.Background(), "sk-abcdefghijkl")
			if tt.invalid {
				if !errors.Is(err, ErrInvalidKey) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got=%v", got)
			}
		})
	}
}
func TestValidateKeyOtherStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL}).ValidateKey(context.Background(), "sk-abcdefghijkl")
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 503 {
		t.Fatalf("err=%v", err)
	}
}
func TestKeyPrefix(t *testing.T) {
	p, err := KeyPrefix("123456789012rest")
	if err != nil || p != "123456789012" {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := KeyPrefix("short"); err == nil {
		t.Fatal("short accepted")
	}
}

func TestValidateKeyRejectsRedirectMissingDataAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"redirect", 302, ""}, {"missing-data", 200, `{}`}, {"oversize", 200, strings.Repeat("x", (1<<20)+1)}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 302 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := (&Client{BaseURL: srv.URL}).ValidateKey(context.Background(), "sk-abcdefghijkl"); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestNormalizeKey(t *testing.T) {
	got, err := NormalizeKey("  Bearer sk-abcdefghijkl  ")
	if err != nil || got != "sk-abcdefghijkl" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for _, bad := range []string{"sk-abc defghijkl", "密钥abcdefghijkl"} {
		if _, err := NormalizeKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
