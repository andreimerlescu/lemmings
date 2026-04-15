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
	defer t.wg.Done() // handles ALL exit paths cleanly

	if err := t.sem.Acquire(ctx); err != nil {
		t.metrics.LemmingsFailed.Add(1)
		t.bus.Emit(Event{
			Kind:    EventLemmingFailed,
			Terrain: int(t.id),
			Pack:    int(packIndex),
			Err:     err,
		})
		return // defer fires here cleanly
	}
	defer t.sem.Release()

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
		return // defer fires here cleanly
	}

	client := &http.Client{
		Jar:     jar,
		Timeout: t.cfg.Until,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
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

	ll := lemming.Run(ctx)
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
