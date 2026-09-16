package server

import "net"

// cloudflareCIDRStrings is Cloudflare's published edge network ranges
// (https://www.cloudflare.com/ips). When the API runs behind Cloudflare
// (as pricing.c3x.dev does), every connection arrives from one of these
// ranges, so the real client IP must be read from the CF-Connecting-IP /
// X-Forwarded-For header rather than RemoteAddr. Setting
// TRUSTED_PROXIES=cloudflare expands to this list.
//
// These ranges are stable but not immutable. Refresh from the URL above
// if Cloudflare announces a change; cloudflareNetworks is covered by a
// test that fails on a malformed entry.
var cloudflareCIDRStrings = []string{
	// IPv4
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	// IPv6
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// cloudflareNetworks parses cloudflareCIDRStrings into *net.IPNet,
// skipping any malformed entry (guarded by a test).
func cloudflareNetworks() []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cloudflareCIDRStrings))
	for _, c := range cloudflareCIDRStrings {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}
