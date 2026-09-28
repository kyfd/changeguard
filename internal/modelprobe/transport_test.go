package modelprobe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGuardedDialValidatesAndPinsAddresses(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ips            []string
		allow, blocked bool
	}{
		{"public", []string{"8.8.8.8"}, false, false},
		{"rebound", []string{"127.0.0.1"}, false, true},
		{"mixed", []string{"8.8.8.8", "10.0.0.1"}, false, true},
		{"metadata", []string{"169.254.169.254"}, true, true},
		{"mapped", []string{"::ffff:127.0.0.1"}, false, true},
		{"private opt in", []string{"10.0.0.1"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups, dials := 0, 0
			lookup := func(context.Context, string) ([]net.IPAddr, error) {
				lookups++
				var ips []net.IPAddr
				for _, ip := range tc.ips {
					ips = append(ips, net.IPAddr{IP: net.ParseIP(ip)})
				}
				return ips, nil
			}
			dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
				dials++
				if address != net.JoinHostPort(tc.ips[0], "443") {
					t.Fatalf("not pinned: %s", address)
				}
				return nil, errors.New("test dial stopped")
			}
			_, err := guardedDial(tc.allow, lookup, dial)(context.Background(), "tcp", "model.example:443")
			if lookups != 1 {
				t.Fatalf("lookups=%d", lookups)
			}
			if tc.blocked && (!errors.Is(err, ErrBlockedAddress) || dials != 0) {
				t.Fatalf("err=%v dials=%d", err, dials)
			}
			if !tc.blocked && dials != 1 {
				t.Fatalf("dials=%d", dials)
			}
		})
	}
}

func TestHTTPClientDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			hits := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
			defer target.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
			defer upstream.Close()
			client := NewHTTPClient(time.Second, true)
			defer client.CloseIdleConnections()
			response, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != status || hits != 0 {
				t.Fatalf("status=%d target hits=%d", response.StatusCode, hits)
			}
		})
	}
}

func TestHTTPClientDisablesProxyAndBlocksLocalhost(t *testing.T) {
	client := NewHTTPClient(time.Second, false)
	defer client.CloseIdleConnections()
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy bypass enabled")
	}
	_, err := client.Get("http://127.0.0.1:1")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err=%v", err)
	}
}
