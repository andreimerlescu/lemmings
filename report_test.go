package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── NewReporter ───────────────────────────────────────────────────────────────

// TestNewReporter_InitialState verifies that a freshly constructed Reporter
// has an empty path map, no accumulated logs, and a non-zero start time.
func TestNewReporter_InitialState(t *testing.T) {
	r := NewReporter(testConfig())
	if r == nil {
		t.Fatal("NewReporter returned nil")
	}
	if len(r.byPath) != 0 {
		t.Errorf("expected empty byPath map, got %d entries", len(r.byPath))
	}
	if len(r.all) != 0 {
		t.Errorf("expected empty all slice, got %d entries", len(r.all))
	}
	if r.startedAt.IsZero() {
		t.Error("startedAt should be set at construction")
	}
}

// ── Reporter.Ingest ───────────────────────────────────────────────────────────

// TestReporter_Ingest_IncrementHitCount verifies that each Visit in a
// LifeLog increments the hit counter for its path.
func TestReporter_Ingest_IncrementHitCount(t *testing.T) {
	r := NewReporter(testConfig())
	ll := makeLifeLog("http://example.com/about", 3, http.StatusOK, false, nil)
	r.Ingest(ll)

	ps, ok := r.byPath["http://example.com/about"]
	if !ok {
		t.Fatal("expected path entry for /about")
	}
	if ps.Hits != 3 {
		t.Errorf("expected 3 hits, got %d", ps.Hits)
	}
}

// TestReporter_Ingest_AccumulatesBytes verifies that BytesIn across
// all visits to a path are summed correctly.
func TestReporter_Ingest_AccumulatesBytes(t *testing.T) {
	r := NewReporter(testConfig())

	ll := LifeLog{
		Visits: []Visit{
			{URL: "http://example.com/", BytesIn: 1024, StatusCode: 200},
			{URL: "http://example.com/", BytesIn: 2048, StatusCode: 200},
			{URL: "http://example.com/", BytesIn: 512, StatusCode: 200},
		},
	}
	r.Ingest(ll)

	ps := r.byPath["http://example.com/"]
	if ps.Bytes != 3584 {
		t.Errorf("expected 3584 total bytes, got %d", ps.Bytes)
	}
}

// TestReporter_Ingest_StatusCodeBuckets verifies that status codes are
// placed into the correct 2xx/3xx/4xx/5xx buckets.
func TestReporter_Ingest_StatusCodeBuckets(t *testing.T) {
	r := NewReporter(testConfig())

	ll := LifeLog{
		Visits: []Visit{
			{URL: "http://example.com/", StatusCode: 200},
			{URL: "http://example.com/", StatusCode: 201},
			{URL: "http://example.com/", StatusCode: 301},
			{URL: "http://example.com/", StatusCode: 302},
			{URL: "http://example.com/", StatusCode: 404},
			{URL: "http://example.com/", StatusCode: 500},
			{URL: "http://example.com/", StatusCode: 503},
		},
	}
	r.Ingest(ll)

	ps := r.byPath["http://example.com/"]
	if ps.xx2 != 2 {
		t.Errorf("expected 2 2xx, got %d", ps.xx2)
	}
	if ps.xx3 != 2 {
		t.Errorf("expected 2 3xx, got %d", ps.xx3)
	}
	if ps.xx4 != 1 {
		t.Errorf("expected 1 4xx, got %d", ps.xx4)
	}
	if ps.xx5 != 2 {
		t.Errorf("expected 2 5xx, got %d", ps.xx5)
	}
}

// TestReporter_Ingest_RecordsWaitingRoom verifies that visits with
// WaitingRoom.Detected=true increment the waitingRoom counter.
func TestReporter_Ingest_RecordsWaitingRoom(t *testing.T) {
	r := NewReporter(testConfig())

	ll := LifeLog{
		Visits: []Visit{
			{URL: "http://example.com/", StatusCode: 200,
				WaitingRoom: WaitingRoomMetric{Detected: true}},
			{URL: "http://example.com/", StatusCode: 200,
				WaitingRoom: WaitingRoomMetric{Detected: false}},
			{URL: "http://example.com/", StatusCode: 200,
				WaitingRoom: WaitingRoomMetric{Detected: true}},
		},
	}
	r.Ingest(ll)

	ps := r.byPath["http://example.com/"]
	if ps.waitingRoom != 2 {
		t.Errorf("expected 2 waiting room hits, got %d", ps.waitingRoom)
	}
}

// TestReporter_Ingest_RecordsErrors verifies that visits with a non-nil
// Error field increment the error counter for the path.
func TestReporter_Ingest_RecordsErrors(t *testing.T) {
	r := NewReporter(testConfig())

	ll := LifeLog{
		Visits: []Visit{
			{URL: "http://example.com/", StatusCode: 0,
				Error: fmt.Errorf("connection refused")},
			{URL: "http://example.com/", StatusCode: 200},
			{URL: "http://example.com/", StatusCode: 0,
				Error: fmt.Errorf("timeout")},
		},
	}
	r.Ingest(ll)

	ps := r.byPath["http://example.com/"]
	if ps.errors != 2 {
		t.Errorf("expected 2 errors, got %d", ps.errors)
	}
}

// TestReporter_Ingest_MultiplePathsSeparated verifies that visits to
// different paths are tracked in separate pathStats entries.
func TestReporter_Ingest_MultiplePathsSeparated(t *testing.T) {
	r := NewReporter(testConfig())

	ll := LifeLog{
		Visits: []Visit{
			{URL: "http://example.com/", StatusCode: 200},
			{URL: "http://example.com/about", StatusCode: 200},
			{URL: "http://example.com/", StatusCode: 200},
			{URL: "http://example.com/pricing", StatusCode: 200},
		},
	}
	r.Ingest(ll)

	if len(r.byPath) != 3 {
		t.Errorf("expected 3 path entries, got %d", len(r.byPath))
	}
	if r.byPath["http://example.com/"].Hits != 2 {
		t.Errorf("/ should have 2 hits")
	}
	if r.byPath["http://example.com/about"].Hits != 1 {
		t.Errorf("/about should have 1 hit")
	}
}

// TestReporter_Ingest_ConcurrentSafe verifies that concurrent Ingest calls
// from multiple goroutines do not produce data races. Run with -race.
func TestReporter_Ingest_ConcurrentSafe(t *testing.T) {
	r := NewReporter(testConfig())
	var wg sync.WaitGroup
	const goroutines = 50

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			ll := makeLifeLog(
				fmt.Sprintf("http://example.com/page-%d", n%5),
				10,
				http.StatusOK,
				false,
				nil,
			)
			r.Ingest(ll)
		}(i)
	}

	wg.Wait()

	// Verify total hits: 50 goroutines * 10 visits each = 500
	var total int64
	for _, ps := range r.byPath {
		total += ps.Hits
	}
	if total != 500 {
		t.Errorf("expected 500 total hits across all paths, got %d", total)
	}
}

// TestReporter_Ingest_EmptyLifeLog verifies that a LifeLog with no Visits
// is handled gracefully without panicking.
func TestReporter_Ingest_EmptyLifeLog(t *testing.T) {
	r := NewReporter(testConfig())
	r.Ingest(LifeLog{}) // must not panic
	if len(r.byPath) != 0 {
		t.Errorf("empty LifeLog should add no path entries, got %d", len(r.byPath))
	}
}

// TestReporter_Ingest_AccumulatesDurations verifies that visit durations
// are stored for percentile calculation.
func TestReporter_Ingest_AccumulatesDurations(t *testing.T) {
	r := NewReporter(testConfig())

	durations := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	}

	ll := LifeLog{
		Visits: []Visit{
			{URL: "http://example.com/", StatusCode: 200, Duration: durations[0]},
			{URL: "http://example.com/", StatusCode: 200, Duration: durations[1]},
			{URL: "http://example.com/", StatusCode: 200, Duration: durations[2]},
		},
	}
	r.Ingest(ll)

	ps := r.byPath["http://example.com/"]
	if len(ps.durations) != 3 {
		t.Fatalf("expected 3 durations, got %d", len(ps.durations))
	}
}

// ── percentile ────────────────────────────────────────────────────────────────

// TestPercentile_EmptySlice verifies that an empty slice returns 0.
func TestPercentile_EmptySlice(t *testing.T) {
	got := percentile([]time.Duration{}, 99)
	if got != 0 {
		t.Errorf("expected 0 for empty slice, got %v", got)
	}
}

// TestPercentile_SingleElement verifies that any percentile of a single
// element slice returns that element.
func TestPercentile_SingleElement(t *testing.T) {
	d := []time.Duration{42 * time.Millisecond}
	for _, p := range []int{1, 50, 90, 99, 100} {
		got := percentile(d, p)
		if got != 42*time.Millisecond {
			t.Errorf("percentile(%d) of single-element slice: expected 42ms, got %v", p, got)
		}
	}
}

// TestPercentile_P50_KnownDataset verifies p50 against a pre-sorted
// dataset with a known median.
func TestPercentile_P50_KnownDataset(t *testing.T) {
	// 10 elements — p50 should be element at index 4 (nearest rank)
	d := []time.Duration{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
	}
	got := percentile(d, 50)
	// nearest rank: ceil(0.5 * 10) = 5 → index 4 → value 5
	if got != 5 {
		t.Errorf("expected p50=5, got %v", got)
	}
}

// TestPercentile_P99_KnownDataset verifies p99 against a dataset where
// the 99th percentile value is predictable.
func TestPercentile_P99_KnownDataset(t *testing.T) {
	d := make([]time.Duration, 100)
	for i := range d {
		d[i] = time.Duration(i+1) * time.Millisecond
	}
	// nearest rank: ceil(0.99 * 100) = 99 → index 98 → value 99ms
	got := percentile(d, 99)
	if got != 99*time.Millisecond {
		t.Errorf("expected p99=99ms, got %v", got)
	}
}

// TestPercentile_P100_ReturnsMax verifies that p100 always returns
// the last (maximum) element.
func TestPercentile_P100_ReturnsMax(t *testing.T) {
	d := []time.Duration{1, 5, 10, 50, 100}
	got := percentile(d, 100)
	if got != 100 {
		t.Errorf("expected p100=100, got %v", got)
	}
}

// TestPercentile_P1_ReturnsMin verifies that p1 on a small dataset
// returns a value near the minimum.
func TestPercentile_P1_ReturnsMin(t *testing.T) {
	d := []time.Duration{1, 2, 3, 4, 5}
	got := percentile(d, 1)
	// nearest rank: ceil(0.01 * 5) = 1 → index 0 → value 1
	if got != 1 {
		t.Errorf("expected p1=1, got %v", got)
	}
}

// TestPercentile_PreSortedAssumption verifies that percentile requires
// a pre-sorted slice — documents the contract.
func TestPercentile_PreSortedAssumption(t *testing.T) {
	sorted := []time.Duration{10, 20, 30, 40, 50}
	p50sorted := percentile(sorted, 50)

	// The same values unsorted will produce wrong results —
	// this test documents that the caller is responsible for sorting.
	unsorted := []time.Duration{50, 10, 40, 20, 30}
	p50unsorted := percentile(unsorted, 50)

	// We don't assert they're equal — we document they may differ
	// to make the pre-sort contract visible in test output.
	t.Logf("p50 sorted=%v unsorted=%v (caller must pre-sort)", p50sorted, p50unsorted)
}

// ── buildReportData ───────────────────────────────────────────────────────────

// TestBuildReportData_TotalsCorrect verifies that buildReportData sums
// all visit metrics correctly across multiple LifeLogs.
func TestBuildReportData_TotalsCorrect(t *testing.T) {
	r := NewReporter(testConfig())

	// Ingest 3 lifelogs: 2 visits each, mix of status codes
	for i := 0; i < 3; i++ {
		r.Ingest(LifeLog{
			Visits: []Visit{
				{URL: "http://example.com/", StatusCode: 200,
					BytesIn: 1024, Duration: 10 * time.Millisecond},
				{URL: "http://example.com/about", StatusCode: 404,
					BytesIn: 512, Duration: 5 * time.Millisecond},
			},
		})
	}

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	if data.TotalVisits != 6 {
		t.Errorf("expected 6 total visits, got %d", data.TotalVisits)
	}
	if data.TotalBytes != (1024+512)*3 {
		t.Errorf("expected %d total bytes, got %d", (1024+512)*3, data.TotalBytes)
	}
	if data.Total2xx != 3 {
		t.Errorf("expected 3 2xx, got %d", data.Total2xx)
	}
	if data.Total4xx != 3 {
		t.Errorf("expected 3 4xx, got %d", data.Total4xx)
	}
}

// TestBuildReportData_PathsSortedByHits verifies that Paths are sorted
// by hit count in descending order.
func TestBuildReportData_PathsSortedByHits(t *testing.T) {
	r := NewReporter(testConfig())

	// /about gets 1 hit, / gets 3 hits, /pricing gets 2 hits
	r.Ingest(LifeLog{Visits: []Visit{
		{URL: "http://example.com/", StatusCode: 200},
		{URL: "http://example.com/", StatusCode: 200},
		{URL: "http://example.com/", StatusCode: 200},
		{URL: "http://example.com/pricing", StatusCode: 200},
		{URL: "http://example.com/pricing", StatusCode: 200},
		{URL: "http://example.com/about", StatusCode: 200},
	}})

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	if len(data.Paths) != 3 {
		t.Fatalf("expected 3 paths, got %d", len(data.Paths))
	}

	// Verify descending order
	for i := 1; i < len(data.Paths); i++ {
		if data.Paths[i].Hits > data.Paths[i-1].Hits {
			t.Errorf("paths not sorted: [%d].Hits=%d > [%d].Hits=%d",
				i, data.Paths[i].Hits, i-1, data.Paths[i-1].Hits)
		}
	}

	if data.Paths[0].URL != "http://example.com/" {
		t.Errorf("expected / to be first (most hits), got %q", data.Paths[0].URL)
	}
}

// TestBuildReportData_GlobalPercentiles verifies that the global timing
// percentiles are computed correctly from all visit durations.
func TestBuildReportData_GlobalPercentiles(t *testing.T) {
	r := NewReporter(testConfig())

	durations := make([]time.Duration, 100)
	for i := range durations {
		durations[i] = time.Duration(i+1) * time.Millisecond
	}

	visits := make([]Visit, 100)
	for i, d := range durations {
		visits[i] = Visit{
			URL:        "http://example.com/",
			StatusCode: 200,
			Duration:   d,
		}
	}
	r.Ingest(LifeLog{Visits: visits})

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	if data.Fastest != 1*time.Millisecond {
		t.Errorf("expected Fastest=1ms, got %v", data.Fastest)
	}
	if data.Slowest != 100*time.Millisecond {
		t.Errorf("expected Slowest=100ms, got %v", data.Slowest)
	}
	if data.P99 != 99*time.Millisecond {
		t.Errorf("expected P99=99ms, got %v", data.P99)
	}
}

// TestBuildReportData_PerPathPercentiles verifies that per-path p50 and
// p99 values are computed independently from other paths.
func TestBuildReportData_PerPathPercentiles(t *testing.T) {
	r := NewReporter(testConfig())

	// / has fast durations, /slow has slow durations
	visits := []Visit{}
	for i := 0; i < 10; i++ {
		visits = append(visits, Visit{
			URL: "http://example.com/", StatusCode: 200,
			Duration: time.Duration(i+1) * time.Millisecond,
		})
		visits = append(visits, Visit{
			URL: "http://example.com/slow", StatusCode: 200,
			Duration: time.Duration(i+1) * 100 * time.Millisecond,
		})
	}
	r.Ingest(LifeLog{Visits: visits})

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	var fastPath, slowPath *PathReport
	for i := range data.Paths {
		if data.Paths[i].URL == "http://example.com/" {
			fastPath = &data.Paths[i]
		}
		if data.Paths[i].URL == "http://example.com/slow" {
			slowPath = &data.Paths[i]
		}
	}

	if fastPath == nil || slowPath == nil {
		t.Fatal("expected both paths in report data")
	}
	if fastPath.P50 >= slowPath.P50 {
		t.Errorf("fast path p50 %v should be less than slow path p50 %v",
			fastPath.P50, slowPath.P50)
	}
}

// TestBuildReportData_TotalLemmings verifies that TotalLemmings is derived
// from config Terrain * Pack, not from observed data.
func TestBuildReportData_TotalLemmings(t *testing.T) {
	cfg := testConfig()
	cfg.Terrain = 5
	cfg.Pack = 10
	r := NewReporter(cfg)

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	if data.TotalLemmings != 50 {
		t.Errorf("expected TotalLemmings=50 (5*10), got %d", data.TotalLemmings)
	}
}

// TestBuildReportData_WaitingRoomTotal verifies that TotalWaitingRoom
// correctly sums waiting room hits across all paths.
func TestBuildReportData_WaitingRoomTotal(t *testing.T) {
	r := NewReporter(testConfig())
	r.Ingest(LifeLog{Visits: []Visit{
		{URL: "http://example.com/", StatusCode: 200,
			WaitingRoom: WaitingRoomMetric{Detected: true}},
		{URL: "http://example.com/about", StatusCode: 200,
			WaitingRoom: WaitingRoomMetric{Detected: true}},
		{URL: "http://example.com/", StatusCode: 200},
	}})

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	if data.TotalWaitingRoom != 2 {
		t.Errorf("expected TotalWaitingRoom=2, got %d", data.TotalWaitingRoom)
	}
}

// ── renderMarkdown ────────────────────────────────────────────────────────────

// TestRenderMarkdown_NonEmpty verifies that renderMarkdown produces
// a non-empty output for a valid ReportData.
func TestRenderMarkdown_NonEmpty(t *testing.T) {
	data := makeTestReportData()
	md, err := renderMarkdown(data)
	if err != nil {
		t.Fatalf("renderMarkdown error: %v", err)
	}
	if len(md) == 0 {
		t.Error("renderMarkdown returned empty string")
	}
}

// TestRenderMarkdown_ContainsHitURL verifies that the target URL appears
// in the markdown output.
func TestRenderMarkdown_ContainsHitURL(t *testing.T) {
	data := makeTestReportData()
	data.Config.Hit = "https://example.com/test"

	md, err := renderMarkdown(data)
	if err != nil {
		t.Fatalf("renderMarkdown error: %v", err)
	}
	if !strings.Contains(md, "https://example.com/test") {
		t.Error("markdown output should contain the hit URL")
	}
}

// TestRenderMarkdown_ContainsVersion verifies that the version string
// appears in the markdown output.
func TestRenderMarkdown_ContainsVersion(t *testing.T) {
	data := makeTestReportData()
	md, err := renderMarkdown(data)
	if err != nil {
		t.Fatalf("renderMarkdown error: %v", err)
	}
	if !strings.Contains(md, data.Version) {
		t.Errorf("markdown should contain version %q", data.Version)
	}
}

// TestRenderMarkdown_ContainsPathBreakdown verifies that per-path entries
// appear in the rendered markdown.
func TestRenderMarkdown_ContainsPathBreakdown(t *testing.T) {
	data := makeTestReportData()
	md, err := renderMarkdown(data)
	if err != nil {
		t.Fatalf("renderMarkdown error: %v", err)
	}
	for _, p := range data.Paths {
		if !strings.Contains(md, p.URL) {
			t.Errorf("markdown should contain path %q", p.URL)
		}
	}
}

// TestRenderMarkdown_ValidTable verifies that the markdown output contains
// properly formatted table separators.
func TestRenderMarkdown_ValidTable(t *testing.T) {
	data := makeTestReportData()
	md, err := renderMarkdown(data)
	if err != nil {
		t.Fatalf("renderMarkdown error: %v", err)
	}
	if !strings.Contains(md, "|---") {
		t.Error("markdown output should contain table separators")
	}
}

// ── renderHTML ────────────────────────────────────────────────────────────────

// TestRenderHTML_NonEmpty verifies that renderHTML produces a non-empty output.
func TestRenderHTML_NonEmpty(t *testing.T) {
	data := makeTestReportData()
	html, err := renderHTML(data)
	if err != nil {
		t.Fatalf("renderHTML error: %v", err)
	}
	if len(html) == 0 {
		t.Error("renderHTML returned empty string")
	}
}

// TestRenderHTML_ContainsHitURL verifies that the target URL appears
// in the HTML output.
func TestRenderHTML_ContainsHitURL(t *testing.T) {
	data := makeTestReportData()
	data.Config.Hit = "https://example.com/test"

	html, err := renderHTML(data)
	if err != nil {
		t.Fatalf("renderHTML error: %v", err)
	}
	if !strings.Contains(html, "https://example.com/test") {
		t.Error("HTML output should contain the hit URL")
	}
}

// TestRenderHTML_ValidDoctype verifies that the HTML output begins with
// a proper DOCTYPE declaration indicating a complete HTML document.
func TestRenderHTML_ValidDoctype(t *testing.T) {
	data := makeTestReportData()
	html, err := renderHTML(data)
	if err != nil {
		t.Fatalf("renderHTML error: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(html), "<!DOCTYPE html>") {
		t.Error("HTML output should begin with <!DOCTYPE html>")
	}
}

// TestRenderHTML_ContainsPathBreakdown verifies that per-path entries
// appear in the rendered HTML.
func TestRenderHTML_ContainsPathBreakdown(t *testing.T) {
	data := makeTestReportData()
	html, err := renderHTML(data)
	if err != nil {
		t.Fatalf("renderHTML error: %v", err)
	}
	for _, p := range data.Paths {
		if !strings.Contains(html, p.URL) {
			t.Errorf("HTML should contain path %q", p.URL)
		}
	}
}

// TestRenderHTML_SelfContained verifies that the HTML output contains
// no external resource references — it must be fully self-contained.
func TestRenderHTML_SelfContained(t *testing.T) {
	data := makeTestReportData()
	html, err := renderHTML(data)
	if err != nil {
		t.Fatalf("renderHTML error: %v", err)
	}

	// Must not reference external CDNs or stylesheets
	forbidden := []string{
		"cdn.jsdelivr.net",
		"cdnjs.cloudflare.com",
		"unpkg.com",
		`<link rel="stylesheet" href="http`,
		`<script src="http`,
	}
	for _, f := range forbidden {
		if strings.Contains(html, f) {
			t.Errorf("HTML must be self-contained but contains external reference: %q", f)
		}
	}
}

// ── resolveSavePath ───────────────────────────────────────────────────────────

// TestResolveSavePath_LocalPath verifies that a local save path is resolved
// to <saveTo>/lemmings/<domain>/.
func TestResolveSavePath_LocalPath(t *testing.T) {
	got, err := resolveSavePath("/tmp/reports", "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join("/tmp/reports", "lemmings", "example.com")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

// TestResolveSavePath_CurrentDir verifies that "." as saveTo produces
// a relative path starting with lemmings/.
func TestResolveSavePath_CurrentDir(t *testing.T) {
	got, err := resolveSavePath(".", "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, filepath.Join(".", "lemmings")) {
		t.Errorf("expected path starting with ./lemmings, got %q", got)
	}
}

// TestResolveSavePath_S3Prefix verifies that an s3:// prefix is preserved
// in the returned path.
func TestResolveSavePath_S3Prefix(t *testing.T) {
	got, err := resolveSavePath("s3://my-bucket/prefix", "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "s3://") {
		t.Errorf("expected s3:// prefix to be preserved, got %q", got)
	}
	if !strings.Contains(got, "example.com") {
		t.Errorf("expected domain in s3 path, got %q", got)
	}
}

// TestResolveSavePath_LocalhostPort verifies that a localhost:PORT hit URL
// produces a filesystem-safe directory name.
func TestResolveSavePath_LocalhostPort(t *testing.T) {
	got, err := resolveSavePath(".", "http://localhost:8080/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, ":") {
		t.Errorf("save path should not contain colon, got %q", got)
	}
}

// ── Reporter.Write ────────────────────────────────────────────────────────────

// TestReporterWrite_CreatesFiles verifies that Write produces both a
// .md and a .html file in the configured output directory.
func TestReporterWrite_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Hit = "http://localhost:8080/"
	cfg.SaveTo = dir

	r := NewReporter(cfg)
	r.Ingest(makeLifeLog("http://localhost:8080/", 5, http.StatusOK, false, nil))

	if err := r.Write(dir); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	domain := domainFromURL(cfg.Hit)
	date := time.Now().Format(reportDateFormat)

	mdPath := filepath.Join(dir, "lemmings", domain,
		fmt.Sprintf("lemmings.%s.%s.md", date, domain))
	htmlPath := filepath.Join(dir, "lemmings", domain,
		fmt.Sprintf("lemmings.%s.%s.html", date, domain))

	if _, err := os.Stat(mdPath); os.IsNotExist(err) {
		t.Errorf("expected markdown file at %s", mdPath)
	}
	if _, err := os.Stat(htmlPath); os.IsNotExist(err) {
		t.Errorf("expected HTML file at %s", htmlPath)
	}
}

// TestReporterWrite_CreatesDirectoryIfAbsent verifies that Write creates
// the output directory tree if it does not already exist.
func TestReporterWrite_CreatesDirectoryIfAbsent(t *testing.T) {
	base := t.TempDir()
	// Use a nested path that definitely does not exist yet
	dir := filepath.Join(base, "deep", "nested", "path")

	cfg := testConfig()
	cfg.Hit = "http://localhost:8080/"
	r := NewReporter(cfg)

	if err := r.Write(dir); err != nil {
		t.Fatalf("Write should create missing directories, got error: %v", err)
	}
}

// TestReporterWrite_FilesNonEmpty verifies that both output files contain
// meaningful content (not empty files).
func TestReporterWrite_FilesNonEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Hit = "http://localhost:8080/"

	r := NewReporter(cfg)
	r.Ingest(makeLifeLog("http://localhost:8080/", 3, http.StatusOK, false, nil))

	if err := r.Write(dir); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	domain := domainFromURL(cfg.Hit)
	date := time.Now().Format(reportDateFormat)
	destDir := filepath.Join(dir, "lemmings", domain)

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("could not read output directory: %v", err)
	}

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("could not stat %s: %v", entry.Name(), err)
		}
		if info.Size() == 0 {
			t.Errorf("output file %s is empty", entry.Name())
		}
		_ = date
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkReporter_Ingest measures Ingest throughput under concurrent
// load from 20 goroutines — representative of a large swarm completing.
//
//go:build bench
func BenchmarkReporter_Ingest_Concurrent(b *testing.B) {
	r := NewReporter(testConfig())
	ll := makeLifeLog("http://example.com/", 10, http.StatusOK, false, nil)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Ingest(ll)
		}
	})
}

// BenchmarkBuildReportData measures report data aggregation time with
// 10000 visits across 20 paths — a large but realistic run.
//
//go:build bench
func BenchmarkBuildReportData(b *testing.B) {
	r := NewReporter(testConfig())
	paths := make([]string, 20)
	for i := range paths {
		paths[i] = fmt.Sprintf("http://example.com/page-%d", i)
	}

	// Ingest 10000 visits spread across 20 paths
	visits := make([]Visit, 500)
	for i := range visits {
		visits[i] = Visit{
			URL:        paths[i%20],
			StatusCode: 200,
			BytesIn:    1024,
			Duration:   time.Duration(i) * time.Millisecond,
		}
	}
	for i := 0; i < 20; i++ {
		r.Ingest(LifeLog{Visits: visits})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.mu.Lock()
		r.buildReportData()
		r.mu.Unlock()
	}
}

// BenchmarkRenderMarkdown measures markdown rendering time for a report
// with 50 paths — representative of a large sitemap run.
//
//go:build bench
func BenchmarkRenderMarkdown(b *testing.B) {
	data := makeLargeReportData(50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := renderMarkdown(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRenderHTML measures HTML rendering time for a report with
// 50 paths.
//
//go:build bench
func BenchmarkRenderHTML(b *testing.B) {
	data := makeLargeReportData(50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := renderHTML(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPercentile_1M measures percentile computation on a 1M duration
// slice — worst case for a very long run with many lemmings.
//
//go:build bench
func BenchmarkPercentile_1M(b *testing.B) {
	d := make([]time.Duration, 1_000_000)
	for i := range d {
		d[i] = time.Duration(i) * time.Millisecond
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		percentile(d, 99)
	}
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// makeLifeLog constructs a LifeLog with n identical visits to the given URL.
// All visits have the provided status code. Set waitingRoom=true to mark
// all visits as waiting room hits. Set err non-nil to set Visit.Error.
func makeLifeLog(url string, n int, statusCode int, waitingRoom bool, err error) LifeLog {
	visits := make([]Visit, n)
	for i := range visits {
		visits[i] = Visit{
			URL:        url,
			StatusCode: statusCode,
			BytesIn:    512,
			Duration:   10 * time.Millisecond,
			Timestamp:  time.Now(),
			WaitingRoom: WaitingRoomMetric{
				Detected: waitingRoom,
			},
			Error: err,
		}
	}
	return LifeLog{
		Visits:   visits,
		BornAt:   time.Now(),
		DiedAt:   time.Now(),
		Duration: time.Duration(n) * 10 * time.Millisecond,
	}
}

// makeTestReportData returns a minimal but valid ReportData for template tests.
func makeTestReportData() ReportData {
	return ReportData{
		Version:       "test",
		GeneratedAt:   time.Now(),
		Duration:      30 * time.Second,
		Config:        testConfig(),
		TotalLemmings: 100,
		TotalVisits:   1000,
		TotalBytes:    1024 * 1024,
		Total2xx:      950,
		Total3xx:      30,
		Total4xx:      15,
		Total5xx:      5,
		TotalWaitingRoom: 10,
		Fastest:       1 * time.Millisecond,
		Slowest:       500 * time.Millisecond,
		P50:           20 * time.Millisecond,
		P90:           100 * time.Millisecond,
		P95:           200 * time.Millisecond,
		P99:           400 * time.Millisecond,
		Paths: []PathReport{
			{
				URL:   "http://localhost:8080/",
				Hits:  600,
				Bytes: "600 KB",
				xx2:   580,
				xx4:   15,
				xx5:   5,
				P50:   18 * time.Millisecond,
				P99:   380 * time.Millisecond,
			},
			{
				URL:         "http://localhost:8080/about",
				Hits:        400,
				Bytes:       "400 KB",
				xx2:         370,
				xx3:         30,
				WaitingRoom: 10,
				P50:         22 * time.Millisecond,
				P99:         420 * time.Millisecond,
			},
		},
	}
}

// makeLargeReportData returns a ReportData with n paths for benchmark use.
func makeLargeReportData(n int) ReportData {
	data := makeTestReportData()
	data.Paths = make([]PathReport, n)
	for i := range data.Paths {
		data.Paths[i] = PathReport{
			URL:   fmt.Sprintf("http://localhost:8080/page-%d", i),
			Hits:  int64(100 - i),
			Bytes: "100 KB",
			xx2:   int64(90 - i),
			xx4:   int64(i),
			P50:   time.Duration(i+1) * time.Millisecond,
			P99:   time.Duration(i+10) * time.Millisecond,
		}
	}
	return data
}
