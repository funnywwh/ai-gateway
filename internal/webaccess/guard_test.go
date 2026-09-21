package webaccess

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeResolver answers lookups from a table, so the guard's decisions can be tested without
// DNS and without depending on how this machine resolves names.
type fakeResolver struct {
	answers map[string][]string
	err     error
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	raw, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]net.IPAddr, 0, len(raw))
	for _, entry := range raw {
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, errors.New("bad answer in fixture: " + entry)
		}
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

// TestGuardRefusesNonPublicTargets is the SSRF contract: every one of these addresses is a
// thing a model could be talked into fetching by a page it just read.
func TestGuardRefusesNonPublicTargets(t *testing.T) {
	g := newGuard(false)
	g.resolver = &fakeResolver{answers: map[string][]string{
		"internal.example": {"10.1.2.3"},
		"mixed.example":    {"93.184.216.34", "127.0.0.1"},
		"metadata.example": {"169.254.169.254"},
		"public.example":   {"93.184.216.34"},
	}}

	for _, raw := range []string{
		"http://localhost/admin",
		"http://localhost.localdomain/",
		"http://127.0.0.1:8088/admin/ui/",
		"http://127.1.2.3/",
		"http://[::1]/",
		"http://10.0.0.5/",
		"http://172.16.3.4/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.12.3/",
		"http://0.0.0.0/",
		"http://240.0.0.1/",
		"http://198.18.0.9/",
		"http://[::ffff:127.0.0.1]/",
		"http://[fe80::1]/",
		"http://[fc00::1]/",
		"http://[2001:db8::1]/",
		"http://wiki.internal/",
		"http://printer.local/",
		"file:///etc/passwd",
		"gopher://example.com/",
		"http://example.com:8080/",
		"http://example.com:9200/_cat/indices",
		"http://user:secret@public.example/",
		"http:///no-host",
		"http://" + strings.Repeat("a", maxURLLength) + "/",
	} {
		if _, err := g.checkURL(raw); err == nil {
			t.Errorf("checkURL(%q) must be refused", raw)
		}
	}

	if _, err := g.checkURL("https://public.example/docs"); err != nil {
		t.Fatalf("a public https URL must be allowed: %v", err)
	}
	if _, err := g.checkURL("http://public.example/"); err != nil {
		t.Fatalf("a public http URL must be allowed: %v", err)
	}
}

// TestGuardRefusesHostsThatResolveInward covers the DNS half: the URL looks public and the
// answer is not. checkURL deliberately does not resolve names (the dial does), so these are
// asserted where the decision is actually made.
func TestGuardRefusesHostsThatResolveInward(t *testing.T) {
	g := newGuard(false)
	g.resolver = &fakeResolver{answers: map[string][]string{
		"rebind.example":   {"127.0.0.1"},
		"internal.example": {"10.1.2.3"},
		"mixed.example":    {"93.184.216.34", "127.0.0.1"},
		"metadata.example": {"169.254.169.254"},
		"public.example":   {"93.184.216.34"},
	}}
	for _, host := range []string{"rebind.example", "internal.example", "mixed.example", "metadata.example"} {
		// An answer that mixes public and private addresses is refused whole: a dialer that
		// picked the first entry would otherwise reach the private one.
		if _, err := g.resolve(context.Background(), host); err == nil {
			t.Errorf("resolve(%s) must be refused", host)
		}
	}
	ips, err := g.resolve(context.Background(), "public.example")
	if err != nil || len(ips) != 1 {
		t.Fatalf("resolve(public.example) = %v, %v", ips, err)
	}
	if _, err := g.resolve(context.Background(), "unknown.example"); err == nil {
		t.Fatal("a lookup failure must be reported, not ignored")
	}
}

// TestGuardDialRechecksTheAddress is the DNS-rebinding defence: the address handed to the
// dialer is the address that was validated, and an address that fails validation never reaches
// a socket.
func TestGuardDialRechecksTheAddress(t *testing.T) {
	g := newGuard(false)
	g.resolver = &fakeResolver{answers: map[string][]string{
		"rebind.example": {"127.0.0.1"},
	}}
	if _, err := g.dial(context.Background(), "tcp", "rebind.example:443"); err == nil {
		t.Fatal("dial must refuse a name whose answer is loopback")
	}
	if _, err := g.dial(context.Background(), "tcp", "public.example:9200"); err == nil {
		t.Fatal("dial must refuse a port outside 80/443")
	}
}

// TestGuardEscapeHatch pins the one switch that lifts the whole guard, so it cannot be turned
// on by accident and cannot silently stop working.
func TestGuardEscapeHatch(t *testing.T) {
	g := newGuard(true)
	if _, err := g.checkURL("http://127.0.0.1:9200/_cat/health"); err != nil {
		t.Fatalf("allow_private_hosts must permit an internal target: %v", err)
	}
	if _, err := g.checkURL("file:///etc/passwd"); err == nil {
		t.Fatal("the escape hatch must not permit non-http schemes")
	}
}
