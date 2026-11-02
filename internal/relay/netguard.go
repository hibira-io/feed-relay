package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ErrPrivateAddress is returned when block_private_urls refuses an address.
var ErrPrivateAddress = errors.New("refused: the address is on a private network (block_private_urls is on)")

// fetchClient is the client for feeds and web pages. With block_private_urls
// it checks every address it connects to, after DNS, so a public name that
// resolves to a private address (or redirects to one) is refused too.
func fetchClient(cfg *Config) *http.Client {
	if !cfg.BlockPrivateURLs {
		return &http.Client{Timeout: 30 * time.Second}
	}
	allowed := map[string]bool{}
	if u, err := url.Parse(cfg.RSSHubURL); err == nil && u.Hostname() != "" {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		allowed[strings.ToLower(net.JoinHostPort(u.Hostname(), port))] = true
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if allowed[strings.ToLower(addr)] {
			return dialer.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error = fmt.Errorf("no addresses for %s", host)
		for _, ip := range ips {
			if privateAddr(ip) {
				lastErr = ErrPrivateAddress
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func privateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip)
}
