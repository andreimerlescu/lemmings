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
	SaveTo        string
	DashboardPort int
	Version       string
}

// SwarmMetrics holds the live atomic counters that both the STDOUT ticker
// and the dashboard read from. Atomic so no lock needed on hot path.
type SwarmMetrics struct {
	LemmingsAlive     atomic.Int64
	LemmingsCompleted atomic.Int64
	LemmingsFailed    atomic.Int64
	TotalVisits       atomic.Int64
	TotalBytes        atomic.Int64
	TotalWaitingRoom  atomic.Int64 // lemmings that hit a waiting room at least once
	Total2xx          atomic.Int64
	Total3xx          atomic.Int64
	Total4xx          atomic.Int64
	Total5xx          atomic.Int64
	TerrainsOnline    atomic.Int64
}

// Swarm is the top-level coordinator. One swarm per lemmings invocation.
type Swarm struct {
	cfg       SwarmConfig
	ctx       context.Context
	cancel    context.CancelFunc
	pool      *URLPool
	terrains  []*Terrain
	results   chan LifeLog
	events    *EventBus
	metrics   SwarmMetrics
	reporter  *Reporter
	dashboard *Dashboard
	sema      sema.Semaphore
	token     string    // dashboard auth token
	startedAt time.Time
	mu        sync.Mutex
}

// NewSwarm constructs and indexes the swarm but does not start lemmings yet.
func NewSwarm(ctx context.Context, cfg SwarmConfig) (*Swarm, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Generate dashboard auth token
	token, err := generateToken()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to generate dashboard token: %w", err)
	}

	// Initialize semaphore
	// -1 means unlimited: set ceiling to terrain*pack (the raw total)
	limit := cfg.Limit
	if limit == -1 {
		limit = int(cfg.Terrain * cfg.Pack)
	}
	sem := sema.New(limit)

	// Results channel — buffered to total lemmings so no lemming ever blocks
	// on send at death. If total exceeds int, cap at a safe buffer.
	total := cfg.Terrain * cfg.Pack
	bufSize := int(total)
	if bufSize > 1_000_000 {
		bufSize = 1_000_000
	}
	results := make(chan LifeLog, bufSize)

	// Event bus
	bus := NewEventBus()

	s := &Swarm{
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
		results: results,
		events:  bus,
		sema:    sem,
		token:   token,
	}

	// Print dashboard token to STDOUT — engineer enters this at localhost:4000
	fmt.Printf("  dashboard token: %s\n\n", token)

	// Index the URL pool before any lemming spawns
	fmt.Println("  indexing origin...")
	pool, err := BuildURLPool(ctx, cfg.Hit, cfg.Crawl, cfg.CrawlDepth)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to index origin %s: %w", cfg.Hit, err)
	}
	s.pool = pool
	fmt.Printf("  indexed %d URLs from %s\n\n", len(pool.URLs), cfg.Hit)

	// Build terrains — do not spawn lemmings yet, ramp does that
	s.terrains = make([]*Terrain, cfg.Terrain)
	for i := int64(0); i < cfg.Terrain; i++ {
		s.terrains[i] = NewTerrain(i, cfg, pool, results, bus, sem, &s.metrics)
	}

	// Wire reporter and dashboard
	s.reporter = NewReporter(cfg)
	s.dashboard = NewDashboard(cfg, bus, &s.metrics, token)

	return s, nil
}

// Run starts the dashboard, begins the ramp scheduler, collects results,
// and blocks until all lemmings have died or context is cancelled.
func (s *Swarm) Run() error {
	s.startedAt = time.Now()

	// Start dashboard server
	go func() {
		if err := s.dashboard.Serve(s.ctx); err != nil {
			log.Printf("dashboard error: %v", err)
		}
	}()

	// Start result collector
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)
	go func() {
		defer collectorWg.Done()
		s.collectResults()
	}()

	// Start STDOUT ticker
	go s.tickSTDOUT()

	// Ramp scheduler — brings terrains online over cfg.Ramp duration
	if err := s.ramp(); err != nil {
		return fmt.Errorf("ramp error: %w", err)
	}

	// Wait for all terrains to finish
	var terrainWg sync.WaitGroup
	for _, t := range s.terrains {
		terrainWg.Add(1)
		go func(t *Terrain) {
			defer terrainWg.Done()
			t.Wait()
		}(t)
	}
	terrainWg.Wait()

	// All lemmings dead — close results channel so collector can drain and exit
	close(s.results)
	collectorWg.Wait()

	elapsed := time.Since(s.startedAt).Round(time.Second)
	fmt.Printf("\n\n  all lemmings have died. elapsed: %s\n", elapsed)
	s.printFinalSummary()

	return nil
}

// ramp brings terrain groups online sequentially over cfg.Ramp duration.
// Each terrain is started with a linear delay between them.
// If ctx is cancelled during ramp, we stop launching new terrains.
func (s *Swarm) ramp() error {
	n := len(s.terrains)
	if n == 0 {
		return nil
	}

	// Interval between each terrain coming online
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

		// Don't sleep after the last terrain
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

// collectResults drains the results channel until it is closed,
// aggregating LifeLogs into the swarm's metric counters and reporter.
func (s *Swarm) collectResults() {
	for log := range s.results {
		s.metrics.LemmingsCompleted.Add(1)
		s.metrics.LemmingsAlive.Add(-1)

		for _, visit := range log.Visits {
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

		if log.Error != nil {
			s.metrics.LemmingsFailed.Add(1)
		}

		// Hand log to reporter for per-path accounting
		s.reporter.Ingest(log)

		// Emit to event bus for dashboard
		s.events.Emit(Event{
			Kind:    EventLemmingDied,
			Terrain: log.Terrain,
			Pack:    log.Pack,
		})
	}
}

// tickSTDOUT prints a live one-line summary to STDOUT every second.
func (s *Swarm) tickSTDOUT() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

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

			fmt.Printf("\r  [%s] terrains: %d | alive: %s | done: %s | visits: %s | 2xx: %s 4xx: %s 5xx: %s",
				elapsed,
				terrains,
				formatInt(alive),
				formatInt(completed),
				formatInt(visits),
				formatInt(xx2),
				formatInt(xx4),
				formatInt(xx5),
			)

			// Flush stdout — important for CI environments
			os.Stdout.Sync()
		}
	}
}

// printFinalSummary writes the post-run summary block to STDOUT.
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

	fmt.Println()
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  lemmings v%s — final report\n", s.cfg.Version)
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  target:        %s\n", s.cfg.Hit)
	fmt.Printf("  total lemmings: %s\n", formatInt(total))
	fmt.Printf("  completed:     %s\n", formatInt(completed))
	fmt.Printf("  failed:        %s\n", formatInt(failed))
	fmt.Println()
	fmt.Printf("  total visits:  %s\n", formatInt(visits))
	fmt.Printf("  total bytes:   %s\n", formatBytes(bytes))
	fmt.Println()
	fmt.Printf("  2xx:           %s\n", formatInt(xx2))
	fmt.Printf("  3xx:           %s\n", formatInt(xx3))
	fmt.Printf("  4xx:           %s\n", formatInt(xx4))
	fmt.Printf("  5xx:           %s\n", formatInt(xx5))
	fmt.Println()
	fmt.Printf("  waiting room:  %s lemmings held\n", formatInt(wr))
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  report saved to: %s\n\n", s.cfg.SaveTo)
}

// Report triggers the reporter to write the .md and .html output files.
func (s *Swarm) Report() error {
	return s.reporter.Write(s.cfg.SaveTo)
}

// generateToken produces a cryptographically random hex string for
// dashboard authentication.
func generateToken() (string, error) {
	b := make([]byte, 32) // 256 bits → 64 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// formatBytes renders a byte count as a human-readable string.
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
