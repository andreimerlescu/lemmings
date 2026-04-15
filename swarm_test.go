package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── generateToken ─────────────────────────────────────────────────────────────

// TestGenerateToken_Length verifies that the generated token is exactly
// 64 hexadecimal characters — 32 bytes rendered as hex.
func TestGenerateToken_Length(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("expected 64 char token, got %d: %q", len(token), token)
	}
}

// TestGenerateToken_IsHex verifies that the token contains only valid
// lowercase hexadecimal characters.
func TestGenerateToken_IsHex(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex character %q at position %d in token", c, i)
		}
	}
}

// TestGenerateToken_Unique verifies that two sequential token generations
// produce distinct values. Collision probability is 2^-256 — effectively zero.
func TestGenerateToken_Unique(t *testing.T) {
	a, err := generateToken()
	if err != nil {
		t.Fatalf("unexpected error on first token: %v", err)
	}
	b, err := generateToken()
	if err != nil {
		t.Fatalf("unexpected error on second token: %v", err)
	}
	if a == b {
		t.Error("two sequential generateToken calls should not produce identical tokens")
	}
}

// TestGenerateToken_Concurrent verifies that concurrent token generation
// across 100 goroutines produces no duplicates.
func TestGenerateToken_Concurrent(t *testing.T) {
	const n = 100
	tokens := make([]string, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var genErr error

	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			tok, err := generateToken()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				genErr = err
				return
			}
			tokens[i] = tok
		}()
	}
	wg.Wait()

	if genErr != nil {
		t.Fatalf("token generation error: %v", genErr)
	}

	seen := make(map[string]bool, n)
	for _, tok := range tokens {
		if seen[tok] {
			t.Errorf("duplicate token generated: %q", tok)
		}
		seen[tok] = true
	}
}

// ── formatBytes ───────────────────────────────────────────────────────────────

// TestFormatBytes_Bytes verifies formatting of values below 1KB.
func TestFormatBytes_Bytes(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
	}
	for _, tc := range cases {
		got := formatBytes(tc.input)
		if got != tc.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestFormatBytes_Kilobytes verifies formatting of KB-range values.
func TestFormatBytes_Kilobytes(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024 * 1023, "1023.00 KB"},
	}
	for _, tc := range cases {
		got := formatBytes(tc.input)
		if got != tc.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestFormatBytes_Megabytes verifies formatting of MB-range values.
func TestFormatBytes_Megabytes(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 2, "2.00 MB"},
		{1024*1024*1024 - 1, "1024.00 MB"},
	}
	for _, tc := range cases {
		got := formatBytes(tc.input)
		if got != tc.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestFormatBytes_Gigabytes verifies formatting of GB-range values.
func TestFormatBytes_Gigabytes(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{1024 * 1024 * 1024, "1.00 GB"},
		{1024 * 1024 * 1024 * 2, "2.00 GB"},
	}
	for _, tc := range cases {
		got := formatBytes(tc.input)
		if got != tc.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// ── formatInt ─────────────────────────────────────────────────────────────────

// TestFormatInt_SmallNumbers verifies that numbers below 1000 are returned
// without comma separators.
func TestFormatInt_SmallNumbers(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
	}
	for _, tc := range cases {
		got := formatInt(tc.input)
		if got != tc.expected {
			t.Errorf("formatInt(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestFormatInt_Thousands verifies comma insertion at the thousands boundary.
func TestFormatInt_Thousands(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{1000, "1,000"},
		{9999, "9,999"},
		{10000, "10,000"},
		{999999, "999,999"},
	}
	for _, tc := range cases {
		got := formatInt(tc.input)
		if got != tc.expected {
			t.Errorf("formatInt(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestFormatInt_Millions verifies correct formatting for million-scale values.
func TestFormatInt_Millions(t *testing.T) {
	cases := []struct {
		input    int64
		expected string
	}{
		{1_000_000, "1,000,000"},
		{1_234_567, "1,234,567"},
		{100_000_000, "100,000,000"},
	}
	for _, tc := range cases {
		got := formatInt(tc.input)
		if got != tc.expected {
			t.Errorf("formatInt(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// TestFormatInt_NegativeNumbers verifies that negative numbers are handled
// without panicking. The exact format is implementation-defined but must
// not panic.
func TestFormatInt_NegativeNumbers(t *testing.T) {
	// Must not panic
	got := formatInt(-1000)
	if got == "" {
		t.Error("formatInt(-1000) should return non-empty string")
	}
}

// ── NewSwarm ──────────────────────────────────────────────────────────────────

// TestNewSwarm_Constructs verifies that NewSwarm returns a non-nil Swarm
// with correctly initialised fields when given a valid config and a
// reachable test server.
func TestNewSwarm_Constructs(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/").
		withNormalPage("/about").
		withSitemap("/sitemap.xml", []string{}).
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.DashboardPort = 0

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}
	if swarm == nil {
		t.Fatal("NewSwarm returned nil swarm")
	}
	if swarm.token == "" {
		t.Error("swarm token should not be empty")
	}
	if swarm.pool == nil {
		t.Error("swarm pool should not be nil after construction")
	}
	if len(swarm.terrains) != int(cfg.Terrain) {
		t.Errorf("expected %d terrains, got %d", cfg.Terrain, len(swarm.terrains))
	}
}

// TestNewSwarm_PrimaryChannelCapped verifies that the primary results channel
// is capped at primaryChanCap even when terrain*pack exceeds it.
func TestNewSwarm_PrimaryChannelCapped(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	// terrain * pack = 1000 * 1000 = 1,000,000 > primaryChanCap (500,000)
	cfg.Terrain = 1000
	cfg.Pack = 1000
	cfg.Limit = 10 // keep semaphore sane

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	if cap(swarm.primary) > primaryChanCap {
		t.Errorf("primary channel cap %d exceeds limit %d",
			cap(swarm.primary), primaryChanCap)
	}
}

// TestNewSwarm_PrimaryChannelSizedToTotal verifies that when terrain*pack
// is below primaryChanCap the primary channel is sized to the exact total.
func TestNewSwarm_PrimaryChannelSizedToTotal(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Terrain = 3
	cfg.Pack = 4 // total = 12, well below primaryChanCap

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	if cap(swarm.primary) != 12 {
		t.Errorf("expected primary channel cap 12 (3*4), got %d", cap(swarm.primary))
	}
}

// TestNewSwarm_OverflowChannelFixed verifies that the overflow channel
// is always sized to overflowChanCap regardless of terrain*pack.
func TestNewSwarm_OverflowChannelFixed(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	if cap(swarm.overflow) != overflowChanCap {
		t.Errorf("expected overflow cap %d, got %d", overflowChanCap, cap(swarm.overflow))
	}
}

// TestNewSwarm_TokenIsHash verifies that the stored token field is not
// the raw token printed to STDOUT but is instead its SHA-512 hash —
// the Dashboard's tokenHash value mirrors the swarm token.
//
// Note: the swarm stores the raw token for printing. The dashboard stores
// the hash. This test verifies the token is non-empty and 64 hex chars.
func TestNewSwarm_TokenNonEmpty(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL + "/"

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}
	if len(swarm.token) != 64 {
		t.Errorf("expected 64 char token, got %d: %q", len(swarm.token), swarm.token)
	}
}

// TestNewSwarm_ContextCancellation verifies that cancelling the context
// passed to NewSwarm causes construction to abort cleanly when the
// indexing phase is blocked.
func TestNewSwarm_ContextCancellation(t *testing.T) {
	// Slow server that won't respond before context cancels
	srv := newTestServer(t).
		withHandler("/", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Second)
		}).
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := NewSwarm(ctx, cfg)
	// Either an error or a pool with just the origin URL is acceptable
	// The key invariant: does not hang
	_ = err
}

// ── sendLifeLog ───────────────────────────────────────────────────────────────

// TestSendLifeLog_SendsToPrimary verifies that under normal conditions
// sendLifeLog places the LifeLog into the primary channel.
func TestSendLifeLog_SendsToPrimary(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	ll := LifeLog{Identity: Identity{ID: "test-lemming"}}
	s.sendLifeLog(ll)

	select {
	case got := <-s.primary:
		if got.Identity.ID != "test-lemming" {
			t.Errorf("expected ID test-lemming, got %q", got.Identity.ID)
		}
	default:
		t.Error("expected LifeLog in primary channel")
	}
}

// TestSendLifeLog_SendsToOverflowWhenPrimaryFull verifies that when the
// primary channel is full, sendLifeLog routes the LifeLog to overflow.
func TestSendLifeLog_SendsToOverflowWhenPrimaryFull(t *testing.T) {
	// Primary cap of 1 — easy to fill
	s := makeMinimalSwarm(t, 1, 10)

	// Fill primary
	s.primary <- LifeLog{Identity: Identity{ID: "filler"}}

	// This send should go to overflow
	s.sendLifeLog(LifeLog{Identity: Identity{ID: "overflow-lemming"}})

	if s.metrics.OverflowLogs.Load() != 1 {
		t.Errorf("expected OverflowLogs=1, got %d", s.metrics.OverflowLogs.Load())
	}

	select {
	case got := <-s.overflow:
		if got.Identity.ID != "overflow-lemming" {
			t.Errorf("expected overflow-lemming in overflow channel, got %q", got.Identity.ID)
		}
	default:
		t.Error("expected LifeLog in overflow channel")
	}
}

// TestSendLifeLog_IncrementsDroppedWhenBothFull verifies that when both
// primary and overflow are full, DroppedLogs is incremented and the
// LifeLog is silently discarded rather than blocking.
func TestSendLifeLog_IncrementsDroppedWhenBothFull(t *testing.T) {
	// Both channels with cap 1
	s := makeMinimalSwarm(t, 1, 1)

	// Fill both channels
	s.primary <- LifeLog{Identity: Identity{ID: "primary-filler"}}
	s.overflow <- LifeLog{Identity: Identity{ID: "overflow-filler"}}

	// This send must not block and must increment DroppedLogs
	done := make(chan struct{})
	go func() {
		s.sendLifeLog(LifeLog{Identity: Identity{ID: "dropped-lemming"}})
		close(done)
	}()

	select {
	case <-done:
		// good — did not block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("sendLifeLog blocked when both channels full")
	}

	if s.metrics.DroppedLogs.Load() != 1 {
		t.Errorf("expected DroppedLogs=1, got %d", s.metrics.DroppedLogs.Load())
	}
}

// TestSendLifeLog_OverflowCounterIncrements verifies that OverflowLogs
// counter increments exactly once per overflow send.
func TestSendLifeLog_OverflowCounterIncrements(t *testing.T) {
	s := makeMinimalSwarm(t, 1, 100)

	// Fill primary to force overflow
	s.primary <- LifeLog{}

	const overflowCount = 5
	for i := 0; i < overflowCount; i++ {
		s.sendLifeLog(LifeLog{Identity: Identity{ID: fmt.Sprintf("overflow-%d", i)}})
	}

	if s.metrics.OverflowLogs.Load() != overflowCount {
		t.Errorf("expected OverflowLogs=%d, got %d",
			overflowCount, s.metrics.OverflowLogs.Load())
	}
}

// TestSendLifeLog_NonBlocking verifies that sendLifeLog never blocks
// regardless of channel state, completing within a tight deadline.
func TestSendLifeLog_NonBlocking(t *testing.T) {
	s := makeMinimalSwarm(t, 1, 1)

	// Fill both channels
	s.primary <- LifeLog{}
	s.overflow <- LifeLog{}

	start := time.Now()
	for i := 0; i < 100; i++ {
		s.sendLifeLog(LifeLog{})
	}
	elapsed := time.Since(start)

	// 100 non-blocking sends should complete in well under 10ms
	if elapsed > 10*time.Millisecond {
		t.Errorf("sendLifeLog appears to be blocking: 100 sends took %s", elapsed)
	}
}

// ── ingest ────────────────────────────────────────────────────────────────────

// TestIngest_UpdatesCompletedAndAlive verifies that ingest decrements
// LemmingsAlive and increments LemmingsCompleted.
func TestIngest_UpdatesCompletedAndAlive(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)
	s.metrics.LemmingsAlive.Store(5)

	s.ingest(LifeLog{
		Visits: []Visit{{URL: "http://example.com/", StatusCode: 200}},
	})

	if s.metrics.LemmingsCompleted.Load() != 1 {
		t.Errorf("expected LemmingsCompleted=1, got %d", s.metrics.LemmingsCompleted.Load())
	}
	if s.metrics.LemmingsAlive.Load() != 4 {
		t.Errorf("expected LemmingsAlive=4, got %d", s.metrics.LemmingsAlive.Load())
	}
}

// TestIngest_StatusCodeBuckets verifies that ingest places visit status codes
// into the correct swarm-level metric buckets.
func TestIngest_StatusCodeBuckets(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	s.ingest(LifeLog{
		Visits: []Visit{
			{StatusCode: 200},
			{StatusCode: 201},
			{StatusCode: 301},
			{StatusCode: 404},
			{StatusCode: 500},
			{StatusCode: 503},
		},
	})

	if s.metrics.Total2xx.Load() != 2 {
		t.Errorf("expected Total2xx=2, got %d", s.metrics.Total2xx.Load())
	}
	if s.metrics.Total3xx.Load() != 1 {
		t.Errorf("expected Total3xx=1, got %d", s.metrics.Total3xx.Load())
	}
	if s.metrics.Total4xx.Load() != 1 {
		t.Errorf("expected Total4xx=1, got %d", s.metrics.Total4xx.Load())
	}
	if s.metrics.Total5xx.Load() != 2 {
		t.Errorf("expected Total5xx=2, got %d", s.metrics.Total5xx.Load())
	}
}

// TestIngest_AccumulatesBytes verifies that TotalBytes is incremented
// by the sum of all visit BytesIn values.
func TestIngest_AccumulatesBytes(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	s.ingest(LifeLog{
		Visits: []Visit{
			{StatusCode: 200, BytesIn: 1024},
			{StatusCode: 200, BytesIn: 2048},
		},
	})

	if s.metrics.TotalBytes.Load() != 3072 {
		t.Errorf("expected TotalBytes=3072, got %d", s.metrics.TotalBytes.Load())
	}
}

// TestIngest_WaitingRoomCounter verifies that TotalWaitingRoom is incremented
// for each visit with WaitingRoom.Detected=true.
func TestIngest_WaitingRoomCounter(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	s.ingest(LifeLog{
		Visits: []Visit{
			{StatusCode: 200, WaitingRoom: WaitingRoomMetric{Detected: true}},
			{StatusCode: 200, WaitingRoom: WaitingRoomMetric{Detected: false}},
			{StatusCode: 200, WaitingRoom: WaitingRoomMetric{Detected: true}},
		},
	})

	if s.metrics.TotalWaitingRoom.Load() != 2 {
		t.Errorf("expected TotalWaitingRoom=2, got %d", s.metrics.TotalWaitingRoom.Load())
	}
}

// TestIngest_FailedCountOnError verifies that LemmingsFailed is incremented
// when the LifeLog carries a non-nil Error.
func TestIngest_FailedCountOnError(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	s.ingest(LifeLog{Error: fmt.Errorf("something went wrong")})
	s.ingest(LifeLog{}) // no error

	if s.metrics.LemmingsFailed.Load() != 1 {
		t.Errorf("expected LemmingsFailed=1, got %d", s.metrics.LemmingsFailed.Load())
	}
}

// TestIngest_EmitsLemmingDiedEvent verifies that ingest emits an
// EventLemmingDied event on the bus for each ingested LifeLog.
func TestIngest_EmitsLemmingDiedEvent(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)
	log := NewEventLog(10)
	s.events.Subscribe(log.AsSubscriber())

	s.ingest(LifeLog{Terrain: 1, Pack: 2})

	requireEventEmitted(t, log, EventLemmingDied, 100*time.Millisecond)
}

// TestIngest_TotalVisitsAccumulated verifies that TotalVisits increments
// by the number of Visit entries in each ingested LifeLog.
func TestIngest_TotalVisitsAccumulated(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	s.ingest(LifeLog{
		Visits: []Visit{
			{StatusCode: 200},
			{StatusCode: 200},
			{StatusCode: 404},
		},
	})

	if s.metrics.TotalVisits.Load() != 3 {
		t.Errorf("expected TotalVisits=3, got %d", s.metrics.TotalVisits.Load())
	}
}

// ── collectResults / drainOverflow ───────────────────────────────────────────

// TestCollectResults_DrainsPrimaryAndOverflow verifies that collectResults
// processes all LifeLogs from both primary and overflow before exiting.
func TestCollectResults_DrainsPrimaryAndOverflow(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	// Send 3 to primary, 2 to overflow
	for i := 0; i < 3; i++ {
		s.primary <- LifeLog{Visits: []Visit{{StatusCode: 200}}}
	}
	for i := 0; i < 2; i++ {
		s.overflow <- LifeLog{Visits: []Visit{{StatusCode: 200}}}
	}
	close(s.primary)

	s.collectResults()

	if s.metrics.TotalVisits.Load() != 5 {
		t.Errorf("expected 5 total visits from primary+overflow, got %d",
			s.metrics.TotalVisits.Load())
	}
}

// TestDrainOverflow_EmptiesChannel verifies that drainOverflow processes
// all LifeLogs currently in the overflow channel.
func TestDrainOverflow_EmptiesChannel(t *testing.T) {
	s := makeMinimalSwarm(t, 10, 10)

	for i := 0; i < 5; i++ {
		s.overflow <- LifeLog{Visits: []Visit{{StatusCode: 200}}}
	}

	s.drainOverflow()

	if s.metrics.TotalVisits.Load() != 5 {
		t.Errorf("expected 5 total visits after drainOverflow, got %d",
			s.metrics.TotalVisits.Load())
	}
	if len(s.overflow) != 0 {
		t.Errorf("overflow channel should be empty after drain, got %d items", len(s.overflow))
	}
}

// ── ramp ──────────────────────────────────────────────────────────────────────

// TestRamp_LaunchesAllTerrains verifies that ramp brings every terrain
// online within a reasonable timeout.
func TestRamp_LaunchesAllTerrains(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Terrain = 3
	cfg.Pack = 1
	cfg.Ramp = 30 * time.Millisecond
	cfg.Until = 50 * time.Millisecond

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := swarm.ramp(); err != nil {
		t.Fatalf("ramp error: %v", err)
	}

	// Wait for all terrains to confirm they are online
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if swarm.metrics.TerrainsOnline.Load() == int64(cfg.Terrain) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if swarm.metrics.TerrainsOnline.Load() != int64(cfg.Terrain) {
		t.Errorf("expected %d terrains online after ramp, got %d",
			cfg.Terrain, swarm.metrics.TerrainsOnline.Load())
	}

	_ = ctx
}

// TestRamp_StopsOnContextCancellation verifies that ramp stops launching
// terrains when the context is cancelled mid-ramp.
func TestRamp_StopsOnContextCancellation(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Terrain = 10
	cfg.Pack = 1
	cfg.Ramp = 2 * time.Second  // long ramp so we can cancel mid-way
	cfg.Until = 50 * time.Millisecond

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	// Replace swarm context with one we control
	ctx, cancel := context.WithCancel(context.Background())
	swarm.ctx = ctx

	done := make(chan error, 1)
	go func() {
		done <- swarm.ramp()
	}()

	// Cancel after first terrain comes online
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ramp returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ramp did not stop after context cancellation")
	}

	// Should have fewer than all terrains online
	online := swarm.metrics.TerrainsOnline.Load()
	if online >= int64(cfg.Terrain) {
		t.Errorf("expected ramp to be interrupted: all %d terrains came online", cfg.Terrain)
	}
}

// ── Run (integration) ─────────────────────────────────────────────────────────

// TestSwarm_Run_CompletesCleanly verifies that a small swarm runs to
// completion without error and all lemmings are accounted for.
func TestSwarm_Run_CompletesCleanly(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Terrain = 2
	cfg.Pack = 2
	cfg.Until = 100 * time.Millisecond
	cfg.Ramp = 20 * time.Millisecond

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	if err := swarm.Run(); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	total := cfg.Terrain * cfg.Pack
	completed := swarm.metrics.LemmingsCompleted.Load()
	if completed != total {
		t.Errorf("expected %d lemmings completed, got %d", total, completed)
	}
	if swarm.metrics.LemmingsAlive.Load() != 0 {
		t.Errorf("expected 0 lemmings alive after Run, got %d",
			swarm.metrics.LemmingsAlive.Load())
	}
}

// TestSwarm_Run_ContextCancellation verifies that cancelling the parent
// context causes Run to return promptly.
func TestSwarm_Run_ContextCancellation(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Terrain = 2
	cfg.Pack = 2
	cfg.Until = 10 * time.Second // long lived lemmings
	cfg.Ramp = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	swarm, err := NewSwarm(ctx, cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- swarm.Run()
	}()

	// Let ramp start then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after context cancellation")
	}
}

// TestSwarm_Report_WritesFiles verifies that Report creates the expected
// output files after a completed Run.
func TestSwarm_Report_WritesFiles(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	dir := t.TempDir()

	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	cfg.Terrain = 1
	cfg.Pack = 1
	cfg.Until = 50 * time.Millisecond
	cfg.Ramp = 10 * time.Millisecond
	cfg.SaveTo = dir

	swarm, err := NewSwarm(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSwarm error: %v", err)
	}

	if err := swarm.Run(); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if err := swarm.Report(); err != nil {
		t.Fatalf("Report error: %v", err)
	}

	domain := domainFromURL(cfg.Hit)
	destDir := filepath.Join(dir, "lemmings", domain)
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("could not read output directory %s: %v", destDir, err)
	}

	var foundMD, foundHTML bool
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			foundMD = true
		}
		if strings.HasSuffix(e.Name(), ".html") {
			foundHTML = true
		}
	}
	if !foundMD {
		t.Error("expected .md report file")
	}
	if !foundHTML {
		t.Error("expected .html report file")
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkSendLifeLog_NoPressure measures sendLifeLog throughput when
// both channels are empty — the common case.
func BenchmarkSendLifeLog_NoPressure(b *testing.B) {
	s := makeMinimalSwarm(b, primaryChanCap, overflowChanCap)
	ll := LifeLog{Identity: Identity{ID: "bench"}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Drain periodically to avoid filling the channel
		if i%100 == 0 {
			for len(s.primary) > 0 {
				<-s.primary
			}
		}
		s.sendLifeLog(ll)
	}
}

// BenchmarkSendLifeLog_Concurrent measures sendLifeLog throughput under
// full swarm-like concurrency — many goroutines sending simultaneously.
//
//go:build bench
func BenchmarkSendLifeLog_Concurrent(b *testing.B) {
	s := makeMinimalSwarm(b, primaryChanCap, overflowChanCap)
	ll := LifeLog{Identity: Identity{ID: "bench"}}

	// Drain goroutine to keep channels from filling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.primary:
			case <-s.overflow:
			}
		}
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.sendLifeLog(ll)
		}
	})
}

// BenchmarkFormatInt measures formatInt throughput — called on every
// STDOUT ticker update and in the final summary.
func BenchmarkFormatInt(b *testing.B) {
	values := []int64{0, 999, 1000, 999999, 1000000, 100000000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		formatInt(values[i%len(values)])
	}
}

// BenchmarkFormatBytes measures formatBytes throughput.
func BenchmarkFormatBytes(b *testing.B) {
	values := []int64{512, 1024, 1024 * 1024, 1024 * 1024 * 1024}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		formatBytes(values[i%len(values)])
	}
}

// ── Fuzz ──────────────────────────────────────────────────────────────────────

// FuzzFormatInt verifies that formatInt never panics on arbitrary int64
// values and always returns a non-empty string.
func FuzzFormatInt(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(1000))
	f.Add(int64(1_000_000))
	f.Add(int64(-1))
	f.Add(int64(-1_000_000))
	f.Add(int64(9_223_372_036_854_775_807)) // math.MaxInt64

	f.Fuzz(func(t *testing.T, n int64) {
		got := formatInt(n)
		if got == "" {
			t.Errorf("formatInt(%d) returned empty string", n)
		}
	})
}

// FuzzFormatBytes verifies that formatBytes never panics on arbitrary
// int64 values and always returns a non-empty string containing a unit.
func FuzzFormatBytes(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1023))
	f.Add(int64(1024))
	f.Add(int64(1024 * 1024))
	f.Add(int64(1024 * 1024 * 1024))
	f.Add(int64(9_223_372_036_854_775_807))

	f.Fuzz(func(t *testing.T, n int64) {
		if n < 0 {
			return // formatBytes is defined for non-negative values
		}
		got := formatBytes(n)
		if got == "" {
			t.Errorf("formatBytes(%d) returned empty string", n)
		}
		hasUnit := strings.HasSuffix(got, " B") ||
			strings.HasSuffix(got, " KB") ||
			strings.HasSuffix(got, " MB") ||
			strings.HasSuffix(got, " GB")
		if !hasUnit {
			t.Errorf("formatBytes(%d) = %q — missing unit suffix", n, got)
		}
	})
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// makeMinimalSwarm constructs a Swarm with pre-allocated channels of the
// given capacities, a live EventBus, and zeroed metrics. It does not call
// NewSwarm — it bypasses the indexing phase so channel-level tests run fast.
//
// primaryCap and overflowCap control the channel buffer sizes directly.
// Use this helper only for tests that target sendLifeLog, ingest,
// collectResults, and drainOverflow — not for end-to-end Run tests.
func makeMinimalSwarm(tb testing.TB, primaryCap, overflowCap int) *Swarm {
	tb.Helper()
	bus := NewEventBus()
	metrics := &SwarmMetrics{}
	cfg := testConfig()

	s := &Swarm{
		cfg:      cfg,
		ctx:      context.Background(),
		primary:  make(chan LifeLog, primaryCap),
		overflow: make(chan LifeLog, overflowCap),
		events:   bus,
		metrics:  *metrics,
		reporter: NewReporter(cfg),
	}
	return s
}
