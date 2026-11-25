package swarm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/sema"
)

func TestConfig_Validate(t *testing.T) {
	def := DefaultConfig()
	if def.Assets || def.AssetsPerPage != DefaultAssetsPerPage || def.AllowRemoteCDP {
		t.Fatalf("unexpected DefaultConfig: %+v", def)
	}

	base := Config{
		Hit:     "http://127.0.0.1:8080/",
		Terrain: 2,
		Pack:    3,
		Limit:   6,
		Until:   5 * time.Second,
		Ramp:    1 * time.Second,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config failed: %v", err)
	}
	if base.RampShape != RampLinear || base.CaptureFormat != CaptureFormatJPEG || base.CaptureMax != 50 || base.AssetsPerPage != DefaultAssetsPerPage {
		t.Errorf("defaults not applied: %+v", base)
	}

	// Reject bad ramp shape.
	c := base
	c.RampShape = "exponential"
	if err := c.Validate(); err == nil {
		t.Error("expected invalid ramp-shape error")
	}

	// Reject out-of-range AssetsPerPage.
	c = base
	c.AssetsPerPage = -1
	if err := c.Validate(); err == nil {
		t.Error("expected error for negative -assets-per-page")
	}
	c = base
	c.AssetsPerPage = 65
	if err := c.Validate(); err == nil {
		t.Error("expected error for -assets-per-page > 64")
	}
	c = base
	c.AssetsPerPage = 64
	if err := c.Validate(); err != nil {
		t.Errorf("expected -assets-per-page=64 to succeed, got %v", err)
	}

	// AllowRemoteCDP must not serialize into JSON reports.
	c = base
	c.Assets = true
	c.AssetsPerPage = 12
	c.AllowRemoteCDP = true
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(raw), "AllowRemoteCDP") || strings.Contains(string(raw), "allow_remote_cdp") {
		t.Errorf("AllowRemoteCDP leaked into JSON: %s", raw)
	}
	if !strings.Contains(string(raw), `"assets":true`) || !strings.Contains(string(raw), `"assets_per_page":12`) {
		t.Errorf("expected assets fields in JSON: %s", raw)
	}

	// Reject path traversal in report name.
	c = base
	c.ReportName = "../escape"
	if err := c.Validate(); err == nil {
		t.Error("expected report-name traversal error")
	}

	// Accept clean report slug.
	c = base
	c.ReportName = "release-2.0_staging"
	if err := c.Validate(); err != nil {
		t.Errorf("valid report slug failed: %v", err)
	}
}

func TestRampDelays_AllShapes(t *testing.T) {
	total := 10 * time.Second
	for _, shape := range []string{RampLinear, RampFibonacci, RampStep} {
		delays := RampDelays(6, total, shape)
		if len(delays) != 5 {
			t.Fatalf("%s: expected 5 inter-terrain delays, got %d", shape, len(delays))
		}
		var sum time.Duration
		for _, d := range delays {
			if d < 0 {
				t.Errorf("%s: negative delay %s", shape, d)
			}
			sum += d
		}
		diff := sum - total
		if diff < 0 {
			diff = -diff
		}
		if diff > 50*time.Millisecond {
			t.Errorf("%s: sum %s drifted from total %s", shape, sum, total)
		}
	}
	if got := RampDelays(1, total, RampLinear); len(got) != 0 {
		t.Errorf("expected 0 delays for 1 terrain, got %d", len(got))
	}
}

func TestInspectingJar_TracksMetadataExpiryAndRedaction(t *testing.T) {
	key, err := secrets.NewEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	jar, err := NewInspectingJar(key)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://example.com/checkout")

	jar.SetCookies(u, []*http.Cookie{
		{Name: "sid", Value: "secret-session-value", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode},
		{Name: "theme", Value: "synthwave", Path: "/"},
	})

	snap := jar.Snapshot(u, time.Now())
	if snap.TotalCookies != 2 || snap.ActiveCookies != 2 || snap.Health != "healthy" {
		t.Fatalf("unexpected jar snapshot: %+v", snap)
	}
	var foundSID bool
	for _, c := range snap.Cookies {
		if c.Name == "sid" {
			foundSID = true
			if !c.HTTPOnly || !c.Secure || c.SameSite != "lax" || c.Fingerprint == "" {
				t.Errorf("missing cookie metadata: %+v", c)
			}
		}
	}
	if !foundSID {
		t.Error("expected sid cookie in snapshot")
	}

	replay := jar.ReplayCookies(u)
	if len(replay) != 2 {
		t.Errorf("expected 2 replay cookies, got %d", len(replay))
	}

	// Delete cookie via MaxAge < 0 — InspectingJar retains forensic visibility of the expired cookie.
	jar.SetCookies(u, []*http.Cookie{{Name: "theme", Value: "", Path: "/", MaxAge: -1}})
	snap2 := jar.Snapshot(u, time.Now())
	if snap2.ActiveCookies != 1 || snap2.ExpiredCookies != 1 || snap2.Health != "has-expired" {
		t.Errorf("expected 1 active & 1 expired cookie after deletion, got %+v", snap2)
	}
}

func TestInspectingJar_RFC6265ScopingCapDeterministicReplayAndHasActiveCookie(t *testing.T) {
	key, err := secrets.NewEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	jar, err := NewInspectingJar(key)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Reject cross-domain, sibling-domain, subdomain, IP-suffix, and public-suffix poisoning.
	appURL, _ := url.Parse("https://app.example.com/checkout/pay")
	jar.SetCookies(appURL, []*http.Cookie{
		{Name: "evil_other", Value: "1", Domain: "other.com", Path: "/"},
		{Name: "evil_sibling", Value: "1", Domain: "api.example.com", Path: "/"},
		{Name: "evil_sub", Value: "1", Domain: "deep.app.example.com", Path: "/"},
		{Name: "evil_tld", Value: "1", Domain: "com", Path: "/"},
		{Name: "evil_dot_tld", Value: "1", Domain: ".com", Path: "/"},
		{Name: "evil_suffix_trick", Value: "1", Domain: "ample.com", Path: "/"},
		{Name: "evil_double_dot", Value: "1", Domain: "..example.com", Path: "/"},
	})

	ukURL, _ := url.Parse("https://shop.co.uk/")
	jar.SetCookies(ukURL, []*http.Cookie{
		{Name: "evil_psl", Value: "1", Domain: "co.uk", Path: "/"},
		{Name: "evil_dot_psl", Value: "1", Domain: ".co.uk", Path: "/"},
	})

	ipURL, _ := url.Parse("http://127.0.0.1:8080/page")
	jar.SetCookies(ipURL, []*http.Cookie{
		{Name: "evil_ip_suffix", Value: "1", Domain: "0.0.1", Path: "/"},
		{Name: "evil_ip_dot", Value: "1", Domain: ".127.0.0.1", Path: "/"},
		{Name: "evil_ip_other", Value: "1", Domain: "127.0.0.2", Path: "/"},
	})

	snap := jar.Snapshot(appURL, time.Now())
	if snap.TotalCookies != 0 {
		t.Fatalf("expected all poisoned cookies to be rejected, got %d: %+v", snap.TotalCookies, snap.Cookies)
	}

	// 2. Deterministic (domain, path, name) best-match lookup and sorted ReplayCookies.
	jar.SetCookies(appURL, []*http.Cookie{
		{Name: "sid", Value: "parent-root", Domain: ".example.com", Path: "/", Secure: true, SameSite: http.SameSiteNoneMode},
		{Name: "sid", Value: "host-root", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode},
		{Name: "sid", Value: "host-checkout", Path: "/checkout", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode},
		{Name: "concert_admit", Value: "pass-token-123", Path: "/", HttpOnly: true, Secure: true},
		{Name: "empty_pass", Value: "", Path: "/"},
	})

	// HasActiveCookie checks active non-empty cookies via inner RFC 6265 jar.
	if !jar.HasActiveCookie(appURL, "concert_admit") {
		t.Error("expected HasActiveCookie(concert_admit)=true on https://app.example.com/checkout/pay")
	}
	httpAppURL, _ := url.Parse("http://app.example.com/checkout/pay")
	if jar.HasActiveCookie(httpAppURL, "concert_admit") {
		t.Error("Secure cookie must not be reported active over plain http://")
	}
	if jar.HasActiveCookie(appURL, "empty_pass") {
		t.Error("empty cookie value must not count as active")
	}
	if jar.HasActiveCookie(appURL, "missing") {
		t.Error("absent cookie must return false")
	}

	// Repeated ReplayCookies calls must return identical deterministic sorted output.
	first := jar.ReplayCookies(appURL)
	if len(first) != 5 { // concert_admit + empty_pass + 3 sid cookies at distinct (domain, path)
		t.Fatalf("expected 5 replay cookies, got %d: %+v", len(first), first)
	}
	for range 20 {
		again := jar.ReplayCookies(appURL)
		if len(again) != len(first) {
			t.Fatalf("nondeterministic ReplayCookies length: %d vs %d", len(again), len(first))
		}
		for i := range first {
			if again[i] != first[i] {
				t.Fatalf("nondeterministic ReplayCookies at [%d]: %+v vs %+v", i, again[i], first[i])
			}
		}
	}

	// Verify each (domain, path, name) sid entry kept its exact metadata.
	seenSID := make(map[string]forensics.ReplayCookie)
	for _, rc := range first {
		if rc.Name == "sid" {
			seenSID[rc.Domain+"|"+rc.Path] = rc
		}
	}
	if got := seenSID["app.example.com|/checkout"]; got.Value != "host-checkout" || got.SameSite != "strict" || !got.HTTPOnly {
		t.Errorf("wrong metadata for host /checkout sid: %+v", got)
	}
	if got := seenSID["app.example.com|/"]; got.Value != "host-root" || got.SameSite != "lax" || !got.HTTPOnly {
		t.Errorf("wrong metadata for host / sid: %+v", got)
	}
	if got := seenSID["example.com|/"]; got.Value != "parent-root" || got.SameSite != "none" || got.HTTPOnly {
		t.Errorf("wrong metadata for parent domain sid: %+v", got)
	}

	// Deleting concert_admit marks it inactive.
	jar.SetCookies(appURL, []*http.Cookie{{Name: "concert_admit", Value: "", Path: "/", MaxAge: -1}})
	if jar.HasActiveCookie(appURL, "concert_admit") {
		t.Error("deleted concert_admit cookie must not be active")
	}

	// 3. Enforce maxTrackedCookies = 256 cap (evict expired first, then oldest-seen).
	capJar, err := NewInspectingJar(key)
	if err != nil {
		t.Fatal(err)
	}
	uCap, _ := url.Parse("https://cap.example.com/")
	// Seed 10 expired cookies first.
	for i := range 10 {
		capJar.SetCookies(uCap, []*http.Cookie{{
			Name:    fmt.Sprintf("exp_%03d", i),
			Value:   "old",
			Path:    "/",
			Expires: time.Now().Add(-time.Minute),
		}})
	}
	// Add 256 active cookies — all 10 expired entries must be evicted first, leaving 256 active.
	for i := range maxTrackedCookies {
		capJar.SetCookies(uCap, []*http.Cookie{{
			Name:  fmt.Sprintf("act_%03d", i),
			Value: "v",
			Path:  "/",
		}})
	}
	capSnap := capJar.Snapshot(uCap, time.Now())
	if capSnap.TotalCookies != maxTrackedCookies || capSnap.ExpiredCookies != 0 || capSnap.ActiveCookies != maxTrackedCookies {
		t.Fatalf("expected expired evicted first leaving 256 active, got %+v", capSnap)
	}
	// Adding 5 more active cookies evicts oldest-seen (act_000..act_004).
	for i := range 5 {
		capJar.SetCookies(uCap, []*http.Cookie{{
			Name:  fmt.Sprintf("new_%03d", i),
			Value: "v",
			Path:  "/",
		}})
	}
	capSnap2 := capJar.Snapshot(uCap, time.Now())
	if capSnap2.TotalCookies != maxTrackedCookies {
		t.Fatalf("expected cap %d, got %d", maxTrackedCookies, capSnap2.TotalCookies)
	}
	names := make(map[string]bool, len(capSnap2.Cookies))
	for _, c := range capSnap2.Cookies {
		names[c.Name] = true
	}
	for i := range 5 {
		if names[fmt.Sprintf("act_%03d", i)] {
			t.Errorf("expected oldest cookie act_%03d to be evicted", i)
		}
		if !names[fmt.Sprintf("new_%03d", i)] {
			t.Errorf("expected newest cookie new_%03d to be retained", i)
		}
	}
	if !names["act_005"] {
		t.Error("expected act_005 to still be present")
	}
}

func TestNewLemmingTransport_NetguardAndMinTLS(t *testing.T) {
	tr := newLemmingTransport()
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected TLSClientConfig MinVersion TLS 1.2, got %+v", tr.TLSClientConfig)
	}
	if tr.DialContext == nil {
		t.Fatal("expected SafeDialContext wired on transport")
	}
	for _, blocked := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80", "0.0.0.0:80"} {
		conn, err := tr.DialContext(context.Background(), "tcp", blocked)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, discovery.ErrBlockedAddress) {
			t.Errorf("expected ErrBlockedAddress for %s, got %v", blocked, err)
		}
	}
}

func TestSwarm_RunLifecycleConcurrencyAndForensics(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "player", Value: "p42", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Home</title></head><body><a href="/ok">OK</a><a href="/boom">Boom</a></body></html>`)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>OK</title></head><body><a href="/boom">Boom</a></body></html>`)
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<html><head><title>500</title></head><body><h1>Kaboom</h1></body></html>`)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>http://%[1]s/</loc></url><url><loc>http://%[1]s/ok</loc></url><url><loc>http://%[1]s/boom</loc></url></urlset>`, r.Host)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	bus := events.NewBus()
	defer bus.Close()

	var born, died, failed atomic.Int64
	seenNumbers := make(map[int64]bool)
	var mu sync.Mutex
	bus.Subscribe(func(e events.Event) {
		switch e.Kind {
		case events.LemmingBorn:
			born.Add(1)
			mu.Lock()
			seenNumbers[e.LemmingNumber] = true
			mu.Unlock()
		case events.LemmingDied:
			died.Add(1)
		case events.LemmingFailed:
			failed.Add(1)
		}
	})

	store := forensics.NewStore(20, 5)
	coord := forensics.NewCoordinator(context.Background(), store, forensics.NewHTTPFallbackCapturer("test"), bus, forensics.CoordinatorConfig{Workers: 2})

	cfg := Config{
		Hit:            srv.URL + "/",
		Terrain:        2,
		Pack:           4,
		Limit:          3,
		Until:          400 * time.Millisecond,
		Ramp:           50 * time.Millisecond,
		RampShape:      RampLinear,
		ThinkMin:       5 * time.Millisecond,
		ThinkMax:       15 * time.Millisecond,
		MaxPages:       4,
		Navigation:     "random",
		CaptureEnabled: true,
	}

	pool, err := discovery.BuildURLPool(context.Background(), cfg.Hit, discovery.BuildOptions{})
	if err != nil {
		t.Fatalf("BuildURLPool: %v", err)
	}

	sw, err := New(context.Background(), cfg, Options{
		Bus:       bus,
		Pool:      pool,
		Forensics: coord,
	})
	if err != nil {
		t.Fatalf("swarm.New: %v", err)
	}

	// Verify Swarm bus subscriptions for QueuePoll and ScoutVisit.
	bus.Emit(events.Event{Kind: events.QueuePoll, URL: srv.URL + "/queue/status", StatusCode: 200})
	bus.Emit(events.Event{Kind: events.ScoutVisit, URL: srv.URL + "/", StatusCode: 200, Failed: false})
	bus.Emit(events.Event{Kind: events.ScoutVisit, URL: srv.URL + "/boom", StatusCode: 500, Failed: true})

	if err := sw.Run(context.Background()); err != nil {
		t.Fatalf("swarm.Run: %v", err)
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = coord.DrainAndClose(drainCtx)

	if born.Load() != 8 || died.Load() != 8 || failed.Load() != 0 {
		t.Errorf("lifecycle mismatch: born=%d died=%d failed=%d (want 8, 8, 0)", born.Load(), died.Load(), failed.Load())
	}
	if sw.Metrics().LemmingsStarted.Load() != 8 || sw.Metrics().LemmingsCompleted.Load() != 8 || sw.Metrics().LemmingsAlive.Load() != 0 {
		t.Errorf("started/completed/alive mismatch: started=%d completed=%d alive=%d",
			sw.Metrics().LemmingsStarted.Load(), sw.Metrics().LemmingsCompleted.Load(), sw.Metrics().LemmingsAlive.Load())
	}
	if sw.Metrics().QueuePolls.Load() != 1 || sw.Metrics().ScoutVisits.Load() != 2 || sw.Metrics().ScoutFailures.Load() != 1 {
		t.Errorf("queue/scout metrics mismatch: polls=%d scouts=%d scoutFail=%d",
			sw.Metrics().QueuePolls.Load(), sw.Metrics().ScoutVisits.Load(), sw.Metrics().ScoutFailures.Load())
	}
	mu.Lock()
	numCount := len(seenNumbers)
	mu.Unlock()
	if numCount != 8 {
		t.Errorf("expected 8 unique sequential lemming numbers, got %d", numCount)
	}
	if sw.Metrics().Total5xx.Load() > 0 && store.Stats().Captured == 0 {
		t.Error("expected forensics store to capture at least one 500 error artifact")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	if d := ParseRetryAfter("3", now); d != 3*time.Second {
		t.Errorf("ParseRetryAfter(3) = %s, want 3s", d)
	}
	if d := ParseRetryAfter("", now); d != 0 {
		t.Errorf("empty Retry-After should return 0, got %s", d)
	}
}

func FuzzDetectWaitingRoom(f *testing.F) {
	f.Add([]byte(`<p>Your position: <b id="position">12</b></p><script>fetch('/queue/status')</script>`))
	f.Add([]byte(`<!doctype html><html lang="en"><head><script src="/_concert/waiting.js" defer></script></head><body class="queue-page"><div class="position" id="position">007</div></body></html>`))
	f.Add([]byte(`{"queued":true,"status_url":"/queue/status","retry_after_seconds":3,"position":15}`))
	f.Add([]byte(`{"queued":true,"status_url":"https://evil.example/steal","retry_after_seconds":999999,"position":-5}`))
	f.Add([]byte(`{"queued":false,"error":"waiting room is full","retry_after_seconds":5}`))
	f.Add([]byte(`<html><body>normal page</body></html>`))
	f.Add([]byte(`{"queued":true,"status_url":"//evil.example/x"}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		detected, pos := DetectWaitingRoom(body)
		if pos < 0 {
			t.Fatalf("DetectWaitingRoom returned negative position %d", pos)
		}
		if !detected && pos != 0 {
			t.Fatalf("DetectWaitingRoom returned non-zero pos %d when not detected", pos)
		}

		for _, tc := range []struct {
			status int
			ct     string
		}{
			{http.StatusOK, "text/html; charset=utf-8"},
			{http.StatusTooManyRequests, "application/json; charset=utf-8"},
			{http.StatusServiceUnavailable, "application/json"},
		} {
			sig, ok := DetectWaitingRoomResponse(tc.status, tc.ct, nil, body)
			if ok {
				if !sig.Detected {
					t.Fatal("ok=true but sig.Detected=false")
				}
				if sig.Position < 0 {
					t.Fatalf("negative signal position %d", sig.Position)
				}
				if sig.RetryAfter < 0 || sig.RetryAfter > maxRetryAfter {
					t.Fatalf("RetryAfter out of bounds: %s", sig.RetryAfter)
				}
				if !strings.HasPrefix(sig.StatusURL, "/") || strings.HasPrefix(sig.StatusURL, "//") || strings.Contains(sig.StatusURL, "://") {
					t.Fatalf("unsafe StatusURL: %q", sig.StatusURL)
				}
			}
		}
	})
}

func TestFetch_RedirectsOriginScopeBodyLimitTruncatedAndCancel(t *testing.T) {
	slowRelease := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/hop1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop2", http.StatusFound)
	})
	mux.HandleFunc("/hop2", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>landed</body></html>")
	})
	mux.HandleFunc("/offsite", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://offsite.example.test/steal", http.StatusFound)
	})
	mux.HandleFunc("/huge", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("A"), 2048))
	})
	mux.HandleFunc("/truncated", func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n<html>short")
		_ = buf.Flush()
	})
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slowRelease:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer func() {
		close(slowRelease)
		srv.Close()
	}()

	cfg := Config{
		Hit:            srv.URL + "/",
		RequestTimeout: 2 * time.Second,
		MaxBodyBytes:   512,
	}
	lem := NewLemming(1, 0, 0, cfg, &discovery.URLPool{Origin: srv.URL, URLs: []string{srv.URL + "/"}}, srv.Client(), nil, nil, nil, nil)

	// 1. Same-origin redirect chain records hop, FinishedAt, and succeeds.
	v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/hop1", nil)
	if v.Failed || v.StatusCode != 200 || len(v.Hops) == 0 || !strings.HasSuffix(v.FinalURL, "/hop2") || v.FinishedAt.IsZero() {
		t.Errorf("same-origin redirect mismatch: status=%d hops=%v final=%q finished=%v err=%v", v.StatusCode, v.Hops, v.FinalURL, v.FinishedAt, v.Error)
	}

	// 2. Off-origin redirect fails closed with ErrOffsiteRedirect.
	v, _, _, _, _ = lem.hit(context.Background(), srv.URL+"/offsite", nil)
	if !v.Failed || !errors.Is(v.Error, ErrOffsiteRedirect) {
		t.Errorf("expected ErrOffsiteRedirect, got failed=%v err=%v", v.Failed, v.Error)
	}

	// 3. Oversized body fails with ErrBodyTooLarge.
	v, _, _, _, _ = lem.hit(context.Background(), srv.URL+"/huge", nil)
	if !v.Failed || !errors.Is(v.Error, ErrBodyTooLarge) {
		t.Errorf("expected ErrBodyTooLarge, got failed=%v err=%v", v.Failed, v.Error)
	}

	// 4. Truncated body (fewer bytes than Content-Length) fails with ErrTruncatedBody -> "truncated".
	v, _, _, _, _ = lem.hit(context.Background(), srv.URL+"/truncated", nil)
	if !v.Failed || !errors.Is(v.Error, ErrTruncatedBody) || v.ErrorKind != "truncated" || ErrorKind(v.Error) != "truncated" {
		t.Errorf("expected ErrTruncatedBody ('truncated'), got failed=%v kind=%q err=%v", v.Failed, v.ErrorKind, v.Error)
	}

	// 5. Clean EOF before Content-Length via custom transport also yields ErrTruncatedBody,
	//    while Uncompressed=true does not false-positive.
	synthClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/uncompressed") {
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": []string{"text/html"}},
					ContentLength: 500,
					Uncompressed:  true,
					Body:          io.NopCloser(strings.NewReader("<html><body>ok</body></html>")),
					Request:       req,
				}, nil
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"text/html"}},
				ContentLength: 500,
				Uncompressed:  false,
				Body:          io.NopCloser(strings.NewReader("<html>short")),
				Request:       req,
			}, nil
		}),
	}
	lemSynth := NewLemming(2, 0, 1, cfg, &discovery.URLPool{Origin: "http://127.0.0.1:8080"}, synthClient, nil, nil, nil, nil)
	v, _, _, _, _ = lemSynth.hit(context.Background(), "http://127.0.0.1:8080/short", nil)
	if !v.Failed || !errors.Is(v.Error, ErrTruncatedBody) || v.ErrorKind != "truncated" {
		t.Errorf("expected clean-EOF truncation ErrTruncatedBody, got failed=%v kind=%q err=%v", v.Failed, v.ErrorKind, v.Error)
	}
	v, _, _, _, _ = lemSynth.hit(context.Background(), "http://127.0.0.1:8080/uncompressed", nil)
	if v.Failed || v.Error != nil {
		t.Errorf("expected Uncompressed=true to succeed without truncation error, got failed=%v err=%v", v.Failed, v.Error)
	}

	// 6. Parent context cancellation marks Cancelled=true and Failed=false.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, _, _, _, _ = lem.hit(cctx, srv.URL+"/hang", nil)
	if !v.Cancelled || v.Failed {
		t.Errorf("cancelled visit must set Cancelled=true, Failed=false; got cancelled=%v failed=%v err=%v", v.Cancelled, v.Failed, v.Error)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLemming_PersonaRefererJourneyAndStrictChecksum(t *testing.T) {
	var seenUA, seenLang, seenReferer []string
	var bodyVer atomic.Int32
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenUA = append(seenUA, r.UserAgent())
		seenLang = append(seenLang, r.Header.Get("Accept-Language"))
		seenReferer = append(seenReferer, r.Referer())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Home</title></head><body><a href="/step2">next</a></body></html>`)
	})
	mux.HandleFunc("/step2", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenUA = append(seenUA, r.UserAgent())
		seenLang = append(seenLang, r.Header.Get("Accept-Language"))
		seenReferer = append(seenReferer, r.Referer())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><title>Step 2</title></head><body>v%d</body></html>`, bodyVer.Add(1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	j, err := journey.Parse(strings.NewReader(`{"name":"ordered","steps":[
		{"name":"s1","path":"/","expect":{"status":[200]}},
		{"name":"s2","path":"/step2","expect":{"status":[200]}}
	]}`), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}

	pool := &discovery.URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/", srv.URL + "/step2"},
		Checksums: map[string]string{srv.URL + "/": discovery.SHA512Hex([]byte(`<html><head><title>Home</title></head><body><a href="/step2">next</a></body></html>`)), srv.URL + "/step2": "mismatched-baseline"},
	}
	cfg := Config{
		Hit:            srv.URL + "/",
		Until:          2 * time.Second,
		ThinkMin:       0,
		ThinkMax:       0,
		Navigation:     journey.NavigationLinks,
		Journey:        j,
		StrictChecksum: true,
	}
	lem := NewLemming(1, 0, 0, cfg, pool, srv.Client(), nil, nil, nil, nil)
	ll := lem.Run(context.Background())

	if ll.ExitReason != ExitJourneyComplete || ll.TotalVisits != 2 {
		t.Fatalf("expected ExitJourneyComplete with 2 visits, got exit=%q visits=%d", ll.ExitReason, ll.TotalVisits)
	}
	if len(seenUA) != 2 || seenUA[0] != seenUA[1] || seenLang[0] != seenLang[1] {
		t.Errorf("lemming changed persona mid-life: ua=%v lang=%v", seenUA, seenLang)
	}
	if !strings.HasSuffix(seenReferer[1], "/") {
		t.Errorf("second visit missing Referer from first page: %v", seenReferer)
	}
	// Step 2 had mismatched baseline checksum under -strict-checksum -> FailedVisits == 1.
	if ll.FailedVisits != 1 {
		t.Errorf("expected -strict-checksum to fail step 2, got FailedVisits=%d", ll.FailedVisits)
	}
}

func TestWaitingRoom_ConcertHTMLPollingReloadAndAdmit(t *testing.T) {
	origPoll := waitingRoomPollInterval
	waitingRoomPollInterval = 15 * time.Millisecond
	t.Cleanup(func() { waitingRoomPollInterval = origPoll })

	var pageCalls, pollCalls atomic.Int32
	var gotTicketOnPoll, gotAcceptJSON atomic.Bool
	admittedBody := `<html><head><title>Concert Origin</title></head><body><h1>Welcome</h1><a href="/next">Next</a><a href="/_concert/access">Skip</a></body></html>`

	mux := http.NewServeMux()
	mux.HandleFunc("/show", func(w http.ResponseWriter, r *http.Request) {
		n := pageCalls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if n == 1 {
			http.SetCookie(w, &http.Cookie{Name: "room_ticket", Value: "tkt-42", Path: "/", HttpOnly: true})
			fmt.Fprint(w, `<!doctype html><html lang="en"><head><script src="/_concert/waiting.js" defer></script></head><body class="queue-page"><div class="position" id="position">3</div><a href="/_concert/access">Fastlane</a></body></html>`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "admit-ok-42", Path: "/", HttpOnly: true})
		fmt.Fprint(w, admittedBody)
	})
	mux.HandleFunc("/queue/status", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("room_ticket"); err == nil && c.Value == "tkt-42" {
			gotTicketOnPoll.Store(true)
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			gotAcceptJSON.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		switch pollCalls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"ready":false,"position":2,"waiting":true}`)
		case 2:
			fmt.Fprint(w, `{"ready":false,"position":1,"waiting":true}`)
		default:
			fmt.Fprint(w, `{"ready":true,"position":0,"waiting":false}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	key, _ := secrets.NewEphemeralKey()
	jar, err := NewInspectingJar(key)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 2 * time.Second}
	bus := events.NewBus()
	defer bus.Close()

	var pollEvents []events.Event
	var visitEvents []events.Event
	var mu sync.Mutex
	bus.Subscribe(func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Kind {
		case events.QueuePoll:
			pollEvents = append(pollEvents, e)
		case events.VisitComplete, events.VisitError:
			visitEvents = append(visitEvents, e)
		}
	})

	m := &Metrics{}
	pool := &discovery.URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/show"},
		Checksums: map[string]string{srv.URL + "/show": discovery.SHA512Hex([]byte(admittedBody))},
	}
	cfg := Config{
		Hit:            srv.URL + "/show",
		Until:          2 * time.Second,
		MaxPages:       1,
		RequestTimeout: 2 * time.Second,
		StrictChecksum: true,
	}
	lem := NewLemming(1, 0, 0, cfg, pool, client, jar, bus, m, nil)
	ll := lem.Run(context.Background())

	if ll.TotalVisits != 1 || len(ll.Visits) != 1 {
		t.Fatalf("expected 1 page visit (polls must never count as visits), got total=%d len=%d", ll.TotalVisits, len(ll.Visits))
	}
	v := ll.Visits[0]
	if v.Failed || v.Cancelled {
		t.Fatalf("expected admitted visit to succeed, got failed=%v cancelled=%v err=%v checks=%+v", v.Failed, v.Cancelled, v.Error, v.Page.Checks)
	}
	if !v.WaitingRoom.Detected || v.WaitingRoom.Mode != WaitingRoomModeConcertHTML || !v.WaitingRoom.Admitted {
		t.Fatalf("unexpected waiting room state: %+v", v.WaitingRoom)
	}
	if v.WaitingRoom.Polls != 3 || v.QueuePolls != 3 || pollCalls.Load() != 3 {
		t.Errorf("expected 3 queue polls, got wr.Polls=%d v.QueuePolls=%d server=%d", v.WaitingRoom.Polls, v.QueuePolls, pollCalls.Load())
	}
	if pageCalls.Load() != 2 {
		t.Errorf("expected exactly 2 hits to /show (initial + 1 admitted reload), got %d", pageCalls.Load())
	}
	if !gotTicketOnPoll.Load() || !gotAcceptJSON.Load() {
		t.Errorf("expected room_ticket cookie and Accept: application/json on poll; ticket=%v accept=%v", gotTicketOnPoll.Load(), gotAcceptJSON.Load())
	}
	if !v.ConcertAdmit || !v.RoomTicket || v.Banned {
		t.Errorf("expected ConcertAdmit=true RoomTicket=true Banned=false, got admit=%v ticket=%v banned=%v", v.ConcertAdmit, v.RoomTicket, v.Banned)
	}
	if v.Page.Title != "Concert Origin" || !v.Match {
		t.Errorf("admitted page inspection/checksum mismatch: title=%q match=%v", v.Page.Title, v.Match)
	}
	if v.WaitingRoom.Duration <= 0 || v.Latency <= 0 || v.Latency >= v.WaitingRoom.Duration {
		t.Errorf("expected pure origin Latency (%s) separated from WaitingRoom.Duration (%s)", v.Latency, v.WaitingRoom.Duration)
	}
	if m.TotalVisits.Load() != 1 || m.Total2xx.Load() != 1 || m.TotalWaitingRoom.Load() != 1 {
		t.Errorf("metrics mismatch: total=%d 2xx=%d wr=%d", m.TotalVisits.Load(), m.Total2xx.Load(), m.TotalWaitingRoom.Load())
	}
	for _, link := range lem.links {
		if strings.Contains(link, "/_concert/") {
			t.Errorf("/_concert/* link must never be kept as site page link: %q", link)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pollEvents) != 3 {
		t.Errorf("expected 3 QueuePoll events, got %d", len(pollEvents))
	}
	if len(visitEvents) != 1 || visitEvents[0].Kind != events.VisitComplete || visitEvents[0].QueuePolls != 3 || !visitEvents[0].AdmitPass || visitEvents[0].Banned {
		t.Errorf("unexpected VisitComplete event fields: %+v", visitEvents)
	}
}

func TestWaitingRoom_ConcertAPIJSON429AndLegacyNeonArcade(t *testing.T) {
	origPoll := waitingRoomPollInterval
	waitingRoomPollInterval = 15 * time.Millisecond
	t.Cleanup(func() { waitingRoomPollInterval = origPoll })

	// 1. Concert -api-json 429 JSON queue with custom same-origin status_url.
	var apiHits, statusHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/drop", func(w http.ResponseWriter, r *http.Request) {
		if apiHits.Add(1) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "room_ticket", Value: "api-tkt", Path: "/"})
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"queued":true,"status_url":"/custom/status","retry_after_seconds":0,"position":5}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "api-admit", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/custom/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if statusHits.Add(1) == 1 {
			fmt.Fprint(w, `{"ready":false,"position":2,"waiting":true}`)
			return
		}
		fmt.Fprint(w, `{"ready":true,"position":0,"waiting":false}`)
	})
	// 2. Legacy Neon Arcade HTML (/queue/status + id="position", ready omitted -> position==0 means ready).
	var legacyHits, legacyPolls atomic.Int32
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if legacyHits.Add(1) == 1 {
			fmt.Fprint(w, `<html><body><b id="position">2</b><script>fetch('/queue/status')</script></body></html>`)
			return
		}
		fmt.Fprint(w, `<html><head><title>Checkout</title></head><body>Admitted!</body></html>`)
	})
	mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if legacyPolls.Add(1) == 1 {
			fmt.Fprint(w, `{"position":1}`)
			return
		}
		fmt.Fprint(w, `{"position":0}`)
	})
	// 3. Concert 503 breaker JSON.
	mux.HandleFunc("/full", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"queued":false,"error":"waiting room is full","retry_after_seconds":5}`)
	})
	// 4. Non-queue 429 ban.
	mux.HandleFunc("/ratelimit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "slow down")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	key, _ := secrets.NewEphemeralKey()
	jar, _ := NewInspectingJar(key)
	client := &http.Client{Jar: jar, Timeout: 2 * time.Second}
	cfg := Config{Hit: srv.URL + "/", RequestTimeout: 2 * time.Second}
	lem := NewLemming(1, 0, 0, cfg, &discovery.URLPool{Origin: srv.URL}, client, jar, nil, nil, nil)

	vAPI, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/api/drop", nil)
	if vAPI.Failed || !vAPI.WaitingRoom.Detected || vAPI.WaitingRoom.Mode != WaitingRoomModeConcertJSON || !vAPI.WaitingRoom.Admitted || vAPI.WaitingRoom.StatusURL != "/custom/status" || !vAPI.ConcertAdmit || vAPI.Banned {
		t.Fatalf("Concert -api-json admission failed: %+v wr=%+v", vAPI, vAPI.WaitingRoom)
	}

	vLeg, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/checkout", nil)
	if vLeg.Failed || !vLeg.WaitingRoom.Detected || vLeg.WaitingRoom.Mode != WaitingRoomModeLegacyHTML || !vLeg.WaitingRoom.Admitted || vLeg.Page.Title != "Checkout" {
		t.Fatalf("legacy waiting room admission failed: %+v wr=%+v title=%q", vLeg, vLeg.WaitingRoom, vLeg.Page.Title)
	}

	v503, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/full", nil)
	if !v503.Failed || v503.WaitingRoom.Detected || !v503.WaitingRoom.Rejected503 {
		t.Errorf("expected 503 breaker Rejected503=true Detected=false, got failed=%v wr=%+v", v503.Failed, v503.WaitingRoom)
	}

	v429, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/ratelimit", nil)
	if !v429.Failed || v429.WaitingRoom.Detected || !v429.Banned || v429.StatusCode != 429 {
		t.Errorf("expected non-queue 429 to set Banned=true, got failed=%v banned=%v status=%d", v429.Failed, v429.Banned, v429.StatusCode)
	}

	// Off-origin status_url in 429 JSON must be sanitized to /queue/status, and huge retry_after clamped.
	sig, ok := DetectWaitingRoomResponse(http.StatusTooManyRequests, "application/json", nil, []byte(`{"queued":true,"status_url":"https://evil.example/steal","retry_after_seconds":999999,"position":9}`))
	if !ok || sig.StatusURL != "/queue/status" || sig.RetryAfter != maxRetryAfter || sig.Position != 9 {
		t.Errorf("unsafe JSON signal not sanitized: ok=%v sig=%+v", ok, sig)
	}
}

func TestWaitingRoom_Poll429Backoff403BanEvictionCookiesAndCancel(t *testing.T) {
	origPoll := waitingRoomPollInterval
	orig429 := waitingRoom429Backoff
	origMax := waitingRoomMaxBackoff
	waitingRoomPollInterval = 15 * time.Millisecond
	waitingRoom429Backoff = 60 * time.Millisecond
	waitingRoomMaxBackoff = 120 * time.Millisecond
	t.Cleanup(func() {
		waitingRoomPollInterval = origPoll
		waitingRoom429Backoff = orig429
		waitingRoomMaxBackoff = origMax
	})

	waitingPage := `<html><head><script src="/_concert/waiting.js"></script></head><body class="queue-page"><div id="position">5</div></body></html>`

	// 1. Poll 429 triggers backoff >= waitingRoom429Backoff and then admits cleanly.
	t.Run("poll-429-backoff", func(t *testing.T) {
		var polls, pageHits atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			if pageHits.Add(1) == 1 {
				fmt.Fprint(w, waitingPage)
				return
			}
			fmt.Fprint(w, `<html><head><title>OK</title></head><body>In!</body></html>`)
		})
		mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ready":true,"position":0}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		lem := NewLemming(1, 0, 0, Config{Hit: srv.URL + "/page", RequestTimeout: 2 * time.Second}, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, nil, nil)
		start := time.Now()
		v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page", nil)
		elapsed := time.Since(start)
		if v.Failed || !v.WaitingRoom.Admitted || v.Banned || v.QueuePolls != 2 {
			t.Fatalf("expected 429 poll backoff then admission, got failed=%v banned=%v polls=%d wr=%+v", v.Failed, v.Banned, v.QueuePolls, v.WaitingRoom)
		}
		if elapsed < waitingRoom429Backoff {
			t.Errorf("poll 429 did not back off: elapsed=%s want >= %s", elapsed, waitingRoom429Backoff)
		}
	})

	// 2. Poll 403 immediately terminates waiting room, marks Denied=true, Banned=true, Failed=true, StatusCode=403.
	t.Run("poll-403-ban", func(t *testing.T) {
		var pageHits atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
			pageHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, waitingPage)
		})
		mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"banned"}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		m := &Metrics{}
		lem := NewLemming(2, 0, 0, Config{Hit: srv.URL + "/page", RequestTimeout: 2 * time.Second}, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, m, nil)
		v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page", nil)
		m.RecordVisit(&v)

		if !v.Failed || !v.Banned || !v.WaitingRoom.Denied || v.WaitingRoom.Admitted || v.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 denial termination, got failed=%v banned=%v status=%d wr=%+v", v.Failed, v.Banned, v.StatusCode, v.WaitingRoom)
		}
		if pageHits.Load() != 1 {
			t.Errorf("must not reload target after 403 poll denial; pageHits=%d", pageHits.Load())
		}
		if m.Bans403.Load() != 1 {
			t.Errorf("expected Bans403=1, got %d", m.Bans403.Load())
		}
	})

	// 3. Ticket eviction (waiting:false, ready:false) sets Expired=true, Failed=true, stops without reload.
	t.Run("poll-evicted-waiting-false", func(t *testing.T) {
		var pageHits atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
			pageHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, waitingPage)
		})
		mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ready":false,"waiting":false,"position":0}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		lem := NewLemming(3, 0, 0, Config{Hit: srv.URL + "/page", RequestTimeout: 2 * time.Second}, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, nil, nil)
		v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page", nil)
		if !v.Failed || !v.WaitingRoom.Expired || v.WaitingRoom.Admitted || v.Banned || v.ErrorKind != "queue-expired" {
			t.Fatalf("expected Expired=true Failed=true, got failed=%v banned=%v kind=%q wr=%+v", v.Failed, v.Banned, v.ErrorKind, v.WaitingRoom)
		}
		if pageHits.Load() != 1 {
			t.Errorf("must not reload target after ticket expiry; pageHits=%d", pageHits.Load())
		}
	})

	// 4. cookies_required fails cleanly without looping.
	t.Run("poll-cookies-required", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, waitingPage)
		})
		mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"cookies_required":true}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		lem := NewLemming(4, 0, 0, Config{Hit: srv.URL + "/page", RequestTimeout: 2 * time.Second}, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, nil, nil)
		v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page", nil)
		if !v.Failed || v.ErrorKind != "queue-cookies" || v.QueuePolls != 1 {
			t.Fatalf("expected clean cookies_required failure on poll 1, got failed=%v kind=%q polls=%d", v.Failed, v.ErrorKind, v.QueuePolls)
		}
	})

	// 5. Transient 502 error on poll backs off exponentially and recovers when queue answers ready.
	t.Run("poll-transient-error-backoff-recovery", func(t *testing.T) {
		var polls, pageHits atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			if pageHits.Add(1) == 1 {
				fmt.Fprint(w, waitingPage)
				return
			}
			fmt.Fprint(w, `<html><head><title>Recovered</title></head><body>OK</body></html>`)
		})
		mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
			if polls.Add(1) <= 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ready":true,"position":0}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		lem := NewLemming(6, 0, 0, Config{Hit: srv.URL + "/page", RequestTimeout: 2 * time.Second}, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, nil, nil)
		v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page", nil)
		if v.Failed || !v.WaitingRoom.Admitted || v.QueuePolls != 3 || v.Page.Title != "Recovered" {
			t.Fatalf("expected transient poll recovery on poll 3, got failed=%v polls=%d title=%q wr=%+v", v.Failed, v.QueuePolls, v.Page.Title, v.WaitingRoom)
		}
	})

	// 6. Context cancellation exits waiting room promptly.
	t.Run("poll-cancel", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, waitingPage)
		})
		mux.HandleFunc("/queue/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ready":false,"waiting":true,"position":99}`)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		lem := NewLemming(5, 0, 0, Config{Hit: srv.URL + "/page", RequestTimeout: 2 * time.Second}, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, nil, nil)
		v, _, _, _, _ := lem.hit(cctx, srv.URL+"/page", nil)
		if !v.WaitingRoom.Detected || !v.Cancelled || v.Failed {
			t.Errorf("cancelled waiting room should set Detected=true Cancelled=true Failed=false, got wr=%+v cancelled=%v failed=%v", v.WaitingRoom, v.Cancelled, v.Failed)
		}
	})
}

func TestAssets_AdmitPassEnforcementCSSSubAssetsAndPerLemmingCache(t *testing.T) {
	var mu sync.Mutex
	assetHits := make(map[string]int)

	mux := http.NewServeMux()
	mux.HandleFunc("/page-no-pass", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>NoPass</title><link rel="stylesheet" href="/assets/app.css"><link rel="stylesheet" href="/_concert/waiting.css"></head><body><img src="/assets/logo.svg"><script src="https://offsite.example.test/evil.js"></script></body></html>`)
	})
	mux.HandleFunc("/page-admit", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "pass-777", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Admitted</title><link rel="stylesheet" href="/assets/app.css"></head><body><img src="/assets/logo.svg"></body></html>`)
	})
	mux.HandleFunc("/page-2", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Reuses /assets/app.css and /assets/logo.svg (should be cached!) plus one new /assets/extra.js.
		fmt.Fprint(w, `<html><head><title>Page 2</title><link rel="stylesheet" href="/assets/app.css"></head><body><img src="/assets/logo.svg"><script src="/assets/extra.js"></script></body></html>`)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		assetHits[r.URL.Path]++
		mu.Unlock()
		c, err := r.Cookie("concert_admit")
		if err != nil || c.Value != "pass-777" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "admit pass required")
			return
		}
		switch r.URL.Path {
		case "/assets/app.css":
			w.Header().Set("Content-Type", "text/css")
			fmt.Fprint(w, `@font-face{src:url(/assets/arcade.woff2)} body{background:url('/assets/bg.png')}`)
		case "/assets/logo.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			fmt.Fprint(w, `<svg></svg>`)
		case "/assets/arcade.woff2":
			w.Header().Set("Content-Type", "font/woff2")
			fmt.Fprint(w, "WOFF2DATA")
		case "/assets/bg.png":
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "PNGDATA")
		case "/assets/extra.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, "console.log('ok');")
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	key, _ := secrets.NewEphemeralKey()
	jar, _ := NewInspectingJar(key)
	client := &http.Client{Jar: jar, Timeout: 2 * time.Second}
	bus := events.NewBus()
	defer bus.Close()

	var assetEvts []events.Event
	var evMu sync.Mutex
	bus.Subscribe(events.Filter(func(e events.Event) {
		evMu.Lock()
		assetEvts = append(assetEvts, e)
		evMu.Unlock()
	}, events.AssetComplete, events.AssetError))

	m := &Metrics{}
	cfg := Config{
		Hit:            srv.URL + "/",
		RequestTimeout: 2 * time.Second,
		Assets:         true,
		AssetsPerPage:  8,
	}
	lem := NewLemming(1, 0, 0, cfg, &discovery.URLPool{Origin: srv.URL}, client, jar, bus, m, nil)

	// Step 1: Without concert_admit pass -> both assets receive 403 and are NOT cached.
	v1, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page-no-pass", nil)
	if v1.Failed {
		t.Fatalf("page-no-pass should succeed: %+v", v1)
	}
	if m.TotalVisits.Load() != 0 {
		t.Errorf("assets must never increment page TotalVisits, got %d", m.TotalVisits.Load())
	}
	if m.AssetRequests.Load() != 2 || m.AssetFailed.Load() != 2 || m.AssetDenied403.Load() != 2 {
		t.Fatalf("expected 2 denied 403 assets, got req=%d fail=%d denied403=%d",
			m.AssetRequests.Load(), m.AssetFailed.Load(), m.AssetDenied403.Load())
	}

	// Step 2: Page sets concert_admit -> /assets/app.css (200) + /assets/logo.svg (200)
	//         + CSS sub-assets /assets/arcade.woff2 (200) + /assets/bg.png (200) all succeed.
	v2, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page-admit", nil)
	if v2.Failed || !v2.ConcertAdmit {
		t.Fatalf("page-admit failed or missing admit cookie: failed=%v admit=%v", v2.Failed, v2.ConcertAdmit)
	}
	if m.AssetRequests.Load() != 6 || m.AssetFailed.Load() != 2 || m.AssetDenied403.Load() != 2 || m.AssetBytes.Load() <= 0 {
		t.Fatalf("expected 4 new successful assets (including CSS sub-assets), got req=%d fail=%d denied403=%d bytes=%d",
			m.AssetRequests.Load(), m.AssetFailed.Load(), m.AssetDenied403.Load(), m.AssetBytes.Load())
	}

	// Step 3: Second page reuses cached app.css and logo.svg, fetching ONLY /assets/extra.js.
	v3, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/page-2", nil)
	if v3.Failed {
		t.Fatalf("page-2 failed: %+v", v3)
	}
	if m.AssetRequests.Load() != 7 || m.AssetFailed.Load() != 2 {
		t.Fatalf("expected per-lemming cache to skip app.css & logo.svg and fetch only extra.js; req=%d fail=%d",
			m.AssetRequests.Load(), m.AssetFailed.Load())
	}

	mu.Lock()
	if assetHits["/assets/app.css"] != 2 || assetHits["/assets/logo.svg"] != 2 || assetHits["/assets/arcade.woff2"] != 1 || assetHits["/assets/bg.png"] != 1 || assetHits["/assets/extra.js"] != 1 {
		t.Errorf("unexpected server asset hit counts: %+v", assetHits)
	}
	mu.Unlock()

	evMu.Lock()
	defer evMu.Unlock()
	if len(assetEvts) != 7 {
		t.Fatalf("expected 7 asset events, got %d", len(assetEvts))
	}
	var sawCSS, sawFont, sawImage, sawScript, sawDenied int
	for _, e := range assetEvts {
		av, ok := e.Payload.(*AssetVisit)
		if !ok || av == nil {
			t.Fatalf("asset event missing *AssetVisit payload: %+v", e)
		}
		if e.Kind == events.AssetError && e.StatusCode == 403 && e.Banned && !e.AdmitPass {
			sawDenied++
		}
		if e.Kind == events.AssetComplete && e.AdmitPass {
			switch e.AssetClass {
			case "style":
				sawCSS++
			case "font":
				sawFont++
			case "image":
				sawImage++
			case "script":
				sawScript++
			}
		}
	}
	if sawDenied != 2 || sawCSS != 1 || sawFont != 1 || sawImage != 2 || sawScript != 1 {
		t.Errorf("asset event class breakdown mismatch: denied=%d css=%d font=%d img=%d js=%d", sawDenied, sawCSS, sawFont, sawImage, sawScript)
	}

	// Step 4: Verify AssetsPerPage budget cap (cap=2 on 4-asset page) and Assets=false skips assets.
	jar2, _ := NewInspectingJar(key)
	client2 := &http.Client{Jar: jar2, Timeout: 2 * time.Second}
	mCap := &Metrics{}
	lemCap := NewLemming(2, 0, 1, Config{Hit: srv.URL + "/", RequestTimeout: 2 * time.Second, Assets: true, AssetsPerPage: 2}, &discovery.URLPool{Origin: srv.URL}, client2, jar2, nil, mCap, nil)
	vCap, _, _, _, _ := lemCap.hit(context.Background(), srv.URL+"/page-admit", nil)
	if vCap.Failed || mCap.AssetRequests.Load() != 2 {
		t.Errorf("expected AssetsPerPage=2 to cap asset fetches at 2, got failed=%v req=%d", vCap.Failed, mCap.AssetRequests.Load())
	}

	mOff := &Metrics{}
	lemOff := NewLemming(3, 0, 2, Config{Hit: srv.URL + "/", RequestTimeout: 2 * time.Second, Assets: false, AssetsPerPage: 8}, &discovery.URLPool{Origin: srv.URL}, client2, jar2, nil, mOff, nil)
	vOff, _, _, _, _ := lemOff.hit(context.Background(), srv.URL+"/page-admit", nil)
	if vOff.Failed || mOff.AssetRequests.Load() != 0 {
		t.Errorf("expected Assets=false to fetch 0 assets, got failed=%v req=%d", vOff.Failed, mOff.AssetRequests.Load())
	}
}

func TestMetrics_CancelledVisitsExcludedFromStatusClasses(t *testing.T) {
	m := &Metrics{}
	m.RecordVisit(&Visit{StatusCode: 200, BytesIn: 100, Cancelled: true, WaitingRoom: WaitingRoomMetric{Detected: true}})
	m.RecordVisit(&Visit{StatusCode: 500, BytesIn: 50, Cancelled: true, Failed: true, Banned: true})
	m.RecordVisit(&Visit{StatusCode: 200, BytesIn: 200})
	m.RecordVisit(&Visit{StatusCode: 403, BytesIn: 30, Failed: true, Banned: true})
	m.RecordVisit(&Visit{StatusCode: 429, BytesIn: 40, Failed: true, Banned: true})

	if m.TotalVisits.Load() != 5 {
		t.Errorf("TotalVisits=%d want 5", m.TotalVisits.Load())
	}
	if m.TotalBytes.Load() != 420 {
		t.Errorf("TotalBytes=%d want 420", m.TotalBytes.Load())
	}
	if m.TotalWaitingRoom.Load() != 1 {
		t.Errorf("TotalWaitingRoom=%d want 1", m.TotalWaitingRoom.Load())
	}
	if m.CancelledVisits.Load() != 2 {
		t.Errorf("CancelledVisits=%d want 2", m.CancelledVisits.Load())
	}
	if m.Total2xx.Load() != 1 || m.Total3xx.Load() != 0 || m.Total4xx.Load() != 2 || m.Total5xx.Load() != 0 {
		t.Errorf("cancelled visits leaked into status classes: 2xx=%d 3xx=%d 4xx=%d 5xx=%d",
			m.Total2xx.Load(), m.Total3xx.Load(), m.Total4xx.Load(), m.Total5xx.Load())
	}
	if m.FailedVisits.Load() != 2 || m.Bans403.Load() != 1 || m.Bans429.Load() != 1 {
		t.Errorf("failure/ban counts mismatch: failed=%d bans403=%d bans429=%d",
			m.FailedVisits.Load(), m.Bans403.Load(), m.Bans429.Load())
	}
}

func TestThink_RetryAfterAndZeroThinkBackoff(t *testing.T) {
	lem := NewLemming(1, 0, 0, Config{ThinkMin: 0, ThinkMax: 0}, &discovery.URLPool{}, http.DefaultClient, nil, nil, nil, nil)

	// 1. Failed visit with zero think time enforces failureBackoff.
	start := time.Now()
	if !lem.think(context.Background(), &Visit{Failed: true, StatusCode: 500}) {
		t.Fatal("think returned false on uncancelled context")
	}
	if elapsed := time.Since(start); elapsed < failureBackoff/2 {
		t.Errorf("zero-think failure did not back off: elapsed=%s want >= %s", elapsed, failureBackoff/2)
	}

	// 2. 429 with Retry-After honors Retry-After (and aborts immediately on ctx cancel).
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	if lem.think(cctx, &Visit{Failed: true, StatusCode: 429, RetryAfter: 5 * time.Second}) {
		t.Fatal("think should return false when Retry-After wait is cancelled")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("think did not exit promptly on context cancel during Retry-After")
	}
}

func TestSwarm_SendLifeLogOverflowAndDrop(t *testing.T) {
	s := &Swarm{
		primary:  make(chan LifeLog, 1),
		overflow: make(chan LifeLog, 1),
		events:   events.NewBus(),
	}
	defer s.events.Close()

	s.SendLifeLog(LifeLog{Terrain: 0}) // fills primary
	s.SendLifeLog(LifeLog{Terrain: 1}) // spills to overflow
	s.SendLifeLog(LifeLog{Terrain: 2}) // drops non-blockingly

	if s.metrics.OverflowLogs.Load() != 1 || s.metrics.DroppedLogs.Load() != 1 {
		t.Errorf("backpressure counters mismatch: overflow=%d dropped=%d (want 1, 1)",
			s.metrics.OverflowLogs.Load(), s.metrics.DroppedLogs.Load())
	}
}

func TestTerrain_SemaphoreCapStartedVsCompletedAndCancel(t *testing.T) {
	var inFlight, peak atomic.Int64
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			prev := peak.Load()
			if cur <= prev || peak.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>ok</body></html>")
	}))
	defer srv.Close()

	// 1. Semaphore cap of 2 strictly bounds 6 pack lemmings; LemmingsStarted increments on birth.
	sem, err := sema.New(2)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	defer bus.Close()
	m := &Metrics{}
	cfg := Config{Hit: srv.URL + "/", Pack: 6, Limit: 2, Until: 2 * time.Second, MaxPages: 1, Navigation: journey.NavigationRandom}
	pool := &discovery.URLPool{Origin: srv.URL, URLs: []string{srv.URL + "/"}}

	tr := NewTerrain(0, cfg, pool, nil, bus, sem, m, nil, nil, nil)
	tr.Launch(context.Background())

	// Wait until 2 lemmings have started and are blocked on gate — Started > Completed mid-flight.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.LemmingsStarted.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if m.LemmingsStarted.Load() < 2 || m.LemmingsCompleted.Load() != 0 {
		t.Errorf("expected Started>=2 and Completed==0 while blocked on gate; started=%d completed=%d",
			m.LemmingsStarted.Load(), m.LemmingsCompleted.Load())
	}

	// Release all handlers promptly once spawned.
	close(gate)
	tr.Wait()

	if peak.Load() > 2 {
		t.Errorf("semaphore cap breached: peak=%d > 2", peak.Load())
	}
	if m.LemmingsStarted.Load() != 6 || m.LemmingsCompleted.Load() != 6 || m.LemmingsAlive.Load() != 0 {
		t.Errorf("unexpected metrics: started=%d completed=%d alive=%d",
			m.LemmingsStarted.Load(), m.LemmingsCompleted.Load(), m.LemmingsAlive.Load())
	}

	// 2. Cancelled context while semaphore is full emits LemmingFailed for unborn lemmings without incrementing Started.
	semFull, _ := sema.New(1)
	_ = semFull.AcquireWith(context.Background())
	defer func() { _ = semFull.Release() }()
	var failedEvts atomic.Int64
	bus.Subscribe(events.Filter(func(events.Event) { failedEvts.Add(1) }, events.LemmingFailed))

	m2 := &Metrics{}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr2 := NewTerrain(1, Config{Hit: srv.URL + "/", Pack: 3, Limit: 1, Until: time.Second}, pool, nil, bus, semFull, m2, nil, nil, nil)
	tr2.Launch(cctx)
	tr2.Wait()

	if failedEvts.Load() != 3 || m2.LemmingsFailed.Load() != 3 || m2.LemmingsStarted.Load() != 0 || m2.LemmingsCompleted.Load() != 0 {
		t.Errorf("expected 3 unborn LemmingFailed with 0 started/completed, got evts=%d failed=%d started=%d completed=%d",
			failedEvts.Load(), m2.LemmingsFailed.Load(), m2.LemmingsStarted.Load(), m2.LemmingsCompleted.Load())
	}
}

func BenchmarkDetectWaitingRoom(b *testing.B) {
	queuePage := []byte(`<!doctype html><html><body><h1>Waiting</h1><b id="position">42</b><script>fetch('/queue/status')</script></body></html>`)
	concertPage := []byte(`<!doctype html><html><head><script src="/_concert/waiting.js" defer></script></head><body class="queue-page"><div class="position" id="position">12</div></body></html>`)
	normalPage := []byte(`<!doctype html><html><head><title>Neon Arcade</title></head><body><main>Insert coin</main></body></html>`)
	b.ResetTimer()
	for range b.N {
		_, _ = DetectWaitingRoom(queuePage)
		_, _ = DetectWaitingRoom(concertPage)
		_, _ = DetectWaitingRoom(normalPage)
	}
}
