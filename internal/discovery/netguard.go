package discovery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ErrBlockedAddress is returned when a target IP address is rejected by the
// dial-time network guard (unspecified, link-local, multicast, or cloud IMDS).
var ErrBlockedAddress = errors.New("address blocked by network guard")

var awsIPv6IMDS = netip.MustParseAddr("fd00:ec2::254")

// CheckTargetAddr validates a destination IP address before dialing.
//
// IPv4-in-IPv6 mapped addresses are unmapped prior to classification. Invalid,
// unspecified (0.0.0.0, ::), link-local unicast (169.254.0.0/16, fe80::/10
// including zoned addresses), link-local multicast, multicast, and the AWS IPv6
// IMDS endpoint (fd00:ec2::254) are rejected with ErrBlockedAddress. Loopback
// (127.0.0.0/8, ::1) and private/public unicast addresses are allowed.
func CheckTargetAddr(ip netip.Addr) error {
	if !ip.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrBlockedAddress)
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() {
		return fmt.Errorf("%w: unspecified address %s", ErrBlockedAddress, ip)
	}
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("%w: link-local unicast address %s", ErrBlockedAddress, ip)
	}
	if ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: link-local multicast address %s", ErrBlockedAddress, ip)
	}
	if ip.IsMulticast() {
		return fmt.Errorf("%w: multicast address %s", ErrBlockedAddress, ip)
	}
	if ip.WithZone("") == awsIPv6IMDS {
		return fmt.Errorf("%w: cloud metadata address %s", ErrBlockedAddress, ip)
	}
	return nil
}

// SafeDialContext wraps base so every connection target is resolved and
// validated against CheckTargetAddr before dialing the validated literal IP(s).
// Dialing literal IPs eliminates DNS-rebinding TOCTOU windows while preserving
// IPv4/IPv6 fallback for multi-homed hosts such as localhost.
func SafeDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := base
	if dialer == nil {
		dialer = &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}
		if host == "" {
			return nil, fmt.Errorf("%w: empty host", ErrBlockedAddress)
		}

		// Fast path: literal IP address (no DNS resolution needed).
		if ip, err := netip.ParseAddr(host); err == nil {
			if err := CheckTargetAddr(ip); err != nil {
				return nil, err
			}
			dialIP := normalizeDialIP(network, ip)
			return dialer.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
		}

		ipNetwork := "ip"
		switch network {
		case "tcp4", "udp4", "ip4":
			ipNetwork = "ip4"
		case "tcp6", "udp6", "ip6":
			ipNetwork = "ip6"
		}

		resolver := dialer.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		ips, err := resolver.LookupNetIP(ctx, ipNetwork, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w: no addresses resolved for %q", ErrBlockedAddress, host)
		}

		// Validate every resolved IP before dialing any of them so a mixed
		// DNS response cannot slip a blocked address past the guard.
		for _, ip := range ips {
			if err := CheckTargetAddr(ip); err != nil {
				return nil, err
			}
		}

		var lastErr error
		for _, ip := range ips {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			dialIP := normalizeDialIP(network, ip)
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

func normalizeDialIP(network string, ip netip.Addr) netip.Addr {
	switch network {
	case "tcp6", "udp6", "ip6":
		return ip
	default:
		return ip.Unmap()
	}
}

// IsLoopbackURL reports whether rawURL uses http, https, ws, or wss and targets
// localhost (case-insensitive) or a loopback IP address (after Unmap).
// It fails closed on malformed URLs, unknown schemes, and non-loopback hosts.
func IsLoopbackURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.Unmap().IsLoopback()
}

// MinTLSConfig returns a fresh TLS configuration enforcing TLS 1.2 or higher.
func MinTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
}
