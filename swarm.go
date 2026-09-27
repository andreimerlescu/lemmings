package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/sema"
)

// SwarmConfig is the single source of truth flowing from CLI into every layer.
//
// The zero value of every behaviour field means "default", so a config
// built in code only needs the fields it cares about. Call validate (NewSwarm
// does) to apply defaults and reject impossible combinations.
type SwarmConfig struct {
	Hit             string        `json:"hit"`
	Terrain         int64         `json:"terrain"`
	Pack            int64         `json:"pack"`
	Limit           int           `json:"limit"`
	Until           time.Duration `json:"until_ns"`
	Ramp            time.Duration `json:"ramp_ns"`
	Crawl           bool          `json:"crawl"`
	CrawlDepth      int           `json:"crawl_depth"`
	DashboardPort   int           `json:"-"`
	TTY             bool          `json:"-"`
	Color           bool          `json:"-"`
	Version         string        `json:"version"`
	Observe         bool          `json:"observe"`
	MetricsPort     int           `json:"-"`
	MetricsURLLabel string        `json:"metrics_url_label"`

	// Delivery settings never appear in reports: they hold credentials,
	// addresses and bucket names that a shared report must not leak.
	SaveTo   []string `json:"-"`
	SMTPHost string   `json:"-"`
	SMTPPort int      `json:"-"`
	SMTPUser string   `json:"-"`
	SMTPPass string   `json:"-"`
	SMTPFrom string   `json:"-"`

	// Behaviour of each lemming.
	ThinkMin       time.Duration `json:"think_min_ns"`       // minimum pause between pages
	ThinkMax       time.Duration `json:"think_max_ns"`       // maximum pause between pages
	RequestTimeout time.Duration `json:"request_timeout_ns"` // per request; 0 means defaultRequestTimeout
	MaxBodyBytes   int64         `json:"max_body_bytes"`     // per response; 0 means defaultMaxBodyBytes
	MaxPages       int           `json:"max_pages"`          // per lemming; 0 means until -until expires
	Navigation     string        `json:"navigation"`         // NavigationLinks or NavigationRandom
	StrictChecksum bool          `json:"strict_checksum"`    // a checksum change fails the visit
	JourneyFile    string        `json:"-"`                  // path to a -journey JSON file
	Journey        *Journey      `json:"-"`                  // loaded from JourneyFile by validate
	TraceFile      string        `json:"-"`                  // JSONL file receiving every visit

	// CI gates. A gate that trips makes the run exit with code 2.
	FailureGate    bool          `json:"failure_gate"`     // enforce MaxFailureRate
	MaxFailureRate float64       `json:"max_failure_rate"` // 0..1 share of non-cancelled visits
	P95Budget      time.Duration `json:"p95_budget_ns"`    // 0 disables the latency gate
}

// validate applies defaults and rejects configurations that cannot run.
// It loads the journey file, so it may touch the filesystem.
func (c *SwarmConfig) validate() error {
	if _, err := parseOrigin(c.Hit); err != nil {
		return fmt.Errorf("-hit %q: %w", c.Hit, err)
	}
	switch {
	case c.Terrain < 1 || c.Terrain > 10_000:
		return fmt.Errorf("-terrain must be 1..10000, got %d", c.Terrain)
	case c.Pack < 0 || c.Pack > 100_000:
		return fmt.Errorf("-pack must be 0..100000, got %d", c.Pack)
	case c.Limit == 0 || c.Limit < -1:
		return fmt.Errorf("-limit must be positive or -1, got %d", c.Limit)
	case c.Until <= 0:
		return fmt.Errorf("-until must be positive, got %s", c.Until)
	case c.ThinkMin < 0 || c.ThinkMax < 0:
		return errors.New("-think-min and -think-max must not be negative")
	case c.ThinkMax < c.ThinkMin:
		return fmt.Errorf("-think-max (%s) must be at least -think-min (%s)", c.ThinkMax, c.ThinkMin)
	case c.RequestTimeout < 0:
		return errors.New("-request-timeout must not be negative")
	case c.MaxBodyBytes < 0 || c.MaxBodyBytes > 64<<20:
		return errors.New("-max-body-bytes must be 0..64MiB")
	case c.MaxPages < 0:
		return errors.New("-max-pages must not be negative")
	case c.P95Budget < 0:
		return errors.New("-p95-budget must not be negative")
	case c.FailureGate && (math.IsNaN(c.MaxFailureRate) || c.MaxFailureRate < 0 || c.MaxFailureRate > 1):
		return fmt.Errorf("-max-failure-rate must be 0..1, got %v", c.MaxFailureRate)
	}
	switch c.Navigation {
	case "":
		c.Navigation = NavigationLinks
	case NavigationLinks, NavigationRandom:
	default:
		return fmt.Errorf("-navigation must be %q or %q, got %q", NavigationLinks, NavigationRandom, c.Navigation)
	}
	if c.JourneyFile != "" && c.Journey == nil {
		j, err := LoadJourney(c.JourneyFile, c.Hit)
		if err != nil {
			return err
		}
		c.Journey = j
	}
	return nil
}

// SwarmMetrics holds live atomic counters readable by the ticker and dashboard.
//
// Visit counters are updated the moment each visit completes; lemming
// counters when a lemming is born and dies. They are exact.
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
	FailedVisits      atomic.Int64 // failed transport, status or checks
	CancelledVisits   atomic.Int64 // cut short when a lemming's life ended
	OverflowLogs      atomic.Int64 // lifelogs that hit the overflow channel
	DroppedLogs       atomic.Int64 // lifelogs dropped when both channels full
}

// recordVisit counts one finished visit.
func (m *SwarmMetrics) recordVisit(v *Visit) {
	m.TotalVisits.Add(1)
	m.TotalBytes.Add(v.BytesIn)
	if v.WaitingRoom.Detected {
		m.TotalWaitingRoom.Add(1)
	}
	switch {
	case v.StatusCode >= 200 && v.StatusCode < 300:
		m.Total2xx.Add(1)
	case v.StatusCode >= 300 && v.StatusCode < 400:
		m.Total3xx.Add(1)
	case v.StatusCode >= 400 && v.StatusCode < 500:
		m.Total4xx.Add(1)
	case v.StatusCode >= 500:
		m.Total5xx.Add(1)
	}
	switch {
	case v.Cancelled:
		m.CancelledVisits.Add(1)
	case v.Failed:
		m.FailedVisits.Add(1)
	}
}

const (
	// primaryChanCap and overflowChanCap bound the LifeLog channels. Each
	// LifeLog carries at most lifeTailSize visits and the collector drains
	// continuously, so a few thousand slots absorb any realistic burst of
	// simultaneous deaths without preallocating memory for the whole swarm.
	primaryChanCap  = 4096
	overflowChanCap = 4096
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
	observers []Observer
}

// NewSwarm validates the config, indexes the target and wires every
// component, but does not start lemmings yet.
func NewSwarm(ctx context.Context, cfg SwarmConfig) (*Swarm, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	fail := func(err error) (*Swarm, error) {
		cancel()
		return nil, err
	}

	token, err := generateToken()
	if err != nil {
		return fail(fmt.Errorf("failed to generate dashboard token: %w", err))
	}

	limit := cfg.Limit
	if limit == -1 {
		limit = int(max(cfg.Terrain*cfg.Pack, 1))
	}
	sem, err := sema.New(limit)
	if err != nil {
		return fail(fmt.Errorf("failed to initialize semaphore: %w", err))
	}

	primarySize := int(min(cfg.Terrain*cfg.Pack, primaryChanCap))
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

	fmt.Println("  indexing origin...")
	pool, err := BuildURLPool(ctx, cfg.Hit, cfg.Crawl, cfg.CrawlDepth)
	if err != nil {
		return fail(fmt.Errorf("failed to index origin %s: %w", cfg.Hit, err))
	}
	s.pool = pool
	fmt.Printf("  indexed %d URLs from %s\n\n", len(pool.URLs), cfg.Hit)

	// The swarm counts terrains online in ramp and counts them down here,
	// when each terrain reports that its last lemming has died.
	bus.Subscribe(Filter(func(Event) { s.metrics.TerrainsOnline.Add(-1) }, EventTerrainDone))

	s.terrains = make([]*Terrain, cfg.Terrain)
	for i := int64(0); i < cfg.Terrain; i++ {
		s.terrains[i] = NewTerrain(i, cfg, pool, s.sendLifeLog, bus, sem, &s.metrics)
	}

	s.reporter = NewReporter(cfg)
	s.reporter.Attach(bus)
	if cfg.TraceFile != "" {
		trace, err := newTraceWriter(cfg.TraceFile)
		if err != nil {
			return fail(fmt.Errorf("-trace-file: %w", err))
		}
		s.reporter.trace = trace
	}
	for _, dest := range cfg.SaveTo {
		target, err := ParseTarget(dest)
		if err != nil {
			s.reporter.closeTrace()
			return fail(fmt.Errorf("invalid save-to target %q: %w", dest, err))
		}
		// Inject SMTP overrides from flags into every MailTarget
		if mt, ok := target.(*MailTarget); ok {
			applyFlagSMTPOverrides(mt, cfg)
		}
		s.reporter.AddTarget(target)
	}
	s.dashboard = NewDashboard(cfg, bus, &s.metrics, token)
	s.dashboard.stats = s.reporter

	if cfg.Observe {
		obs := NewPrometheusObserver(cfg)
		if err := s.RegisterObserver(obs); err != nil {
			s.reporter.closeTrace()
			return fail(fmt.Errorf("prometheus observer: %w", err))
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

// Token returns the raw dashboard token for printing. It is never stored
// anywhere else; the dashboard keeps only its hash.
func (s *Swarm) Token() string {
	return s.token
}

// sendLifeLog is the non-blocking death drain. Primary first, overflow second,
// drop counter third. A lemming never blocks at death.
//
// A dropped LifeLog loses only that lemming's session detail. Its visits
// were already counted as they happened.
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
	defer s.cancel()
	defer func() {
		for _, o := range s.observers {
			if err := o.Detach(); err != nil {
				log.Printf("observer %s detach error: %v", o.Name(), err)
			}
		}
	}()

	s.startedAt = time.Now()
	s.reporter.markStarted(s.startedAt)
	s.dashboard.markStarted(s.startedAt)
	s.events.Emit(Event{Kind: EventSwarmStarted})

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

	tickerDone := make(chan struct{})
	var tickerWg sync.WaitGroup
	tickerWg.Add(1)
	go func() {
		defer tickerWg.Done()
		s.tickSTDOUT(tickerDone)
	}()

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

	// Stop the live ticker before the summary so it cannot overwrite it.
	close(tickerDone)
	tickerWg.Wait()

	s.reporter.markEnded(time.Now(), &s.metrics)
	s.events.Emit(Event{Kind: EventSwarmDone})
	s.dashboard.finish()

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

		// Announce before launching so a terrain whose lemmings all fail
		// instantly can never report done before it reported online.
		s.metrics.TerrainsOnline.Add(1)
		s.events.Emit(Event{
			Kind:    EventTerrainOnline,
			Terrain: int(t.id),
		})
		t.Launch(s.ctx)

		if i < n-1 {
			timer := time.NewTimer(interval)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
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

// ingest hands a LifeLog to the reporter.
//
// LifeLogs from running lemmings are Streamed: their visits and lifecycle
// were counted as they happened, so ingest only records the session. A
// LifeLog built elsewhere (Streamed == false) is counted in full here,
// and ingest emits its EventLemmingDied, standing in for the Terrain.
func (s *Swarm) ingest(ll LifeLog) {
	if !ll.Streamed {
		s.metrics.LemmingsCompleted.Add(1)
		s.metrics.LemmingsAlive.Add(-1)
		for i := range ll.Visits {
			s.metrics.recordVisit(&ll.Visits[i])
			s.reporter.RecordVisit(&ll.Visits[i])
		}
		if ll.Error != nil {
			s.metrics.LemmingsFailed.Add(1)
		}
		s.events.Emit(Event{
			Kind:      EventLemmingDied,
			LemmingID: ll.Identity.ID,
			Terrain:   ll.Terrain,
			Pack:      ll.Pack,
			Life:      &ll,
		})
	}
	s.reporter.Ingest(ll)
}

// Report renders the report and delivers it to every target.
func (s *Swarm) Report(ctx context.Context) error {
	return s.reporter.Write(ctx)
}

// applyFlagSMTPOverrides copies non-zero flag values from SwarmConfig
// into a MailTarget's SMTPConfig, overriding environment variables.
// This is only called when the target is a *MailTarget.
func applyFlagSMTPOverrides(mt *MailTarget, cfg SwarmConfig) {
	if cfg.SMTPHost != "" {
		mt.smtpCfg.Host = cfg.SMTPHost
	}
	if cfg.SMTPPort != 0 {
		mt.smtpCfg.Port = cfg.SMTPPort
	}
	if cfg.SMTPUser != "" {
		mt.smtpCfg.User = cfg.SMTPUser
	}
	if cfg.SMTPPass != "" {
		mt.smtpCfg.Pass = cfg.SMTPPass
	}
	if cfg.SMTPFrom != "" {
		mt.smtpCfg.From = cfg.SMTPFrom
	}
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
