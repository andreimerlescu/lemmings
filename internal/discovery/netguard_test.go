package discovery

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestCheckTargetAddr(t *testing.T) {
	blocked := []struct {
		name string
		ip   netip.Addr
	}{
		{"invalid_zero", netip.Addr{}},
		{"unspecified_v4", netip.MustParseAddr("0.0.0.0")},
		{"unspecified_v6", netip.MustParseAddr("::")},
		{"unspecified_v4_mapped_v6", netip.MustParseAddr("::ffff:0.0.0.0")},
		{"aws_imds_v4", netip.MustParseAddr("169.254.169.254")},
		{"link_local_v4_low", netip.MustParseAddr("169.254.0.1")},
		{"link_local_v4_high", netip.MustParseAddr("169.254.255.254")},
		{"link_local_v4_mapped_v6", netip.MustParseAddr("::ffff:169.254.169.254")},
		{"link_local_v4_mapped_v6_zoned", netip.MustParseAddr("::ffff:169.254.169.254%en0")},
		{"link_local_v6", netip.MustParseAddr("fe80::1")},
		{"link_local_v6_zoned", netip.MustParseAddr("fe80::1%en0")},
		{"link_local_v6_high", netip.MustParseAddr("febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff")},
		{"link_local_multicast_v4", netip.MustParseAddr("224.0.0.1")},
		{"link_local_multicast_v4_mdns", netip.MustParseAddr("224.0.0.251")},
		{"link_local_multicast_v6", netip.MustParseAddr("ff02::1")},
		{"link_local_multicast_v6_zoned", netip.MustParseAddr("ff02::1%en0")},
		{"multicast_v4", netip.MustParseAddr("239.255.255.250")},
		{"multicast_v4_mapped_v6", netip.MustParseAddr("::ffff:224.0.0.1")},
		{"multicast_v4_global_mapped_v6", netip.MustParseAddr("::ffff:239.1.2.3")},
		{"multicast_v6", netip.MustParseAddr("ff0e::1")},
		{"aws_imds_v6", netip.MustParseAddr("fd00:ec2::254")},
		{"aws_imds_v6_zoned", netip.MustParseAddr("fd00:ec2::254%eth0")},
	}
	for _, tc := range blocked {
		t.Run("blocked_"+tc.name, func(t *testing.T) {
			err := CheckTargetAddr(tc.ip)
			if err == nil {
				t.Fatalf("expected CheckTargetAddr(%v) to fail, got nil", tc.ip)
			}
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("expected ErrBlockedAddress for %v, got %v", tc.ip, err)
			}
		})
	}

	allowed := []struct {
		name string
		ip   netip.Addr
	}{
		{"loopback_v4", netip.MustParseAddr("127.0.0.1")},
		{"loopback_v4_alt", netip.MustParseAddr("127.0.1.1")},
		{"loopback_v6", netip.MustParseAddr("::1")},
		{"loopback_v4_mapped_v6", netip.MustParseAddr("::ffff:127.0.0.1")},
		{"rfc1918_10", netip.MustParseAddr("10.0.0.1")},
		{"rfc1918_172", netip.MustParseAddr("172.16.10.20")},
		{"rfc1918_192", netip.MustParseAddr("192.168.1.50")},
		{"ula_v6_non_imds", netip.MustParseAddr("fd00::1")},
		{"ula_v6_adjacent_imds", netip.MustParseAddr("fd00:ec2::253")},
		{"public_v4", netip.MustParseAddr("93.184.216.34")},
		{"public_v4_mapped_v6", netip.MustParseAddr("::ffff:93.184.216.34")},
		{"public_v6", netip.MustParseAddr("2606:2800:220:1:248:1893:25c8:1946")},
	}
	for _, tc := range allowed {
		t.Run("allowed_"+tc.name, func(t *testing.T) {
			if err := CheckTargetAddr(tc.ip); err != nil {
				t.Fatalf("expected CheckTargetAddr(%v) to succeed, got %v", tc.ip, err)
			}
		})
	}
}

func TestSafeDialContext_LiveAndBlocked(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	// nil base dialer should use sensible defaults and dial loopback cleanly.
	dialFn := SafeDialContext(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Direct IPv4 loopback dial.
	conn, err := dialFn(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("SafeDialContext loopback IPv4 failed: %v", err)
	}
	_ = conn.Close()

	// 2. IPv4-mapped IPv6 loopback dial.
	conn, err = dialFn(ctx, "tcp", net.JoinHostPort("::ffff:127.0.0.1", port))
	if err != nil {
		t.Fatalf("SafeDialContext IPv4-mapped IPv6 loopback failed: %v", err)
	}
	_ = conn.Close()

	// 3. Hostname "localhost" dial (must handle dual-stack IPv6+IPv4 fallback).
	conn, err = dialFn(ctx, "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("SafeDialContext localhost failed: %v", err)
	}
	_ = conn.Close()

	// 4. tcp4 hostname dial.
	conn, err = dialFn(ctx, "tcp4", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("SafeDialContext tcp4 localhost failed: %v", err)
	}
	_ = conn.Close()

	// 5. Blocked literal addresses must fail with ErrBlockedAddress without connecting.
	blockedTargets := []string{
		"169.254.169.254:80",
		"[::ffff:169.254.169.254]:80",
		"[fe80::1]:80",
		"[fe80::1%en0]:80",
		"[fd00:ec2::254]:80",
		"0.0.0.0:80",
		"[::]:80",
		"224.0.0.1:80",
		"[ff02::1]:80",
		":80",
	}
	for _, target := range blockedTargets {
		c, err := dialFn(ctx, "tcp", target)
		if c != nil {
			_ = c.Close()
		}
		if err == nil {
			t.Errorf("SafeDialContext(%q) succeeded, expected ErrBlockedAddress", target)
			continue
		}
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("SafeDialContext(%q) err = %v, want ErrBlockedAddress", target, err)
		}
	}

	// 6. Malformed address without port.
	if _, err := dialFn(ctx, "tcp", "127.0.0.1"); err == nil {
		t.Error("expected error for address without port")
	}
}

func TestSafeDialContext_IPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	dialFn := SafeDialContext(&net.Dialer{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dialFn(ctx, "tcp6", ln.Addr().String())
	if err != nil {
		t.Fatalf("SafeDialContext tcp6 [::1] failed: %v", err)
	}
	_ = conn.Close()
}

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"http://localhost:9222/json/version", true},
		{"HTTP://LOCALHOST:9222/json/version", true},
		{"https://localhost/devtools", true},
		{"ws://localhost:9222/devtools/browser/1", true},
		{"wss://localhost:9222/devtools/browser/1", true},
		{"http://127.0.0.1:9222", true},
		{"http://127.0.1.2:9222/json", true},
		{"ws://[::1]:9222/devtools/browser/abc", true},
		{"http://[::ffff:127.0.0.1]:9222/", true},
		// Blocked / fail-closed cases.
		{"", false},
		{"://not-a-url", false},
		{"http://0.0.0.0:9222", false},
		{"http://[::]:9222", false},
		{"http://169.254.169.254/latest/meta-data", false},
		{"http://[fe80::1]:9222", false},
		{"http://[fd00:ec2::254]/", false},
		{"http://10.0.0.1:9222", false},
		{"http://192.168.1.10:9222", false},
		{"http://example.com:9222", false},
		{"http://localhost.evil.test:9222", false},
		{"http://sub.localhost:9222", false},
		{"http://localhost.:9222", false},
		{"http://127.0.0.1@evil.test:9222", false},
		{"http://2130706433:9222", false},
		{"http://0177.0.0.1:9222", false},
		{"ftp://127.0.0.1:9222", false},
		{"file:///etc/passwd", false},
		{"javascript:alert(1)", false},
		{"http:localhost", false},
		{"http:///path", false},
		{"http://127.0.0.1:notaport/", false},
	}

	for _, tc := range cases {
		if got := IsLoopbackURL(tc.raw); got != tc.want {
			t.Errorf("IsLoopbackURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestMinTLSConfigAndIndexClientHardening(t *testing.T) {
	cfg1 := MinTLSConfig()
	cfg2 := MinTLSConfig()
	if cfg1 == nil || cfg1.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinTLSConfig().MinVersion = %v, want TLS 1.2 (%v)", cfg1, tls.VersionTLS12)
	}
	if cfg1 == cfg2 {
		t.Error("MinTLSConfig should return a distinct *tls.Config instance per call")
	}
	if indexConcurrency != 8 {
		t.Errorf("indexConcurrency = %d, want 8", indexConcurrency)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := newIndexClient(srv.URL)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("newIndexClient transport missing TLS 1.2+ config: %+v", tr.TLSClientConfig)
	}
	if tr.DialContext == nil {
		t.Fatal("newIndexClient transport missing SafeDialContext")
	}

	// Verify index client blocks metadata IP at dial time.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("expected ErrBlockedAddress for IMDS request, got err=%v", err)
	}

	// Verify index client reaches loopback server cleanly.
	body, status, _, err := FetchURL(context.Background(), client, srv.URL+"/")
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Errorf("FetchURL loopback failed: status=%d body=%q err=%v", status, string(body), err)
	}
}
