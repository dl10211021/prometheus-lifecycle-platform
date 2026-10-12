package utils

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// IsBlockedIP reports whether an IP is a private/loopback/link-local/unspecified
// address that must not be reachable from server-side fetches (SSRF guard).
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// SafeDialContext dials like net.Dialer.DialContext but rejects private IPs
// (DNS results are resolved here, so domain names pointing at internal
// addresses are blocked as well).
func SafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if allowPrivateFetch() {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ipa := range ips {
		if IsBlockedIP(ipa.IP) {
			return nil, fmt.Errorf("拒绝访问内网/回环地址 %s（SSRF 防护；如需导入内网资源请设置 PROMLIFECYCLE_ALLOW_PRIVATE_FETCH=1 后重启）", ipa.IP)
		}
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func allowPrivateFetch() bool {
	return strings.TrimSpace(os.Getenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH")) == "1"
}
