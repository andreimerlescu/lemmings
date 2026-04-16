package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// maxURLLabelCardinality is the maximum number of distinct URL label values
// permitted on the visit duration histogram. Prometheus charges memory and
// scrape time proportionally to unique label combinations. A histogram with
// 1000+ URL labels at 15 buckets each creates 15,000+ active time series
// from one label alone, which exceeds the comfortable range of a default
// Prometheus installation without explicit tuning. 999 keeps worst-case
// series counts within safe defaults while still providing per-URL granularity
// for any real-world application with a navigable page count.
//
// If the URL pool exceeds this limit, the observer logs a warning and falls
// back to recording excess URLs under the label value "other". Engineers
// running against sites with more than 999 distinct pages should set
// -metrics-url-label=none to eliminate cardinality risk entirely.
const maxURLLabelCardinality = 999

// Observer is the interface for anything that wants to observe swarm events
// in real time. Implementations receive events from the EventBus, maintain
// their own internal state, and expose that state through whatever mechanism
// is appropriate — an HTTP endpoint, a file, a remote write, etc.
//
// Usage:
//
//	obs := NewPrometheusObserver(cfg)
//	if err := swarm.RegisterObserver(obs); err != nil {
//	    log.Fatal(err)
//	}
//
// Observers are attached before the swarm runs and detached after it
// completes. RegisterObserver handles both operations.
//
// Warning: Attach must be called before the swarm emits any events.
// Events emitted before Attach is called are not replayed.
type Observer interface {
	// Attach registers the observer's EventBus subscriber and starts
	// any background goroutines the observer requires. The provided
	// context governs the observer's lifetime — cancelling it is
	// equivalent to calling Detach.
	Attach(ctx context.Context, bus *EventBus) error

	// Detach unregisters the EventBus subscriber and shuts down any
	// background goroutines or servers the observer started. Detach
	// must be safe to call multiple times without panicking.
	Detach() error

	// Name returns a human-readable identifier used in log output.
	Name() string
}

// PrometheusObserver implements Observer by maintaining a set of Prometheus
// collectors that update in real time as swarm events arrive, and serving
// them via a /metrics HTTP endpoint that Prometheus can scrape.
//
// Usage:
//
//	obs := NewPrometheusObserver(cfg)
//	swarm.RegisterObserver(obs)
//
// The observer exposes eleven metrics covering lemming lifecycle, visit
// outcomes, timing distributions, waiting room behaviour, and channel
// pressure. All metrics are namespaced under "lemmings_".
//
// Warning: PrometheusObserver uses its own prometheus.Registry isolated
// from the default global registry. This prevents conflicts when multiple
// test instances run in the same process, and avoids polluting the global
// registry with lemmings metrics when lemmings is used as a library.
type PrometheusObserver struct {
	cfg      SwarmConfig
	registry *prometheus.Registry
	server   *http.Server
	unsub    func()
	mu       sync.Mutex

	// url label cardinality tracking
	seenURLs    map[string]bool
	urlLabelCap bool // true once maxURLLabelCardinality is reached

	// ── Collectors ───────────────────────────────────────────────────

	// lemmings_alive tracks the current number of running lemmings.
	// Incremented on EventLemmingBorn, decremented on EventLemmingDied.
	alive prometheus.Gauge

	// lemmings_completed_total counts lemmings that completed normally.
	completed prometheus.Counter

	// lemmings_failed_total counts lemmings that failed to start —
	// typically because the semaphore could not be acquired before
	// the context was cancelled.
	failed prometheus.Counter

	// lemmings_visits_total counts page visits by HTTP status class.
	// Label: status_class with values "2xx", "3xx", "4xx", "5xx".
	visits *prometheus.CounterVec

	// lemmings_visit_duration_seconds records per-visit latency.
	// Label: url — behaviour controlled by cfg.MetricsURLLabel.
	//   "full"  — full URL, capped at maxURLLabelCardinality
	//   "path"  — path component only, capped at maxURLLabelCardinality
	//   "none"  — no url label, single histogram for all visits
	visitDuration *prometheus.HistogramVec

	// lemmings_bytes_total counts total bytes transferred across all visits.
	bytesTotal prometheus.Counter

	// lemmings_waiting_room_total counts lemmings that encountered a
	// waiting room at least once during their lifespan.
	waitingRoom prometheus.Counter

	// lemmings_waiting_room_duration_seconds records how long lemmings
	// spent in waiting rooms before admission or context expiry.
	waitingRoomDuration prometheus.Histogram

	// lemmings_terrains_online tracks how many terrain groups are
	// currently active. Incremented on EventTerrainOnline,
	// decremented on EventTerrainDone.
	terrainsOnline prometheus.Gauge

	// lemmings_dropped_logs_total counts LifeLogs dropped because both
	// the primary and overflow result channels were full. Non-zero values
	// indicate the -limit flag is too low for the requested volume.
	droppedLogs prometheus.Counter

	// lemmings_overflow_logs_total counts LifeLogs routed to the overflow
	// channel because the primary channel was full. Non-zero values are
	// a warning sign but do not indicate data loss.
	overflowLogs prometheus.Counter
}

// NewPrometheusObserver constructs a PrometheusObserver from the provided
// SwarmConfig. No collectors are registered and no server is started until
// Attach is called.
//
// Usage:
//
//	obs := NewPrometheusObserver(cfg)
//	if err := swarm.RegisterObserver(obs); err != nil {
//	    log.Fatal(err)
//	}
func NewPrometheusObserver(cfg SwarmConfig) *PrometheusObserver {
	reg := prometheus.NewRegistry()

	o := &PrometheusObserver{
		cfg:      cfg,
		registry: reg,
		seenURLs: make(map[string]bool),
	}

	// ── Lemming lifecycle ─────────────────────────────────────────────

	o.alive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lemmings_alive",
		Help: "Number of lemmings currently running.",
	})

	o.completed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_completed_total",
		Help: "Total number of lemmings that completed their lifespan normally.",
	})

	o.failed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_failed_total",
		Help: "Total number of lemmings that failed to start.",
	})

	// ── Visit metrics ─────────────────────────────────────────────────

	o.visits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lemmings_visits_total",
		Help: "Total page visits by HTTP status class.",
	}, []string{"status_class"})
	o.visits.With(prometheus.Labels{"status_class": "000"}).Add(0)

	// Duration histogram label depends on MetricsURLLabel setting.
	// "none" uses an empty label set so all visits collapse into one
	// histogram. "path" and "full" both use a url label but differ in
	// how the label value is derived from the visit URL.
	urlLabels := []string{"url"}
	if cfg.MetricsURLLabel == "none" {
		urlLabels = []string{}
	}
	o.visitDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "lemmings_visit_duration_seconds",
		Help:    "Visit latency distribution in seconds.",
		Buckets: prometheus.DefBuckets,
	}, urlLabels)
	if o.cfg.MetricsURLLabel == "none" {
		o.visitDuration.With(prometheus.Labels{}).Observe(0)
	} else {
		o.visitDuration.With(prometheus.Labels{"url": ""}).Observe(0)
	}

	o.bytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_bytes_total",
		Help: "Total bytes transferred across all visits.",
	})

	// ── Waiting room ──────────────────────────────────────────────────

	o.waitingRoom = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_waiting_room_total",
		Help: "Total number of lemmings that encountered a waiting room.",
	})

	o.waitingRoomDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "lemmings_waiting_room_duration_seconds",
		Help:    "Duration spent in waiting rooms before admission or expiry.",
		Buckets: prometheus.DefBuckets,
	})

	// ── Terrain ───────────────────────────────────────────────────────

	o.terrainsOnline = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lemmings_terrains_online",
		Help: "Number of terrain goroutine groups currently active.",
	})

	// ── Channel pressure ──────────────────────────────────────────────

	o.droppedLogs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_dropped_logs_total",
		Help: "LifeLogs dropped because both result channels were full. " +
			"Non-zero values indicate -limit is too low for the requested volume.",
	})

	o.overflowLogs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_overflow_logs_total",
		Help: "LifeLogs routed to the overflow channel because primary was full. " +
			"Non-zero values are a warning but do not indicate data loss.",
	})

	// Register all collectors with the isolated registry
	reg.MustRegister(
		o.alive,
		o.completed,
		o.failed,
		o.visits,
		o.visitDuration,
		o.bytesTotal,
		o.waitingRoom,
		o.waitingRoomDuration,
		o.terrainsOnline,
		o.droppedLogs,
		o.overflowLogs,
	)

	return o
}

// Name returns the human-readable identifier for this observer.
// Satisfies the Observer interface.
func (o *PrometheusObserver) Name() string {
	return "prometheus"
}

// Attach registers the observer's EventBus subscriber and starts the
// /metrics HTTP server on cfg.MetricsPort. After Attach returns, the
// server is ready to accept Prometheus scrape requests.
//
// Attach subscribes to the following event kinds:
//   - EventLemmingBorn, EventLemmingDied, EventLemmingFailed
//   - EventVisitComplete
//   - EventWaitingRoom
//   - EventTerrainOnline, EventTerrainDone
//   - EventLogDropped, EventLogOverflow
//
// Warning: Attach must be called before the swarm emits events. Events
// emitted before Attach returns are not retroactively processed.
func (o *PrometheusObserver) Attach(ctx context.Context, bus *EventBus) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Subscribe to relevant event kinds only — Filter keeps the handler
	// off the hot path for events the observer does not care about.
	o.unsub = bus.Subscribe(Filter(
		o.handleEvent,
		EventLemmingBorn,
		EventLemmingDied,
		EventLemmingFailed,
		EventVisitComplete,
		EventVisitError,
		EventWaitingRoom,
		EventTerrainOnline,
		EventTerrainDone,
		EventLogDropped,
		EventLogOverflow,
	))

	// Start the metrics HTTP server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/metrics", http.StatusMovedPermanently)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf("localhost:%d", o.cfg.MetricsPort),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	o.server = srv

	// Start server in background — shut down when context cancels
	go func() {
		<-ctx.Done()
		o.mu.Lock()
		s := o.server
		o.mu.Unlock()
		if s == nil {
			return
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(shutCtx); err != nil {
			log.Printf("prometheus observer shutdown error: %v", err)
		}
	}()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("prometheus observer server error: %v", err)
		}
	}()

	return nil
}

// Detach unregisters the EventBus subscriber and shuts down the /metrics
// HTTP server. Safe to call multiple times — subsequent calls after the
// first are no-ops.
//
// Warning: Detach does not wait for in-flight Prometheus scrape requests
// to complete beyond the 5-second shutdown grace period built into Attach.
// If your Prometheus scrape interval is longer than 5 seconds, increase
// the shutdown timeout or ensure scrapes complete before calling Detach.
func (o *PrometheusObserver) Detach() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.unsub != nil {
		o.unsub()
		o.unsub = nil
	}

	if o.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := o.server.Shutdown(ctx)
		o.server = nil
		return err
	}

	return nil
}

// handleEvent translates a swarm Event into one or more Prometheus metric
// updates. This function runs on the EventBus subscriber goroutine — it
// must not block.
//
// Each event kind maps to a specific set of metric operations. Unknown
// event kinds are silently ignored so future event kinds added to the
// bus do not require changes here.
func (o *PrometheusObserver) handleEvent(e Event) {
	switch e.Kind {

	case EventLemmingBorn:
		o.alive.Inc()

	case EventLemmingDied:
		o.alive.Dec()
		o.completed.Inc()

		// Record visit-level metrics from the event if populated.
		// Individual visit metrics arrive via EventVisitComplete —
		// this handles the aggregate counters on lemming death.
		o.bytesTotal.Add(float64(e.BytesIn))

	case EventLemmingFailed:
		o.failed.Inc()

	case EventVisitComplete, EventVisitError:
		o.recordVisit(e)

	case EventWaitingRoom:
		o.waitingRoom.Inc()
		if e.Duration > 0 {
			o.waitingRoomDuration.Observe(e.Duration.Seconds())
		}

	case EventTerrainOnline:
		o.terrainsOnline.Inc()

	case EventTerrainDone:
		o.terrainsOnline.Dec()

	case EventLogDropped:
		o.droppedLogs.Inc()

	case EventLogOverflow:
		o.overflowLogs.Inc()
	}
}

// recordVisit updates the visits counter and visit duration histogram
// for a single page visit event. The URL label value is derived from
// e.URL according to cfg.MetricsURLLabel.
//
// If the number of distinct URL label values has reached
// maxURLLabelCardinality, excess URLs are recorded under the synthetic
// label value "other" and a one-time warning is logged.
func (o *PrometheusObserver) recordVisit(e Event) {
	// Status class label
	statusclass := statusClass(e.StatusCode)
	o.visits.With(prometheus.Labels{"status_class": statusclass}).Inc()

	// Duration observation
	if e.Duration > 0 {
		if o.cfg.MetricsURLLabel == "none" {
			o.visitDuration.With(prometheus.Labels{}).Observe(e.Duration.Seconds())
		} else {
			label := o.urlLabel(e.URL)
			o.visitDuration.With(prometheus.Labels{"url": label}).Observe(e.Duration.Seconds())
		}
	}
}

// urlLabel derives the url histogram label value from a raw URL string
// according to cfg.MetricsURLLabel. Enforces maxURLLabelCardinality by
// returning "other" for URLs beyond the cap.
//
// "full" — returns the full URL as-is.
// "path" — returns the path component only, stripping scheme, host,
//
//	and query string.
//
// Cardinality is tracked across all calls. Once maxURLLabelCardinality
// distinct values have been seen, any new URL returns "other".
func (o *PrometheusObserver) urlLabel(rawURL string) string {
	var label string

	switch o.cfg.MetricsURLLabel {
	case "full":
		label = rawURL
	case "path":
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Path == "" {
			label = rawURL
		} else {
			label = parsed.Path
		}
	default:
		label = rawURL
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.seenURLs[label] {
		return label
	}

	if len(o.seenURLs) >= maxURLLabelCardinality {
		if !o.urlLabelCap {
			o.urlLabelCap = true
			log.Printf(
				"prometheus observer: url label cardinality reached %d — "+
					"additional URLs will be recorded as 'other'. "+
					"Set -metrics-url-label=none to disable url labels entirely.",
				maxURLLabelCardinality,
			)
		}
		return "other"
	}

	o.seenURLs[label] = true
	return label
}

// statusClass returns the HTTP status class string for a status code.
// Unknown or zero status codes return "unknown".
func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// urlPathOnly extracts the path component from a raw URL string,
// stripping the scheme, host, query string, and fragment.
// Returns the raw URL unchanged if parsing fails or the path is empty.
//
// Used internally by urlLabel when MetricsURLLabel is "path".
func urlPathOnly(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Path == "" {
		return rawURL
	}
	return parsed.Path
}
