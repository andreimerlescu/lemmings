package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// defaultRequestTimeout bounds a single request when -request-timeout
	// is unset.
	defaultRequestTimeout = 10 * time.Second

	// defaultMaxBodyBytes bounds the decoded body read per response when
	// -max-body-bytes is unset.
	defaultMaxBodyBytes = 2 << 20

	// maxRedirects is the redirect chain length a lemming will follow.
	maxRedirects = 10

	// maxHops bounds the redirect and queue-poll responses kept per visit.
	maxHops = 32
)

// Sentinel errors a lemming's fetch can return. They are classified by
// errorKind so reports can group failures by cause.
var (
	errOffsiteRedirect  = errors.New("redirect leaves the target origin")
	errTooManyRedirects = fmt.Errorf("stopped after %d redirects", maxRedirects)
	errBodyTooLarge     = errors.New("response body exceeds -max-body-bytes")
)

// RequestTiming breaks down where the time went on a visit's final request.
//
// DNS, Connect and TLS are zero when the lemming reused a kept-alive
// connection — which, as with a real browser, is most visits after the
// first. TTFB runs from the start of the visit to the first response byte
// of the final response, so it includes any redirects.
type RequestTiming struct {
	DNS     time.Duration `json:"dns_ns"`
	Connect time.Duration `json:"connect_ns"`
	TLS     time.Duration `json:"tls_ns"`
	TTFB    time.Duration `json:"ttfb_ns"`
	Reused  bool          `json:"connection_reused"`
}

// ResponseHop is one response on the way to a visit's final page: a
// redirect, or a waiting room poll.
type ResponseHop struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}

// response is everything one fetch observed.
type response struct {
	body        []byte
	status      int
	bytes       int64
	finalURL    string
	contentType string
	cookieNames []string
	hops        []ResponseHop
	timing      RequestTiming
	retryAfter  time.Duration
	elapsed     time.Duration // from sending the request to reading the body
	err         error
}

// applyTo copies the response's outcome onto a visit. Byte counts and
// hops are left to the caller because queue polls accumulate them.
func (r *response) applyTo(v *Visit) {
	v.StatusCode = r.status
	v.FinalURL = r.finalURL
	v.CookieNames = r.cookieNames
	v.Timing = r.timing
	v.RetryAfter = r.retryAfter
	v.Error = r.err
}

// fetch performs one GET as this lemming: its persona headers, its cookie
// jar, same-origin redirects only, a per-request timeout and a bounded
// body read. It never panics and always returns a response; failures are
// reported in response.err.
func (l *Lemming) fetch(ctx context.Context, rawURL string) response {
	r := response{finalURL: rawURL}

	timeout := l.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	limit := l.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	tr := &timingTrace{start: start}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, tr.trace()), http.MethodGet, rawURL, nil)
	if err != nil {
		r.err = fmt.Errorf("build request: %w", err)
		return r
	}
	req.Header.Set("User-Agent", l.identity.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", l.identity.Language)
	if l.referrer != "" {
		req.Header.Set("Referer", safeURL(l.referrer))
	}

	// A shallow copy of the client lets this request record its own
	// redirect chain without sharing state with any other request.
	client := *l.client
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.Response != nil {
			r.hops = appendHops(r.hops, ResponseHop{
				URL:    safeURL(via[len(via)-1].URL.String()),
				Status: next.Response.StatusCode,
			})
		}
		if !sameOrigin(rawURL, next.URL.String()) {
			return errOffsiteRedirect
		}
		if len(via) >= maxRedirects {
			return errTooManyRedirects
		}
		return nil
	}

	resp, err := client.Do(req)
	if resp != nil {
		r.status = resp.StatusCode
		r.finalURL = resp.Request.URL.String()
		r.contentType = resp.Header.Get("Content-Type")
		r.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if err == nil {
			r.body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
			r.bytes = int64(len(r.body))
			if r.bytes > limit {
				r.body = r.body[:limit]
				r.bytes = limit
				if err == nil {
					err = errBodyTooLarge
				}
			}
			if err != nil && !errors.Is(err, errBodyTooLarge) {
				err = fmt.Errorf("read body: %w", err)
			}
		}
		_ = resp.Body.Close()
		if len(r.hops) > 0 {
			r.hops = appendHops(r.hops, ResponseHop{URL: safeURL(r.finalURL), Status: r.status})
		}
		if client.Jar != nil {
			for _, c := range client.Jar.Cookies(resp.Request.URL) {
				r.cookieNames = append(r.cookieNames, c.Name)
			}
		}
	} else if err != nil {
		err = fmt.Errorf("do request: %w", err)
	}

	r.timing = tr.result()
	r.elapsed = time.Since(start)
	r.err = err
	return r
}

// timingTrace collects httptrace callbacks. Callbacks may run on dialer
// goroutines, so every field is guarded by mu.
type timingTrace struct {
	mu                          sync.Mutex
	start                       time.Time
	dnsStart, connStart, tlsBeg time.Time
	t                           RequestTiming
}

// trace returns the ClientTrace that feeds this timingTrace.
func (tt *timingTrace) trace() *httptrace.ClientTrace {
	since := func(from *time.Time, into *time.Duration) {
		tt.mu.Lock()
		if !from.IsZero() {
			*into += time.Since(*from)
		}
		tt.mu.Unlock()
	}
	mark := func(at *time.Time) {
		tt.mu.Lock()
		*at = time.Now()
		tt.mu.Unlock()
	}
	return &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { mark(&tt.dnsStart) },
		DNSDone:           func(httptrace.DNSDoneInfo) { since(&tt.dnsStart, &tt.t.DNS) },
		ConnectStart:      func(string, string) { mark(&tt.connStart) },
		ConnectDone:       func(string, string, error) { since(&tt.connStart, &tt.t.Connect) },
		TLSHandshakeStart: func() { mark(&tt.tlsBeg) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { since(&tt.tlsBeg, &tt.t.TLS) },
		GotConn: func(info httptrace.GotConnInfo) {
			tt.mu.Lock()
			tt.t.Reused = info.Reused
			tt.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			tt.mu.Lock()
			tt.t.TTFB = time.Since(tt.start)
			tt.mu.Unlock()
		},
	}
}

// result returns a copy of the collected timing.
func (tt *timingTrace) result() RequestTiming {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.t
}

// appendHops appends while respecting maxHops.
func appendHops(hops []ResponseHop, more ...ResponseHop) []ResponseHop {
	for _, h := range more {
		if len(hops) >= maxHops {
			break
		}
		hops = append(hops, h)
	}
	return hops
}

// errorKind classifies a visit error into a short, stable label used to
// group failures in reports, metrics and the dashboard.
func errorKind(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errOffsiteRedirect):
		return "offsite-redirect"
	case errors.Is(err, errTooManyRedirects):
		return "redirect-loop"
	case errors.Is(err, errBodyTooLarge):
		return "body-too-large"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &certErr), errors.As(err, &unknownAuthority), errors.As(err, &hostnameErr):
		return "tls"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "reset"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	}
	return "request"
}

// parseRetryAfter reads a Retry-After header in either delay-seconds or
// HTTP-date form, relative to now. It returns 0 for absent or invalid
// values and caps the result at maxRetryAfter.
func parseRetryAfter(s string, now time.Time) time.Duration {
	var d time.Duration
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(s); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
