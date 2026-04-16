package main

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	// testUntil is the lemming lifespan used in tests.
	// Short enough that tests complete in milliseconds.
	testUntil = 100 * time.Millisecond

	// testRamp is the ramp duration used in tests.
	testRamp = 50 * time.Millisecond

	// testCrawlDepth is the crawl depth used in tests.
	testCrawlDepth = 2

	// normalPageBody is a minimal but realistic HTML page body
	// used as the "expected" content for normal page visits.
	normalPageBody = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><title>Test Page</title></head>
<body>
  <nav>
    <a href="/">Home</a>
    <a href="/about">About</a>
    <a href="/pricing">Pricing</a>
    <a href="/contact">Contact</a>
  </nav>
  <main><h1>Hello lemmings</h1></main>
</body>
</html>`

	// waitingRoomPosition is the queue position embedded in test
	// waiting room responses.
	waitingRoomPosition = 42
)

// ── SwarmConfig helper ────────────────────────────────────────────────────────

// testConfig returns a minimal valid SwarmConfig suitable for unit tests.
// All durations are short so tests complete quickly. The Hit field is
// intentionally left empty — callers that need a real URL should set it
// to their httptest.Server URL after calling testConfig.
//
// Usage:
//
//	cfg := testConfig()
//	cfg.Hit = srv.URL
func testConfig() SwarmConfig {
	return SwarmConfig{
		Hit:             "http://localhost:8080/",
		Terrain:         2,
		Pack:            2,
		Limit:           10,
		Until:           testUntil,
		Ramp:            testRamp,
		Crawl:           false,
		CrawlDepth:      testCrawlDepth,
		DashboardPort:   0,
		TTY:             false,
		Observe:         false,  // new
		MetricsPort:     0,      // new — 0 means no metrics server in tests
		MetricsURLLabel: "path", // new
		Version:         "test",
	}
}

// ── URLPool helper ────────────────────────────────────────────────────────────

// testPool returns a *URLPool populated with the provided base URL and
// three sub-paths. Checksums are computed from normalPageBody so that
// a test server returning normalPageBody will produce matching checksums.
//
// Usage:
//
//	pool := testPool(srv.URL)
//	// pool.URLs contains srv.URL+"/", srv.URL+"/about", srv.URL+"/pricing"
//
// Warning: checksums are derived from normalPageBody. If your test server
// returns different content, checksums will not match and lemmings will
// record every visit as a mismatch. This is intentional for tests that
// verify mismatch handling, and unintentional otherwise.
func testPool(baseURL string) *URLPool {
	urls := []string{
		baseURL + "/",
		baseURL + "/about",
		baseURL + "/pricing",
	}

	checksums := make(map[string]string, len(urls))
	for _, u := range urls {
		checksums[u] = sha512hex([]byte(normalPageBody))
	}

	return &URLPool{
		Origin:    baseURL,
		URLs:      urls,
		Checksums: checksums,
	}
}

// sha512hex is the test-internal equivalent of sha512sum, kept here so
// test helpers don't depend on the implementation under test.
func sha512hex(b []byte) string {
	h := sha512.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// ── EventBus helper ───────────────────────────────────────────────────────────

// testBus returns a wired *EventBus with an *EventLog subscriber attached
// and the unsubscribe function for cleanup.
//
// Usage:
//
//	bus, log, unsub := testBus()
//	defer unsub()
//	bus.Emit(Event{Kind: EventLemmingBorn})
//	events := log.Snapshot()
func testBus() (*EventBus, *EventLog, func()) {
	bus := NewEventBus()
	log := NewEventLog(256)
	unsub := bus.Subscribe(log.AsSubscriber())
	return bus, log, unsub
}

// ── SwarmMetrics helper ───────────────────────────────────────────────────────

// testMetrics returns a zero-value *SwarmMetrics ready for use in tests.
// All atomic counters start at zero.
func testMetrics() *SwarmMetrics {
	return &SwarmMetrics{}
}

// ── testServer ────────────────────────────────────────────────────────────────

// testServer is a configurable httptest.Server builder that supports
// the range of HTTP behaviors lemmings encounters in production:
// normal pages, waiting rooms, sitemaps, robots.txt, and error codes.
//
// Use the fluent With* methods to configure routes, then call Build to
// get a running *httptest.Server. The server is automatically shut down
// via t.Cleanup when the test ends.
//
// Usage:
//
//	srv := newTestServer(t).
//	    withNormalPage("/", normalPageBody).
//	    withWaitingRoom("/about", 42).
//	    withSitemap("/sitemap.xml", []string{"/", "/about"}).
//	    build()
//
//	// srv.URL is the base URL to pass to testConfig or testPool.
type testServer struct {
	t      *testing.T
	tb     *testing.TB
	b      *testing.B
	f      *testing.F
	mu     sync.RWMutex
	routes map[string]http.HandlerFunc
}

// newTestServer constructs a testServer with no routes configured.
// At least one route should be added before calling Build.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	return &testServer{
		t:      t,
		routes: make(map[string]http.HandlerFunc),
	}
}

// newTestServer constructs a testServer with no routes configured.
// At least one route should be added before calling Build.
func newTestBenchServer(t testing.TB) *testServer {
	return &testServer{
		tb:     &t,
		routes: make(map[string]http.HandlerFunc),
	}
}

// newFuzzServer constructs a testServer with no routes configured.
// At least one route should be added before calling Build.
func newFuzzServer(f *testing.F) *testServer {
	f.Helper()
	return &testServer{
		t:      nil,
		b:      nil,
		f:      f,
		routes: make(map[string]http.HandlerFunc),
	}
}

// newBenchServer constructs a testServer with no routes configured.
// At least one route should be added before calling Build.
func newBenchServer(b *testing.B) *testServer {
	b.Helper()
	return &testServer{
		t:      nil,
		b:      b,
		routes: make(map[string]http.HandlerFunc),
	}
}

// withNormalPage registers a route that returns normalPageBody with a 200
// status and Content-Type text/html. The checksum of this body matches
// what testPool precomputes, so lemmings will record these visits as matches.
func (ts *testServer) withNormalPage(path string) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, normalPageBody)
	}
	return ts
}

// withCustomPage registers a route that returns the provided body with a 200.
// Use this when you need the checksum to differ from testPool's precomputed
// values — for example, to simulate a page that changed after indexing.
func (ts *testServer) withCustomPage(path, body string) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
	return ts
}

// withWaitingRoom registers a route that returns waiting room HTML with the
// provided queue position embedded in the id="position" element.
// Subsequent requests to the same path continue returning waiting room HTML
// until withNormalPage is called for the same path.
//
// Warning: this registers a static waiting room response. The position
// number does not decrement between requests. Tests that need dynamic
// position changes should use withHandler directly and manage state manually.
func (ts *testServer) withWaitingRoom(path string, position int) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	body := waitingRoomHTML(position)
	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
	return ts
}

// withAdmittingWaitingRoom registers a route that returns waiting room HTML
// for the first N requests then switches to normalPageBody. This simulates
// a real room admitting a lemming after it has waited.
//
// Usage:
//
//	// Returns waiting room for first 2 requests, then normal page
//	ts.withAdmittingWaitingRoom("/queue", 42, 2)
func (ts *testServer) withAdmittingWaitingRoom(path string, position, holdCount int) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	var mu sync.Mutex
	requests := 0

	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count := requests
		requests++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if count < holdCount {
			fmt.Fprint(w, waitingRoomHTML(position))
		} else {
			fmt.Fprint(w, normalPageBody)
		}
	}
	return ts
}

// withSitemap registers a route that returns a valid sitemap.xml containing
// the provided URL list as <loc> entries.
func (ts *testServer) withSitemap(path string, urls []string) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range urls {
		sb.WriteString(`<url><loc>`)
		sb.WriteString(u)
		sb.WriteString(`</loc></url>`)
	}
	sb.WriteString(`</urlset>`)
	body := sb.String()

	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
	return ts
}

// withSitemapIndex registers a route that returns a sitemap index XML file
// pointing to the provided child sitemap URLs.
func (ts *testServer) withSitemapIndex(path string, childSitemapURLs []string) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range childSitemapURLs {
		sb.WriteString(`<sitemap><loc>`)
		sb.WriteString(u)
		sb.WriteString(`</loc></sitemap>`)
	}
	sb.WriteString(`</sitemapindex>`)
	body := sb.String()

	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
	return ts
}

// withRobots registers a route that returns a robots.txt containing a
// Sitemap: directive pointing to the provided sitemap URL.
func (ts *testServer) withRobots(sitemapURL string) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	body := fmt.Sprintf("User-agent: *\nDisallow: /admin\nSitemap: %s\n", sitemapURL)
	ts.routes["/robots.txt"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
	return ts
}

// withStatusCode registers a route that returns the provided HTTP status code
// with an empty body. Use this to test how lemmings handle 4xx and 5xx.
func (ts *testServer) withStatusCode(path string, code int) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}
	return ts
}

// withHandler registers an arbitrary http.HandlerFunc for the given path.
// Use this for test cases that require stateful or dynamic behavior not
// covered by the other With* methods.
func (ts *testServer) withHandler(path string, fn http.HandlerFunc) *testServer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.routes[path] = fn
	return ts
}

// build starts the httptest.Server and registers t.Cleanup to close it.
// Returns the running server. All With* calls must happen before Build.
//
// Warning: routes cannot be added after Build is called. The underlying
// mux is frozen at build time.
func (ts *testServer) build() *httptest.Server {
	ts.t.Helper()
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	mux := http.NewServeMux()

	// Register all configured routes
	for path, handler := range ts.routes {
		mux.HandleFunc(path, handler)
	}

	// Default fallback — returns 404 with a diagnostic body so test
	// failures clearly identify which path was unexpectedly requested.
	// Only register if "/" was not already explicitly configured.
	if _, hasRoot := ts.routes["/"]; !hasRoot {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "testServer: no route registered for %s", r.URL.Path)
		})
	}

	srv := httptest.NewServer(mux)
	ts.t.Cleanup(srv.Close)
	return srv
}

// ── HTML generators ───────────────────────────────────────────────────────────

// waitingRoomHTML returns a minimal but structurally correct waiting room HTML
// page that satisfies detectWaitingRoom detection. The position argument is
// embedded in the id="position" element exactly as the room package does.
//
// The body contains /queue/status (the waitingRoomSignature constant) and
// the position marker so detectWaitingRoom returns (true, position).
func waitingRoomHTML(position int) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><title>Please Wait</title></head>
<body>
<div class="card">
  <h1>You're in the queue</h1>
  <div class="position-block">
    <div class="position-number" id="position">%d</div>
  </div>
  <div class="status">Waiting for a slot to open...</div>
</div>
<script>
const STATUS_ENDPOINT = "/queue/status";
</script>
</body>
</html>`, position)
}

// ── Assertion helpers ─────────────────────────────────────────────────────────

// requireEventEmitted asserts that at least one event of the given kind
// appears in the provided EventLog snapshot within timeout.
// Fails the test immediately if the event does not appear in time.
//
// Usage:
//
//	requireEventEmitted(t, log, EventLemmingBorn, 500*time.Millisecond)
func requireEventEmitted(t *testing.T, log *EventLog, kind EventKind, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range log.Snapshot() {
			if e.Kind == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("event %q was not emitted within %s", kind, timeout)
}

// requireNoEventEmitted asserts that no event of the given kind appears
// in the EventLog within the observation window. Used to verify that
// certain failure paths are NOT triggered.
func requireNoEventEmitted(t *testing.T, log *EventLog, kind EventKind, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	for _, e := range log.Snapshot() {
		if e.Kind == kind {
			t.Fatalf("event %q was emitted but should not have been", kind)
		}
	}
}

// collectEvents subscribes to a bus, collects all events emitted during
// fn's execution, then returns them. The subscription is removed after fn
// returns.
//
// Usage:
//
//	events := collectEvents(t, bus, func() {
//	    bus.Emit(Event{Kind: EventLemmingBorn})
//	})
//	// events contains the emitted event
func collectEvents(t *testing.T, bus *EventBus, fn func()) []Event {
	t.Helper()
	var mu sync.Mutex
	var collected []Event
	unsub := bus.Subscribe(func(e Event) {
		mu.Lock()
		collected = append(collected, e)
		mu.Unlock()
	})
	defer unsub()
	fn()
	mu.Lock()
	defer mu.Unlock()
	return collected
}
