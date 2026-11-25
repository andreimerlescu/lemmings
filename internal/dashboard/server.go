package dashboard

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

const (
	dashboardSSEInterval   = 1 * time.Second
	dashboardFrameInterval = 200 * time.Millisecond
	dashboardStageEvery    = 25
	eventLogCapacity       = 1000
	liveTimelinePoints     = 180
	livePaths              = 10
)

//go:embed web/index.html web/login.html web/app.css web/app.js web/theme.js
var webAssets embed.FS

// LiveSource supplies once-a-second aggregate statistics for dashboard charts.
type LiveSource interface {
	Live(points, paths int) report.LiveStats
}

// Dashboard serves the live Synthwave '84 portal on loopback only.
// All background goroutines are tracked and joined cleanly on Shutdown (R2/SEC-08).
type Dashboard struct {
	cfg        swarm.Config
	bus        *events.Bus
	metrics    *swarm.Metrics
	tokenHash  string
	sessionKey []byte
	authLimit  authLimiter
	eventLog   *events.Log
	clients    clientRegistry
	unsub      func()
	obs        *Observatory
	stats      LiveSource
	forensics  *forensics.Store
	startedAt  atomic.Int64
	lastFeed   atomic.Uint64

	closing      chan struct{}
	closeOnce    sync.Once
	finalizeCh   chan struct{}
	finalizeOnce sync.Once

	mu         sync.Mutex
	srv        *http.Server
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
}

type clientRegistry struct {
	mu      sync.RWMutex
	clients map[uint64]chan sseEvent
	nextID  atomic.Uint64
}

type sseEvent struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

type metricsSnapshot struct {
	Alive             int64  `json:"alive"`
	LemmingsStarted   int64  `json:"lemmings_started"`
	Completed         int64  `json:"completed"`
	Failed            int64  `json:"failed"`
	FailedVisits      int64  `json:"failed_visits"`
	CancelledVisits   int64  `json:"cancelled_visits"`
	TerrainsOnline    int64  `json:"terrains_online"`
	TotalVisits       int64  `json:"total_visits"`
	TotalBytes        string `json:"total_bytes"`
	WaitingRoom       int64  `json:"waiting_room"`
	QueuePolls        int64  `json:"queue_polls"`
	AssetRequests     int64  `json:"asset_requests"`
	AssetFailed       int64  `json:"asset_failed"`
	AssetDenied403    int64  `json:"asset_denied_403"`
	AssetBytes        int64  `json:"asset_bytes"`
	Bans429           int64  `json:"bans_429"`
	Bans403           int64  `json:"bans_403"`
	Xx2               int64  `json:"XX2"`
	Xx3               int64  `json:"XX3"`
	Xx4               int64  `json:"XX4"`
	Xx5               int64  `json:"XX5"`
	OverflowLogs      int64  `json:"overflow_logs"`
	DroppedLogs       int64  `json:"dropped_logs"`
	ArtifactsCaptured int64  `json:"artifacts_captured"`
	ScoutsAlive       int64  `json:"scouts_alive"`
	ScoutVisits       int64  `json:"scout_visits"`
	ScoutFailures     int64  `json:"scout_failures"`
	ElapsedSecs       int64  `json:"elapsed_secs"`
}

type dashboardStats struct {
	Metrics   metricsSnapshot   `json:"m"`
	Live      *report.LiveStats `json:"live,omitempty"`
	Terrains  []terrainTile     `json:"terrains,omitempty"`
	Forensics *forensics.Stats  `json:"forensics,omitempty"`
	Phase     string            `json:"phase"`
}

// New constructs a Dashboard. Only the SHA-512 hash of rawToken is retained;
// session cookies are signed with a freshly generated ephemeral HMAC-SHA256 key.
func New(
	cfg swarm.Config,
	bus *events.Bus,
	metrics *swarm.Metrics,
	rawToken string,
	stats LiveSource,
	store *forensics.Store,
) (*Dashboard, error) {
	key, err := secrets.NewEphemeralKey()
	if err != nil {
		return nil, err
	}
	d := &Dashboard{
		cfg:        cfg,
		bus:        bus,
		metrics:    metrics,
		tokenHash:  sha512Hex(rawToken),
		sessionKey: key,
		eventLog:   events.NewLog(eventLogCapacity),
		clients: clientRegistry{
			clients: make(map[uint64]chan sseEvent),
		},
		obs:        NewObservatory(cfg.Terrain),
		stats:      stats,
		forensics:  store,
		closing:    make(chan struct{}),
		finalizeCh: make(chan struct{}),
	}
	d.startedAt.Store(time.Now().UnixNano())

	d.unsub = bus.Subscribe(events.Tee(
		d.handleEvent,
		events.Filter(d.eventLog.AsSubscriber(),
			events.LemmingBorn, events.LemmingDied, events.LemmingFailed, events.VisitError,
			events.WaitingRoomQueued, events.WaitingRoom, events.TerrainOnline, events.TerrainDone,
			events.LogOverflow, events.LogDropped, events.SwarmStarted, events.SwarmDone,
			events.ArtifactCaptured, events.ArtifactReviewed),
	))
	return d, nil
}

// TokenHash returns the SHA-512 hex digest of the boot token (never raw).
func (d *Dashboard) TokenHash() string { return d.tokenHash }

// MarkStarted sets the start timestamp for elapsed time calculations.
func (d *Dashboard) MarkStarted(at time.Time) {
	d.startedAt.Store(at.UnixNano())
	d.obs.markStarted(at)
}

func (d *Dashboard) elapsed() time.Duration {
	return time.Since(time.Unix(0, d.startedAt.Load()))
}

// Handler returns the fully wired HTTP handler (useful for unit tests).
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	csrf := http.NewCrossOriginProtection()

	mux.HandleFunc("/", d.handleRoot)
	mux.HandleFunc("/auth", d.handleAuth)
	mux.HandleFunc("/events", d.requireAuth(d.handleSSE))
	mux.HandleFunc("/metrics", d.requireAuth(d.handleMetrics))
	mux.HandleFunc("/replay", d.requireAuth(d.handleReplay))
	mux.HandleFunc("/api/state", d.requireAuth(d.handleState))
	mux.HandleFunc("/api/lemming", d.requireAuth(d.handleLemming))
	mux.HandleFunc("GET /api/artifacts", d.requireAuth(d.handleArtifactsList))
	mux.HandleFunc("GET /api/artifacts/{id}", d.requireAuth(d.handleArtifactDetail))
	mux.HandleFunc("GET /api/artifacts/{id}/image", d.requireAuth(d.handleArtifactImage))
	mux.Handle("POST /api/artifacts/{id}/review", csrf.Handler(http.HandlerFunc(d.requireAuth(d.handleArtifactReview))))
	mux.Handle("POST /api/finalize", csrf.Handler(http.HandlerFunc(d.requireAuth(d.handleFinalize))))

	return LocalOnly(mux)
}

// Start binds localhost:<DashboardPort> synchronously and launches the server
// and SSE broadcast loops. Call Shutdown to stop and join every goroutine.
func (d *Dashboard) Start(ctx context.Context) (string, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(d.cfg.DashboardPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dashboard listen %s: %w", addr, err)
	}

	loopCtx, loopCancel := context.WithCancel(ctx)
	srv := &http.Server{
		Handler:           d.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      0, // SSE connections are long-lived
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	d.mu.Lock()
	d.srv = srv
	d.loopCancel = loopCancel
	d.mu.Unlock()

	d.wg.Add(3)
	go func() {
		defer d.wg.Done()
		d.broadcastMetrics(loopCtx)
	}()
	go func() {
		defer d.wg.Done()
		d.broadcastFrames(loopCtx)
	}()
	go func() {
		defer d.wg.Done()
		_ = srv.Serve(ln)
	}()

	return ln.Addr().String(), nil
}

// EnterReviewMode switches the portal to "reviewing" phase so the admin can
// inspect and triage captured error screenshots until FinalizeCh is closed.
func (d *Dashboard) EnterReviewMode() <-chan struct{} {
	d.obs.SetPhase("reviewing")
	d.sendFrame(true)
	d.clients.broadcast(sseEvent{Kind: "metrics", Data: d.stats1s()})
	return d.finalizeCh
}

// Finish broadcasts the final completed state to all connected browsers.
func (d *Dashboard) Finish() {
	d.obs.SetPhase("done")
	d.sendFrame(true)
	d.clients.broadcast(sseEvent{Kind: "metrics", Data: d.stats1s()})
}

// Shutdown stops broadcasters, ends active SSE streams cleanly, shuts down the
// HTTP server, unsubscribes from EventBus, and joins all owned goroutines.
func (d *Dashboard) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	srv := d.srv
	cancel := d.loopCancel
	d.srv = nil
	d.loopCancel = nil
	d.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	d.closeOnce.Do(func() { close(d.closing) })

	var err error
	if srv != nil {
		if shutErr := srv.Shutdown(ctx); shutErr != nil {
			err = errors.Join(shutErr, srv.Close())
		}
	}
	if d.unsub != nil {
		d.unsub()
	}
	d.wg.Wait()
	return err
}

// ── Handlers ─────────────────────────────────────────────────────────────────

func setPageHeaders(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
}

func (d *Dashboard) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if d.isAuthenticated(r) {
		setPageHeaders(w, pages.dashboardCSP)
		_, _ = w.Write([]byte(DashboardHTML(d.cfg)))
		return
	}
	setPageHeaders(w, pages.loginCSP)
	_, _ = w.Write([]byte(AuthHTML()))
}

func (d *Dashboard) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	id := d.clients.add()
	ch := d.clients.channel(id)
	defer d.clients.remove(id)

	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.closing:
			// Drain any already-queued final frame/metrics before closing stream.
			for {
				select {
				case evt, ok := <-ch:
					if !ok {
						return
					}
					if data, err := json.Marshal(evt); err == nil {
						fmt.Fprintf(w, "data: %s\n\n", data)
						flusher.Flush()
					}
				default:
					return
				}
			}
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

func (d *Dashboard) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var live *report.LiveStats
	if d.stats != nil {
		l := d.stats.Live(0, 0)
		live = &l
	}
	writeJSON(w, d.snapshot(int64(d.elapsed().Seconds()), live))
}

func (d *Dashboard) handleReplay(w http.ResponseWriter, _ *http.Request) {
	evts := d.eventLog.Snapshot()
	writeJSON(w, struct {
		Events []events.Event `json:"events"`
		Count  int            `json:"count"`
	}{evts, len(evts)})
}

func (d *Dashboard) handleState(w http.ResponseWriter, _ *http.Request) {
	t, phase, stage, feed, _ := d.obs.snapshot()
	var summaries []forensics.Summary
	var fStats forensics.Stats
	if d.forensics != nil {
		summaries, fStats = d.forensics.List(forensics.FilterOptions{})
	}
	writeJSON(w, struct {
		T          float64             `json:"t"`
		Phase      string              `json:"phase"`
		Stage      []stageSlot         `json:"stage"`
		Feed       []feedItem          `json:"feed"`
		Stats      dashboardStats      `json:"stats"`
		Artifacts  []forensics.Summary `json:"artifacts,omitempty"`
		Forensics  forensics.Stats     `json:"forensics"`
		ReviewMode bool                `json:"review_mode"`
	}{t, phase, stage, feed, d.stats1s(), summaries, fStats, d.cfg.ReviewMode})
}

func (d *Dashboard) handleLemming(w http.ResponseWriter, r *http.Request) {
	life, ok := d.obs.Life(r.URL.Query().Get("id"))
	if !ok {
		http.Error(w, "lemming not tracked", http.StatusNotFound)
		return
	}
	if d.forensics != nil {
		for i := range life.History {
			if life.History[i].ArtifactID == "" && life.History[i].Failed {
				if aid, found := d.forensics.FindByVisit(life.ID, life.History[i].Seq); found {
					life.History[i].ArtifactID = aid
				}
			}
		}
	}
	writeJSON(w, life)
}

func (d *Dashboard) handleArtifactsList(w http.ResponseWriter, r *http.Request) {
	if d.forensics == nil {
		writeJSON(w, struct {
			Artifacts []forensics.Summary `json:"artifacts"`
			Stats     forensics.Stats     `json:"stats"`
		}{[]forensics.Summary{}, forensics.Stats{}})
		return
	}
	q := r.URL.Query()
	items, stats := d.forensics.List(forensics.FilterOptions{
		Status: q.Get("status"),
		Kind:   q.Get("kind"),
		Query:  q.Get("q"),
	})
	writeJSON(w, struct {
		Artifacts []forensics.Summary `json:"artifacts"`
		Stats     forensics.Stats     `json:"stats"`
	}{items, stats})
}

func (d *Dashboard) handleArtifactDetail(w http.ResponseWriter, r *http.Request) {
	if d.forensics == nil {
		http.Error(w, "artifact store disabled", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	view, ok := d.forensics.GetForPortal(id, d.cfg.RevealSessionCookies)
	if !ok {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	writeJSON(w, view)
}

func (d *Dashboard) handleArtifactImage(w http.ResponseWriter, r *http.Request) {
	if d.forensics == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	data, mimeType, ok := d.forensics.Image(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(data)
}

func (d *Dashboard) handleArtifactReview(w http.ResponseWriter, r *http.Request) {
	if d.forensics == nil {
		http.Error(w, "artifact store disabled", http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid review payload", http.StatusBadRequest)
		return
	}

	upd, err := d.forensics.Review(id, forensics.ReviewStatus(strings.ToLower(strings.TrimSpace(body.Status))), body.Note)
	if err != nil {
		if errors.Is(err, forensics.ErrNotFound) {
			http.Error(w, "artifact not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.bus.Emit(events.Event{
		Kind:         events.ArtifactReviewed,
		ArtifactID:   upd.ID,
		ReviewStatus: string(upd.ReviewStatus),
		Payload:      upd,
	})
	writeJSON(w, upd)
}

func (d *Dashboard) handleFinalize(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	d.finalizeOnce.Do(func() { close(d.finalizeCh) })
	writeJSON(w, map[string]any{"finalizing": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// ── Event Handling & SSE Broadcasting ────────────────────────────────────────

func (d *Dashboard) handleEvent(e events.Event) {
	switch e.Kind {
	case events.TerrainOnline:
		d.obs.SetPhase("ramping")
	case events.TerrainDone:
		if d.metrics.TerrainsOnline.Load() <= 0 {
			d.obs.SetPhase("draining")
		}
	case events.ArtifactCaptured:
		if sum, ok := e.Payload.(forensics.Summary); ok {
			var st forensics.Stats
			if d.forensics != nil {
				st = d.forensics.Stats()
			}
			d.clients.broadcast(sseEvent{
				Kind: "artifact",
				Data: struct {
					Summary forensics.Summary `json:"summary"`
					Stats   forensics.Stats   `json:"stats"`
				}{sum, st},
			})
		}
	case events.ArtifactReviewed:
		if upd, ok := e.Payload.(forensics.ReviewUpdate); ok {
			d.clients.broadcast(sseEvent{Kind: "artifact_review", Data: upd})
		}
	}
	d.obs.Handle(e)
}

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

func (d *Dashboard) sendFrame(withStage bool) {
	f, seq := d.obs.frame(d.lastFeed.Load(), withStage)
	d.lastFeed.Store(seq)
	if len(f.Events) == 0 && len(f.Feed) == 0 && len(f.Stage) == 0 && f.Skipped == 0 && !withStage {
		return
	}
	d.clients.broadcast(sseEvent{Kind: "frame", Data: f})
}

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

func (d *Dashboard) stats1s() dashboardStats {
	var livePtr *report.LiveStats
	if d.stats != nil {
		live := d.stats.Live(liveTimelinePoints, livePaths)
		livePtr = &live
	}
	s := dashboardStats{
		Metrics:  d.snapshot(int64(d.elapsed().Seconds()), livePtr),
		Live:     livePtr,
		Terrains: d.obs.terrainTiles(),
	}
	d.obs.mu.Lock()
	s.Phase = d.obs.phase
	d.obs.mu.Unlock()
	if s.Phase == "ramping" && d.metrics.TerrainsOnline.Load() >= d.cfg.Terrain {
		s.Phase = "running"
	}
	if d.forensics != nil {
		fs := d.forensics.Stats()
		s.Forensics = &fs
	}
	return s
}

func (d *Dashboard) snapshot(elapsedSecs int64, live *report.LiveStats) metricsSnapshot {
	snap := metricsSnapshot{
		Alive:             d.metrics.LemmingsAlive.Load(),
		LemmingsStarted:   d.metrics.LemmingsStarted.Load(),
		Completed:         d.metrics.LemmingsCompleted.Load(),
		Failed:            d.metrics.LemmingsFailed.Load(),
		FailedVisits:      d.metrics.FailedVisits.Load(),
		CancelledVisits:   d.metrics.CancelledVisits.Load(),
		TerrainsOnline:    d.metrics.TerrainsOnline.Load(),
		TotalVisits:       d.metrics.TotalVisits.Load(),
		TotalBytes:        report.FormatBytes(d.metrics.TotalBytes.Load()),
		WaitingRoom:       d.metrics.TotalWaitingRoom.Load(),
		QueuePolls:        d.metrics.QueuePolls.Load(),
		AssetRequests:     d.metrics.AssetRequests.Load(),
		AssetFailed:       d.metrics.AssetFailed.Load(),
		AssetDenied403:    d.metrics.AssetDenied403.Load(),
		AssetBytes:        d.metrics.AssetBytes.Load(),
		Bans429:           d.metrics.Bans429.Load(),
		Bans403:           d.metrics.Bans403.Load(),
		Xx2:               d.metrics.Total2xx.Load(),
		Xx3:               d.metrics.Total3xx.Load(),
		Xx4:               d.metrics.Total4xx.Load(),
		Xx5:               d.metrics.Total5xx.Load(),
		OverflowLogs:      d.metrics.OverflowLogs.Load(),
		DroppedLogs:       d.metrics.DroppedLogs.Load(),
		ArtifactsCaptured: d.metrics.ArtifactsCaptured.Load(),
		ScoutsAlive:       d.metrics.ScoutsAlive.Load(),
		ScoutVisits:       d.metrics.ScoutVisits.Load(),
		ScoutFailures:     d.metrics.ScoutFailures.Load(),
		ElapsedSecs:       elapsedSecs,
	}
	if live != nil {
		snap.AssetRequests = max(snap.AssetRequests, live.Assets.Requests)
		snap.AssetFailed = max(snap.AssetFailed, live.Assets.Failed)
		snap.AssetDenied403 = max(snap.AssetDenied403, live.Assets.Denied403)
		snap.AssetBytes = max(snap.AssetBytes, live.Assets.Bytes)
		snap.QueuePolls = max(snap.QueuePolls, live.Room.TotalPolls)
		snap.Bans429 = max(snap.Bans429, live.Concert.Bans429)
		snap.Bans403 = max(snap.Bans403, live.Concert.Bans403)
		snap.ScoutVisits = max(snap.ScoutVisits, live.Scouts.Visits)
		snap.ScoutFailures = max(snap.ScoutFailures, live.Scouts.Failures)
	}
	return snap
}

// ── Client Registry ──────────────────────────────────────────────────────────

func (cr *clientRegistry) add() uint64 {
	id := cr.nextID.Add(1)
	ch := make(chan sseEvent, 64)
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

func (cr *clientRegistry) count() int {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	return len(cr.clients)
}

func (cr *clientRegistry) broadcast(evt sseEvent) {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	for _, ch := range cr.clients {
		select {
		case ch <- evt:
		default:
		}
	}
}

// ── Embedded Pages & Strict Hash-Based CSP ───────────────────────────────────

var pages = buildPages()

type builtPages struct {
	dashboard, login       string
	dashboardCSP, loginCSP string
}

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
	loginScript := between(loginJS, "<script>", "</script>")
	return builtPages{
		dashboard:    inline(read("index.html")),
		login:        inline(loginJS),
		dashboardCSP: csp(theme, js),
		loginCSP:     csp(theme, loginScript),
	}
}

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

// AuthHTML returns the self-contained login page.
func AuthHTML() string { return pages.login }

type dashboardConfig struct {
	Hit           string  `json:"hit"`
	Version       string  `json:"version"`
	Terrain       int64   `json:"terrain"`
	Pack          int64   `json:"pack"`
	Total         int64   `json:"total"`
	Limit         int     `json:"limit"`
	UntilSecs     float64 `json:"until"`
	RampSecs      float64 `json:"ramp"`
	RampShape     string  `json:"ramp_shape"`
	EtaSecs       float64 `json:"eta"`
	ThinkMin      float64 `json:"think_min"`
	ThinkMax      float64 `json:"think_max"`
	Navigation    string  `json:"navigation"`
	Journey       string  `json:"journey,omitempty"`
	Stage         int     `json:"stage"`
	FailGate      float64 `json:"fail_gate"`
	P95Gate       float64 `json:"p95_gate"`
	Scouts        int     `json:"scouts"`
	Assets        bool    `json:"assets"`
	AssetsPerPage int     `json:"assets_per_page"`
	ReviewMode    bool    `json:"review_mode"`
	RevealCookies bool    `json:"reveal_cookies"`
}

// DashboardHTML returns the rendered live portal HTML with JSON-escaped config.
func DashboardHTML(cfg swarm.Config) string {
	hit := discovery.SafeURL(cfg.Hit)
	c := dashboardConfig{
		Hit: hit, Version: cfg.Version, Terrain: cfg.Terrain, Pack: cfg.Pack,
		Total: cfg.Terrain * cfg.Pack, Limit: cfg.Limit,
		UntilSecs: cfg.Until.Seconds(), RampSecs: cfg.Ramp.Seconds(), RampShape: cfg.RampShape,
		EtaSecs:  swarm.EstimateWallClock(cfg).Seconds(),
		ThinkMin: cfg.ThinkMin.Seconds(), ThinkMax: cfg.ThinkMax.Seconds(),
		Navigation: cfg.Navigation, Stage: StageSize, FailGate: -1,
		P95Gate: cfg.P95Budget.Seconds(), Scouts: cfg.Scouts,
		Assets: cfg.Assets, AssetsPerPage: cfg.AssetsPerPage,
		ReviewMode: cfg.ReviewMode, RevealCookies: cfg.RevealSessionCookies,
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
