package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/sema"
)

// SwarmConfig is the single source of truth flowing from CLI into every layer.
type SwarmConfig struct {
	Hit           string
	Terrain       int64
	Pack          int64
	Limit         int
	Until         time.Duration
	Ramp          time.Duration
	Crawl         bool
	CrawlDepth    int
	DashboardPort int
	TTY           bool
	Version       string
	Observe         bool
	MetricsPort     int
	MetricsURLLabel string
	SaveTo   []string
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
}

// SwarmMetrics holds live atomic counters readable by the ticker and dashboard.
type SwarmMetrics struct {
	LemmingsAlive     atomic.Int64
	LemmingsCompleted atomic.Int64
	LemmingsFailed    atomic.Int64
	TerrainsOnline    atomic.Int64
	TotalVisits       atomic.Int64
	TotalBytes        atomic.Int64
	TotalWaitingRoom  atomic.Int64
	Total2xx          atomic.Int64
	Total3xx          atomic.Int64
	Total4xx          atomic.Int64
	Total5xx          atomic.Int64
	OverflowLogs      atomic.Int64 // lifelogs that hit the overflow channel
	DroppedLogs       atomic.Int64 // lifelogs dropped when both channels full
}

const (
	primaryChanCap  = 500_000
	overflowChanCap = 500_000
)

// Swarm is the top-level coordinator. One swarm per lemmings invocation.
type Swarm struct {
	cfg       SwarmConfig
	ctx       context.Context
	cancel    context.CancelFunc
	pool      *URLPool
	terrains  []*Terrain
	primary   chan LifeLog
	overflow  chan LifeLog
	events    *EventBus
	metrics   SwarmMetrics
	reporter  *Reporter
	dashboard *Dashboard
	sema      sema.Semaphore
	token     string
	startedAt time.Time
	mu        sync.Mutex
	observers []Observer
}

// NewSwarm constructs and indexes the swarm but does not start lemmings yet.
func NewSwarm(ctx context.Context, cfg SwarmConfig) (*Swarm, error) {
	ctx, cancel := context.WithCancel(ctx)

	token, err := generateToken()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to generate dashboard token: %w", err)
	}

	limit := cfg.Limit
	if limit == -1 {
		limit = int(cfg.Terrain * cfg.Pack)
	}
	sem := sema.New(limit)

	// Primary channel: sized to actual total, capped at primaryChanCap
	primarySize := int(cfg.Terrain * cfg.Pack)
	if primarySize > primaryChanCap {
		primarySize = primaryChanCap
	}

	bus := NewEventBus()

	s := &Swarm{
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		primary:  make(chan LifeLog, primarySize),
		overflow: make(chan LifeLog, overflowChanCap),
		events:   bus,
		sema:     sem,
		token:    token,
	}

	fmt.Printf("  dashboard token: %s\n\n", token)

	fmt.Println("  indexing origin...")
	pool, err := BuildURLPool(ctx, cfg.Hit, cfg.Crawl, cfg.CrawlDepth)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to index origin %s: %w", cfg.Hit, err)
	}
	s.pool = pool
	fmt.Printf("  indexed %d URLs from %s\n\n", len(pool.URLs), cfg.Hit)

	s.terrains = make([]*Terrain, cfg.Terrain)
	for i := int64(0); i < cfg.Terrain; i++ {
		s.terrains[i] = NewTerrain(i, cfg, pool, s.sendLifeLog, bus, sem, &s.metrics)
	}

	s.reporter = NewReporter(cfg)
	s.dashboard = NewDashboard(cfg, bus, &s.metrics, token)

	if cfg.Observe {
	    obs := NewPrometheusObserver(cfg)
    	if err := s.RegisterObserver(obs); err != nil {
    	    cancel()
    	    return nil, fmt.Errorf("prometheus observer: %w", err)
    	}
	}

	return s, nil
}

// RegisterObserver attaches an Observer to the swarm's EventBus and
// appends it to the internal observer list for automatic cleanup when
// Run completes.
//
// Must be called after NewSwarm and before Run. Not safe for concurrent
// use — call RegisterObserver sequentially during swarm setup.
//
// Usage:
//
//	obs := NewPrometheusObserver(cfg)
//	if err := swarm.RegisterObserver(obs); err != nil {
//	    log.Fatal(err)
//	}
//
// Warning: if Attach returns an error the observer is not appended to
// the list and RegisterObserver returns the error. Run will not attempt
// to call Detach on an observer that was never successfully attached.
func (s *Swarm) RegisterObserver(o Observer) error {
	if err := o.Attach(s.ctx, s.events); err != nil {
		return fmt.Errorf("observer %s attach: %w", o.Name(), err)
	}
	s.observers = append(s.observers, o)
	return nil
}

// sendLifeLog is the non-blocking death drain. Primary first, overflow second,
// drop counter third. A lemming never blocks at death.
func (s *Swarm) sendLifeLog(ll LifeLog) {
	select {
	case s.primary <- ll:
	default:
		select {
		case s.overflow <- ll:
			s.metrics.OverflowLogs.Add(1)
			s.events.Emit(Event{Kind: EventLogOverflow})
		default:
			s.metrics.DroppedLogs.Add(1)
			s.events.Emit(Event{Kind: EventLogDropped})
		}
	}
}

// Run starts the dashboard, ramp scheduler, collector, and STDOUT ticker.
// Blocks until all lemmings are dead and results are collected.
func (s *Swarm) Run() error {
	s.startedAt = time.Now()
	defer func() {
	    for _, o := range s.observers {
	        if err := o.Detach(); err != nil {
	            log.Printf("observer %s detach error: %v", o.Name(), err)
	        }
	    }
	}()

	go func() {
		if err := s.dashboard.Serve(s.ctx); err != nil {
			log.Printf("dashboard error: %v", err)
		}
	}()

	var collectorWg sync.WaitGroup
	collectorWg.Add(1)
	go func() {
		defer collectorWg.Done()
		s.collectResults()
	}()

	go s.tickSTDOUT()

	if err := s.ramp(); err != nil {
		return fmt.Errorf("ramp error: %w", err)
	}

	var terrainWg sync.WaitGroup
	for _, t := range s.terrains {
		terrainWg.Add(1)
		go func(t *Terrain) {
			defer terrainWg.Done()
			t.Wait()
		}(t)
	}
	terrainWg.Wait()

	// All lemmings dead — close primary so collector drains and exits
	close(s.primary)
	collectorWg.Wait()

	elapsed := time.Since(s.startedAt).Round(time.Second)
	fmt.Printf("\n\n  all lemmings have died. elapsed: %s\n", elapsed)
	s.printFinalSummary()

	return nil
}

// ramp brings terrain groups online linearly over cfg.Ramp duration.
func (s *Swarm) ramp() error {
	n := len(s.terrains)
	if n == 0 {
		return nil
	}

	interval := s.cfg.Ramp / time.Duration(n)
	fmt.Printf("  ramping %d terrain groups over %s (%s between each)\n\n",
		n, s.cfg.Ramp, interval.Round(time.Millisecond))

	for i, t := range s.terrains {
		select {
		case <-s.ctx.Done():
			fmt.Printf("\n  ramp cancelled after %d/%d terrains\n", i, n)
			return nil
		default:
		}

		t.Launch(s.ctx)
		s.metrics.TerrainsOnline.Add(1)
		s.events.Emit(Event{
			Kind:    EventTerrainOnline,
			Terrain: int(t.id),
		})

		if i < n-1 {
			select {
			case <-s.ctx.Done():
				return nil
			case <-time.After(interval):
			}
		}
	}

	return nil
}

// collectResults drains primary then overflow until primary is closed and
// overflow is empty.
func (s *Swarm) collectResults() {
	for {
		select {
		case ll, ok := <-s.primary:
			if !ok {
				// primary closed — drain overflow and exit
				s.drainOverflow()
				return
			}
			s.ingest(ll)

		case ll := <-s.overflow:
			s.ingest(ll)
		}
	}
}

// drainOverflow empties the overflow channel after primary closes.
func (s *Swarm) drainOverflow() {
	for {
		select {
		case ll := <-s.overflow:
			s.ingest(ll)
		default:
			return
		}
	}
}

// ingest updates metrics and hands a LifeLog to the reporter and event bus.
func (s *Swarm) ingest(ll LifeLog) {
	s.metrics.LemmingsCompleted.Add(1)
	s.metrics.LemmingsAlive.Add(-1)

	for _, visit := range ll.Visits {
		s.metrics.TotalVisits.Add(1)
		s.metrics.TotalBytes.Add(visit.BytesIn)

		if visit.WaitingRoom.Detected {
			s.metrics.TotalWaitingRoom.Add(1)
		}

		switch {
		case visit.StatusCode >= 200 && visit.StatusCode < 300:
			s.metrics.Total2xx.Add(1)
		case visit.StatusCode >= 300 && visit.StatusCode < 400:
			s.metrics.Total3xx.Add(1)
		case visit.StatusCode >= 400 && visit.StatusCode < 500:
			s.metrics.Total4xx.Add(1)
		case visit.StatusCode >= 500:
			s.metrics.Total5xx.Add(1)
		}
	}

	if ll.Error != nil {
		s.metrics.LemmingsFailed.Add(1)
	}

	s.reporter.Ingest(ll)
	s.events.Emit(Event{
		Kind:    EventLemmingDied,
		Terrain: ll.Terrain,
		Pack:    ll.Pack,
	})
}

// tickSTDOUT prints live metrics every second.
// Uses \r in TTY mode to overwrite the line; newline in CI mode.
func (s *Swarm) tickSTDOUT() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	terminator := "\n"
	if s.cfg.TTY {
		terminator = "\r"
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			alive := s.metrics.LemmingsAlive.Load()
			completed := s.metrics.LemmingsCompleted.Load()
			visits := s.metrics.TotalVisits.Load()
			terrains := s.metrics.TerrainsOnline.Load()
			elapsed := time.Since(s.startedAt).Round(time.Second)
			xx2 := s.metrics.Total2xx.Load()
			xx4 := s.metrics.Total4xx.Load()
			xx5 := s.metrics.Total5xx.Load()
			overflow := s.metrics.OverflowLogs.Load()
			dropped := s.metrics.DroppedLogs.Load()

			line := fmt.Sprintf(
				"  [%s] terrains: %d | alive: %s | done: %s | visits: %s | 2xx: %s 4xx: %s 5xx: %s",
				elapsed,
				terrains,
				formatInt(alive),
				formatInt(completed),
				formatInt(visits),
				formatInt(xx2),
				formatInt(xx4),
				formatInt(xx5),
			)

			// Only show overflow/dropped if they're non-zero —
			// no need to alarm engineers who sized their runs correctly
			if overflow > 0 {
				line += fmt.Sprintf(" | overflow: %s", formatInt(overflow))
			}
			if dropped > 0 {
				line += fmt.Sprintf(" | ⚠ dropped: %s", formatInt(dropped))
			}

			fmt.Print(line + terminator)
			os.Stdout.Sync()
		}
	}
}

// printFinalSummary writes the post-run block to STDOUT.
func (s *Swarm) printFinalSummary() {
	total := s.cfg.Terrain * s.cfg.Pack
	completed := s.metrics.LemmingsCompleted.Load()
	failed := s.metrics.LemmingsFailed.Load()
	visits := s.metrics.TotalVisits.Load()
	bytes := s.metrics.TotalBytes.Load()
	wr := s.metrics.TotalWaitingRoom.Load()
	xx2 := s.metrics.Total2xx.Load()
	xx3 := s.metrics.Total3xx.Load()
	xx4 := s.metrics.Total4xx.Load()
	xx5 := s.metrics.Total5xx.Load()
	overflow := s.metrics.OverflowLogs.Load()
	dropped := s.metrics.DroppedLogs.Load()

	fmt.Println()
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  lemmings v%s — final summary\n", s.cfg.Version)
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  target:         %s\n", s.cfg.Hit)
	fmt.Printf("  total lemmings: %s\n", formatInt(total))
	fmt.Printf("  completed:      %s\n", formatInt(completed))
	fmt.Printf("  failed:         %s\n", formatInt(failed))
	fmt.Println()
	fmt.Printf("  total visits:   %s\n", formatInt(visits))
	fmt.Printf("  total bytes:    %s\n", formatBytes(bytes))
	fmt.Println()
	fmt.Printf("  2xx:            %s\n", formatInt(xx2))
	fmt.Printf("  3xx:            %s\n", formatInt(xx3))
	fmt.Printf("  4xx:            %s\n", formatInt(xx4))
	fmt.Printf("  5xx:            %s\n", formatInt(xx5))
	fmt.Println()
	fmt.Printf("  waiting room:   %s lemmings held\n", formatInt(wr))
	fmt.Println()

	// Only surface channel pressure metrics if they occurred
	if overflow > 0 || dropped > 0 {
		fmt.Println("  channel pressure:")
		fmt.Printf("    overflow logs:  %s\n", formatInt(overflow))
		fmt.Printf("    dropped logs:   %s\n", formatInt(dropped))
		if dropped > 0 {
			fmt.Println()
			fmt.Println("  ⚠  dropped logs indicate your -limit is too low")
			fmt.Println("     for the volume requested. increase -limit or")
			fmt.Println("     reduce -terrain and -pack for accurate results.")
		}
		fmt.Println()
	}

	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  report: %s\n\n", s.cfg.SaveTo)
}

// Report triggers the reporter to write output files.
func (s *Swarm) Report() error {
	return s.reporter.Write(s.cfg.SaveTo)
}

// generateToken produces a cryptographically random 64-char hex string.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// formatBytes renders a byte count as human-readable.
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
