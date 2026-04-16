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
//
// Semaphore discipline: Release is only registered as a deferred call
// AFTER Acquire succeeds. If Acquire fails for any reason (including
// context cancellation or deadline), the slot was never held, so we MUST
// NOT call Release — doing so would return ErrReleaseExceedsCount and,
// worse, any successful Release would corrupt the semaphore count by
// freeing a slot that another goroutine legitimately held. This was the
// root cause of TestTerrain_Launch_SemaphoreRespected failing with
// max concurrent=4 under a limit=2 semaphore.
func (t *Terrain) spawnLemming(ctx context.Context, packIndex int64) {
	defer t.wg.Done() // handles ALL exit paths cleanly

	// Acquire using the parent context directly. Any error means the
	// slot was never held. The previous implementation wrapped ctx in
	// a 1-second WithTimeout and only returned for ErrAcquireCancelled,
	// allowing other error types to fall through to Release — corrupting
	// the semaphore. Treat every acquire error as terminal failure.
	if err := t.sem.AcquireWith(ctx); err != nil {
		t.metrics.LemmingsFailed.Add(1)
		t.bus.Emit(Event{
			Kind:    EventLemmingFailed,
			Terrain: int(t.id),
			Pack:    int(packIndex),
			Err:     err,
		})
		return
	}

	// Slot is held. Register Release now, and only now. The error from
	// Release is discarded on this defer — in normal operation it will
	// always be nil because we hold exactly one slot. A non-nil return
	// indicates a logic bug elsewhere in the package.
	defer func() {
		_ = t.sem.Release()
	}()

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
