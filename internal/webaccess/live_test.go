package webaccess

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A live test against the real internet. It never runs in CI or `go test ./...`: it stays
// skipped unless GW_WEBACCESS_LIVE=1.
//
// It exists because two of the risks here cannot be covered by a fixture:
//
//   - the Bing backend parses HTML rather than JSON, so "the page still looks like the
//     fixture" is a claim about a live third-party page that only the live page can settle;
//   - fetching is the half that talks to arbitrary sites, so it is worth seeing real text
//     come back (and how much) at least once per backend change.
//
// The assertions are deliberately loose — at least one result with a title and an http(s)
// URL, and a fetch of it that returns some text. Whether the text is *good* is a judgement
// call, so the findings are logged for a human to read:
//
//	GW_WEBACCESS_LIVE=1 go test ./internal/webaccess/ -run TestLiveSearchAndFetch -v
//
// Optional knobs: GW_WEBACCESS_PROVIDER (default bing), GW_WEBACCESS_BASE_URL,
// GW_WEBACCESS_API_KEY, GW_WEBACCESS_QUERY, GW_WEBACCESS_TARGET (the page to fetch),
// GW_WEBACCESS_PROXY (for a deployment whose only way out is a proxy).
func TestLiveSearchAndFetch(t *testing.T) {
	if os.Getenv("GW_WEBACCESS_LIVE") != "1" {
		t.Skip("GW_WEBACCESS_LIVE is not set; this test talks to the public internet")
	}
	env := func(key, fallback string) string {
		if value := os.Getenv(key); value != "" {
			return value
		}
		return fallback
	}
	provider := env("GW_WEBACCESS_PROVIDER", ProviderBing)
	baseURL := os.Getenv("GW_WEBACCESS_BASE_URL")
	apiKey := os.Getenv("GW_WEBACCESS_API_KEY")
	query := env("GW_WEBACCESS_QUERY", "DeepSeek API 模型价格")
	target := os.Getenv("GW_WEBACCESS_TARGET")

	client, err := New(Config{
		Provider:   provider,
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Proxy:      os.Getenv("GW_WEBACCESS_PROXY"),
		Timeout:    20 * time.Second,
		MaxResults: 5,
	}, nil)
	if err != nil {
		t.Fatalf("New(%s): %v", provider, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Search was called with the operator's raw term on purpose: no query rewriting exists
	// (it is on the 不做 list), so what this shows is what a model would get.
	found, err := client.Search(ctx, query, 5, FreshnessNone)
	if err != nil {
		t.Fatalf("Search(%q) via %s: %v", query, provider, err)
	}
	t.Logf("provider=%s query=%q count=%d note=%q", found.Provider, found.Query, found.Count, found.Note)
	for i, item := range found.Items {
		t.Logf("  [%d] %s\n      %s\n      %s", i+1, item.Title, item.URL, item.Snippet)
	}
	if len(found.Items) == 0 {
		t.Fatalf("no results at all — the %s backend answered but nothing parsed (note=%q)", provider, found.Note)
	}
	first := found.Items[0]
	if !strings.HasPrefix(first.URL, "http://") && !strings.HasPrefix(first.URL, "https://") {
		t.Errorf("first result is not an absolute http(s) URL: %q", first.URL)
	}
	if strings.TrimSpace(first.Title) == "" {
		t.Errorf("first result has no title: %+v", first)
	}
	// A note usually means "the page structure moved" — that is the signal this test is for,
	// so it is worth failing loudly on rather than printing and moving on.
	if found.Note != "" {
		t.Errorf("%s returned a note: %s", provider, found.Note)
	}

	if target == "" {
		target = first.URL
	}
	page, err := client.Fetch(ctx, target)
	if err != nil {
		t.Fatalf("Fetch(%q): %v", target, err)
	}
	text := strings.TrimSpace(page.Content)
	t.Logf("fetched %s (%s, %d bytes, truncated=%v) title=%q",
		page.FinalURL, page.ContentType, page.Bytes, page.Truncated, page.Title)
	t.Logf("first 400 characters of text:\n%s", firstN(text, 400))
	if len([]rune(text)) < 80 {
		t.Errorf("only %d characters of text came back from %s — the extractor found almost nothing",
			len([]rune(text)), page.FinalURL)
	}
}

func firstN(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
