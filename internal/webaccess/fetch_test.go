package webaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestFetchExtractsReadableText is the happy path: HTML in, prose out, with the page's own
// title kept so the model can name its source.
func TestFetchExtractsReadableText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Error("a request without a User-Agent is refused by many sites")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>价格页</title><script>track()</script></head>
			<body><h1>价格</h1><p>输入 0.5 元</p></body></html>`))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})

	page, err := client.Fetch(context.Background(), server.URL+"/pricing")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if page.Title != "价格页" || !strings.Contains(page.Content, "输入 0.5 元") {
		t.Fatalf("page = %+v", page)
	}
	if strings.Contains(page.Content, "track()") {
		t.Errorf("script text must not reach the model: %q", page.Content)
	}
	if page.ContentType != "text/html" || page.Truncated || page.Bytes == 0 {
		t.Errorf("page metadata = %+v", page)
	}
}

// TestFetchTruncatesAtARuneBoundary: the cap must never split a multi-byte character, because
// half a rune is invalid text in every downstream view.
func TestFetchTruncatesAtARuneBoundary(t *testing.T) {
	body := "<html><body><p>" + strings.Repeat("深度求索", 500) + "</p></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL, FetchMaxTextBytes: 101})

	page, err := client.Fetch(context.Background(), server.URL+"/long")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !page.Truncated {
		t.Fatal("a page longer than fetch_max_text_bytes must be marked truncated")
	}
	if len(page.Content) > 101 {
		t.Errorf("content length = %d", len(page.Content))
	}
	if !utf8.ValidString(page.Content) {
		t.Errorf("the cut must land on a rune boundary: %q", page.Content)
	}
	if !strings.Contains(page.Note, "截断") {
		t.Errorf("note = %q", page.Note)
	}
}

// TestFetchKeepsPlainTextAndJSON: not every source is HTML, and a README or an API response is
// exactly what a model wants to read.
func TestFetchKeepsPlainTextAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readme":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("# 标题\n\n正文"))
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"price":0.5}`))
		default:
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.7"))
		}
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})

	page, err := client.Fetch(context.Background(), server.URL+"/readme")
	if err != nil || !strings.Contains(page.Content, "正文") {
		t.Fatalf("plain text: page = %+v err = %v", page, err)
	}
	page, err = client.Fetch(context.Background(), server.URL+"/api")
	if err != nil || !strings.Contains(page.Content, `"price"`) {
		t.Fatalf("json: page = %+v err = %v", page, err)
	}
	_, err = client.Fetch(context.Background(), server.URL+"/doc.pdf")
	if err == nil || !strings.Contains(err.Error(), "不是文本内容") {
		t.Fatalf("a PDF must be refused with an explanation, got %v", err)
	}
}

// TestFetchRefusesNonUTF8Pages: the extractor decodes UTF-8 only, and mojibake is worse than a
// clear "I cannot read this".
func TestFetchRefusesNonUTF8Pages(t *testing.T) {
	gbk := []byte{0xc9, 0xee, 0xb6, 0xc8, 0xc7, 0xf3, 0xcb, 0xf7} // "深度求索" in GBK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		_, _ = w.Write(append([]byte("<html><body><p>"), gbk...))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})

	_, err := client.Fetch(context.Background(), server.URL+"/gbk")
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error = %v, want an encoding explanation", err)
	}
}

// TestFetchTrustsTheBytesOverTheDeclaration: plenty of sites declare a legacy charset and serve
// UTF-8 anyway. Refusing those would refuse half the web for no reason.
func TestFetchTrustsTheBytesOverTheDeclaration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=gb2312")
		_, _ = w.Write([]byte("<html><head><title>中文</title></head><body><p>正文</p></body></html>"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})

	page, err := client.Fetch(context.Background(), server.URL+"/liar")
	if err != nil || !strings.Contains(page.Content, "正文") {
		t.Fatalf("page = %+v err = %v", page, err)
	}
}

// TestFetchFollowsRedirects checks the happy half of a redirect: the page's real address is
// reported as FinalURL, because a model citing the address it asked for would cite a redirect.
func TestFetchFollowsRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>最终页面</body></html>"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})
	page, err := client.Fetch(context.Background(), server.URL+"/start")
	if err != nil || !strings.Contains(page.Content, "最终页面") {
		t.Fatalf("page = %+v err = %v", page, err)
	}
	if !strings.HasSuffix(page.FinalURL, "/final") || page.URL == page.FinalURL {
		t.Errorf("final url = %q (from %q)", page.FinalURL, page.URL)
	}
}

// TestRedirectPolicyChecksEveryHop is the other half, tested on the policy itself: a public URL
// that redirects into private space is the classic way around a check that only inspected the
// URL the model typed.
func TestRedirectPolicyChecksEveryHop(t *testing.T) {
	g := newGuard(false)
	g.resolver = &fakeResolver{answers: map[string][]string{"public.example": {"93.184.216.34"}}}
	policy := redirectPolicy(g)

	hop := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("bad test URL %q: %v", raw, err)
		}
		return req
	}
	for _, raw := range []string{
		"http://127.0.0.1/admin",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1:8088/",
		"http://localhost:9200/_cat/indices",
		"file:///etc/passwd",
	} {
		if err := policy(hop(raw), []*http.Request{hop("https://public.example/")}); err == nil {
			t.Errorf("redirect to %q must be refused", raw)
		}
	}
	if err := policy(hop("https://public.example/final"), []*http.Request{hop("https://public.example/")}); err != nil {
		t.Errorf("a public https hop must be allowed: %v", err)
	}
	// Six hops already taken: the chain is abandoned rather than followed.
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = hop("https://public.example/hop")
	}
	if err := policy(hop("https://public.example/next"), via); err == nil || !strings.Contains(err.Error(), "跳转次数") {
		t.Errorf("error = %v, want the redirect limit", err)
	}
}

// TestFetchStopsRunawayRedirectChains: five hops is a courtesy, not a maze solver.
func TestFetchStopsRunawayRedirectChains(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/next", http.StatusFound)
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})
	_, err := client.Fetch(context.Background(), server.URL+"/loop")
	if err == nil || !strings.Contains(err.Error(), "跳转次数") {
		t.Fatalf("error = %v", err)
	}
}

// TestFetchBoundsTheDownload: a page bigger than the cap is read partially and says so, rather
// than filling memory or silently returning half a document.
func TestFetchBoundsTheDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL, FetchMaxBytes: 512})
	page, err := client.Fetch(context.Background(), server.URL+"/big")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !page.Truncated || page.Bytes != 512 {
		t.Fatalf("page = %+v", page)
	}
	if !strings.Contains(page.Note, "页面超过") {
		t.Errorf("note = %q", page.Note)
	}
}

// TestFetchReportsStatusesAndTimeouts: the two failures an operator will actually meet.
func TestFetchReportsStatusesAndTimeouts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})

	_, err := client.Fetch(context.Background(), server.URL+"/missing")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v", err)
	}
	slow := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL, Timeout: 50 * time.Millisecond})
	if _, err := slow.Fetch(context.Background(), server.URL+"/slow"); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("timeout error = %v", err)
	}
}

// TestFetchRejectsEmptyTargetsAndBadSchemes keeps the guard wired into the fetch path.
func TestFetchRejectsEmptyTargetsAndBadSchemes(t *testing.T) {
	client := newTestClient(t, Config{Provider: ProviderBing})
	for _, raw := range []string{"", "   ", "ftp://example.com/x", "https://user:pw@example.com/"} {
		if _, err := client.Fetch(context.Background(), raw); err == nil {
			t.Errorf("Fetch(%q) must be refused", raw)
		}
	}
}
