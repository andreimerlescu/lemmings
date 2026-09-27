package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"
)

type RequestTiming struct {
	DNS     time.Duration `json:"dns_ns"`
	Connect time.Duration `json:"connect_ns"`
	TLS     time.Duration `json:"tls_ns"`
	TTFB    time.Duration `json:"ttfb_ns"`
	Total   time.Duration `json:"total_ns"`
	Reused  bool          `json:"connection_reused"`
}
type ResponseHop struct {
	URL      string        `json:"url"`
	Status   int           `json:"status"`
	Duration time.Duration `json:"duration_ns"`
}
type requestEvidence struct {
	body        []byte
	status      int
	bytes       int64
	finalURL    string
	contentType string
	cookieNames []string
	hops        []ResponseHop
	timing      RequestTiming
	retryAfter  time.Duration
	err         error
}

// tracingTransport records every HTTP response, including followed redirects.
// net/http's ClientTrace is per round trip, so TTFB is measured separately from
// the complete navigation (which includes all redirect hops and body transfer).
type tracingTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	hops   []ResponseHop
	timing RequestTiming
}

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	var dns, connect, handshake time.Time
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { t.mu.Lock(); dns = time.Now(); t.mu.Unlock() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			t.mu.Lock()
			if !dns.IsZero() {
				t.timing.DNS += time.Since(dns)
			}
			t.mu.Unlock()
		},
		ConnectStart: func(string, string) { t.mu.Lock(); connect = time.Now(); t.mu.Unlock() },
		ConnectDone: func(string, string, error) {
			t.mu.Lock()
			if !connect.IsZero() {
				t.timing.Connect += time.Since(connect)
			}
			t.mu.Unlock()
		},
		TLSHandshakeStart: func() { t.mu.Lock(); handshake = time.Now(); t.mu.Unlock() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			t.mu.Lock()
			if !handshake.IsZero() {
				t.timing.TLS += time.Since(handshake)
			}
			t.mu.Unlock()
		},
		GotConn:              func(i httptrace.GotConnInfo) { t.mu.Lock(); t.timing.Reused = i.Reused; t.mu.Unlock() },
		GotFirstResponseByte: func() { t.mu.Lock(); t.timing.TTFB = time.Since(start); t.mu.Unlock() },
	}
	resp, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if resp != nil {
		t.mu.Lock()
		t.hops = append(t.hops, ResponseHop{safeURL(req.URL.String()), resp.StatusCode, time.Since(start)})
		t.mu.Unlock()
	}
	return resp, err
}

func (l *Lemming) requestDetailed(ctx context.Context, raw string) requestEvidence {
	r := requestEvidence{finalURL: raw}
	timeout := l.cfg.Experience.RequestTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		r.err = fmt.Errorf("build request: %w", err)
		return r
	}
	req.Header.Set("User-Agent", l.identity.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", l.identity.Language)
	if l.previousURL != "" {
		req.Header.Set("Referer", safeURL(l.previousURL))
	}
	client := *l.client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	t := &tracingTransport{base: base}
	client.Transport = t
	originalRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(raw, req.URL.String()) {
			return fmt.Errorf("cross-origin redirect blocked")
		}
		if len(via) >= 10 {
			return fmt.Errorf("redirect limit exceeded")
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		return nil
	}
	start := time.Now()
	resp, err := client.Do(req)
	if resp != nil {
		r.status = resp.StatusCode
		r.finalURL = resp.Request.URL.String()
		r.contentType = resp.Header.Get("Content-Type")
		r.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		if err == nil {
			limit := l.cfg.Experience.MaxBodyBytes
			if limit == 0 {
				limit = 2 << 20
			}
			r.body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
			r.bytes = int64(len(r.body))
			if r.bytes > limit {
				r.body = r.body[:limit]
				err = fmt.Errorf("body exceeds %d bytes", limit)
			}
		}
		_ = resp.Body.Close()
		if client.Jar != nil {
			for _, c := range client.Jar.Cookies(resp.Request.URL) {
				r.cookieNames = append(r.cookieNames, c.Name)
			}
		}
	}
	t.mu.Lock()
	r.timing = t.timing
	r.hops = append([]ResponseHop(nil), t.hops...)
	t.mu.Unlock()
	r.timing.Total = time.Since(start)
	r.err = err
	return r
}

func errorKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "request"
}

func (m *SwarmMetrics) recordVisit(v Visit) {
	m.TotalVisits.Add(1)
	m.TotalBytes.Add(v.BytesIn)
	if v.WaitingRoom.Detected {
		m.TotalWaitingRoom.Add(1)
	}
	switch {
	case v.StatusCode >= 200 && v.StatusCode < 300:
		m.Total2xx.Add(1)
	case v.StatusCode >= 300 && v.StatusCode < 400:
		m.Total3xx.Add(1)
	case v.StatusCode >= 400 && v.StatusCode < 500:
		m.Total4xx.Add(1)
	case v.StatusCode >= 500:
		m.Total5xx.Add(1)
	}
	if v.Cancelled {
		m.CancelledVisits.Add(1)
	} else if v.failed() {
		m.FailedVisits.Add(1)
	}
}

// Retry-After is honored for 429/503 responses and bounded by the session context.
func parseRetryAfter(s string) time.Duration {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 300 {
			n = 300
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		d := time.Until(t)
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		if d > 0 {
			return d
		}
	}
	return 0
}
