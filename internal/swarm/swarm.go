package swarm

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"github.com/andreimerlescu/sema"
)

// Metrics holds exact live atomic counters readable by the CLI ticker,
// dashboard, and reporters.
type Metrics struct {
	LemmingsStarted    atomic.Int64
	LemmingsAlive      atomic.Int64
	LemmingsCompleted  atomic.Int64
	LemmingsFailed     atomic.Int64
	TerrainsOnline     atomic.Int64
	TotalVisits        atomic.Int64
	TotalBytes         atomic.Int64
	TotalWaitingRoom   atomic.Int64
	Total2xx           atomic.Int64
	Total3xx           atomic.Int64
	Total4xx           atomic.Int64
	Total5xx           atomic.Int64
	FailedVisits       atomic.Int64
	CancelledVisits    atomic.Int64
	OverflowLogs       atomic.Int64
	DroppedLogs        atomic.Int64
	ArtifactsCaptured  atomic.Int64
	ScoutsAlive        atomic.Int64
	AssetRequests      atomic.Int64
	AssetFailed        atomic.Int64
	AssetBytes         atomic.Int64
	AssetDenied403     atomic.Int64
	QueuePolls         atomic.Int64
	Bans429            atomic.Int64
	Bans403            atomic.Int64
	ScoutVisits        atomic.Int64
	ScoutFailures      atomic.Int64
	PlanLemmingsPassed atomic.Int64
	PlanLemmingsFailed atomic.Int64
	RogueLemmings      atomic.Int64
}

func (m *Metrics) recordDeath(ll *LifeLog) {
	if m == nil || ll == nil {
		return
	}
	role := ll.Role
	if role == "" {
		role = ll.Identity.Role
	}
	switch role {
	case RolePlan:
		if ll.PlanPassed {
			m.PlanLemmingsPassed.Add(1)
		} else {
			m.PlanLemmingsFailed.Add(1)
		}
	case RoleRogue:
		m.RogueLemmings.Add(1)
	}
}

// RecordVisit counts one completed visit atomically.
// Cancelled visits increment TotalVisits, TotalBytes, TotalWaitingRoom (when
// detected), and CancelledVisits, and skip HTTP status classes so partial
// shutdown reads never distort status distributions.
func (m *Metrics) RecordVisit(v *Visit) {
	if v == nil {
		return
	}
	m.TotalVisits.Add(1)
	m.TotalBytes.Add(v.BytesIn)
	if v.WaitingRoom.Detected {
		m.TotalWaitingRoom.Add(1)
	}
	if v.Cancelled {
		m.CancelledVisits.Add(1)
		return
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
	if v.Failed {
		m.FailedVisits.Add(1)
	}
	if v.Banned {
		switch v.StatusCode {
		case http.StatusTooManyRequests:
			m.Bans429.Add(1)
		case http.StatusForbidden:
			m.Bans403.Add(1)
		}
	}
}

// RecordAsset counts one completed same-origin asset fetch atomically.
func (m *Metrics) RecordAsset(a *AssetVisit) {
	if a == nil {
		return
	}
	m.AssetRequests.Add(1)
	m.AssetBytes.Add(a.BytesIn)
	if a.Failed {
		m.AssetFailed.Add(1)
	}
	if a.Denied403 || a.StatusCode == http.StatusForbidden {
		m.AssetDenied403.Add(1)
	}
}

// Observer receives live swarm events on the EventBus (e.g. PrometheusObserver).
type Observer interface {
	Attach(ctx context.Context, bus *events.Bus) error
	Detach() error
	Name() string
}

// LifeSink consumes completed LifeLogs drained from the collector channels.
type LifeSink interface {
	Ingest(ll LifeLog)
}

const (
	primaryChanCap  = 4096
	overflowChanCap = 4096
)

// Options provides dependency injection hooks for constructing a Swarm.
type Options struct {
	Bus        *events.Bus
	Pool       *discovery.URLPool
	Forensics  *forensics.Coordinator
	LifeSink   LifeSink
	OnProgress func(msg string)
}

// Swarm is the top-level load simulation coordinator. It owns terrains, the
// dual-channel LifeLog collector, ramp scheduling, optional browser Scouts,
// and attached Observers. It intentionally holds no dashboard secret token (SEC-05).
type Swarm struct {
	cfg        Config
	pool       *discovery.URLPool
	terrains   []*Terrain
	primary    chan LifeLog
	overflow   chan LifeLog
	events     *events.Bus
	metrics    Metrics
	sema       sema.Semaphore
	observers  []Observer
	lifeSink   LifeSink
	forensics  *forensics.Coordinator
	fpKey      []byte
	nextNumber atomic.Int64
	onProgress func(string)
	startedAt  time.Time
}

// New validates cfg, indexes the target URLPool (unless supplied via opts.Pool),
// and wires all terrains. No lemmings move until Run is called.
func New(ctx context.Context, cfg Config, opts Options) (*Swarm, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	limit := cfg.Limit
	if limit == -1 {
		limit = int(max(cfg.Terrain*cfg.Pack, 1))
	}
	sem, err := sema.New(limit)
	if err != nil {
		return nil, fmt.Errorf("initialize semaphore: %w", err)
	}

	fpKey, err := secrets.NewEphemeralKey()
	if err != nil {
		return nil, err
	}

	bus := opts.Bus
	if bus == nil {
		bus = events.NewBus()
	}

	pool := opts.Pool
	if pool == nil {
		if opts.OnProgress != nil {
			opts.OnProgress("indexing origin...")
		}
		pool, err = discovery.BuildURLPool(ctx, cfg.Hit, discovery.BuildOptions{
			Crawl:      cfg.Crawl,
			CrawlDepth: cfg.CrawlDepth,
			OnProgress: opts.OnProgress,
		})
		if err != nil {
			return nil, fmt.Errorf("index origin %s: %w", discovery.SafeURL(cfg.Hit), err)
		}
		if opts.OnProgress != nil {
			opts.OnProgress(fmt.Sprintf("indexed %d URLs from %s", len(pool.URLs), discovery.SafeURL(cfg.Hit)))
		}
	}

	primarySize := int(min(cfg.Terrain*cfg.Pack, primaryChanCap))
	if primarySize < 1 {
		primarySize = 1
	}

	s := &Swarm{
		cfg:        cfg,
		pool:       pool,
		primary:    make(chan LifeLog, primarySize),
		overflow:   make(chan LifeLog, overflowChanCap),
		events:     bus,
		sema:       sem,
		lifeSink:   opts.LifeSink,
		forensics:  opts.Forensics,
		fpKey:      fpKey,
		onProgress: opts.OnProgress,
	}

	bus.Subscribe(events.Filter(func(events.Event) {
		s.metrics.TerrainsOnline.Add(-1)
	}, events.TerrainDone))
	bus.Subscribe(events.Filter(func(events.Event) {
		s.metrics.ArtifactsCaptured.Add(1)
	}, events.ArtifactCaptured))
	bus.Subscribe(events.Filter(func(e events.Event) {
		if e.Kind == events.ScoutStarted {
			s.metrics.ScoutsAlive.Add(1)
		} else {
			s.metrics.ScoutsAlive.Add(-1)
		}
	}, events.ScoutStarted, events.ScoutDone))
	bus.Subscribe(events.Filter(func(events.Event) {
		s.metrics.QueuePolls.Add(1)
	}, events.QueuePoll))
	bus.Subscribe(events.Filter(func(e events.Event) {
		s.metrics.ScoutVisits.Add(1)
		if e.Failed {
			s.metrics.ScoutFailures.Add(1)
		}
	}, events.ScoutVisit))

	s.terrains = make([]*Terrain, cfg.Terrain)
	for i := int64(0); i < cfg.Terrain; i++ {
		s.terrains[i] = NewTerrain(
			i, cfg, pool, s.SendLifeLog, bus, sem, &s.metrics, opts.Forensics, s.NextLemmingNumber, fpKey,
		)
	}
	return s, nil
}

// NextLemmingNumber atomically allocates the next sequential global lemming number (#1..N).
func (s *Swarm) NextLemmingNumber() int64 { return s.nextNumber.Add(1) }

// Config returns the validated Swarm Config.
func (s *Swarm) Config() Config { return s.cfg }

// Pool returns the indexed URLPool.
func (s *Swarm) Pool() *discovery.URLPool { return s.pool }

// Bus returns the Swarm's EventBus.
func (s *Swarm) Bus() *events.Bus { return s.events }

// Metrics returns a pointer to the Swarm's atomic Metrics.
func (s *Swarm) Metrics() *Metrics { return &s.metrics }

// StartedAt returns when Run began.
func (s *Swarm) StartedAt() time.Time { return s.startedAt }

// RegisterObserver attaches o to the EventBus and tracks it for automatic Detach.
func (s *Swarm) RegisterObserver(ctx context.Context, o Observer) error {
	if err := o.Attach(ctx, s.events); err != nil {
		return fmt.Errorf("observer %s attach: %w", o.Name(), err)
	}
	s.observers = append(s.observers, o)
	return nil
}

// SendLifeLog is the non-blocking death drain: primary -> overflow -> drop counter.
func (s *Swarm) SendLifeLog(ll LifeLog) {
	select {
	case s.primary <- ll:
	default:
		select {
		case s.overflow <- ll:
			s.metrics.OverflowLogs.Add(1)
			s.events.Emit(events.Event{Kind: events.LogOverflow})
		default:
			s.metrics.DroppedLogs.Add(1)
			s.events.Emit(events.Event{Kind: events.LogDropped})
		}
	}
}

// Run launches terrains according to cfg.RampShape, starts optional browser
// Scouts, drains all LifeLogs, and joins every owned goroutine before returning.
func (s *Swarm) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		for _, o := range s.observers {
			_ = o.Detach()
		}
	}()

	s.startedAt = time.Now()
	s.events.Emit(events.Event{Kind: events.SwarmStarted, OccurredAt: s.startedAt})

	var collectorWg sync.WaitGroup
	collectorWg.Add(1)
	go func() {
		defer collectorWg.Done()
		s.collectResults()
	}()

	// Start optional real-browser Scouts concurrently alongside the HTTP swarm.
	var scoutWg sync.WaitGroup
	if s.cfg.Scouts > 0 && s.forensics != nil {
		if cdp, ok := s.forensics.Capturer().(*forensics.CDPCapturer); ok {
			scoutWg.Add(1)
			go func() {
				defer scoutWg.Done()
				forensics.RunScouts(runCtx, forensics.ScoutConfig{
					Count:          s.cfg.Scouts,
					Origin:         s.pool.Origin,
					Hit:            s.cfg.Hit,
					URLs:           s.pool.URLs,
					Plan:           s.cfg.Plan,
					Until:          s.cfg.Until,
					ThinkMin:       s.cfg.ThinkMin,
					ThinkMax:       s.cfg.ThinkMax,
					NextNumber:     s.NextLemmingNumber,
					FingerprintKey: s.fpKey,
				}, cdp, s.forensics.Store(), s.events)
			}()
		}
	}

	if err := s.ramp(runCtx); err != nil {
		cancel()
		scoutWg.Wait()
		close(s.primary)
		collectorWg.Wait()
		return fmt.Errorf("ramp error: %w", err)
	}

	var terrainWg sync.WaitGroup
	for _, t := range s.terrains {
		terrainWg.Add(1)
		go func(tr *Terrain) {
			defer terrainWg.Done()
			tr.Wait()
		}(t)
	}
	terrainWg.Wait()
	scoutWg.Wait()

	close(s.primary)
	collectorWg.Wait()

	s.events.Emit(events.Event{Kind: events.SwarmDone})
	return nil
}

func (s *Swarm) ramp(ctx context.Context) error {
	n := len(s.terrains)
	if n == 0 {
		return nil
	}
	delays := RampDelays(n, s.cfg.Ramp, s.cfg.RampShape)
	if s.onProgress != nil {
		s.onProgress(fmt.Sprintf("ramping %d terrain groups over %s (%s)", n, s.cfg.Ramp, s.cfg.RampShape))
	}

	for i, t := range s.terrains {
		select {
		case <-ctx.Done():
			if s.onProgress != nil {
				s.onProgress(fmt.Sprintf("ramp cancelled after %d/%d terrains", i, n))
			}
			return nil
		default:
		}

		s.metrics.TerrainsOnline.Add(1)
		s.events.Emit(events.Event{
			Kind:    events.TerrainOnline,
			Terrain: int(t.id),
		})
		t.Launch(ctx)

		if i < len(delays) && delays[i] > 0 {
			timer := time.NewTimer(delays[i])
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}
	return nil
}

func (s *Swarm) collectResults() {
	for {
		select {
		case ll, ok := <-s.primary:
			if !ok {
				s.drainOverflow()
				return
			}
			s.ingest(ll)
		case ll := <-s.overflow:
			s.ingest(ll)
		}
	}
}

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

func (s *Swarm) ingest(ll LifeLog) {
	if !ll.Streamed {
		s.metrics.LemmingsCompleted.Add(1)
		s.metrics.LemmingsAlive.Add(-1)
		for i := range ll.Visits {
			s.metrics.RecordVisit(&ll.Visits[i])
		}
		if ll.Error != nil {
			s.metrics.LemmingsFailed.Add(1)
		}
		s.metrics.recordDeath(&ll)
		role := ll.Role
		if role == "" {
			role = ll.Identity.Role
		}
		planName := ll.PlanName
		if planName == "" {
			planName = ll.Identity.PlanName
		}
		s.events.Emit(events.Event{
			Kind:          events.LemmingDied,
			LemmingID:     ll.Identity.ID,
			LemmingNumber: ll.Identity.Number,
			Terrain:       ll.Terrain,
			Pack:          ll.Pack,
			Duration:      ll.Duration,
			Role:          role,
			PlanName:      planName,
			PlanPassed:    ll.PlanPassed,
			Payload:       &ll,
		})
	}
	if s.lifeSink != nil {
		s.lifeSink.Ingest(ll)
	}
}
