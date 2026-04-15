package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/publicsuffix"
)

// ── NewDashboard ──────────────────────────────────────────────────────────────

// TestNewDashboard_Constructs verifies that NewDashboard returns a non-nil
// Dashboard with all fields correctly initialised.
func TestNewDashboard_Constructs(t *testing.T) {
	bus, _, unsub := testBus()
	defer unsub()
	metrics := testMetrics()
	cfg := testConfig()
	token := "testtoken1234567890abcdef1234567890abcdef1234567890abcdef1234"

	d := NewDashboard(cfg, bus, metrics, token)
	if d == nil {
		t.Fatal("NewDashboard returned nil")
	}
	if d.tokenHash == "" {
		t.Error("tokenHash should not be empty")
	}
	if d.tokenHash == token {
		t.Error("tokenHash should be the SHA-512 hash of the token, not the raw token")
	}
	if d.eventLog == nil {
		t.Error("eventLog should not be nil")
	}
	if d.clients.clients == nil {
		t.Fatal("client registry map should not be nil")
	}
}

// TestNewDashboard_SubscribesToBus verifies that NewDashboard registers a
// subscriber on the EventBus — events emitted after construction must
// reach the dashboard's event log.
func TestNewDashboard_SubscribesToBus(t *testing.T) {
	bus, _, unsub := testBus()
	defer unsub()
	metrics := testMetrics()
	cfg := testConfig()

	d := NewDashboard(cfg, bus, metrics, testToken())

	// Emit an event that the dashboard subscribes to
	bus.Emit(Event{Kind: EventLemmingBorn, LemmingID: "test-lemming"})

	// Give the synchronous bus a moment — bus is synchronous so this
	// is just a memory barrier, not a timing dependency
	snap := d.eventLog.Snapshot()
	found := false
	for _, e := range snap {
		if e.Kind == EventLemmingBorn {
			found = true
			break
		}
	}
	if !found {
		t.Error("dashboard event log should contain EventLemmingBorn after bus emit")
	}
}

// TestNewDashboard_TokenHashNotRaw verifies that the stored tokenHash is
// the SHA-512 of the token, not the token itself — raw tokens must never
// be stored in memory beyond the print-to-STDOUT moment.
func TestNewDashboard_TokenHashNotRaw(t *testing.T) {
	bus, _, unsub := testBus()
	defer unsub()

	raw := testToken()
	d := NewDashboard(testConfig(), bus, testMetrics(), raw)

	if d.tokenHash == raw {
		t.Error("tokenHash must be SHA-512 hash of token, not the raw token value")
	}
	if len(d.tokenHash) != 128 {
		t.Errorf("SHA-512 hex hash should be 128 chars, got %d", len(d.tokenHash))
	}
}

// ── handleAuth ────────────────────────────────────────────────────────────────

// TestHandleAuth_RejectsGET verifies that GET requests to /auth return
// 405 Method Not Allowed.
func TestHandleAuth_RejectsGET(t *testing.T) {
	d, _ := newTestDashboard(t)

	req := httptest.NewRequest(http.MethodGet, "/auth", nil)
	w := httptest.NewRecorder()
	d.handleAuth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// TestHandleAuth_RejectsMissingToken verifies that a POST to /auth without
// a token field returns 400 Bad Request.
func TestHandleAuth_RejectsMissingToken(t *testing.T) {
	d, _ := newTestDashboard(t)

	req := httptest.NewRequest(http.MethodPost, "/auth",
		strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handleAuth(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// TestHandleAuth_RejectsWrongToken verifies that a POST with an incorrect
// token returns 401 Unauthorized.
func TestHandleAuth_RejectsWrongToken(t *testing.T) {
	d, _ := newTestDashboard(t)

	req := httptest.NewRequest(http.MethodPost, "/auth",
		strings.NewReader("token=wrongtoken"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handleAuth(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// TestHandleAuth_AcceptsCorrectToken verifies that a POST with the correct
// token sets the session cookie and redirects.
func TestHandleAuth_AcceptsCorrectToken(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	body := fmt.Sprintf("token=%s", raw)
	req := httptest.NewRequest(http.MethodPost, "/auth",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handleAuth(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected 303 redirect, got %d", w.Code)
	}

	// Verify session cookie is set
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == tokenCookieName {
			found = true
			if c.Value == "" {
				t.Error("session cookie value should not be empty")
			}
			if !c.HttpOnly {
				t.Error("session cookie should be HttpOnly")
			}
			break
		}
	}
	if !found {
		t.Errorf("expected cookie %q to be set after auth", tokenCookieName)
	}
}

// TestHandleAuth_EmitsAuthEvent verifies that a successful auth emits
// EventDashboardAuth on the bus.
func TestHandleAuth_EmitsAuthEvent(t *testing.T) {
	raw := testToken()
	d, log := newTestDashboardWithToken(t, raw)
	_ = d

	body := fmt.Sprintf("token=%s", raw)
	req := httptest.NewRequest(http.MethodPost, "/auth",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handleAuth(w, req)

	requireEventEmitted(t, log, EventDashboardAuth, 200*time.Millisecond)
}

// TestHandleAuth_FailedAuthEmitsEventWithError verifies that a failed auth
// attempt emits EventDashboardAuth with a non-nil Err field.
func TestHandleAuth_FailedAuthEmitsEventWithError(t *testing.T) {
	d, _ := newTestDashboard(t)

	var receivedErr error
	var mu sync.Mutex
	d.bus.Subscribe(func(e Event) {
		if e.Kind == EventDashboardAuth && e.Err != nil {
			mu.Lock()
			receivedErr = e.Err
			mu.Unlock()
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/auth",
		strings.NewReader("token=badtoken"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handleAuth(w, req)

	mu.Lock()
	defer mu.Unlock()
	if receivedErr == nil {
		t.Error("expected EventDashboardAuth with non-nil Err for failed auth")
	}
}

// ── requireAuth ───────────────────────────────────────────────────────────────

// TestRequireAuth_RejectsNoCookie verifies that requireAuth returns 401
// when no session cookie is present.
func TestRequireAuth_RejectsNoCookie(t *testing.T) {
	d, _ := newTestDashboard(t)

	called := false
	handler := d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("inner handler should not be called when no cookie present")
	}
}

// TestRequireAuth_RejectsWrongCookieValue verifies that requireAuth returns
// 401 when the session cookie contains an incorrect value.
func TestRequireAuth_RejectsWrongCookieValue(t *testing.T) {
	d, _ := newTestDashboard(t)

	called := false
	handler := d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: "wrongvalue"})
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("inner handler should not be called with wrong cookie value")
	}
}

// TestRequireAuth_PassesWithCorrectCookie verifies that requireAuth calls
// the inner handler when a valid session cookie is present.
func TestRequireAuth_PassesWithCorrectCookie(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	called := false
	handler := d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.AddCookie(&http.Cookie{
		Name:  tokenCookieName,
		Value: d.tokenHash, // the stored hash is what the cookie holds
	})
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("inner handler should be called with correct cookie")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ── isAuthenticated ───────────────────────────────────────────────────────────

// TestIsAuthenticated_FalseWithoutCookie verifies false is returned when
// no session cookie is present in the request.
func TestIsAuthenticated_FalseWithoutCookie(t *testing.T) {
	d, _ := newTestDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if d.isAuthenticated(req) {
		t.Error("isAuthenticated should return false with no cookie")
	}
}

// TestIsAuthenticated_FalseWithWrongValue verifies false is returned when
// the cookie contains an incorrect hash value.
func TestIsAuthenticated_FalseWithWrongValue(t *testing.T) {
	d, _ := newTestDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: "notthehash"})
	if d.isAuthenticated(req) {
		t.Error("isAuthenticated should return false with wrong cookie value")
	}
}

// TestIsAuthenticated_TrueWithCorrectValue verifies true is returned when
// the cookie contains the correct token hash.
func TestIsAuthenticated_TrueWithCorrectValue(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	if !d.isAuthenticated(req) {
		t.Error("isAuthenticated should return true with correct cookie value")
	}
}

// ── handleRoot ────────────────────────────────────────────────────────────────

// TestHandleRoot_UnauthenticatedReturnsAuthForm verifies that an
// unauthenticated request to / returns the token entry form.
func TestHandleRoot_UnauthenticatedReturnsAuthForm(t *testing.T) {
	d, _ := newTestDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handleRoot(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "token") {
		t.Error("unauthenticated root should contain token input form")
	}
	if !strings.Contains(body, "/auth") {
		t.Error("unauthenticated root should contain form action pointing to /auth")
	}
}

// TestHandleRoot_AuthenticatedReturnsDashboard verifies that an authenticated
// request to / returns the live dashboard HTML.
func TestHandleRoot_AuthenticatedReturnsDashboard(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	w := httptest.NewRecorder()
	d.handleRoot(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "lemmings") {
		t.Error("authenticated root should return dashboard HTML containing 'lemmings'")
	}
	// Dashboard should contain EventSource setup
	if !strings.Contains(body, "EventSource") {
		t.Error("dashboard HTML should set up SSE EventSource")
	}
}

// ── handleMetrics ─────────────────────────────────────────────────────────────

// TestHandleMetrics_ReturnsJSON verifies that /metrics returns valid JSON
// containing the expected metric fields.
func TestHandleMetrics_ReturnsJSON(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	// Set some non-zero metrics
	d.metrics.LemmingsAlive.Store(42)
	d.metrics.Total2xx.Store(100)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	w := httptest.NewRecorder()
	d.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("expected application/json content type, got %q", contentType)
	}

	var snap metricsSnapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("failed to decode metrics JSON: %v", err)
	}
	if snap.Alive != 42 {
		t.Errorf("expected Alive=42, got %d", snap.Alive)
	}
	if snap.Xx2 != 100 {
		t.Errorf("expected Xx2=100, got %d", snap.Xx2)
	}
}

// TestHandleMetrics_RequiresAuth verifies that /metrics returns 401
// without a valid session cookie.
func TestHandleMetrics_RequiresAuth(t *testing.T) {
	d, _ := newTestDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	// Wire through the mux that requireAuth wraps
	handler := d.requireAuth(d.handleMetrics)
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ── handleReplay ──────────────────────────────────────────────────────────────

// TestHandleReplay_ReturnsEventLog verifies that /replay returns a JSON
// payload containing the events recorded in the event log.
func TestHandleReplay_ReturnsEventLog(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	// Inject events into the log directly
	d.eventLog.Record(Event{Kind: EventLemmingBorn, LemmingID: "lemming-1"})
	d.eventLog.Record(Event{Kind: EventLemmingDied, LemmingID: "lemming-1"})

	req := httptest.NewRequest(http.MethodGet, "/replay", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	w := httptest.NewRecorder()
	d.handleReplay(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var payload struct {
		Events []Event `json:"events"`
		Count  int     `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("failed to decode replay JSON: %v", err)
	}
	if payload.Count != 2 {
		t.Errorf("expected count=2, got %d", payload.Count)
	}
	if len(payload.Events) != 2 {
		t.Errorf("expected 2 events, got %d", len(payload.Events))
	}
}

// TestHandleReplay_EmptyLog verifies that /replay returns an empty events
// array when no events have been recorded yet.
func TestHandleReplay_EmptyLog(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	req := httptest.NewRequest(http.MethodGet, "/replay", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	w := httptest.NewRecorder()
	d.handleReplay(w, req)

	var payload struct {
		Events []Event `json:"events"`
		Count  int     `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("failed to decode replay JSON: %v", err)
	}
	if payload.Count != 0 {
		t.Errorf("expected count=0 for empty log, got %d", payload.Count)
	}
}

// TestHandleReplay_RequiresAuth verifies that /replay returns 401 without
// a valid session cookie.
func TestHandleReplay_RequiresAuth(t *testing.T) {
	d, _ := newTestDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/replay", nil)
	w := httptest.NewRecorder()
	handler := d.requireAuth(d.handleReplay)
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ── handleSSE ─────────────────────────────────────────────────────────────────

// TestHandleSSE_StreamsEvents verifies that events broadcast to all clients
// appear in the SSE stream.
func TestHandleSSE_StreamsEvents(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	// Use a pipe to capture SSE output
	pr, pw := io.Pipe()

	// Custom ResponseWriter that implements http.Flusher
	w := newFlushRecorder(pw)
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})

	// Run SSE handler in background — it blocks until context cancels
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	sseErr := make(chan error, 1)
	go func() {
		d.handleSSE(w, req)
		pw.Close()
		sseErr <- nil
	}()

	// Give handler time to register the client
	time.Sleep(20 * time.Millisecond)

	// Broadcast an event to all clients
	d.clients.broadcast(sseEvent{
		Kind: string(EventLemmingBorn),
		Data: map[string]any{"lemming_id": "test-123"},
	})

	// Read from the pipe with a deadline
	readDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		readDone <- string(buf[:n])
	}()

	var received string
	select {
	case received = <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream did not deliver event within 2 seconds")
	}

	cancel() // stop the SSE handler

	if !strings.Contains(received, "data:") {
		t.Errorf("SSE output should begin with 'data:', got: %q", received)
	}
	if !strings.Contains(received, string(EventLemmingBorn)) {
		t.Errorf("SSE output should contain event kind %q, got: %q",
			EventLemmingBorn, received)
	}
}

// TestHandleSSE_RequiresAuth verifies that /events returns 401 without a
// valid session cookie — unauthenticated clients must not receive the stream.
func TestHandleSSE_RequiresAuth(t *testing.T) {
	d, _ := newTestDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	w := httptest.NewRecorder()
	handler := d.requireAuth(d.handleSSE)
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ── clientRegistry ────────────────────────────────────────────────────────────

// TestClientRegistry_Add_UniqueIDs verifies that each call to add returns
// a distinct ID.
func TestClientRegistry_Add_UniqueIDs(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	ids := make(map[uint64]bool)

	for i := 0; i < 100; i++ {
		id := cr.add()
		if ids[id] {
			t.Fatalf("duplicate client ID %d", id)
		}
		ids[id] = true
	}
}

// TestClientRegistry_Remove_ClosesChannel verifies that remove closes the
// client's channel and removes its entry from the registry.
func TestClientRegistry_Remove_ClosesChannel(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	id := cr.add()
	ch := cr.channel(id)

	cr.remove(id)

	// Channel should be closed
	select {
	case _, open := <-ch:
		if open {
			t.Error("channel should be closed after remove")
		}
	default:
		t.Error("closed channel should return immediately")
	}

	// Entry should be gone
	cr.mu.RLock()
	_, exists := cr.clients[id]
	cr.mu.RUnlock()
	if exists {
		t.Error("client entry should be removed from registry after remove")
	}
}

// TestClientRegistry_Remove_Idempotent verifies that calling remove twice
// for the same ID does not panic.
func TestClientRegistry_Remove_Idempotent(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	id := cr.add()
	cr.remove(id)
	cr.remove(id) // must not panic
}

// TestClientRegistry_Broadcast_DeliveresToAll verifies that broadcast sends
// the event to every registered client channel.
func TestClientRegistry_Broadcast_DeliveresToAll(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	const numClients = 5

	ids := make([]uint64, numClients)
	for i := range ids {
		ids[i] = cr.add()
	}

	evt := sseEvent{Kind: string(EventLemmingBorn)}
	cr.broadcast(evt)

	for _, id := range ids {
		ch := cr.channel(id)
		select {
		case got := <-ch:
			if got.Kind != evt.Kind {
				t.Errorf("client %d: expected kind %q, got %q", id, evt.Kind, got.Kind)
			}
		default:
			t.Errorf("client %d: expected event in channel, got nothing", id)
		}
	}
}

// TestClientRegistry_Broadcast_DropsOnFullChannel verifies that broadcast
// does not block when a client's channel is full — the event is dropped
// for that client only, other clients are unaffected.
func TestClientRegistry_Broadcast_DropsOnFullChannel(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}

	// Add two clients
	fastID := cr.add()
	slowID := cr.add()

	// Fill the slow client's channel completely
	slowCh := cr.channel(slowID)
	for len(slowCh) < cap(slowCh) {
		slowCh <- sseEvent{Kind: "filler"}
	}

	// Broadcast must not block even though slow client is full
	done := make(chan struct{})
	go func() {
		cr.broadcast(sseEvent{Kind: string(EventLemmingDied)})
		close(done)
	}()

	select {
	case <-done:
		// good — did not block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("broadcast blocked on a full client channel")
	}

	// Fast client should have received the event
	fastCh := cr.channel(fastID)
	select {
	case got := <-fastCh:
		if got.Kind != string(EventLemmingDied) {
			t.Errorf("fast client: expected %q, got %q", EventLemmingDied, got.Kind)
		}
	default:
		t.Error("fast client should have received the broadcast event")
	}
}

// TestClientRegistry_Broadcast_Concurrent verifies that concurrent broadcast
// calls from multiple goroutines do not cause data races. Run with -race.
func TestClientRegistry_Broadcast_Concurrent(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	for i := 0; i < 10; i++ {
		cr.add()
	}

	var wg sync.WaitGroup
	const senders = 50
	wg.Add(senders)

	for i := 0; i < senders; i++ {
		go func() {
			defer wg.Done()
			cr.broadcast(sseEvent{Kind: string(EventVisitComplete)})
		}()
	}

	wg.Wait() // must complete without race detector firing
}

// TestClientRegistry_Channel_NilForMissingID verifies that channel returns
// nil for an ID that was never registered or has been removed.
func TestClientRegistry_Channel_NilForMissingID(t *testing.T) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	ch := cr.channel(99999)
	if ch != nil {
		t.Error("channel should return nil for unregistered ID")
	}
}

// ── snapshot ──────────────────────────────────────────────────────────────────

// TestSnapshot_ReadsMetricsCorrectly verifies that snapshot returns a
// metricsSnapshot reflecting the current atomic counter values.
func TestSnapshot_ReadsMetricsCorrectly(t *testing.T) {
	d, _ := newTestDashboard(t)

	d.metrics.LemmingsAlive.Store(10)
	d.metrics.LemmingsCompleted.Store(90)
	d.metrics.LemmingsFailed.Store(2)
	d.metrics.TerrainsOnline.Store(5)
	d.metrics.TotalVisits.Store(1000)
	d.metrics.TotalBytes.Store(1024 * 1024)
	d.metrics.TotalWaitingRoom.Store(7)
	d.metrics.Total2xx.Store(950)
	d.metrics.Total3xx.Store(30)
	d.metrics.Total4xx.Store(15)
	d.metrics.Total5xx.Store(5)
	d.metrics.OverflowLogs.Store(3)
	d.metrics.DroppedLogs.Store(1)

	snap := d.snapshot(42)

	if snap.Alive != 10 {
		t.Errorf("expected Alive=10, got %d", snap.Alive)
	}
	if snap.Completed != 90 {
		t.Errorf("expected Completed=90, got %d", snap.Completed)
	}
	if snap.Failed != 2 {
		t.Errorf("expected Failed=2, got %d", snap.Failed)
	}
	if snap.TerrainsOnline != 5 {
		t.Errorf("expected TerrainsOnline=5, got %d", snap.TerrainsOnline)
	}
	if snap.TotalVisits != 1000 {
		t.Errorf("expected TotalVisits=1000, got %d", snap.TotalVisits)
	}
	if snap.WaitingRoom != 7 {
		t.Errorf("expected WaitingRoom=7, got %d", snap.WaitingRoom)
	}
	if snap.Xx2 != 950 {
		t.Errorf("expected Xx2=950, got %d", snap.Xx2)
	}
	if snap.Xx3 != 30 {
		t.Errorf("expected Xx3=30, got %d", snap.Xx3)
	}
	if snap.Xx4 != 15 {
		t.Errorf("expected Xx4=15, got %d", snap.Xx4)
	}
	if snap.Xx5 != 5 {
		t.Errorf("expected Xx5=5, got %d", snap.Xx5)
	}
	if snap.OverflowLogs != 3 {
		t.Errorf("expected OverflowLogs=3, got %d", snap.OverflowLogs)
	}
	if snap.DroppedLogs != 1 {
		t.Errorf("expected DroppedLogs=1, got %d", snap.DroppedLogs)
	}
	if snap.ElapsedSecs != 42 {
		t.Errorf("expected ElapsedSecs=42, got %d", snap.ElapsedSecs)
	}
}

// TestSnapshot_FormatsBytesHumanReadable verifies that TotalBytes in the
// snapshot is a human-readable string, not a raw integer.
func TestSnapshot_FormatsBytesHumanReadable(t *testing.T) {
	d, _ := newTestDashboard(t)
	d.metrics.TotalBytes.Store(1024 * 1024) // 1 MB

	snap := d.snapshot(0)
	if snap.TotalBytes != "1.00 MB" {
		t.Errorf("expected '1.00 MB', got %q", snap.TotalBytes)
	}
}

// ── handleEvent ───────────────────────────────────────────────────────────────

// TestHandleEvent_BroadcastsToClients verifies that handleEvent fans the
// event out to all connected SSE clients via the client registry.
func TestHandleEvent_BroadcastsToClients(t *testing.T) {
	d, _ := newTestDashboard(t)

	// Register a client manually
	id := d.clients.add()
	ch := d.clients.channel(id)
	defer d.clients.remove(id)

	d.handleEvent(Event{
		Kind:      EventLemmingBorn,
		LemmingID: "test-lemming",
		Terrain:   1,
		Pack:      2,
		URL:       "http://example.com/",
	})

	select {
	case evt := <-ch:
		if evt.Kind != string(EventLemmingBorn) {
			t.Errorf("expected kind %q, got %q", EventLemmingBorn, evt.Kind)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handleEvent did not broadcast to registered client")
	}
}

// TestHandleEvent_NonBlocking verifies that handleEvent returns quickly
// even when all client channels are full.
func TestHandleEvent_NonBlocking(t *testing.T) {
	d, _ := newTestDashboard(t)

	// Add a client and fill its channel
	id := d.clients.add()
	ch := d.clients.channel(id)
	for len(ch) < cap(ch) {
		ch <- sseEvent{Kind: "filler"}
	}
	defer d.clients.remove(id)

	start := time.Now()
	for i := 0; i < 100; i++ {
		d.handleEvent(Event{Kind: EventVisitComplete})
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Millisecond {
		t.Errorf("handleEvent appears to block: 100 calls took %s", elapsed)
	}
}

// ── broadcastMetrics ──────────────────────────────────────────────────────────

// TestBroadcastMetrics_SendsMetricsKind verifies that broadcastMetrics
// delivers SSE events with kind="metrics" to connected clients.
func TestBroadcastMetrics_SendsMetricsKind(t *testing.T) {
	d, _ := newTestDashboard(t)

	id := d.clients.add()
	ch := d.clients.channel(id)
	defer d.clients.remove(id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.broadcastMetrics(ctx)

	// Wait for at least one metrics event
	select {
	case evt := <-ch:
		if evt.Kind != "metrics" {
			t.Errorf("expected kind 'metrics', got %q", evt.Kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("broadcastMetrics did not send a metrics event within 3 seconds")
	}
}

// TestBroadcastMetrics_StopsOnContextCancel verifies that broadcastMetrics
// stops sending when the context is cancelled.
func TestBroadcastMetrics_StopsOnContextCancel(t *testing.T) {
	d, _ := newTestDashboard(t)

	id := d.clients.add()
	ch := d.clients.channel(id)
	defer d.clients.remove(id)

	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan struct{})
	go func() {
		d.broadcastMetrics(ctx)
		close(stopped)
	}()

	// Drain one event then cancel
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("broadcastMetrics did not send initial event")
	}

	cancel()

	select {
	case <-stopped:
		// correct — goroutine exited after cancel
	case <-time.After(2 * time.Second):
		t.Fatal("broadcastMetrics did not stop after context cancellation")
	}
}

// ── authHTML / dashboardHTML ──────────────────────────────────────────────────

// TestAuthHTML_NonEmpty verifies that authHTML returns a non-empty string.
func TestAuthHTML_NonEmpty(t *testing.T) {
	got := authHTML()
	if len(got) == 0 {
		t.Error("authHTML should return non-empty string")
	}
}

// TestAuthHTML_ContainsForm verifies that the auth HTML contains a form
// that POSTs to /auth with a password field.
func TestAuthHTML_ContainsForm(t *testing.T) {
	got := authHTML()
	if !strings.Contains(got, `action="/auth"`) {
		t.Error("auth HTML should contain form action pointing to /auth")
	}
	if !strings.Contains(got, `type="password"`) {
		t.Error("auth HTML should contain password input field")
	}
	if !strings.Contains(got, `method="POST"`) {
		t.Error("auth HTML should use POST method")
	}
}

// TestAuthHTML_ValidDoctype verifies that the auth HTML begins with a
// proper DOCTYPE declaration.
func TestAuthHTML_ValidDoctype(t *testing.T) {
	got := authHTML()
	if !strings.HasPrefix(strings.TrimSpace(got), "<!DOCTYPE html>") {
		t.Error("auth HTML should begin with <!DOCTYPE html>")
	}
}

// TestDashboardHTML_ContainsHitURL verifies that the dashboard HTML
// contains the configured hit URL.
func TestDashboardHTML_ContainsHitURL(t *testing.T) {
	cfg := testConfig()
	cfg.Hit = "https://example.com/launch"
	got := dashboardHTML(cfg)
	if !strings.Contains(got, "https://example.com/launch") {
		t.Error("dashboard HTML should contain the hit URL")
	}
}

// TestDashboardHTML_ContainsMetricElements verifies that the dashboard HTML
// contains the expected metric display elements referenced by the JS.
func TestDashboardHTML_ContainsMetricElements(t *testing.T) {
	got := dashboardHTML(testConfig())
	ids := []string{
		`id="alive"`,
		`id="completed"`,
		`id="failed"`,
		`id="visits"`,
		`id="bytes"`,
		`id="xx2"`,
		`id="xx3"`,
		`id="xx4"`,
		`id="xx5"`,
		`id="elapsed"`,
	}
	for _, id := range ids {
		if !strings.Contains(got, id) {
			t.Errorf("dashboard HTML should contain element %q", id)
		}
	}
}

// TestDashboardHTML_ContainsEventSource verifies that the dashboard JS
// sets up an SSE EventSource connection to /events.
func TestDashboardHTML_ContainsEventSource(t *testing.T) {
	got := dashboardHTML(testConfig())
	if !strings.Contains(got, "EventSource") {
		t.Error("dashboard HTML should set up SSE EventSource")
	}
	if !strings.Contains(got, "/events") {
		t.Error("dashboard HTML should connect EventSource to /events")
	}
}

// TestDashboardHTML_ContainsReplayFetch verifies that the dashboard JS
// fetches /replay on page load for cold-join event history.
func TestDashboardHTML_ContainsReplayFetch(t *testing.T) {
	got := dashboardHTML(testConfig())
	if !strings.Contains(got, "/replay") {
		t.Error("dashboard HTML should fetch /replay for cold-join history")
	}
}

// TestDashboardHTML_ValidDoctype verifies that the dashboard HTML begins
// with a proper DOCTYPE declaration.
func TestDashboardHTML_ValidDoctype(t *testing.T) {
	got := dashboardHTML(testConfig())
	if !strings.HasPrefix(strings.TrimSpace(got), "<!DOCTYPE html>") {
		t.Error("dashboard HTML should begin with <!DOCTYPE html>")
	}
}

// ── Serve (integration) ───────────────────────────────────────────────────────

// TestServe_HealthCheck verifies that the dashboard HTTP server starts,
// handles requests, and shuts down cleanly when the context is cancelled.
func TestServe_HealthCheck(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	// Find a free port
	listener, port := freePort(t)
	listener.Close()

	cfg := testConfig()
	cfg.DashboardPort = port
	d.cfg = cfg

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- d.Serve(ctx)
	}()

	// Wait for server to start
	var resp *http.Response
	var err error
	for i := 0; i < 20; i++ {
		resp, err = http.Get(fmt.Sprintf("http://localhost:%d/", port))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dashboard server did not start: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from dashboard root, got %d", resp.StatusCode)
	}

	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not shut down after context cancellation")
	}
}

// TestServe_AuthFlow verifies the complete authentication flow against
// a real running dashboard server using an http.Client with cookie jar.
func TestServe_AuthFlow(t *testing.T) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(t, raw)

	listener, port := freePort(t)
	listener.Close()

	cfg := testConfig()
	cfg.DashboardPort = port
	d.cfg = cfg

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Serve(ctx)

	// Wait for server
	baseURL := fmt.Sprintf("http://localhost:%d", port)
	waitForServer(t, baseURL+"/", 20, 50*time.Millisecond)

	// Create client with cookie jar to persist session
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		t.Fatalf("cookie jar error: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // follow redirects
		},
	}

	// POST correct token to /auth
	resp, err := client.Post(
		baseURL+"/auth",
		"application/x-www-form-urlencoded",
		strings.NewReader(fmt.Sprintf("token=%s", raw)),
	)
	if err != nil {
		t.Fatalf("auth POST error: %v", err)
	}
	resp.Body.Close()

	// Now GET /metrics — should succeed with cookie jar holding session
	resp, err = client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("metrics GET error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /metrics after auth, got %d", resp.StatusCode)
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkClientRegistry_Broadcast_OneClient measures broadcast throughput
// with a single connected client — the baseline case.
func BenchmarkClientRegistry_Broadcast_OneClient(b *testing.B) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	id := cr.add()
	ch := cr.channel(id)

	// Drain goroutine to keep channel from filling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
			}
		}
	}()

	evt := sseEvent{Kind: string(EventVisitComplete)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cr.broadcast(evt)
	}
}

// BenchmarkClientRegistry_Broadcast_HundredClients measures broadcast
// throughput with 100 connected clients — a heavily monitored run.
//
//go:build bench
func BenchmarkClientRegistry_Broadcast_HundredClients(b *testing.B) {
	cr := &clientRegistry{clients: make(map[uint64]chan sseEvent)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 100; i++ {
		id := cr.add()
		ch := cr.channel(id)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ch:
				}
			}
		}()
	}

	evt := sseEvent{Kind: string(EventVisitComplete)}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cr.broadcast(evt)
		}
	})
}

// BenchmarkHandleMetrics_Concurrent measures /metrics handler throughput
// under concurrent authenticated requests.
//
//go:build bench
func BenchmarkHandleMetrics_Concurrent(b *testing.B) {
	raw := testToken()
	d, _ := newTestDashboardWithToken(b, raw)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
			w := httptest.NewRecorder()
			d.handleMetrics(w, req)
		}
	})
}

// BenchmarkSnapshot_Concurrent measures snapshot throughput under concurrent
// metric updates — simulates a live swarm updating counters while the
// dashboard reads them.
//
//go:build bench
func BenchmarkSnapshot_Concurrent(b *testing.B) {
	d, _ := newTestDashboard(b)

	// Background goroutine updating metrics continuously
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var i int64
		for {
			select {
			case <-ctx.Done():
				return
			default:
				d.metrics.LemmingsAlive.Store(i % 1000)
				d.metrics.TotalVisits.Add(1)
				i++
			}
		}
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			d.snapshot(0)
		}
	})
}

// ── Fuzz ──────────────────────────────────────────────────────────────────────

// FuzzHandleAuth verifies that handleAuth never panics on arbitrary token
// values and always returns a valid HTTP status code.
func FuzzHandleAuth(f *testing.F) {
	f.Add("correcttoken")
	f.Add("")
	f.Add("a")
	f.Add(strings.Repeat("x", 1000))
	f.Add("token=injected&extra=field")

	f.Fuzz(func(t *testing.T, token string) {
		d, _ := newTestDashboard(t)

		body := fmt.Sprintf("token=%s", token)
		req := httptest.NewRequest(http.MethodPost, "/auth",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()

		// Must not panic
		d.handleAuth(w, req)

		// Must return a valid HTTP status
		validStatuses := map[int]bool{
			http.StatusOK:              true,
			http.StatusBadRequest:      true,
			http.StatusUnauthorized:    true,
			http.StatusMethodNotAllowed: true,
			http.StatusSeeOther:        true,
		}
		if !validStatuses[w.Code] {
			t.Errorf("handleAuth returned unexpected status %d for token %q",
				w.Code, token)
		}
	})
}

// FuzzDashboardHTML verifies that dashboardHTML never panics on arbitrary
// hit URL strings and always returns a non-empty HTML string.
func FuzzDashboardHTML(f *testing.F) {
	f.Add("http://localhost:8080/")
	f.Add("https://example.com/")
	f.Add("")
	f.Add("<script>alert(1)</script>")
	f.Add("http://localhost:8080/?q=<>&x='")

	f.Fuzz(func(t *testing.T, hit string) {
		cfg := testConfig()
		cfg.Hit = hit

		// Must not panic
		got := dashboardHTML(cfg)
		if len(got) == 0 {
			t.Error("dashboardHTML should never return empty string")
		}
	})
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// newTestDashboard constructs a Dashboard with a random test token wired
// to a fresh EventBus and zeroed metrics. Returns the dashboard and its
// event log for assertion.
//
// The EventBus unsubscribe is registered via t.Cleanup automatically.
func newTestDashboard(tb testing.TB) (*Dashboard, *EventLog) {
	tb.Helper()
	return newTestDashboardWithToken(tb, testToken())
}

// newTestDashboardWithToken constructs a Dashboard with the provided raw
// token. Use this when the test needs to authenticate using the same token.
func newTestDashboardWithToken(tb testing.TB, raw string) (*Dashboard, *EventLog) {
	tb.Helper()
	bus := NewEventBus()
	log := NewEventLog(256)
	bus.Subscribe(log.AsSubscriber())
	metrics := testMetrics()
	cfg := testConfig()

	d := NewDashboard(cfg, bus, metrics, raw)
	tb.Cleanup(func() { bus.Close() })

	return d, log
}

// testToken returns a fixed 64-char hex string suitable as a test token.
// It is not cryptographically random — use it only in tests, never in
// production paths.
func testToken() string {
	return strings.Repeat("a", 64)
}

// flushRecorder is an http.ResponseWriter that also implements http.Flusher
// by writing directly to an io.WriteCloser. Used to test SSE handlers that
// call w.(http.Flusher).Flush().
type flushRecorder struct {
	header http.Header
	w      io.WriteCloser
	code   int
}

// newFlushRecorder constructs a flushRecorder that writes to w.
func newFlushRecorder(w io.WriteCloser) *flushRecorder {
	return &flushRecorder{
		header: make(http.Header),
		w:      w,
		code:   http.StatusOK,
	}
}

func (r *flushRecorder) Header() http.Header         { return r.header }
func (r *flushRecorder) WriteHeader(code int)        { r.code = code }
func (r *flushRecorder) Write(b []byte) (int, error) { return r.w.Write(b) }
func (r *flushRecorder) Flush()                      {} // no-op — writes go directly to pipe

// freePort finds an available TCP port by opening a listener on :0 and
// returning both the listener and the assigned port number. The caller
// must close the listener before using the port.
func freePort(tb testing.TB) (interface{ Close() error }, int) {
	tb.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		tb.Fatalf("could not find free port: %v", err)
	}
	return l, l.Addr().(*net.TCPAddr).Port
}

// waitForServer polls the given URL until it returns a non-error response
// or maxAttempts is exhausted. Fails the test if the server never responds.
func waitForServer(tb testing.TB, url string, maxAttempts int, interval time.Duration) {
	tb.Helper()
	for i := 0; i < maxAttempts; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(interval)
	}
	tb.Fatalf("server at %s did not respond after %d attempts", url, maxAttempts)
}

// atomicMax atomically updates the value at addr to max(*addr, candidate).
// Used in tests that track peak concurrency across goroutines.
func atomicMax(addr *atomic.Int64, candidate int64) {
	for {
		old := addr.Load()
		if candidate <= old {
			return
		}
		if addr.CompareAndSwap(old, candidate) {
			return
		}
	}
}
