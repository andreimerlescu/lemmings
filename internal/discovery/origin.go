// Package discovery builds the read-only, origin-scoped URL surface indexed at
// boot and provides strict URL origin scoping, safe URL redaction, and hashing.
package discovery

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// ParseOrigin extracts scheme://host from hit, rejecting missing schemes,
// non-http(s) schemes, empty hosts, and embedded userinfo.
func ParseOrigin(hit string) (string, error) {
	u, err := url.Parse(hit)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("URL must include http(s) scheme and host without userinfo")
	}
	return u.Scheme + "://" + u.Host, nil
}

// SameOrigin compares parsed origins by scheme, case-folded hostname, and
// effective port (80 for http, 443 for https), rejecting userinfo and non-HTTP schemes.
func SameOrigin(a, b string) bool {
	x, err := url.Parse(a)
	if err != nil {
		return false
	}
	y, err := url.Parse(b)
	if err != nil {
		return false
	}
	if x.User != nil || y.User != nil || (x.Scheme != "http" && x.Scheme != "https") || x.Scheme != y.Scheme || x.Hostname() == "" {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(x.Hostname(), y.Hostname()) && port(x) == port(y)
}

// ResolveURL resolves href against base (an origin or page URL). Returns "" if
// the result is off-origin, unparseable, or a non-navigable link (fragment,
// mailto:, javascript:, tel:, data:).
func ResolveURL(origin, href string) string {
	href = strings.TrimSpace(href)
	lower := strings.ToLower(href)
	if href == "" ||
		strings.HasPrefix(href, "#") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "javascript:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "data:") {
		return ""
	}

	base, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}

	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""

	out := resolved.String()
	if !SameOrigin(out, base.String()) || !strings.HasPrefix(out, base.Scheme+"://"+base.Host) {
		return ""
	}
	return out
}

// SafeURL removes userinfo, query parameters, and fragments before a URL is
// written to reports, traces, logs, or the dashboard.
func SafeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	u.RawQuery = ""
	u.ForceQuery = false
	return Clip(u.String(), 2048)
}

// PathOf returns the URL path component (without query/fragment), defaulting to "/".
func PathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "/"
	}
	return u.Path
}

// DomainFromURL extracts a filesystem-safe host[-port] identifier from hit.
// Path separators, traversal sequences, and unexpected characters are stripped.
func DomainFromURL(hit string) string {
	host := ""
	if u, err := url.Parse(hit); err == nil && u.Host != "" {
		host = u.Host
	} else {
		host = strings.TrimPrefix(strings.TrimPrefix(hit, "https://"), "http://")
		host = strings.Split(host, "/")[0]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	}
	host = strings.NewReplacer(":", "-", "[", "", "]", "").Replace(host)
	var b strings.Builder
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" || strings.Contains(out, "..") {
		return "unknown"
	}
	return out
}

// SHA512Hex returns the lowercase 128-char hex SHA-512 digest of b.
func SHA512Hex(b []byte) string {
	h := sha512.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Clip truncates s to at most n bytes without splitting a UTF-8 rune.
func Clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
