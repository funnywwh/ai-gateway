package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

const nginxTestTimeout = 5 * time.Second

// TestNginxTLSProxyIntegration exercises the rendered edge, not a hand-written
// approximation. It never invokes systemd or nginx -s/-t, reads host nginx
// configuration, contacts a model, or binds anything except ephemeral loopback
// listeners. Run as a non-root user with DSHGW_TEST_NGINX=1.
func TestNginxTLSProxyIntegration(t *testing.T) {
	if os.Getenv("DSHGW_TEST_NGINX") != "1" {
		t.Skip("set DSHGW_TEST_NGINX=1 to run isolated real nginx TLS integration")
	}
	binary, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal("DSHGW_TEST_NGINX=1 requires nginx on PATH")
	}
	if os.Geteuid() <= 0 {
		t.Fatal("run this test as an unprivileged, non-root user; it will not start privileged nginx")
	}

	root := t.TempDir()
	alice := newNginxTestWorker(t, "alice")
	bob := newNginxTestWorker(t, "bob")
	// Hold all public ports until just before exec so neither httptest nor
	// another public-port allocation can reuse one of them.
	var listeners []net.Listener
	var ports []int
	for range 3 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		port := listener.Addr().(*net.TCPAddr).Port
		if port <= 1024 || port == 3080 {
			t.Fatal("ephemeral listener selected a reserved or privileged port")
		}
		listeners = append(listeners, listener)
		ports = append(ports, port)
	}
	cfg := &config.Config{
		PublicHost: "dsh-nginx.test", PortalPort: ports[0], EdgePortHeader: "X-DSHGW-Port",
		ValidateTimeout: config.Duration(time.Second), SessionTTL: config.Duration(time.Hour),
		KeyRevalidate: "off", LoginRate: config.RateLimit{Requests: 10, Window: config.Duration(time.Minute)},
		RegistryPath: filepath.Join(root, "registry.json"), KeyMapPath: filepath.Join(root, "keys.map"),
		SessionPath: filepath.Join(root, "sessions.json"), Deploy: config.DeployConfig{PublicListen: "127.0.0.1"},
	}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	for i, worker := range []*nginxTestWorker{alice, bob} {
		prefix := "sk-aaaaaaaaa"
		if i == 1 {
			prefix = "sk-bbbbbbbbb"
		}
		tenant := registry.Tenant{
			Name: worker.name, UID: 1001 + i, PublicPort: ports[i+1], WorkerPort: worker.port,
			KeyPrefix: prefix, DshHome: filepath.Join(root, worker.name, ".dsh"),
			Workspace: filepath.Join(root, worker.name, "work"), CreatedAt: time.Now(), Handshake: registry.HandshakeOK,
		}
		if err := reg.Put(tenant); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewFileStore(cfg.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg, reg, store, sourceStub("http://127.0.0.1:1/?token=unused-dummy"),
		nginxTestExchanger{alice.authority(): "alice", bob.authority(): "bob"},
		validatorFunc(func(_ context.Context, key string) ([]string, error) {
			if key != "sk-aaaaaaaaa-nginx-dummy" && key != "sk-bbbbbbbbb-nginx-dummy" {
				return nil, aigw.ErrInvalidKey
			}
			return []string{"fake-model-never-called"}, nil
		}))
	p.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	upstreamTransport := p.Transport.(*http.Transport)
	upstreamTransport.DialContext = (&net.Dialer{Timeout: nginxTestTimeout}).DialContext
	upstreamTransport.ResponseHeaderTimeout = nginxTestTimeout
	upstreamTransport.MaxResponseHeaderBytes = 16 << 10
	t.Cleanup(upstreamTransport.CloseIdleConnections)
	edge := newNginxTestServer(t, p.Dispatch())
	cfg.Listen = edge.Listener.Addr().String()
	roots := nginxTestCertificate(t, root, cfg)
	fragments, err := tenancy.RenderNginx(cfg, reg.List())
	if err != nil {
		t.Fatal(err)
	}
	client := nginxTestClient(t, cfg, ports, roots)
	startNginxTestProcess(t, binary, root, fragments, listeners, client, cfg.OriginForPort(cfg.PortalPort))

	portal := cfg.OriginForPort(ports[0])
	originA, originB := cfg.OriginForPort(ports[1]), cfg.OriginForPort(ports[2])
	nameA, nameB := cfg.SessionCookieName("alice"), cfg.SessionCookieName("bob")
	login := func(key, name, destination string) *http.Cookie {
		req := nginxTestRequest(t, http.MethodPost, portal+"/login", url.Values{"key": {key}}.Encode())
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", portal)
		res := nginxTestDo(t, client, req, http.StatusFound, "")
		if res.Header.Get("Location") != destination+"/" {
			t.Fatal("portal did not redirect to the authenticated tenant")
		}
		return nginxTestCookie(t, res, name, "")
	}
	getTenant := func(target, name, value, body string, navigation bool) {
		req := nginxTestRequest(t, http.MethodGet, target, "")
		if navigation {
			req.Header.Set("Sec-Fetch-Site", "same-site")
			req.Header.Set("Sec-Fetch-Mode", "navigate")
			req.Header.Set("Sec-Fetch-Dest", "document")
		}
		res := nginxTestDo(t, client, req, http.StatusOK, body)
		nginxTestCookie(t, res, name, value) // Renewal must survive nginx, too.
	}

	cookieA := login("sk-aaaaaaaaa-nginx-dummy", nameA, originA)
	portalURL, _ := url.Parse(portal)
	client.Jar.SetCookies(portalURL, []*http.Cookie{
		{Name: "browser-only", Value: "dummy-browser-cookie", Path: "/", Secure: true},
		{Name: "dsh-auth-forged", Value: "dummy-browser-worker-cookie", Path: "/", Secure: true},
	})
	// No manually supplied gateway cookie: the host-scoped secure jar must
	// carry the portal cookie across ports, including the navigation exception.
	getTenant(originA+"/", nameA, cookieA.Value, "alice", true)
	res := nginxTestDo(t, client, nginxTestRequest(t, http.MethodGet, originB+"/", ""), http.StatusFound, "")
	if res.Header.Get("Location") != portal+"/" {
		t.Fatal("Alice's browser session authenticated Bob")
	}
	nginxTestCookie(t, res, "", "")
	// Renaming a genuine A token to B's cookie name must not bypass ownership.
	withoutJar := *client
	withoutJar.Jar = nil
	req := nginxTestRequest(t, http.MethodGet, originB+"/", "")
	req.AddCookie(&http.Cookie{Name: nameB, Value: cookieA.Value})
	res = nginxTestDo(t, &withoutJar, req, http.StatusFound, "")
	nginxTestCookie(t, res, "", "")

	cookieB := login("sk-bbbbbbbbb-nginx-dummy", nameB, originB)
	if cookieA.Value == cookieB.Value {
		t.Fatal("two tenants received the same gateway session")
	}
	// Both logins live in the same browser; each listener must still choose
	// its own session and inject only that worker's private authentication.
	getTenant(originB+"/isolation/bob", nameB, cookieB.Value, "bob", false)
	getTenant(originA+"/isolation/alice", nameA, cookieA.Value, "alice", false)
	jarCookies := client.Jar.Cookies(portalURL)
	for name, value := range map[string]string{nameA: cookieA.Value, nameB: cookieB.Value} {
		found := 0
		for _, cookie := range jarCookies {
			if cookie.Name == name && cookie.Value == value {
				found++
			}
		}
		if found != 1 {
			t.Fatal("same-browser tenant cookies did not remain independent")
		}
	}

	// Dial A's TLS listener while claiming B in BOTH browser-controlled
	// authorities. The real rendered nginx must replace them with fixed A.
	req = nginxTestRequest(t, http.MethodPost, originA+"/api/test?shape=one%2Ftwo", "fixture-body")
	req.Host = net.JoinHostPort(cfg.PublicHost, strconv.Itoa(ports[2]))
	req.Header.Set(cfg.EdgePortHeader, strconv.Itoa(ports[2]))
	req.Header.Set("Origin", originA)
	res = nginxTestDo(t, client, req, http.StatusOK, "alice")
	nginxTestCookie(t, res, nameA, cookieA.Value)

	for _, origin := range []string{originB, "https://outside.invalid"} {
		req = nginxTestRequest(t, http.MethodPost, originA+"/denied", "fixture-body")
		req.Header.Set("Origin", origin)
		res = nginxTestDo(t, client, req, http.StatusForbidden, "")
		nginxTestCookie(t, res, "", "")
		conn, _, response := nginxTestWebSocket(t, client, cfg, ports[1], origin)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin WebSocket returned %d, want 403", response.StatusCode)
		}
		nginxTestCookie(t, response, "", "")
		_ = response.Body.Close()
		_ = conn.Close()
	}
	conn, reader, response := nginxTestWebSocket(t, client, cfg, ports[1], originA)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("same-origin WebSocket returned %d, want 101", response.StatusCode)
	}
	if !headerHasToken(response.Header, "Connection", "upgrade") || response.Header.Get("Upgrade") != "websocket" || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("nginx did not preserve the WebSocket handshake")
	}
	nginxTestCookie(t, response, nameA, cookieA.Value)
	if _, err := conn.Write(nginxTestMaskedPing()); err != nil {
		t.Fatal("write WebSocket frame failed")
	}
	echo := make([]byte, 6)
	if _, err := io.ReadFull(reader, echo); err != nil || !bytes.Equal(echo, []byte{0x81, 4, 'p', 'i', 'n', 'g'}) {
		t.Fatal("WebSocket frame did not pass through nginx and Dispatch")
	}
	_ = conn.Close()

	alice.assertRequests(t, []string{"GET /", "GET /isolation/alice", "POST /api/test?shape=one%2Ftwo", "GET /browser-fs/ws"})
	bob.assertRequests(t, []string{"GET /isolation/bob"})
}

// Every successful worker request retains its entire header map for assertions,
// but failures report only invariant names, never cookie or bearer values.
type nginxTestWorker struct {
	name     string
	port     int
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	failed   bool
}

func (w *nginxTestWorker) authority() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(w.port))
}

func newNginxTestWorker(t *testing.T, name string) *nginxTestWorker {
	t.Helper()
	worker := &nginxTestWorker{name: name}
	server := newNginxTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		copy := r.Clone(context.Background())
		copy.Header = r.Header.Clone()
		worker.mu.Lock()
		if err != nil || len(body) > 4096 || len(worker.requests) >= 32 {
			worker.failed = true
		} else {
			worker.requests = append(worker.requests, copy)
			worker.bodies = append(worker.bodies, string(body))
		}
		worker.mu.Unlock()
		if r.URL.Path != "/browser-fs/ws" {
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-" + name, Value: "worker-" + name, Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-leak", Value: "dummy-worker-leak"})
			w.Header().Set("Set-Cookie2", "legacy-worker=dummy; Version=1")
			_, _ = io.WriteString(w, name)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			worker.markFailed()
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(nginxTestTimeout))
		_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\nSet-Cookie: dsh-auth-leak=dummy-upgrade-leak\r\nSet-Cookie2: legacy-worker=dummy; Version=1\r\n\r\n")
		if err != nil || rw.Flush() != nil {
			worker.markFailed()
			return
		}
		frame := make([]byte, len(nginxTestMaskedPing()))
		if _, err := io.ReadFull(rw, frame); err != nil || !bytes.Equal(frame, nginxTestMaskedPing()) {
			worker.markFailed()
			return
		}
		if _, err := conn.Write([]byte{0x81, 4, 'p', 'i', 'n', 'g'}); err != nil {
			worker.markFailed()
		}
	}))
	worker.port = server.Listener.Addr().(*net.TCPAddr).Port
	return worker
}

func (w *nginxTestWorker) markFailed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failed = true
}

func (w *nginxTestWorker) assertRequests(t *testing.T, targets []string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed || len(w.requests) != len(targets) {
		t.Fatalf("%s worker: I/O failure=%t, requests=%d, want %d (denied requests must not arrive)", w.name, w.failed, len(w.requests), len(targets))
	}
	for i, r := range w.requests {
		if r.Method+" "+r.RequestURI != targets[i] || r.Host != w.authority() {
			t.Errorf("%s request %d: worker Host or exact target invariant failed", w.name, i)
		}
		if got := r.Header.Values("Cookie"); len(got) != 1 || got[0] != "dsh-auth-"+w.name+"=worker-"+w.name {
			t.Errorf("%s request %d: private worker cookie isolation failed (values redacted)", w.name, i)
		}
		for _, header := range []string{"Origin", "Cookie2", "Authorization", "X-API-Key", "Forwarded", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port", "X-Forwarded-Proto", "Proxy-Authorization", "Proxy-Connection", "X-DSHGW-Port"} {
			if len(r.Header.Values(header)) != 0 {
				t.Errorf("%s request %d: %s was not stripped (value redacted)", w.name, i, header)
			}
		}
		for header := range r.Header {
			if strings.HasPrefix(strings.ToLower(header), "sec-fetch-") {
				t.Errorf("%s request %d: Fetch Metadata was not stripped", w.name, i)
			}
		}
		if r.Method == http.MethodPost && w.bodies[i] != "fixture-body" {
			t.Errorf("%s request %d: POST body changed", w.name, i)
		}
		if r.URL.Path == "/browser-fs/ws" && (!isWebSocket(r) || r.Header.Get("Sec-WebSocket-Version") != "13" || r.Header.Get("Sec-WebSocket-Key") != "dGhlIHNhbXBsZSBub25jZQ==") {
			t.Errorf("%s request %d: worker WebSocket headers changed", w.name, i)
		}
	}
}

type nginxTestExchanger map[string]string

func (e nginxTestExchanger) Exchange(_ context.Context, _ string, authority string) (*session.Upstream, error) {
	name, ok := e[authority]
	if !ok {
		return nil, errors.New("exchange attempted outside the fake workers")
	}
	return &session.Upstream{Name: "dsh-auth-" + name, Value: "worker-" + name, Authority: authority}, nil
}

func newNginxTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadHeaderTimeout = nginxTestTimeout
	server.Config.ReadTimeout = nginxTestTimeout
	server.Config.WriteTimeout = nginxTestTimeout
	server.Config.IdleTimeout = nginxTestTimeout
	server.Config.MaxHeaderBytes = 32 << 10
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func nginxTestRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal("create fixture request failed")
	}
	for header, value := range map[string]string{
		"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty", "Sec-Fetch-User": "?1",
		"Authorization": "Bearer dummy-browser-bearer", "X-API-Key": "dummy-browser-api-key", "Cookie2": "legacy=dummy",
		"Forwarded": "for=192.0.2.9;host=outside.invalid", "X-Real-IP": "192.0.2.9", "X-Forwarded-For": "192.0.2.9",
		"X-Forwarded-Host": "outside.invalid", "X-Forwarded-Port": "9", "X-Forwarded-Proto": "http",
		"Proxy-Authorization": "Basic ZHVtbXk=", "Proxy-Connection": "keep-alive",
	} {
		req.Header.Set(header, value)
	}
	return req
}

func nginxTestDo(t *testing.T, client *http.Client, req *http.Request, status int, body string) *http.Response {
	t.Helper()
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("fixture HTTP request failed: %T (request credentials redacted)", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		t.Fatal("fixture response exceeded its I/O limit")
	}
	if res.StatusCode != status {
		t.Fatalf("%s %s returned %d, want %d", req.Method, req.URL.Path, res.StatusCode, status)
	}
	if body != "" && string(data) != body {
		t.Fatal("response came from the wrong fake worker")
	}
	return res
}

func nginxTestCookie(t *testing.T, res *http.Response, name, value string) *http.Cookie {
	t.Helper()
	if len(res.Header.Values("Set-Cookie2")) != 0 || len(res.Trailer.Values("Set-Cookie")) != 0 || len(res.Trailer.Values("Set-Cookie2")) != 0 {
		t.Fatal("worker cookie header or trailer leaked (values redacted)")
	}
	cookies := res.Cookies()
	if name == "" {
		if len(res.Header.Values("Set-Cookie")) != 0 {
			t.Fatal("unauthenticated response exposed a cookie (values redacted)")
		}
		return nil
	}
	if len(res.Header.Values("Set-Cookie")) != 1 || len(cookies) != 1 {
		t.Fatal("expected only one gateway cookie; worker cookie leaked or renewal was hidden (values redacted)")
	}
	cookie := cookies[0]
	if cookie.Name != name || cookie.Value == "" || value != "" && cookie.Value != value || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge <= 0 || !cookie.Expires.After(time.Now()) {
		t.Fatal("gateway cookie identity, renewal, or secure host-only attributes failed (values redacted)")
	}
	return cookie
}

func nginxTestCertificate(t *testing.T, root string, cfg *config.Config) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: serial, DNSNames: []string{cfg.PublicHost},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TLS.Certificate = filepath.Join(root, "tls.pem")
	cfg.TLS.CertificateKey = filepath.Join(root, "tls-key.pem")
	nginxTestWrite(t, cfg.TLS.Certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	nginxTestWrite(t, cfg.TLS.CertificateKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) {
		t.Fatal("trust temporary nginx certificate failed")
	}
	return roots
}

func nginxTestClient(t *testing.T, cfg *config.Config, ports []int, roots *x509.CertPool) *http.Client {
	t.Helper()
	allowed := make(map[string]bool)
	for _, port := range ports {
		allowed[strconv.Itoa(port)] = true
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: roots, ServerName: cfg.PublicHost, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: nginxTestTimeout, ResponseHeaderTimeout: nginxTestTimeout,
		MaxResponseHeaderBytes: 16 << 10, IdleConnTimeout: nginxTestTimeout,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || host != cfg.PublicHost || !allowed[port] {
				return nil, errors.New("refusing HTTP connection outside isolated nginx listeners")
			}
			return (&net.Dialer{Timeout: nginxTestTimeout}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", port))
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: transport, Jar: jar, Timeout: nginxTestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func nginxTestMaskedPing() []byte {
	return []byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'i' ^ 2, 'n' ^ 3, 'g' ^ 4}
}

func nginxTestWebSocket(t *testing.T, client *http.Client, cfg *config.Config, port int, origin string) (*tls.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: nginxTestTimeout}, Config: client.Transport.(*http.Transport).TLSClientConfig.Clone()}
	ctx, cancel := context.WithTimeout(t.Context(), nginxTestTimeout)
	defer cancel()
	raw, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("fixture WebSocket TLS dial failed: %T", err)
	}
	conn := raw.(*tls.Conn)
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(nginxTestTimeout)); err != nil {
		t.Fatal(err)
	}
	if len(conn.ConnectionState().VerifiedChains) == 0 {
		t.Fatal("nginx TLS certificate was not verified")
	}
	req := nginxTestRequest(t, http.MethodGet, cfg.OriginForPort(port)+"/browser-fs/ws", "")
	req.Header.Set("Origin", origin)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for _, cookie := range client.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	if err := req.Write(conn); err != nil {
		t.Fatal("write WebSocket handshake failed")
	}
	reader := bufio.NewReader(io.LimitReader(conn, 32<<10))
	res, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read WebSocket handshake failed: %T (headers redacted)", err)
	}
	return conn, reader, res
}

func nginxTestWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A single foreground process avoids orphan workers even on the kill fallback.
// All nginx paths (including compiled-in temporary-path defaults) are replaced;
// -e stderr also avoids opening the compiled-in host error log at startup.
func startNginxTestProcess(t *testing.T, binary, root string, fragments map[string][]byte, listeners []net.Listener, client *http.Client, portal string) {
	t.Helper()
	var includes []string
	for name, data := range fragments {
		path := filepath.Join(root, name)
		nginxTestWrite(t, path, data)
		includes = append(includes, fmt.Sprintf("include %q;", path))
	}
	sort.Strings(includes)
	var conf strings.Builder
	fmt.Fprintf(&conf, "master_process off;\nworker_processes 1;\npid %q;\nerror_log stderr crit;\nevents { worker_connections 64; }\nhttp {\naccess_log off;\nclient_header_timeout 5s;\nclient_body_timeout 5s;\nsend_timeout 5s;\nkeepalive_timeout 1s;\n", filepath.Join(root, "nginx.pid"))
	for _, name := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		fmt.Fprintf(&conf, "%s_temp_path %q;\n", name, filepath.Join(root, name+"_temp"))
	}
	conf.WriteString(strings.Join(includes, "\n") + "\n}\n")
	configPath := filepath.Join(root, "nginx.conf")
	nginxTestWrite(t, configPath, []byte(conf.String()))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "-p", root+string(os.PathSeparator), "-c", configPath, "-g", "daemon off;", "-e", "stderr")
	cmd.Dir = root
	output := &nginxTestOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Second
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start isolated nginx: %v", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			if waitErr != nil {
				t.Errorf("isolated nginx exited unexpectedly: %v", waitErr)
			}
			return
		default:
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("isolated nginx did not finish Wait after kill")
			}
		}
	})
	deadline := time.NewTimer(nginxTestTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			// No authentication has happened yet: these bounded startup
			// diagnostics cannot contain a browser session or bearer key.
			t.Fatalf("isolated nginx exited before readiness: %v\n%s", waitErr, output.String())
		case <-deadline.C:
			t.Fatalf("isolated nginx TLS readiness timed out\n%s", output.String())
		case <-ticker.C:
			probeCtx, probeCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, portal+"/", nil)
			if err != nil {
				probeCancel()
				t.Fatal(err)
			}
			res, err := client.Do(req)
			ready := false
			if err == nil {
				_, bodyErr := io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
				_ = res.Body.Close()
				ready = bodyErr == nil && res.StatusCode == http.StatusOK && res.TLS != nil && len(res.TLS.VerifiedChains) > 0
			}
			probeCancel()
			if ready {
				return
			}
		}
	}
}

type nginxTestOutput struct {
	mu   sync.Mutex
	data []byte
}

func (o *nginxTestOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data = append(o.data, p[:min(len(p), (16<<10)-len(o.data))]...)
	return len(p), nil
}

func (o *nginxTestOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data)
}
