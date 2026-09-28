package modelprobe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewHTTPClient applies the same outbound policy to probes and real inference.
// Resolve once per connection and dial the validated IP, never the hostname:
// a second DNS lookup would permit DNS rebinding. TLS still verifies the URL host.
// Environment proxies are intentionally disabled because they bypass this dialer.
func NewHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guardedDial(allowPrivate, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func guardedDial(allowPrivate bool,
	lookup func(context.Context, string) ([]net.IPAddr, error),
	dial func(context.Context, string, string) (net.Conn, error),
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(strings.ToLower(host), ".")
		if strings.Contains(host, "%") || (!allowPrivate && (strings.HasSuffix(name, ".internal") || strings.HasSuffix(name, ".local"))) {
			return nil, ErrBlockedAddress
		}
		var addresses []net.IPAddr
		if ip := net.ParseIP(host); ip != nil {
			addresses = []net.IPAddr{{IP: ip}}
		} else {
			addresses, err = lookup(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("upstream DNS lookup failed")
			}
		}
		if len(addresses) == 0 {
			return nil, ErrBlockedAddress
		}
		for _, addr := range addresses {
			if addr.Zone != "" || !addressAllowed(addr.IP, allowPrivate) {
				return nil, ErrBlockedAddress
			}
		}
		for _, addr := range addresses {
			var conn net.Conn
			conn, err = dial(ctx, network, net.JoinHostPort(addr.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, err
	}
}
