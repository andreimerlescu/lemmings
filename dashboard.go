package main

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// dashboardSSEInterval is how often the browser receives metrics.
	dashboardSSEInterval = 1 * time.Second

	// dashboardFrameInterval is how often the browser receives stage
	// frames. The browser animates at its own refresh rate in between.
	dashboardFrameInterval = 200 * time.Millisecond

	// dashboardStageEvery is how many frames pass between full stage
	// snapshots, which heal any browser that dropped frames.
	dashboardStageEvery = 25

	// eventLogCapacity is how many events the dashboard replays to a
	// newly authenticated session.
	eventLogCapacity = 1000

	// tokenCookieName is the cookie set after successful auth.
	tokenCookieName = "lemmings_token"

	// liveTimelinePoints and livePaths size the dashboard's charts and
	// path table.
	liveTimelinePoints = 180
	livePaths          = 10
)

//go:embed web/index.html web/login.html web/app.css web/app.js web/theme.js
var webAssets embed.FS

// liveSource provides the aggregate statistics the dashboard charts.
type liveSource interface {
	Live(points, paths int) LiveStats
}

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
	obs       *Observatory
	stats     liveSource // nil until the swarm wires its reporter
	startedAt atomic.Int64
	lastFeed  atomic.Uint64
	closing   chan struct{} // closed when the server shuts down
	closeOnce sync.Once
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

// metricsSnapshot is the counter payload sent to the browser every second.
type metricsSnapshot struct {
	Alive           int64  `json:"alive"`
	Completed       int64  `json:"completed"`
	Failed          int64  `json:"failed"`
	FailedVisits    int64  `json:"failed_visits"`
	CancelledVisits int64  `json:"cancelled_visits"`
	TerrainsOnline  int64  `json:"terrains_online"`
	TotalVisits     int64  `json:"total_visits"`
	TotalBytes      string `json:"total_bytes"`
	WaitingRoom     int64  `json:"waiting_room"`
	Xx2             int64  `json:"XX2"`
	Xx3             int64  `json:"XX3"`
	Xx4             int64  `json:"XX4"`
	Xx5             int64  `json:"XX5"`
	OverflowLogs    int64  `json:"overflow_logs"`
	DroppedLogs     int64  `json:"dropped_logs"`
	ElapsedSecs     int64  `json:"elapsed_secs"`
}

// dashboardStats is the once-a-second SSE "metrics" payload.
type dashboardStats struct {
	Metrics  metricsSnapshot `json:"m"`
	Live     *LiveStats      `json:"live,omitempty"`
	Terrains []terrainTile   `json:"terrains,omitempty"`
	Phase    string          `json:"phase"`
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

	d := &Dashboard{
		cfg:       cfg,
		bus:       bus,
		metrics:   metrics,
		tokenHash: hex.EncodeToString(h.Sum(nil)),
		eventLog:  NewEventLog(eventLogCapacity),
		clients: clientRegistry{
			clients: make(map[uint64]chan sseEvent),
		},
		obs:     NewObservatory(cfg.Terrain),
		closing: make(chan struct{}),
	}
	d.startedAt.Store(time.Now().UnixNano())

	// The observatory sees every event; the replay log keeps the
	// low-volume ones. Successful visits are far too many to replay.
	d.unsub = bus.Subscribe(Tee(
		d.handleEvent,
		Filter(d.eventLog.AsSubscriber(),
			EventLemmingBorn, EventLemmingDied, EventLemmingFailed, EventVisitError,
			EventWaitingRoomQueued, EventWaitingRoom, EventTerrainOnline, EventTerrainDone,
			EventLogOverflow, EventLogDropped, EventSwarmStarted, EventSwarmDone),
	))

	return d
}

// markStarted sets the moment elapsed time is measured from.
func (d *Dashboard) markStarted(at time.Time) {
	d.startedAt.Store(at.UnixNano())
	d.obs.markStarted(at)
}

// elapsed returns time since the swarm started.
func (d *Dashboard) elapsed() time.Duration {
	return time.Since(time.Unix(0, d.startedAt.Load()))
}

// Serve starts the HTTP server and blocks until ctx is cancelled.
func (d *Dashboard) Serve(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", d.handleRoot)
	mux.HandleFunc("/auth", d.handleAuth)
	mux.HandleFunc("/events", d.requireAuth(d.handleSSE))
	mux.HandleFunc("/metrics", d.requireAuth(d.handleMetrics))
	mux.HandleFunc("/replay", d.requireAuth(d.handleReplay))
	mux.HandleFunc("/api/state", d.requireAuth(d.handleState))
	mux.HandleFunc("/api/lemming", d.requireAuth(d.handleLemming))

	addr := fmt.Sprintf("localhost:%d", d.cfg.DashboardPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           localOnly(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      0, // SSE connections are long-lived
		IdleTimeout:       120 * time.Second,
	}

	go d.broadcastMetrics(ctx)
	go d.broadcastFrames(ctx)

	// Shutdown when context cancels. Event streams are ended first so
	// browsers see a finished response rather than a dropped connection.
	go func() {
		<-ctx.Done()
		d.closeOnce.Do(func() { close(d.closing) })
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

// localOnly rejects requests whose Host header is not a loopback name.
// The server only listens on localhost; this also defeats DNS-rebinding
// pages that point a public hostname at 127.0.0.1.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch strings.Trim(host, "[]") {
		case "localhost", "127.0.0.1", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
		}
	})
}

// finish broadcasts the final state so open dashboards can show the end
// of the run before the server shuts down.
func (d *Dashboard) finish() {
	d.obs.setPhase("done")
	d.sendFrame(true)
	d.clients.broadcast(sseEvent{Kind: "metrics", Data: d.stats1s()})
	if d.clients.count() > 0 {
		time.Sleep(300 * time.Millisecond) // let SSE handlers write it out
	}
}

// ── Auth ─────────────────────────────────────────────────────────────────────

// handleAuth accepts a POST with a token field, verifies it via constant-time
// SHA512 comparison, and sets a session cookie on success.
func (d *Dashboard) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	raw := strings.TrimSpace(r.FormValue("token"))
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
		if !d.isAuthenticated(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
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

// ── Handlers ─────────────────────────────────────────────────────────────────

// setPageHeaders applies the security headers shared by both pages.
func setPageHeaders(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
}

// handleRoot serves the dashboard HTML. Unauthenticated users see the
// token entry form. Authenticated users see the live dashboard.
func (d *Dashboard) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if d.isAuthenticated(r) {
		setPageHeaders(w, pages.dashboardCSP)
		_, _ = w.Write([]byte(dashboardHTML(d.cfg)))
		return
	}
	setPageHeaders(w, pages.loginCSP)
	_, _ = w.Write([]byte(authHTML()))
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

	flusher.Flush() // send headers now so the browser's EventSource opens

	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.closing:
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
	writeJSON(w, d.snapshot(int64(d.elapsed().Seconds())))
}

// handleReplay returns the recent event log as JSON for cold-join clients.
func (d *Dashboard) handleReplay(w http.ResponseWriter, r *http.Request) {
	events := d.eventLog.Snapshot()
	writeJSON(w, struct {
		Events []Event `json:"events"`
		Count  int     `json:"count"`
	}{events, len(events)})
}

// handleState returns everything a newly opened dashboard needs to draw
// the current moment before the first frame arrives.
func (d *Dashboard) handleState(w http.ResponseWriter, r *http.Request) {
	t, phase, stage, feed, _ := d.obs.snapshot()
	writeJSON(w, struct {
		T     float64        `json:"t"`
		Phase string         `json:"phase"`
		Stage []stageSlot    `json:"stage"`
		Feed  []feedItem     `json:"feed"`
		Stats dashboardStats `json:"stats"`
	}{t, phase, stage, feed, d.stats1s()})
}

// handleLemming returns one lemming's life for the inspector.
func (d *Dashboard) handleLemming(w http.ResponseWriter, r *http.Request) {
	life, ok := d.obs.life(r.URL.Query().Get("id"))
	if !ok {
		http.Error(w, "lemming not tracked", http.StatusNotFound)
		return
	}
	writeJSON(w, life)
}

// writeJSON encodes v as an uncached JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// ── Event handling ────────────────────────────────────────────────────────────

// handleEvent is the EventBus subscriber. It must not block: it updates
// the observatory, which frames are later built from.
func (d *Dashboard) handleEvent(e Event) {
	switch e.Kind {
	case EventTerrainOnline:
		d.obs.setPhase("ramping")
	case EventTerrainDone:
		if d.metrics.TerrainsOnline.Load() <= 0 {
			d.obs.setPhase("draining")
		}
	}
	d.obs.handle(e)
}

// broadcastFrames sends stage frames to every browser several times a
// second, with a full stage snapshot every few seconds.
func (d *Dashboard) broadcastFrames(ctx context.Context) {
	ticker := time.NewTicker(dashboardFrameInterval)
	defer ticker.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sendFrame(n%dashboardStageEvery == 0)
		}
	}
}

// sendFrame builds and broadcasts one frame when there is anything to say.
func (d *Dashboard) sendFrame(withStage bool) {
	f, seq := d.obs.frame(d.lastFeed.Load(), withStage)
	d.lastFeed.Store(seq)
	if len(f.Events) == 0 && len(f.Feed) == 0 && len(f.Stage) == 0 && f.Skipped == 0 && !withStage {
		return
	}
	d.clients.broadcast(sseEvent{Kind: "frame", Data: f})
}

// broadcastMetrics sends a metrics snapshot to all SSE clients every second.
func (d *Dashboard) broadcastMetrics(ctx context.Context) {
	ticker := time.NewTicker(dashboardSSEInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if d.clients.count() == 0 {
				continue
			}
			d.clients.broadcast(sseEvent{Kind: "metrics", Data: d.stats1s()})
		}
	}
}

// stats1s builds the once-a-second payload.
func (d *Dashboard) stats1s() dashboardStats {
	s := dashboardStats{
		Metrics:  d.snapshot(int64(d.elapsed().Seconds())),
		Terrains: d.obs.terrainTiles(),
	}
	d.obs.mu.Lock()
	s.Phase = d.obs.phase
	d.obs.mu.Unlock()
	if s.Phase == "ramping" && d.metrics.TerrainsOnline.Load() >= d.cfg.Terrain {
		s.Phase = "running"
	}
	if d.stats != nil {
		live := d.stats.Live(liveTimelinePoints, livePaths)
		s.Live = &live
	}
	return s
}

// snapshot builds a metricsSnapshot from the current atomic counters.
func (d *Dashboard) snapshot(elapsedSecs int64) metricsSnapshot {
	return metricsSnapshot{
		Alive:           d.metrics.LemmingsAlive.Load(),
		Completed:       d.metrics.LemmingsCompleted.Load(),
		Failed:          d.metrics.LemmingsFailed.Load(),
		FailedVisits:    d.metrics.FailedVisits.Load(),
		CancelledVisits: d.metrics.CancelledVisits.Load(),
		TerrainsOnline:  d.metrics.TerrainsOnline.Load(),
		TotalVisits:     d.metrics.TotalVisits.Load(),
		TotalBytes:      formatBytes(d.metrics.TotalBytes.Load()),
		WaitingRoom:     d.metrics.TotalWaitingRoom.Load(),
		Xx2:             d.metrics.Total2xx.Load(),
		Xx3:             d.metrics.Total3xx.Load(),
		Xx4:             d.metrics.Total4xx.Load(),
		Xx5:             d.metrics.Total5xx.Load(),
		OverflowLogs:    d.metrics.OverflowLogs.Load(),
		DroppedLogs:     d.metrics.DroppedLogs.Load(),
		ElapsedSecs:     elapsedSecs,
	}
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

// count returns how many browsers are connected.
func (cr *clientRegistry) count() int {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	return len(cr.clients)
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

// pages holds both HTML pages with their assets inlined, and the
// Content-Security-Policy that allows exactly those inline assets.
var pages = buildPages()

type builtPages struct {
	dashboard, login       string
	dashboardCSP, loginCSP string
}

// buildPages inlines the embedded CSS and JS into both pages and hashes
// every inline script and style for the CSP. Nothing is loaded from
// anywhere else, so the dashboard works offline and allows nothing more.
func buildPages() builtPages {
	read := func(name string) string {
		b, err := webAssets.ReadFile("web/" + name)
		if err != nil {
			panic("lemmings: missing embedded asset " + name + ": " + err.Error())
		}
		return string(b)
	}
	css, js, theme := read("app.css"), read("app.js"), read("theme.js")
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	csp := func(scripts ...string) string {
		var hs []string
		for _, s := range scripts {
			hs = append(hs, hash(s))
		}
		return "default-src 'none'; connect-src 'self'; img-src 'self' data:; form-action 'self'; " +
			"base-uri 'none'; frame-ancestors 'none'; style-src " + hash(css) + "; script-src " + strings.Join(hs, " ")
	}
	inline := func(page string) string {
		return strings.NewReplacer(
			"<!--STYLE-->", "<style>"+css+"</style>",
			"<!--THEME-->", "<script>"+theme+"</script>",
			"<!--SCRIPT-->", "<script>"+js+"</script>",
		).Replace(page)
	}
	loginJS := read("login.html")
	// The login page's own script is the only inline script besides theme.
	loginScript := between(loginJS, "<script>", "</script>")
	return builtPages{
		dashboard:    inline(read("index.html")),
		login:        inline(loginJS),
		dashboardCSP: csp(theme, js),
		loginCSP:     csp(theme, loginScript),
	}
}

// between returns the text between the first start marker and the next
// end marker, or "".
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}

// authHTML returns the login page.
func authHTML() string {
	return pages.login
}

// dashboardConfig is the run configuration embedded in the dashboard page.
type dashboardConfig struct {
	Hit        string  `json:"hit"`
	Version    string  `json:"version"`
	Terrain    int64   `json:"terrain"`
	Pack       int64   `json:"pack"`
	Total      int64   `json:"total"`
	Limit      int     `json:"limit"`
	UntilSecs  float64 `json:"until"`
	RampSecs   float64 `json:"ramp"`
	EtaSecs    float64 `json:"eta"`
	ThinkMin   float64 `json:"think_min"`
	ThinkMax   float64 `json:"think_max"`
	Navigation string  `json:"navigation"`
	Journey    string  `json:"journey,omitempty"`
	Stage      int     `json:"stage"`
	FailGate   float64 `json:"fail_gate"` // -1 when off
	P95Gate    float64 `json:"p95_gate"`  // seconds, 0 when off
}

// dashboardHTML returns the dashboard page for cfg. The configuration is
// embedded as a JSON data block, which json.Marshal escapes for HTML.
func dashboardHTML(cfg SwarmConfig) string {
	hit := safeURL(cfg.Hit)
	c := dashboardConfig{
		Hit: hit, Version: cfg.Version, Terrain: cfg.Terrain, Pack: cfg.Pack,
		Total: cfg.Terrain * cfg.Pack, Limit: cfg.Limit,
		UntilSecs: cfg.Until.Seconds(), RampSecs: cfg.Ramp.Seconds(),
		EtaSecs:  estimateWallClock(cfg).Seconds(),
		ThinkMin: cfg.ThinkMin.Seconds(), ThinkMax: cfg.ThinkMax.Seconds(),
		Navigation: cfg.Navigation, Stage: stageSize, FailGate: -1,
		P95Gate: cfg.P95Budget.Seconds(),
	}
	if cfg.Journey != nil {
		c.Journey = cfg.Journey.Name
	}
	if cfg.FailureGate {
		c.FailGate = cfg.MaxFailureRate
	}
	js, _ := json.Marshal(c)
	return strings.NewReplacer(
		"{{HIT}}", html.EscapeString(hit),
		"{{VERSION}}", html.EscapeString(cfg.Version),
		"{{CONFIG}}", string(js),
	).Replace(pages.dashboard)
}
