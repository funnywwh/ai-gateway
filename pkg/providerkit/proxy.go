package providerkit

import (
	"fmt"
	"net/url"
	"strings"
)

// proxySchemes are the proxy URL schemes net/http knows how to dial.
// "socks5" is treated exactly like "socks5h" by the standard library: the proxy
// resolves the target name. Both are accepted so operators can paste either.
var proxySchemes = map[string]bool{
	"http":    true,
	"https":   true,
	"socks5":  true,
	"socks5h": true,
}

// SupportedProxySchemes returns the accepted proxy URL schemes, in the order they
// should be presented to an operator (used by error messages and provider forms).
func SupportedProxySchemes() []string {
	return []string{"http", "https", "socks5", "socks5h"}
}

// ParseProxyURL validates a provider `proxy` setting and returns it normalized.
//
// An empty (or whitespace-only) value means "no proxy configured" and yields
// (nil, nil), which callers translate into "fall back to the process
// environment" so behaviour is unchanged for providers that never set a proxy.
//
// Only the scheme, host[:port] and userinfo carry meaning: net/http turns
// userinfo into a Proxy-Authorization header, so a proxy that needs credentials
// can be expressed as http://user:pass@host:port. Any path or query in the URL
// is ignored by the standard library and, for that reason, not rejected here.
func ParseProxyURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// "127.0.0.1:2334" fails to parse outright ("first path segment in URL
		// cannot contain colon"); a forgotten scheme is the common mistake, so
		// name it instead of echoing the parser's wording.
		if !strings.Contains(trimmed, "://") {
			return nil, fmt.Errorf("proxy URL must start with a scheme, for example http://%s", trimmed)
		}
		return nil, fmt.Errorf("proxy URL is not a valid URL: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if !proxySchemes[u.Scheme] {
		return nil, fmt.Errorf("unsupported proxy scheme %q (want %s)",
			u.Scheme, strings.Join(SupportedProxySchemes(), ", "))
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy URL must include a host, for example http://127.0.0.1:2334")
	}
	if (u.Scheme == "socks5" || u.Scheme == "socks5h") && u.Port() == "" {
		return nil, fmt.Errorf("socks5 proxy URL must include a port, for example socks5://127.0.0.1:1080")
	}
	return u, nil
}

// MaskProxyURL renders a proxy URL for logs and read-only diagnostics as
// "scheme://host[:port]". Userinfo is dropped, so the result is safe to log and
// to return from actions such as whoami. A nil URL renders as "".
func MaskProxyURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
