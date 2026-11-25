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
