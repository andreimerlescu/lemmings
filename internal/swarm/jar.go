package swarm

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"golang.org/x/net/publicsuffix"
)

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
	secure   bool
	httpOnly bool
	sameSite http.SameSite
	expires  time.Time
	hasExp   bool
	deleted  bool
}

// InspectingJar wraps standard net/http/cookiejar.Jar so each lemming keeps
// RFC 6265 cookie behaviour while also capturing full structured metadata
// (Secure, HttpOnly, SameSite, expiry, size, HMAC fingerprint) for error artifacts.
type InspectingJar struct {
	inner *cookiejar.Jar
	fpKey []byte

	mu      sync.Mutex
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

// SetCookies delegates to the underlying cookiejar and updates the metadata table.
func (j *InspectingJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.inner.SetCookies(u, cookies)
	if u == nil || len(cookies) == 0 {
		return
	}
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()

	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		dom := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c.Domain)), ".")
		if dom == "" {
			dom = strings.ToLower(u.Hostname())
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		tc := trackedCookie{
			name:     c.Name,
			value:    c.Value,
			domain:   dom,
			path:     p,
			secure:   c.Secure,
			httpOnly: c.HttpOnly,
			sameSite: c.SameSite,
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
		j.entries[cookieKey{domain: dom, path: p, name: c.Name}] = tc
	}
}

// Cookies returns cookies applicable to u via the underlying RFC 6265 jar.
func (j *InspectingJar) Cookies(u *url.URL) []*http.Cookie {
	return j.inner.Cookies(u)
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
		expired := tc.deleted || (tc.hasExp && tc.expires.Before(now))
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
// CDP injection during error screenshot replay.
func (j *InspectingJar) ReplayCookies(targetURL *url.URL) []forensics.ReplayCookie {
	if targetURL == nil {
		return nil
	}
	active := j.inner.Cookies(targetURL)
	if len(active) == 0 {
		return nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	out := make([]forensics.ReplayCookie, 0, len(active))
	for _, c := range active {
		rc := forensics.ReplayCookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: targetURL.Hostname(),
			Path:   "/",
		}
		// Enrich with tracked attributes if known.
		for k, tc := range j.entries {
			if k.name == c.Name && !tc.deleted {
				rc.Domain = tc.domain
				rc.Path = tc.path
				rc.Secure = tc.secure
				rc.HTTPOnly = tc.httpOnly
				rc.SameSite = sameSiteLabel(tc.sameSite)
				if tc.hasExp {
					rc.Expires = float64(tc.expires.Unix())
				}
				break
			}
		}
		out = append(out, rc)
	}
	return out
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
