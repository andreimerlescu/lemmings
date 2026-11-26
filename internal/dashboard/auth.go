package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
)

const (
	sessionCookieName           = "lemmings_session"
	maxAuthFailuresPerIPPerMin  = 15
	maxAuthFailuresGlobalPerMin = 60
	maxAuthTrackedIPs           = 1024
)

// GenerateToken produces a cryptographically random 256-bit (64-char hex) token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sha512Hex(s string) string {
	h := sha512.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// remoteIP extracts the client IP strictly from r.RemoteAddr using
// net.SplitHostPort (falling back to the raw value). It never trusts
// X-Forwarded-For or other spoofable proxy headers.
func remoteIP(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		return host
	}
	return strings.Trim(addr, "[]")
}

// authLimiter bounds failed login attempts per remote IP (15/min, capped at
// 1024 tracked IPs) and globally (60/min) over a sliding 1-minute window (SEC-05).
type authLimiter struct {
	mu     sync.Mutex
	global []time.Time
	byIP   map[string][]time.Time
}

func pruneTimestamps(ts []time.Time, cutoff time.Time) []time.Time {
	n := 0
	for _, t := range ts {
		if t.After(cutoff) {
			ts[n] = t
			n++
		}
	}
	return ts[:n]
}

func (a *authLimiter) allow(ip string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	a.global = pruneTimestamps(a.global, cutoff)
	if len(a.global) >= maxAuthFailuresGlobalPerMin {
		return false
	}
	if a.byIP == nil {
		return true
	}
	hits, ok := a.byIP[ip]
	if !ok {
		return true
	}
	hits = pruneTimestamps(hits, cutoff)
	if len(hits) == 0 {
		delete(a.byIP, ip)
		return true
	}
	a.byIP[ip] = hits
	return len(hits) < maxAuthFailuresPerIPPerMin
}

func (a *authLimiter) recordFailure(ip string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := now.Add(-time.Minute)

	a.global = pruneTimestamps(a.global, cutoff)
	a.global = append(a.global, now)
	if len(a.global) > maxAuthFailuresGlobalPerMin {
		a.global = a.global[len(a.global)-maxAuthFailuresGlobalPerMin:]
	}

	if a.byIP == nil {
		a.byIP = make(map[string][]time.Time)
	}
	if hits, ok := a.byIP[ip]; ok {
		hits = pruneTimestamps(hits, cutoff)
		hits = append(hits, now)
		if len(hits) > maxAuthFailuresPerIPPerMin {
			hits = hits[len(hits)-maxAuthFailuresPerIPPerMin:]
		}
		a.byIP[ip] = hits
		return
	}

	if len(a.byIP) >= maxAuthTrackedIPs {
		a.evictLocked(cutoff)
	}
	a.byIP[ip] = []time.Time{now}
}

func (a *authLimiter) evictLocked(cutoff time.Time) {
	// First drop expired entries so stale IPs never evict active offenders.
	for k, hits := range a.byIP {
		pruned := pruneTimestamps(hits, cutoff)
		if len(pruned) == 0 {
			delete(a.byIP, k)
		} else {
			a.byIP[k] = pruned
		}
	}
	// If still at capacity, evict the least-recently-failed IP deterministically.
	for len(a.byIP) >= maxAuthTrackedIPs {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, hits := range a.byIP {
			last := hits[len(hits)-1]
			if first || last.Before(oldestTime) || (last.Equal(oldestTime) && k < oldestKey) {
				oldestKey = k
				oldestTime = last
				first = false
			}
		}
		if first {
			return
		}
		delete(a.byIP, oldestKey)
	}
}

func (d *Dashboard) issueSessionCookie(w http.ResponseWriter) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	sid := hex.EncodeToString(raw[:])
	mac := d.signSession(sid)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid + "." + mac,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (d *Dashboard) signSession(sid string) string {
	m := hmac.New(sha256.New, d.sessionKey)
	_, _ = m.Write([]byte(sid))
	return hex.EncodeToString(m.Sum(nil))
}

func (d *Dashboard) verifySessionCookie(val string) bool {
	sid, macHex, ok := strings.Cut(val, ".")
	if !ok || len(sid) != 32 || len(macHex) != 64 {
		return false
	}
	got, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, d.sessionKey)
	_, _ = m.Write([]byte(sid))
	expected := m.Sum(nil)
	return hmac.Equal(got, expected)
}

// handleAuth verifies the submitted boot token in constant time, enforces
// brute-force rate limiting, and sets an HMAC-signed HttpOnly SameSite=Strict session cookie.
func (d *Dashboard) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := remoteIP(r)
	now := time.Now()
	if !d.authLimit.allow(ip, now) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	raw := strings.TrimSpace(r.FormValue("token"))
	if raw == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	submitted := sha512Hex(raw)
	if subtle.ConstantTimeCompare([]byte(submitted), []byte(d.tokenHash)) != 1 {
		d.authLimit.recordFailure(ip, now)
		d.bus.Emit(events.Event{Kind: events.DashboardAuth, Err: fmt.Errorf("invalid token")})
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	if err := d.issueSessionCookie(w); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	d.bus.Emit(events.Event{Kind: events.DashboardAuth})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (d *Dashboard) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.isAuthenticated(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (d *Dashboard) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return d.verifySessionCookie(cookie.Value)
}

// LocalOnly rejects any request whose Host header is not a loopback address,
// defeating DNS-rebinding pages pointing a public hostname at 127.0.0.1.
func LocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch strings.Trim(strings.ToLower(host), "[]") {
		case "localhost", "127.0.0.1", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
		}
	})
}
