package swarm

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

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
)

const (
	// DefaultRequestTimeout bounds a single HTTP visit when unset.
	DefaultRequestTimeout = 10 * time.Second

	// DefaultMaxBodyBytes bounds the decoded body read per response when unset.
	DefaultMaxBodyBytes = 2 << 20

	// maxRedirects bounds the redirect chain a lemming will follow.
	maxRedirects = 10

	// maxHops bounds the redirect and waiting-room hops retained per visit.
	maxHops = 32
)

var (
	// ErrOffsiteRedirect is returned when a redirect leaves the target origin.
	ErrOffsiteRedirect = errors.New("redirect leaves the target origin")
	// ErrTooManyRedirects is returned when a redirect chain exceeds maxRedirects.
	ErrTooManyRedirects = fmt.Errorf("stopped after %d redirects", maxRedirects)
	// ErrBodyTooLarge is returned when a decoded response body exceeds MaxBodyBytes.
	ErrBodyTooLarge = errors.New("response body exceeds -max-body-bytes")
	// ErrTruncatedBody is returned when fewer bytes are read than declared by Content-Length.
	ErrTruncatedBody = errors.New("response body truncated before Content-Length")
)

// RequestTiming breaks down where time went on a visit's final HTTP request.
type RequestTiming struct {
	DNS     time.Duration `json:"dns_ns"`
	Connect time.Duration `json:"connect_ns"`
	TLS     time.Duration `json:"tls_ns"`
	TTFB    time.Duration `json:"ttfb_ns"`
	Reused  bool          `json:"connection_reused"`
}

// ResponseHop records one intermediate redirect or queue-poll response.
type ResponseHop struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}

type fetchResponse struct {
	body        []byte
	status      int
	bytes       int64
	finalURL    string
	contentType string
	headers     http.Header
	cookieNames []string
	hops        []ResponseHop
	timing      RequestTiming
	retryAfter  time.Duration
	elapsed     time.Duration
	finishedAt  time.Time
	err         error
}

func (r *fetchResponse) applyTo(v *Visit) {
	v.StatusCode = r.status
	v.FinalURL = r.finalURL
	v.CookieNames = r.cookieNames
	v.Timing = r.timing
	v.RetryAfter = r.retryAfter
	v.Error = r.err
}

func (l *Lemming) fetch(ctx context.Context, rawURL string) fetchResponse {
	r := fetchResponse{finalURL: rawURL}

	timeout := l.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	limit := l.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
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
		req.Header.Set("Referer", discovery.SafeURL(l.referrer))
	}

	client := *l.client
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.Response != nil {
			r.hops = AppendHops(r.hops, ResponseHop{
				URL:    discovery.SafeURL(via[len(via)-1].URL.String()),
				Status: next.Response.StatusCode,
			})
		}
		if !discovery.SameOrigin(rawURL, next.URL.String()) {
			return ErrOffsiteRedirect
		}
		if len(via) >= maxRedirects {
			return ErrTooManyRedirects
		}
		return nil
	}

	resp, err := client.Do(req)
	if resp != nil {
		r.status = resp.StatusCode
		r.finalURL = resp.Request.URL.String()
		r.contentType = resp.Header.Get("Content-Type")
		r.headers = resp.Header.Clone()
		r.retryAfter = ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if err == nil {
			r.body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
			r.bytes = int64(len(r.body))
			if r.bytes > limit {
				r.body = r.body[:limit]
				r.bytes = limit
				if err == nil {
					err = ErrBodyTooLarge
				}
			} else if (err == nil || errors.Is(err, io.ErrUnexpectedEOF)) &&
				resp.ContentLength >= 0 && !resp.Uncompressed && r.bytes < resp.ContentLength &&
				req.Method != http.MethodHead {
				err = fmt.Errorf("%w: read %d of %d bytes", ErrTruncatedBody, r.bytes, resp.ContentLength)
			}
			if err != nil && !errors.Is(err, ErrBodyTooLarge) && !errors.Is(err, ErrTruncatedBody) {
				err = fmt.Errorf("read body: %w", err)
			}
		}
		_ = resp.Body.Close() // read side
		if len(r.hops) > 0 {
			r.hops = AppendHops(r.hops, ResponseHop{URL: discovery.SafeURL(r.finalURL), Status: r.status})
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
	r.finishedAt = time.Now()
	r.elapsed = time.Since(start)
	r.err = err
	return r
}

type timingTrace struct {
	mu                          sync.Mutex
	start                       time.Time
	dnsStart, connStart, tlsBeg time.Time
	t                           RequestTiming
}

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

func (tt *timingTrace) result() RequestTiming {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.t
}

// AppendHops appends response hops up to maxHops.
func AppendHops(hops []ResponseHop, more ...ResponseHop) []ResponseHop {
	for _, h := range more {
		if len(hops) >= maxHops {
			break
		}
		hops = append(hops, h)
	}
	return hops
}

// ErrorKind classifies a transport or request error into a stable report label.
func ErrorKind(err error) string {
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
	case errors.Is(err, ErrOffsiteRedirect):
		return "offsite-redirect"
	case errors.Is(err, ErrTooManyRedirects):
		return "redirect-loop"
	case errors.Is(err, ErrBodyTooLarge):
		return "body-too-large"
	case errors.Is(err, ErrTruncatedBody):
		return "truncated"
	case errors.Is(err, errQueueCookiesRequired):
		return "queue-cookies"
	case errors.Is(err, errQueueExpired):
		return "queue-expired"
	case errors.Is(err, errQueueNotAdmitted):
		return "queue-not-admitted"
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

// ParseRetryAfter parses an HTTP Retry-After header (delay-seconds or HTTP-date)
// relative to now, returning 0 on absent/invalid input and capping at maxRetryAfter.
func ParseRetryAfter(s string, now time.Time) time.Duration {
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
