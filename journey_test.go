package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// These tests cover a lemming's behaviour as a visitor: its persona and
// session, navigation, page checks, pacing and the evidence it leaves.
// Several began life as the experience tests in the pre-1.0 upgrade and
// were ported to the 1.0 API.

// TestLemming_SessionPersonaAndNavigation verifies that a lemming keeps one
// user agent and cookie jar for its whole life, follows a link from the
// page it just read, sends that page as its Referer, and never writes a
// cookie value into its evidence.
func TestLemming_SessionPersonaAndNavigation(t *testing.T) {
	var mu sync.Mutex
	var uas, refs, cookies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uas = append(uas, r.UserAgent())
		refs = append(refs, r.Referer())
		cookies = append(cookies, r.Header.Get("Cookie"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "private-session-value", Path: "/"})
		}
		fmt.Fprintf(w, `<!doctype html><html><head><title>Catalog</title></head><body><main id="app">Welcome</main><a href="/catalog/item">Item</a><input value="%d"></body></html>`, time.Now().UnixNano())
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Until = time.Second
	cfg.ThinkMin, cfg.ThinkMax = 5*time.Millisecond, 5*time.Millisecond
	cfg.MaxPages = 2
	cfg.Navigation = NavigationLinks
	pool := &URLPool{URLs: []string{cfg.Hit}, Checksums: map[string]string{cfg.Hit: "different"}}
	client := newTestClient()
	client.Jar, _ = cookiejar.New(nil)

	ll := NewLemming(0, 0, cfg, pool, client, NewEventBus(), testMetrics()).Run(context.Background())

	if ll.TotalVisits != 2 || ll.FailedVisits != 0 || ll.ExitReason != ExitPageLimit {
		t.Fatalf("unexpected session: visits=%d failed=%d exit=%s", ll.TotalVisits, ll.FailedVisits, ll.ExitReason)
	}
	if ll.Visits[1].URL != srv.URL+"/catalog/item" {
		t.Fatalf("lemming did not follow the link on the page: %s", ll.Visits[1].URL)
	}
	if uas[0] != uas[1] || uas[0] == "" {
		t.Fatal("a lemming must keep one user agent for its whole life")
	}
	if !strings.Contains(cookies[1], "private-session-value") {
		t.Fatal("session cookie was not sent back on the next page")
	}
	if refs[1] != cfg.Hit {
		t.Fatalf("expected Referer %q, got %q", cfg.Hit, refs[1])
	}
	if ll.Visits[0].Match {
		t.Fatal("a dynamic page should not match its index-time checksum")
	}
	if ll.Identity.Name == "" || ll.Identity.Language == "" {
		t.Fatal("a lemming should be born with a name and a language")
	}
	b, _ := json.Marshal(traceOf(&ll.Visits[1]))
	if strings.Contains(string(b), "private-session-value") {
		t.Fatal("cookie value leaked into the trace")
	}
	if !strings.Contains(string(b), `"session"`) {
		t.Fatal("cookie names should be recorded")
	}
}

// TestInspectPage_DetectsSoftError verifies that a 200 response carrying an
// error page fails its journey checks.
func TestInspectPage_DetectsSoftError(t *testing.T) {
	body := []byte(`<html><head><title>Oops</title></head><body><main class="error">Something went wrong</main></body></html>`)
	p, _ := inspectPage(body, "text/html; charset=utf-8", 200, Expectations{
		Status: []int{200}, Contains: []string{"Welcome"}, NotContains: []string{"Something went wrong"},
		TitleContains: "Catalog", Selectors: []string{"#app", "main", ".error"},
	}, "https://example.com/")

	if p.failedChecks() != 4 {
		t.Fatalf("expected 4 failed checks (contains, not-contains, title, #app), got %+v", p.Checks)
	}
	v := Visit{StatusCode: 200, Page: p}
	if !failedVisit(&v) && p.failedChecks() == 0 {
		t.Fatal("soft error passed")
	}
}

// TestInspectPage_NotBlank verifies the blank-page rule: text, media or
// script count as content; a page with nothing a person could see fails.
func TestInspectPage_NotBlank(t *testing.T) {
	cases := []struct {
		name string
		body string
		pass bool
	}{
		{"text", `<html><body><p>Hello</p></body></html>`, true},
		{"spa shell", `<html><body><div id="root"></div><script src="/app.js"></script></body></html>`, true},
		{"image only", `<html><body><img src="/hero.png" alt=""></body></html>`, true},
		{"title only", `<!doctype html><html><head><title>Blank</title></head><body></body></html>`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := inspectPage([]byte(tc.body), "text/html", 200, Expectations{}, "https://example.com/")
			for _, c := range p.Checks {
				if c.Name == "not-blank" && c.Passed != tc.pass {
					t.Fatalf("not-blank passed=%v, want %v", c.Passed, tc.pass)
				}
			}
		})
	}
}

// TestInspectPage_NavigableLinks verifies that lemmings never follow
// downloads, destructive links or links that leave the origin.
func TestInspectPage_NavigableLinks(t *testing.T) {
	body := `<html><body>
		<a href="/about">about</a>
		<a href="/about">duplicate</a>
		<a href="/guide.pdf">pdf</a>
		<a href="/export" download>download</a>
		<a href="/account/logout">log out</a>
		<a href="/items/7/delete">delete</a>
		<a href="https://elsewhere.example/">external</a>
		<a href="mailto:ops@example.com">mail</a>
		<a href="catalog/list">relative</a>
	</body></html>`
	_, links := inspectPage([]byte(body), "text/html", 200, Expectations{}, "https://example.com/shop/")
	want := []string{"https://example.com/about", "https://example.com/shop/catalog/list"}
	if strings.Join(links, " ") != strings.Join(want, " ") {
		t.Fatalf("links = %v, want %v", links, want)
	}
}

// TestFetch_RedirectsAndOriginScope verifies that a redirect chain is
// recorded hop by hop, that a redirect leaving the origin is refused
// without contacting the other host, and that origins compare by parsed
// scheme, host and effective port.
func TestFetch_RedirectsAndOriginScope(t *testing.T) {
	var external atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { external.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/done", http.StatusFound)
		case "/escape":
			http.Redirect(w, r, other.URL, http.StatusFound)
		default:
			w.Write([]byte("done"))
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Hit = srv.URL
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics())

	v, _ := l.hit(context.Background(), srv.URL, nil)
	if v.StatusCode != 200 || len(v.Hops) != 2 || v.Hops[0].Status != 302 || v.FinalURL != srv.URL+"/done" {
		t.Fatalf("redirect evidence: status=%d hops=%+v final=%s", v.StatusCode, v.Hops, v.FinalURL)
	}
	v, _ = l.hit(context.Background(), srv.URL+"/escape", nil)
	if v.Error == nil || v.ErrorKind != "offsite-redirect" || external.Load() != 0 {
		t.Fatalf("off-origin redirect was followed: kind=%q external=%d", v.ErrorKind, external.Load())
	}
	if sameOrigin("https://example.com", "https://example.com.evil.test") || sameOrigin("https://example.com", "http://example.com") {
		t.Fatal("different origins compared equal")
	}
	if !sameOrigin("https://EXAMPLE.com", "https://example.com:443/path") {
		t.Fatal("equivalent origins rejected")
	}
	if resolveURL(srv.URL+"/catalog/list", "item") != srv.URL+"/catalog/item" {
		t.Fatal("relative link resolved against the wrong directory")
	}
}

// TestFetch_LimitsAndCancellation verifies the body limit, the per-request
// timeout (a failure) and run cancellation (not a failure).
func TestFetch_LimitsAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(strings.Repeat("x", 1024)))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.MaxBodyBytes = 128
	cfg.RequestTimeout = 30 * time.Millisecond
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics())

	v, _ := l.hit(context.Background(), srv.URL, nil)
	if v.ErrorKind != "body-too-large" || v.BytesIn != 128 || !v.Failed {
		t.Fatalf("body limit: kind=%q bytes=%d failed=%v", v.ErrorKind, v.BytesIn, v.Failed)
	}
	v, _ = l.hit(context.Background(), srv.URL+"/slow", nil)
	if v.ErrorKind != "timeout" || v.Cancelled || !v.Failed {
		t.Fatalf("request timeout: kind=%q cancelled=%v failed=%v", v.ErrorKind, v.Cancelled, v.Failed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, _ = l.hit(ctx, srv.URL, nil)
	if !v.Cancelled || v.Failed {
		t.Fatalf("run cancellation must not count against the target: cancelled=%v failed=%v", v.Cancelled, v.Failed)
	}
}

// TestSwarm_StreamsVisitsAndOwnsLifecycleOnce verifies that visits are
// counted while lemmings are alive, that Born and Died fire exactly once
// per lemming, and that the report does not count streamed visits twice.
func TestSwarm_StreamsVisitsAndOwnsLifecycleOnce(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Terrain, cfg.Pack = 1, 2
	cfg.Until = time.Second
	cfg.MaxPages = 2
	cfg.ThinkMin, cfg.ThinkMax = 20*time.Millisecond, 20*time.Millisecond
	cfg.Navigation = NavigationRandom

	s, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var born, died, visits atomic.Int64
	s.events.Subscribe(func(e Event) {
		switch e.Kind {
		case EventLemmingBorn:
			born.Add(1)
		case EventLemmingDied:
			died.Add(1)
		case EventVisitComplete, EventVisitError:
			visits.Add(1)
			if s.metrics.TotalVisits.Load() == 0 {
				t.Error("live visit counter still zero while a visit is being announced")
			}
		}
	})
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	if born.Load() != 2 || died.Load() != 2 || visits.Load() != 4 || s.metrics.TotalVisits.Load() != 4 || s.metrics.LemmingsAlive.Load() != 0 {
		t.Fatalf("born=%d died=%d visits=%d metrics=%d alive=%d",
			born.Load(), died.Load(), visits.Load(), s.metrics.TotalVisits.Load(), s.metrics.LemmingsAlive.Load())
	}
	d := s.reporter.Result()
	if d.TotalVisits != 4 || d.Sessions != 2 || d.Rescued != 2 || d.RescuedPercent != 100 {
		t.Fatalf("report: visits=%d sessions=%d rescued=%d (%.1f%%)", d.TotalVisits, d.Sessions, d.Rescued, d.RescuedPercent)
	}
	if s.metrics.TerrainsOnline.Load() != 0 {
		t.Fatalf("terrains should all be done, %d still online", s.metrics.TerrainsOnline.Load())
	}
}

// TestReporter_PercentilesStayExactThenBounded verifies exact nearest-rank
// percentiles on unsorted input, histogram percentiles within 2% once
// samples exceed what is kept, bounded memory, and that query strings
// never reach the report.
func TestReporter_PercentilesStayExactThenBounded(t *testing.T) {
	r := NewReporter(testConfig())
	for _, ms := range []int{100, 1, 50, 2, 3} {
		r.RecordVisit(&Visit{URL: "https://example.com/p?token=secret", StatusCode: 200, Duration: time.Duration(ms) * time.Millisecond})
	}
	d := r.Result()
	if d.Paths[0].P50 != 3*time.Millisecond || d.PercentileMethod != "exact (nearest rank)" {
		t.Fatalf("p50=%s method=%q", d.Paths[0].P50, d.PercentileMethod)
	}
	for i := 0; i < 10_000; i++ {
		r.RecordVisit(&Visit{URL: "https://example.com/p?token=other", StatusCode: 503, Duration: 100 * time.Millisecond})
	}
	d = r.Result()
	if d.TotalVisits != 10_005 || d.FailedVisits != 10_000 || len(d.Paths) != 1 {
		t.Fatalf("aggregates: visits=%d failed=%d paths=%d", d.TotalVisits, d.FailedVisits, len(d.Paths))
	}
	if d.P95 < 100*time.Millisecond || d.P95 > 102*time.Millisecond {
		t.Fatalf("histogram p95=%s, want within 2%% of 100ms", d.P95)
	}
	if len(r.latency.durations) > exactGlobalSamples || len(r.byPath["https://example.com/p"].durations) > exactPathSamples {
		t.Fatal("latency samples are unbounded")
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "token=") {
		t.Fatal("query value leaked into the JSON report")
	}
}

// TestTraceWriter_RoundTripAndNoOverwrite verifies that the trace writes
// sanitised JSONL and refuses to overwrite an existing file.
func TestTraceWriter_RoundTripAndNoOverwrite(t *testing.T) {
	name := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := newTraceWriter(name)
	if err != nil {
		t.Fatal(err)
	}
	tw.record(&Visit{URL: "https://example.com/a?password=secret", StatusCode: 200, LemmingID: "session-a", Sequence: 1})
	tw.close()
	tw.close() // idempotent

	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var got VisitTrace
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Lemming != "session-a" || got.URL != "https://example.com/a" || tw.written.Load() != 1 {
		t.Fatalf("bad trace line: %s", b)
	}
	if _, err := newTraceWriter(name); err == nil {
		t.Fatal("trace writer overwrote existing evidence")
	}
}

// TestEventBus_SubscriberMayUnsubscribeItself verifies that a subscriber
// can unsubscribe from inside its own callback without deadlocking.
func TestEventBus_SubscriberMayUnsubscribeItself(t *testing.T) {
	b := NewEventBus()
	var called atomic.Int64
	var unsub func()
	unsub = b.Subscribe(func(Event) { called.Add(1); unsub() })
	done := make(chan struct{})
	go func() { b.Emit(Event{}); b.Emit(Event{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscriber deadlocked the bus")
	}
	if called.Load() != 1 {
		t.Fatalf("unsubscribe ignored: called %d times", called.Load())
	}
}

// TestSitemap_CyclesAndEntities verifies that a self-referencing sitemap
// index terminates and that XML entities in <loc> are decoded.
func TestSitemap_CyclesAndEntities(t *testing.T) {
	var root string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<sitemapindex><sitemap><loc>%s/sitemap.xml</loc></sitemap></sitemapindex>`, root)
	}))
	defer srv.Close()
	root = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := indexSitemap(ctx, srv.Client(), root+"/sitemap.xml"); err != nil {
		t.Fatal(err)
	}
	u := extractSitemapLocs(`<urlset><url><loc>https://example.com/?a=1&amp;b=2</loc></url></urlset>`)
	if len(u) != 1 || u[0] != "https://example.com/?a=1&b=2" {
		t.Fatalf("XML entities: %v", u)
	}
}

// TestRenderHTML_EscapesUntrustedContent verifies that URLs, titles and
// names from the target cannot inject markup into the HTML report.
func TestRenderHTML_EscapesUntrustedContent(t *testing.T) {
	r := NewReporter(testConfig())
	evil := `https://example.com/<script>alert(1)</script>`
	v := Visit{URL: evil, StatusCode: 500, Sequence: 1, Page: PageEvidence{Title: `<img src=x onerror=alert(1)>`}}
	r.RecordVisit(&v)
	r.Ingest(LifeLog{
		Streamed: true, TotalVisits: 1, FailedVisits: 1, SlowestVisit: time.Millisecond, FirstFailureAt: time.Now(),
		Identity: Identity{ID: "0123456789abcdef", Name: `<b>Neon</b>`}, Visits: []Visit{v},
	})
	h, err := renderHTML(r.Result())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script>alert(1)</script>", "<img src=x onerror", "<b>Neon</b>"} {
		if strings.Contains(h, bad) {
			t.Fatalf("report contains unescaped %q", bad)
		}
	}
	if !strings.Contains(h, "Patient Zero") {
		t.Fatal("the failing lemming should be featured as Patient Zero")
	}
}

// TestParseJourney_Validation verifies that journeys leaving the origin,
// using unsupported selectors or containing unknown fields are rejected,
// and that a valid journey loads through SwarmConfig.validate.
func TestParseJourney_Validation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "journey.json")
	cfg := testConfig()
	cfg.JourneyFile = file
	for _, body := range []string{
		`{"steps":[{"path":"http://evil.test/"}]}`,
		`{"steps":[{"path":"/","expect":{"selectors":["main .unsupported"]}}]}`,
		`{"steps":[{"path":"/","typo":true}]}`,
		`{"steps":[{"path":"/","expect":{"status":[42]}}]}`,
		`{"steps":[]}`,
		`{"steps":[{"path":"/"}]} {"steps":[]}`,
	} {
		os.WriteFile(file, []byte(body), 0o600)
		c := cfg
		if c.validate() == nil {
			t.Fatalf("accepted invalid journey %s", body)
		}
	}
	os.WriteFile(file, []byte(`{"name":"browse","steps":[{"path":"/","expect":{"status":[200],"selectors":["main","#app"]}}]}`), 0o600)
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Journey == nil || cfg.Journey.Steps[0].Name != "step-1" {
		t.Fatalf("journey not loaded or unnamed step not defaulted: %+v", cfg.Journey)
	}
}

// TestJourney_WalksStepsInOrderAndStops verifies that a lemming on a
// journey visits each step once, in order, applies each step's checks,
// and leaves when the journey is complete.
func TestJourney_WalksStepsInOrderAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><title>page %s</title></head><body><main>%s</main></body></html>`, r.URL.Path, r.URL.Path)
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 2 * time.Second
	cfg.Journey = &Journey{Name: "tour", Steps: []JourneyStep{
		{Name: "home", Path: "/"},
		{Name: "catalog", Path: "/catalog", Expect: Expectations{Contains: []string{"/catalog"}}},
		{Name: "missing", Path: "/cart", Expect: Expectations{TitleContains: "checkout"}},
	}}
	ll := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics()).Run(context.Background())

	if ll.ExitReason != ExitJourneyComplete || ll.TotalVisits != 3 {
		t.Fatalf("exit=%s visits=%d", ll.ExitReason, ll.TotalVisits)
	}
	steps := []string{ll.Visits[0].Step, ll.Visits[1].Step, ll.Visits[2].Step}
	if strings.Join(steps, ",") != "home,catalog,missing" {
		t.Fatalf("steps visited out of order: %v", steps)
	}
	if ll.Visits[1].Failed || !ll.Visits[2].Failed || ll.FailedVisits != 1 {
		t.Fatalf("step checks misapplied: catalog failed=%v cart failed=%v", ll.Visits[1].Failed, ll.Visits[2].Failed)
	}
}

// TestWaitingRoom_AdmitsDynamicHTML verifies admission when the queue
// signature disappears even though the admitted page differs from the
// index-time body, and that queue events carry the position.
func TestWaitingRoom_AdmitsDynamicHTML(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, waitingRoomHTML(7))
			return
		}
		fmt.Fprint(w, `<html><body><main>Admitted with a fresh CSRF nonce</main></body></html>`)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Hit = srv.URL
	bus := NewEventBus()
	var queued, left []Event
	bus.Subscribe(Filter(func(e Event) { queued = append(queued, e) }, EventWaitingRoomQueued))
	bus.Subscribe(Filter(func(e Event) { left = append(left, e) }, EventWaitingRoom))
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{srv.URL: "old-nonce"}}, newTestClient(), bus, testMetrics())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, _ := l.hit(ctx, srv.URL, nil)
	if v.Error != nil || v.Failed || v.Match || !v.WaitingRoom.Detected || calls.Load() != 2 {
		t.Fatalf("admission: err=%v failed=%v match=%v detected=%v calls=%d", v.Error, v.Failed, v.Match, v.WaitingRoom.Detected, calls.Load())
	}
	if len(queued) != 1 || queued[0].Position != 7 {
		t.Fatalf("expected one queued event at position 7, got %+v", queued)
	}
	if len(left) != 1 || left[0].Duration <= 0 {
		t.Fatalf("expected one waiting room exit with a duration, got %+v", left)
	}
}

// TestThink_HonoursRetryAfter verifies that a 429 with Retry-After parks
// the lemming instead of hammering the server.
func TestThink_HonoursRetryAfter(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 150 * time.Millisecond
	ll := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics()).Run(context.Background())
	if calls.Load() != 1 || ll.FailedVisits != 1 {
		t.Fatalf("rate-limit backoff ignored: calls=%d failures=%d", calls.Load(), ll.FailedVisits)
	}
}

// TestThink_BacksOffFailuresWithZeroThinkTime verifies that a dead origin
// is not spun against even when think time is zero.
func TestThink_BacksOffFailuresWithZeroThinkTime(t *testing.T) {
	cfg := testConfig()
	cfg.Hit = "http://127.0.0.1:1/"
	cfg.Until = 350 * time.Millisecond
	ll := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics()).Run(context.Background())
	if ll.TotalVisits > 5 {
		t.Fatalf("a failing lemming made %d visits in 350ms; it should back off", ll.TotalVisits)
	}
}

// TestErrorKind_Classification verifies the failure labels reports use.
func TestErrorKind_Classification(t *testing.T) {
	cases := map[string]error{
		"":                 nil,
		"cancelled":        context.Canceled,
		"timeout":          fmt.Errorf("do request: %w", context.DeadlineExceeded),
		"offsite-redirect": fmt.Errorf("x: %w", errOffsiteRedirect),
		"redirect-loop":    errTooManyRedirects,
		"body-too-large":   errBodyTooLarge,
		"dns":              &net.DNSError{Err: "no such host", Name: "nope.invalid"},
		"refused":          &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		"reset":            fmt.Errorf("read body: %w", io.ErrUnexpectedEOF),
		"request":          errors.New("something else"),
	}
	for want, err := range cases {
		if got := errorKind(err); got != want {
			t.Errorf("errorKind(%v) = %q, want %q", err, got, want)
		}
	}
}

// TestParseRetryAfter verifies both header forms and the cap.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":    0,
		"0":   0,
		"-5":  0,
		"abc": 0,
		"7":   7 * time.Second,
		"999": maxRetryAfter,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
		now.Add(time.Hour).Format(http.TimeFormat):        maxRetryAfter,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestHistogram_QuantileErrorBound verifies that histogram quantiles stay
// within histogramGrowth of the exact nearest-rank value.
func TestHistogram_QuantileErrorBound(t *testing.T) {
	var h histogram
	var exact []time.Duration
	for i := 1; i <= 5000; i++ {
		d := time.Duration(i*i) * time.Microsecond
		h.add(d)
		exact = append(exact, d)
	}
	for _, p := range []float64{1, 50, 90, 95, 99, 100} {
		got, want := h.quantile(p), percentile(exact, int(p))
		if got < want || float64(got) > float64(want)*histogramGrowth+1 {
			t.Errorf("p%.0f = %s, exact %s: outside the %.0f%% bound", p, got, want, (histogramGrowth-1)*100)
		}
	}
	var merged histogram
	merged.merge(&h)
	if merged.quantile(95) != h.quantile(95) || merged.count != h.count || merged.min != h.min || merged.max != h.max {
		t.Fatal("merge changed the distribution")
	}
}

// TestTimeline_StaysBoundedAndCountsAlive verifies that the timeline
// compacts rather than grows, and that alive is births minus deaths.
func TestTimeline_StaysBoundedAndCountsAlive(t *testing.T) {
	tl := newTimeline()
	tl.bucket(0).Born += 3
	tl.bucket(2*time.Second).Died++
	if early := tl.points(0); early[0].Alive != 3 || early[2].Alive != 2 {
		t.Fatalf("alive before compaction: %d then %d, want 3 then 2", early[0].Alive, early[2].Alive)
	}
	for i := 0; i < 5000; i++ {
		b := tl.bucket(time.Duration(i) * time.Second)
		b.Visits++
		b.Latency.add(time.Millisecond)
	}
	if len(tl.buckets) > maxTimelineBuckets {
		t.Fatalf("timeline grew to %d buckets", len(tl.buckets))
	}
	pts := tl.points(0)
	var visits int64
	for _, p := range pts {
		visits += p.Visits
	}
	if visits != 5000 || pts[len(pts)-1].Alive != 2 {
		t.Fatalf("visits=%d last alive=%d", visits, pts[len(pts)-1].Alive)
	}
	if got := tl.points(10); len(got) != 10 {
		t.Fatalf("points(10) returned %d", len(got))
	}
}
