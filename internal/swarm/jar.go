package swarm

import (
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"golang.org/x/net/publicsuffix"
)

const maxTrackedCookies = 256

type cookieKey struct {
	domain string
	path   string
	name   string
}

type trackedCookie struct {
	name     string
	value    string
	domain   string
	path     string
	hostOnly bool
	secure   bool
	httpOnly bool
	sameSite http.SameSite
	expires  time.Time
	hasExp   bool
	deleted  bool
	seenSeq  uint64
}

// InspectingJar wraps standard net/http/cookiejar.Jar so each lemming keeps
// RFC 6265 cookie behaviour while also capturing full structured metadata
// (Secure, HttpOnly, SameSite, expiry, size, HMAC fingerprint) for error artifacts.
type InspectingJar struct {
	inner *cookiejar.Jar
	fpKey []byte

	mu      sync.Mutex
	nextSeq uint64
	entries map[cookieKey]trackedCookie
}

// NewInspectingJar constructs an isolated cookie jar with publicsuffix scoping.
func NewInspectingJar(fpKey []byte) (*InspectingJar, error) {
	inner, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		return nil, err
	}
	return &InspectingJar{
		inner:   inner,
		fpKey:   fpKey,
		entries: make(map[cookieKey]trackedCookie),
	}, nil
}

// SetCookies delegates to the underlying cookiejar and updates the bounded,
// RFC 6265-scoped metadata table. Cookies with cross-domain, IP-suffix, or
// public-suffix Domain attributes are rejected so hostile responses cannot
// poison metadata or ReplayCookies.
func (j *InspectingJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.inner.SetCookies(u, cookies)
	if u == nil || len(cookies) == 0 {
		return
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return
	}
	reqHost := canonicalHost(u.Hostname())
	if reqHost == "" {
		return
	}

	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()

	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		dom, hostOnly, ok := canonicalCookieDomain(reqHost, c.Domain)
		if !ok {
			continue
		}
		p := cookiePath(u.Path, c.Path)
		j.nextSeq++
		tc := trackedCookie{
			name:     c.Name,
			value:    c.Value,
			domain:   dom,
			path:     p,
			hostOnly: hostOnly,
			secure:   c.Secure,
			httpOnly: c.HttpOnly,
			sameSite: c.SameSite,
			seenSeq:  j.nextSeq,
		}
		switch {
		case c.MaxAge < 0:
			tc.deleted = true
			tc.expires = now.Add(-time.Second)
			tc.hasExp = true
		case c.MaxAge > 0:
			tc.expires = now.Add(time.Duration(c.MaxAge) * time.Second).UTC()
			tc.hasExp = true
		case !c.Expires.IsZero():
			tc.expires = c.Expires.UTC()
			tc.hasExp = true
		}

		key := cookieKey{domain: dom, path: p, name: c.Name}
		if _, exists := j.entries[key]; !exists {
			for len(j.entries) >= maxTrackedCookies {
				j.evictOneLocked(now)
			}
		}
		j.entries[key] = tc
	}
}

func (j *InspectingJar) evictOneLocked(now time.Time) {
	var (
		hasExpired bool
		expKey     cookieKey
		expSeq     uint64

		hasOldest bool
		oldKey    cookieKey
		oldSeq    uint64
	)
	for k, tc := range j.entries {
		expired := tc.deleted || (tc.hasExp && !tc.expires.After(now))
		if expired {
			if !hasExpired || tc.seenSeq < expSeq {
				hasExpired = true
				expKey = k
				expSeq = tc.seenSeq
			}
		}
		if !hasOldest || tc.seenSeq < oldSeq {
			hasOldest = true
			oldKey = k
			oldSeq = tc.seenSeq
		}
	}
	if hasExpired {
		delete(j.entries, expKey)
		return
	}
	if hasOldest {
		delete(j.entries, oldKey)
	}
}

// Cookies returns cookies applicable to u via the underlying RFC 6265 jar.
func (j *InspectingJar) Cookies(u *url.URL) []*http.Cookie {
	return j.inner.Cookies(u)
}

// HasActiveCookie reports whether the underlying RFC 6265 jar holds a non-empty
// cookie named name applicable to u (used for concert_admit and room_ticket checks).
func (j *InspectingJar) HasActiveCookie(u *url.URL, name string) bool {
	if j == nil || j.inner == nil || u == nil || name == "" {
		return false
	}
	for _, c := range j.inner.Cookies(u) {
		if c != nil && c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}

// Snapshot builds a forensics.CookieJarSnapshot describing the jar's current
// state without losing expired/deleted cookie visibility.
func (j *InspectingJar) Snapshot(targetURL *url.URL, now time.Time) forensics.CookieJarSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()

	if len(j.entries) == 0 {
		return forensics.CookieJarSnapshot{Health: "empty"}
	}

	keys := make([]cookieKey, 0, len(j.entries))
	for k := range j.entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, k int) bool {
		if keys[i].name != keys[k].name {
			return keys[i].name < keys[k].name
		}
		if keys[i].domain != keys[k].domain {
			return keys[i].domain < keys[k].domain
		}
		return keys[i].path < keys[k].path
	})

	snap := forensics.CookieJarSnapshot{
		TotalCookies: len(keys),
		Cookies:      make([]forensics.CookieStatus, 0, len(keys)),
	}
	for _, k := range keys {
		tc := j.entries[k]
		expired := tc.deleted || (tc.hasExp && !tc.expires.After(now))
		if expired {
			snap.ExpiredCookies++
		} else {
			snap.ActiveCookies++
		}
		snap.Cookies = append(snap.Cookies, forensics.CookieStatus{
			Name:        tc.name,
			Domain:      tc.domain,
			Path:        tc.path,
			Secure:      tc.secure,
			HTTPOnly:    tc.httpOnly,
			SameSite:    sameSiteLabel(tc.sameSite),
			ExpiresAt:   tc.expires,
			SessionOnly: !tc.hasExp,
			Expired:     expired,
			ValueBytes:  len(tc.value),
			Fingerprint: secrets.Fingerprint(tc.value, j.fpKey),
			RawValue:    tc.value,
		})
	}

	switch {
	case snap.TotalCookies == 0:
		snap.Health = "empty"
	case snap.ExpiredCookies > 0:
		snap.Health = "has-expired"
	default:
		snap.Health = "healthy"
	}
	return snap
}

// ReplayCookies returns active cookies applicable to targetURL for ephemeral
// CDP injection during error screenshot replay, resolved via deterministic
// (domain, path, name) best-match lookup and returned in deterministic sorted order.
func (j *InspectingJar) ReplayCookies(targetURL *url.URL) []forensics.ReplayCookie {
	if targetURL == nil {
		return nil
	}
	active := j.inner.Cookies(targetURL)
	if len(active) == 0 {
		return nil
	}

	reqHost := canonicalHost(targetURL.Hostname())
	reqPath := targetURL.Path
	if reqPath == "" {
		reqPath = "/"
	}
	isHTTPS := strings.EqualFold(targetURL.Scheme, "https")
	now := time.Now()

	j.mu.Lock()
	defer j.mu.Unlock()

	used := make(map[cookieKey]bool, len(active))
	out := make([]forensics.ReplayCookie, 0, len(active))

	for _, c := range active {
		if c == nil || c.Name == "" {
			continue
		}
		rc := forensics.ReplayCookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: reqHost,
			Path:   "/",
		}

		var (
			found   bool
			bestKey cookieKey
			bestTC  trackedCookie
		)
		for k, tc := range j.entries {
			if used[k] || k.name != c.Name {
				continue
			}
			if tc.deleted || (tc.hasExp && !tc.expires.After(now)) {
				continue
			}
			if tc.secure && !isHTTPS {
				continue
			}
			if !domainMatches(reqHost, tc.domain, tc.hostOnly) {
				continue
			}
			if !pathMatches(reqPath, tc.path) {
				continue
			}
			if !found || betterCookieMatch(tc, bestTC, c.Value, reqHost) {
				found = true
				bestKey = k
				bestTC = tc
			}
		}
		if found {
			used[bestKey] = true
			rc.Domain = bestTC.domain
			rc.Path = bestTC.path
			rc.Secure = bestTC.secure
			rc.HTTPOnly = bestTC.httpOnly
			rc.SameSite = sameSiteLabel(bestTC.sameSite)
			if bestTC.hasExp {
				rc.Expires = float64(bestTC.expires.Unix())
			}
		}
		out = append(out, rc)
	}

	sort.Slice(out, func(i, k int) bool {
		if out[i].Name != out[k].Name {
			return out[i].Name < out[k].Name
		}
		if out[i].Domain != out[k].Domain {
			return out[i].Domain < out[k].Domain
		}
		if out[i].Path != out[k].Path {
			return out[i].Path < out[k].Path
		}
		return out[i].Value < out[k].Value
	})
	return out
}

func betterCookieMatch(cand, cur trackedCookie, value, reqHost string) bool {
	candVal := cand.value == value
	curVal := cur.value == value
	if candVal != curVal {
		return candVal
	}
	if len(cand.path) != len(cur.path) {
		return len(cand.path) > len(cur.path)
	}
	candExact := cand.domain == reqHost
	curExact := cur.domain == reqHost
	if candExact != curExact {
		return candExact
	}
	if len(cand.domain) != len(cur.domain) {
		return len(cand.domain) > len(cur.domain)
	}
	if cand.hostOnly != cur.hostOnly {
		return cand.hostOnly
	}
	if cand.domain != cur.domain {
		return cand.domain < cur.domain
	}
	if cand.path != cur.path {
		return cand.path < cur.path
	}
	return cand.seenSeq > cur.seenSeq
}

func canonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	return strings.TrimSuffix(h, ".")
}

func isIPHost(host string) bool {
	if host == "" {
		return false
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

// canonicalCookieDomain validates attrDomain against reqHost per RFC 6265 §5.3
// and Go's net/http/cookiejar + publicsuffix rules.
func canonicalCookieDomain(reqHost, attrDomain string) (dom string, hostOnly bool, ok bool) {
	if reqHost == "" {
		return "", false, false
	}
	raw := strings.TrimSpace(attrDomain)
	if raw == "" {
		return reqHost, true, true
	}

	if isIPHost(reqHost) {
		if strings.EqualFold(raw, reqHost) {
			return reqHost, true, true
		}
		return "", false, false
	}

	// RFC 6265 §5.2.3: strip at most one leading dot.
	if strings.HasPrefix(raw, ".") {
		raw = raw[1:]
	}
	dom = strings.ToLower(raw)
	if dom == "" ||
		strings.HasPrefix(dom, ".") ||
		strings.HasSuffix(dom, ".") ||
		strings.Contains(dom, "..") ||
		strings.ContainsAny(dom, " \t\r\n/") {
		return "", false, false
	}
	if isIPHost(dom) {
		return "", false, false
	}

	// Reject public-suffix cookies unless the request host is itself that suffix
	// (e.g. "localhost"), in which case RFC 6265 §5.3 step 5 scopes it host-only.
	if ps := publicsuffix.List.PublicSuffix(dom); ps != "" && dom == ps {
		if reqHost == dom {
			return reqHost, true, true
		}
		return "", false, false
	}

	if reqHost == dom || strings.HasSuffix(reqHost, "."+dom) {
		return dom, false, true
	}
	return "", false, false
}

func cookiePath(reqPath, attrPath string) string {
	if attrPath != "" && attrPath[0] == '/' {
		return attrPath
	}
	if reqPath == "" || reqPath[0] != '/' {
		return "/"
	}
	if i := strings.LastIndexByte(reqPath, '/'); i > 0 {
		return reqPath[:i]
	}
	return "/"
}

func domainMatches(reqHost, cookieDom string, hostOnly bool) bool {
	if reqHost == cookieDom {
		return true
	}
	if hostOnly || isIPHost(reqHost) {
		return false
	}
	return strings.HasSuffix(reqHost, "."+cookieDom)
}

func pathMatches(reqPath, cookiePath string) bool {
	if reqPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
}

func sameSiteLabel(s http.SameSite) string {
	switch s {
	case http.SameSiteLaxMode:
		return "lax"
	case http.SameSiteStrictMode:
		return "strict"
	case http.SameSiteNoneMode:
		return "none"
	default:
		return "default"
	}
}
