package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExperienceSessionCookiesPersonaJourney(t *testing.T) {
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
	cfg.Experience = ExperienceConfig{ThinkMin: 5 * time.Millisecond, ThinkMax: 5 * time.Millisecond, MaxPages: 2, Navigation: "links"}
	pool := &URLPool{URLs: []string{cfg.Hit}, Checksums: map[string]string{cfg.Hit: "different"}}
	client := newTestClient()
	client.Jar, _ = cookiejar.New(nil)
	l := NewLemming(0, 0, cfg, pool, client, NewEventBus(), testMetrics())
	ll := l.Run(context.Background())
	if ll.TotalVisits != 2 || ll.FailedVisits != 0 {
		t.Fatalf("session: %+v", ll)
	}
	if ll.Visits[1].URL != srv.URL+"/catalog/item" {
		t.Fatalf("not following links: %s", ll.Visits[1].URL)
	}
	if uas[0] != uas[1] || uas[0] == "" {
		t.Fatal("persona changed")
	}
	if !strings.Contains(cookies[1], "private-session-value") {
		t.Fatal("session cookie lost")
	}
	if refs[1] != cfg.Hit {
		t.Fatalf("referrer=%s", refs[1])
	}
	if ll.Visits[0].Match {
		t.Fatal("dynamic checksum should differ")
	}
	b, _ := json.Marshal(evidence(ll.Visits[1]))
	if strings.Contains(string(b), "private-session-value") {
		t.Fatal("cookie value leaked")
	}
}

func TestExperienceAssertionsDetectSoftError(t *testing.T) {
	body := []byte(`<html><head><title>Oops</title></head><body><main class="error">Something went wrong</main></body></html>`)
	p := inspectPage(body, "text/html; charset=utf-8", 200, Expectations{Status: []int{200}, Contains: []string{"Welcome"}, NotContains: []string{"Something went wrong"}, TitleContains: "Catalog", Selectors: []string{"#app", "main", ".error"}})
	v := Visit{StatusCode: 200, Page: p}
	if !v.failed() {
		t.Fatal("soft 200 error passed")
	}
	failed := 0
	for _, c := range p.Checks {
		if !c.Passed {
			failed++
		}
	}
	if failed != 4 {
		t.Fatalf("expected 4 failed checks: %+v", p.Checks)
	}
	if p.Mode != "http-response" {
		t.Fatal("HTML response mislabeled as rendered")
	}
}

func TestExperienceRedirectAndOriginScope(t *testing.T) {
	var external atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { external.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/done", 302)
		case "/escape":
			http.Redirect(w, r, other.URL, 302)
		default:
			w.Write([]byte("done"))
		}
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Hit = srv.URL
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics())
	v := l.hit(context.Background(), srv.URL)
	if v.StatusCode != 200 || len(v.Responses) != 2 || v.Responses[0].Status != 302 || v.FinalURL != srv.URL+"/done" {
		t.Fatalf("redirect evidence: %+v", v)
	}
	v = l.hit(context.Background(), srv.URL+"/escape")
	if v.Error == nil || external.Load() != 0 {
		t.Fatal("off-origin redirect was followed")
	}
	if sameOrigin("https://example.com", "https://example.com.evil.test") || sameOrigin("https://example.com", "http://example.com") {
		t.Fatal("scope mismatch")
	}
	if !sameOrigin("https://EXAMPLE.com", "https://example.com:443/path") {
		t.Fatal("equivalent origins rejected")
	}
	if resolveURL(srv.URL+"/catalog/list", "item") != srv.URL+"/catalog/item" {
		t.Fatal("relative link resolved at wrong level")
	}
}

func TestExperienceLimitsAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		w.Write([]byte(strings.Repeat("x", 1024)))
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Experience.MaxBodyBytes = 128
	cfg.Experience.RequestTimeout = 20 * time.Millisecond
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics())
	v := l.hit(context.Background(), srv.URL)
	if v.Error == nil || v.BytesIn != 129 {
		t.Fatalf("unbounded body: %+v", v)
	}
	v = l.hit(context.Background(), srv.URL+"/slow")
	if v.ErrorKind != "timeout" || v.Cancelled {
		t.Fatalf("request timeout misclassified: %+v", v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v = l.hit(ctx, srv.URL)
	if !v.Cancelled || v.failed() {
		t.Fatalf("run cancellation must not fail target: %+v", v)
	}
}

func TestExperienceStreamingCountersAndSingleLifecycle(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Terrain = 1
	cfg.Pack = 2
	cfg.Until = 100 * time.Millisecond
	cfg.Experience = ExperienceConfig{MaxPages: 2, ThinkMin: 20 * time.Millisecond, ThinkMax: 20 * time.Millisecond}
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
				t.Error("live counter still zero during visit")
			}
		}
	})
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	if born.Load() != 2 || died.Load() != 2 || visits.Load() != 4 || s.metrics.TotalVisits.Load() != 4 || s.metrics.LemmingsAlive.Load() != 0 {
		t.Fatalf("born=%d died=%d visits=%d metrics=%d alive=%d", born.Load(), died.Load(), visits.Load(), s.metrics.TotalVisits.Load(), s.metrics.LemmingsAlive.Load())
	}
	d := s.reporter.buildReportData()
	if d.TotalVisits != 4 || d.Experience.Sessions != 2 {
		t.Fatalf("double counted report: %+v", d.Experience)
	}
}

func TestExperienceUnsortedPercentilesAndBoundedEvidence(t *testing.T) {
	r := NewReporter(testConfig())
	for _, ms := range []int{100, 1, 50, 2, 3} {
		r.RecordVisit(Visit{URL: "https://example.com/p?token=secret", StatusCode: 200, Duration: time.Duration(ms) * time.Millisecond})
	}
	d := r.buildReportData()
	if d.Paths[0].P50 != 3*time.Millisecond {
		t.Fatalf("unsorted p50=%s", d.Paths[0].P50)
	}
	for i := 0; i < 10000; i++ {
		r.RecordVisit(Visit{URL: "https://example.com/p", StatusCode: 503, Duration: 100 * time.Millisecond})
	}
	d = r.buildReportData()
	if d.TotalVisits != 10005 || d.Experience.Failed != 10000 {
		t.Fatal("aggregates lost")
	}
	if d.P95 < 100*time.Millisecond || d.P95 > 105*time.Millisecond {
		t.Fatalf("histogram p95=%s", d.P95)
	}
	if len(r.globalDurations) > retainedDurations || len(r.byPath["https://example.com/p"].durations) > retainedDurations {
		t.Fatal("unbounded durations")
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "token=secret") {
		t.Fatal("query value leaked")
	}
}

func TestExperienceTraceRoundTripAndNoOverwrite(t *testing.T) {
	name := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := newTraceWriter(name)
	if err != nil {
		t.Fatal(err)
	}
	v := Visit{URL: "https://example.com/a?password=secret", StatusCode: 200, LemmingID: "session-a", Sequence: 1}
	tw.record(v)
	tw.close()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var got VisitEvidence
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "session-a" || got.URL != "https://example.com/a" {
		t.Fatalf("bad trace: %s", b)
	}
	if _, err := newTraceWriter(name); err == nil {
		t.Fatal("overwrote trace")
	}
}

func TestExperienceEventBusAllowsSelfUnsubscribe(t *testing.T) {
	b := NewEventBus()
	var called atomic.Int64
	var unsub func()
	unsub = b.Subscribe(func(Event) { called.Add(1); unsub() })
	done := make(chan struct{})
	go func() { b.Emit(Event{}); b.Emit(Event{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscriber deadlocked")
	}
	if called.Load() != 1 {
		t.Fatal("unsubscribe ignored")
	}
}

func TestExperienceSitemapCyclesAndEntities(t *testing.T) {
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

func TestExperienceHTMLReportEscapesUntrustedContent(t *testing.T) {
	r := NewReporter(testConfig())
	r.RecordVisit(Visit{URL: `https://example.com/<script>alert(1)</script>`, StatusCode: 500})
	r.Ingest(LifeLog{Streamed: true, Identity: Identity{ID: `<img src=x onerror=alert(1)>`}})
	h, err := renderHTML(r.buildReportData())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "<script>alert(1)</script>") || strings.Contains(h, "<img src=x onerror") {
		t.Fatal("unescaped report evidence")
	}
}

func TestExperienceScenarioValidation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "scenario.json")
	cfg := testConfig()
	cfg.Experience.ScenarioFile = file
	for _, body := range []string{`{"steps":[{"path":"http://evil.test/"}]}`, `{"steps":[{"path":"/","expect":{"selectors":["main .unsupported"]}}]}`, `{"steps":[{"path":"/","typo":true}]}`} {
		os.WriteFile(file, []byte(body), 0600)
		if cfg.prepareExperience() == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	os.WriteFile(file, []byte(`{"name":"browse","steps":[{"name":"home","path":"/","expect":{"status":[200],"selectors":["main","#app"]}}]}`), 0600)
	if err := cfg.prepareExperience(); err != nil {
		t.Fatal(err)
	}
}

func TestExperienceQueueAdmitsDynamicHTML(t *testing.T) {
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
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{srv.URL: "old-nonce"}}, newTestClient(), NewEventBus(), testMetrics())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	v := l.hit(ctx, srv.URL)
	if v.Error != nil || v.failed() || v.Match || !v.WaitingRoom.Detected {
		t.Fatalf("dynamic queue admission: %+v", v)
	}
	if calls.Load() != 2 || v.ResponseCodes[200] != 2 {
		t.Fatalf("queue responses missing: %v", v.ResponseCodes)
	}
}

func TestExperienceRetryAfterPacesRateLimitedUser(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 150 * time.Millisecond
	l := NewLemming(0, 0, cfg, &URLPool{Checksums: map[string]string{}}, newTestClient(), NewEventBus(), testMetrics())
	ll := l.Run(context.Background())
	if calls.Load() != 1 || ll.FailedVisits != 1 {
		t.Fatalf("rate-limit backoff ignored: calls=%d failures=%d", calls.Load(), ll.FailedVisits)
	}
}
