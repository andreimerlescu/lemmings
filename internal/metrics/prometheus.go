// Package metrics implements the isolated Prometheus /metrics observer with
// synchronous loopback binding, DNS-rebinding host validation, and URL
// cardinality protection.
package metrics

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const maxURLLabelCardinality = 999

// PrometheusObserver exposes 17 lemmings_* metrics on a loopback /metrics endpoint.
type PrometheusObserver struct {
	cfg      swarm.Config
	registry *prometheus.Registry
	server   *http.Server
	unsub    func()
	srvWg    sync.WaitGroup
	watchWg  sync.WaitGroup
	stopCh   chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	addr     string

	seenURLs    map[string]bool
	urlLabelCap bool

	alive               prometheus.Gauge
	completed           prometheus.Counter
	failed              prometheus.Counter
	visits              *prometheus.CounterVec
	failedVisits        prometheus.Counter
	cancelledVisits     prometheus.Counter
	visitDuration       *prometheus.HistogramVec
	bytesTotal          prometheus.Counter
	waitingRoom         prometheus.Counter
	waitingRoomDuration prometheus.Histogram
	terrainsOnline      prometheus.Gauge
	droppedLogs         prometheus.Counter
	overflowLogs        prometheus.Counter

	// v2.0 forensics & browser scout metrics.
	artifactsCaptured *prometheus.CounterVec
	artifactsDropped  *prometheus.CounterVec
	jsConsoleErrors   prometheus.Counter
	scoutsAlive       prometheus.Gauge
}

// NewPrometheusObserver creates a PrometheusObserver backed by an isolated registry.
func NewPrometheusObserver(cfg swarm.Config) *PrometheusObserver {
	reg := prometheus.NewRegistry()
	o := &PrometheusObserver{
		cfg:      cfg,
		registry: reg,
		seenURLs: make(map[string]bool),
	}

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
	o.visits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lemmings_visits_total",
		Help: "Total page visits by HTTP status class.",
	}, []string{"status_class"})
	o.visits.With(prometheus.Labels{"status_class": "000"}).Add(0)

	urlLabels := []string{"url"}
	if cfg.MetricsURLLabel == "none" {
		urlLabels = []string{}
	}
	o.visitDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "lemmings_visit_duration_seconds",
		Help:    "Visit latency distribution in seconds.",
		Buckets: prometheus.DefBuckets,
	}, urlLabels)
	if cfg.MetricsURLLabel == "none" {
		o.visitDuration.With(prometheus.Labels{})
	} else {
		o.visitDuration.With(prometheus.Labels{"url": ""})
	}

	o.bytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_bytes_total",
		Help: "Total bytes transferred across all visits.",
	})
	o.waitingRoom = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_waiting_room_total",
		Help: "Total number of waiting room stays.",
	})
	o.waitingRoomDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "lemmings_waiting_room_duration_seconds",
		Help:    "Duration spent in waiting rooms before admission or expiry.",
		Buckets: prometheus.DefBuckets,
	})
	o.terrainsOnline = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lemmings_terrains_online",
		Help: "Number of terrain goroutine groups currently active.",
	})
	o.droppedLogs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_dropped_logs_total",
		Help: "LifeLogs dropped because both primary and overflow channels were full.",
	})
	o.overflowLogs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_overflow_logs_total",
		Help: "LifeLogs routed to the overflow channel because primary was full.",
	})
	o.failedVisits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_failed_visits_total",
		Help: "Page visits that failed transport, status, or page checks.",
	})
	o.cancelledVisits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_cancelled_visits_total",
		Help: "Page visits cut short when a lemming's life or the run ended.",
	})
	o.artifactsCaptured = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lemmings_artifacts_captured_total",
		Help: "Error forensic artifacts captured by capture mode.",
	}, []string{"mode"})
	o.artifactsDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lemmings_artifacts_dropped_total",
		Help: "Error forensic captures skipped or dropped by reason.",
	}, []string{"reason"})
	o.jsConsoleErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lemmings_js_console_errors_total",
		Help: "Total JavaScript console errors and exceptions observed across captures.",
	})
	o.scoutsAlive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lemmings_scouts_alive",
		Help: "Number of live real-browser Scout lemmings currently active.",
	})

	reg.MustRegister(
		o.alive, o.completed, o.failed, o.visits, o.failedVisits, o.cancelledVisits,
		o.visitDuration, o.bytesTotal, o.waitingRoom, o.waitingRoomDuration,
		o.terrainsOnline, o.droppedLogs, o.overflowLogs,
		o.artifactsCaptured, o.artifactsDropped, o.jsConsoleErrors, o.scoutsAlive,
	)
	return o
}

// Name satisfies swarm.Observer.
func (o *PrometheusObserver) Name() string { return "prometheus" }

// Registry exposes the isolated Prometheus registry for testing.
func (o *PrometheusObserver) Registry() *prometheus.Registry { return o.registry }

// Addr returns the bound TCP address after Attach succeeds.
func (o *PrometheusObserver) Addr() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.addr
}

// Attach subscribes to bus and synchronously binds 127.0.0.1:<MetricsPort> with
// a localOnly Host-header guard against DNS rebinding (SEC-04).
func (o *PrometheusObserver) Attach(ctx context.Context, bus *events.Bus) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	bindAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(o.cfg.MetricsPort))
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("metrics listen %s: %w", bindAddr, err)
	}
	o.addr = ln.Addr().String()

	o.unsub = bus.Subscribe(events.Filter(
		o.handleEvent,
		events.LemmingBorn, events.LemmingDied, events.LemmingFailed,
		events.VisitComplete, events.VisitError, events.WaitingRoom,
		events.TerrainOnline, events.TerrainDone, events.LogDropped, events.LogOverflow,
		events.ArtifactCaptured, events.ArtifactDropped, events.ScoutStarted, events.ScoutDone,
	))

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/metrics", http.StatusMovedPermanently)
	})

	srv := &http.Server{
		Handler:           localOnlyHost(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	o.server = srv
	stopCh := make(chan struct{})
	o.stopCh = stopCh
	o.stopOnce = sync.Once{}

	o.srvWg.Add(1)
	go func() {
		defer o.srvWg.Done()
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("prometheus observer server error: %v", err)
		}
	}()

	o.watchWg.Add(1)
	go func() {
		defer o.watchWg.Done()
		select {
		case <-ctx.Done():
			_ = o.stopServer()
		case <-stopCh:
		}
	}()
	return nil
}

func (o *PrometheusObserver) stopServer() error {
	o.mu.Lock()
	unsub := o.unsub
	o.unsub = nil
	srv := o.server
	o.server = nil
	o.mu.Unlock()

	if unsub != nil {
		unsub()
	}
	var err error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = srv.Shutdown(ctx)
		if err != nil {
			_ = srv.Close()
		}
	}
	o.srvWg.Wait()
	return err
}

// Detach unsubscribes from the bus, shuts down the HTTP server, and joins every goroutine.
func (o *PrometheusObserver) Detach() error {
	o.mu.Lock()
	stopCh := o.stopCh
	o.mu.Unlock()

	if stopCh != nil {
		o.stopOnce.Do(func() { close(stopCh) })
	}
	err := o.stopServer()
	o.watchWg.Wait()
	return err
}

func (o *PrometheusObserver) handleEvent(e events.Event) {
	switch e.Kind {
	case events.LemmingBorn:
		o.alive.Inc()
	case events.LemmingDied:
		o.alive.Dec()
		o.completed.Inc()
	case events.LemmingFailed:
		o.failed.Inc()
	case events.VisitComplete, events.VisitError:
		o.recordVisit(e)
	case events.WaitingRoom:
		o.waitingRoom.Inc()
		if e.Duration > 0 {
			o.waitingRoomDuration.Observe(e.Duration.Seconds())
		}
	case events.TerrainOnline:
		o.terrainsOnline.Inc()
	case events.TerrainDone:
		o.terrainsOnline.Dec()
	case events.LogDropped:
		o.droppedLogs.Inc()
	case events.LogOverflow:
		o.overflowLogs.Inc()
	case events.ArtifactCaptured:
		mode := e.CaptureMode
		if mode == "" {
			mode = "unknown"
		}
		o.artifactsCaptured.With(prometheus.Labels{"mode": mode}).Inc()
		if e.ConsoleErrors > 0 {
			o.jsConsoleErrors.Add(float64(e.ConsoleErrors))
		}
	case events.ArtifactDropped:
		reason := e.DropReason
		if reason == "" {
			reason = "unknown"
		}
		o.artifactsDropped.With(prometheus.Labels{"reason": reason}).Inc()
	case events.ScoutStarted:
		o.scoutsAlive.Inc()
	case events.ScoutDone:
		o.scoutsAlive.Dec()
	}
}

func (o *PrometheusObserver) recordVisit(e events.Event) {
	o.bytesTotal.Add(float64(e.BytesIn))
	switch {
	case e.Cancelled:
		o.cancelledVisits.Inc()
	case e.Failed:
		o.failedVisits.Inc()
	}
	o.visits.With(prometheus.Labels{"status_class": report.StatusClass(e.StatusCode)}).Inc()

	if e.Duration > 0 {
		if o.cfg.MetricsURLLabel == "none" {
			o.visitDuration.With(prometheus.Labels{}).Observe(e.Duration.Seconds())
		} else {
			label := o.urlLabel(e.URL)
			o.visitDuration.With(prometheus.Labels{"url": label}).Observe(e.Duration.Seconds())
		}
	}
}

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
			log.Printf("prometheus observer: url label cardinality reached %d — recording additional URLs as 'other'", maxURLLabelCardinality)
		}
		return "other"
	}
	o.seenURLs[label] = true
	return label
}

func localOnlyHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch strings.Trim(strings.ToLower(host), "[]") {
		case "localhost", "127.0.0.1", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
		}
	})
}
