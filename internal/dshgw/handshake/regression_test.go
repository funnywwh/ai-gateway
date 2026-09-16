package handshake

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorsNeverExposeTokenURL(t *testing.T) {
	token := strings.Repeat("a", 43)
	exchanger := &HTTPExchanger{HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("failed URL: " + r.URL.String())
	})}}
	_, err := exchanger.Exchange(context.Background(), "http://127.0.0.1:32100/?token="+token, "127.0.0.1:32100")
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "?token") {
		t.Fatalf("unsafe transport error: %v", err)
	}
	_, err = ParseTokenURL("http://127.0.0.1:32100/?token=" + token + "%zz")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("unsafe parser error: %v", err)
	}
}

func TestExchangeRejectsConflictingOrExpiredCookies(t *testing.T) {
	for _, value := range []string{"none", "duplicate", "deleted", "expired"} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cookie := &http.Cookie{Name: "dsh-auth-one", Value: "held"}
				switch value {
				case "deleted":
					cookie.MaxAge = -1
				case "expired":
					cookie.Expires = time.Unix(1, 0)
				case "duplicate":
					http.SetCookie(w, &http.Cookie{Name: "dsh-auth-two", Value: "other"})
				}
				if value != "none" {
					http.SetCookie(w, cookie)
				}
				w.WriteHeader(http.StatusSeeOther)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			if _, err := (&HTTPExchanger{}).Exchange(context.Background(), server.URL+"/?token="+strings.Repeat("a", 43), u.Host); err == nil {
				t.Fatal("invalid auth cookie accepted")
			}
		})
	}
}

func TestCanonicalAuthorityRequired(t *testing.T) {
	for _, authority := range []string{"127.0.0.1:0", "127.0.0.1:032100", "127.0.0.1:65536", "127.0.0.2:32100", "[::1]:32100", "localhost:32100"} {
		if _, err := ParseTokenURL("http://" + authority + "/?token=" + strings.Repeat("a", 43)); err == nil {
			t.Errorf("accepted %s", authority)
		}
	}
}
