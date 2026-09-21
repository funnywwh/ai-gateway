package webaccess

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// The guard is the one place that decides whether a URL chosen by a model may be dialled.
//
// Two checks, deliberately duplicated:
//
//   - checkURL runs on the URL the model wrote and on every redirect hop, so a public URL that
//     redirects to 127.0.0.1 is refused at the hop;
//   - dial runs on the address actually being connected to, so a hostname that resolves to a
//     public IP for the first lookup and a private one for the second (DNS rebinding) cannot
//     slip through the gap between resolution and connection.
//
// Both matter because the *content* of a fetched page can steer the next URL, and because the
// gateway usually runs next to the management plane it protects.
type guard struct {
	allowPrivate bool
	// resolver is injectable so tests can answer a lookup without touching DNS. It is also
	// why the guard resolves addresses itself instead of letting the dialer do it: a
	// validated connection must be made to the validated IP, not to whatever the second
	// lookup says.
	resolver ipResolver
}

// ipResolver is the slice of net.Resolver the guard needs.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

func newGuard(allowPrivate bool) *guard {
	return &guard{allowPrivate: allowPrivate, resolver: net.DefaultResolver}
}

// allowedPorts are the ports a fetched page may use. Anything else is a service, not a website,
// and probing services is exactly what this guard exists to prevent.
var allowedPorts = map[string]bool{"": true, "80": true, "443": true}

// dialTimeout bounds one TCP connect; the request timeout bounds the whole fetch. Keeping them
// separate means a black-holed address fails fast instead of consuming the page budget.
const (
	dialTimeout   = 5 * time.Second
	dialKeepAlive = 30 * time.Second
)

// checkURL validates one target URL and returns it parsed, so callers can reuse the result.
func (g *guard) checkURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("地址不能为空")
	}
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("地址过长（上限 %d 个字符）", maxURLLength)
	}
	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("不是合法的地址：%v", err)
	}
	switch target.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("只支持 http/https 地址（收到 %q）", target.Scheme)
	}
	if target.User != nil {
		return nil, fmt.Errorf("地址里不能带账号密码，网关不会替模型发送凭据")
	}
	if target.Host == "" {
		return nil, fmt.Errorf("地址缺少主机名")
	}
	if g.allowPrivate {
		return target, nil
	}
	if !allowedPorts[target.Port()] {
		return nil, fmt.Errorf("只允许 80 与 443 端口（这个地址用的是 %s）；确需抓取内网服务时，由运维打开 chat.web_access.allow_private_hosts", target.Port())
	}
	if reason, blocked := g.blockedHost(target.Hostname()); blocked {
		return nil, blockedTargetError(reason)
	}
	return target, nil
}

// maxURLLength is a sanity bound: no legitimate page URL is longer, and a megabyte-long URL
// would be a request-smuggling curiosity rather than a fetch.
const maxURLLength = 2048

// dial re-validates the address and connects to the address it validated.
func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("无法解析目标地址 %q", addr)
	}
	if !g.allowPrivate && !allowedPorts[port] {
		return nil, blockedTargetError(fmt.Sprintf("端口 %s", port))
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用地址")
	}
	return nil, lastErr
}

// resolve looks a host up and returns only IPs this deployment is allowed to connect to. An
// answer that mixes public and private addresses is refused whole: a dialer that picks the
// first entry would otherwise reach the private one.
func (g *guard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if reason, blocked := g.blockedIP(ip); blocked {
			return nil, blockedTargetError(reason)
		}
		return []net.IP{ip}, nil
	}
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("域名 %s 解析失败：%v", host, err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if reason, blocked := g.blockedIP(addr.IP); blocked {
			return nil, blockedTargetError(reason)
		}
		ips = append(ips, addr.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("域名 %s 没有解析到任何地址", host)
	}
	return ips, nil
}

// blockedIP is the guard's classifier. It is a no-op when the deployment has opted into
// private hosts: that switch means "this gateway may read our intranet", so neither the URL
// check nor the per-dial check may then refuse an address.
//
// Everything net.IP already knows is taken from it, and the remaining ranges (CGNAT,
// documentation, benchmarking, reserved) are listed explicitly: net.IP.IsPrivate covers only
// RFC1918 and fc00::/7.
func (g *guard) blockedIP(ip net.IP) (string, bool) {
	if g != nil && g.allowPrivate {
		return "", false
	}
	if g == nil || ip == nil {
		return "无法识别的地址", true
	}
	// ::ffff:127.0.0.1 is loopback wearing an IPv6 costume; unwrap before classifying.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return "未指定地址", true
	case ip.IsLoopback():
		return "回环地址", true
	case ip.IsPrivate():
		return "内网地址", true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "链路本地地址", true
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "组播地址", true
	}
	for _, entry := range blockedCIDRs {
		if entry.cidr.Contains(ip) {
			return entry.label, true
		}
	}
	return "", false
}

// blockedHost rejects an IP literal or a name that only ever means "this machine". Names that
// resolve elsewhere are caught by resolve, which runs on every dial.
func (g *guard) blockedHost(host string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" {
		return "空主机名", true
	}
	if ip := net.ParseIP(name); ip != nil {
		return g.blockedIP(ip)
	}
	switch name {
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return "本机地址 " + name, true
	}
	// *.internal and *.local are the two suffixes that mean "this network" rather than "the
	// internet"; both are unambiguous enough to refuse by name.
	for _, suffix := range []string{".local", ".internal", ".localhost"} {
		if strings.HasSuffix(name, suffix) {
			return "内网域名 " + name, true
		}
	}
	return "", false
}

// blockedCIDRs are the ranges Go's standard predicates do not cover. The first entry is the
// one that matters most in practice: 100.64.0.0/10 (carrier-grade NAT) is where cloud
// metadata and cluster-internal services commonly live.
var blockedCIDRs = []struct {
	cidr  *net.IPNet
	label string
}{
	{mustCIDR("100.64.0.0/10"), "运营商级 NAT 地址（云内网常见）"},
	{mustCIDR("0.0.0.0/8"), "保留地址（0/8）"},
	{mustCIDR("192.0.0.0/24"), "保留地址（192.0.0.0/24）"},
	{mustCIDR("192.0.2.0/24"), "文档地址（TEST-NET-1）"},
	{mustCIDR("198.18.0.0/15"), "基准测试地址（198.18/15）"},
	{mustCIDR("198.51.100.0/24"), "文档地址（TEST-NET-2）"},
	{mustCIDR("203.0.113.0/24"), "文档地址（TEST-NET-3）"},
	{mustCIDR("240.0.0.0/4"), "保留地址（240/4）"},
	{mustCIDR("2001:db8::/32"), "文档地址（2001:db8::/32）"},
	{mustCIDR("64:ff9b::/96"), "NAT64 转换地址"},
	{mustCIDR("100::/64"), "丢弃地址（100::/64）"},
}

func mustCIDR(raw string) *net.IPNet {
	_, network, err := net.ParseCIDR(raw)
	if err != nil {
		panic("webaccess: invalid built-in CIDR " + raw)
	}
	return network
}

// blockedTargetError phrases a refusal as something the model can pass on to the operator:
// the reason, the fact that it is a deliberate guard, and the switch that lifts it.
func blockedTargetError(reason string) error {
	return fmt.Errorf("目标地址被拒绝（%s）：这是网关的 SSRF 防护，只允许抓取公网 http/https 地址；需要抓内网时由运维打开 chat.web_access.allow_private_hosts", reason)
}
