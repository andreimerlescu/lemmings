package main

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// dashboardPollInterval is how often the browser polls for metric updates.
	dashboardSSEInterval = 1 * time.Second

	// eventLogCapacity is how many events the dashboard replays to a
	// newly authenticated session.
	eventLogCapacity = 1000

	// tokenCookieName is the cookie set after successful auth.
	tokenCookieName = "lemmings_token"
)

// Dashboard serves the live monitoring interface on localhost:PORT.
// It owns its own HTTP server, token authentication, SSE stream,
// and EventBus subscription. It never blocks the swarm.
type Dashboard struct {
	cfg       SwarmConfig
	bus       *EventBus
	metrics   *SwarmMetrics
	tokenHash string    // SHA512 hex of the raw token
	eventLog  *EventLog // recent event replay for cold joins
	clients   clientRegistry
	unsub     func() // EventBus unsubscribe handle
}

// clientRegistry manages active SSE connections.
type clientRegistry struct {
	mu      sync.RWMutex
	clients map[uint64]chan sseEvent
	nextID  atomic.Uint64
}

// sseEvent is a single server-sent event payload.
type sseEvent struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

// metricsSnapshot is the payload sent to the browser every second.
type metricsSnapshot struct {
	Alive          int64  `json:"alive"`
	Completed      int64  `json:"completed"`
	Failed         int64  `json:"failed"`
	TerrainsOnline int64  `json:"terrains_online"`
	TotalVisits    int64  `json:"total_visits"`
	TotalBytes     string `json:"total_bytes"`
	WaitingRoom    int64  `json:"waiting_room"`
	Xx2            int64  `json:"XX2"`
	Xx3            int64  `json:"XX3"`
	Xx4            int64  `json:"XX4"`
	Xx5            int64  `json:"XX5"`
	OverflowLogs   int64  `json:"overflow_logs"`
	DroppedLogs    int64  `json:"dropped_logs"`
	ElapsedSecs    int64  `json:"elapsed_secs"`
}

// NewDashboard constructs a Dashboard.
func NewDashboard(
	cfg SwarmConfig,
	bus *EventBus,
	metrics *SwarmMetrics,
	token string,
) *Dashboard {
	// Store the SHA512 of the token, never the raw token, so even if
	// the dashboard's memory is inspected the raw token isn't present.
	h := sha512.New()
	h.Write([]byte(token))
	tokenHash := hex.EncodeToString(h.Sum(nil))

	log := NewEventLog(eventLogCapacity)

	d := &Dashboard{
		cfg:       cfg,
		bus:       bus,
		metrics:   metrics,
		tokenHash: tokenHash,
		eventLog:  log,
		clients: clientRegistry{
			clients: make(map[uint64]chan sseEvent),
		},
	}

	// Subscribe to the event bus — non-blocking, hands off to SSE clients.
	d.unsub = bus.Subscribe(Filter(
		d.handleEvent,
		EventLemmingBorn,
		EventLemmingDied,
		EventLemmingFailed,
		EventVisitComplete,
		EventVisitError,
		EventWaitingRoom,
		EventTerrainOnline,
		EventTerrainDone,
		EventLogOverflow,
		EventLogDropped,
	))

	// Also record everything to the event log for cold-join replay.
	bus.Subscribe(log.AsSubscriber())

	return d
}

// Serve starts the HTTP server and blocks until ctx is cancelled.
func (d *Dashboard) Serve(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", d.handleRoot)
	mux.HandleFunc("/auth", d.handleAuth)
	mux.HandleFunc("/events", d.requireAuth(d.handleSSE))
	mux.HandleFunc("/metrics", d.requireAuth(d.handleMetrics))
	mux.HandleFunc("/replay", d.requireAuth(d.handleReplay))

	addr := fmt.Sprintf("localhost:%d", d.cfg.DashboardPort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE connections are long-lived
		IdleTimeout:  120 * time.Second,
	}

	// Start the metrics broadcast ticker
	go d.broadcastMetrics(ctx)

	// Shutdown when context cancels
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if d.unsub != nil {
			d.unsub()
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("dashboard server: %w", err)
	}
	return nil
}

// ── Auth ─────────────────────────────────────────────────────────────────────

// handleAuth accepts a POST with a token field, verifies it via constant-time
// SHA512 comparison, and sets a session cookie on success.
func (d *Dashboard) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	raw := r.FormValue("token")
	if raw == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	// Hash the submitted token and compare with constant-time equality
	// to prevent timing attacks.
	h := sha512.New()
	h.Write([]byte(raw))
	submitted := hex.EncodeToString(h.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(submitted), []byte(d.tokenHash)) != 1 {
		d.bus.Emit(Event{Kind: EventDashboardAuth, Err: fmt.Errorf("invalid token")})
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// Set the verified token hash as the session cookie.
	// httpOnly + SameSiteStrict — dashboard is localhost only but
	// we apply best practices regardless.
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookieName,
		Value:    submitted,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	d.bus.Emit(Event{Kind: EventDashboardAuth})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// requireAuth wraps a handler with cookie-based auth verification.
func (d *Dashboard) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(tokenCookieName)
		if err != nil || cookie.Value == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(d.tokenHash)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// handleRoot serves the dashboard HTML. Unauthenticated users see the
// token entry form. Authenticated users see the live dashboard.
func (d *Dashboard) handleRoot(w http.ResponseWriter, r *http.Request) {
	authed := d.isAuthenticated(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if authed {
		w.Write([]byte(dashboardHTML(d.cfg)))
	} else {
		w.Write([]byte(authHTML()))
	}
}

// handleSSE streams server-sent events to the browser.
// Each authenticated browser tab gets its own channel in the clientRegistry.
func (d *Dashboard) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied

	// Register this client
	id := d.clients.add()
	ch := d.clients.channel(id)
	defer d.clients.remove(id)

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleMetrics returns the current SwarmMetrics snapshot as JSON.
// Called by the dashboard's JS on its own poll cycle as a fallback
// when SSE reconnects.
func (d *Dashboard) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	snap := d.snapshot(0)
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// handleReplay returns the recent event log as JSON for cold-join clients.
func (d *Dashboard) handleReplay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	events := d.eventLog.Snapshot()

	type replayPayload struct {
		Events []Event `json:"events"`
		Count  int     `json:"count"`
	}

	if err := json.NewEncoder(w).Encode(replayPayload{
		Events: events,
		Count:  len(events),
	}); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// ── Event handling ────────────────────────────────────────────────────────────

// handleEvent is the EventBus subscriber. It must not block.
// It fans the event out to all connected SSE clients via non-blocking sends.
func (d *Dashboard) handleEvent(e Event) {
	evt := sseEvent{
		Kind: string(e.Kind),
		Data: map[string]any{
			"lemming_id":  e.LemmingID,
			"terrain":     e.Terrain,
			"pack":        e.Pack,
			"url":         e.URL,
			"status_code": e.StatusCode,
			"occurred_at": e.OccurredAt.UnixMilli(),
		},
	}
	d.clients.broadcast(evt)
}

// broadcastMetrics sends a metrics snapshot to all SSE clients every second.
func (d *Dashboard) broadcastMetrics(ctx context.Context) {
	ticker := time.NewTicker(dashboardSSEInterval)
	defer ticker.Stop()

	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			elapsed := int64(time.Since(start).Seconds())
			snap := d.snapshot(elapsed)
			d.clients.broadcast(sseEvent{
				Kind: "metrics",
				Data: snap,
			})
		}
	}
}

// snapshot builds a metricsSnapshot from the current atomic counters.
func (d *Dashboard) snapshot(elapsedSecs int64) metricsSnapshot {
	return metricsSnapshot{
		Alive:          d.metrics.LemmingsAlive.Load(),
		Completed:      d.metrics.LemmingsCompleted.Load(),
		Failed:         d.metrics.LemmingsFailed.Load(),
		TerrainsOnline: d.metrics.TerrainsOnline.Load(),
		TotalVisits:    d.metrics.TotalVisits.Load(),
		TotalBytes:     formatBytes(d.metrics.TotalBytes.Load()),
		WaitingRoom:    d.metrics.TotalWaitingRoom.Load(),
		Xx2:            d.metrics.Total2xx.Load(),
		Xx3:            d.metrics.Total3xx.Load(),
		Xx4:            d.metrics.Total4xx.Load(),
		Xx5:            d.metrics.Total5xx.Load(),
		OverflowLogs:   d.metrics.OverflowLogs.Load(),
		DroppedLogs:    d.metrics.DroppedLogs.Load(),
		ElapsedSecs:    elapsedSecs,
	}
}

// isAuthenticated checks the session cookie without writing a response.
func (d *Dashboard) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(tokenCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(d.tokenHash)) == 1
}

// ── Client registry ───────────────────────────────────────────────────────────

func (cr *clientRegistry) add() uint64 {
	id := cr.nextID.Add(1)
	ch := make(chan sseEvent, 64) // buffered — slow clients drop events not the swarm
	cr.mu.Lock()
	cr.clients[id] = ch
	cr.mu.Unlock()
	return id
}

func (cr *clientRegistry) channel(id uint64) chan sseEvent {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	return cr.clients[id]
}

func (cr *clientRegistry) remove(id uint64) {
	cr.mu.Lock()
	if ch, ok := cr.clients[id]; ok {
		close(ch)
		delete(cr.clients, id)
	}
	cr.mu.Unlock()
}

// broadcast sends an event to all connected clients via non-blocking sends.
// Slow clients that can't keep up have their events dropped — their channel
// fills up and they see gaps rather than stalling the swarm.
func (cr *clientRegistry) broadcast(evt sseEvent) {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	for _, ch := range cr.clients {
		select {
		case ch <- evt:
		default:
			// client channel full — drop this event for this client only
		}
	}
}

// ── HTML ──────────────────────────────────────────────────────────────────────

func authHTML() string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>lemmings dashboard</title>
<style>
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
:root { --bg:#0f1117; --surface:#1a1d27; --border:#2a2d3a;
  --accent:#6c8ef5; --accent2:#a78bfa; --text:#e2e8f0; --muted:#64748b; }
body { background:var(--bg); color:var(--text);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
  min-height:100vh; display:flex; align-items:center; justify-content:center; }
.card { background:var(--surface); border:1px solid var(--border);
  border-radius:12px; padding:2.5rem 3rem; max-width:400px; width:100%; }
h1 { font-size:1.3rem; margin-bottom:0.4rem;
  background:linear-gradient(135deg,var(--accent),var(--accent2));
  -webkit-background-clip:text; -webkit-text-fill-color:transparent;
  background-clip:text; }
p { color:var(--muted); font-size:0.875rem; margin-bottom:1.5rem; }
input { width:100%; background:var(--bg); border:1px solid var(--border);
  border-radius:8px; padding:0.6rem 0.75rem; color:var(--text);
  font-size:0.95rem; margin-bottom:1rem; outline:none; }
input:focus { border-color:var(--accent); }
button { width:100%; background:linear-gradient(135deg,var(--accent),var(--accent2));
  color:#fff; border:none; border-radius:8px; padding:0.65rem;
  font-size:0.9rem; font-weight:600; cursor:pointer; }
</style>
</head>
<body>
<div class="card">
  <h1>lemmings dashboard</h1>
  <p>Enter the token printed in your terminal to unlock the live view.</p>
  <form method="POST" action="/auth">
    <input type="password" name="token" placeholder="paste token here" autofocus>
    <button type="submit">unlock →</button>
  </form>
</div>
</body>
</html>`
}

func dashboardHTML(cfg SwarmConfig) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>lemmings — %s</title>
<style>
*, *::before, *::after { box-sizing:border-box; margin:0; padding:0; }
:root { --bg:#0f1117; --surface:#1a1d27; --border:#2a2d3a;
  --accent:#6c8ef5; --accent2:#a78bfa; --text:#e2e8f0; --muted:#64748b;
  --success:#34d399; --warn:#f59e0b; --danger:#ef4444; }
body { background:var(--bg); color:var(--text);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
  padding:1.5rem; }
header { display:flex; align-items:baseline; gap:1rem; margin-bottom:1.5rem;
  border-bottom:1px solid var(--border); padding-bottom:1rem; }
h1 { font-size:1.2rem;
  background:linear-gradient(135deg,var(--accent),var(--accent2));
  -webkit-background-clip:text; -webkit-text-fill-color:transparent;
  background-clip:text; }
.target { color:var(--muted); font-size:0.85rem; }
.elapsed { margin-left:auto; color:var(--muted); font-size:0.85rem; }
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(160px,1fr));
  gap:1rem; margin-bottom:1.5rem; }
.stat { background:var(--surface); border:1px solid var(--border);
  border-radius:8px; padding:1rem 1.25rem; }
.stat-label { font-size:0.72rem; text-transform:uppercase;
  letter-spacing:0.07em; color:var(--muted); margin-bottom:0.3rem; }
.stat-value { font-size:1.6rem; font-weight:700; line-height:1; }
.stat-value.accent { color:var(--accent); }
.stat-value.success { color:var(--success); }
.stat-value.warn { color:var(--warn); }
.stat-value.danger { color:var(--danger); }
.events { background:var(--surface); border:1px solid var(--border);
  border-radius:8px; padding:1rem; max-height:320px; overflow-y:auto; }
.events h2 { font-size:0.8rem; text-transform:uppercase;
  letter-spacing:0.07em; color:var(--muted); margin-bottom:0.75rem; }
.event { font-size:0.78rem; color:var(--muted); padding:0.2rem 0;
  border-bottom:1px solid var(--border); font-family:monospace; }
.event:last-child { border-bottom:none; }
.dot { display:inline-block; width:6px; height:6px; border-radius:50%%;
  margin-right:6px; vertical-align:middle; }
.dot-born { background:var(--success); }
.dot-died { background:var(--muted); }
.dot-wr   { background:var(--accent); }
.dot-err  { background:var(--danger); }
.warn-bar { background:rgba(239,68,68,0.1); border:1px solid var(--danger);
  border-radius:6px; padding:0.5rem 0.75rem; margin-bottom:1rem;
  font-size:0.82rem; color:var(--danger); display:none; }
</style>
</head>
<body>
<header>
  <h1>lemmings</h1>
  <span class="target">%s</span>
  <span class="elapsed" id="elapsed">0s</span>
</header>

<div class="warn-bar" id="warn-bar">
  ⚠ dropped logs detected — increase -limit for accurate results
</div>

<div class="grid">
  <div class="stat">
    <div class="stat-label">alive</div>
    <div class="stat-value accent" id="alive">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">completed</div>
    <div class="stat-value success" id="completed">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">failed</div>
    <div class="stat-value warn" id="failed">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">terrains online</div>
    <div class="stat-value accent" id="terrains">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">total visits</div>
    <div class="stat-value" id="visits">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">total bytes</div>
    <div class="stat-value" id="bytes">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">waiting room</div>
    <div class="stat-value accent" id="wr">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">2xx</div>
    <div class="stat-value success" id="XX2">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">3xx</div>
    <div class="stat-value warn" id="XX3">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">4xx</div>
    <div class="stat-value danger" id="XX4">—</div>
  </div>
  <div class="stat">
    <div class="stat-label">5xx</div>
    <div class="stat-value danger" id="XX5">—</div>
  </div>
</div>

<div class="events">
  <h2>recent events</h2>
  <div id="event-list"></div>
</div>

<script>
const MAX_EVENTS = 100;

function fmt(n) {
  return Number(n).toLocaleString();
}

function elapsed(secs) {
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs %% 3600) / 60);
  const s = secs %% 60;
  if (h > 0) return h + 'h ' + m + 'm ' + s + 's';
  if (m > 0) return m + 'm ' + s + 's';
  return s + 's';
}

function applyMetrics(d) {
  document.getElementById('alive').textContent       = fmt(d.alive);
  document.getElementById('completed').textContent   = fmt(d.completed);
  document.getElementById('failed').textContent      = fmt(d.failed);
  document.getElementById('terrains').textContent    = fmt(d.terrains_online);
  document.getElementById('visits').textContent      = fmt(d.total_visits);
  document.getElementById('bytes').textContent       = d.total_bytes;
  document.getElementById('wr').textContent          = fmt(d.waiting_room);
  document.getElementById('XX2').textContent         = fmt(d.XX2);
  document.getElementById('XX3').textContent         = fmt(d.XX3);
  document.getElementById('XX4').textContent         = fmt(d.XX4);
  document.getElementById('XX5').textContent         = fmt(d.XX5);
  document.getElementById('elapsed').textContent     = elapsed(d.elapsed_secs);
  if (d.dropped_logs > 0) {
    document.getElementById('warn-bar').style.display = 'block';
  }
}

function addEvent(kind, data) {
  const list = document.getElementById('event-list');
  const dot = dotClass(kind);
  const label = eventLabel(kind, data);
  const div = document.createElement('div');
  div.className = 'event';
  div.innerHTML = '<span class="dot ' + dot + '"></span>' + label;
  list.insertBefore(div, list.firstChild);
  // cap the list
  while (list.children.length > MAX_EVENTS) {
    list.removeChild(list.lastChild);
  }
}

function dotClass(kind) {
  if (kind === 'lemming.born')    return 'dot-born';
  if (kind === 'lemming.died')    return 'dot-died';
  if (kind === 'waiting_room.entered') return 'dot-wr';
  if (kind === 'lemming.failed')  return 'dot-err';
  return '';
}

function eventLabel(kind, data) {
  const t = data.terrain !== undefined ? ' t:' + data.terrain : '';
  const u = data.url ? ' → ' + data.url : '';
  const sc = data.status_code ? ' [' + data.status_code + ']' : '';
  return kind + t + u + sc;
}

// Fetch the replay log on first load so cold-join sessions aren't empty
fetch('/replay')
  .then(r => r.json())
  .then(payload => {
    const events = payload.events || [];
    // replay oldest first so newest ends up at top after addEvent reversal
    for (let i = events.length - 1; i >= 0; i--) {
      addEvent(events[i].Kind, events[i]);
    }
  });

// Open SSE stream
const es = new EventSource('/events');
es.onmessage = function(e) {
  const msg = JSON.parse(e.data);
  if (msg.kind === 'metrics') {
    applyMetrics(msg.data);
  } else {
    addEvent(msg.kind, msg.data || {});
  }
};
es.onerror = function() {
  // SSE reconnects automatically — fall back to polling metrics
  setTimeout(function() {
    fetch('/metrics')
      .then(r => r.json())
      .then(applyMetrics);
  }, 2000);
};
</script>
</body>
</html>`, cfg.Hit, cfg.Hit)
}
