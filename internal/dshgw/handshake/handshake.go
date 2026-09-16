// Package handshake exchanges dsh web's reusable startup token for one
// authority-bound cookie without following redirects or exposing the token.
package handshake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

type Source interface {
	TokenURL(tenant string) (string, error)
}
type Exchanger interface {
	Exchange(context.Context, string, string) (*session.Upstream, error)
}

type FileSource struct{ Dir string }

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var directTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	return transport
}()

func (s FileSource) TokenURL(tenant string) (string, error) {
	if !config.ValidTenantName(tenant) {
		return "", errors.New("invalid tenant name")
	}
	path := filepath.Join(s.Dir, tenant+".url")
	if err := securefile.CheckPermissions(path, 0o640); err != nil {
		return "", err
	}
	data, err := securefile.ReadLimitedRegular(path, 4096)
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(data))
	if strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("handshake URL must occupy one line")
	}
	if _, err := ParseTokenURL(raw); err != nil {
		return "", err
	}
	return raw, nil
}

func validPort(raw string) bool {
	port, err := strconv.Atoi(raw)
	return err == nil && port > 0 && port <= 65535 && strconv.Itoa(port) == raw
}

func ParseTokenURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid handshake URL")
	}
	if u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.Path != "/" || u.RawPath != "" || u.Hostname() != "127.0.0.1" || !validPort(u.Port()) {
		return "", errors.New("handshake URL must be http://127.0.0.1:<port>/?token=<value>")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) != 1 {
		return "", errors.New("handshake URL must contain exactly one token query parameter")
	}
	values, ok := q["token"]
	if !ok || len(values) != 1 || !tokenRE.MatchString(values[0]) {
		return "", errors.New("handshake URL must contain exactly one 43-character base64url token")
	}
	return values[0], nil
}

type HTTPExchanger struct {
	HTTP *http.Client
	Now  func() time.Time
}

func (e *HTTPExchanger) Exchange(ctx context.Context, tokenURL, authority string) (*session.Upstream, error) {
	token, err := ParseTokenURL(tokenURL)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host != "127.0.0.1" || !validPort(port) {
		return nil, errors.New("worker authority must be 127.0.0.1:<numeric port>")
	}
	sourceURL, _ := url.Parse(tokenURL)
	if sourceURL.Host != authority {
		return nil, errors.New("handshake URL authority does not match worker")
	}
	target := &url.URL{Scheme: "http", Host: authority, Path: "/", RawQuery: url.Values{"token": []string{token}}.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("cannot construct handshake request")
	}
	req.Host = authority
	var client http.Client
	if e.HTTP != nil {
		client = *e.HTTP
	}
	if client.Transport == nil {
		client.Transport = directTransport
	}
	if client.Timeout == 0 {
		client.Timeout = 10 * time.Second
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// url.Error contains the full bearer-token URL. Never wrap it in a
		// loggable error, even when a custom transport supplies the cause.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("handshake request failed: %w", ctx.Err())
		}
		return nil, errors.New("handshake request failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusSeeOther {
		return nil, fmt.Errorf("dsh token exchange returned HTTP %d, want 303", resp.StatusCode)
	}
	var found []*http.Cookie
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, "dsh-auth-") {
			found = append(found, cookie)
		}
	}
	if len(found) != 1 || found[0].Value == "" || found[0].MaxAge < 0 {
		return nil, errors.New("dsh token exchange must return one nonempty, live auth cookie")
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	expires := found[0].Expires
	if found[0].MaxAge > 0 {
		expires = now().Add(time.Duration(found[0].MaxAge) * time.Second)
	}
	if !expires.IsZero() && !expires.After(now()) {
		return nil, errors.New("dsh token exchange returned an expired auth cookie")
	}
	return &session.Upstream{Name: found[0].Name, Value: found[0].Value, Authority: authority, ExpiresAt: expires}, nil
}
