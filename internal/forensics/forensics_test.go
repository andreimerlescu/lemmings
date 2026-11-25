package forensics

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"golang.org/x/net/websocket"
)

func TestStore_CapsDedupReviewAndRedaction(t *testing.T) {
	st := NewStore(3, 2)
	fp1 := Fingerprint("/tokens", 500, "status 500")
	fp2 := Fingerprint("/scores", 503, "status 503")

	// Per-fingerprint cap = 2.
	if ok, _ := st.TryReserve(fp1); !ok {
		t.Fatal("first reservation for fp1 should succeed")
	}
	if ok, _ := st.TryReserve(fp1); !ok {
		t.Fatal("second reservation for fp1 should succeed")
	}
	if ok, reason := st.TryReserve(fp1); ok || reason != DropDeduped {
		t.Fatalf("third fp1 should dedup, got ok=%v reason=%q", ok, reason)
	}

	// Global cap = 3.
	if ok, _ := st.TryReserve(fp2); !ok {
		t.Fatal("fp2 should fill 3rd global slot")
	}
	if ok, reason := st.TryReserve(Fingerprint("/other", 404, "404")); ok || reason != DropCapReached {
		t.Fatalf("4th slot should hit cap, got ok=%v reason=%q", ok, reason)
	}

	// Save artifact with raw cookie values — reports MUST redact raw values;
	// portal ONLY exposes raw values when revealCookies=true.
	art := Artifact{
		Fingerprint:   fp1,
		CapturedAt:    time.Now(),
		URL:           "http://127.0.0.1/tokens",
		Path:          "/tokens",
		StatusCode:    500,
		TriggerReason: "status 500",
		CaptureMode:   ModeCDPReplay,
		Session: SessionSnapshot{
			LemmingNumber: 12,
			LemmingID:     "t0-p11",
			LemmingName:   "Pixel Hopper",
			VisitSequence: 4,
			CookieJar: CookieJarSnapshot{
				TotalCookies: 1,
				Cookies: []CookieStatus{
					{Name: "session", Domain: "127.0.0.1", Path: "/", HTTPOnly: true, Fingerprint: "abc123", RawValue: "SUPER_SECRET_COOKIE_VAL"},
				},
			},
		},
		Screenshot: &Screenshot{
			MIMEType: "image/jpeg",
			Width:    1280,
			Height:   800,
			Bytes:    4,
			Data:     []byte{0xff, 0xd8, 0xff, 0xd9},
		},
	}
	sum := st.Save(art)
	if sum.ID == "" || !sum.HasScreenshot || sum.LemmingNumber != 12 {
		t.Fatalf("unexpected summary: %+v", sum)
	}

	// Default portal view (revealCookies=false) MUST NOT expose raw value anywhere.
	pvHidden, ok := st.GetForPortal(sum.ID, false)
	if !ok {
		t.Fatal("GetForPortal(false) returned not found")
	}
	if pvHidden.RevealCookies {
		t.Error("expected RevealCookies=false")
	}
	for _, pc := range pvHidden.PortalCookies {
		if pc.Value != "" || pc.RawValue != "" {
			t.Errorf("portal view with reveal=false leaked cookie value: %+v", pc)
		}
	}
	if len(pvHidden.Session.CookieJar.Cookies) != 1 || pvHidden.Session.CookieJar.Cookies[0].RawValue != "" {
		t.Error("GetForPortal must strip RawValue on embedded Session.CookieJar.Cookies")
	}

	// Opt-in portal view (revealCookies=true) exposes raw value in PortalCookies.
	pvShown, ok := st.GetForPortal(sum.ID, true)
	if !ok {
		t.Fatal("GetForPortal(true) returned not found")
	}
	if !pvShown.RevealCookies || len(pvShown.PortalCookies) != 1 || pvShown.PortalCookies[0].Value != "SUPER_SECRET_COOKIE_VAL" {
		t.Errorf("expected raw value with reveal=true, got %+v", pvShown.PortalCookies)
	}

	// Admin triage review + validation.
	if _, err := st.Review(sum.ID, ReviewStatus("bogus"), "bad"); err == nil {
		t.Error("expected invalid review status to fail")
	}
	upd, err := st.Review(sum.ID, ReviewBug, "Confirmed hopper jam on checkout")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if upd.ReviewStatus != ReviewBug || upd.Stats.Bug != 1 || upd.Stats.Unreviewed != 0 {
		t.Errorf("unexpected review update: %+v", upd)
	}

	// SnapshotForReport MUST strip all RawValues and keep ReviewStatus + ReviewNote.
	repArts, stats := st.SnapshotForReport()
	if len(repArts) != 1 || stats.Bug != 1 {
		t.Fatalf("unexpected report snapshot: len=%d stats=%+v", len(repArts), stats)
	}
	if repArts[0].ReviewStatus != ReviewBug || repArts[0].ReviewNote != "Confirmed hopper jam on checkout" {
		t.Errorf("review triage lost in report snapshot: %+v", repArts[0])
	}
	if repArts[0].Session.CookieJar.Cookies[0].RawValue != "" {
		t.Fatal("SEC-09/S5 VIOLATION: raw cookie value leaked into SnapshotForReport!")
	}
}

func TestMinimalBrowserEnv_ExcludesHostSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "super-secret-aws-key")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "ops_secret_token_value")
	t.Setenv("LEMMINGS_SMTP_PASS", "smtp-secret-pass")

	env := minimalBrowserEnv("/tmp/fake-chrome-profile")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"AWS_SECRET_ACCESS_KEY", "OP_SERVICE_ACCOUNT_TOKEN", "LEMMINGS_SMTP_PASS", "super-secret-aws-key", "ops_secret_token_value"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("SEC-09 violation: minimalBrowserEnv leaked %q", forbidden)
		}
	}
	if !strings.Contains(joined, "HOME=/tmp/fake-chrome-profile") {
		t.Errorf("expected isolated HOME in minimalBrowserEnv, got %v", env)
	}
}

func TestCoordinator_NonBlockingAndDrain(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	var capturedCount int
	var mu sync.Mutex
	bus.Subscribe(events.Filter(func(e events.Event) {
		mu.Lock()
		capturedCount++
		mu.Unlock()
	}, events.ArtifactCaptured))

	store := NewStore(10, 5)
	capImpl := NewHTTPFallbackCapturer("unit-test")
	coord := NewCoordinator(context.Background(), store, capImpl, bus, CoordinatorConfig{Workers: 2, Queue: 8})

	for i := range 4 {
		ok := coord.Submit(CaptureRequest{
			URL:           fmt.Sprintf("http://127.0.0.1/err/%d", i),
			SafeURL:       fmt.Sprintf("http://127.0.0.1/err/%d", i),
			Path:          fmt.Sprintf("/err/%d", i),
			StatusCode:    500,
			TriggerReason: "status 500",
			ContentType:   "text/html",
			RawBody:       []byte("<h1>Internal Server Error</h1>"),
			Session:       SessionSnapshot{LemmingNumber: int64(i + 1), LemmingID: fmt.Sprintf("l-%d", i)},
		})
		if !ok {
			t.Fatalf("submit %d should have succeeded", i)
		}
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := coord.DrainAndClose(drainCtx); err != nil {
		t.Fatalf("DrainAndClose: %v", err)
	}

	mu.Lock()
	got := capturedCount
	mu.Unlock()
	if got != 4 {
		t.Errorf("expected 4 ArtifactCaptured events, got %d", got)
	}
}

// TestCDP_LiveHeadlessBrowserCaptureAndScout verifies real Chrome DevTools Protocol
// screenshots, isolated incognito cookie injection, JS console error & uncaught
// exception collection, and real-browser Scout detection on an HTTP 200 page.
func TestCDP_LiveHeadlessBrowserCaptureAndScout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Chromium CDP test in -short mode")
	}
	if _, err := FindBrowser(""); err != nil {
		t.Skipf("no local Chrome/Chromium installed (%v); skipping live CDP test", err)
	}

	var seenCookie string
	var mu sync.Mutex
	offOriginSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "third_party_secret", Value: "must-not-capture", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><h1>THIRD_PARTY_SECRET_DOM</h1></body></html>`)
	}))
	defer offOriginSrv.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, `{"ok":true,"service":"lemmings"}`)
	})
	mux.HandleFunc("/escape", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, offOriginSrv.URL+"/landing", http.StatusFound)
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session_id"); err == nil {
			mu.Lock()
			seenCookie = c.Value
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Request-Id", "req-cdp-500")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<!doctype html><html><head><title>500 Crash</title></head>
			<body style="background:#241b2f;color:#ff7edb;font-family:sans-serif;padding:40px">
			<h1>500 — Checkout Engine Exploded</h1>
			<p>Session state captured by Lemmings CDP forensics.</p>
			<script>
			console.error("fatal checkout hydration error");
			window.nonExistentObject.triggerCrash();
			</script></body></html>`)
	})
	mux.HandleFunc("/js-fault-200", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Looks 200 OK</title></head>
			<body style="background:#171520;color:#36f9f6;padding:40px">
			<h1>HTTP 200 OK with JS Crash</h1>
			<script>
			console.error("scout caught invisible frontend error");
			throw new Error("uncaught-scout-boom");
			</script></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cdp, err := NewCDPCapturer(ctx, CDPOptions{Format: "jpeg", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewCDPCapturer: %v", err)
	}
	defer func() { _ = cdp.Close() }()

	// 1. Replay failing visit with cookies -> verify JPEG screenshot, cookie injection, and JS console errors.
	art, err := cdp.Capture(ctx, CaptureRequest{
		URL:           srv.URL + "/broken",
		SafeURL:       srv.URL + "/broken",
		Origin:        srv.URL,
		Path:          "/broken",
		StatusCode:    500,
		TriggerReason: "status 500",
		ContentType:   "text/html",
		Headers:       http.Header{"X-Request-Id": {"req-cdp-500"}},
		RawBody:       []byte("<h1>500 — Checkout Engine Exploded</h1>"),
		Session: SessionSnapshot{
			LemmingNumber: 7,
			LemmingID:     "t0-p6",
			LemmingName:   "Neon Runner",
			UserAgent:     "Mozilla/5.0 Test",
			Language:      "en-US",
		},
		ReplayCookies: []ReplayCookie{
			{Name: "session_id", Value: "lem-cookie-777", Domain: "127.0.0.1", Path: "/", HTTPOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("cdp.Capture: %v (warning=%q)", err, art.CaptureWarning)
	}
	if art.Screenshot == nil || len(art.Screenshot.Data) < 500 || !bytes.HasPrefix(art.Screenshot.Data, []byte{0xff, 0xd8}) {
		t.Fatalf("expected valid JPEG screenshot bytes, got %+v", art.Screenshot)
	}
	mu.Lock()
	gotCookie := seenCookie
	mu.Unlock()
	if gotCookie != "lem-cookie-777" {
		t.Errorf("CDP replay did not inject lemming cookie: got %q, want lem-cookie-777", gotCookie)
	}
	if art.ConsoleErrors < 2 {
		t.Errorf("expected >= 2 JS console/exception errors, got %d (%+v)", art.ConsoleErrors, art.Console)
	}
	if art.ErrorBody.RequestID != "req-cdp-500" || !strings.Contains(art.ErrorBody.Headline, "Checkout Engine Exploded") {
		t.Errorf("unexpected extracted error body: %+v", art.ErrorBody)
	}

	// 2. Run a real-browser Scout on the HTTP 200 JS-crash page and verify it captures a scout artifact.
	store := NewStore(10, 3)
	bus := events.NewBus()
	defer bus.Close()
	var liveScoutVisits []events.Event
	var liveMu sync.Mutex
	bus.Subscribe(events.Filter(func(e events.Event) {
		liveMu.Lock()
		liveScoutVisits = append(liveScoutVisits, e)
		liveMu.Unlock()
	}, events.ScoutVisit))

	fpKey, _ := secrets.NewEphemeralKey()
	j, _ := journey.Parse(bytes.NewReader([]byte(`{"name":"scout-check","steps":[{"name":"glitch","path":"/js-fault-200"}]}`)), srv.URL+"/")

	RunScouts(ctx, ScoutConfig{
		Count:          1,
		Origin:         srv.URL,
		Hit:            srv.URL + "/",
		URLs:           []string{srv.URL + "/js-fault-200"},
		Journey:        j,
		Until:          6 * time.Second,
		ThinkMin:       10 * time.Millisecond,
		ThinkMax:       20 * time.Millisecond,
		NextNumber:     func() int64 { return 99 },
		FingerprintKey: fpKey,
	}, cdp, store, bus)

	arts, stats := store.SnapshotForReport()
	if stats.Captured == 0 || len(arts) == 0 {
		t.Fatalf("expected Scout to capture JS error artifact on HTTP 200 page, got stats=%+v", stats)
	}
	if arts[0].CaptureMode != ModeCDPScout || arts[0].ConsoleErrors == 0 || arts[0].Screenshot == nil {
		t.Errorf("unexpected Scout artifact: mode=%q consoleErrors=%d hasScreenshot=%v",
			arts[0].CaptureMode, arts[0].ConsoleErrors, arts[0].Screenshot != nil)
	}
	liveMu.Lock()
	gotVisits := len(liveScoutVisits)
	liveMu.Unlock()
	if gotVisits != 1 {
		t.Errorf("expected 1 ScoutVisit event in live test, got %d", gotVisits)
	}

	// 3. Real-browser Scout on JSON 200 OK journey step passes content_type without false failure,
	// and off-origin 302 redirect skips screenshot, cookies, and DOM capture.
	store2 := NewStore(10, 3)
	bus2 := events.NewBus()
	defer bus2.Close()
	var visits2 []events.Event
	var mu2 sync.Mutex
	bus2.Subscribe(events.Filter(func(e events.Event) {
		mu2.Lock()
		visits2 = append(visits2, e)
		mu2.Unlock()
	}, events.ScoutVisit))

	j2, err := journey.Parse(bytes.NewReader([]byte(`{
		"name": "live-json-and-escape",
		"steps": [
			{"name": "health-json", "path": "/api/health", "expect": {"status": [200], "content_type": "application/json", "contains": ["\"ok\":true"]}},
			{"name": "off-origin", "path": "/escape"}
		]
	}`)), srv.URL+"/")
	if err != nil {
		t.Fatalf("journey.Parse j2: %v", err)
	}
	RunScouts(ctx, ScoutConfig{
		Count:          1,
		Origin:         srv.URL,
		Hit:            srv.URL + "/",
		Journey:        j2,
		Until:          10 * time.Second,
		ThinkMin:       10 * time.Millisecond,
		ThinkMax:       20 * time.Millisecond,
		NextNumber:     func() int64 { return 100 },
		FingerprintKey: fpKey,
	}, cdp, store2, bus2)

	mu2.Lock()
	gotVisits2 := append([]events.Event(nil), visits2...)
	mu2.Unlock()
	if len(gotVisits2) != 2 {
		t.Fatalf("expected 2 ScoutVisit events in live JSON+escape journey, got %d", len(gotVisits2))
	}
	if gotVisits2[0].StatusCode != 200 || gotVisits2[0].Failed || gotVisits2[0].Err != nil {
		t.Errorf("live JSON 200 step falsely failed: %+v (err=%v)", gotVisits2[0], gotVisits2[0].Err)
	}
	if !gotVisits2[1].Failed || gotVisits2[1].Err == nil {
		t.Errorf("live off-origin redirect step should have failed: %+v", gotVisits2[1])
	}
	arts2, _ := store2.SnapshotForReport()
	if len(arts2) != 1 {
		t.Fatalf("expected exactly 1 warning artifact (only off-origin, none for JSON 200), got %d (%+v)", len(arts2), arts2)
	}
	if arts2[0].TriggerReason != "off-origin-redirect" || arts2[0].Screenshot != nil || len(arts2[0].Session.CookieJar.Cookies) != 0 {
		t.Errorf("live off-origin redirect leaked data: trigger=%q hasShot=%v cookies=%d",
			arts2[0].TriggerReason, arts2[0].Screenshot != nil, len(arts2[0].Session.CookieJar.Cookies))
	}
	if strings.Contains(arts2[0].ErrorBody.BodyExcerpt, "THIRD_PARTY_SECRET_DOM") {
		t.Error("live off-origin redirect leaked third-party DOM into ErrorBody")
	}
}

type blockingCapturer struct {
	delay     time.Duration
	started   atomic.Int64
	completed atomic.Int64
	cancelled atomic.Int64
	closed    atomic.Bool
	firstHit  chan struct{}
	once      sync.Once
}

func (b *blockingCapturer) Mode() string        { return ModeCDPReplay }
func (b *blockingCapturer) BrowserInfo() string { return "blocking-mock" }
func (b *blockingCapturer) Close() error {
	b.closed.Store(true)
	return nil
}

func (b *blockingCapturer) Capture(ctx context.Context, req CaptureRequest) (Artifact, error) {
	b.started.Add(1)
	b.once.Do(func() {
		if b.firstHit != nil {
			close(b.firstHit)
		}
	})
	timer := time.NewTimer(b.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		b.cancelled.Add(1)
		return Artifact{}, ctx.Err()
	case <-timer.C:
		b.completed.Add(1)
		return Artifact{
			ID:            newArtifactID(),
			Fingerprint:   Fingerprint(req.Path, req.StatusCode, req.TriggerReason),
			CapturedAt:    time.Now(),
			URL:           req.SafeURL,
			Path:          req.Path,
			TriggerReason: req.TriggerReason,
			CaptureMode:   ModeCDPReplay,
			StatusCode:    req.StatusCode,
			Session:       req.Session,
			ReviewStatus:  ReviewUnreviewed,
		}, nil
	}
}

// TestCoordinator_CtrlCDrainCompletesQueuedCaptures verifies that cancelling the
// parent context (SIGINT/SIGTERM) does not abort in-flight or queued captures
// before DrainAndClose drains them, and that DrainAndClose timeout cancels
// stragglers cleanly without leaking goroutines.
func TestCoordinator_CtrlCDrainCompletesQueuedCaptures(t *testing.T) {
	t.Run("parent-cancel-still-drains-all-queued", func(t *testing.T) {
		bus := events.NewBus()
		defer bus.Close()

		var captured atomic.Int64
		bus.Subscribe(events.Filter(func(events.Event) {
			captured.Add(1)
		}, events.ArtifactCaptured))

		parentCtx, parentCancel := context.WithCancel(context.Background())
		store := NewStore(10, 5)
		capImpl := &blockingCapturer{
			delay:    40 * time.Millisecond,
			firstHit: make(chan struct{}),
		}
		coord := NewCoordinator(parentCtx, store, capImpl, bus, CoordinatorConfig{Workers: 1, Queue: 8})

		for i := range 4 {
			if ok := coord.Submit(CaptureRequest{
				URL:           fmt.Sprintf("http://127.0.0.1/ctrlc/%d", i),
				SafeURL:       fmt.Sprintf("http://127.0.0.1/ctrlc/%d", i),
				Path:          fmt.Sprintf("/ctrlc/%d", i),
				StatusCode:    500,
				TriggerReason: "status 500",
				Session:       SessionSnapshot{LemmingNumber: int64(i + 1), LemmingID: fmt.Sprintf("l-%d", i)},
			}); !ok {
				t.Fatalf("submit %d failed", i)
			}
		}

		// Wait until worker is actively inside capture #1, then simulate Ctrl-C.
		<-capImpl.firstHit
		parentCancel()

		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer drainCancel()
		if err := coord.DrainAndClose(drainCtx); err != nil {
			t.Fatalf("DrainAndClose: %v", err)
		}

		if got := capImpl.completed.Load(); got != 4 {
			t.Fatalf("expected all 4 queued captures to finish after parent cancel, got completed=%d cancelled=%d",
				got, capImpl.cancelled.Load())
		}
		if capImpl.cancelled.Load() != 0 {
			t.Errorf("expected 0 cancelled captures, got %d", capImpl.cancelled.Load())
		}
		if !capImpl.closed.Load() {
			t.Error("expected capturer.Close() to be called")
		}
		if got := store.Stats().Captured; got != 4 {
			t.Errorf("expected 4 stored artifacts, got %d", got)
		}
		if got := captured.Load(); got != 4 {
			t.Errorf("expected 4 ArtifactCaptured events, got %d", got)
		}
		if coord.workCtx.Err() == nil {
			t.Error("expected coordinator workCtx to be cancelled after DrainAndClose returns")
		}
	})

	t.Run("drain-timeout-cancels-straggler-without-leak", func(t *testing.T) {
		parentCtx, parentCancel := context.WithCancel(context.Background())
		defer parentCancel()

		store := NewStore(10, 5)
		capImpl := &blockingCapturer{
			delay:    5 * time.Second,
			firstHit: make(chan struct{}),
		}
		coord := NewCoordinator(parentCtx, store, capImpl, nil, CoordinatorConfig{Workers: 1, Queue: 4})

		for i := range 3 {
			coord.Submit(CaptureRequest{
				URL:           fmt.Sprintf("http://127.0.0.1/slow/%d", i),
				SafeURL:       fmt.Sprintf("http://127.0.0.1/slow/%d", i),
				Path:          fmt.Sprintf("/slow/%d", i),
				StatusCode:    503,
				TriggerReason: "status 503",
			})
		}

		<-capImpl.firstHit
		parentCancel()

		drainCtx, drainCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer drainCancel()
		start := time.Now()
		if err := coord.DrainAndClose(drainCtx); err != nil {
			t.Fatalf("DrainAndClose: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("DrainAndClose did not cancel straggler promptly: took %v", elapsed)
		}
		if capImpl.cancelled.Load() != 1 {
			t.Errorf("expected exactly 1 in-flight capture cancelled (remaining skipped), got started=%d cancelled=%d",
				capImpl.started.Load(), capImpl.cancelled.Load())
		}
		if !capImpl.closed.Load() {
			t.Error("expected capturer.Close() to be called on drain timeout")
		}
		if coord.workCtx.Err() == nil {
			t.Error("expected workCtx to be cancelled after DrainAndClose timeout")
		}
	})
}

func TestCDP_RemoteURLPolicyAndNetworkGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("reject-non-loopback-when-allow-remote-false", func(t *testing.T) {
		cases := []string{
			"http://10.0.0.5:9222",
			"https://remote-cdp.example.com:9222",
			"ws://192.168.1.50:9222/devtools/browser/abc",
			"wss://remote-cdp.example.com:9222/devtools/browser/abc",
		}
		for _, raw := range cases {
			_, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: raw, AllowRemoteCDP: false})
			if err == nil {
				t.Errorf("expected error for remote CDPURL %q with AllowRemoteCDP=false", raw)
				continue
			}
			if !strings.Contains(err.Error(), "-allow-remote-cdp") {
				t.Errorf("expected error for %q to mention -allow-remote-cdp, got: %v", raw, err)
			}
		}
	})

	t.Run("reject-remote-webSocketDebuggerUrl-when-allow-remote-false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/json/version" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://10.0.0.5:9222/devtools/browser/evil"}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		_, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, AllowRemoteCDP: false})
		if err == nil {
			t.Fatal("expected remote webSocketDebuggerUrl to be rejected when AllowRemoteCDP=false")
		}
		if !strings.Contains(err.Error(), "-allow-remote-cdp") {
			t.Errorf("expected error mentioning -allow-remote-cdp, got: %v", err)
		}
	})

	t.Run("reject-remote-redirect-when-allow-remote-false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://10.0.0.5:9222/json/version", http.StatusFound)
		}))
		defer srv.Close()

		_, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, AllowRemoteCDP: false})
		if err == nil {
			t.Fatal("expected remote redirect to be rejected when AllowRemoteCDP=false")
		}
		if !strings.Contains(err.Error(), "-allow-remote-cdp") {
			t.Errorf("expected error mentioning -allow-remote-cdp, got: %v", err)
		}
	})

	t.Run("reject-link-local-and-metadata-even-when-allow-remote-true", func(t *testing.T) {
		cases := []string{
			"http://169.254.169.254:9222",
			"http://[::ffff:169.254.169.254]:9222",
			"http://[fd00:ec2::254]:9222",
			"http://[fe80::1]:9222",
			"http://0.0.0.0:9222",
			"ws://169.254.169.254:9222/devtools/browser/1",
			"ws://[::ffff:169.254.169.254]:9222/devtools/browser/1",
			"ws://[fd00:ec2::254]:9222/devtools/browser/1",
			"wss://[fd00:ec2::254]:9222/devtools/browser/1",
		}
		for _, raw := range cases {
			_, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: raw, AllowRemoteCDP: true})
			if err == nil {
				t.Errorf("expected network guard error for %q even with AllowRemoteCDP=true", raw)
				continue
			}
			if !errors.Is(err, discovery.ErrBlockedAddress) {
				t.Errorf("expected ErrBlockedAddress for %q, got: %v", raw, err)
			}
		}
	})

	t.Run("reject-metadata-webSocketDebuggerUrl-even-when-allow-remote-true", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://169.254.169.254:9222/devtools/browser/imds"}`)
		}))
		defer srv.Close()

		_, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, AllowRemoteCDP: true})
		if err == nil {
			t.Fatal("expected metadata webSocketDebuggerUrl to be rejected by dialCDP")
		}
		if !errors.Is(err, discovery.ErrBlockedAddress) {
			t.Errorf("expected ErrBlockedAddress, got: %v", err)
		}
	})
}

func TestInjectReplayCookies_DomainValidation(t *testing.T) {
	cases := []struct {
		name       string
		cookieDom  string
		targetHost string
		want       bool
	}{
		{"empty-host-only", "", "app.example.com", true},
		{"exact-host", "app.example.com", "app.example.com", true},
		{"exact-host-leading-dot", ".app.example.com", "app.example.com", true},
		{"exact-host-case-insensitive", "APP.Example.COM", "app.example.com", true},
		{"parent-domain", "example.com", "app.example.com", true},
		{"parent-domain-leading-dot", ".example.com", "app.example.com", true},
		{"deep-parent-domain", ".b.example.com", "a.b.example.com", true},
		{"ip-exact-match", "127.0.0.1", "127.0.0.1", true},
		{"ipv6-exact-match", "::1", "::1", true},
		{"localhost-exact", "localhost", "localhost", true},
		{"cross-domain", "evil.com", "app.example.com", false},
		{"cross-domain-dot", ".evil.com", "app.example.com", false},
		{"non-boundary-suffix", "example.com", "notexample.com", false},
		{"child-subdomain-rejected", "sub.app.example.com", "app.example.com", false},
		{"ip-suffix-trick-1", "1.1", "192.168.1.1", false},
		{"ip-suffix-trick-2", ".168.1.1", "192.168.1.1", false},
		{"ip-suffix-trick-loopback", "0.0.1", "127.0.0.1", false},
		{"tld-supercookie", "com", "app.example.com", false},
		{"tld-supercookie-dot", ".com", "app.example.com", false},
		{"public-suffix-co-uk", "co.uk", "app.example.co.uk", false},
		{"double-leading-dot", "..example.com", "app.example.com", false},
		{"trailing-dot", "example.com.", "app.example.com", false},
		{"only-dot", ".", "app.example.com", false},
		{"host-with-port", "example.com:443", "example.com", false},
		{"ip-with-port", "127.0.0.1:8080", "127.0.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cookieDomainAllowed(tc.cookieDom, tc.targetHost); got != tc.want {
				t.Errorf("cookieDomainAllowed(%q, %q) = %v, want %v", tc.cookieDom, tc.targetHost, got, tc.want)
			}
		})
	}

	// Verify end-to-end Network.setCookies payload via fake CDP server.
	var mu sync.Mutex
	var sentCookies []map[string]any
	srv := newFakeCDPServer(t, func(send func(any), req cdpServerReq) {
		switch req.Method {
		case "Network.setCookies":
			var p struct {
				Cookies []map[string]any `json:"cookies"`
			}
			_ = json.Unmarshal(req.Params, &p)
			mu.Lock()
			sentCookies = append(sentCookies, p.Cookies...)
			mu.Unlock()
			send(map[string]any{"id": req.ID, "result": map[string]any{}})
		default:
			send(map[string]any{"id": req.ID, "result": map[string]any{}})
		}
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cdp, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewCDPCapturer: %v", err)
	}
	defer func() { _ = cdp.Close() }()

	injectReplayCookies(ctx, cdp.browser.conn, "S1", "https://app.example.com/checkout", []ReplayCookie{
		{Name: "host_only", Value: "v1", Domain: ""},
		{Name: "exact", Value: "v2", Domain: "app.example.com"},
		{Name: "parent", Value: "v3", Domain: ".example.com"},
		{Name: "evil", Value: "v4", Domain: "evil.com"},
		{Name: "suffix_trick", Value: "v5", Domain: "ple.com"},
		{Name: "child", Value: "v6", Domain: "sub.app.example.com"},
		{Name: "tld", Value: "v7", Domain: ".com"},
	})

	mu.Lock()
	gotNames := make([]string, 0, len(sentCookies))
	for _, ck := range sentCookies {
		if n, ok := ck["name"].(string); ok {
			gotNames = append(gotNames, n)
		}
	}
	mu.Unlock()

	wantNames := []string{"host_only", "exact", "parent"}
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("Network.setCookies sent %v, want %v", gotNames, wantNames)
	}
}

type cdpServerReq struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

// newFakeCDPServer creates a loopback HTTP+WebSocket fake CDP server exposing
// /json/version and /devtools/browser/test.
func newFakeCDPServer(t *testing.T, onCall func(send func(any), req cdpServerReq)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/devtools/browser/test"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": wsURL})
	})
	mux.Handle("/devtools/browser/test", websocket.Handler(func(ws *websocket.Conn) {
		var sendMu sync.Mutex
		send := func(msg any) {
			raw, err := json.Marshal(msg)
			if err != nil {
				return
			}
			sendMu.Lock()
			_ = websocket.Message.Send(ws, string(raw))
			sendMu.Unlock()
		}
		for {
			var raw []byte
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			var req cdpServerReq
			if err := json.Unmarshal(raw, &req); err != nil {
				continue
			}
			onCall(send, req)
		}
	}))
	srv = httptest.NewServer(mux)
	return srv
}

func fakeCDPDefaultLifecycle(send func(any), req cdpServerReq) bool {
	switch req.Method {
	case "Target.createBrowserContext":
		send(map[string]any{"id": req.ID, "result": map[string]any{"browserContextId": "BC1"}})
		return true
	case "Target.createTarget":
		send(map[string]any{"id": req.ID, "result": map[string]any{"targetId": "T1"}})
		return true
	case "Target.attachToTarget":
		send(map[string]any{"id": req.ID, "result": map[string]any{"sessionId": "S1"}})
		return true
	case "Target.disposeBrowserContext", "Target.closeTarget", "Browser.close",
		"Page.enable", "Runtime.enable", "Log.enable", "Network.enable",
		"Emulation.setDeviceMetricsOverride", "Network.setUserAgentOverride", "Network.setExtraHTTPHeaders":
		send(map[string]any{"id": req.ID, "result": map[string]any{}})
		return true
	}
	return false
}

// TestCDP_ScreenshotGraceBudgetWhenLoadEventDelayed verifies that when a page
// never fires Page.loadEventFired (or fires it past the event budget), Capture
// reserves the grace window and still captures a valid screenshot cleanly.
func TestCDP_ScreenshotGraceBudgetWhenLoadEventDelayed(t *testing.T) {
	validJPEG := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xff, 0xd9}
	encodedJPEG := base64.StdEncoding.EncodeToString(validJPEG)

	var screenshotCalled atomic.Bool
	srv := newFakeCDPServer(t, func(send func(any), req cdpServerReq) {
		if fakeCDPDefaultLifecycle(send, req) {
			return
		}
		switch req.Method {
		case "Page.navigate":
			send(map[string]any{"id": req.ID, "result": map[string]any{"frameId": "F1"}})
			// Intentionally emit a console error but NEVER emit Page.loadEventFired
			// before the capture timeout expires.
			send(map[string]any{
				"sessionId": req.SessionID,
				"method":    "Runtime.consoleAPICalled",
				"params": map[string]any{
					"type": "error",
					"args": []map[string]any{{"type": "string", "value": "slow page error"}},
				},
			})
		case "Runtime.evaluate":
			var p struct {
				Expression string `json:"expression"`
			}
			_ = json.Unmarshal(req.Params, &p)
			val := "<html><body><h1>Still Loading</h1></body></html>"
			if strings.Contains(p.Expression, "location.href") {
				val = "http://127.0.0.1:8080/hanging"
			}
			send(map[string]any{
				"id":     req.ID,
				"result": map[string]any{"result": map[string]any{"value": val}},
			})
		case "Page.captureScreenshot":
			screenshotCalled.Store(true)
			send(map[string]any{
				"id":     req.ID,
				"result": map[string]any{"data": encodedJPEG},
			})
		default:
			send(map[string]any{"id": req.ID, "result": map[string]any{}})
		}
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cdp, err := NewCDPCapturer(ctx, CDPOptions{
		CDPURL:  srv.URL,
		Format:  "jpeg",
		Timeout: 600 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCDPCapturer: %v", err)
	}
	defer func() { _ = cdp.Close() }()

	art, err := cdp.Capture(ctx, CaptureRequest{
		URL:           "http://127.0.0.1:8080/hanging",
		SafeURL:       "http://127.0.0.1:8080/hanging",
		Origin:        "http://127.0.0.1:8080",
		Path:          "/hanging",
		StatusCode:    500,
		TriggerReason: "status 500",
	})
	if err != nil {
		t.Fatalf("cdp.Capture failed: %v", err)
	}
	if !screenshotCalled.Load() {
		t.Fatal("expected Page.captureScreenshot to be invoked within grace budget")
	}
	if art.Screenshot == nil || !bytes.Equal(art.Screenshot.Data, validJPEG) {
		t.Fatalf("expected valid screenshot bytes, got %+v (warning=%q)", art.Screenshot, art.CaptureWarning)
	}
	if art.CaptureWarning != "" {
		t.Errorf("expected no CaptureWarning when screenshot succeeds in grace budget, got %q", art.CaptureWarning)
	}
	if art.ConsoleErrors != 1 {
		t.Errorf("expected 1 console error preserved during event wait, got %d", art.ConsoleErrors)
	}
}

// TestScout_OffOriginRedirectBlockedAndEmitsScoutVisit verifies that when a
// Scout navigation redirects off-origin (either HTTP 302 or client-side JS),
// DOM extraction, cookie capture, and screenshot capture are ALL skipped,
// a warning is recorded, and events.ScoutVisit is emitted with Failed=true.
func TestScout_OffOriginRedirectBlockedAndEmitsScoutVisit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		clientSideJS bool
	}{
		{"http-redirect-frameNavigated", false},
		{"client-side-js-location-redirect", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				domEvaluated     atomic.Bool
				cookiesFetched   atomic.Bool
				screenshotCalled atomic.Bool
			)
			srv := newFakeCDPServer(t, func(send func(any), req cdpServerReq) {
				if fakeCDPDefaultLifecycle(send, req) {
					return
				}
				switch req.Method {
				case "Page.navigate":
					send(map[string]any{"id": req.ID, "result": map[string]any{"frameId": "F1"}})
					respURL := "http://evil.example.org/secret-portal"
					frameURL := "http://evil.example.org/secret-portal"
					if tc.clientSideJS {
						respURL = "http://target.local/redir"
						frameURL = "http://target.local/redir"
					}
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Network.responseReceived",
						"params": map[string]any{
							"type":    "Document",
							"frameId": "F1",
							"response": map[string]any{
								"url":      respURL,
								"status":   200,
								"mimeType": "text/html",
								"headers":  map[string]any{"Content-Type": "text/html"},
							},
						},
					})
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Page.frameNavigated",
						"params": map[string]any{
							"frame": map[string]any{"id": "F1", "url": frameURL},
						},
					})
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Page.loadEventFired",
						"params":    map[string]any{},
					})
				case "Runtime.evaluate":
					var p struct {
						Expression string `json:"expression"`
					}
					_ = json.Unmarshal(req.Params, &p)
					if strings.Contains(p.Expression, "location.href") {
						send(map[string]any{
							"id":     req.ID,
							"result": map[string]any{"result": map[string]any{"value": "http://evil.example.org/secret-portal"}},
						})
						return
					}
					domEvaluated.Store(true)
					send(map[string]any{
						"id":     req.ID,
						"result": map[string]any{"result": map[string]any{"value": "<html><body>OFF_ORIGIN_SECRET_DOM</body></html>"}},
					})
				case "Network.getCookies":
					cookiesFetched.Store(true)
					send(map[string]any{"id": req.ID, "result": map[string]any{"cookies": []any{}}})
				case "Page.captureScreenshot":
					screenshotCalled.Store(true)
					send(map[string]any{"id": req.ID, "result": map[string]any{"data": ""}})
				default:
					send(map[string]any{"id": req.ID, "result": map[string]any{}})
				}
			})
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cdp, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, Timeout: 2 * time.Second})
			if err != nil {
				t.Fatalf("NewCDPCapturer: %v", err)
			}
			defer func() { _ = cdp.Close() }()

			store := NewStore(10, 3)
			bus := events.NewBus()
			defer bus.Close()

			var mu sync.Mutex
			var visits []events.Event
			bus.Subscribe(events.Filter(func(e events.Event) {
				mu.Lock()
				visits = append(visits, e)
				mu.Unlock()
			}, events.ScoutVisit))

			j, err := journey.Parse(bytes.NewReader([]byte(`{"name":"off-origin","steps":[{"name":"redir","path":"/redir"}]}`)), "http://target.local/")
			if err != nil {
				t.Fatalf("journey.Parse: %v", err)
			}

			RunScouts(ctx, ScoutConfig{
				Count:      1,
				Origin:     "http://target.local",
				Hit:        "http://target.local/",
				Journey:    j,
				Until:      4 * time.Second,
				ThinkMin:   5 * time.Millisecond,
				ThinkMax:   10 * time.Millisecond,
				NextNumber: func() int64 { return 42 },
			}, cdp, store, bus)

			if domEvaluated.Load() {
				t.Error("SECURITY VIOLATION: Scout extracted DOM after off-origin redirect")
			}
			if cookiesFetched.Load() {
				t.Error("SECURITY VIOLATION: Scout fetched cookies after off-origin redirect")
			}
			if screenshotCalled.Load() {
				t.Error("SECURITY VIOLATION: Scout captured screenshot after off-origin redirect")
			}

			mu.Lock()
			gotVisits := append([]events.Event(nil), visits...)
			mu.Unlock()
			if len(gotVisits) != 1 {
				t.Fatalf("expected 1 ScoutVisit event, got %d", len(gotVisits))
			}
			v := gotVisits[0]
			if !v.Failed || v.Err == nil || v.LemmingNumber != 42 || v.URL != "http://target.local/redir" {
				t.Errorf("unexpected ScoutVisit event: %+v (err=%v)", v, v.Err)
			}

			arts, _ := store.SnapshotForReport()
			if len(arts) != 1 {
				t.Fatalf("expected 1 warning-only artifact, got %d", len(arts))
			}
			art := arts[0]
			if art.Screenshot != nil {
				t.Error("expected nil Screenshot on off-origin warning artifact")
			}
			if len(art.Session.CookieJar.Cookies) != 0 {
				t.Error("expected zero cookies on off-origin warning artifact")
			}
			if strings.Contains(art.ErrorBody.BodyExcerpt, "OFF_ORIGIN_SECRET_DOM") {
				t.Error("off-origin DOM leaked into artifact")
			}
			if art.CaptureWarning == "" || art.TriggerReason != "off-origin-redirect" {
				t.Errorf("expected off-origin warning & trigger, got warning=%q trigger=%q", art.CaptureWarning, art.TriggerReason)
			}
		})
	}
}

// TestScout_JSONJourneyContentTypeNavErrorStaleDrainAndPoolUntil tests:
// 1. JSON 200 OK journey step passing content_type: "application/json" without false failure.
// 2. Stale CDP events from page 1 drained before page 2 so page 2 is not contaminated.
// 3. Page.navigate errorText keeps docStatus=0 and triggers "nav-error".
// 4. URL pool mode cycles through cfg.URLs until -until expires and emits ScoutVisit per page.
func TestScout_JSONJourneyContentTypeNavErrorStaleDrainAndPoolUntil(t *testing.T) {
	t.Run("json-200-passes-and-stale-events-drained", func(t *testing.T) {
		var navCount atomic.Int32
		srv := newFakeCDPServer(t, func(send func(any), req cdpServerReq) {
			if fakeCDPDefaultLifecycle(send, req) {
				return
			}
			switch req.Method {
			case "Page.navigate":
				var p struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(req.Params, &p)
				n := navCount.Add(1)
				send(map[string]any{"id": req.ID, "result": map[string]any{"frameId": "F1"}})
				switch n {
				case 1:
					// Step 1: JSON 200 OK via headers["content-type"].
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Network.responseReceived",
						"params": map[string]any{
							"type":    "Document",
							"frameId": "F1",
							"response": map[string]any{
								"url":      p.URL,
								"status":   200,
								"mimeType": "text/plain",
								"headers":  map[string]any{"Content-Type": "application/json; charset=utf-8"},
							},
						},
					})
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Page.frameNavigated",
						"params":    map[string]any{"frame": map[string]any{"id": "F1", "url": p.URL}},
					})
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Page.loadEventFired",
						"params":    map[string]any{},
					})
				case 2:
					// Step 2: JSON 200 OK via mimeType fallback (no Content-Type header).
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Network.responseReceived",
						"params": map[string]any{
							"type":    "Document",
							"frameId": "F1",
							"response": map[string]any{
								"url":      p.URL,
								"status":   200,
								"mimeType": "application/json",
								"headers":  map[string]any{},
							},
						},
					})
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Page.frameNavigated",
						"params":    map[string]any{"frame": map[string]any{"id": "F1", "url": p.URL}},
					})
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Page.loadEventFired",
						"params":    map[string]any{},
					})
				}
			case "Runtime.evaluate":
				var p struct {
					Expression string `json:"expression"`
				}
				_ = json.Unmarshal(req.Params, &p)
				if strings.Contains(p.Expression, "location.href") {
					if navCount.Load() == 1 {
						send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "http://api.local/v1/health"}}})
					} else {
						send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "http://api.local/v1/status"}}})
					}
					return
				}
				send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": `{"ok":true,"service":"concert"}`}}})
				if navCount.Load() == 1 {
					// Emit a late console.error AFTER step 1 body eval (during think time)
					// to verify step 2 drains evCh before Page.navigate.
					send(map[string]any{
						"sessionId": req.SessionID,
						"method":    "Runtime.consoleAPICalled",
						"params": map[string]any{
							"type": "error",
							"args": []map[string]any{{"type": "string", "value": "stale-late-error-from-step-1"}},
						},
					})
				}
			default:
				send(map[string]any{"id": req.ID, "result": map[string]any{}})
			}
		})
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cdp, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewCDPCapturer: %v", err)
		}
		defer func() { _ = cdp.Close() }()

		store := NewStore(10, 3)
		bus := events.NewBus()
		defer bus.Close()

		var mu sync.Mutex
		var visits []events.Event
		bus.Subscribe(events.Filter(func(e events.Event) {
			mu.Lock()
			visits = append(visits, e)
			mu.Unlock()
		}, events.ScoutVisit))

		j, err := journey.Parse(bytes.NewReader([]byte(`{
			"name": "json-api",
			"steps": [
				{"name": "health", "path": "/v1/health", "expect": {"status": [200], "content_type": "application/json", "contains": ["\"ok\":true"]}},
				{"name": "status", "path": "/v1/status", "expect": {"status": [200], "content_type": "application/json", "contains": ["concert"]}}
			]
		}`)), "http://api.local/")
		if err != nil {
			t.Fatalf("journey.Parse: %v", err)
		}

		RunScouts(ctx, ScoutConfig{
			Count:      1,
			Origin:     "http://api.local",
			Hit:        "http://api.local/",
			Journey:    j,
			Until:      4 * time.Second,
			ThinkMin:   30 * time.Millisecond,
			ThinkMax:   40 * time.Millisecond,
			NextNumber: func() int64 { return 7 },
		}, cdp, store, bus)

		if stats := store.Stats(); stats.Captured != 0 {
			arts, _ := store.SnapshotForReport()
			t.Fatalf("expected 0 failure artifacts for valid 200 JSON journey, got %d (%+v)", stats.Captured, arts)
		}
		mu.Lock()
		gotVisits := append([]events.Event(nil), visits...)
		mu.Unlock()
		if len(gotVisits) != 2 {
			t.Fatalf("expected 2 ScoutVisit events, got %d", len(gotVisits))
		}
		for i, v := range gotVisits {
			if v.StatusCode != 200 || v.Failed || v.Err != nil {
				t.Errorf("visit[%d] unexpected: status=%d failed=%v err=%v", i, v.StatusCode, v.Failed, v.Err)
			}
		}
	})

	t.Run("navigate-errorText-sets-status-0-and-nav-error", func(t *testing.T) {
		srv := newFakeCDPServer(t, func(send func(any), req cdpServerReq) {
			if fakeCDPDefaultLifecycle(send, req) {
				return
			}
			switch req.Method {
			case "Page.navigate":
				send(map[string]any{"id": req.ID, "result": map[string]any{"frameId": "F1", "errorText": "net::ERR_CONNECTION_REFUSED"}})
				send(map[string]any{
					"sessionId": req.SessionID,
					"method":    "Page.frameNavigated",
					"params":    map[string]any{"frame": map[string]any{"id": "F1", "url": "chrome-error://chromewebdata/"}},
				})
				send(map[string]any{
					"sessionId": req.SessionID,
					"method":    "Page.loadEventFired",
					"params":    map[string]any{},
				})
			case "Runtime.evaluate":
				var p struct {
					Expression string `json:"expression"`
				}
				_ = json.Unmarshal(req.Params, &p)
				if strings.Contains(p.Expression, "location.href") {
					send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "chrome-error://chromewebdata/"}}})
					return
				}
				send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "<html><body>Connection refused</body></html>"}}})
			case "Page.captureScreenshot":
				send(map[string]any{"id": req.ID, "result": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xd9})}})
			default:
				send(map[string]any{"id": req.ID, "result": map[string]any{}})
			}
		})
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cdp, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewCDPCapturer: %v", err)
		}
		defer func() { _ = cdp.Close() }()

		store := NewStore(10, 3)
		bus := events.NewBus()
		defer bus.Close()
		var mu sync.Mutex
		var visits []events.Event
		bus.Subscribe(events.Filter(func(e events.Event) {
			mu.Lock()
			visits = append(visits, e)
			mu.Unlock()
		}, events.ScoutVisit))

		j, _ := journey.Parse(bytes.NewReader([]byte(`{"name":"nav-err","steps":[{"name":"down","path":"/down"}]}`)), "http://down.local/")
		RunScouts(ctx, ScoutConfig{
			Count:   1,
			Origin:  "http://down.local",
			Hit:     "http://down.local/",
			Journey: j,
			Until:   3 * time.Second,
		}, cdp, store, bus)

		arts, _ := store.SnapshotForReport()
		if len(arts) != 1 {
			t.Fatalf("expected 1 nav-error artifact, got %d", len(arts))
		}
		if arts[0].StatusCode != 0 || arts[0].TriggerReason != "nav-error" {
			t.Errorf("expected StatusCode=0 TriggerReason=nav-error, got status=%d trigger=%q", arts[0].StatusCode, arts[0].TriggerReason)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(visits) != 1 || visits[0].StatusCode != 0 || !visits[0].Failed || visits[0].Err == nil {
			t.Errorf("unexpected nav-error ScoutVisit: %+v", visits)
		}
	})

	t.Run("url-pool-cycles-until-deadline", func(t *testing.T) {
		srv := newFakeCDPServer(t, func(send func(any), req cdpServerReq) {
			if fakeCDPDefaultLifecycle(send, req) {
				return
			}
			switch req.Method {
			case "Page.navigate":
				var p struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(req.Params, &p)
				send(map[string]any{"id": req.ID, "result": map[string]any{"frameId": "F1"}})
				send(map[string]any{
					"sessionId": req.SessionID,
					"method":    "Network.responseReceived",
					"params": map[string]any{
						"type":    "Document",
						"frameId": "F1",
						"response": map[string]any{
							"url":      p.URL,
							"status":   200,
							"mimeType": "text/html",
							"headers":  map[string]any{"Content-Type": "text/html"},
						},
					},
				})
				send(map[string]any{
					"sessionId": req.SessionID,
					"method":    "Page.frameNavigated",
					"params":    map[string]any{"frame": map[string]any{"id": "F1", "url": p.URL}},
				})
				send(map[string]any{
					"sessionId": req.SessionID,
					"method":    "Page.loadEventFired",
					"params":    map[string]any{},
				})
			case "Runtime.evaluate":
				var p struct {
					Expression string `json:"expression"`
				}
				_ = json.Unmarshal(req.Params, &p)
				if strings.Contains(p.Expression, "location.href") {
					send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "http://pool.local/a"}}})
					return
				}
				send(map[string]any{"id": req.ID, "result": map[string]any{"result": map[string]any{"value": "<html><body>Hello Pool</body></html>"}}})
			default:
				send(map[string]any{"id": req.ID, "result": map[string]any{}})
			}
		})
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cdp, err := NewCDPCapturer(ctx, CDPOptions{CDPURL: srv.URL, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("NewCDPCapturer: %v", err)
		}
		defer func() { _ = cdp.Close() }()

		bus := events.NewBus()
		defer bus.Close()
		var visitCount atomic.Int64
		bus.Subscribe(events.Filter(func(events.Event) {
			visitCount.Add(1)
		}, events.ScoutVisit))

		// Each visit settles for 600ms + 10ms think; with Until=1600ms and 1 URL
		// in pool, Scout must cycle and visit more than once until -until expires.
		RunScouts(ctx, ScoutConfig{
			Count:    1,
			Origin:   "http://pool.local",
			Hit:      "http://pool.local/a",
			URLs:     []string{"http://pool.local/a"},
			Until:    1600 * time.Millisecond,
			ThinkMin: 10 * time.Millisecond,
			ThinkMax: 15 * time.Millisecond,
		}, cdp, NewStore(10, 3), bus)

		if got := visitCount.Load(); got < 2 {
			t.Fatalf("expected URL pool Scout to cycle until -until (>=2 visits), got %d", got)
		}
	})
}
