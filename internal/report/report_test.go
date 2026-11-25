package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

func TestHistogram_AccuracyAndQuantiles(t *testing.T) {
	var h Histogram
	for i := 1; i <= 1000; i++ {
		h.Add(time.Duration(i) * time.Millisecond)
	}
	p50 := h.Quantile(50)
	p95 := h.Quantile(95)
	p99 := h.Quantile(99)
	if p50 < 480*time.Millisecond || p50 > 520*time.Millisecond {
		t.Errorf("p50 out of 2%% bound: %s", p50)
	}
	if p95 < 920*time.Millisecond || p95 > 980*time.Millisecond {
		t.Errorf("p95 out of 2%% bound: %s", p95)
	}
	if p99 < 960*time.Millisecond || p99 > 1020*time.Millisecond {
		t.Errorf("p99 out of 2%% bound: %s", p99)
	}
}

func TestTraceWriter_PermissionsAndNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")

	tw, err := NewTraceWriter(path)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	tw.Record(&swarm.Visit{URL: "http://127.0.0.1/a", StatusCode: 200, Duration: 10 * time.Millisecond})
	tw.Close()

	// Must refuse to overwrite existing file (O_EXCL).
	if _, err := NewTraceWriter(path); err == nil {
		t.Error("NewTraceWriter must refuse overwriting existing trace file")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("trace file mode = %04o, want 0600", info.Mode().Perm())
		}
	}
}

func TestRenderData_HybridForensicsPackagingAndSecretRedaction(t *testing.T) {
	smtpPass := secrets.New("REAL_SMTP_SENTINEL_999")
	cfg := swarm.Config{
		Hit:          "http://127.0.0.1:8080/",
		Terrain:      1,
		Pack:         2,
		Limit:        2,
		Until:        5 * time.Second,
		Ramp:         time.Second,
		Version:      "2.0.0",
		SMTPPass:     smtpPass,
		UniqueReport: true,
	}

	store := forensics.NewStore(10, 3)
	fakeJPG := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x42}, 128)...)
	sum := store.Save(forensics.Artifact{
		Fingerprint:   forensics.Fingerprint("/api/tokens", 500, "status 500"),
		CapturedAt:    time.Now(),
		URL:           "http://127.0.0.1:8080/api/tokens",
		Path:          "/api/tokens",
		StatusCode:    500,
		TriggerReason: "status 500",
		CaptureMode:   forensics.ModeCDPReplay,
		ConsoleErrors: 1,
		Console: []forensics.ConsoleEntry{
			{Level: "error", SourceURL: "http://127.0.0.1:8080/app.js", Text: "Hopper coil fault"},
		},
		Session: forensics.SessionSnapshot{
			LemmingNumber: 4,
			LemmingID:     "t0-p3",
			LemmingName:   "Turbo Lemming",
			VisitSequence: 2,
			CookieJar: forensics.CookieJarSnapshot{
				TotalCookies: 1,
				Cookies: []forensics.CookieStatus{
					{Name: "session", Domain: "127.0.0.1", Path: "/", HTTPOnly: true, Fingerprint: "fp1234", RawValue: "COOKIE_RAW_SECRET_DO_NOT_LEAK"},
				},
			},
		},
		ErrorBody: journey.ExtractedError{
			Format:    "json",
			Summary:   "Coin Hopper Fault",
			ErrorCode: "E_HOPPER_JAM",
			RequestID: "req-999",
		},
		Screenshot: &forensics.Screenshot{
			MIMEType: "image/jpeg",
			Width:    1280,
			Height:   800,
			Bytes:    len(fakeJPG),
			Data:     fakeJPG,
		},
	})
	if _, err := store.Review(sum.ID, forensics.ReviewBug, "Needs hotfix before release"); err != nil {
		t.Fatal(err)
	}

	rep := New(cfg)
	rep.SetForensics(store)
	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1:8080/api/tokens", StatusCode: 500, Failed: true, Duration: 25 * time.Millisecond})

	rendered, err := rep.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Verify standalone asset generated.
	if len(rendered.Artifacts) != 1 || !strings.HasSuffix(rendered.Artifacts[0].Filename, ".jpg") {
		t.Fatalf("expected 1 standalone jpg asset, got %+v", rendered.Artifacts)
	}
	// Verify HTML embeds inline base64 data URI and admin triage note.
	if !strings.Contains(rendered.HTML, "data:image/jpeg;base64,") {
		t.Error("HTML report missing inline screenshot data URI")
	}
	if !strings.Contains(rendered.HTML, "Needs hotfix before release") {
		t.Error("HTML report missing admin triage note")
	}
	// Verify Markdown links relative artifact file (<filename>.artifacts/<id>.jpg).
	wantRel := rendered.Filename + ".artifacts/" + sum.ID + ".jpg"
	if !strings.Contains(rendered.Markdown, wantRel) {
		t.Errorf("Markdown report missing relative artifact image link %q", wantRel)
	}
	// Verify JSON has image_path, triage status, and NO base64 / NO secrets / NO raw cookies.
	allOutputs := rendered.HTML + rendered.HTMLForEmail + rendered.Markdown + string(rendered.JSON)
	for _, forbidden := range []string{"REAL_SMTP_SENTINEL_999", "COOKIE_RAW_SECRET_DO_NOT_LEAK"} {
		if strings.Contains(allOutputs, forbidden) {
			t.Fatalf("SECURITY VIOLATION: secret %q leaked into rendered report!", forbidden)
		}
	}
	var parsed Data
	if err := json.Unmarshal(rendered.JSON, &parsed); err != nil {
		t.Fatalf("JSON report unmarshal: %v", err)
	}
	if len(parsed.Forensics.Artifacts) != 1 || parsed.Forensics.Artifacts[0].ReviewStatus != forensics.ReviewBug {
		t.Errorf("JSON forensics triage mismatch: %+v", parsed.Forensics)
	}
	if parsed.Forensics.Artifacts[0].Screenshot == nil || parsed.Forensics.Artifacts[0].Screenshot.RelativePath != wantRel {
		t.Errorf("JSON screenshot relative path = %+v, want %q", parsed.Forensics.Artifacts[0].Screenshot, wantRel)
	}
}

func TestCompare_RegressionAndFingerprints(t *testing.T) {
	oldData := Data{
		Config:         swarm.Config{Hit: "http://127.0.0.1:8080/"},
		RescuedPercent: 100.0,
		FailureRate:    0.0,
		P95:            100 * time.Millisecond,
		Paths:          []PathReport{{URL: "/checkout", Hits: 50, P95: 100 * time.Millisecond}},
		Forensics: ForensicsReport{
			Artifacts: []forensics.Artifact{{Fingerprint: "fp-old", Path: "/legacy", TriggerReason: "status 404"}},
		},
	}
	newData := Data{
		Config:         swarm.Config{Hit: "http://127.0.0.1:8080/"},
		RescuedPercent: 80.0,
		FailureRate:    0.15,
		P95:            450 * time.Millisecond,
		Paths:          []PathReport{{URL: "/checkout", Hits: 50, P95: 450 * time.Millisecond, Failed: 8}},
		Forensics: ForensicsReport{
			Artifacts: []forensics.Artifact{{Fingerprint: "fp-new", Path: "/checkout", TriggerReason: "status 500"}},
		},
	}

	res := Compare(oldData, newData, CompareOptions{
		MaxFailureDelta: 0.05,
		MaxP95DeltaMS:   100,
		MaxRescuedDrop:  5,
	})
	if !res.Regressed {
		t.Fatal("expected regression to trip gates")
	}
	if len(res.NewFingerprints) != 1 || len(res.ResolvedFingerprints) != 1 {
		t.Errorf("fingerprint diff mismatch: new=%v resolved=%v", res.NewFingerprints, res.ResolvedFingerprints)
	}
	if len(res.Gates) != 3 {
		t.Errorf("expected 3 evaluated gates, got %d", len(res.Gates))
	}
}

func FuzzFormatInt(f *testing.F) {
	for _, n := range []int64{0, -1, 1000, -1000, 1234567890, -9223372036854775808} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, n int64) {
		got := FormatInt(n)
		if strings.HasPrefix(got, "-,") {
			t.Fatalf("malformed negative separator: FormatInt(%d) = %q", n, got)
		}
	})
}

func FuzzFormatBytes(f *testing.F) {
	for _, n := range []int64{0, 512, 1024, 1 << 20, 1 << 30} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, n int64) {
		if FormatBytes(n) == "" {
			t.Fatalf("empty FormatBytes(%d)", n)
		}
	})
}

func TestLatencySeries_ExactThenBoundedHistogram(t *testing.T) {
	ls := LatencySeries{exactCap: 10}
	for i := 1; i <= 10; i++ {
		ls.Add(time.Duration(i) * time.Millisecond)
	}
	if !ls.IsExact() {
		t.Fatal("expected IsExact=true at exactCap")
	}
	if got := ls.Quantile(90); got != 9*time.Millisecond {
		t.Errorf("exact p90 = %s, want 9ms", got)
	}

	// Exceed exactCap -> switches cleanly to bounded log histogram (within 2%).
	for i := 11; i <= 500; i++ {
		ls.Add(time.Duration(i) * time.Millisecond)
	}
	if ls.IsExact() {
		t.Fatal("expected IsExact=false after exceeding exactCap")
	}
	if len(ls.durations) != 10 {
		t.Errorf("exact buffer grew past cap: len=%d", len(ls.durations))
	}
	p95 := ls.Quantile(95)
	if p95 < 460*time.Millisecond || p95 > 490*time.Millisecond {
		t.Errorf("histogram p95 out of 2%% bound: %s", p95)
	}
}

func TestTimeline_CompactionAndAliveCounts(t *testing.T) {
	tl := NewTimeline()
	// Span > maxTimelineBuckets (600s) to trigger compaction multiple times.
	for sec := 0; sec < 1500; sec++ {
		b := tl.bucket(time.Duration(sec) * time.Second)
		b.Visits++
		b.Status[2]++
		if sec%10 == 0 {
			b.Born++
		}
		if sec%10 == 5 {
			b.Died++
		}
	}
	pts := tl.Points(0)
	if len(pts) > maxTimelineBuckets {
		t.Fatalf("timeline exceeded maxTimelineBuckets: %d > %d", len(pts), maxTimelineBuckets)
	}
	var visits int64
	for _, p := range pts {
		visits += p.Visits
	}
	if visits != 1500 {
		t.Errorf("compaction lost visits: got %d, want 1500", visits)
	}
}

func TestRenderHTML_EscapesUntrustedContent(t *testing.T) {
	const xss = `<script>alert("xss")</script>`
	store := forensics.NewStore(10, 3)
	sum := store.Save(forensics.Artifact{
		Fingerprint:   "fp-xss",
		URL:           "http://127.0.0.1/" + xss,
		Path:          "/" + xss,
		StatusCode:    500,
		TriggerReason: "status 500",
		CaptureMode:   forensics.ModeHTTPOnly,
		Console:       []forensics.ConsoleEntry{{Level: "error", Text: xss}},
		ConsoleErrors: 1,
		ErrorBody:     journey.ExtractedError{Format: "html", Headline: xss, Summary: xss, BodyExcerpt: xss},
	})
	_, _ = store.Review(sum.ID, forensics.ReviewBug, xss)

	rep := New(swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second, Version: "2.0.0"})
	rep.SetForensics(store)
	rep.RecordVisit(&swarm.Visit{
		URL:        "http://127.0.0.1/" + xss,
		StatusCode: 500,
		Failed:     true,
		Page:       journey.PageEvidence{Title: xss},
		Duration:   10 * time.Millisecond,
	})

	rendered, err := rep.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(rendered.HTML, xss) {
		t.Fatal("XSS payload appeared unescaped in rendered HTML report")
	}
}

func TestReporter_LiveNotableAndVerdicts(t *testing.T) {
	cfg := swarm.Config{
		Hit:            "http://127.0.0.1/",
		Terrain:        1,
		Pack:           3,
		Limit:          3,
		Until:          5 * time.Second,
		FailureGate:    true,
		MaxFailureRate: 0.05,
		P95Budget:      50 * time.Millisecond,
	}
	rep := New(cfg)
	now := time.Now()

	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1/fast", StatusCode: 200, Duration: 10 * time.Millisecond})
	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1/slow", StatusCode: 500, Failed: true, Duration: 250 * time.Millisecond})

	rep.Ingest(swarm.LifeLog{
		Identity:    swarm.Identity{Number: 1, ID: "a", Name: "Explorer"},
		Streamed:    true,
		TotalVisits: 10,
		Duration:    time.Second,
	})
	rep.Ingest(swarm.LifeLog{
		Identity:       swarm.Identity{Number: 2, ID: "b", Name: "Patient Zero"},
		Streamed:       true,
		TotalVisits:    2,
		FailedVisits:   1,
		FirstFailureAt: now.Add(-time.Second),
		SlowestVisit:   250 * time.Millisecond,
		LongestWait:    2 * time.Second,
		Duration:       3 * time.Second,
	})

	live := rep.Live(10, 5)
	if live.Visits != 2 || live.Failed != 1 || live.Sessions != 2 || live.Rescued != 1 {
		t.Errorf("unexpected Live snapshot: %+v", live)
	}

	res := rep.Result()
	if res.Verdict != "FAILED" || !res.GatesFailed() || len(res.Gates) != 2 {
		t.Errorf("expected FAILED verdict with 2 gates, got verdict=%q gates=%+v", res.Verdict, res.Gates)
	}
	if len(res.Notable) < 3 {
		t.Errorf("expected Explorer, Patient Zero, Unlucky, Patient notable lives, got %d", len(res.Notable))
	}
	if line := RescueLine(res); line == "" {
		t.Error("expected non-empty RescueLine")
	}
}

type funcTarget struct {
	name string
	fn   func(context.Context, Rendered) error
}

func (f funcTarget) Name() string                                  { return f.name }
func (f funcTarget) Deliver(ctx context.Context, r Rendered) error { return f.fn(ctx, r) }

func TestReporter_WritePartialFailureAndCancel(t *testing.T) {
	rep := New(swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second})
	var delivered atomic.Int64
	rep.AddTarget(funcTarget{name: "broken", fn: func(context.Context, Rendered) error { return errors.New("disk full") }})
	rep.AddTarget(funcTarget{name: "healthy", fn: func(context.Context, Rendered) error { delivered.Add(1); return nil }})

	if err := rep.Write(context.Background()); err == nil {
		t.Error("expected error when one target fails")
	}
	if delivered.Load() != 1 {
		t.Errorf("healthy target should still receive report despite peer failure, got %d", delivered.Load())
	}

	// Context cancellation aborts slow target cleanly.
	rep2 := New(swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second})
	rep2.AddTarget(funcTarget{name: "slow", fn: func(ctx context.Context, _ Rendered) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rep2.Write(cctx); !errors.Is(err, context.Canceled) && err == nil {
		t.Errorf("expected cancellation error, got %v", err)
	}
}

func BenchmarkReporter_RecordVisit(b *testing.B) {
	rep := New(swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second})
	v := &swarm.Visit{URL: "http://127.0.0.1/games/outrun", StatusCode: 200, BytesIn: 4096, Duration: 12 * time.Millisecond}
	b.ResetTimer()
	for range b.N {
		rep.RecordVisit(v)
	}
}

func TestHistogram_HalfPercentPrecisionAndClamping(t *testing.T) {
	if got := unsafe.Sizeof(Histogram{}.bins); got != 32<<10 {
		t.Fatalf("Histogram.bins size = %d bytes, want 32768 (4096 * uint64)", got)
	}

	// Verify <= 0.5% relative error across single-point and mixed distributions from 1ms..60s.
	probes := []time.Duration{
		1 * time.Millisecond,
		2500 * time.Microsecond,
		10 * time.Millisecond,
		37 * time.Millisecond,
		123 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		3750 * time.Millisecond,
		15 * time.Second,
		45 * time.Second,
		60 * time.Second,
	}
	for _, target := range probes {
		var h Histogram
		// Bracket target with extreme low/high values so min/max clamping does not mask bin error.
		h.Add(100 * time.Microsecond)
		for range 100 {
			h.Add(target)
		}
		h.Add(120 * time.Second)
		got := h.Quantile(50)
		relErr := math.Abs(float64(got-target)) / float64(target)
		if relErr > 0.005 {
			t.Errorf("target=%s: Quantile(50)=%s relative error %.4f%% > 0.5%%", target, got, relErr*100)
		}
	}

	// Dense uniform distribution across 1ms..60s (10,000 samples).
	var dense Histogram
	const n = 10000
	sorted := make([]time.Duration, n)
	for i := range n {
		d := time.Millisecond + time.Duration(i)*(60*time.Second-time.Millisecond)/(n-1)
		sorted[i] = d
		dense.Add(d)
	}
	for _, p := range []int{50, 90, 95, 99} {
		want := Percentile(sorted, p)
		got := dense.Quantile(float64(p))
		relErr := math.Abs(float64(got-want)) / float64(want)
		if relErr > 0.005 {
			t.Errorf("uniform 1ms..60s p%d: got=%s want=%s relErr=%.4f%% > 0.5%%", p, got, want, relErr*100)
		}
	}

	// Values beyond max bin and negative values must clamp cleanly without panic/overflow.
	var extreme Histogram
	extreme.Add(-5 * time.Millisecond)
	extreme.Add(10 * time.Millisecond)
	extreme.Add(10 * time.Hour)
	extreme.Add(100 * time.Hour)
	if extreme.min != 0 {
		t.Errorf("expected negative duration clamped to 0, got %s", extreme.min)
	}
	if p95 := extreme.Quantile(95); p95 <= 0 || p95 > extreme.max {
		t.Errorf("clamped p95 out of valid bounds: %s (max=%s)", p95, extreme.max)
	}
	if p100 := extreme.Quantile(100); p100 != 100*time.Hour {
		t.Errorf("p100 = %s, want 100h", p100)
	}
}

func TestReporter_CancelledVisitsExcludedFromStatusCodes(t *testing.T) {
	rep := New(swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 2, Limit: 2, Until: time.Second})
	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1/ok", StatusCode: 200, BytesIn: 100, Duration: 10 * time.Millisecond})
	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1/ok", StatusCode: 200, BytesIn: 50, Cancelled: true, Duration: 5 * time.Millisecond})
	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1/err", StatusCode: 500, BytesIn: 25, Cancelled: true, Failed: true, Banned: true, Duration: 8 * time.Millisecond})
	rep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1/err", StatusCode: 403, BytesIn: 10, Cancelled: true, Banned: true})

	d := rep.Result()
	if d.TotalVisits != 4 || d.CancelledVisits != 3 || d.FailedVisits != 0 {
		t.Fatalf("visit counts mismatch: total=%d cancelled=%d failed=%d", d.TotalVisits, d.CancelledVisits, d.FailedVisits)
	}
	if d.Total2xx != 1 || d.Total4xx != 0 || d.Total5xx != 0 {
		t.Errorf("status totals polluted by cancelled visits: 2xx=%d 4xx=%d 5xx=%d", d.Total2xx, d.Total4xx, d.Total5xx)
	}
	if len(d.StatusCodes) != 1 || d.StatusCodes[0].Label != "200" || d.StatusCodes[0].Count != 1 {
		t.Errorf("StatusCodes polluted by cancelled visits: %+v", d.StatusCodes)
	}
	if d.Concert.Bans403 != 0 || d.Concert.Bans429 != 0 {
		t.Errorf("cancelled visits must not count as bans: %+v", d.Concert)
	}
	if d.TotalBytes != 185 {
		t.Errorf("TotalBytes = %d, want 185", d.TotalBytes)
	}
}

func TestReporter_AssetsQueueConcertScoutsAndTraces(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")
	tw, err := NewTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}

	bus := events.NewBus()
	defer bus.Close()

	start := time.Now().Add(-10 * time.Second)
	cfg := swarm.Config{
		Hit:           "http://127.0.0.1:8080/",
		Terrain:       2,
		Pack:          5,
		Limit:         10,
		Until:         10 * time.Second,
		Assets:        true,
		AssetsPerPage: 8,
		Scouts:        1,
		Version:       "2.0.0",
		TraceFile:     tracePath,
	}
	rep := New(cfg)
	rep.MarkStarted(start)
	rep.SetTrace(tw)
	rep.Attach(bus)

	// 1. Emit 3 QueuePoll events and then an admitted waiting-room page visit with Polls=3.
	// Room.TotalPolls must be 3 (never double-counted to 6).
	for i := 1; i <= 3; i++ {
		bus.Emit(events.Event{Kind: events.QueuePoll, URL: "http://127.0.0.1:8080/queue/status", StatusCode: 200, QueuePolls: i})
	}
	admittedVisit := &swarm.Visit{
		LemmingID:     "lem-1",
		LemmingNumber: 1,
		Sequence:      1,
		URL:           "http://127.0.0.1:8080/",
		StatusCode:    200,
		BytesIn:       2048,
		Duration:      2*time.Second + 20*time.Millisecond,
		Latency:       20 * time.Millisecond,
		Timestamp:     start.Add(3 * time.Second),
		FinishedAt:    start.Add(5 * time.Second),
		QueuePolls:    3,
		ConcertAdmit:  true,
		RoomTicket:    true,
		WaitingRoom: swarm.WaitingRoomMetric{
			Detected: true,
			Position: 14,
			Duration: 2 * time.Second,
			Polls:    3,
			Admitted: true,
		},
	}
	bus.Emit(events.Event{Kind: events.VisitComplete, Payload: admittedVisit})

	// 2. Waiting-room expired, denied (403), died-in-line, breaker 503 (without Detected), and non-queue 429 ban.
	bus.Emit(events.Event{Kind: events.VisitError, Payload: &swarm.Visit{
		LemmingID:  "lem-2",
		URL:        "http://127.0.0.1:8080/",
		StatusCode: 200,
		Failed:     true,
		Error:      errors.New("ticket expired"),
		Duration:   30 * time.Millisecond,
		FinishedAt: start.Add(6 * time.Second),
		RoomTicket: true,
		WaitingRoom: swarm.WaitingRoomMetric{
			Detected: true,
			Position: 9,
			Duration: time.Second,
			Expired:  true,
		},
	}})
	bus.Emit(events.Event{Kind: events.VisitError, Payload: &swarm.Visit{
		LemmingID:  "lem-3",
		URL:        "http://127.0.0.1:8080/",
		StatusCode: 403,
		Failed:     true,
		Banned:     true,
		Duration:   15 * time.Millisecond,
		FinishedAt: start.Add(6 * time.Second),
		WaitingRoom: swarm.WaitingRoomMetric{
			Detected: true,
			Position: 5,
			Duration: 500 * time.Millisecond,
			Denied:   true,
		},
	}})
	bus.Emit(events.Event{Kind: events.VisitError, Payload: &swarm.Visit{
		LemmingID:  "lem-4",
		URL:        "http://127.0.0.1:8080/",
		StatusCode: 0,
		Cancelled:  true,
		Error:      context.DeadlineExceeded,
		FinishedAt: start.Add(7 * time.Second),
		WaitingRoom: swarm.WaitingRoomMetric{
			Detected: true,
			Position: 20,
			Duration: 4 * time.Second,
		},
	}})
	bus.Emit(events.Event{Kind: events.VisitError, Payload: &swarm.Visit{
		LemmingID:   "lem-5",
		URL:         "http://127.0.0.1:8080/",
		StatusCode:  503,
		Failed:      true,
		Duration:    10 * time.Millisecond,
		FinishedAt:  start.Add(7 * time.Second),
		WaitingRoom: swarm.WaitingRoomMetric{Rejected503: true},
	}})
	bus.Emit(events.Event{Kind: events.VisitError, Payload: &swarm.Visit{
		LemmingID:  "lem-6",
		URL:        "http://127.0.0.1:8080/",
		StatusCode: 429,
		Failed:     true,
		Banned:     true,
		Duration:   12 * time.Millisecond,
		FinishedAt: start.Add(8 * time.Second),
	}})

	// 3. Emit AssetComplete (with *swarm.AssetVisit payload) and AssetError (scalar fallback + unknown class).
	// Use huge asset latencies (500ms, 600ms) to verify page P95 stays completely isolated!
	cssAsset := &swarm.AssetVisit{
		LemmingID:     "lem-1",
		LemmingNumber: 1,
		PageURL:       "http://127.0.0.1:8080/",
		URL:           "http://127.0.0.1:8080/assets/app.css",
		Class:         "style",
		StatusCode:    200,
		BytesIn:       4096,
		Duration:      500 * time.Millisecond,
		Timestamp:     start.Add(5 * time.Second),
	}
	bus.Emit(events.Event{
		Kind:       events.AssetComplete,
		AssetClass: "style",
		StatusCode: 200,
		BytesIn:    4096,
		Duration:   500 * time.Millisecond,
		AdmitPass:  true,
		Payload:    cssAsset,
	})
	bus.Emit(events.Event{
		Kind:          events.AssetError,
		LemmingID:     "lem-1",
		LemmingNumber: 1,
		URL:           "http://127.0.0.1:8080/assets/font.woff2",
		AssetClass:    "font",
		StatusCode:    403,
		BytesIn:       128,
		Duration:      600 * time.Millisecond,
		Failed:        true,
		Banned:        true,
	})
	bus.Emit(events.Event{
		Kind:       events.AssetComplete,
		LemmingID:  "lem-1",
		URL:        "http://127.0.0.1:8080/assets/blob.bin",
		AssetClass: "unrecognized-custom",
		StatusCode: 200,
		BytesIn:    64,
		Duration:   450 * time.Millisecond,
	})

	// 4. Emit ScoutVisit events (1 ok, 1 failed).
	bus.Emit(events.Event{
		Kind:          events.ScoutVisit,
		LemmingID:     "scout-1",
		LemmingNumber: 99,
		URL:           "http://127.0.0.1:8080/",
		StatusCode:    200,
		Duration:      80 * time.Millisecond,
	})
	bus.Emit(events.Event{
		Kind:          events.ScoutVisit,
		LemmingID:     "scout-1",
		LemmingNumber: 99,
		URL:           "http://127.0.0.1:8080/broken",
		StatusCode:    500,
		Duration:      90 * time.Millisecond,
		Failed:        true,
	})

	var swMetrics swarm.Metrics
	swMetrics.LemmingsStarted.Store(7)
	swMetrics.LemmingsCompleted.Store(5)
	swMetrics.DroppedLogs.Store(2)
	rep.MarkEnded(start.Add(10*time.Second), &swMetrics)
	rep.CloseTrace()

	d := rep.Result()
	if d.SchemaVersion != "2.0" {
		t.Errorf("SchemaVersion = %q, want 2.0", d.SchemaVersion)
	}
	if d.TotalLemmings != 10 || d.LemmingsStarted != 7 || d.LemmingsCompleted != 5 || d.LemmingsUnborn != 3 {
		t.Errorf("lemming counts = total:%d started:%d completed:%d unborn:%d",
			d.TotalLemmings, d.LemmingsStarted, d.LemmingsCompleted, d.LemmingsUnborn)
	}
	if d.TotalVisits != 6 {
		t.Errorf("TotalVisits = %d, want 6 (assets and scouts must not mix into pages)", d.TotalVisits)
	}
	if d.P95 >= 100*time.Millisecond {
		t.Errorf("page P95 = %s, polluted by 450-600ms asset latencies!", d.P95)
	}
	if d.Assets.Requests != 3 || d.Assets.Failed != 1 || d.Assets.Denied403 != 1 || d.Assets.Bytes != 4096+128+64 {
		t.Errorf("unexpected Assets summary: %+v", d.Assets)
	}
	if d.Assets.P95 < 450*time.Millisecond {
		t.Errorf("expected Assets.P95 >= 450ms, got %s", d.Assets.P95)
	}
	if len(d.Assets.ByClass) != 3 {
		t.Errorf("expected 3 asset classes (font, other, style), got %+v", d.Assets.ByClass)
	}
	if d.Room.Held != 4 || d.Room.Admitted != 1 || d.Room.Expired != 1 || d.Room.Denied != 1 ||
		d.Room.DiedInLine != 1 || d.Room.Rejected503 != 1 || d.Room.TotalPolls != 3 || d.Room.MaxPosition != 20 {
		t.Errorf("unexpected RoomReport: %+v", d.Room)
	}
	if d.Concert.AdmitPasses != 1 || d.Concert.RoomTickets != 2 || d.Concert.Bans429 != 1 || d.Concert.Bans403 != 1 {
		t.Errorf("unexpected ConcertReport: %+v", d.Concert)
	}
	if d.Scouts.Visits != 2 || d.Scouts.Failures != 1 {
		t.Errorf("unexpected ScoutReport: %+v", d.Scouts)
	}

	// Verify timeline bucketing placed the admitted visit at offset 5s (FinishedAt), not 0s.
	if len(d.Timeline) <= 5 || d.Timeline[5].Visits < 1 {
		t.Errorf("expected timeline bucket at t=5s from FinishedAt, got len=%d", len(d.Timeline))
	}

	// Render HTML and Markdown and check all new sections + warning banner.
	rendered, err := rep.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"Data Integrity Warning",
		"Same-origin assets",
		"Waiting room",
		"Concert passes",
		"Browser scouts",
		"5 completed",
		"3 unborn",
	} {
		if !strings.Contains(rendered.HTML, want) {
			t.Errorf("HTML missing expected section/text %q", want)
		}
	}
	for _, want := range []string{
		"WARNING — DATA INTEGRITY",
		"## same-origin assets",
		"## waiting room",
		"## concert passes & bans",
		"## browser scouts",
		"7 started, 5 completed, 3 never started",
		"schema 2.0",
	} {
		if !strings.Contains(rendered.Markdown, want) {
			t.Errorf("Markdown missing expected section/text %q", want)
		}
	}

	// Inspect JSONL trace lines for page, asset, and scout records.
	f, err := os.Open(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	kinds := map[string]int{}
	var sawAdmittedPage, sawAsset403, sawScoutFailed bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec VisitTrace
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("unmarshal trace line: %v", err)
		}
		kinds[rec.Kind]++
		if rec.Kind == "page" && rec.QueueAdmitted && rec.QueuePolls == 3 && rec.ConcertAdmit && rec.RoomTicket {
			sawAdmittedPage = true
		}
		if rec.Kind == "asset" && rec.AssetClass == "font" && rec.Banned && rec.Status == 403 {
			sawAsset403 = true
		}
		if rec.Kind == "scout" && rec.Failed && rec.Status == 500 {
			sawScoutFailed = true
		}
	}
	if kinds["page"] != 6 || kinds["asset"] != 3 || kinds["scout"] != 2 {
		t.Errorf("unexpected trace kinds: %+v", kinds)
	}
	if !sawAdmittedPage || !sawAsset403 || !sawScoutFailed {
		t.Errorf("missing expected trace details: page=%v asset403=%v scoutFailed=%v",
			sawAdmittedPage, sawAsset403, sawScoutFailed)
	}
}

func TestRenderData_DataDropWarningBannerConditions(t *testing.T) {
	base := Data{
		SchemaVersion: SchemaVersion,
		Version:       "2.0.0",
		GeneratedAt:   time.Now(),
		Config:        swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second},
	}

	clean, err := RenderData(base, "clean", "127.0.0.1")
	if err != nil {
		t.Fatalf("RenderData clean: %v", err)
	}
	if strings.Contains(clean.HTML, "Data Integrity Warning") || strings.Contains(clean.Markdown, "DATA INTEGRITY") {
		t.Fatal("warning banner must not appear when no drops occurred")
	}

	cases := []struct {
		name   string
		mutate func(*Data)
	}{
		{"dropped_logs", func(d *Data) { d.DroppedLogs = 4 }},
		{"trace_dropped", func(d *Data) { d.TraceDropped = 9 }},
		{"forensics_dropped_queue", func(d *Data) { d.Forensics.Stats.DroppedQueue = 3 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mutate(&d)
			out, err := RenderData(d, tc.name, "127.0.0.1")
			if err != nil {
				t.Fatalf("RenderData: %v", err)
			}
			if !strings.Contains(out.HTML, "Data Integrity Warning") {
				t.Errorf("%s: HTML missing Data Integrity Warning banner", tc.name)
			}
			if !strings.Contains(out.Markdown, "DATA INTEGRITY") {
				t.Errorf("%s: Markdown missing DATA INTEGRITY warning banner", tc.name)
			}
		})
	}
}

func TestMarkdownInjectionResistance_MdCellAndMdFence(t *testing.T) {
	if got := mdCell("a|b\nc\r\nd\re"); got != `a\|b c d e` {
		t.Errorf("mdCell = %q, want %q", got, `a\|b c d e`)
	}
	if got := mdFence("plain text"); got != "```" {
		t.Errorf("mdFence(plain) = %q, want ```", got)
	}
	if got := mdFence("has ``` triple"); got != "````" {
		t.Errorf("mdFence(triple) = %q, want ````", got)
	}
	if got := mdFence("has ````` five"); got != "``````" {
		t.Errorf("mdFence(five) = %q, want ``````", got)
	}

	hostileExcerpt := "payload\n```\n# INJECTED\n````\nend"
	d := Data{
		SchemaVersion: SchemaVersion,
		Version:       "2.0.0",
		GeneratedAt:   time.Now(),
		Config:        swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second, Navigation: "crawl|evil\nline"},
		Journey:       "step|one\nbreak",
		ErrorKinds:    []CountRow{{Label: "err|pipe\nnewline", Count: 1}},
		Paths: []PathReport{{
			URL: "http://127.0.0.1/path|pipe\n|injected|", Hits: 1, Bytes: "10 B",
		}},
		Forensics: ForensicsReport{
			Stats: forensics.Stats{Captured: 1, Mode: forensics.ModeHTTPOnly},
			Artifacts: []forensics.Artifact{{
				ID:            "art1",
				Sequence:      1,
				Path:          "/err",
				StatusCode:    500,
				TriggerReason: "http-500",
				ReviewStatus:  forensics.ReviewUnreviewed,
				ErrorBody: journey.ExtractedError{
					Format:      "text",
					Summary:     "boom",
					StackTrace:  "trace ``` inside",
					BodyExcerpt: hostileExcerpt,
				},
			}},
		},
	}

	rendered, err := RenderData(d, "inject", "127.0.0.1")
	if err != nil {
		t.Fatalf("RenderData: %v", err)
	}
	if !strings.Contains(rendered.Markdown, `http://127.0.0.1/path\|pipe \|injected\|`) {
		t.Errorf("Markdown did not escape pipe/newline in path table cell:\n%s", rendered.Markdown)
	}
	if !strings.Contains(rendered.Markdown, `err\|pipe newline`) {
		t.Errorf("Markdown did not escape pipe/newline in error_kinds cell")
	}
	// BodyExcerpt contains 4 backticks, so mdFence must wrap it in 5 backticks.
	if !strings.Contains(rendered.Markdown, "`````text\n"+hostileExcerpt+"\n`````") {
		t.Errorf("Markdown did not wrap hostile BodyExcerpt with 5-backtick fence:\n%s", rendered.Markdown)
	}
	// StackTrace contains 3 backticks, so mdFence must wrap it in 4 backticks.
	if !strings.Contains(rendered.Markdown, "````text\ntrace ``` inside\n````") {
		t.Errorf("Markdown did not wrap StackTrace with 4-backtick fence")
	}
}

func TestCompare_AssetAndQueueDeltasAndLegacyCompat(t *testing.T) {
	dir := t.TempDir()
	// Legacy v1-style JSON lacking schema_version, assets, concert, scouts, lemmings_completed.
	legacyJSON := `{
		"version": "1.9.0",
		"generated_at": "2026-10-01T00:00:00Z",
		"config": {"hit": "http://127.0.0.1:8080/"},
		"rescued_percent": 99.0,
		"failure_rate": 0.01,
		"p95_ns": 100000000,
		"waiting_room": {"held": 4}
	}`
	oldPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(oldPath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	newData := Data{
		SchemaVersion:  SchemaVersion,
		Version:        "2.0.0",
		GeneratedAt:    time.Now(),
		Config:         swarm.Config{Hit: "http://127.0.0.1:8080/"},
		RescuedPercent: 98.0,
		FailureRate:    0.02,
		P95:            150 * time.Millisecond,
		Assets:         AssetReport{Requests: 20, P95: 45 * time.Millisecond},
		Room:           RoomReport{Held: 11, Admitted: 10, TotalPolls: 33},
	}
	newBytes, err := json.Marshal(newData)
	if err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(dir, "v2.json")
	if err := os.WriteFile(newPath, newBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := CompareFiles(oldPath, newPath, CompareOptions{
		MaxFailureDelta: -1,
		MaxP95DeltaMS:   0,
		MaxRescuedDrop:  -1,
	})
	if err != nil {
		t.Fatalf("CompareFiles legacy vs v2: %v", err)
	}
	if res.AssetP95DeltaMS != 45.0 {
		t.Errorf("AssetP95DeltaMS = %v, want 45.0", res.AssetP95DeltaMS)
	}
	if res.QueueHeldDelta != 7 {
		t.Errorf("QueueHeldDelta = %d, want 7 (11 - 4)", res.QueueHeldDelta)
	}
}
