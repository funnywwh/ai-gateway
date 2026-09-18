package frontproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RegistryFile is the subset of dshgw's registry the proxy needs.
type registryFile struct {
	Tenants []struct {
		Name       string `json:"name"`
		PublicPort int    `json:"public_port"`
	} `json:"tenants"`
}

// Proxy is the router. It is safe for concurrent use: route lookups read an
// immutable snapshot built at construction, and the registry snapshot is swapped
// under a mutex.
type Proxy struct {
	cfg *Config
	log *slog.Logger

	mu       sync.RWMutex
	ports    map[string]int
	lastLoad time.Time
	loadErr  error
}

// New builds the router and its per-route reverse proxies.
func New(cfg *Config, log *slog.Logger) (*Proxy, error) {
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{cfg: cfg, log: log, ports: map[string]int{}}, nil
}

// Routes builds the nine-line routing table once, so each request only compares
// segments instead of parsing URLs.
func (p *Proxy) Handler() http.Handler {
	aigwPrefix, portalPrefix, tenantPrefix := p.cfg.NormalizedPrefixes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		// One domain only: a request for another Host is not ours to serve.
		if host := hostOnly(r.Host); !strings.EqualFold(host, p.cfg.PublicHost) {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		switch {
		// The specific prefixes win over aigw's, so a root-mounted aigw never
		// swallows the portal or a tenant.
		case pathMatches(r.URL.Path, portalPrefix):
			if p.cfg.PortalRedirect {
				// /dshgw/login on the domain becomes /login on the portal's origin.
				rest := strings.TrimPrefix(r.URL.Path, portalPrefix)
				if rest == "" {
					rest = "/"
				}
				p.redirectTo(w, r, p.cfg.PortalPort, rest)
				return
			}
			p.forwardPortal(w, r, portalPrefix)
		case pathMatches(r.URL.Path, tenantPrefix):
			if p.cfg.TenantRedirect {
				p.redirectTenant(w, r, tenantPrefix)
				return
			}
			p.forwardTenant(w, r, tenantPrefix)
		case rootMounted(aigwPrefix):
			if r.URL.Path == "/" && p.cfg.RootRedirect != "" {
				http.Redirect(w, r, p.cfg.RootRedirect, http.StatusFound)
				return
			}
			p.forwardAigw(w, r, aigwPrefix)
		case pathMatches(r.URL.Path, aigwPrefix):
			p.forwardAigw(w, r, aigwPrefix)
		case r.URL.Path == "/":
			http.Redirect(w, r, portalPrefix+"/", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
}

// scheme is the scheme this proxy advertises in redirect targets.
func (p *Proxy) scheme() string {
	switch p.cfg.PublicScheme {
	case "http", "https":
		return p.cfg.PublicScheme
	}
	if p.cfg.TLS.Certificate != "" {
		return "https"
	}
	return "http"
}

// redirectTo sends the browser to a service's own origin, keeping any sub-path so
// a deep link still lands on the same page.
func (p *Proxy) redirectTo(w http.ResponseWriter, r *http.Request, port int, rest string) {
	target := fmt.Sprintf("%s://%s:%d%s", p.scheme(), p.cfg.PublicHost, port, rest)
	http.Redirect(w, r, target, http.StatusFound)
}

// redirectTenant hands /t/<tenant>/... over to that tenant's own port, which is the
// origin its UI requires.
func (p *Proxy) redirectTenant(w http.ResponseWriter, r *http.Request, prefix string) {
	tenant, rest, ok := splitTenantPath(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	port, known := p.tenantPort(tenant)
	if !known {
		http.Error(w, "unknown tenant", http.StatusNotFound)
		return
	}
	if rest == "/" {
		rest = "/"
	}
	p.redirectTo(w, r, port, rest)
}

// forwardAigw passes the prefix through unchanged: aigw's server.base_path serves
// it, so its cookie Path, redirects and console URLs already carry the prefix.
func (p *Proxy) forwardAigw(w http.ResponseWriter, r *http.Request, prefix string) {
	target, err := url.Parse(p.cfg.AigwUpstream)
	if err != nil {
		http.Error(w, "misconfigured upstream", http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Stripping a root prefix would eat the leading slash itself, and it is
			// meaningless anyway: aigw already receives the paths it serves.
			if p.cfg.AigwStripPrefix && !rootMounted(prefix) {
				pr.Out.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
				if pr.Out.URL.Path == "" || !strings.HasPrefix(pr.Out.URL.Path, "/") {
					pr.Out.URL.Path = "/" + strings.TrimPrefix(pr.Out.URL.Path, "/")
				}
			}
			// aigw builds absolute redirects from the request Host (its
			// /admin/ui -> /admin/ui/ 301 does exactly that), so the browser's own
			// authority is preserved — including the port the proxy listens on. It is
			// safe to do so because a request for any other host was rejected above.
			if r.Host != "" {
				pr.Out.Host = r.Host
			} else {
				pr.Out.Host = p.cfg.PublicHost
			}
			pr.SetXForwarded()
		},
		ErrorHandler: p.upstreamError("aigw"),
	}
	proxy.ServeHTTP(w, r)
}

// forwardPortal proxies the dshgw portal. dshgw routes on the request's Host
// authority *and* the edge port header (it rejects a mismatch), so the proxy
// supplies both from the configured portal port.
func (p *Proxy) forwardPortal(w http.ResponseWriter, r *http.Request, prefix string) {
	p.forwardToDshgw(w, r, p.cfg.PortalUpstream, prefix, p.cfg.PortalPort, "")
}

// forwardTenant proxies one tenant's dsh UI: the tenant name comes from the path,
// and dshgw then applies the same session, entitlement and worker-handshake rules
// it applies to port-based traffic.
func (p *Proxy) forwardTenant(w http.ResponseWriter, r *http.Request, prefix string) {
	tenant, rest, ok := splitTenantPath(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	port, known := p.tenantPort(tenant)
	if !known {
		http.Error(w, "unknown tenant", http.StatusNotFound)
		return
	}
	clone := r.Clone(r.Context())
	clone.URL.Path = rest
	p.forwardToDshgw(w, clone, p.cfg.TenantUpstream, "", port, prefix+"/"+tenant)
}

func (p *Proxy) forwardToDshgw(w http.ResponseWriter, r *http.Request, upstream, stripPrefix string, port int, htmlPrefix string) {
	target, err := url.Parse(upstream)
	if err != nil {
		http.Error(w, "misconfigured upstream", http.StatusBadGateway)
		return
	}
	authority := net.JoinHostPort(p.cfg.PublicHost, strconv.Itoa(port))
	rewrite := func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		if stripPrefix != "" {
			pr.Out.URL.Path = strings.TrimPrefix(r.URL.Path, stripPrefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
		}
		pr.Out.Host = authority
		pr.Out.Header.Set(p.edgeHeader(), strconv.Itoa(port))
		pr.SetXForwarded()
	}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite:       rewrite,
		ErrorHandler:  p.upstreamError("dshgw"),
	}
	if htmlPrefix != "" && p.cfg.RewriteHTML() {
		proxy.ModifyResponse = func(resp *http.Response) error {
			return rewriteShellPaths(resp, htmlPrefix)
		}
	}
	proxy.ServeHTTP(w, r)
}

func (p *Proxy) edgeHeader() string {
	if strings.TrimSpace(p.cfg.EdgePortHeader) != "" {
		return p.cfg.EdgePortHeader
	}
	return "X-DSHGW-Port"
}

func (p *Proxy) upstreamError(name string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		p.log.Warn("upstream request failed", "upstream", name, "path", r.URL.Path, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
}

// RefreshTenants re-reads dshgw's registry so a tenant created through the console
// becomes reachable at its path without restarting the proxy.
func (p *Proxy) RefreshTenants(force bool) error {
	p.mu.Lock()
	if !force && time.Since(p.lastLoad) < p.cfg.RegistryReload.Duration() {
		p.mu.Unlock()
		return p.loadErr
	}
	p.mu.Unlock()

	data, err := os.ReadFile(p.cfg.RegistryPath)
	if errors.Is(err, os.ErrNotExist) {
		// No tenants yet is a valid state, not an error to publish.
		data, err = []byte(`{"tenants":[]}`), nil
	}
	ports := map[string]int{}
	if err == nil {
		var doc registryFile
		if err = json.Unmarshal(data, &doc); err != nil {
			err = fmt.Errorf("decode %s: %w", p.cfg.RegistryPath, err)
		} else {
			for _, tenant := range doc.Tenants {
				ports[tenant.Name] = tenant.PublicPort
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastLoad = time.Now()
	p.loadErr = err
	if err == nil {
		p.ports = ports
	}
	return err
}

func (p *Proxy) tenantPort(name string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	port, ok := p.ports[name]
	return port, ok
}

// Run refreshes the tenant list in the background until the context is done.
func (p *Proxy) Run(done <-chan struct{}) {
	_ = p.RefreshTenants(true)
	ticker := time.NewTicker(p.cfg.RegistryReload.Duration())
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := p.RefreshTenants(false); err != nil {
				p.log.Warn("reading the tenant registry failed", "err", err)
			}
		}
	}
}

// rootMounted reports whether the aigw route is the root fallback.
func rootMounted(prefix string) bool { return prefix == "/" }

// pathMatches reports whether a request path is inside a route prefix, matching on
// segment boundaries so /aigw cannot capture /aigw-something.
func pathMatches(requestPath, prefix string) bool {
	if requestPath == prefix {
		return true
	}
	return strings.HasPrefix(requestPath, prefix+"/")
}

// splitTenantPath pulls the tenant name out of <prefix>/<tenant>/rest.
func splitTenantPath(requestPath, prefix string) (tenant, rest string, ok bool) {
	trimmed := strings.TrimPrefix(requestPath, prefix+"/")
	if trimmed == requestPath {
		return "", "", false
	}
	tenant, rest, found := strings.Cut(trimmed, "/")
	if tenant == "" || strings.ContainsAny(tenant, "/\\\x00") {
		return "", "", false
	}
	if !found {
		rest = ""
	}
	return tenant, "/" + rest, true
}

func hostOnly(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host
	}
	return authority
}

// rootAttributeRE matches a root-absolute reference inside a tag: href="/x",
// src='/plugins/...'. Protocol-relative values ("//host/x") and already-prefixed
// values are excluded by requiring "/" followed by a character that is neither
// "/" nor the quote.
// rootAttributeRE matches one root-absolute attribute value inside a tag:
// href="/", src="/plugins/...", action='/login'. The value is captured whole so
// the replacement can decide what to do with it exactly once.
var rootAttributeRE = regexp.MustCompile(`(?i)\b(href|src|action)(=)("|')(/[^"']*)`)

// rewriteRootAbsoluteReferences prefixes root-absolute attribute values with the
// tenant's path, and leaves everything else byte-identical.
//
// It walks tags rather than doing a blanket string replacement: only attribute
// values inside a tag are candidates, so prose that happens to mention href="/"
// (a documentation page, an error message) is never touched. Comments and script
// bodies are copied verbatim — the rewrite is about the shell's link and asset
// references, not about its code.
func rewriteRootAbsoluteReferences(page, prefix string) string {
	var out strings.Builder
	out.Grow(len(page) + 64)
	for i := 0; i < len(page); {
		if page[i] != '<' {
			out.WriteByte(page[i])
			i++
			continue
		}
		end, isTag := tagEnd(page, i)
		if !isTag {
			out.WriteByte(page[i])
			i++
			continue
		}
		out.WriteString(rootAttributeRE.ReplaceAllStringFunc(page[i:end], func(match string) string {
			groups := rootAttributeRE.FindStringSubmatch(match)
			if len(groups) < 5 {
				return match
			}
			name, equals, quote, value := groups[1], groups[2], groups[3], groups[4]
			switch {
			case strings.HasPrefix(value, "//"):
				// Protocol-relative: it already names a host.
				return match
			case value == prefix || strings.HasPrefix(value, prefix+"/"):
				// Already ours (an application that prefixes its own references, or a
				// second pass): rewriting again would double the prefix.
				return match
			default:
				return name + equals + quote + prefix + value
			}
		}))
		i = end
	}
	return out.String()
}

// tagEnd finds the end of the markup starting at index start, respecting quoted
// attribute values and skipping comments and declarations. A '<' that is not
// markup (a stray comparison in text) reports false so the caller copies it.
func tagEnd(page string, start int) (int, bool) {
	if start+1 >= len(page) {
		return 0, false
	}
	switch next := page[start+1]; {
	case next == '!':
		if strings.HasPrefix(page[start:], "<!--") {
			if end := strings.Index(page[start:], "-->"); end >= 0 {
				return start + end + 3, true
			}
			return len(page), true
		}
		if end := strings.IndexByte(page[start:], '>'); end >= 0 {
			return start + end + 1, true
		}
		return len(page), true
	case next == '/', next == '?':
		if end := strings.IndexByte(page[start:], '>'); end >= 0 {
			return start + end + 1, true
		}
		return len(page), true
	case isASCIILetter(next):
	default:
		return 0, false
	}
	quote := byte(0)
	for i := start + 1; i < len(page); i++ {
		switch ch := page[i]; {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == '>':
			return i + 1, true
		}
	}
	return len(page), true
}

func isASCIILetter(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

// rewriteShellPaths adds the tenant prefix to the root-absolute references the dsh
// shell emits. dsh has no base-path option (its CLI offers only --host/--port/
// --trusted-host/--no-open), but its assets are relative and its bundles contain
// no hardcoded API path — measured — so the shell can be served under a prefix
// with this narrow substitution instead of forking dsh.
//
// Only text/html is touched, only two literal shapes are replaced, and the body is
// read with a bound: a proxy that rewrites arbitrary application content is a
// liability, one that adds a path prefix to two known references is not.
func rewriteShellPaths(resp *http.Response, prefix string) error {
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/html") {
		return nil
	}
	const limit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	closeErr := resp.Body.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(body)) > limit {
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return nil // too large to be the shell; pass it through untouched
	}
	page := rewriteRootAbsoluteReferences(string(body), prefix)
	resp.Body = io.NopCloser(strings.NewReader(page))
	resp.ContentLength = int64(len(page))
	resp.Header.Set("Content-Length", strconv.Itoa(len(page)))
	return nil
}
