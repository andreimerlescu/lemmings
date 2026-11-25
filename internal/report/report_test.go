package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
