package handshake

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestExchangeManualRedirectAndFixedAuthority(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landed" {
			followed = true
			return
		}
		if r.Host != strings.TrimPrefix(srvURL(t, r), "http://") {
			t.Fatalf("host=%q", r.Host)
		}
		http.SetCookie(w, &http.Cookie{Name: "dsh-auth-abc", Value: "upstream", HttpOnly: true})
		w.Header().Set("Location", "/landed")
		w.WriteHeader(303)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	got, err := (&HTTPExchanger{HTTP: srv.Client()}).Exchange(context.Background(), srv.URL+"/?token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	if followed || got.Name != "dsh-auth-abc" || got.Value != "upstream" || got.Authority != u.Host {
		t.Fatalf("followed=%v got=%#v", followed, got)
	}
}
func srvURL(t *testing.T, r *http.Request) string { t.Helper(); return "http://" + r.Host }
func TestParseTokenURLRejectsUnsafeForms(t *testing.T) {
	for _, raw := range []string{"https://127.0.0.1:1/?token=x", "http://example.com:1/?token=x", "http://127.0.0.1:1/x?token=x", "http://127.0.0.1:1/?token=x&x=1", "http://127.0.0.1:1/?token=x&token=y", "http://127.0.0.1:1/"} {
		if _, err := ParseTokenURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
func TestExchangeRequires303AndOneCookie(t *testing.T) {
	for _, status := range []int{200, 302, 401} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		u, _ := url.Parse(srv.URL)
		_, err := (&HTTPExchanger{}).Exchange(context.Background(), srv.URL+"/?token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", u.Host)
		srv.Close()
		if err == nil {
			t.Errorf("accepted %d", status)
		}
	}
}
