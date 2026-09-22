package webaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// searxng is the backend for a self-hosted metasearch instance: no credential, no quota, and
// the operator decides which engines it queries. Its JSON API is off by default, which is the
// one configuration mistake worth naming in the error.
func searchSearxNG(ctx context.Context, c *Client, query string, count int, freshness string) (SearchResult, error) {
	endpoint, err := url.Parse(c.cfg.BaseURL + "/search")
	if err != nil {
		return SearchResult{}, fmt.Errorf("base_url 不是合法地址：%v", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("language", "zh-CN")
	params.Set("safesearch", "0")
	if rangeValue := searxTimeRange(freshness); rangeValue != "" {
		params.Set("time_range", rangeValue)
	}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return SearchResult{}, fmt.Errorf("无法构造搜索请求：%v", err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return SearchResult{}, fetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return SearchResult{}, fmt.Errorf("searxng 实例拒绝了 JSON 搜索（%s）：需要在实例的 settings.yml 里把 search.formats 加上 json，并确认 base_url 指向该实例", resp.Status)
		}
		return SearchResult{}, backendStatusError(ProviderSearxNG, "搜索", resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return SearchResult{}, fmt.Errorf("读取 searxng 响应失败：%v", err)
	}
	var payload struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			Engine        string `json:"engine"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return SearchResult{}, fmt.Errorf("searxng 返回的不是 JSON（可能是实例没开 json 格式）：%v", err)
	}
	items := make([]Item, 0, len(payload.Results))
	for _, row := range payload.Results {
		items = append(items, Item{
			Title:     row.Title,
			URL:       row.URL,
			Snippet:   row.Content,
			Source:    row.Engine,
			Published: row.PublishedDate,
		})
	}
	return SearchResult{Items: items}, nil
}

// searxTimeRange maps the shared freshness vocabulary to searxng's own.
func searxTimeRange(freshness string) string {
	switch freshness {
	case FreshnessDay:
		return "day"
	case FreshnessWeek:
		return "week"
	case FreshnessMonth:
		return "month"
	case FreshnessYear:
		return "year"
	default:
		return ""
	}
}

// maxBackendBody bounds how much of a search response is read. A page of results is tens of
// kilobytes; anything past this is a misconfigured base_url answering with something else.
const maxBackendBody = 4 << 20

// bocha is the Chinese search API. Its response format mirrors Bing's (the vendor says so),
// so the fields are read from data.webPages.value.
func searchBocha(ctx context.Context, c *Client, query string, count int, freshness string) (SearchResult, error) {
	payload := map[string]any{
		"query":     query,
		"count":     count,
		"summary":   true,
		"freshness": bochaFreshness(freshness),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SearchResult{}, fmt.Errorf("无法构造搜索请求：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/web-search", strings.NewReader(string(body)))
	if err != nil {
		return SearchResult{}, fmt.Errorf("无法构造搜索请求：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return SearchResult{}, fetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SearchResult{}, backendStatusError(ProviderBocha, "搜索", resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return SearchResult{}, fmt.Errorf("读取博查响应失败：%v", err)
	}
	var decoded struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name          string `json:"name"`
					URL           string `json:"url"`
					Snippet       string `json:"snippet"`
					Summary       string `json:"summary"`
					SiteName      string `json:"siteName"`
					DatePublished string `json:"datePublished"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SearchResult{}, fmt.Errorf("博查返回的不是可解析的结果：%v", err)
	}
	items := make([]Item, 0, len(decoded.Data.WebPages.Value))
	for _, row := range decoded.Data.WebPages.Value {
		snippet := row.Snippet
		if strings.TrimSpace(row.Summary) != "" {
			// summary=true asks for the long abstract, which is what a model can actually
			// reason about; the short snippet is the fallback when it is absent.
			snippet = row.Summary
		}
		items = append(items, Item{
			Title:     row.Name,
			URL:       row.URL,
			Snippet:   snippet,
			Source:    row.SiteName,
			Published: row.DatePublished,
		})
	}
	return SearchResult{Items: items}, nil
}

// bochaFreshness maps the shared vocabulary to bocha's own names. noLimit is the vendor's
// recommended default (a fixed window can legitimately return nothing).
func bochaFreshness(freshness string) string {
	switch freshness {
	case FreshnessDay:
		return "oneDay"
	case FreshnessWeek:
		return "oneWeek"
	case FreshnessMonth:
		return "oneMonth"
	case FreshnessYear:
		return "oneYear"
	default:
		return "noLimit"
	}
}

// tavily is the search API built for LLM clients: one POST, results with a content field that
// is already an extract rather than a 160-character teaser.
func searchTavily(ctx context.Context, c *Client, query string, count int, freshness string) (SearchResult, error) {
	payload := map[string]any{
		"query":          query,
		"max_results":    count,
		"search_depth":   "basic",
		"include_answer": false,
	}
	if rangeValue := tavilyTimeRange(freshness); rangeValue != "" {
		payload["time_range"] = rangeValue
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SearchResult{}, fmt.Errorf("无法构造搜索请求：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/search", strings.NewReader(string(body)))
	if err != nil {
		return SearchResult{}, fmt.Errorf("无法构造搜索请求：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return SearchResult{}, fetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SearchResult{}, backendStatusError(ProviderTavily, "搜索", resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBackendBody))
	if err != nil {
		return SearchResult{}, fmt.Errorf("读取 Tavily 响应失败：%v", err)
	}
	var decoded struct {
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			Score         float64 `json:"score"`
			PublishedDate string  `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SearchResult{}, fmt.Errorf("Tavily 返回的不是可解析的结果：%v", err)
	}
	items := make([]Item, 0, len(decoded.Results))
	for _, row := range decoded.Results {
		items = append(items, Item{
			Title:     row.Title,
			URL:       row.URL,
			Snippet:   row.Content,
			Published: row.PublishedDate,
		})
	}
	return SearchResult{Items: items}, nil
}

// tavilyTimeRange maps the shared vocabulary to tavily's own.
func tavilyTimeRange(freshness string) string {
	switch freshness {
	case FreshnessDay:
		return "day"
	case FreshnessWeek:
		return "week"
	case FreshnessMonth:
		return "month"
	case FreshnessYear:
		return "year"
	default:
		return ""
	}
}
