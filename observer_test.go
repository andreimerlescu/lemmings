package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ── NewPrometheusObserver ─────────────────────────────────────────────────────

// TestNewPrometheusObserver_Constructs verifies that NewPrometheusObserver
// returns a non-nil observer with a populated registry and all collectors
// initialised before Attach is called.
func TestNewPrometheusObserver_Constructs(t *testing.T) {
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	if o == nil {
		t.Fatal("NewPrometheusObserver returned nil")
	}
	if o.registry == nil {
		t.Error("registry should not be nil")
	}
	if o.alive == nil {
		t.Error("alive gauge should not be nil")
	}
	if o.completed == nil {
		t.Error("completed counter should not be nil")
	}
	if o.failed == nil {
		t.Error("failed counter should not be nil")
	}
	if o.visits == nil {
		t.Error("visits counter should not be nil")
	}
	if o.visitDuration == nil {
		t.Error("visitDuration histogram should not be nil")
	}
	if o.bytesTotal == nil {
		t.Error("bytesTotal counter should not be nil")
	}
	if o.waitingRoom == nil {
		t.Error("waitingRoom counter should not be nil")
	}
	if o.waitingRoomDuration == nil {
		t.Error("waitingRoomDuration histogram should not be nil")
	}
	if o.terrainsOnline == nil {
		t.Error("terrainsOnline gauge should not be nil")
	}
	if o.droppedLogs == nil {
		t.Error("droppedLogs counter should not be nil")
	}
	if o.overflowLogs == nil {
		t.Error("overflowLogs counter should not be nil")
	}
	if o.server != nil {
		t.Error("server should be nil before Attach is called")
	}
	if o.unsub != nil {
		t.Error("unsub should be nil before Attach is called")
	}
}

// TestNewPrometheusObserver_IsolatedRegistry verifies that each observer
// instance uses its own isolated registry rather than the global default.
// Two observers must be constructable in the same process without collision.
func TestNewPrometheusObserver_IsolatedRegistry(t *testing.T) {
	cfg := testObserverConfig()

	// Must not panic — two observers with isolated registries can coexist
	o1 := NewPrometheusObserver(cfg)
	o2 := NewPrometheusObserver(cfg)

	if o1.registry == o2.registry {
		t.Error("each observer should have its own isolated registry")
	}
}

// ── Name ──────────────────────────────────────────────────────────────────────

// TestPrometheusObserver_Name verifies that Name returns the expected
// human-readable identifier used in log output and observer lists.
func TestPrometheusObserver_Name(t *testing.T) {
	o := NewPrometheusObserver(testObserverConfig())
	if o.Name() != "prometheus" {
		t.Errorf("expected name 'prometheus', got %q", o.Name())
	}
}

// ── Attach ────────────────────────────────────────────────────────────────────

// TestPrometheusObserver_Attach_RegistersSubscriber verifies that Attach
// registers a subscriber on the EventBus such that events emitted after
// Attach returns are received by the observer.
func TestPrometheusObserver_Attach_RegistersSubscriber(t *testing.T) {
	bus, _, unsub := testBus()
	defer unsub()
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	defer o.Detach()

	// Emit a born event and verify the alive gauge incremented
	bus.Emit(Event{Kind: EventLemmingBorn})

	// Give the synchronous bus time to deliver
	time.Sleep(10 * time.Millisecond)

	val := gaugeValue(t, o.alive.With(prometheus.Labels{}))
	if val != 1 {
		t.Errorf("expected alive=1 after EventLemmingBorn, got %v", val)
	}
}

// TestPrometheusObserver_Attach_StartsServer verifies that after Attach
// returns, the /metrics HTTP endpoint responds with 200 OK.
func TestPrometheusObserver_Attach_StartsServer(t *testing.T) {
	bus, _, unsub := testBus()
	defer unsub()
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	defer o.Detach()

	url := fmt.Sprintf("http://localhost:%d/metrics", cfg.MetricsPort)
	waitForServer(t, url, 20, 50*time.Millisecond)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /metrics error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// TestPrometheusObserver_Attach_RedirectsRoot verifies that a GET to /
// on the metrics server redirects to /metrics.
func TestPrometheusObserver_Attach_RedirectsRoot(t *testing.T) {
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewEventBus()
	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	defer o.Detach()

	url := fmt.Sprintf("http://localhost:%d/", cfg.MetricsPort)
	waitForServer(t, url, 20, 50*time.Millisecond)

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // do not follow redirects
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET / error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("expected 301 redirect from /, got %d", resp.StatusCode)
	}
}

// ── Detach ────────────────────────────────────────────────────────────────────

// TestPrometheusObserver_Detach_UnregistersSubscriber verifies that after
// Detach is called, events emitted on the bus no longer reach the observer.
func TestPrometheusObserver_Detach_UnregistersSubscriber(t *testing.T) {
	bus := NewEventBus()
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}

	// Confirm it is receiving events
	bus.Emit(Event{Kind: EventLemmingBorn})
	time.Sleep(10 * time.Millisecond)

	if err := o.Detach(); err != nil {
		t.Fatalf("Detach error: %v", err)
	}

	// Emit another born event after detach — should not increment
	bus.Emit(Event{Kind: EventLemmingBorn})
	time.Sleep(10 * time.Millisecond)

	// alive should still be 1, not 2
	val := gaugeValue(t, o.alive.With(prometheus.Labels{}))
	if val != 1 {
		t.Errorf("expected alive=1 (pre-detach value), got %v after post-detach emit", val)
	}
}

// TestPrometheusObserver_Detach_StopsServer verifies that after Detach,
// the /metrics endpoint refuses connections.
func TestPrometheusObserver_Detach_StopsServer(t *testing.T) {
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewEventBus()
	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}

	url := fmt.Sprintf("http://localhost:%d/metrics", cfg.MetricsPort)
	waitForServer(t, url, 20, 50*time.Millisecond)

	if err := o.Detach(); err != nil {
		t.Fatalf("Detach error: %v", err)
	}

	// Give server time to shut down
	time.Sleep(100 * time.Millisecond)

	_, err := http.Get(url)
	if err == nil {
		t.Error("expected connection refused after Detach, got nil error")
	}
}

// TestPrometheusObserver_Detach_Idempotent verifies that calling Detach
// multiple times does not panic or return an error after the first call.
func TestPrometheusObserver_Detach_Idempotent(t *testing.T) {
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewEventBus()
	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}

	if err := o.Detach(); err != nil {
		t.Fatalf("first Detach error: %v", err)
	}
	if err := o.Detach(); err != nil {
		t.Fatalf("second Detach should not error, got: %v", err)
	}
	o.Detach() // third — must not panic
}

// ── handleEvent — lemming lifecycle ───────────────────────────────────────────

// TestObserver_LemmingBorn_IncrementsAlive verifies that EventLemmingBorn
// increments the lemmings_alive gauge by exactly 1.
func TestObserver_LemmingBorn_IncrementsAlive(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventLemmingBorn})
	time.Sleep(10 * time.Millisecond)

	if val := gaugeValue(t, o.alive.With(prometheus.Labels{})); val != 1 {
		t.Errorf("expected alive=1, got %v", val)
	}
}

// TestObserver_LemmingDied_DecrementsAlive verifies that EventLemmingDied
// decrements the lemmings_alive gauge and increments lemmings_completed_total.
func TestObserver_LemmingDied_DecrementsAlive(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Emit(Event{Kind: EventLemmingDied})
	time.Sleep(10 * time.Millisecond)

	if val := gaugeValue(t, o.alive.With(prometheus.Labels{})); val != 1 {
		t.Errorf("expected alive=1 after 2 born + 1 died, got %v", val)
	}
	if val := counterValue(t, o.completed); val != 1 {
		t.Errorf("expected completed=1, got %v", val)
	}
}

// TestObserver_LemmingFailed_IncrementsCounter verifies that
// EventLemmingFailed increments lemmings_failed_total.
func TestObserver_LemmingFailed_IncrementsCounter(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventLemmingFailed})
	bus.Emit(Event{Kind: EventLemmingFailed})
	time.Sleep(10 * time.Millisecond)

	if val := counterValue(t, o.failed); val != 2 {
		t.Errorf("expected failed=2, got %v", val)
	}
}

// TestObserver_LemmingDied_AccumulatesBytes verifies that EventLemmingDied
// with a non-zero BytesIn increments lemmings_bytes_total.
func TestObserver_LemmingDied_AccumulatesBytes(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Emit(Event{Kind: EventLemmingDied, BytesIn: 1024})
	time.Sleep(10 * time.Millisecond)

	if val := counterValue(t, o.bytesTotal); val != 1024 {
		t.Errorf("expected bytesTotal=1024, got %v", val)
	}
}

// ── handleEvent — visit outcomes ──────────────────────────────────────────────

// TestObserver_VisitComplete_2xx verifies that EventVisitComplete with a
// 2xx status code increments the 2xx visit counter.
func TestObserver_VisitComplete_2xx(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 200,
		URL:        "http://example.com/",
		Duration:   10 * time.Millisecond,
	})
	time.Sleep(10 * time.Millisecond)

	if val := counterVecValue(t, o.visits, "status_class", "2xx"); val != 1 {
		t.Errorf("expected 2xx visits=1, got %v", val)
	}
}

// TestObserver_VisitComplete_3xx verifies that a 3xx status code increments
// the 3xx visit counter.
func TestObserver_VisitComplete_3xx(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 301,
		URL:        "http://example.com/old",
		Duration:   5 * time.Millisecond,
	})
	time.Sleep(10 * time.Millisecond)

	if val := counterVecValue(t, o.visits, "status_class", "3xx"); val != 1 {
		t.Errorf("expected 3xx visits=1, got %v", val)
	}
}

// TestObserver_VisitComplete_4xx verifies that a 4xx status code increments
// the 4xx visit counter.
func TestObserver_VisitComplete_4xx(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 404,
		URL:        "http://example.com/missing",
		Duration:   3 * time.Millisecond,
	})
	time.Sleep(10 * time.Millisecond)

	if val := counterVecValue(t, o.visits, "status_class", "4xx"); val != 1 {
		t.Errorf("expected 4xx visits=1, got %v", val)
	}
}

// TestObserver_VisitComplete_5xx verifies that a 5xx status code increments
// the 5xx visit counter.
func TestObserver_VisitComplete_5xx(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 500,
		URL:        "http://example.com/error",
		Duration:   50 * time.Millisecond,
	})
	time.Sleep(10 * time.Millisecond)

	if val := counterVecValue(t, o.visits, "status_class", "5xx"); val != 1 {
		t.Errorf("expected 5xx visits=1, got %v", val)
	}
}

// TestObserver_VisitComplete_UnknownStatus verifies that a zero or unknown
// status code increments the "unknown" visit counter without panicking.
func TestObserver_VisitComplete_UnknownStatus(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 0,
		URL:        "http://example.com/",
	})
	time.Sleep(10 * time.Millisecond)

	if val := counterVecValue(t, o.visits, "status_class", "unknown"); val != 1 {
		t.Errorf("expected unknown visits=1, got %v", val)
	}
}

// TestObserver_VisitComplete_RecordsDuration verifies that EventVisitComplete
// with a non-zero Duration causes an observation on the visit duration
// histogram. We verify this by checking the histogram sample count.
func TestObserver_VisitComplete_RecordsDuration(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 200,
		URL:        "http://example.com/",
		Duration:   25 * time.Millisecond,
	})
	time.Sleep(10 * time.Millisecond)

	count := histogramSampleCount(t, o.visitDuration,
		prometheus.Labels{"url": "http://example.com/"})
	if count != 1 {
		t.Errorf("expected 1 duration observation, got %d", count)
	}
}

// TestObserver_VisitComplete_ZeroDurationSkipped verifies that a visit with
// Duration=0 does not produce a histogram observation — zero duration
// indicates the request never completed and should not skew percentiles.
func TestObserver_VisitComplete_ZeroDurationSkipped(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 200,
		URL:        "http://example.com/",
		Duration:   0,
	})
	time.Sleep(10 * time.Millisecond)

	count := histogramSampleCount(t, o.visitDuration,
		prometheus.Labels{"url": "http://example.com/"})
	if count != 0 {
		t.Errorf("expected 0 observations for zero-duration visit, got %d", count)
	}
}

// ── handleEvent — waiting room ────────────────────────────────────────────────

// TestObserver_WaitingRoom_IncrementsCounter verifies that EventWaitingRoom
// increments lemmings_waiting_room_total.
func TestObserver_WaitingRoom_IncrementsCounter(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventWaitingRoom})
	bus.Emit(Event{Kind: EventWaitingRoom})
	bus.Emit(Event{Kind: EventWaitingRoom})
	time.Sleep(10 * time.Millisecond)

	if val := counterValue(t, o.waitingRoom); val != 3 {
		t.Errorf("expected waitingRoom=3, got %v", val)
	}
}

// TestObserver_WaitingRoom_RecordsDuration verifies that EventWaitingRoom
// with a non-zero Duration produces a histogram observation on
// lemmings_waiting_room_duration_seconds.
func TestObserver_WaitingRoom_RecordsDuration(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{
		Kind:     EventWaitingRoom,
		Duration: 45 * time.Second,
	})
	time.Sleep(10 * time.Millisecond)

	count := simpleHistogramSampleCount(t, o.waitingRoomDuration)
	if count != 1 {
		t.Errorf("expected 1 waiting room duration observation, got %d", count)
	}
}

// TestObserver_WaitingRoom_ZeroDurationSkipped verifies that a waiting room
// event with Duration=0 does not produce a histogram observation.
func TestObserver_WaitingRoom_ZeroDurationSkipped(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventWaitingRoom, Duration: 0})
	time.Sleep(10 * time.Millisecond)

	count := simpleHistogramSampleCount(t, o.waitingRoomDuration)
	if count != 0 {
		t.Errorf("expected 0 observations for zero-duration waiting room, got %d", count)
	}
}

// ── handleEvent — terrain lifecycle ──────────────────────────────────────────

// TestObserver_TerrainOnline_IncrementsGauge verifies that EventTerrainOnline
// increments lemmings_terrains_online.
func TestObserver_TerrainOnline_IncrementsGauge(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventTerrainOnline})
	bus.Emit(Event{Kind: EventTerrainOnline})
	time.Sleep(10 * time.Millisecond)

	if val := simpleGaugeValue(t, o.terrainsOnline); val != 2 {
		t.Errorf("expected terrainsOnline=2, got %v", val)
	}
}

// TestObserver_TerrainDone_DecrementsGauge verifies that EventTerrainDone
// decrements lemmings_terrains_online.
func TestObserver_TerrainDone_DecrementsGauge(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventTerrainOnline})
	bus.Emit(Event{Kind: EventTerrainOnline})
	bus.Emit(Event{Kind: EventTerrainOnline})
	bus.Emit(Event{Kind: EventTerrainDone})
	time.Sleep(10 * time.Millisecond)

	if val := simpleGaugeValue(t, o.terrainsOnline); val != 2 {
		t.Errorf("expected terrainsOnline=2 after 3 online + 1 done, got %v", val)
	}
}

// ── handleEvent — channel pressure ───────────────────────────────────────────

// TestObserver_LogDropped_IncrementsCounter verifies that EventLogDropped
// increments lemmings_dropped_logs_total.
func TestObserver_LogDropped_IncrementsCounter(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventLogDropped})
	bus.Emit(Event{Kind: EventLogDropped})
	time.Sleep(10 * time.Millisecond)

	if val := counterValue(t, o.droppedLogs); val != 2 {
		t.Errorf("expected droppedLogs=2, got %v", val)
	}
}

// TestObserver_LogOverflow_IncrementsCounter verifies that EventLogOverflow
// increments lemmings_overflow_logs_total.
func TestObserver_LogOverflow_IncrementsCounter(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()

	bus.Emit(Event{Kind: EventLogOverflow})
	time.Sleep(10 * time.Millisecond)

	if val := counterValue(t, o.overflowLogs); val != 1 {
		t.Errorf("expected overflowLogs=1, got %v", val)
	}
}

// ── URL label behaviour ───────────────────────────────────────────────────────

// TestURLLabel_FullMode verifies that MetricsURLLabel="full" uses the
// complete URL as the label value.
func TestURLLabel_FullMode(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "full"
	o := NewPrometheusObserver(cfg)

	label := o.urlLabel("https://example.com/about?ref=nav")
	if label != "https://example.com/about?ref=nav" {
		t.Errorf("expected full URL as label, got %q", label)
	}
}

// TestURLLabel_PathMode verifies that MetricsURLLabel="path" strips the
// scheme, host, and query string, returning only the path component.
func TestURLLabel_PathMode(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "path"
	o := NewPrometheusObserver(cfg)

	cases := []struct {
		input    string
		expected string
	}{
		{"https://example.com/about", "/about"},
		{"https://example.com/pricing?plan=pro", "/pricing"},
		{"http://localhost:8080/", "/"},
		{"https://example.com/a/b/c", "/a/b/c"},
	}

	for _, tc := range cases {
		got := o.urlLabel(tc.input)
		if got != tc.expected {
			t.Errorf("urlLabel(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestURLLabel_NoneMode verifies that MetricsURLLabel="none" causes
// visitDuration to be constructed with no url label dimension.
func TestURLLabel_NoneMode(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "none"
	o := NewPrometheusObserver(cfg)
	bus, _, unsub := testBus()
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	defer o.Detach()

	// Emit a visit — must not panic with "none" label mode
	bus.Emit(Event{
		Kind:       EventVisitComplete,
		StatusCode: 200,
		URL:        "http://example.com/",
		Duration:   10 * time.Millisecond,
	})
	time.Sleep(10 * time.Millisecond)

	// Verify sample count via no-label histogram
	count := histogramSampleCount(t, o.visitDuration, prometheus.Labels{})
	if count != 1 {
		t.Errorf("expected 1 observation in none-mode histogram, got %d", count)
	}
}

// TestURLLabel_CardinalityCap verifies that once maxURLLabelCardinality
// distinct URL labels have been seen, additional URLs are recorded under
// the synthetic label value "other".
func TestURLLabel_CardinalityCap(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "full"
	o := NewPrometheusObserver(cfg)

	// Fill to cap
	for i := 0; i < maxURLLabelCardinality; i++ {
		label := o.urlLabel(fmt.Sprintf("https://example.com/page-%d", i))
		if label == "other" {
			t.Fatalf("URL %d should not return 'other' before cap is reached", i)
		}
	}

	// The next URL must return "other"
	label := o.urlLabel("https://example.com/one-too-many")
	if label != "other" {
		t.Errorf("expected 'other' after cap reached, got %q", label)
	}
}

// TestURLLabel_CardinalityCap_KnownURLsStillWork verifies that URLs that
// were registered before the cap was reached continue to return their
// correct label value even after the cap is enforced.
func TestURLLabel_CardinalityCap_KnownURLsStillWork(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "full"
	o := NewPrometheusObserver(cfg)

	first := "https://example.com/first"
	o.urlLabel(first) // register before filling

	// Fill to cap with other URLs
	for i := 0; i < maxURLLabelCardinality-1; i++ {
		o.urlLabel(fmt.Sprintf("https://example.com/page-%d", i))
	}

	// Cap is now reached — but first should still return correctly
	label := o.urlLabel(first)
	if label != first {
		t.Errorf("known URL should still return correct label after cap, got %q", label)
	}
}

// TestURLLabel_CardinalityCap_OnlyLoggedOnce verifies that the cardinality
// warning is only logged once — not on every subsequent capped URL.
// We verify this structurally by checking that urlLabelCap is set to true
// after the first overflow and does not change again.
func TestURLLabel_CardinalityCap_OnlyLoggedOnce(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "full"
	o := NewPrometheusObserver(cfg)

	// Fill to cap
	for i := 0; i < maxURLLabelCardinality; i++ {
		o.urlLabel(fmt.Sprintf("https://example.com/page-%d", i))
	}

	// First overflow sets urlLabelCap
	o.urlLabel("https://example.com/overflow-1")
	if !o.urlLabelCap {
		t.Error("urlLabelCap should be true after first overflow")
	}

	// Second overflow — urlLabelCap should still be true, not flipped
	o.urlLabel("https://example.com/overflow-2")
	if !o.urlLabelCap {
		t.Error("urlLabelCap should remain true after subsequent overflows")
	}
}

// ── statusClass ───────────────────────────────────────────────────────────────

// TestStatusClass verifies that statusClass correctly maps HTTP status
// codes to their class strings across all valid ranges.
func TestStatusClass(t *testing.T) {
	cases := []struct {
		code     int
		expected string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{299, "2xx"},
		{300, "3xx"},
		{301, "3xx"},
		{399, "3xx"},
		{400, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{599, "5xx"},
		{0, "unknown"},
		{100, "unknown"},
		{199, "unknown"},
		{600, "unknown"},
	}

	for _, tc := range cases {
		got := statusClass(tc.code)
		if got != tc.expected {
			t.Errorf("statusClass(%d) = %q, want %q", tc.code, got, tc.expected)
		}
	}
}

// ── urlPathOnly ───────────────────────────────────────────────────────────────

// TestURLPathOnly_ExtractsPath verifies that urlPathOnly correctly strips
// scheme, host, and query string from a URL.
func TestURLPathOnly_ExtractsPath(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"https://example.com/about", "/about"},
		{"https://example.com/pricing?plan=pro", "/pricing"},
		{"http://localhost:8080/", "/"},
		{"https://example.com", ""},
		{"not-a-url", "not-a-url"},
	}

	for _, tc := range cases {
		got := urlPathOnly(tc.input)
		if tc.expected == "" {
			// Empty path — urlPathOnly returns raw URL as fallback
			if got != tc.input {
				t.Errorf("urlPathOnly(%q): expected raw URL fallback %q, got %q",
					tc.input, tc.input, got)
			}
		} else {
			if got != tc.expected {
				t.Errorf("urlPathOnly(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		}
	}
}

// TestURLPathOnly_MalformedURL verifies that a malformed URL returns the
// raw input rather than panicking or returning empty string.
func TestURLPathOnly_MalformedURL(t *testing.T) {
	input := "://not valid"
	got := urlPathOnly(input)
	if got != input {
		t.Errorf("malformed URL should return raw input, got %q", got)
	}
}

// ── /metrics endpoint content ─────────────────────────────────────────────────

// TestObserver_Metrics_Scrapeable verifies that the /metrics endpoint
// returns content in a format that Prometheus can parse — specifically
// that it contains the text exposition format header and metric lines.
func TestObserver_Metrics_Scrapeable(t *testing.T) {
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewEventBus()
	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	defer o.Detach()

	url := fmt.Sprintf("http://localhost:%d/metrics", cfg.MetricsPort)
	waitForServer(t, url, 20, 50*time.Millisecond)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /metrics error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error: %v", err)
	}

	content := string(body)

	// Prometheus text format uses # HELP and # TYPE comment lines
	if !strings.Contains(content, "# HELP") {
		t.Error("metrics output should contain # HELP lines")
	}
	if !strings.Contains(content, "# TYPE") {
		t.Error("metrics output should contain # TYPE lines")
	}
}

// TestObserver_Metrics_ContainsExpectedNames verifies that all eleven
// lemmings metric names appear in the /metrics scrape output.
func TestObserver_Metrics_ContainsExpectedNames(t *testing.T) {
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewEventBus()
	if err := o.Attach(ctx, bus); err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	defer o.Detach()

	url := fmt.Sprintf("http://localhost:%d/metrics", cfg.MetricsPort)
	waitForServer(t, url, 20, 50*time.Millisecond)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /metrics error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error: %v", err)
	}
	content := string(body)

	expectedMetrics := []string{
		"lemmings_alive",
		"lemmings_completed_total",
		"lemmings_failed_total",
		"lemmings_visits_total",
		"lemmings_visit_duration_seconds",
		"lemmings_bytes_total",
		"lemmings_waiting_room_total",
		"lemmings_waiting_room_duration_seconds",
		"lemmings_terrains_online",
		"lemmings_dropped_logs_total",
		"lemmings_overflow_logs_total",
	}

	for _, name := range expectedMetrics {
		if !strings.Contains(content, name) {
			t.Errorf("metrics output should contain %q", name)
		}
	}
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

// TestPrometheusObserver_HandleEvent_Concurrent verifies that concurrent
// event delivery from multiple goroutines does not cause data races.
// Run with -race.
func TestPrometheusObserver_HandleEvent_Concurrent(t *testing.T) {
	o, bus, cleanup := attachedObserver(t)
	defer cleanup()
	_ = o

	var wg sync.WaitGroup
	const goroutines = 100

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			bus.Emit(Event{
				Kind:       EventVisitComplete,
				StatusCode: 200,
				URL:        fmt.Sprintf("http://example.com/page-%d", n%10),
				Duration:   time.Duration(n) * time.Millisecond,
			})
		}(i)
	}

	wg.Wait()
	time.Sleep(20 * time.Millisecond)

	// All 100 visits must be counted
	total := counterVecValue(t, o.visits, "status_class", "2xx")
	if total != 100 {
		t.Errorf("expected 100 2xx visits after concurrent emit, got %v", total)
	}
}

// TestURLLabel_Concurrent verifies that concurrent urlLabel calls from
// multiple goroutines do not cause data races on the seenURLs map.
// Run with -race.
func TestURLLabel_Concurrent(t *testing.T) {
	cfg := testObserverConfig()
	cfg.MetricsURLLabel = "full"
	o := NewPrometheusObserver(cfg)

	var wg sync.WaitGroup
	const goroutines = 200

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			o.urlLabel(fmt.Sprintf("https://example.com/page-%d", i))
		}()
	}

	wg.Wait() // must complete without race detector firing
}

// ── Benchmark ─────────────────────────────────────────────────────────────────

// BenchmarkPrometheusObserver_HandleEvent measures the cost of translating
// one Event into a Prometheus metric update. This runs on the EventBus
// subscriber path — any regression here adds latency to every lemming's
// visit cycle.
func BenchmarkPrometheusObserver_HandleEvent(b *testing.B) {
	o, bus, cleanup := attachedObserver(b)
	defer cleanup()
	_ = bus

	evt := Event{
		Kind:       EventVisitComplete,
		StatusCode: 200,
		URL:        "http://example.com/",
		Duration:   25 * time.Millisecond,
		BytesIn:    1024,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o.handleEvent(evt)
	}
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// testObserverConfig returns a SwarmConfig with Prometheus observer settings
// suitable for unit tests. Uses a free port to avoid conflicts between
// parallel test runs.
func testObserverConfig() SwarmConfig {
	cfg := testConfig()
	cfg.Observe = true
	cfg.MetricsPort = freeMetricsPort()
	cfg.MetricsURLLabel = "full"
	return cfg
}

// attachedObserver constructs a PrometheusObserver, attaches it to a fresh
// EventBus, and returns the observer, the bus, and a cleanup function.
// The cleanup function calls Detach and cancels the context.
//
// Usage:
//
//	o, bus, cleanup := attachedObserver(t)
//	defer cleanup()
func attachedObserver(tb testing.TB) (*PrometheusObserver, *EventBus, func()) {
	tb.Helper()
	cfg := testObserverConfig()
	o := NewPrometheusObserver(cfg)
	bus := NewEventBus()

	ctx, cancel := context.WithCancel(context.Background())
	if err := o.Attach(ctx, bus); err != nil {
		cancel()
		tb.Fatalf("attachedObserver: Attach error: %v", err)
	}

	cleanup := func() {
		o.Detach()
		cancel()
		bus.Close()
	}

	return o, bus, cleanup
}

// freeMetricsPort finds an available port for the Prometheus metrics server.
// Uses a different range from freePort to avoid collisions with the
// dashboard test helper.
//
// Warning: there is a small race window between finding the port and binding
// it. Tests that call freeMetricsPort should start their server promptly.
var freeMetricsPortMu sync.Mutex
var freeMetricsPortBase = 19000

func freeMetricsPort() int {
	freeMetricsPortMu.Lock()
	defer freeMetricsPortMu.Unlock()
	p := freeMetricsPortBase
	freeMetricsPortBase++
	return p
}

// ── Prometheus metric reading helpers ─────────────────────────────────────────
// These functions extract numeric values from Prometheus collectors for
// use in test assertions. They use the prometheus/client_model DTOs rather
// than scraping the HTTP endpoint, which is faster and more reliable in tests.

// gaugeValue reads the current float64 value from a prometheus.Gauge
// obtained via a GaugeVec.With() call.
func gaugeValue(tb testing.TB, g prometheus.Gauge) float64 {
	tb.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		tb.Fatalf("gaugeValue: Write error: %v", err)
	}
	return m.GetGauge().GetValue()
}

// simpleGaugeValue reads the current float64 value from a plain prometheus.Gauge.
func simpleGaugeValue(tb testing.TB, g prometheus.Gauge) float64 {
	tb.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		tb.Fatalf("simpleGaugeValue: Write error: %v", err)
	}
	return m.GetGauge().GetValue()
}

// counterValue reads the current float64 value from a prometheus.Counter.
func counterValue(tb testing.TB, c prometheus.Counter) float64 {
	tb.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		tb.Fatalf("counterValue: Write error: %v", err)
	}
	return m.GetCounter().GetValue()
}

// counterVecValue reads the current float64 value from a CounterVec for
// a single label key=value pair.
func counterVecValue(tb testing.TB, cv *prometheus.CounterVec, labelKey, labelValue string) float64 {
	tb.Helper()
	c, err := cv.GetMetricWith(prometheus.Labels{labelKey: labelValue})
	if err != nil {
		tb.Fatalf("counterVecValue: GetMetricWith error: %v", err)
	}
	return counterValue(tb, c)
}

// histogramSampleCount reads the sample count from a HistogramVec for a
// given label set. Returns 0 if no observations have been recorded yet.
func histogramSampleCount(tb testing.TB, hv *prometheus.HistogramVec, labels prometheus.Labels) uint64 {
	tb.Helper()
	h, err := hv.GetMetricWith(labels)
	if err != nil {
		tb.Fatalf("histogramSampleCount: GetMetricWith error: %v", err)
	}
	return simpleHistogramSampleCount(tb, h)
}

// simpleHistogramSampleCount reads the sample count from a plain
// prometheus.Histogram. Returns 0 if no observations have been recorded.
func simpleHistogramSampleCount(tb testing.TB, h prometheus.Histogram) uint64 {
	tb.Helper()
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		tb.Fatalf("simpleHistogramSampleCount: Write error: %v", err)
	}
	if m.GetHistogram() == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}
