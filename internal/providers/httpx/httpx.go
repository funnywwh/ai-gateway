// Package httpx holds HTTP helpers shared by the builtin providers.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// MaxErrorBody caps how much of an upstream error body is read.
const MaxErrorBody = 64 * 1024

// ErrorFromResponse converts a non-2xx upstream response into a protocol error,
// classifying it for the router (retryable / quota_exhausted / fatal).
func ErrorFromResponse(resp *http.Response) *pluginapi.Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
	message := decodeMessage(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}

	status := resp.StatusCode
	code := fmt.Sprintf("upstream_%d", status)

	switch {
	case status == http.StatusTooManyRequests:
		resetAt := int64(0)
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				resetAt = time.Now().Add(time.Duration(secs) * time.Second).Unix()
			}
		}
		if resetAt > 0 {
			return &pluginapi.Error{
				Code: "quota_exhausted", Message: message, Retryable: true,
				HTTPStatus: status, Kind: pluginapi.KindQuotaExhausted, ResetAt: resetAt,
			}
		}
		return pluginapi.NewRetryableError(code, message, status)
	case status == http.StatusRequestTimeout, status == http.StatusConflict,
		status == http.StatusTooEarly, status >= 500:
		return pluginapi.NewRetryableError(code, message, status)
	default:
		err := pluginapi.NewError(code, message)
		err.HTTPStatus = status
		return err
	}
}

// decodeMessage extracts a human-readable message from common error envelopes.
func decodeMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return envelope.Message
}

// NewRequest builds a request bound to ctx (so cancellation aborts the upstream call).
func NewRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, body)
}

// ClientWithTimeout builds an HTTP client used by builtin providers.
// The transport keeps connections alive to avoid per-request TCP setup.
func ClientWithTimeout(timeout time.Duration) *http.Client {
	return ClientWithProxy(timeout, nil)
}

// ClientWithProxy builds the same client with an egress proxy function.
//
// A nil proxy means "connect directly" and deliberately does NOT fall back to the
// process environment: net/http's zero Transport proxy is direct, while a transport
// built with http.ProxyFromEnvironment would silently reroute every existing
// deployment whose host happens to export HTTPS_PROXY. A provider that wants the
// environment says so explicitly (see openaichat's `proxy: env`), so an upgrade
// never changes where traffic goes on its own.
//
// http.ProxyURL accepts http/https/socks5(socks5h) URLs, which is why the builtin
// providers validate the operator's value with providerkit.ParseProxyURL first.
func ClientWithProxy(timeout time.Duration, proxy func(*http.Request) (*url.URL, error)) *http.Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               proxy,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}
