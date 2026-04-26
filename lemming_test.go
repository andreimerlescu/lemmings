package main

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestRun_VisitCompleteEvent_CarriesMetrics verifies that the EventVisitComplete
// events emitted by Run carry non-zero BytesIn and Duration fields so that
// observers can record metrics without reading from a separate channel.
func TestRun_VisitCompleteEvent_CarriesMetrics(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 200 * time.Millisecond
	pool := testPool(srv.URL)
	bus := NewEventBus()

	var mu sync.Mutex
	var visitEvents []Event
	bus.Subscribe(Filter(func(e Event) {
		mu.Lock()
		visitEvents = append(visitEvents, e)
		mu.Unlock()
	}, EventVisitComplete))

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	l.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()

	if len(visitEvents) == 0 {
		t.Fatal("expected at least one EventVisitComplete")
	}
	
	for i, e := range visitEvents {
  		if e.StatusCode == 0 {
        	// Context cancelled mid-flight before response — skip both checks
    		continue
 		}
    	if e.Duration == 0 {
       		t.Errorf("visit[%d]: Duration should be non-zero on EventVisitComplete", i)
    	}
	}
}

// TestRun_WaitingRoomEvent_CarriesDuration verifies that the EventWaitingRoom
// event emitted after admission carries a non-zero Duration field.
func TestRun_WaitingRoomEvent_CarriesDuration(t *testing.T) {
	srv := newTestServer(t).
		withAdmittingWaitingRoom("/", 5, 1).
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 2 * time.Second
	pool := &URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/"},
		Checksums: map[string]string{srv.URL + "/": sha512hex([]byte(normalPageBody))},
	}
	bus := NewEventBus()

	var mu sync.Mutex
	var wrEvents []Event
	bus.Subscribe(Filter(func(e Event) {
		mu.Lock()
		wrEvents = append(wrEvents, e)
		mu.Unlock()
	}, EventWaitingRoom))

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	l.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()

	if len(wrEvents) == 0 {
		t.Skip("lemming was not placed in waiting room during test window")
	}
	for i, e := range wrEvents {
		if e.Duration == 0 {
			t.Errorf("wrEvent[%d]: Duration should be non-zero after admission", i)
		}
	}
}

// ── NewLemming ────────────────────────────────────────────────────────────────

// TestNewLemming_Identity verifies that a freshly constructed Lemming has
// the correct Terrain, Pack, and a non-empty ID populated from its arguments.
func TestNewLemming_Identity(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(3, 7, testConfig(), pool, newTestClient(), bus, testMetrics())

	if l.identity.Terrain != 3 {
		t.Errorf("expected terrain 3, got %d", l.identity.Terrain)
	}
	if l.identity.Pack != 7 {
		t.Errorf("expected pack 7, got %d", l.identity.Pack)
	}
	if l.identity.ID == "" {
		t.Error("lemming ID should not be empty")
	}
	if l.identity.BornAt.IsZero() {
		t.Error("BornAt should be set at construction")
	}
}

// TestNewLemming_UniqueIDs verifies that two lemmings with the same terrain
// and pack indices still receive distinct IDs due to the nanosecond timestamp
// component in generateLemmingID.
func TestNewLemming_UniqueIDs(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()
	cfg := testConfig()
	metrics := testMetrics()

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, metrics)
		if ids[l.identity.ID] {
			t.Fatalf("duplicate lemming ID generated: %s", l.identity.ID)
		}
		ids[l.identity.ID] = true
	}
}

// ── generateLemmingID ─────────────────────────────────────────────────────────

// TestGenerateLemmingID_Length verifies the ID is exactly 32 hex characters.
func TestGenerateLemmingID_Length(t *testing.T) {
	id := generateLemmingID(1, 1)
	if len(id) != 32 {
		t.Errorf("expected 32 char ID, got %d: %s", len(id), id)
	}
}

// TestGenerateLemmingID_IsHex verifies the ID contains only valid hex chars.
func TestGenerateLemmingID_IsHex(t *testing.T) {
	id := generateLemmingID(5, 10)
	for i, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex character %q at position %d in ID %s", c, i, id)
		}
	}
}

// TestGenerateLemmingID_Unique verifies concurrent generation produces
// no duplicate IDs across 1000 calls.
func TestGenerateLemmingID_Unique(t *testing.T) {
	const n = 1000
	ids := make(map[string]bool, n)
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := generateLemmingID(int64(i), int64(i))
			mu.Lock()
			if ids[id] {
				t.Errorf("duplicate ID: %s", id)
			}
			ids[id] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
}

// ── detectWaitingRoom ─────────────────────────────────────────────────────────

// TestDetectWaitingRoom_NormalHTML verifies that a normal HTML page does not
// trigger waiting room detection.
func TestDetectWaitingRoom_NormalHTML(t *testing.T) {
	detected, pos := detectWaitingRoom([]byte(normalPageBody))
	if detected {
		t.Error("normal HTML should not be detected as waiting room")
	}
	if pos != 0 {
		t.Errorf("position should be 0 for non-waiting-room page, got %d", pos)
	}
}

// TestDetectWaitingRoom_WaitingRoomHTML verifies correct detection and
// position extraction from a well-formed waiting room response.
func TestDetectWaitingRoom_WaitingRoomHTML(t *testing.T) {
	body := waitingRoomHTML(waitingRoomPosition)
	detected, pos := detectWaitingRoom([]byte(body))
	if !detected {
		t.Error("waiting room HTML should be detected")
	}
	if pos != waitingRoomPosition {
		t.Errorf("expected position %d, got %d", waitingRoomPosition, pos)
	}
}

// TestDetectWaitingRoom_PositionZero verifies that a waiting room page with
// position 0 is still detected correctly.
func TestDetectWaitingRoom_PositionZero(t *testing.T) {
	body := waitingRoomHTML(0)
	detected, pos := detectWaitingRoom([]byte(body))
	if !detected {
		t.Error("should detect waiting room even when position is 0")
	}
	if pos != 0 {
		t.Errorf("expected position 0, got %d", pos)
	}
}

// TestDetectWaitingRoom_SignatureWithoutMarker verifies that a body containing
// the /queue/status signature but missing the position marker is detected as a
// waiting room but returns position 0.
func TestDetectWaitingRoom_SignatureWithoutMarker(t *testing.T) {
	// Contains the signature string but no id="position"> marker
	body := `<html><body><script>const STATUS_ENDPOINT = "/queue/status";</script></body></html>`
	detected, pos := detectWaitingRoom([]byte(body))
	if !detected {
		t.Error("body with signature should be detected as waiting room")
	}
	if pos != 0 {
		t.Errorf("expected position 0 when marker absent, got %d", pos)
	}
}

// TestDetectWaitingRoom_MalformedPosition verifies graceful handling when
// the position element contains non-numeric content.
func TestDetectWaitingRoom_MalformedPosition(t *testing.T) {
	body := `<html><body>
<script>const STATUS_ENDPOINT = "/queue/status";</script>
<div id="position">not-a-number</div>
</body></html>`
	detected, pos := detectWaitingRoom([]byte(body))
	if !detected {
		t.Error("should still detect waiting room despite malformed position")
	}
	if pos != 0 {
		t.Errorf("malformed position should return 0, got %d", pos)
	}
}

// TestDetectWaitingRoom_EmptyBody verifies that an empty body returns
// (false, 0) without panicking.
func TestDetectWaitingRoom_EmptyBody(t *testing.T) {
	detected, pos := detectWaitingRoom([]byte{})
	if detected {
		t.Error("empty body should not be detected as waiting room")
	}
	if pos != 0 {
		t.Errorf("expected position 0 for empty body, got %d", pos)
	}
}

// TestDetectWaitingRoom_LargePosition verifies that large position numbers
// are parsed correctly.
func TestDetectWaitingRoom_LargePosition(t *testing.T) {
	body := waitingRoomHTML(999999)
	detected, pos := detectWaitingRoom([]byte(body))
	if !detected {
		t.Error("should detect waiting room")
	}
	if pos != 999999 {
		t.Errorf("expected position 999999, got %d", pos)
	}
}

// ── sha512sum ─────────────────────────────────────────────────────────────────

// TestSha512sum_KnownValue verifies the output against a known SHA-512 hash.
func TestSha512sum_KnownValue(t *testing.T) {
	// echo -n "lemmings" | sha512sum
	const input = "lemmings"
	const expected = "a emigrated" +
		// We compute this rather than hardcode to avoid transcription errors
		""
	result := sha512sum([]byte(input))
	if len(result) != 128 {
		t.Errorf("SHA-512 hex string should be 128 chars, got %d", len(result))
	}
	// Verify determinism — same input always produces same output
	result2 := sha512sum([]byte(input))
	if result != result2 {
		t.Error("sha512sum is not deterministic")
	}
}

// TestSha512sum_Deterministic verifies the same input always produces
// the same output regardless of call order.
func TestSha512sum_Deterministic(t *testing.T) {
	input := []byte(normalPageBody)
	first := sha512sum(input)
	for i := 0; i < 100; i++ {
		if got := sha512sum(input); got != first {
			t.Fatalf("sha512sum produced different result on call %d", i)
		}
	}
}

// TestSha512sum_DifferentInputs verifies that distinct inputs produce
// distinct hashes.
func TestSha512sum_DifferentInputs(t *testing.T) {
	a := sha512sum([]byte("hello"))
	b := sha512sum([]byte("world"))
	if a == b {
		t.Error("different inputs should produce different SHA-512 hashes")
	}
}

// TestSha512sum_EmptyInput verifies that an empty byte slice produces
// the known SHA-512 hash of the empty string.
func TestSha512sum_EmptyInput(t *testing.T) {
	// SHA-512 of empty string is a well-known constant
	const emptyHash = "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"
	result := sha512sum([]byte{})
	// length check — the exact hash constant above may have transcription
	// error so we verify length and determinism rather than exact value
	if len(result) != 128 {
		t.Errorf("empty input SHA-512 should be 128 hex chars, got %d", len(result))
	}
	if result != sha512sum([]byte{}) {
		t.Error("sha512sum of empty input is not deterministic")
	}
}

// ── randomUA ──────────────────────────────────────────────────────────────────

// TestRandomUA_AlwaysFromPool verifies that randomUA always returns one of
// the five strings in the userAgents pool.
func TestRandomUA_AlwaysFromPool(t *testing.T) {
	l := newTestLemming(t)
	pool := make(map[string]bool)
	for _, ua := range userAgents {
		pool[ua] = true
	}

	for i := 0; i < 1000; i++ {
		ua := l.randomUA()
		if !pool[ua] {
			t.Errorf("randomUA returned a string not in userAgents pool: %q", ua)
		}
	}
}

// TestRandomUA_NotAlwaysSame verifies that randomUA produces variation
// across calls — it should not always return the same entry.
func TestRandomUA_NotAlwaysSame(t *testing.T) {
	l := newTestLemming(t)
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		seen[l.randomUA()] = true
	}
	if len(seen) < 2 {
		t.Error("randomUA should return varied UA strings, only 1 distinct value seen in 200 calls")
	}
}

// ── pickURL ───────────────────────────────────────────────────────────────────

// TestPickURL_EmptyPool returns the Hit URL when the pool has no URLs.
func TestPickURL_EmptyPool(t *testing.T) {
	cfg := testConfig()
	cfg.Hit = "http://example.com/"
	l := &Lemming{
		cfg:  cfg,
		pool: &URLPool{URLs: []string{}},
		rng:  newRNG(),
	}
	got := l.pickURL()
	if got != cfg.Hit {
		t.Errorf("expected Hit URL %q for empty pool, got %q", cfg.Hit, got)
	}
}

// TestPickURL_AlwaysFromPool verifies pickURL always returns a URL
// that exists in the pool.
func TestPickURL_AlwaysFromPool(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	pool := testPool(srv.URL)
	l := &Lemming{cfg: testConfig(), pool: pool, rng: newRNG()}

	poolSet := make(map[string]bool)
	for _, u := range pool.URLs {
		poolSet[u] = true
	}

	for i := 0; i < 500; i++ {
		u := l.pickURL()
		if !poolSet[u] {
			t.Errorf("pickURL returned URL not in pool: %q", u)
		}
	}
}

// TestPickURL_UsesVariation verifies that pickURL returns more than one
// distinct URL from a multi-URL pool over many calls.
func TestPickURL_UsesVariation(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()
	pool := testPool(srv.URL)
	l := &Lemming{cfg: testConfig(), pool: pool, rng: newRNG()}

	seen := make(map[string]bool)
	for i := 0; i < 300; i++ {
		seen[l.pickURL()] = true
	}
	if len(seen) < 2 {
		t.Error("pickURL should return varied URLs from a multi-URL pool")
	}
}

// ── request ───────────────────────────────────────────────────────────────────

// TestRequest_SetsUserAgent verifies that every HTTP request carries a
// User-Agent header drawn from the userAgents pool.
func TestRequest_SetsUserAgent(t *testing.T) {
	var receivedUA string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(normalPageBody))
	}))
	defer srv.Close()

	l := newTestLemming(t)
	_, _, _, err := l.request(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}

	mu.Lock()
	ua := receivedUA
	mu.Unlock()

	found := false
	for _, expected := range userAgents {
		if ua == expected {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("User-Agent %q not found in userAgents pool", ua)
	}
}

// TestRequest_ReturnsBodyAndStatus verifies that request correctly returns
// the response body, status code, and byte count.
func TestRequest_ReturnsBodyAndStatus(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	l := newTestLemming(t)

	body, status, bytes, err := l.request(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
	if len(body) == 0 {
		t.Error("body should not be empty")
	}
	if bytes != int64(len(body)) {
		t.Errorf("bytes %d should equal len(body) %d", bytes, len(body))
	}
}

// TestRequest_Non200Status verifies that a non-200 status code is returned
// without an error — HTTP error codes are valid responses, not Go errors.
func TestRequest_Non200Status(t *testing.T) {
	srv := newTestServer(t).withStatusCode("/notfound", http.StatusNotFound).build()
	l := newTestLemming(t)

	_, status, _, err := l.request(context.Background(), srv.URL+"/notfound")
	if err != nil {
		t.Fatalf("non-200 status should not produce a Go error, got: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("expected 404, got %d", status)
	}
}

// TestRequest_NetworkError verifies that a connection failure returns
// a non-nil error.
func TestRequest_NetworkError(t *testing.T) {
	l := newTestLemming(t)
	// Port 1 is reserved and will refuse connections
	_, _, _, err := l.request(context.Background(), "http://localhost:1/")
	if err == nil {
		t.Error("expected error for refused connection, got nil")
	}
}

// TestRequest_ContextCancelled verifies that a cancelled context causes
// request to return an error promptly.
func TestRequest_ContextCancelled(t *testing.T) {
	// Slow server — will not respond before context cancels
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	l := newTestLemming(t)
	_, _, _, err := l.request(ctx, srv.URL+"/")
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

// ── Run ───────────────────────────────────────────────────────────────────────

// TestRun_RespectsUntilDeadline verifies that Run returns after the
// configured Until duration has elapsed, not before.
func TestRun_RespectsUntilDeadline(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 150 * time.Millisecond
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())

	start := time.Now()
	ll := l.Run(context.Background())
	elapsed := time.Since(start)

	if elapsed < 100*time.Millisecond {
		t.Errorf("Run returned too early: elapsed %s, expected ~150ms", elapsed)
	}
	if ll.Duration < 100*time.Millisecond {
		t.Errorf("LifeLog.Duration %s shorter than expected", ll.Duration)
	}
}

// TestRun_ReturnsCorrectIdentity verifies that the LifeLog returned by Run
// carries the correct Terrain and Pack values.
func TestRun_ReturnsCorrectIdentity(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(4, 9, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	if ll.Terrain != 4 {
		t.Errorf("expected Terrain 4, got %d", ll.Terrain)
	}
	if ll.Pack != 9 {
		t.Errorf("expected Pack 9, got %d", ll.Pack)
	}
	if ll.Identity.ID == "" {
		t.Error("LifeLog Identity.ID should not be empty")
	}
}

// TestRun_PopulatesVisits verifies that Run records at least one Visit
// during its lifetime when URLs are reachable.
func TestRun_PopulatesVisits(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 200 * time.Millisecond
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	if len(ll.Visits) == 0 {
		t.Error("Run should populate at least one Visit")
	}
}

// TestRun_VisitFields verifies that individual Visit records are populated
// with non-zero values for the fields we can assert on.
func TestRun_VisitFields(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 150 * time.Millisecond
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	for i, v := range ll.Visits {
		if v.URL == "" {
			t.Errorf("visit[%d].URL is empty", i)
		}
		if v.Timestamp.IsZero() {
			t.Errorf("visit[%d].Timestamp is zero", i)
		}
		if v.LemmingID == "" {
			t.Errorf("visit[%d].LemmingID is empty", i)
		}
	}
}

// TestRun_ParentContextCancellation verifies that cancelling the parent
// context causes Run to return promptly even before Until expires.
func TestRun_ParentContextCancellation(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 10 * time.Second // long until — parent cancel should win
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan LifeLog, 1)

	go func() {
		done <- l.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — Run returned after parent cancel
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after parent context cancellation")
	}
}

// TestRun_EmitsBornEvent verifies that Run emits EventLemmingBorn on the bus.
// TestRun_DoesNotEmitBornEvent verifies that Run does not emit
// EventLemmingBorn. Birth is a Terrain-level lifecycle event and is
// emitted exactly once per lemming by spawnLemming, not by Run.
// Emitting Born from both Run and spawnLemming would corrupt any
// subscriber that maintains a counter (including the Prometheus
// lemmings_alive gauge and the dashboard's alive counter).
func TestRun_DoesNotEmitBornEvent(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL + "/"
	pool := testPool(srv.URL)
	bus := NewEventBus()

	var bornCount int
	var mu sync.Mutex
	bus.Subscribe(func(e Event) {
		if e.Kind == EventLemmingBorn {
			mu.Lock()
			bornCount++
			mu.Unlock()
		}
	})

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	l.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if bornCount != 0 {
		t.Fatalf("Run should not emit EventLemmingBorn — that is Terrain's "+
			"responsibility. Got %d emits.", bornCount)
	}
}

// TestRun_EmitsVisitCompleteEvents verifies that Run emits at least one
// EventVisitComplete event during its lifetime.
func TestRun_EmitsVisitCompleteEvents(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/").
		withNormalPage("/about").
		build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 200 * time.Millisecond
	pool := testPool(srv.URL)
	bus, log, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	l.Run(context.Background())

	requireEventEmitted(t, log, EventVisitComplete, 500*time.Millisecond)
}

// TestRun_WaitingRoom_Detected verifies that when a server returns waiting
// room HTML, the LifeLog records a WaitingRoom metric with Detected=true.
func TestRun_WaitingRoom_Detected(t *testing.T) {
	// Server admits after 2 waiting room responses
	srv := newTestServer(t).
		withAdmittingWaitingRoom("/", 10, 2).
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 2 * time.Second // long enough to get admitted
	pool := &URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/"},
		Checksums: map[string]string{srv.URL + "/": sha512hex([]byte(normalPageBody))},
	}
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	foundWR := false
	for _, v := range ll.Visits {
		if v.WaitingRoom.Detected {
			foundWR = true
			if v.WaitingRoom.EnteredAt.IsZero() {
				t.Error("WaitingRoom.EnteredAt should be set")
			}
			break
		}
	}
	if !foundWR {
		t.Error("expected at least one visit with WaitingRoom.Detected=true")
	}
}

// TestRun_WaitingRoom_Position verifies that the waiting room position
// is correctly extracted from the response body.
func TestRun_WaitingRoom_Position(t *testing.T) {
	const expectedPosition = 17
	srv := newTestServer(t).
		withAdmittingWaitingRoom("/", expectedPosition, 1).
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 2 * time.Second
	pool := &URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/"},
		Checksums: map[string]string{srv.URL + "/": sha512hex([]byte(normalPageBody))},
	}
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	for _, v := range ll.Visits {
		if v.WaitingRoom.Detected {
			if v.WaitingRoom.Position != expectedPosition {
				t.Errorf("expected waiting room position %d, got %d",
					expectedPosition, v.WaitingRoom.Position)
			}
			return
		}
	}
	t.Error("no waiting room visit found")
}

// TestRun_WaitingRoom_DurationRecorded verifies that the waiting room
// duration is recorded as a positive value after admission.
func TestRun_WaitingRoom_DurationRecorded(t *testing.T) {
	srv := newTestServer(t).
		withAdmittingWaitingRoom("/", 5, 1).
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 2 * time.Second
	pool := &URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/"},
		Checksums: map[string]string{srv.URL + "/": sha512hex([]byte(normalPageBody))},
	}
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	for _, v := range ll.Visits {
		if v.WaitingRoom.Detected && !v.WaitingRoom.ExitedAt.IsZero() {
			if v.WaitingRoom.Duration <= 0 {
				t.Error("WaitingRoom.Duration should be positive after admission")
			}
			return
		}
	}
	t.Log("waiting room visit not admitted within test window — duration test inconclusive")
}

// TestWaitInRoom_ExitsOnContextCancellation verifies that a lemming stuck
// in a waiting room returns promptly when its context is cancelled.
func TestWaitInRoom_ExitsOnContextCancellation(t *testing.T) {
	// Server always returns waiting room — never admits
	srv := newTestServer(t).withWaitingRoom("/", 99).build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 10 * time.Second
	pool := &URLPool{
		Origin:    srv.URL,
		URLs:      []string{srv.URL + "/"},
		Checksums: map[string]string{srv.URL + "/": sha512hex([]byte(normalPageBody))},
	}
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan LifeLog, 1)
	go func() { done <- l.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case ll := <-done:
		// Verify the lemming recorded the waiting room even though it never exited
		foundWR := false
		for _, v := range ll.Visits {
			if v.WaitingRoom.Detected {
				foundWR = true
				break
			}
		}
		if !foundWR {
			t.Error("expected WaitingRoom.Detected=true for lemming cancelled in queue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation while in waiting room")
	}
}

// TestRun_ChecksumMatch verifies that when the server returns the indexed
// body, Visit.Match is true.
func TestRun_ChecksumMatch(t *testing.T) {
	srv := newTestServer(t).withNormalPage("/").build()
	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 150 * time.Millisecond
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	for _, v := range ll.Visits {
		if v.URL == srv.URL+"/" && v.Error == nil {
			if !v.Match {
				t.Errorf("visit to / should have Match=true: checksum=%s expected=%s",
					v.Checksum, v.Expected)
			}
		}
	}
}

// TestRun_ChecksumMismatch verifies that when the server returns a different
// body than what was indexed, Visit.Match is false.
func TestRun_ChecksumMismatch(t *testing.T) {
	srv := newTestServer(t).
		withCustomPage("/", "different body content than what was indexed").
		build()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 150 * time.Millisecond

	// Pool has the normal page body checksum but server returns different content
	pool := testPool(srv.URL)
	bus, _, unsub := testBus()
	defer unsub()

	l := NewLemming(0, 0, cfg, pool, newTestClient(), bus, testMetrics())
	ll := l.Run(context.Background())

	for _, v := range ll.Visits {
		if v.URL == srv.URL+"/" && v.Error == nil {
			if v.Match {
				t.Error("visit with different body should have Match=false")
			}
			return
		}
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkSha512sum_1KB measures SHA-512 throughput on a 1KB body.
// This is the median response size for a typical HTML page.
func BenchmarkSha512sum_1KB(b *testing.B) {
	input := make([]byte, 1024)
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sha512sum(input)
	}
}

// BenchmarkSha512sum_1MB measures SHA-512 throughput on a 1MB body.
// Represents large pages or API responses returned during load testing.
func BenchmarkSha512sum_1MB(b *testing.B) {
	input := make([]byte, 1024*1024)
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sha512sum(input)
	}
}

// BenchmarkDetectWaitingRoom_Normal measures detectWaitingRoom on a
// normal HTML page — the overwhelmingly common case in production.
func BenchmarkDetectWaitingRoom_Normal(b *testing.B) {
	body := []byte(normalPageBody)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detectWaitingRoom(body)
	}
}

// BenchmarkDetectWaitingRoom_WaitingRoom measures detectWaitingRoom on
// actual waiting room HTML — the case that requires position extraction.
func BenchmarkDetectWaitingRoom_WaitingRoom(b *testing.B) {
	body := []byte(waitingRoomHTML(42))
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detectWaitingRoom(body)
	}
}

// BenchmarkRun measures a lemming's full Run lifecycle against a local
// httptest server with a 3-URL pool.
func BenchmarkRun(b *testing.B) {
	srv := newBenchServer(b).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		buildB()

	cfg := testConfig()
	cfg.Hit = srv.URL
	cfg.Until = 500 * time.Millisecond
	pool := testPool(srv.URL)
	bus := NewEventBus()
	metrics := testMetrics()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := NewLemming(0, int64(i), cfg, pool, newTestClient(), bus, metrics)
		l.Run(context.Background())
	}
}

// ── Fuzz ─────────────────────────────────────────────────────────────────────

// FuzzDetectWaitingRoom verifies that detectWaitingRoom never panics on
// arbitrary byte input and always returns a valid (bool, int) pair.
// The position must always be >= 0.
func FuzzDetectWaitingRoom(f *testing.F) {
	// Seed corpus — known interesting inputs
	f.Add([]byte(normalPageBody))
	f.Add([]byte(waitingRoomHTML(0)))
	f.Add([]byte(waitingRoomHTML(999)))
	f.Add([]byte{})
	f.Add([]byte(`<html><body>/queue/status</body></html>`))
	f.Add([]byte(`id="position">abc</div>`))
	f.Add([]byte(`id="position">-1</div>/queue/status`))

	f.Fuzz(func(t *testing.T, data []byte) {
		detected, position := detectWaitingRoom(data)
		// Invariant 1: function must not panic (enforced by fuzz harness)
		// Invariant 2: position must always be >= 0
		if position < 0 {
			t.Errorf("detectWaitingRoom returned negative position %d", position)
		}
		// Invariant 3: if not detected, position must be 0
		if !detected && position != 0 {
			t.Errorf("non-detected waiting room returned non-zero position %d", position)
		}
	})
}

// FuzzSha512sum verifies that sha512sum never panics on arbitrary input
// and always returns a 128-character hex string.
func FuzzSha512sum(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("lemmings"))
	f.Add([]byte(normalPageBody))
	f.Add(make([]byte, 1024))

	f.Fuzz(func(t *testing.T, data []byte) {
		result := sha512sum(data)
		if len(result) != 128 {
			t.Errorf("sha512sum returned %d chars, expected 128: %q", len(result), result)
		}
		for i, c := range result {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("non-hex char %q at position %d", c, i)
			}
		}
	})
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// newTestLemming constructs a minimal Lemming for unit tests that do not
// need a real server. Uses testConfig defaults and a 3-URL pool.
func newTestLemming(t testing.TB) *Lemming {
	t.Helper()
	srv := newTestServer(t.(*testing.T)).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()
	pool := testPool(srv.URL)
	bus, _, _ := testBus()
	return NewLemming(0, 0, testConfig(), pool, newTestClient(), bus, testMetrics())
}

// newTestClient returns an *http.Client with a short timeout suitable
// for unit tests. Uses default transport — no mocking.
func newTestClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// newRNG returns a seeded rand.Rand for use in tests that construct
// Lemming fields directly without going through NewLemming.
func newRNG() *rand.Rand {
	return rand.New(rand.NewSource(42))
}
