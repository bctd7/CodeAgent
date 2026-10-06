package webfetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func parseWebURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8192 {
		return nil, fmt.Errorf("URL 必须非空且不超过 8192 字节")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URL 无效: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("仅接受无 userinfo 的绝对 HTTP(S) URL")
	}
	if strings.Contains(u.Hostname(), "%") || strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("禁止带区域标识或空端口的 URL")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("URL 端口无效")
		}
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicWebAddress(ip) {
		return nil, fmt.Errorf("禁止访问非公网地址: %s", ip)
	}
	u.Fragment = ""
	return u, nil
}

// 特殊用途网段额外排除共享地址、文档地址、基准测试及 IPv6 转换地址。
// IPv6 保守限定当前公网单播范围，避免嵌入 IPv4 的转换机制绕过限制。
var deniedWebPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicWebAddress(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range deniedWebPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// webTransport 每一跳都重新校验，拨号函数只收到已经校验的 IP 字面量。
// resolve、dial 仅是包内可信测试接缝；生产构造固定使用系统 DNS 和直接拨号。
type webTransport struct {
	resolve func(context.Context, string) ([]net.IPAddr, error)
	dial    func(context.Context, string, string) (net.Conn, error)
}

func newWebTransport() *webTransport {
	return &webTransport{resolve: net.DefaultResolver.LookupIPAddr,
		dial: (&net.Dialer{Timeout: 30 * time.Second}).DialContext}
}

func (t *webTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := parseWebURL(req.URL.String())
	if err != nil {
		return nil, err
	}
	var addresses []net.IPAddr
	if literal, err := netip.ParseAddr(u.Hostname()); err == nil {
		addresses = []net.IPAddr{{IP: net.IP(literal.AsSlice())}}
	} else {
		addresses, err = t.resolve(req.Context(), u.Hostname())
		if err != nil {
			return nil, fmt.Errorf("解析网页主机失败: %w", err)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("网页主机未返回地址")
	}
	// 任一解析地址不安全时拒绝整个目标，而不是在混合结果中挑选公网地址。
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if !ok || address.Zone != "" || !publicWebAddress(ip) {
			return nil, fmt.Errorf("DNS 返回非公网地址: %s", address.String())
		}
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	pinned := net.JoinHostPort(addresses[0].IP.String(), port)
	// 保留原 URL 和 Host，TLS SNI 与证书校验使用原主机名；不提供跳过验证开关。
	// 每次新建且关闭连接复用，避免旧连接绕过下一跳的 DNS 校验。
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DialContext:            func(ctx context.Context, network, _ string) (net.Conn, error) { return t.dial(ctx, network, pinned) },
	}
	return transport.RoundTrip(req)
}
