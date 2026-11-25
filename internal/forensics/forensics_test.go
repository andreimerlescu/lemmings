package forensics

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
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
	mux := http.NewServeMux()
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
}
