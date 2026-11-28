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
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
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
		ErrorBody: plan.ExtractedError{
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

	// SEC-10: verify default remote variants omit inline base64 screenshots and external screenshot links.
	if strings.Contains(rendered.RemoteHTML, "data:image/jpeg;base64,") {
		t.Error("SEC-10 violation: RemoteHTML embedded inline screenshot base64 data")
	}
	if !strings.Contains(rendered.RemoteHTML, "-include-remote-screenshots") {
		t.Error("RemoteHTML missing opt-in notice for omitted screenshots")
	}
	if strings.Contains(rendered.RemoteMarkdown, wantRel) {
		t.Errorf("SEC-10 violation: RemoteMarkdown linked external screenshot %q", wantRel)
	}
	if !strings.Contains(rendered.RemoteMarkdown, "-include-remote-screenshots") {
		t.Error("RemoteMarkdown missing opt-in notice for omitted screenshots")
	}
	var parsedRemote Data
	if err := json.Unmarshal(rendered.RemoteJSON, &parsedRemote); err != nil {
		t.Fatalf("RemoteJSON unmarshal: %v", err)
	}
	if len(parsedRemote.Forensics.Artifacts) != 1 || parsedRemote.Forensics.Artifacts[0].Screenshot == nil {
		t.Fatalf("RemoteJSON missing artifact metadata: %+v", parsedRemote.Forensics)
	}
	if parsedRemote.Forensics.Artifacts[0].Screenshot.RelativePath != "" {
		t.Errorf("RemoteJSON should omit dangling screenshot image_path, got %q", parsedRemote.Forensics.Artifacts[0].Screenshot.RelativePath)
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

func TestTimeline_QueuedDeduplicationAndSVGChart(t *testing.T) {
	start := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	rep := New(swarm.Config{Hit: "http://127.0.0.1:8080/", Terrain: 1, Pack: 2, Limit: 2, Until: 10 * time.Second})
	rep.MarkStarted(start)

	bus := events.NewBus()
	rep.Attach(bus)
	defer bus.Close()

	// Two lemmings born at t=0s.
	bus.Emit(events.Event{Kind: events.LemmingBorn, LemmingID: "l1", OccurredAt: start})
	bus.Emit(events.Event{Kind: events.LemmingBorn, LemmingID: "l2", OccurredAt: start})
	// l1 enters waiting room at t=1s, gets position update at t=2s (must not double-count), l2 enters at t=2s.
	bus.Emit(events.Event{Kind: events.WaitingRoomQueued, LemmingID: "l1", Position: 10, OccurredAt: start.Add(1 * time.Second)})
	bus.Emit(events.Event{Kind: events.WaitingRoomQueued, LemmingID: "l1", Position: 5, OccurredAt: start.Add(2 * time.Second)})
	bus.Emit(events.Event{Kind: events.WaitingRoomQueued, LemmingID: "l2", Position: 11, OccurredAt: start.Add(2 * time.Second)})
	// l1 exits waiting room at t=3s; l2 dies while still queued at t=4s (must decrement queued).
	bus.Emit(events.Event{Kind: events.WaitingRoom, LemmingID: "l1", OccurredAt: start.Add(3 * time.Second)})
	bus.Emit(events.Event{Kind: events.LemmingDied, LemmingID: "l2", OccurredAt: start.Add(4 * time.Second)})
	bus.Emit(events.Event{Kind: events.LemmingDied, LemmingID: "l1", OccurredAt: start.Add(4 * time.Second)})

	pts := rep.Result().Timeline
	if len(pts) < 5 {
		t.Fatalf("expected at least 5 timeline points, got %d", len(pts))
	}
	wantQueued := []int64{0, 1, 2, 1, 0}
	for i, want := range wantQueued {
		if pts[i].Queued != want {
			t.Errorf("t=%ds: Queued = %d, want %d (pts=%+v)", i, pts[i].Queued, want, pts)
		}
	}

	svg := string(TimelineSVG(pts))
	if !strings.Contains(svg, `class="s-queued line dash"`) {
		t.Errorf("TimelineSVG missing queued series polyline:\n%s", svg)
	}
	if !strings.Contains(svg, `alive / queued`) {
		t.Errorf("TimelineSVG missing right-axis 'alive / queued' label")
	}
}

func TestWaitingRoomGatesAndDiedInLineNotRescued(t *testing.T) {
	// 1. Vacuous fast-lane run with zero held visits fails -min-queue-held gate.
	repEmpty := New(swarm.Config{
		Hit:          "http://127.0.0.1:8080/",
		Terrain:      1,
		Pack:         1,
		Limit:        1,
		Until:        5 * time.Second,
		MinQueueHeld: 1,
	})
	repEmpty.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1:8080/", StatusCode: 200, Latency: 20 * time.Millisecond})
	resEmpty := repEmpty.Result()
	if !resEmpty.GatesFailed() || resEmpty.Verdict != "FAILED" {
		t.Fatalf("expected -min-queue-held 1 to fail when Room.Held=0, got verdict=%s gates=%+v", resEmpty.Verdict, resEmpty.Gates)
	}

	// 2. Died-in-line queue-timeout enters failure rate + latency P95, trips -max-queue-wait, and is NOT counted as rescued.
	repQueue := New(swarm.Config{
		Hit:          "http://127.0.0.1:8080/",
		Terrain:      1,
		Pack:         3,
		Limit:        3,
		Until:        5 * time.Second,
		MinQueueHeld: 2,
		MaxQueueWait: 2 * time.Second,
	})
	// Admitted visitor: fast origin (30ms), short queue wait (500ms).
	repQueue.RecordVisit(&swarm.Visit{
		URL: "http://127.0.0.1:8080/", StatusCode: 200, Latency: 30 * time.Millisecond,
		WaitingRoom: swarm.WaitingRoomMetric{Detected: true, Admitted: true, Duration: 500 * time.Millisecond},
	})
	repQueue.Ingest(swarm.LifeLog{Identity: swarm.Identity{Number: 1, ID: "ok"}, Streamed: true, TotalVisits: 1})

	// Starved visitor (queue-timeout): waited 5s, failed with queue-timeout.
	repQueue.RecordVisit(&swarm.Visit{
		URL: "http://127.0.0.1:8080/", StatusCode: 503, Failed: true, ErrorKind: "queue-timeout",
		Error:       errors.New("waiting room wait exceeded lemming lifespan"),
		Latency:     5 * time.Second,
		WaitingRoom: swarm.WaitingRoomMetric{Detected: true, Admitted: false, Duration: 5 * time.Second},
	})
	repQueue.Ingest(swarm.LifeLog{Identity: swarm.Identity{Number: 2, ID: "starved"}, Streamed: true, TotalVisits: 1, FailedVisits: 1})

	// Cancelled visitor with zero completed visits must NOT count as rescued either.
	repQueue.Ingest(swarm.LifeLog{Identity: swarm.Identity{Number: 3, ID: "cancelled"}, Streamed: true, TotalVisits: 1, CancelledVisits: 1})

	res := repQueue.Result()
	if res.FailedVisits != 1 || res.FailureRate != 0.5 {
		t.Errorf("expected 1 failed visit (50%%), got failed=%d rate=%v", res.FailedVisits, res.FailureRate)
	}
	if res.P95 < 4*time.Second {
		t.Errorf("survivorship bias: expected P95 to reflect 5s queue-timeout visit, got %s", res.P95)
	}
	if res.Rescued != 1 || res.Sessions != 3 {
		t.Errorf("expected only 1 of 3 sessions rescued, got rescued=%d sessions=%d", res.Rescued, res.Sessions)
	}
	if res.Room.Held != 2 || res.Room.DiedInLine != 1 {
		t.Errorf("unexpected room counters: %+v", res.Room)
	}
	// min-queue-held (2 >= 2) passes, max-queue-wait (5s > 2s) fails.
	if len(res.Gates) != 2 || !res.Gates[0].Passed || res.Gates[1].Passed {
		t.Errorf("unexpected waiting-room gate results: %+v", res.Gates)
	}
}

func TestReporter_RoomErrorSplitPersonaQueueAndColdWarmPathTiming(t *testing.T) {
	rep := New(swarm.Config{
		Hit:     "http://127.0.0.1:8080/",
		Terrain: 1,
		Pack:    4,
		Limit:   4,
		Until:   10 * time.Second,
	})

	// Cold hit on "/" (DNS+Connect+TLS = 140ms, total 180ms) + warm hit on "/" (40ms).
	rep.RecordVisit(&swarm.Visit{
		Role: swarm.RolePlan, PlanName: "guest", Step: "home", StepIndex: 1,
		URL: "http://127.0.0.1:8080/", StatusCode: 200, Latency: 180 * time.Millisecond,
		Timing:      swarm.RequestTiming{DNS: 10 * time.Millisecond, Connect: 50 * time.Millisecond, TLS: 80 * time.Millisecond, TTFB: 175 * time.Millisecond, Reused: false},
		WaitingRoom: swarm.WaitingRoomMetric{Detected: true, Admitted: true, Duration: 3 * time.Second},
	})
	rep.RecordVisit(&swarm.Visit{
		Role: swarm.RolePlan, PlanName: "staff", Step: "home", StepIndex: 1,
		URL: "http://127.0.0.1:8080/", StatusCode: 200, Latency: 40 * time.Millisecond,
		Timing: swarm.RequestTiming{TTFB: 38 * time.Millisecond, Reused: true},
	})
	// Post-queue errors: not-admitted vs reload transport error vs true died-in-line.
	rep.RecordVisit(&swarm.Visit{
		Role: swarm.RolePlan, PlanName: "guest", Step: "home", StepIndex: 1,
		URL: "http://127.0.0.1:8080/", StatusCode: 503, Failed: true, ErrorKind: "queue-not-admitted",
		Error:       errors.New("not admitted"),
		WaitingRoom: swarm.WaitingRoomMetric{Detected: true, Duration: 2 * time.Second},
	})
	rep.RecordVisit(&swarm.Visit{
		Role: swarm.RolePlan, PlanName: "guest", Step: "home", StepIndex: 1,
		URL: "http://127.0.0.1:8080/", StatusCode: 0, Failed: true, ErrorKind: "reset",
		Error:       errors.New("connection reset"),
		WaitingRoom: swarm.WaitingRoomMetric{Detected: true, Duration: 1 * time.Second},
	})
	rep.RecordVisit(&swarm.Visit{
		Role: swarm.RolePlan, PlanName: "guest", Step: "home", StepIndex: 1,
		URL: "http://127.0.0.1:8080/", StatusCode: 503, Failed: true, ErrorKind: "queue-timeout",
		Error:       errors.New("queue timeout"),
		Latency:     5 * time.Second,
		WaitingRoom: swarm.WaitingRoomMetric{Detected: true, Duration: 5 * time.Second},
	})

	d := rep.Result()
	if d.Room.Held != 4 || d.Room.Admitted != 1 || d.Room.DiedInLine != 1 || d.Room.NotAdmitted != 1 || d.Room.ReloadErrors != 1 {
		t.Fatalf("RoomReport split mismatch: %+v", d.Room)
	}
	if len(d.Paths) != 1 {
		t.Fatalf("expected 1 path row, got %d", len(d.Paths))
	}
	p := d.Paths[0]
	if p.ColdHits != 1 || p.SetupP50 < 130*time.Millisecond || p.WarmP50 < 35*time.Millisecond || p.WarmP50 > 45*time.Millisecond || p.TTFBP50 <= 0 {
		t.Errorf("PathReport cold/warm/TTFB timing mismatch: %+v", p)
	}
	if d.Plan == nil || len(d.Plan.Personas) != 2 {
		t.Fatalf("expected 2 personas in PlanReport, got %+v", d.Plan)
	}
	byName := map[string]PersonaReport{}
	for _, pr := range d.Plan.Personas {
		byName[pr.Name] = pr
	}
	if g := byName["guest"]; g.QueueHeld != 4 || g.QueueAdmitted != 1 || g.QueueDied != 1 || g.QueueP95Wait < 4*time.Second {
		t.Errorf("guest per-persona queue stats mismatch: %+v", g)
	}
	if s := byName["staff"]; s.QueueHeld != 0 || s.QueueAdmitted != 0 || s.QueueDied != 0 {
		t.Errorf("staff per-persona queue stats should be zero: %+v", s)
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
		ErrorBody:     plan.ExtractedError{Format: "html", Headline: xss, Summary: xss, BodyExcerpt: xss},
	})
	_, _ = store.Review(sum.ID, forensics.ReviewBug, xss)

	rep := New(swarm.Config{Hit: "http://127.0.0.1/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second, Version: "2.0.0"})
	rep.SetForensics(store)
	rep.RecordVisit(&swarm.Visit{
		URL:        "http://127.0.0.1/" + xss,
		StatusCode: 500,
		Failed:     true,
		Page:       plan.PageEvidence{Title: xss},
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
		Plan: &PlanReport{
			RogueRate:    0.05,
			PlanLemmings: 1,
			PlanFailed:   1,
			Personas: []PersonaReport{{
				Name:       "buyer|evil\nbreak",
				Weight:     1,
				TotalSteps: 1,
				Lemmings:   1,
				Failed:     1,
				Steps: []PlanStepReport{{
					Index:      1,
					Name:       "step|one\nbreak",
					Method:     "POST",
					Path:       "/pay|pipe\nline",
					Hits:       1,
					Failed:     1,
					TopFailure: "err|top\nfail",
				}},
			}},
		},
		ErrorKinds: []CountRow{{Label: "err|pipe\nnewline", Count: 1}},
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
				ErrorBody: plan.ExtractedError{
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
	for _, escaped := range []string{`buyer\|evil break`, `step\|one break`, `/pay\|pipe line`, `err\|top fail`} {
		if !strings.Contains(rendered.Markdown, escaped) {
			t.Errorf("Markdown did not escape plan cell %q", escaped)
		}
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

func TestReporter_MultiPersonaPlanReportAndRogueCohort(t *testing.T) {
	// Without a configured plan or plan/rogue sessions, Data.Plan and Live().Plan must be nil.
	plainRep := New(swarm.Config{Hit: "http://127.0.0.1:8080/", Terrain: 1, Pack: 1, Limit: 1, Until: time.Second})
	plainRep.RecordVisit(&swarm.Visit{URL: "http://127.0.0.1:8080/", StatusCode: 200, Duration: 5 * time.Millisecond})
	if plainRep.Live(5, 5).Plan != nil || plainRep.Result().Plan != nil {
		t.Fatal("expected Plan to be nil when no plan configured and no plan/rogue sessions occurred")
	}

	suite, err := plan.NewSuite([]*plan.Plan{
		{
			Name:       "checkout",
			Weight:     3,
			StopOnFail: true,
			Items: []plan.PlanItem{
				{Name: "landing", Path: "/", Method: "GET"},
				{Name: "login", Path: "/login?token=secret_query", Method: "POST"},
				{Name: "confirm", Path: "/checkout/confirm", Method: "POST"},
			},
		},
		{
			Name:   "browser",
			Weight: 1,
			Items: []plan.PlanItem{
				{Name: "home", Path: "/", Method: "GET"},
				{Name: "unreached-catalog", Path: "/catalog", Method: "GET"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSuite: %v", err)
	}

	cfg := swarm.Config{
		Hit:       "http://127.0.0.1:8080/",
		Terrain:   1,
		Pack:      6,
		Limit:     6,
		Until:     10 * time.Second,
		Plan:      suite,
		RogueRate: 0.25,
		Version:   "2.0.0",
	}
	rep := New(cfg)

	// Checkout lemming #1 (streamed): passes all 3 steps.
	for i, step := range []struct {
		idx    int
		name   string
		method string
		url    string
		dur    time.Duration
	}{
		{1, "landing", "GET", "http://127.0.0.1:8080/", 10 * time.Millisecond},
		{2, "login", "POST", "http://127.0.0.1:8080/login", 20 * time.Millisecond},
		{3, "confirm", "POST", "http://127.0.0.1:8080/checkout/confirm", 30 * time.Millisecond},
	} {
		rep.RecordVisit(&swarm.Visit{
			LemmingID: "lem-1", LemmingNumber: 1, Sequence: i + 1,
			Role: swarm.RolePlan, PlanName: "checkout", Step: step.name, StepIndex: step.idx, Method: step.method,
			URL: step.url, StatusCode: 200, Duration: step.dur,
		})
	}
	rep.Ingest(swarm.LifeLog{
		Identity:           swarm.Identity{Number: 1, ID: "lem-1", Name: "Neon Fox", Role: swarm.RolePlan, PlanName: "checkout"},
		Role:               swarm.RolePlan,
		PlanName:           "checkout",
		PlanStepsCompleted: 3,
		PlanStepsTotal:     3,
		PlanPassed:         true,
		Streamed:           true,
		TotalVisits:        3,
		ExitReason:         swarm.ExitPlanComplete,
		Duration:           time.Second,
		Visits: []swarm.Visit{
			{Sequence: 1, Method: "GET", Step: "landing", URL: "http://127.0.0.1:8080/", StatusCode: 200, Duration: 10 * time.Millisecond},
			{Sequence: 2, Method: "POST", Step: "login", URL: "http://127.0.0.1:8080/login", StatusCode: 200, Duration: 20 * time.Millisecond},
			{Sequence: 3, Method: "POST", Step: "confirm", URL: "http://127.0.0.1:8080/checkout/confirm", StatusCode: 200, Duration: 30 * time.Millisecond},
		},
	})

	// Checkout lemmings #2 & #3 (streamed): pass step 1, fail step 2 with two distinct errors to verify deterministic TopFailure tie-break.
	// Tied counts (1 vs 1): "status 500" < "status 503". Then add a second "status 503" so "status 503" (count 2) wins.
	rep.RecordVisit(&swarm.Visit{
		LemmingID: "lem-2", LemmingNumber: 2, Sequence: 1,
		Role: swarm.RolePlan, PlanName: "checkout", Step: "landing", StepIndex: 1, Method: "GET",
		URL: "http://127.0.0.1:8080/", StatusCode: 200, Duration: 12 * time.Millisecond,
	})
	rep.RecordVisit(&swarm.Visit{
		LemmingID: "lem-2", LemmingNumber: 2, Sequence: 2,
		Role: swarm.RolePlan, PlanName: "checkout", Step: "login", StepIndex: 2, Method: "POST",
		URL: "http://127.0.0.1:8080/login", StatusCode: 500, Failed: true, Duration: 40 * time.Millisecond,
	})
	rep.Ingest(swarm.LifeLog{
		Identity:           swarm.Identity{Number: 2, ID: "lem-2", Name: "Synth Cat", Role: swarm.RolePlan, PlanName: "checkout"},
		Role:               swarm.RolePlan,
		PlanName:           "checkout",
		PlanStepsCompleted: 2,
		PlanStepsTotal:     3,
		PlanPassed:         false,
		Streamed:           true,
		TotalVisits:        2,
		FailedVisits:       1,
		FirstFailureAt:     time.Now(),
		ExitReason:         swarm.ExitPlanFailed,
	})

	rep.RecordVisit(&swarm.Visit{
		LemmingID: "lem-3", LemmingNumber: 3, Sequence: 1,
		Role: swarm.RolePlan, PlanName: "checkout", Step: "landing", StepIndex: 1, Method: "GET",
		URL: "http://127.0.0.1:8080/", StatusCode: 200, Duration: 14 * time.Millisecond,
	})
	rep.RecordVisit(&swarm.Visit{
		LemmingID: "lem-3", LemmingNumber: 3, Sequence: 2,
		Role: swarm.RolePlan, PlanName: "checkout", Step: "login", StepIndex: 2, Method: "POST",
		URL: "http://127.0.0.1:8080/login", StatusCode: 503, Failed: true, Duration: 50 * time.Millisecond,
	})
	// Check alphabetical tie-break while 500 (1) == 503 (1): "status 500" must win!
	midLive := rep.Live(5, 5)
	if midLive.Plan == nil || len(midLive.Plan.Personas) != 2 {
		t.Fatalf("midLive.Plan = %+v, want 2 personas", midLive.Plan)
	}
	if got := midLive.Plan.Personas[0].Steps[1].TopFailure; got != "status 500" {
		t.Errorf("TopFailure alphabetical tie-break = %q, want %q", got, "status 500")
	}

	// Add another 503 failure + a cancelled visit on step 2 (cancelled must NOT increment Hits/Failed/Latency).
	rep.RecordVisit(&swarm.Visit{
		LemmingID: "lem-3b", LemmingNumber: 3, Sequence: 2,
		Role: swarm.RolePlan, PlanName: "checkout", Step: "login", StepIndex: 2, Method: "POST",
		URL: "http://127.0.0.1:8080/login", StatusCode: 503, Failed: true, Duration: 60 * time.Millisecond,
	})
	rep.RecordVisit(&swarm.Visit{
		LemmingID: "lem-3c", LemmingNumber: 3, Sequence: 2,
		Role: swarm.RolePlan, PlanName: "checkout", Step: "login", StepIndex: 2, Method: "POST",
		URL: "http://127.0.0.1:8080/login", StatusCode: 500, Cancelled: true, Duration: 999 * time.Millisecond,
	})
	rep.Ingest(swarm.LifeLog{
		Identity:           swarm.Identity{Number: 3, ID: "lem-3", Name: "Laser Owl", Role: swarm.RolePlan, PlanName: "checkout"},
		Role:               swarm.RolePlan,
		PlanName:           "checkout",
		PlanStepsCompleted: 2,
		PlanStepsTotal:     3,
		PlanPassed:         false,
		Streamed:           true,
		TotalVisits:        2,
		FailedVisits:       1,
		ExitReason:         swarm.ExitPlanFailed,
	})

	// Checkout lemming #4 (NON-streamed, PlanPassed omitted): should recompute PlanPassed=true!
	rep.Ingest(swarm.LifeLog{
		Identity: swarm.Identity{Number: 4, ID: "lem-4", Name: "Offline Ace", Role: swarm.RolePlan, PlanName: "checkout"},
		Role:     swarm.RolePlan,
		PlanName: "checkout",
		Streamed: false,
		Visits: []swarm.Visit{
			{Sequence: 1, Role: swarm.RolePlan, PlanName: "checkout", Step: "landing", StepIndex: 1, Method: "GET", URL: "http://127.0.0.1:8080/", StatusCode: 200, Duration: 15 * time.Millisecond},
			{Sequence: 2, Role: swarm.RolePlan, PlanName: "checkout", Step: "login", StepIndex: 2, Method: "POST", URL: "http://127.0.0.1:8080/login", StatusCode: 200, Duration: 25 * time.Millisecond},
			{Sequence: 3, Role: swarm.RolePlan, PlanName: "checkout", Step: "confirm", StepIndex: 3, Method: "POST", URL: "http://127.0.0.1:8080/checkout/confirm", StatusCode: 200, Duration: 35 * time.Millisecond},
		},
	})

	// Browser lemming #5 (NON-streamed, stops after step 1 of 2): step 2 stays unreached (Hits: 0), PlanPassed=false.
	rep.Ingest(swarm.LifeLog{
		Identity: swarm.Identity{Number: 5, ID: "lem-5", Name: "Short Walker", Role: swarm.RolePlan, PlanName: "browser"},
		Role:     swarm.RolePlan,
		PlanName: "browser",
		Streamed: false,
		Visits: []swarm.Visit{
			{Sequence: 1, Role: swarm.RolePlan, PlanName: "browser", Step: "home", StepIndex: 1, Method: "GET", URL: "http://127.0.0.1:8080/", StatusCode: 200, Duration: 8 * time.Millisecond},
		},
	})

	// Rogue cohort: 1 flawless, 1 failed.
	rep.Ingest(swarm.LifeLog{
		Identity:    swarm.Identity{Number: 6, ID: "lem-6", Name: "Rogue Good", Role: swarm.RoleRogue},
		Role:        swarm.RoleRogue,
		Streamed:    true,
		TotalVisits: 4,
	})
	rep.Ingest(swarm.LifeLog{
		Identity:     swarm.Identity{Number: 7, ID: "lem-7", Name: "Rogue Bad", Role: swarm.RoleRogue},
		Role:         swarm.RoleRogue,
		Streamed:     true,
		TotalVisits:  2,
		FailedVisits: 1,
	})

	data := rep.Result()
	live := rep.Live(10, 10)

	for _, pair := range []struct {
		name string
		pr   *PlanReport
	}{{"Result", data.Plan}, {"Live", live.Plan}} {
		pr := pair.pr
		if pr == nil {
			t.Fatalf("%s: PlanReport is nil", pair.name)
		}
		if pr.RogueRate != 0.25 {
			t.Errorf("%s: RogueRate = %v, want 0.25", pair.name, pr.RogueRate)
		}
		if pr.PlanLemmings != 5 || pr.PlanPassed != 2 || pr.PlanFailed != 3 {
			t.Errorf("%s: Plan cohort = %d (%d passed, %d failed), want 5 (2 passed, 3 failed)",
				pair.name, pr.PlanLemmings, pr.PlanPassed, pr.PlanFailed)
		}
		if math.Abs(pr.PlanPassRate-40.0) > 1e-6 {
			t.Errorf("%s: PlanPassRate = %.4f, want 40.0", pair.name, pr.PlanPassRate)
		}
		if pr.RogueLemmings != 2 || pr.RogueFlawless != 1 {
			t.Errorf("%s: Rogue cohort = %d (%d flawless), want 2 (1 flawless)", pair.name, pr.RogueLemmings, pr.RogueFlawless)
		}
		if len(pr.Personas) != 2 {
			t.Fatalf("%s: len(Personas) = %d, want 2", pair.name, len(pr.Personas))
		}

		// Persona 0: checkout
		p0 := pr.Personas[0]
		if p0.Name != "checkout" || p0.Weight != 3 || p0.TotalSteps != 3 || p0.Lemmings != 4 || p0.Passed != 2 || p0.Failed != 2 || math.Abs(p0.PassRate-50.0) > 1e-6 {
			t.Errorf("%s: checkout persona mismatch: %+v", pair.name, p0)
		}
		if len(p0.Steps) != 3 {
			t.Fatalf("%s: checkout steps len = %d, want 3", pair.name, len(p0.Steps))
		}
		// Step 2 (login): 2 passed (lem-1, lem-4) + 3 failed (lem-2 500, lem-3 503, lem-3b 503) = 5 hits (cancelled ignored!).
		stLogin := p0.Steps[1]
		if stLogin.Index != 2 || stLogin.Name != "login" || stLogin.Method != "POST" || stLogin.Path != "/login" {
			t.Errorf("%s: login step metadata mismatch: %+v", pair.name, stLogin)
		}
		if stLogin.Hits != 5 || stLogin.Passed != 2 || stLogin.Failed != 3 || stLogin.TopFailure != "status 503" {
			t.Errorf("%s: login step stats mismatch: %+v", pair.name, stLogin)
		}
		if stLogin.P50 <= 0 || stLogin.P95 <= 0 || stLogin.P99 <= 0 || stLogin.P99 >= 900*time.Millisecond {
			t.Errorf("%s: login step quantiles polluted or zero: p50=%s p95=%s p99=%s", pair.name, stLogin.P50, stLogin.P95, stLogin.P99)
		}

		// Persona 1: browser (step 2 unreached!)
		p1 := pr.Personas[1]
		if p1.Name != "browser" || p1.Weight != 1 || p1.TotalSteps != 2 || p1.Lemmings != 1 || p1.Passed != 0 || p1.Failed != 1 {
			t.Errorf("%s: browser persona mismatch: %+v", pair.name, p1)
		}
		if len(p1.Steps) != 2 {
			t.Fatalf("%s: browser steps len = %d, want 2", pair.name, len(p1.Steps))
		}
		stUnreached := p1.Steps[1]
		if stUnreached.Index != 2 || stUnreached.Name != "unreached-catalog" || stUnreached.Method != "GET" || stUnreached.Path != "/catalog" ||
			stUnreached.Hits != 0 || stUnreached.Passed != 0 || stUnreached.Failed != 0 || stUnreached.P50 != 0 || stUnreached.TopFailure != "" {
			t.Errorf("%s: unreached step mismatch: %+v", pair.name, stUnreached)
		}
	}

	// Verify NotableLife carries Role, PlanName, PlanPassed, and LifeStep.Method.
	var sawNotableWithPlan bool
	for _, nl := range data.Notable {
		if nl.PlanName == "checkout" && nl.Role == swarm.RolePlan {
			sawNotableWithPlan = true
			if len(nl.Steps) > 0 && nl.Steps[0].Method == "" {
				t.Errorf("NotableLife step missing Method: %+v", nl.Steps[0])
			}
		}
	}
	if !sawNotableWithPlan {
		t.Errorf("expected at least one NotableLife with PlanName=checkout and Role=plan, got %+v", data.Notable)
	}
}

func TestReporter_PlanPassRateGate(t *testing.T) {
	suite, err := plan.NewSuite([]*plan.Plan{
		{Name: "smoke", Weight: 1, Items: []plan.PlanItem{{Name: "root", Path: "/", Method: "GET"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("zero_plan_lemmings_fails_even_with_zero_min", func(t *testing.T) {
		rep := New(swarm.Config{
			Hit:             "http://127.0.0.1:8080/",
			Terrain:         1,
			Pack:            1,
			Limit:           1,
			Until:           time.Second,
			Plan:            suite,
			PlanPassGate:    true,
			MinPlanPassRate: 0.0,
		})
		res := rep.Result()
		if res.Verdict != "FAILED" || !res.GatesFailed() || len(res.Gates) != 1 {
			t.Fatalf("expected FAILED gate with 0 plan lemmings, got verdict=%q gates=%+v", res.Verdict, res.Gates)
		}
		g := res.Gates[0]
		if g.Name != "plan pass rate" || g.Limit != "≥ 0.00%" || g.Observed != "0.00%" || g.Passed {
			t.Errorf("unexpected gate result: %+v", g)
		}
	})

	t.Run("below_threshold_fails", func(t *testing.T) {
		rep := New(swarm.Config{
			Hit:             "http://127.0.0.1:8080/",
			Terrain:         1,
			Pack:            10,
			Limit:           10,
			Until:           time.Second,
			Plan:            suite,
			PlanPassGate:    true,
			MinPlanPassRate: 0.90,
		})
		for i := 1; i <= 10; i++ {
			rep.Ingest(swarm.LifeLog{
				Identity:   swarm.Identity{Number: int64(i), ID: "p", Role: swarm.RolePlan, PlanName: "smoke"},
				Role:       swarm.RolePlan,
				PlanName:   "smoke",
				PlanPassed: i <= 8,
				Streamed:   true,
			})
		}
		res := rep.Result()
		if res.Verdict != "FAILED" || !res.GatesFailed() || len(res.Gates) != 1 {
			t.Fatalf("expected FAILED at 80%% < 90%%, got verdict=%q gates=%+v", res.Verdict, res.Gates)
		}
		g := res.Gates[0]
		if g.Name != "plan pass rate" || g.Limit != "≥ 90.00%" || g.Observed != "80.00%" || g.Passed {
			t.Errorf("unexpected gate result: %+v", g)
		}
	})

	t.Run("at_or_above_threshold_passes", func(t *testing.T) {
		rep := New(swarm.Config{
			Hit:             "http://127.0.0.1:8080/",
			Terrain:         1,
			Pack:            10,
			Limit:           10,
			Until:           time.Second,
			Plan:            suite,
			PlanPassGate:    true,
			MinPlanPassRate: 0.90,
		})
		for i := 1; i <= 10; i++ {
			rep.Ingest(swarm.LifeLog{
				Identity:   swarm.Identity{Number: int64(i), ID: "p", Role: swarm.RolePlan, PlanName: "smoke"},
				Role:       swarm.RolePlan,
				PlanName:   "smoke",
				PlanPassed: i <= 9,
				Streamed:   true,
			})
		}
		res := rep.Result()
		if res.Verdict != "PASSED" || res.GatesFailed() || len(res.Gates) != 1 {
			t.Fatalf("expected PASSED at 90%% >= 90%%, got verdict=%q gates=%+v", res.Verdict, res.Gates)
		}
		g := res.Gates[0]
		if g.Name != "plan pass rate" || g.Limit != "≥ 90.00%" || g.Observed != "90.00%" || !g.Passed {
			t.Errorf("unexpected gate result: %+v", g)
		}
	})
}

func TestRenderData_PlanExecutionFormatsAndSEC10NoSecretLeak(t *testing.T) {
	const (
		secretCookie = "SECRET_PLAN_COOKIE_VALUE_XYZ"
		secretHeader = "SECRET_PLAN_HEADER_TOKEN_XYZ"
		secretBodyKV = "SECRET_PLAN_BODY_FIELD_XYZ"
		secretRaw    = "SECRET_RAW_PLAN_BODY_XYZ"
		secretQuery  = "SECRET_QUERY_TOKEN_XYZ"
	)

	suite, err := plan.NewSuite([]*plan.Plan{
		{
			Name:   "auth-buyer",
			Weight: 2,
			Items: []plan.PlanItem{
				{
					Name:        "sign-in",
					Path:        "/api/login?api_key=" + secretQuery,
					Method:      "POST",
					WithCookies: []plan.PlanCookie{{Name: "seed_cookie", Value: secretCookie}},
					WithHeaders: []plan.PlanHeader{{Key: "Authorization", Value: "Bearer " + secretHeader}},
					WithBody:    []plan.PlanBody{{Key: "password", Value: secretBodyKV}},
				},
				{
					Name:   "submit-order",
					Path:   "/api/orders",
					Method: "PUT",
					Body:   secretRaw,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSuite: %v", err)
	}

	dir := t.TempDir()
	tracePath := filepath.Join(dir, "plan-trace.jsonl")
	tw, err := NewTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}

	store := forensics.NewStore(10, 3)
	store.Save(forensics.Artifact{
		Fingerprint:   forensics.Fingerprint("/api/orders", 500, "http-500"),
		CapturedAt:    time.Now(),
		URL:           "http://127.0.0.1:8080/api/orders",
		Path:          "/api/orders",
		StatusCode:    500,
		TriggerReason: "http-500",
		CaptureMode:   forensics.ModeHTTPOnly,
		Session: forensics.SessionSnapshot{
			LemmingNumber:  1,
			LemmingID:      "lem-auth-1",
			LemmingName:    "Arcade Runner",
			Source:         "http-swarm",
			NavigationMode: "plan",
			PlanName:       "auth-buyer",
			PlanStep:       "submit-order",
			Method:         "PUT",
			Role:           swarm.RolePlan,
			VisitSequence:  2,
			CookieJar: forensics.CookieJarSnapshot{
				Health:        "healthy",
				TotalCookies:  1,
				ActiveCookies: 1,
				Cookies: []forensics.CookieStatus{
					{Name: "seed_cookie", Domain: "127.0.0.1", Path: "/", Fingerprint: "abcd1234", RawValue: secretCookie},
				},
			},
		},
		ErrorBody: plan.ExtractedError{Format: "json", Summary: "order error"},
	})

	cfg := swarm.Config{
		Hit:       "http://127.0.0.1:8080/",
		Terrain:   1,
		Pack:      2,
		Limit:     2,
		Until:     5 * time.Second,
		Plan:      suite,
		RogueRate: 0.05,
		Version:   "2.0.0",
		TraceFile: tracePath,
	}
	rep := New(cfg)
	rep.SetTrace(tw)
	rep.SetForensics(store)

	rep.RecordVisit(&swarm.Visit{
		LemmingID:     "lem-auth-1",
		LemmingNumber: 1,
		Sequence:      1,
		Method:        "POST",
		PlanName:      "auth-buyer",
		Role:          swarm.RolePlan,
		Step:          "sign-in",
		StepIndex:     1,
		URL:           "http://127.0.0.1:8080/api/login?api_key=" + secretQuery,
		StatusCode:    200,
		CookieNames:   []string{"seed_cookie"},
		Duration:      18 * time.Millisecond,
	})
	rep.RecordVisit(&swarm.Visit{
		LemmingID:     "lem-auth-1",
		LemmingNumber: 1,
		Sequence:      2,
		Method:        "PUT",
		PlanName:      "auth-buyer",
		Role:          swarm.RolePlan,
		Step:          "submit-order",
		StepIndex:     2,
		URL:           "http://127.0.0.1:8080/api/orders",
		StatusCode:    500,
		Failed:        true,
		Duration:      42 * time.Millisecond,
	})
	rep.Ingest(swarm.LifeLog{
		Identity:           swarm.Identity{Number: 1, ID: "lem-auth-1", Name: "Arcade Runner", Role: swarm.RolePlan, PlanName: "auth-buyer"},
		Role:               swarm.RolePlan,
		PlanName:           "auth-buyer",
		PlanStepsCompleted: 2,
		PlanStepsTotal:     2,
		PlanPassed:         false,
		Streamed:           true,
		TotalVisits:        2,
		FailedVisits:       1,
		FirstFailureAt:     time.Now(),
		ExitReason:         swarm.ExitPlanFailed,
		Visits: []swarm.Visit{
			{Sequence: 1, Method: "POST", Step: "sign-in", URL: "http://127.0.0.1:8080/api/login?api_key=" + secretQuery, StatusCode: 200, Duration: 18 * time.Millisecond},
			{Sequence: 2, Method: "PUT", Step: "submit-order", URL: "http://127.0.0.1:8080/api/orders", StatusCode: 500, Failed: true, Duration: 42 * time.Millisecond},
		},
	})
	rep.CloseTrace()

	rendered, err := rep.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Verify Markdown rendering contains Plan Execution, persona heading, methods, paths, and forensics plan metadata.
	for _, want := range []string{
		"## plan execution",
		"### auth-buyer",
		"| 1 | sign-in | POST | /api/login |",
		"| 2 | submit-order | PUT | /api/orders |",
		"status 500",
		"role plan",
		"plan auth-buyer / submit-order",
	} {
		if !strings.Contains(rendered.Markdown, want) {
			t.Errorf("Markdown missing expected content %q:\n%s", want, rendered.Markdown)
		}
	}

	// Verify HTML rendering contains Plan execution, persona, methods, paths, and forensics plan metadata.
	for _, want := range []string{
		"Plan execution",
		"auth-buyer",
		"sign-in",
		"submit-order",
		"/api/login",
		"/api/orders",
		"m-post",
		"m-put",
		"status 500",
	} {
		if !strings.Contains(rendered.HTML, want) {
			t.Errorf("HTML missing expected content %q", want)
		}
	}

	// Verify JSON unmarshals PlanReport accurately.
	var parsed Data
	if err := json.Unmarshal(rendered.JSON, &parsed); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}
	if parsed.Plan == nil || len(parsed.Plan.Personas) != 1 || len(parsed.Plan.Personas[0].Steps) != 2 {
		t.Fatalf("JSON PlanReport mismatch: %+v", parsed.Plan)
	}
	if parsed.Plan.Personas[0].Steps[0].Path != "/api/login" {
		t.Errorf("JSON step 1 Path = %q, want /api/login (query stripped)", parsed.Plan.Personas[0].Steps[0].Path)
	}

	// Verify JSONL trace records populate Method, PlanName, Role and redact query parameters.
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("ReadFile trace: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(traceBytes)), "\n")
	if len(lines) != 2 {
		t.Fatalf("trace line count = %d, want 2", len(lines))
	}
	var tr0, tr1 VisitTrace
	if err := json.Unmarshal([]byte(lines[0]), &tr0); err != nil {
		t.Fatalf("unmarshal trace[0]: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &tr1); err != nil {
		t.Fatalf("unmarshal trace[1]: %v", err)
	}
	if tr0.Method != "POST" || tr0.PlanName != "auth-buyer" || tr0.Role != swarm.RolePlan || tr0.Step != "sign-in" || tr0.URL != "http://127.0.0.1:8080/api/login" {
		t.Errorf("trace[0] mismatch: %+v", tr0)
	}
	if tr1.Method != "PUT" || tr1.PlanName != "auth-buyer" || tr1.Role != swarm.RolePlan || tr1.Step != "submit-order" {
		t.Errorf("trace[1] mismatch: %+v", tr1)
	}

	// SEC-10: verify none of with-cookies / with-headers / with-body / raw body / query values ever leak!
	allOutputs := rendered.Markdown + rendered.HTML + rendered.HTMLForEmail + string(rendered.JSON) + string(traceBytes)
	for _, secret := range []string{secretCookie, secretHeader, secretBodyKV, secretRaw, secretQuery} {
		if strings.Contains(allOutputs, secret) {
			t.Fatalf("SEC-10 VIOLATION: plan secret %q leaked into report or VisitTrace!", secret)
		}
	}
}

func TestPlanSummary_Formatting(t *testing.T) {
	tests := []struct {
		name string
		in   *PlanReport
		want string
	}{
		{name: "nil plan", in: nil, want: ""},
		{name: "empty personas", in: &PlanReport{}, want: ""},
		{name: "blank names ignored", in: &PlanReport{Personas: []PersonaReport{{Name: ""}, {Name: ""}}}, want: ""},
		{name: "single persona", in: &PlanReport{Personas: []PersonaReport{{Name: "buyer"}}}, want: "buyer"},
		{name: "three personas inline", in: &PlanReport{Personas: []PersonaReport{{Name: "a"}, {Name: "b"}, {Name: "c"}}}, want: "a, b, c"},
		{name: "four personas summarized", in: &PlanReport{Personas: []PersonaReport{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}}, want: "4 personas"},
		{name: "ten personas ignores blanks", in: &PlanReport{Personas: []PersonaReport{
			{Name: "p1"}, {Name: ""}, {Name: "p2"}, {Name: "p3"}, {Name: "p4"},
		}}, want: "4 personas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planSummary(tt.in); got != tt.want {
				t.Errorf("planSummary(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderHTML_LayoutOverflowSafetyAndMultiPersonaFormatting(t *testing.T) {
	personaNames := []string{
		"future-premium-explorer",
		"ancient-free-explorer",
		"press-reader",
		"manifesto-and-legal",
		"newsletter-subscriber",
		"telemetry-api-client",
		"seo-and-agent-crawler",
		"admin-auth-guard-prober",
		"dead-link-404-and-health",
		"looping-reader",
	}
	personas := make([]PersonaReport, 0, len(personaNames))
	for i, name := range personaNames {
		personas = append(personas, PersonaReport{
			Name:       name,
			Weight:     10,
			TotalSteps: 1,
			Lemmings:   int64(i + 1),
			Passed:     int64(i + 1),
			PassRate:   100,
			Steps: []PlanStepReport{{
				Index:  1,
				Name:   "step-" + name,
				Method: "GET",
				Path:   "/projects/" + name,
				Hits:   1,
				Passed: 1,
				P50:    time.Millisecond,
				P95:    2 * time.Millisecond,
				P99:    3 * time.Millisecond,
			}},
		})
	}

	d := Data{
		SchemaVersion: SchemaVersion,
		Version:       "2.0.1",
		GeneratedAt:   time.Date(2026, 11, 28, 4, 25, 2, 0, time.UTC),
		Duration:      55 * time.Second,
		Config: swarm.Config{
			Hit:           "http://127.0.0.1:8080/",
			Terrain:       6,
			Pack:          10,
			Limit:         60,
			Until:         45 * time.Second,
			Ramp:          10 * time.Second,
			RampShape:     "fibonacci",
			ThinkMin:      300 * time.Millisecond,
			ThinkMax:      1200 * time.Millisecond,
			Assets:        true,
			AssetsPerPage: 12,
		},
		TotalLemmings:     60,
		LemmingsStarted:   60,
		LemmingsCompleted: 60,
		Sessions:          60,
		Rescued:           60,
		RescuedPercent:    100,
		PercentileMethod:  "exact (nearest rank)",
		Plan: &PlanReport{
			RogueRate:    0.05,
			PlanLemmings: 57,
			PlanPassed:   57,
			PlanPassRate: 100,
			Personas:     personas,
		},
		StatusCodes: []CountRow{{Label: "200", Count: 532, Share: 1.0}},
		Paths: []PathReport{
			{
				URL:      "http://127.0.0.1:8080/",
				Hits:     86,
				Bytes:    "10.15 MB",
				XX2:      86,
				P50:      1700 * time.Microsecond,
				P95:      5900 * time.Microsecond,
				P99:      8200 * time.Microsecond,
				ColdHits: 49,
				SetupP50: 560 * time.Microsecond,
				WarmP50:  1200 * time.Microsecond,
				HitShare: 1.0,
			},
			{
				URL:      "http://127.0.0.1:8080/projects/graphicle-namespace",
				Hits:     1,
				Bytes:    "81.63 KB",
				XX2:      1,
				P50:      2100 * time.Microsecond,
				P95:      2100 * time.Microsecond,
				P99:      2100 * time.Microsecond,
				HitShare: 1.0 / 86.0,
			},
		},
		TraceFile:    "/var/log/lemmings/very-long-trace-directory-name/visits-2026-11-28.jsonl",
		TraceWritten: 532,
		ExitReasons:  []CountRow{{Label: "plan-complete", Count: 57}, {Label: "lifespan", Count: 3}},
		Forensics: ForensicsReport{
			Stats: forensics.Stats{Mode: forensics.ModeCDPReplay, Browser: "Google Chrome"},
		},
	}

	rendered, err := RenderData(d, "multi-persona", "127.0.0.1-8080")
	if err != nil {
		t.Fatalf("RenderData: %v", err)
	}
	html := rendered.HTML

	// Hero summarizes >3 personas cleanly while preserving full list in title tooltip.
	wantTitle := `title="` + strings.Join(personaNames, ", ") + `"`
	if !strings.Contains(html, `<strong `+wantTitle+`>10 personas</strong>`) {
		t.Errorf("hero subtitle missing summarized 10-persona label with full title tooltip")
	}

	// Configuration & Evidence use fixed KV tables without nowrap td.num on text values.
	for _, want := range []string{
		`table.kv { table-layout:fixed; width:100%; }`,
		`<table class="kv">`,
		`<table class="paths-table">`,
		`<div class="tag-list">`,
		`<span class="tag-item">future-premium-explorer</span>`,
		`<span class="tag-item">looping-reader</span>`,
		`<div class="bar-track"><div class="bar"`,
		`<td class="url">http://127.0.0.1:8080/<br><span class="sub-note">warm p50 1.2 ms · setup 0.56 ms (49 cold)</span></td>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing expected formatting markup %q", want)
		}
	}

	// Cold/warm note must NOT remain inside the nowrap p50 numeric cell.
	if strings.Contains(html, `<td class="num">1.7 ms<br>`) {
		t.Errorf("cold/warm timing note must not live inside nowrap p50 td.num cell")
	}

	// Configuration navigation cell must NOT use nowrap td.num.
	if strings.Contains(html, `<tr><td>navigation</td><td class="num">`) {
		t.Errorf("Configuration navigation cell must not use nowrap td.num")
	}

	// Verify XSS escaping in hero title attribute and Configuration tag items.
	const xssName = `evil"><script>alert(1)</script>`
	d.Plan.Personas[0].Name = xssName
	escapedOut, err := RenderData(d, "multi-persona-xss", "127.0.0.1-8080")
	if err != nil {
		t.Fatalf("RenderData (XSS): %v", err)
	}
	if strings.Contains(escapedOut.HTML, xssName) {
		t.Fatal("XSS payload in persona name appeared unescaped in rendered HTML")
	}
}
