package webaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/winger/ai-gateway/pkg/providerkit"
)

// Client is one deployment's web access client: one search backend plus a guarded fetcher.
// It is safe for concurrent use.
type Client struct {
	cfg    Config
	http   *http.Client
	guard  *guard
	search searchFunc
	log    *slog.Logger
}

// searchFunc is one backend's implementation. The four backends differ only in the request
// they build and the JSON they read, so everything else (counting, deduping, note writing)
// happens once in Search.
type searchFunc func(ctx context.Context, c *Client, query string, count int, freshness string) (SearchResult, error)

// New builds a client. It validates the configuration and fails loudly rather than degrading
// to a direct connection or an unbounded download: a malformed search setup must surface at
// start-up, not as "the model cannot find anything" during an incident.
func New(cfg Config, log *slog.Logger) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	if cfg.FetchMaxTextBytes > cfg.FetchMaxBytes {
		cfg.FetchMaxTextBytes = cfg.FetchMaxBytes
	}
	g := newGuard(cfg.AllowPrivateHosts)
	proxy, err := proxyFunc(cfg.Proxy)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 proxy,
		DialContext:           g.dial,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	c := &Client{
		cfg:   cfg,
		guard: g,
		log:   log,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			// Every hop is re-checked: a public URL that redirects to 127.0.0.1 must be
			// refused at the hop, not merely at the URL the model typed.
			CheckRedirect: redirectPolicy(g),
		},
	}
	switch cfg.Provider {
	case ProviderSearxNG:
		c.search = searchSearxNG
	case ProviderBocha:
		c.search = searchBocha
	case ProviderTavily:
		c.search = searchTavily
	case ProviderBing:
		c.search = searchBing
	default:
		return nil, fmt.Errorf("unknown web access provider %q", cfg.Provider)
	}
	return c, nil
}

// Provider reports which backend this client searches with.
func (c *Client) Provider() string { return c.cfg.Provider }

// MaxResults reports the deployment's cap on hits per search. It is what the tool schema
// advertises, so the model asks for a number the gateway will actually return.
func (c *Client) MaxResults() int { return c.cfg.MaxResults }

// maxRedirects bounds a redirect chain; five hops covers the usual http→https→www dance while
// keeping "follow the links forever" impossible.
const maxRedirects = 5

// proxyFunc turns the configured value into net/http's proxy callback. An empty value is a
// direct connection and deliberately ignores HTTPS_PROXY/NO_PROXY (a deployment that never
// set a proxy must not start using one after an upgrade); the literal "env" opts in.
func proxyFunc(raw string) (func(*http.Request) (*url.URL, error), error) {
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "":
		return nil, nil
	case strings.EqualFold(trimmed, "env"):
		return http.ProxyFromEnvironment, nil
	}
	u, err := providerkit.ParseProxyURL(trimmed)
	if err != nil {
		return nil, err
	}
	return http.ProxyURL(u), nil
}

// Search runs one query through the configured backend.
//
// count is clamped to [1, MaxResults]: the model may ask for fewer results than the
// deployment allows, never for more. freshness must be one of FreshnessValues.
func (c *Client) Search(ctx context.Context, query string, count int, freshness string) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResult{}, fmt.Errorf("检索词不能为空")
	}
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return SearchResult{}, fmt.Errorf("检索词过长（上限 %d 个字符）", maxQueryRunes)
	}
	freshness, err := normalizeFreshness(freshness)
	if err != nil {
		return SearchResult{}, err
	}
	if count <= 0 {
		count = c.cfg.MaxResults
	}
	if count > c.cfg.MaxResults {
		count = c.cfg.MaxResults
	}
	result, err := c.search(ctx, c, query, count, freshness)
	if err != nil {
		return SearchResult{}, err
	}
	result.Provider = c.cfg.Provider
	result.Query = query
	result.Items = dedupeItems(result.Items, count)
	result.Count = len(result.Items)
	if len(result.Items) == 0 && result.Note == "" {
		result.Note = "后端没有返回任何结果，可以换一个检索词，或直接抓取已知的 URL。"
	}
	return result, nil
}

// maxQueryRunes bounds one query. Search backends themselves cap far below this; the point is
// to refuse a runaway prompt before it becomes an HTTP 414.
const maxQueryRunes = 512

// normalizeFreshness accepts the shared vocabulary and rejects anything else, because a
// silently ignored time range would look like "there is no recent news".
func normalizeFreshness(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return freshnessDefault, nil
	}
	for _, allowed := range FreshnessValues() {
		if value == allowed {
			return value, nil
		}
	}
	return "", fmt.Errorf("时间范围只支持 %s", strings.Join(FreshnessValues(), " / "))
}

// dedupeItems trims, drops unusable and duplicate hits, and applies the result cap. Backends
// happily return the same URL twice (once as the site root, once with a trailing slash), and a
// model that reads it twice has spent context for nothing.
func dedupeItems(items []Item, limit int) []Item {
	out := make([]Item, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		item.Title = strings.TrimSpace(item.Title)
		item.URL = strings.TrimSpace(item.URL)
		item.Snippet = strings.TrimSpace(item.Snippet)
		item.Source = strings.TrimSpace(item.Source)
		item.Published = strings.TrimSpace(item.Published)
		if item.URL == "" {
			continue
		}
		parsed, err := url.Parse(item.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			// A backend that hands back a relative or javascript: URL is not a source a
			// model can cite, so the entry is dropped rather than passed through.
			continue
		}
		key := strings.TrimSuffix(item.URL, "/")
		if seen[key] {
			continue
		}
		seen[key] = true
		if item.Title == "" {
			item.Title = parsed.Host
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Fetch downloads one public page and returns its readable text.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	target, err := c.guard.checkURL(strings.TrimSpace(rawURL))
	if err != nil {
		return Page{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("这个地址无法请求：%v", err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, fetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Page{}, backendStatusError(c.cfg.Provider, "抓取", resp)
	}

	// One byte past the limit is read on purpose: it is how truncation is detected without
	// trusting Content-Length (which is often missing for compressed responses).
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.cfg.FetchMaxBytes)+1))
	if err != nil {
		return Page{}, fmt.Errorf("读取页面内容失败：%v", err)
	}
	truncatedDownload := len(body) > c.cfg.FetchMaxBytes
	if truncatedDownload {
		body = body[:c.cfg.FetchMaxBytes]
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, charset := splitContentType(contentType)
	// The document may declare its own charset when the header does not (and plenty of sites
	// serve UTF-8 while claiming something else, which utf8.Valid below settles).
	if charset == "" && (mediaType == "text/html" || mediaType == "application/xhtml+xml" || mediaType == "") {
		charset = declaredCharset(string(body))
	}
	if !isUTF8Name(charset) && !utf8.Valid(body) {
		return Page{}, fmt.Errorf("这个页面声明的编码是 %s，网关只按 UTF-8 解析，无法读取它的正文", charset)
	}

	page := Page{
		URL:         target.String(),
		FinalURL:    resp.Request.URL.String(),
		ContentType: mediaType,
		Bytes:       len(body),
	}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml" || (mediaType == "" && looksLikeHTML(body)):
		extracted := extractHTML(body)
		page.Title = extracted.Title
		page.Content = extracted.Text
	case strings.HasPrefix(mediaType, "text/") ||
		mediaType == "application/json" || mediaType == "application/xml" || mediaType == "application/x-yaml":
		page.Content = string(body)
	case mediaType == "":
		page.Content = string(body)
	default:
		return Page{}, fmt.Errorf("这个地址返回的是 %s（不是文本内容），网关不解析这类文件；把链接直接给用户更合适", mediaType)
	}

	if page.Content == "" {
		if page.Note == "" {
			page.Note = "这个页面没有可读正文"
		}
		return page, nil
	}
	if cut, wasCut := truncateUTF8(page.Content, c.cfg.FetchMaxTextBytes); wasCut {
		page.Content = cut
		page.Truncated = true
		page.Note = joinNotes(page.Note, fmt.Sprintf("正文超过 %d 字节，已截断", c.cfg.FetchMaxTextBytes))
	}
	if truncatedDownload {
		// Truncated means "what you got is a prefix of the page", which is true whether the
		// cut happened while downloading or while trimming the extracted text.
		page.Truncated = true
		page.Note = joinNotes(page.Note, fmt.Sprintf("页面超过 %d 字节，只读取了前一部分", c.cfg.FetchMaxBytes))
	}
	return page, nil
}

// redirectPolicy decides whether one hop of a redirect chain may be followed: the count is
// bounded and every target is re-checked by the guard, because a public URL that redirects into
// private space is the classic way around a check that only looked at the first URL.
func redirectPolicy(g *guard) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("跳转次数超过 %d 次，已停止", maxRedirects)
		}
		_, err := g.checkURL(req.URL.String())
		return err
	}
}

// looksLikeHTML sniffs a body served without a Content-Type: a search result or a CMS page
// mislabelled as octet-stream is still exactly what the model asked for.
func looksLikeHTML(body []byte) bool {
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(string(head))
	return strings.Contains(lower, "<!doctype html") || strings.Contains(lower, "<html")
}

// splitContentType returns the media type (lowercased, parameters stripped) and the charset.
func splitContentType(raw string) (string, string) {
	mediaType := strings.TrimSpace(raw)
	charset := ""
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		params := mediaType[idx+1:]
		mediaType = mediaType[:idx]
		for _, part := range strings.Split(params, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(part), "charset=") {
				charset = strings.Trim(strings.TrimSpace(part[len("charset="):]), `"'`)
			}
		}
	}
	return strings.ToLower(mediaType), charset
}

// truncateUTF8 cuts text at a rune boundary so the model never receives half a character.
func truncateUTF8(text string, limit int) (string, bool) {
	if limit <= 0 || len(text) <= limit {
		return text, false
	}
	cut := text[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

func joinNotes(existing, extra string) string {
	switch {
	case extra == "":
		return existing
	case existing == "":
		return extra
	default:
		return existing + "；" + extra
	}
}

// fetchError turns a transport failure into a sentence the model can act on. Timeouts and
// DNS failures are the two that need naming: "context deadline exceeded" tells nobody
// anything.
func fetchError(err error) error {
	var netErr net.Error
	if ok := asNetError(err, &netErr); ok && netErr.Timeout() {
		return fmt.Errorf("抓取超时，这个站点响应太慢或不可达")
	}
	if errorsIsContext(err) {
		return fmt.Errorf("抓取已取消")
	}
	if strings.Contains(err.Error(), "no such host") {
		return fmt.Errorf("域名解析失败，这个地址可能不存在")
	}
	if strings.Contains(err.Error(), "certificate") {
		return fmt.Errorf("站点的 TLS 证书校验失败，网关拒绝继续")
	}
	return fmt.Errorf("抓取失败：%v", err)
}

func asNetError(err error, target *net.Error) bool {
	for err != nil {
		if netErr, ok := err.(net.Error); ok {
			*target = netErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func errorsIsContext(err error) bool {
	for err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// backendStatusError renders a non-2xx backend response. The backend's own message is kept
// (it says "you do not have enough money" far better than a status code does) but capped, and
// the status-specific hint says what the operator should check.
func backendStatusError(provider, action string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := decodeBackendMessage(body)
	hint := ""
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		hint = "（检查 chat.web_access.api_key）"
	case http.StatusForbidden:
		hint = "（密钥无权限或后端账户余额不足）"
	case http.StatusTooManyRequests:
		hint = "（后端限流，稍后重试或减少 max_calls_per_turn）"
	case http.StatusNotFound:
		hint = "（检查 chat.web_access.base_url）"
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("联网后端 %s %s失败：%s %s%s", provider, action, resp.Status, message, hint)
}

// decodeBackendMessage pulls a human message out of an error body, trying the field names the
// four backends use.
func decodeBackendMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var payload map[string]any
		if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
			for _, key := range []string{"message", "msg", "error", "detail", "error_description"} {
				if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
				if nested, ok := payload[key].(map[string]any); ok {
					if value, ok := nested["message"].(string); ok && strings.TrimSpace(value) != "" {
						return strings.TrimSpace(value)
					}
				}
			}
		}
	}
	if len(trimmed) > 200 {
		trimmed = trimmed[:200] + "…"
	}
	return trimmed
}
