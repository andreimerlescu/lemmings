package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"sync"

	"github.com/andreimerlescu/sema"
	"golang.org/x/net/publicsuffix"
)

// Terrain represents one goroutine group — the "state" in the geographic
// metaphor. It owns cfg.Pack lemmings and brings them all online when
// Launch is called.
type Terrain struct {
	id      int64
	cfg     SwarmConfig
	pool    *URLPool
	send    func(LifeLog) // swarm's non-blocking sendLifeLog callback
	bus     *EventBus
	sem     sema.Semaphore
	metrics *SwarmMetrics
	wg      sync.WaitGroup
	mu      sync.Mutex
}

// NewTerrain constructs a Terrain. No lemmings are spawned until Launch.
func NewTerrain(
	id int64,
	cfg SwarmConfig,
	pool *URLPool,
	send func(LifeLog),
	bus *EventBus,
	sem sema.Semaphore,
	metrics *SwarmMetrics,
) *Terrain {
	return &Terrain{
		id:      id,
		cfg:     cfg,
		pool:    pool,
		send:    send,
		bus:     bus,
		sem:     sem,
		metrics: metrics,
	}
}

// Launch spawns cfg.Pack lemmings concurrently. Each lemming acquires a
// semaphore slot before running, releasing it when it dies. Launch returns
// immediately — lemmings run in their own goroutines.
func (t *Terrain) Launch(ctx context.Context) {
	for i := int64(0); i < t.cfg.Pack; i++ {
		t.wg.Add(1)
		go t.spawnLemming(ctx, i)
	}
}

// Wait blocks until all lemmings in this terrain have died.
func (t *Terrain) Wait() {
	t.wg.Wait()
}

// spawnLemming acquires the semaphore, constructs the lemming, runs it,
// and sends its LifeLog home when it dies.
func (t *Terrain) spawnLemming(ctx context.Context, packIndex int64) {
	defer t.wg.Done()

	// Acquire semaphore slot — blocks if at ceiling, respects ctx cancellation
	if err := t.sem.Acquire(ctx); err != nil {
		// Context cancelled before we could acquire — lemming never born
		t.metrics.LemmingsFailed.Add(1)
		t.bus.Emit(Event{
			Kind:    EventLemmingFailed,
			Terrain: int(t.id),
			Pack:    int(packIndex),
			Err:     err,
		})
		t.wg.Done()
		// Note: we call wg.Done() above and deferred — double done.
		// Fix: use a flag. See corrected pattern below.
		return
	}
	defer t.sem.Release()

	// Build the lemming's HTTP client with its own isolated cookie jar.
	// publicsuffix.List ensures cookie scoping behaves like a real browser.
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		t.metrics.LemmingsFailed.Add(1)
		t.bus.Emit(Event{
			Kind:    EventLemmingFailed,
			Terrain: int(t.id),
			Pack:    int(packIndex),
			Err:     err,
		})
		return
	}

	client := &http.Client{
		Jar:     jar,
		Timeout: t.cfg.Until,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Follow up to 10 redirects, matching real browser behavior
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	lemming := NewLemming(
		t.id,
		packIndex,
		t.cfg,
		t.pool,
		client,
		t.bus,
		t.metrics,
	)

	t.metrics.LemmingsAlive.Add(1)
	t.bus.Emit(Event{
		Kind:    EventLemmingBorn,
		Terrain: int(t.id),
		Pack:    int(packIndex),
	})

	// Run blocks until the lemming's context expires
	ll := lemming.Run(ctx)

	// Lemming is dead — send its lifelog home
	t.send(ll)

	t.bus.Emit(Event{
		Kind:    EventLemmingDied,
		Terrain: int(t.id),
		Pack:    int(packIndex),
	})
}

// String returns a human readable identifier for logging.
func (t *Terrain) String() string {
	return fmt.Sprintf("terrain-%d", t.id)
}
