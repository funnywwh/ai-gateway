package webaccess

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// bing is the zero-credential fallback: it reads Bing's HTML result page instead of an API.
//
// It exists so a deployment can be brought up and verified before anyone buys a search key.
// It is also the one backend that can break without anyone touching this repository, which is
// why (a) the parse is pinned by a captured fixture in testdata/, (b) the provider is marked
// "not recommended for production" in config.example.yaml and docs/chat.md, and (c) a page we
// cannot parse says so in the tool result instead of quietly returning nothing.
func searchBing(ctx context.Context, c *Client, query string, count int, freshness string) (SearchResult, error) {
	endpoint, err := url.Parse(c.cfg.BaseURL + "/search")
	if err != nil {
		return SearchResult{}, fmt.Errorf("base_url 不是合法地址：%v", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", count))
	params.Set("setlang", "zh-CN")
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return SearchResult{}, fmt.Errorf("无法构造搜索请求：%v", err)
	}
	// No cookies and no referer: the gateway is a reader, not a signed-in browser.
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	resp, err := c.http.Do(req)
	if err != nil {
		return SearchResult{}, fetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SearchResult{}, backendStatusError(ProviderBing, "搜索", resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return SearchResult{}, fmt.Errorf("读取 Bing 结果页失败：%v", err)
	}

	result := SearchResult{}
	blocks := bingResultBlocks(string(body))
	for _, block := range blocks {
		item, ok := bingItem(block)
		if !ok {
			continue
		}
		result.Items = append(result.Items, item)
	}
	if len(result.Items) == 0 {
		result.Note = "没能从 Bing 结果页里解析出结果：页面结构可能又变了。可以让运维换一个后端（searxng / bocha / tavily）。"
	}
	if freshness != freshnessDefault {
		result.Note = joinNotes(result.Note, "bing 后端不支持按时间范围过滤，这一条时间范围没有被使用。")
	}
	return result, nil
}

// bingResultBlocks returns the HTML of each organic result. Bing nests <li> elements inside a
// result (breadcrumbs, related links), so blocks are delimited by the matching </li> rather
// than by the next opening tag.
func bingResultBlocks(body string) []string {
	lower := strings.ToLower(body)
	blocks := make([]string, 0, 16)
	for i := 0; i < len(body); {
		next := strings.Index(lower[i:], "<li")
		if next < 0 {
			break
		}
		start := i + next
		end := tagEnd(body, start)
		if end < 0 {
			break
		}
		attrs := parseAttributes(body[start+1 : end])
		if !hasClass(attrs["class"], "b_algo") {
			i = end + 1
			continue
		}
		blockEnd := matchingListEnd(lower, end+1)
		if blockEnd < 0 {
			blockEnd = len(body)
		}
		blocks = append(blocks, body[start:blockEnd])
		i = blockEnd
	}
	return blocks
}

// matchingListEnd finds the </li> that closes a list item opened before offset, honouring
// nested <li> elements.
func matchingListEnd(lower string, offset int) int {
	depth := 1
	for i := offset; i < len(lower); {
		next := strings.Index(lower[i:], "<li")
		closeIdx := strings.Index(lower[i:], "</li")
		if next < 0 && closeIdx < 0 {
			return -1
		}
		switch {
		case closeIdx >= 0 && (next < 0 || closeIdx < next):
			depth--
			i += closeIdx
			if depth == 0 {
				return i
			}
			i += len("</li")
		default:
			// A nested item may still be a self-closing or void-ish tag; treating every "<li"
			// as an open item is the conservative reading.
			depth++
			i += next + len("<li")
		}
	}
	return -1
}

// hasClass reports whether a class attribute contains one token.
func hasClass(class, want string) bool {
	for _, token := range strings.Fields(class) {
		if token == want {
			return true
		}
	}
	return false
}

// bingItem reads one result block: the first heading link is the result, its <cite> is the
// displayed site and the first paragraph is the snippet.
func bingItem(block string) (Item, bool) {
	title, target, ok := bingHeadingLink(block)
	if !ok {
		return Item{}, false
	}
	item := Item{Title: title, URL: bingTargetURL(target), Source: bingCite(block), Snippet: bingSnippet(block)}
	if item.URL == "" {
		return Item{}, false
	}
	return item, true
}

// bingHeadingLink finds the first <h2> in the block and reads the anchor inside it.
func bingHeadingLink(block string) (title, href string, ok bool) {
	lower := strings.ToLower(block)
	start := strings.Index(lower, "<h2")
	if start < 0 {
		return "", "", false
	}
	end := tagEnd(block, start)
	if end < 0 {
		return "", "", false
	}
	headingEnd := strings.Index(lower[end:], "</h2>")
	if headingEnd < 0 {
		headingEnd = len(block) - end
	}
	heading := block[end+1 : end+headingEnd]

	anchorStart := strings.Index(strings.ToLower(heading), "<a")
	if anchorStart < 0 {
		return "", "", false
	}
	anchorEnd := tagEnd(heading, anchorStart)
	if anchorEnd < 0 {
		return "", "", false
	}
	attrs := parseAttributes(heading[anchorStart+1 : anchorEnd])
	href = strings.TrimSpace(attrs["href"])
	if href == "" {
		return "", "", false
	}
	textEnd := strings.Index(strings.ToLower(heading[anchorEnd:]), "</a>")
	if textEnd < 0 {
		textEnd = len(heading) - anchorEnd
	}
	title = tidyTitle(extractHTML([]byte(heading[anchorEnd+1 : anchorEnd+textEnd])).Text)
	if title == "" {
		title = tidyTitle(extractHTML([]byte(heading)).Text)
	}
	return title, href, true
}

// bingTargetURL unwraps Bing's click tracker. A result link is either the real URL or
// https://www.bing.com/ck/a?…&u=a1<base64url of the real URL>; both shapes are handled, and a
// value that does not decode is kept as-is rather than dropped.
func bingTargetURL(raw string) string {
	raw = strings.TrimSpace(decodeEntities(raw))
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	if !strings.HasSuffix(strings.ToLower(parsed.Hostname()), "bing.com") || !strings.HasPrefix(parsed.Path, "/ck/a") {
		return raw
	}
	encoded := parsed.Query().Get("u")
	if !strings.HasPrefix(encoded, "a1") {
		return raw
	}
	payload := encoded[2:]
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.DecodeString(payload); err == nil {
			if strings.HasPrefix(string(decoded), "http") {
				return string(decoded)
			}
		}
	}
	return raw
}

// bingCite reads the displayed site name. Bing renders it as "docs.example.com › guide", so
// the crumbs are turned back into a readable path separator.
func bingCite(block string) string {
	lower := strings.ToLower(block)
	start := strings.Index(lower, "<cite")
	if start < 0 {
		return ""
	}
	end := tagEnd(block, start)
	if end < 0 {
		return ""
	}
	closeIdx := strings.Index(lower[end:], "</cite>")
	if closeIdx < 0 {
		return ""
	}
	title := tidyTitle(extractHTML([]byte(block[end+1 : end+closeIdx])).Text)
	return strings.ReplaceAll(title, " › ", "/")
}

// bingSnippet reads the first paragraph of the block. Bing puts the snippet in a <p> inside
// the caption div; when the layout uses a div instead, the fallback strips whatever follows the
// cite so the model still gets the teaser text.
func bingSnippet(block string) string {
	lower := strings.ToLower(block)
	searchFrom := 0
	if citeStart := strings.Index(lower, "<cite"); citeStart >= 0 {
		if citeEnd := strings.Index(lower[citeStart:], "</cite>"); citeEnd >= 0 {
			searchFrom = citeStart + citeEnd
		}
	}
	rest := block[searchFrom:]
	restLower := strings.ToLower(rest)
	for _, tag := range []string{"<p", "<div class=\"b_caption\"", "<div class=\"b_lineclamp"} {
		start := strings.Index(restLower, tag)
		if start < 0 {
			continue
		}
		end := tagEnd(rest, start)
		if end < 0 {
			continue
		}
		text := tidyTitle(extractHTML([]byte(rest[end+1:])).Text)
		if text != "" {
			return text
		}
	}
	return ""
}
