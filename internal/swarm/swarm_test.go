package swarm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if base.RampShape != RampLinear || base.CaptureFormat != CaptureFormatJPEG || base.CaptureMax != 50 {
		t.Errorf("defaults not applied: %+v", base)
	}

	// Reject bad ramp shape.
	c := base
	c.RampShape = "exponential"
	if err := c.Validate(); err == nil {
		t.Error("expected invalid ramp-shape error")
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

	if err := sw.Run(context.Background()); err != nil {
		t.Fatalf("swarm.Run: %v", err)
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = coord.DrainAndClose(drainCtx)

	if born.Load() != 8 || died.Load() != 8 || failed.Load() != 0 {
		t.Errorf("lifecycle mismatch: born=%d died=%d failed=%d (want 8, 8, 0)", born.Load(), died.Load(), failed.Load())
	}
	if sw.Metrics().LemmingsAlive.Load() != 0 {
		t.Errorf("expected LemmingsAlive=0 after Run, got %d", sw.Metrics().LemmingsAlive.Load())
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
	f.Add([]byte(`<html><body>normal page</body></html>`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = DetectWaitingRoom(body)
	})
}

func TestFetch_RedirectsOriginScopeBodyLimitAndCancel(t *testing.T) {
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

	// 1. Same-origin redirect chain records hop and succeeds.
	v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/hop1", nil)
	if v.Failed || v.StatusCode != 200 || len(v.Hops) == 0 || !strings.HasSuffix(v.FinalURL, "/hop2") {
		t.Errorf("same-origin redirect mismatch: status=%d hops=%v final=%q err=%v", v.StatusCode, v.Hops, v.FinalURL, v.Error)
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

	// 4. Parent context cancellation marks Cancelled=true and Failed=false.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, _, _, _, _ = lem.hit(cctx, srv.URL+"/hang", nil)
	if !v.Cancelled || v.Failed {
		t.Errorf("cancelled visit must set Cancelled=true, Failed=false; got cancelled=%v failed=%v err=%v", v.Cancelled, v.Failed, v.Error)
	}
}

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

func TestWaitingRoom_PollingAdmissionAndCancel(t *testing.T) {
	origPoll := waitingRoomPollInterval
	waitingRoomPollInterval = 15 * time.Millisecond
	t.Cleanup(func() { waitingRoomPollInterval = origPoll })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		if n <= 2 {
			fmt.Fprintf(w, `<html><body><b id="position">%d</b><script>fetch('/queue/status')</script></body></html>`, 3-n)
			return
		}
		fmt.Fprint(w, `<html><head><title>Checkout</title></head><body>Admitted!</body></html>`)
	}))
	defer srv.Close()

	cfg := Config{Hit: srv.URL + "/", RequestTimeout: 2 * time.Second}
	lem := NewLemming(1, 0, 0, cfg, &discovery.URLPool{Origin: srv.URL}, srv.Client(), nil, nil, nil, nil)

	// 1. Admitted on 3rd poll, queue duration recorded separately from final page latency.
	v, _, _, _, _ := lem.hit(context.Background(), srv.URL+"/checkout", nil)
	if v.Failed || !v.WaitingRoom.Detected || v.WaitingRoom.Duration <= 0 || v.Page.Title != "Checkout" {
		t.Fatalf("waiting room admission failed: failed=%v wr=%+v title=%q", v.Failed, v.WaitingRoom, v.Page.Title)
	}

	// 2. Context cancellation exits waiting room promptly.
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><b id="position">99</b><script>fetch('/queue/status')</script></body></html>`)
	}))
	defer stuck.Close()

	cctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	lem2 := NewLemming(2, 0, 1, cfg, &discovery.URLPool{Origin: stuck.URL}, stuck.Client(), nil, nil, nil, nil)
	v2, _, _, _, _ := lem2.hit(cctx, stuck.URL+"/checkout", nil)
	if !v2.WaitingRoom.Detected || !v2.Cancelled {
		t.Errorf("cancelled waiting room should set Detected=true Cancelled=true, got wr=%+v cancelled=%v", v2.WaitingRoom, v2.Cancelled)
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

func TestTerrain_SemaphoreCapAndCancel(t *testing.T) {
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

	// 1. Semaphore cap of 2 strictly bounds 6 pack lemmings.
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
	// Release all handlers promptly once spawned.
	close(gate)
	tr.Wait()

	if peak.Load() > 2 {
		t.Errorf("semaphore cap breached: peak=%d > 2", peak.Load())
	}
	if m.LemmingsCompleted.Load() != 6 || m.LemmingsAlive.Load() != 0 {
		t.Errorf("unexpected metrics: completed=%d alive=%d", m.LemmingsCompleted.Load(), m.LemmingsAlive.Load())
	}

	// 2. Cancelled context while semaphore is full emits LemmingFailed for unborn lemmings.
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

	if failedEvts.Load() != 3 || m2.LemmingsFailed.Load() != 3 {
		t.Errorf("expected 3 unborn LemmingFailed events, got evts=%d metric=%d", failedEvts.Load(), m2.LemmingsFailed.Load())
	}
}

func BenchmarkDetectWaitingRoom(b *testing.B) {
	queuePage := []byte(`<!doctype html><html><body><h1>Waiting</h1><b id="position">42</b><script>fetch('/queue/status')</script></body></html>`)
	normalPage := []byte(`<!doctype html><html><head><title>Neon Arcade</title></head><body><main>Insert coin</main></body></html>`)
	b.ResetTimer()
	for range b.N {
		_, _ = DetectWaitingRoom(queuePage)
		_, _ = DetectWaitingRoom(normalPage)
	}
}
