package metrics

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

func TestPrometheusObserver_MetricsCardinalityHostGuardAndShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	bus := events.NewBus()
	defer bus.Close()

	obs := NewPrometheusObserver(swarm.Config{
		MetricsPort:     port,
		MetricsURLLabel: "path",
	})
	if err := obs.Attach(context.Background(), bus); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = obs.Detach() }()

	// Emit lifecycle, visit, forensics, and scout events.
	bus.Emit(events.Event{Kind: events.TerrainOnline})
	bus.Emit(events.Event{Kind: events.LemmingBorn})
	bus.Emit(events.Event{Kind: events.VisitComplete, URL: "http://127.0.0.1/home", StatusCode: 200, BytesIn: 512, Duration: 20 * time.Millisecond})
	bus.Emit(events.Event{Kind: events.ArtifactCaptured, CaptureMode: "cdp_replay", ConsoleErrors: 3})
	bus.Emit(events.Event{Kind: events.ArtifactDropped, DropReason: "deduped"})
	bus.Emit(events.Event{Kind: events.ScoutStarted})
	bus.Emit(events.Event{Kind: events.ScoutDone})
	bus.Emit(events.Event{Kind: events.LemmingDied})
	bus.Emit(events.Event{Kind: events.TerrainDone})

	// Scrape /metrics with loopback Host.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", resp.StatusCode)
	}
	text := string(body)
	for _, metric := range []string{
		"lemmings_alive",
		"lemmings_completed_total",
		"lemmings_visits_total",
		"lemmings_bytes_total 512",
		`lemmings_artifacts_captured_total{mode="cdp_replay"} 1`,
		`lemmings_artifacts_dropped_total{reason="deduped"} 1`,
		"lemmings_js_console_errors_total 3",
		"lemmings_scouts_alive 0",
	} {
		if !strings.Contains(text, metric) {
			t.Errorf("missing expected metric %q in /metrics output", metric)
		}
	}

	// SEC-04: Reject non-loopback Host header (DNS rebinding protection).
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	req.Host = "evil.attacker.test"
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("evil Host request: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("SEC-04: non-loopback Host status = %d, want 403", resp2.StatusCode)
	}

	// URL cardinality cap at 999 -> "other".
	for i := range maxURLLabelCardinality + 5 {
		got := obs.urlLabel(fmt.Sprintf("http://127.0.0.1/p/%d", i))
		if i >= maxURLLabelCardinality && got != "other" {
			t.Fatalf("expected 'other' at index %d, got %q", i, got)
		}
	}

	// Synchronous bind failure on already-bound port.
	obs2 := NewPrometheusObserver(swarm.Config{MetricsPort: port, MetricsURLLabel: "none"})
	if err := obs2.Attach(context.Background(), bus); err == nil {
		_ = obs2.Detach()
		t.Error("expected synchronous error when metrics port is already in use")
	}
}

func TestPrometheusObserver_CancelledExclusionAndV2Metrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	bus := events.NewBus()
	defer bus.Close()

	obs := NewPrometheusObserver(swarm.Config{
		MetricsPort:     port,
		MetricsURLLabel: "path",
	})
	if err := obs.Attach(context.Background(), bus); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = obs.Detach() }()

	// Normal completed 200 visit.
	bus.Emit(events.Event{
		Kind:       events.VisitComplete,
		URL:        "http://127.0.0.1/page",
		StatusCode: 200,
		BytesIn:    100,
		Duration:   15 * time.Millisecond,
	})
	// Cancelled 500 visit: must count cancelled & bytes, but NOT visits{status_class="5xx"}, failed, or bans.
	bus.Emit(events.Event{
		Kind:       events.VisitError,
		URL:        "http://127.0.0.1/page",
		StatusCode: 500,
		BytesIn:    40,
		Duration:   5 * time.Millisecond,
		Cancelled:  true,
		Failed:     true,
		Banned:     true,
	})
	// Real 429 and 403 bans, plus an out-of-band 500 with Banned=true (must be ignored by bounded bans metric).
	bus.Emit(events.Event{Kind: events.VisitError, URL: "http://127.0.0.1/api", StatusCode: 429, Failed: true, Banned: true, Duration: 8 * time.Millisecond})
	bus.Emit(events.Event{Kind: events.VisitError, URL: "http://127.0.0.1/api", StatusCode: 403, Failed: true, Banned: true, Duration: 9 * time.Millisecond})
	bus.Emit(events.Event{Kind: events.VisitError, URL: "http://127.0.0.1/api", StatusCode: 500, Failed: true, Banned: true, Duration: 7 * time.Millisecond})

	// Asset events (known class, 403 error, unbounded class -> "other", status 0 -> "000").
	bus.Emit(events.Event{
		Kind:       events.AssetComplete,
		AssetClass: "style",
		StatusCode: 200,
		Duration:   25 * time.Millisecond,
	})
	bus.Emit(events.Event{
		Kind:       events.AssetError,
		AssetClass: "image",
		StatusCode: 403,
		Failed:     true,
		Banned:     true,
		Duration:   12 * time.Millisecond,
	})
	bus.Emit(events.Event{
		Kind:       events.AssetError,
		AssetClass: "high-cardinality-evil-class",
		StatusCode: 0,
		Failed:     true,
		Duration:   5 * time.Millisecond,
	})

	// Queue polls.
	bus.Emit(events.Event{Kind: events.QueuePoll, StatusCode: 200})
	bus.Emit(events.Event{Kind: events.QueuePoll, StatusCode: 200})
	bus.Emit(events.Event{Kind: events.QueuePoll, StatusCode: 429})

	// Scout visits (ok and failed).
	bus.Emit(events.Event{Kind: events.ScoutVisit, StatusCode: 200, Failed: false})
	bus.Emit(events.Event{Kind: events.ScoutVisit, StatusCode: 500, Failed: true})

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", resp.StatusCode)
	}
	text := string(body)

	for _, want := range []string{
		`lemmings_bytes_total 140`,
		`lemmings_cancelled_visits_total 1`,
		`lemmings_failed_visits_total 3`,
		`lemmings_visits_total{status_class="2xx"} 1`,
		`lemmings_visits_total{status_class="4xx"} 2`,
		`lemmings_visits_total{status_class="5xx"} 1`,
		`lemmings_assets_total{class="style",status_class="2xx"} 1`,
		`lemmings_assets_total{class="image",status_class="4xx"} 1`,
		`lemmings_assets_total{class="other",status_class="000"} 1`,
		`lemmings_asset_duration_seconds_count 3`,
		`lemmings_queue_polls_total 3`,
		`lemmings_bans_total{status="429"} 1`,
		`lemmings_bans_total{status="403"} 1`,
		`lemmings_scout_visits_total{outcome="ok"} 1`,
		`lemmings_scout_visits_total{outcome="failed"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing expected metric line %q\nfull metrics:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{
		`high-cardinality-evil-class`,
		`lemmings_bans_total{status="500"}`,
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("forbidden unbounded label %q appeared in /metrics output", forbidden)
		}
	}
}

func TestPrometheusObserver_PlanAndRogueCohortMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	bus := events.NewBus()
	defer bus.Close()

	obs := NewPrometheusObserver(swarm.Config{
		MetricsPort:     port,
		MetricsURLLabel: "none",
	})
	if err := obs.Attach(context.Background(), bus); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = obs.Detach() }()

	// Emit plan passed (2), plan failed (1), rogue (1), and non-plan (1) deaths.
	bus.Emit(events.Event{Kind: events.LemmingDied, Role: swarm.RolePlan, PlanName: "auth", PlanPassed: true})
	bus.Emit(events.Event{Kind: events.LemmingDied, Role: swarm.RolePlan, PlanName: "auth", PlanPassed: true})
	bus.Emit(events.Event{Kind: events.LemmingDied, Role: swarm.RolePlan, PlanName: "auth", PlanPassed: false})
	bus.Emit(events.Event{Kind: events.LemmingDied, Role: swarm.RoleRogue})
	bus.Emit(events.Event{Kind: events.LemmingDied}) // non-plan run: must not increment plan or rogue counters

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", resp.StatusCode)
	}
	text := string(body)
	for _, want := range []string{
		`lemmings_completed_total 5`,
		`lemmings_plan_lemmings_total{outcome="passed"} 2`,
		`lemmings_plan_lemmings_total{outcome="failed"} 1`,
		`lemmings_rogue_lemmings_total 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing expected metric %q\nfull metrics:\n%s", want, text)
		}
	}
}
