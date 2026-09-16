package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/session"
)

func TestLogoutRequiresPostOriginAndRevokesAllMatchingCookies(t *testing.T) {
	p, _, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	token := issue(t, p, "alice", nil)
	for _, input := range []struct {
		method, origin string
		status         int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "", http.StatusForbidden},
		{http.MethodPost, "null", http.StatusForbidden},
		{http.MethodPost, "https://dsh.test:32601", http.StatusForbidden},
		{http.MethodPost, "https://dsh.test:32600", http.StatusSeeOther},
	} {
		req := httptest.NewRequest(input.method, "/logout", nil)
		req.Host = "dsh.test:32600"
		if input.origin != "" {
			req.Header.Set("Origin", input.origin)
		}
		req.Header.Set("Cookie", "dshgw_s_alice=poison; dshgw_s_alice="+token)
		w := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(w, req)
		if w.Code != input.status {
			t.Fatalf("%+v status=%d", input, w.Code)
		}
		_, err := p.Sessions.Get(token)
		if input.status == http.StatusSeeOther {
			if !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("logout did not revoke second duplicate: %v", err)
			}
		} else if err != nil {
			t.Fatalf("rejected logout changed session: %v", err)
		}
	}
}
