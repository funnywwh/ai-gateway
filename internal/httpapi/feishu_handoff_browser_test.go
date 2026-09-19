package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const browserHandoffCookie = "aigw_dshgw_ticket"

// TestFeishuHandoffBrowser uses only private TLS fixtures and disposable browser
// profiles. The fixtures record actual received headers, not CDP's preliminary
// request headers, which can omit browser-generated Fetch Metadata and cookies.
func TestFeishuHandoffBrowser(t *testing.T) {
	if os.Getenv("AIGW_FEISHU_BROWSER_TEST") != "1" {
		t.Skip("set AIGW_FEISHU_BROWSER_TEST=1 to run the isolated Chromium regression")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the explicitly enabled browser test")
	}
	fixture := newFeishuHandoffBrowserFixture(t)
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate browser test source")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	args := []string{
		filepath.Join(repo, "scripts", "feishu_handoff_browser_test.py"),
		"--idp-url", fixture.idpURL,
		"--callback-url", fixture.callbackURL,
		"--portal-url", fixture.portalURL,
		"--tenant-url", fixture.tenantURL,
		"--host-rules", "MAP idp.test 127.0.0.1,MAP chat.test 127.0.0.1",
	}
	if chrome := strings.TrimSpace(os.Getenv("CHROME_BIN")); chrome != "" {
		args = append(args, "--chrome", chrome)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	// Give Python's finally blocks a chance to stop its private Chrome process
	// group if the Go test times out, rather than orphaning browsers with SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 8 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser regression failed: %v\n%s", err, output)
	}
	t.Logf("actual browser request metadata:\n%s", output)
}

type handoffBrowserObservation struct {
	Role         string `json:"role"`
	Status       int    `json:"status"`
	Site         string `json:"site"`
	Mode         string `json:"mode"`
	Dest         string `json:"dest"`
	Origin       bool   `json:"origin"`
	TicketCookie bool   `json:"ticketCookie"`
}

type feishuHandoffBrowserFixture struct {
	idp, callback, portal, tenant             *httptest.Server
	idpURL, callbackURL, portalURL, tenantURL string
	mu                                        sync.Mutex
	observations                              []handoffBrowserObservation
}

func newFeishuHandoffBrowserFixture(t *testing.T) *feishuHandoffBrowserFixture {
	t.Helper()
	f := &feishuHandoffBrowserFixture{}
	f.idp = httptest.NewTLSServer(http.HandlerFunc(f.idpHandler))
	f.callback = httptest.NewTLSServer(http.HandlerFunc(f.callbackHandler))
	f.portal = httptest.NewTLSServer(http.HandlerFunc(f.portalHandler))
	f.tenant = httptest.NewTLSServer(http.HandlerFunc(f.tenantHandler))
	t.Cleanup(func() {
		f.idp.Close()
		f.callback.Close()
		f.portal.Close()
		f.tenant.Close()
	})
	f.idpURL = browserTLSOrigin(f.idp.URL, "idp.test") + "/start"
	f.callbackURL = browserTLSOrigin(f.callback.URL, "chat.test") + "/callback"
	f.portalURL = browserTLSOrigin(f.portal.URL, "chat.test")
	f.tenantURL = browserTLSOrigin(f.tenant.URL, "chat.test")
	return f
}

func browserTLSOrigin(raw, hostname string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		panic(fmt.Sprintf("bad test TLS URL %q", raw))
	}
	return "https://" + hostname + ":" + u.Port()
}

func (f *feishuHandoffBrowserFixture) observe(role string, r *http.Request, status int) {
	cookiePresent := false
	if cookie, err := r.Cookie(browserHandoffCookie); err == nil {
		cookiePresent = cookie.Value == "fixture-ticket-value"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observations = append(f.observations, handoffBrowserObservation{
		Role: role, Status: status, Site: r.Header.Get("Sec-Fetch-Site"),
		Mode: r.Header.Get("Sec-Fetch-Mode"), Dest: r.Header.Get("Sec-Fetch-Dest"),
		Origin: len(r.Header.Values("Origin")) > 0, TicketCookie: cookiePresent,
	})
}

func (f *feishuHandoffBrowserFixture) idpHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/start" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	mode := "legacy"
	if r.URL.Query().Get("mode") == "handoff" {
		mode = "handoff"
	}
	target := f.callbackURL + "?mode=" + mode + "&code=fixture-code&state=fixture-state"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A physical click after this document has loaded supplies a real unrelated
	// site initiator; navigating directly to callback would falsely produce none.
	_, _ = io.WriteString(w, `<!doctype html><html><head><title>fixture IdP</title></head><body><a id="idp-continue" href="`+html.EscapeString(target)+`">Continue</a></body></html>`)
}

func (f *feishuHandoffBrowserFixture) callbackHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_observations" {
		f.mu.Lock()
		rows := append([]handoffBrowserObservation{}, f.observations...)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	if r.URL.Path != "/callback" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: browserHandoffCookie, Value: "fixture-ticket-value", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 120,
	})
	target := f.portalURL + "/login/feishu"
	if r.URL.Query().Get("mode") == "handoff" {
		status := http.StatusSeeOther
		if feishuHandoffNeedsDocument(r, f.callbackURL, target) {
			status = http.StatusOK
		}
		f.observe("callback", r, status)
		redirectFeishuHandoff(w, r, f.callbackURL, target)
		return
	}
	f.observe("callback", r, http.StatusSeeOther)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (f *feishuHandoffBrowserFixture) portalHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login/feishu" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusSeeOther
	if _, err := r.Cookie(browserHandoffCookie); err != nil {
		status = http.StatusUnauthorized
	}
	f.observe("portal", r, status)
	if status != http.StatusSeeOther {
		http.Error(w, "fixture cookie missing", status)
		return
	}
	// Deliberately record rather than duplicate the actual dshgw fence. Its unit
	// regression proves the observed cross-site baseline would be rejected.
	http.Redirect(w, r, f.tenantURL+"/", http.StatusSeeOther)
}

func (f *feishuHandoffBrowserFixture) tenantHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusOK
	if _, err := r.Cookie(browserHandoffCookie); err != nil {
		status = http.StatusUnauthorized
	}
	f.observe("tenant", r, status)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html><body><p id="tenant-reached">fixture tenant reached</p></body></html>`)
}
