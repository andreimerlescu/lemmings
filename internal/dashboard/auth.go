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
	sessionCookieName     = "lemmings_session"
	maxAuthFailuresPerMin = 15
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

// authLimiter bounds failed login attempts per sliding 1-minute window (SEC-05).
type authLimiter struct {
	mu       sync.Mutex
	failures []time.Time
}

func (a *authLimiter) allow(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	n := 0
	for _, ts := range a.failures {
		if ts.After(cutoff) {
			a.failures[n] = ts
			n++
		}
	}
	a.failures = a.failures[:n]
	return len(a.failures) < maxAuthFailuresPerMin
}

func (a *authLimiter) recordFailure(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(a.failures, now)
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
	now := time.Now()
	if !d.authLimit.allow(now) {
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
		d.authLimit.recordFailure(now)
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
