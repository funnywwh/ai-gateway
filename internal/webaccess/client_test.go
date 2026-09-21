package webaccess

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// newTestClient builds a client against a local test server. allowPrivate is on because the
// server lives on 127.0.0.1 — the guard's own behaviour is covered in guard_test.go, and every
// other test here is about what the backends send and how their answers are read.
func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	cfg.AllowPrivateHosts = true
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	client, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return client
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(body)
}

// TestClientValidatesItsConfiguration: a broken setup must fail when it is built, not when a
// model asks its first question.
func TestClientValidatesItsConfiguration(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no provider":       {},
		"unknown provider":  {Provider: "google"},
		"searxng no url":    {Provider: ProviderSearxNG},
		"bocha no key":      {Provider: ProviderBocha},
		"tavily no key":     {Provider: ProviderTavily},
		"bad base url":      {Provider: ProviderSearxNG, BaseURL: "ftp://searx.example"},
		"bad proxy":         {Provider: ProviderBing, Proxy: "127.0.0.1:1080"},
		"results too high":  {Provider: ProviderBing, MaxResults: MaxResultsLimit + 1},
		"text above bytes":  {Provider: ProviderBing, FetchMaxBytes: 100, FetchMaxTextBytes: 200},
		"negative timeout":  {Provider: ProviderBing, Timeout: -time.Second},
		"negative download": {Provider: ProviderBing, FetchMaxBytes: -1},
	} {
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("%s: New must reject the configuration", name)
		}
	}
	client, err := New(Config{Provider: ProviderBing}, nil)
	if err != nil {
		t.Fatalf("New(bing): %v", err)
	}
	if client.cfg.BaseURL != "https://cn.bing.com" || client.cfg.MaxResults != DefaultMaxResults ||
		client.cfg.FetchMaxBytes != DefaultFetchMaxBytes || client.cfg.FetchMaxTextBytes != DefaultFetchMaxTextBytes {
		t.Fatalf("defaults not applied: %+v", client.cfg)
	}
}

// TestSearchClampsAndNormalizesInput covers the model-facing half of Search: the query must be
// usable, the count can only be lowered, and the freshness vocabulary is closed.
func TestSearchClampsAndNormalizesInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fixture(t, "searxng.json"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderSearxNG, BaseURL: server.URL, MaxResults: 3})

	ctx := context.Background()
	if _, err := client.Search(ctx, "   ", 0, ""); err == nil {
		t.Error("an empty query must be refused")
	}
	if _, err := client.Search(ctx, strings.Repeat("字", maxQueryRunes+1), 0, ""); err == nil {
		t.Error("an overlong query must be refused")
	}
	if _, err := client.Search(ctx, "价格", 0, "lastFortnight"); err == nil {
		t.Error("an unknown freshness value must be refused rather than ignored")
	}
	result, err := client.Search(ctx, "深度求索 价格", 99, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.Count != 2 || len(result.Items) != 2 {
		t.Fatalf("count = %d items = %d", result.Count, len(result.Items))
	}
	if result.Provider != ProviderSearxNG || result.Query != "深度求索 价格" {
		t.Errorf("result metadata = %+v", result)
	}
	if result.Items[0].URL != "https://api-docs.deepseek.com/quick_start/pricing" ||
		result.Items[0].Title == "" || result.Items[0].Snippet == "" || result.Items[0].Published == "" {
		t.Errorf("first item = %+v", result.Items[0])
	}
}

// TestSearxNGSearchBuildsTheDocumentedRequest pins the JSON-API call: a wrong parameter here
// is the difference between "no results" and "the instance answered".
func TestSearxNGSearchBuildsTheDocumentedRequest(t *testing.T) {
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = io.WriteString(w, fixture(t, "searxng.json"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderSearxNG, BaseURL: server.URL})

	if _, err := client.Search(context.Background(), "深度求索", 4, FreshnessWeek); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, want := range []string{"/search?", "format=json", "q=%E6%B7%B1%E5%BA%A6%E6%B1%82%E7%B4%A2", "time_range=week"} {
		if !strings.Contains(gotURL, want) {
			t.Errorf("request URL %q must contain %q", gotURL, want)
		}
	}
}

// TestSearxNGExplainsAMissingJSONFormat: the instance must opt in to JSON, and the error has
// to say so — this is the most common searxng misconfiguration.
func TestSearxNGExplainsAMissingJSONFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html>403</html>")
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderSearxNG, BaseURL: server.URL})
	_, err := client.Search(context.Background(), "价格", 3, "")
	if err == nil || !strings.Contains(err.Error(), "settings.yml") {
		t.Fatalf("error = %v, want a hint about the json format", err)
	}
}

// TestBochaSearchSendsTheCredentialAndReadsTheLongSummary checks the bocha contract: bearer
// auth, a JSON body, and the summary field preferred over the short snippet.
func TestBochaSearchSendsTheCredentialAndReadsTheLongSummary(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, fixture(t, "bocha.json"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBocha, BaseURL: server.URL, APIKey: "bocha-secret"})

	result, err := client.Search(context.Background(), "深度求索 价格", 5, FreshnessMonth)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "Bearer bocha-secret" || gotPath != "/v1/web-search" {
		t.Errorf("request went to %q with auth %q", gotPath, gotAuth)
	}
	if gotBody["freshness"] != "oneMonth" || gotBody["query"] != "深度求索 价格" || gotBody["summary"] != true {
		t.Errorf("body = %v", gotBody)
	}
	if len(result.Items) != 2 {
		t.Fatalf("items = %+v", result.Items)
	}
	if !strings.Contains(result.Items[0].Snippet, "长文本摘要") {
		t.Errorf("the long summary must win over the short snippet: %+v", result.Items[0])
	}
	if result.Items[1].Snippet != "只有短描述时用它" {
		t.Errorf("second item = %+v", result.Items[1])
	}
}

// TestTavilySearchDeduplicatesTrailingSlashes: the same page twice costs the model context,
// and "URL with and without a trailing slash" is the shape that actually happens.
func TestTavilySearchDeduplicatesTrailingSlashes(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, fixture(t, "tavily.json"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderTavily, BaseURL: server.URL, APIKey: "tvly-secret", MaxResults: 10})

	result, err := client.Search(context.Background(), "deepseek pricing", 5, FreshnessDay)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotBody["time_range"] != "day" || gotBody["search_depth"] != "basic" {
		t.Errorf("body = %v", gotBody)
	}
	if len(result.Items) != 1 {
		t.Fatalf("duplicate URLs must collapse: %+v", result.Items)
	}
	if result.Items[0].Published != "2026-09-15" {
		t.Errorf("published = %q", result.Items[0].Published)
	}
}

// TestSearchDropsUnusableItemsAndReportsEmptyResults: a backend that answers with a relative
// URL or with nothing at all must not look like a successful search.
func TestSearchDropsUnusableItemsAndReportsEmptyResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[
			{"title":"relative","url":"/docs","content":"x"},
			{"title":"script","url":"javascript:alert(1)","content":"x"},
			{"title":"no url","content":"x"},
			{"title":"good","url":"https://example.com/a","content":"ok","engine":"bing"}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderSearxNG, BaseURL: server.URL})
	result, err := client.Search(context.Background(), "anything", 5, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].URL != "https://example.com/a" {
		t.Fatalf("items = %+v", result.Items)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer empty.Close()
	client = newTestClient(t, Config{Provider: ProviderSearxNG, BaseURL: empty.URL})
	result, err = client.Search(context.Background(), "nothing", 5, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.Count != 0 || result.Note == "" {
		t.Fatalf("an empty answer must be explained: %+v", result)
	}
}

// TestSearchReportsBackendFailures: the operator needs to know which knob to turn, so the
// status-specific hints are part of the contract.
func TestSearchReportsBackendFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"unauthorized": {http.StatusUnauthorized, `{"message":"Invalid API KEY"}`, "chat.web_access.api_key"},
		"no money":     {http.StatusForbidden, `{"message":"You do not have enough money"}`, "余额不足"},
		"limited":      {http.StatusTooManyRequests, `{"message":"rate limited"}`, "限流"},
		"server error": {http.StatusInternalServerError, `boom`, "500"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := newTestClient(t, Config{Provider: ProviderTavily, BaseURL: server.URL, APIKey: "k"})
			_, err := client.Search(context.Background(), "q", 3, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSearchReportsUnparseableAnswers: "the backend answered with something else" (a captive
// portal, a wrong base_url) must not be reported as "no results".
func TestSearchReportsUnparseableAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>login required</html>")
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBocha, BaseURL: server.URL, APIKey: "k"})
	if _, err := client.Search(context.Background(), "q", 3, ""); err == nil {
		t.Fatal("a non-JSON answer must be an error")
	}
}

// TestSearchReportsTimeouts: a hanging backend must produce a sentence a model can act on
// instead of a bare context error.
func TestSearchReportsTimeouts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderSearxNG, BaseURL: server.URL, Timeout: 50 * time.Millisecond})
	_, err := client.Search(context.Background(), "q", 3, "")
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("error = %v, want a timeout message", err)
	}
}

// TestBingSearchParsesACapturedResultPage pins the fallback backend against a real page
// captured on 2026-09-21. When Bing changes its markup this test is the alarm, and the failure
// message says what to do about it.
//
// The capture is the *unclosed* shape on purpose: the page has 23 "<li" against 3 "</li>", and
// the first version of the parser — which walked nesting depth to find the matching </li> —
// merged all three results into one on this exact page, with the rest of the document as the
// snippet (found by the live test, not by the older fixture, whose tags happened to be closed).
func TestBingSearchParsesACapturedResultPage(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, fixture(t, "bing_search.html"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})

	result, err := client.Search(context.Background(), "DeepSeek API 模型价格", 3, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/search" || !strings.Contains(gotQuery, "count=3") || !strings.Contains(gotQuery, "setlang=zh-CN") {
		t.Errorf("request = %s?%s", gotPath, gotQuery)
	}
	// Every result in the list is one item: nothing may merge into its neighbour, which is what
	// the unclosed-list regression looked like (1 item instead of 3).
	if len(result.Items) != 3 {
		t.Fatalf("items = %d, want 3: %+v", len(result.Items), result.Items)
	}
	want := []struct{ title, url, source string }{
		{"杭州深度求索人工智能基础技术研究有限公司_百度百科", "https://baike.baidu.com/item/%E6%9D%AD%E5%B7%9E%E6%B7%B1%E5%BA%A6%E6%B1%82%E7%B4%A2%E4%BA%BA%E5%B7%A5%E6%99%BA%E8%83%BD%E5%9F%BA%E7%A1%80%E6%8A%80%E6%9C%AF%E7%A0%94%E7%A9%B6%E6%9C%89%E9%99%90%E5%85%AC%E5%8F%B8/64541110", "baike.baidu.com"},
		{"DeepSeek | API Platform", "https://www.deepseek.com/en/platform/", "www.deepseek.com"},
		{"DeepSeek App", "https://download.deepseek.com/", "download.deepseek.com"},
	}
	for i, expected := range want {
		item := result.Items[i]
		if item.Title != expected.title || item.URL != expected.url {
			t.Errorf("item %d = %q / %q, want %q / %q", i+1, item.Title, item.URL, expected.title, expected.url)
		}
		if item.Snippet == "" {
			t.Errorf("item %d has no snippet: %+v", i+1, item)
		}
		if item.Source != expected.source {
			t.Errorf("item %d source = %q, want %q", i+1, item.Source, expected.source)
		}
	}
	// A snippet that swallowed the rest of the page is the other half of that regression: the
	// cap keeps it a teaser whatever the markup does.
	for i, item := range result.Items {
		if utf8.RuneCountInString(item.Snippet) > maxItemSnippetRunes+1 {
			t.Errorf("item %d snippet has %d runes (cap %d)", i+1, utf8.RuneCountInString(item.Snippet), maxItemSnippetRunes)
		}
	}
	if strings.Count(result.Items[0].Snippet, "deepseek.com") > 1 {
		t.Errorf("item 1 snippet leaked later results: %q", result.Items[0].Snippet)
	}
	// The page's own chrome must not become a result.
	for _, item := range result.Items {
		if strings.Contains(item.URL, "bing.com") {
			t.Errorf("a Bing-internal URL was reported as a source: %+v", item)
		}
	}
}

// TestBingSearchSplitsResultsWithUnclosedListItems states the markup rule on its own, so a
// future rewrite of the parser fails here with the reason instead of only in the fixture test.
func TestBingSearchSplitsResultsWithUnclosedListItems(t *testing.T) {
	page := `<ol id="b_results">` +
		`<li class="b_algo"><h2><a href="https://a.example/">A</a></h2><p>one</p>` +
		`<ul><li>nested</li></ul>` + // a nested item inside an *unclosed* result
		`<li class="b_algo"><h2><a href="https://b.example/">B</a></h2>` +
		`<li class="b_algo"><h2><a href="https://c.example/">C</a></h2><p>three</p>` +
		`</ol>`
	blocks := bingResultBlocks(page)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3: %+v", len(blocks), blocks)
	}
	for i, want := range []string{"https://a.example/", "https://b.example/", "https://c.example/"} {
		item, ok := bingItem(blocks[i])
		if !ok {
			t.Fatalf("block %d did not parse: %q", i, blocks[i])
		}
		if item.URL != want {
			t.Errorf("block %d url = %q, want %q", i, item.URL, want)
		}
	}
}

// TestBingResultBlocksStayInsideTheResultsList: the sidebars and the rail reuse the result
// markup, so the block scan has to be bounded by <ol id="b_results">.
func TestBingResultBlocksStayInsideTheResultsList(t *testing.T) {
	page := `<html><body><ol id="b_context"><li class="b_algo"><h2><a href="https://rail.example/">rail</a></h2></li></ol>` +
		`<ol id="b_results"><li class="b_algo"><h2><a href="https://real.example/">real</a></h2></li></ol>` +
		`<ol id="b_dynRail"><li class="b_algo"><h2><a href="https://dyn.example/">dyn</a></h2></li></ol></body></html>`
	blocks := bingResultBlocks(page)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1: %+v", len(blocks), blocks)
	}
	item, ok := bingItem(blocks[0])
	if !ok || item.URL != "https://real.example/" {
		t.Errorf("item = %+v (ok=%v)", item, ok)
	}
}

// TestClipRunesKeepsASnippetATeaser: the cap is what stops a mis-parse from pouring a page into
// the prompt, and it must not cut a multi-byte character in half.
func TestClipRunesKeepsASnippetATeaser(t *testing.T) {
	long := strings.Repeat("字", maxItemSnippetRunes+50)
	clipped := clipRunes(long, maxItemSnippetRunes)
	if utf8.RuneCountInString(clipped) != maxItemSnippetRunes+1 || !strings.HasSuffix(clipped, "…") {
		t.Errorf("clipped length = %d (tail %q)", utf8.RuneCountInString(clipped), clipped[len(clipped)-6:])
	}
	if !utf8.ValidString(clipped) {
		t.Error("clipping produced invalid UTF-8")
	}
	if short := clipRunes("短", maxItemSnippetRunes); short != "短" {
		t.Errorf("clipRunes changed a short string: %q", short)
	}
}

// TestBingSearchExplainsAnUnparseablePage: a consent page or a layout change must be reported
// as "this backend needs replacing", not as "there is no such information on the internet".
func TestBingSearchExplainsAnUnparseablePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body><div>Before you continue to Bing</div></body></html>")
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})
	result, err := client.Search(context.Background(), "深度求索", 3, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.Count != 0 || !strings.Contains(result.Note, "结构可能又变了") {
		t.Fatalf("result = %+v", result)
	}
}

// TestBingSearchSaysWhenItIgnoresFreshness: silently ignoring the time range would look like
// "there is no recent news".
func TestBingSearchSaysWhenItIgnoresFreshness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fixture(t, "bing_search.html"))
	}))
	defer server.Close()
	client := newTestClient(t, Config{Provider: ProviderBing, BaseURL: server.URL})
	result, err := client.Search(context.Background(), "深度求索", 3, FreshnessDay)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(result.Note, "不支持按时间范围过滤") {
		t.Fatalf("note = %q", result.Note)
	}
}

// TestBingTargetURLUnwrapsTheClickTracker: Bing wraps some links in /ck/a?u=a1<base64url>, and
// a model citing bing.com/ck/a would be citing a tracker rather than a source.
func TestBingTargetURLUnwrapsTheClickTracker(t *testing.T) {
	// base64url("https://example.com/docs") without padding.
	encoded := "a1aHR0cHM6Ly9leGFtcGxlLmNvbS9kb2Nz"
	for _, raw := range []string{
		"https://www.bing.com/ck/a?!&&p=abc&u=" + encoded,
		"https://cn.bing.com/ck/a?u=" + encoded + "&ntb=1",
	} {
		if got := bingTargetURL(raw); got != "https://example.com/docs" {
			t.Errorf("bingTargetURL(%q) = %q", raw, got)
		}
	}
	for _, raw := range []string{
		"https://example.com/plain",
		"https://www.bing.com/ck/a?u=a1not-base64!!",
		"https://www.bing.com/ck/a?u=",
	} {
		if got := bingTargetURL(raw); got != raw {
			t.Errorf("bingTargetURL(%q) = %q, want it unchanged", raw, got)
		}
	}
}
