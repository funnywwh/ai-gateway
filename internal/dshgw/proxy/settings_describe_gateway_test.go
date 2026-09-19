package proxy

// The dsh web UI's provider directory mounts on POST /api/settings/describe. Drive a
// real DSH worker (with the browser-fs profile) behind the real gateway dispatch —
// portal login, session cookie, handshake, proxy — and require the response to arrive
// complete. Environment-gated so ordinary test runs stay hermetic.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

func TestSettingsDescribeThroughGatewayWithRealWorker(t *testing.T) {
	node := os.Getenv("DSHGW_NODE")
	dshRoot := os.Getenv("DSHGW_DSH_ROOT")
	if node == "" || dshRoot == "" {
		t.Skip("DSHGW_NODE and DSHGW_DSH_ROOT are required")
	}
	template := os.Getenv("DSHGW_PROBE_TEMPLATE")

	root := t.TempDir()
	home := filepath.Join(root, "home")
	dshHome := filepath.Join(home, ".dsh")
	_ = os.MkdirAll(dshHome, 0o700)
	_ = os.WriteFile(filepath.Join(dshHome, ".credentials.yaml"),
		[]byte("version: 1\nrefs:\n  AIGW_API_KEY: sk-dummy-probe-key-000000\nrecords: {}\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dshHome, "settings.yaml"), []byte(`{
"agent-default-model": {"provider": "aigw", "model": "deepseek-flash"},
"llm-pi-ai": {"providers": {"aigw": {"apiKeyEnv": "AIGW_API_KEY", "api": "openai-responses",
  "baseURL": "http://127.0.0.1:1/v1", "models": [{"id": "deepseek-flash", "name": "probe"}]}}}
}`), 0o600)
	if template != "" {
		if data, err := os.ReadFile(filepath.Join(template, "profiles", "web", "cordis.patch.yml")); err == nil {
			_ = os.WriteFile(filepath.Join(dshHome, "profiles", "web", "cordis.patch.yml"), data, 0o600)
		}
		_ = filepath.Walk(filepath.Join(template, "profiles"), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(filepath.Join(template, "profiles"), path)
			if info.IsDir() {
				_ = os.MkdirAll(filepath.Join(dshHome, "profiles", rel), 0o700)
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			return os.WriteFile(filepath.Join(dshHome, "profiles", rel), data, 0o600)
		})
	}

	// Fake aigw: /v1/models for the login validator.
	fakeAigw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-flash"}]}`))
	}))
	defer fakeAigw.Close()

	// Real worker on an ephemeral loopback port.
	workerPortCh := make(chan int, 1)
	ready := make(chan string, 1)
	cmd := exec.Command(node, filepath.Join(dshRoot, "lib", "bin.js"), "web", "--no-open", "--host", "127.0.0.1", "--port", "0")
	cmd.Env = append(os.Environ(),
		"HOME="+home, "DSH_HOME="+dshHome, "LANG=C.UTF-8", "NO_COLOR=1", "TMPDIR=/tmp")
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				return
			}
			acc += string(buf[:n])
			if idx := strings.Index(acc, "dsh web: http://127.0.0.1:"); idx >= 0 {
				rest := acc[idx+len("dsh web: http://127.0.0.1:"):]
				port := 0
				for _, c := range rest {
					if c < '0' || c > '9' {
						break
					}
					port = port*10 + int(c-'0')
				}
				if port > 0 {
					select {
					case workerPortCh <- port:
					default:
					}
					fields := strings.Fields(rest)
					if len(fields) > 0 && strings.Contains(fields[0], "/?token=") {
						url := "http://127.0.0.1:" + fields[0]
						select {
						case ready <- url:
						default:
						}
					}
					return
				}
			}
		}
	}()
	var workerPort int
	var startupURL string
	select {
	case workerPort = <-workerPortCh:
		select {
		case startupURL = <-ready:
		case <-time.After(10 * time.Second):
			t.Fatal("worker startup URL never appeared")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not start")
	}
	t.Logf("worker on 127.0.0.1:%d", workerPort)

	portalLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tenantLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	portalPort := portalLn.Addr().(*net.TCPAddr).Port
	listenPort := tenantLn.Addr().(*net.TCPAddr).Port
	tlsCfg := selfSignedTLS()
	cfg := &config.Config{PublicHost: "127.0.0.1", PortalPort: portalPort, TenantPortLo: listenPort, TenantPortHi: listenPort,
		WorkerPortLo: workerPort, WorkerPortHi: workerPort, Listen: "127.0.0.1:0",
		AigwBaseURL: fakeAigw.URL, ValidateTimeout: config.Duration(time.Second), SessionTTL: config.Duration(time.Hour),
		KeyRevalidate: "off", DSHEnforce: "login", LoginRate: config.RateLimit{Requests: 10, Window: config.Duration(time.Minute)},
		DirectoryPicker: "clamp", PluginBrowserFS: "on", StateDir: root,
		TenantRoot: filepath.Join(root, "tenants"), WorkspaceRoot: filepath.Join(root, "work"),
		HandshakeDir: filepath.Join(root, "handshake"), RegistryPath: filepath.Join(root, "registry.json"),
		KeyMapPath: filepath.Join(root, "keys.map"), SessionPath: filepath.Join(root, "sessions.json")}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	tenant := registry.Tenant{Name: "alice", UID: 1001, PublicPort: listenPort, WorkerPort: workerPort,
		KeyPrefix: "sk-aaaaaaaaa", DshHome: dshHome, Workspace: home, CreatedAt: time.Now(), Handshake: registry.HandshakeOK}
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewFileStore(cfg.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg, reg, store, handshake.FileSource{Dir: cfg.HandshakeDir}, &handshake.HTTPExchanger{},
		validatorFunc(func(context.Context, string) ([]aigw.Model, error) { return []aigw.Model{{ID: "deepseek-flash"}}, nil }))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "alice", nil })

	// The real exchanger needs the worker's exact startup URL; FileSource reads a file per
	// tenant. Write the startup URL where the production capture-url flow would.
	if err := os.MkdirAll(cfg.HandshakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.HandshakeDir, "alice.url"), []byte(startupURL), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("startup url: %q", startupURL)

	dispatch := http.Handler(p.Dispatch())
	portalSrv := &http.Server{Handler: dispatch, TLSConfig: tlsCfg}
	tenantSrv := &http.Server{Handler: dispatch, TLSConfig: tlsCfg}
	go func() { _ = portalSrv.ServeTLS(portalLn, "", "") }()
	go func() { _ = tenantSrv.ServeTLS(tenantLn, "", "") }()
	defer portalSrv.Close()
	defer tenantSrv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// Portal login (the browser's native form path).
	loginReq, _ := http.NewRequest(http.MethodPost, "https://127.0.0.1:"+itoa(portalPort)+"/login", strings.NewReader("key=sk-aaaaaaaaa-rest"))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.Header.Set("Origin", "https://127.0.0.1:"+itoa(portalPort))
	loginRes, err := client.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	if loginRes.StatusCode != http.StatusFound {
		b, _ := io.ReadAll(loginRes.Body)
		t.Logf("login status=%d body=%q", loginRes.StatusCode, string(b))
	}
	loginRes.Body.Close()
	var cookie string
	for _, c := range loginRes.Cookies() {
		if strings.HasPrefix(c.Name, "dshgw_s_") {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("login issued no session cookie")
	}

	// settings.describe exactly like the UI: POST /api/settings/describe.
	envelope := map[string]any{"type": "client-request", "rpcId": "probe0001",
		"method": "settings/describe", "payload": map[string]any{"args": map[string]any{}}}
	body, _ := json.Marshal(envelope)
	req, _ := http.NewRequest(http.MethodPost, "https://127.0.0.1:"+itoa(listenPort)+"/api/settings/describe", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://127.0.0.1:"+itoa(listenPort))
	req.Header.Set("Cookie", cookie)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw := new(strings.Builder)
	buf := make([]byte, 32*1024)
	for {
		n, err := res.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}
	t.Logf("describe status=%d bytes=%d", res.StatusCode, raw.Len())
	_ = os.WriteFile("/tmp/gateway-describe-response.json", []byte(raw.String()), 0o600)
	t.Logf("describe through gateway: status=%d bytes=%d", res.StatusCode, raw.Len())

	var parsed struct {
		Type   string `json:"type"`
		Result struct {
			OK    bool `json:"ok"`
			Value *struct {
				Writable   bool `json:"writable"`
				Namespaces []struct {
					Ns string `json:"ns"`
				} `json:"namespaces"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw.String()), &parsed); err != nil {
		t.Fatalf("describe response is not JSON: %v; head=%q", err, raw.String()[:min(200, raw.Len())])
	}
	if !parsed.Result.OK || parsed.Result.Value == nil || len(parsed.Result.Value.Namespaces) == 0 {
		t.Fatalf("describe through gateway lost the view: ok=%v valueNil=%v",
			parsed.Result.OK, parsed.Result.Value == nil)
	}
	t.Logf("describe through gateway carries %d namespaces", len(parsed.Result.Value.Namespaces))
}

// selfSignedTLS builds a throwaway certificate for the local UI probe. It never
// touches production TLS material.
func selfSignedTLS() *tls.Config {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	// HTTP/1.1 only: the throwaway probe does not exercise the h2 path.
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
}
func itoa(v int) string { return fmt.Sprintf("%d", v) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
