package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"time"

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

// Launch brings cfg.Pack lemmings to life and returns immediately.
//
// One scheduler goroutine per terrain acquires a semaphore slot before
// each lemming is created, so lemmings waiting for a slot cost no
// goroutine and no memory — only lemmings that hold a slot exist. When
// every lemming has died the terrain emits EventTerrainDone.
//
// If ctx ends before a slot is acquired, every lemming not yet born is
// counted in LemmingsFailed with one EventLemmingFailed each.
func (t *Terrain) Launch(ctx context.Context) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		var lemmings sync.WaitGroup
		for i := int64(0); i < t.cfg.Pack; i++ {
			if err := t.sem.AcquireWith(ctx); err != nil {
				t.failUnborn(i, err)
				break
			}
			lemmings.Add(1)
			go func(packIndex int64) {
				defer lemmings.Done()
				t.live(ctx, packIndex)
			}(i)
		}
		lemmings.Wait()
		t.bus.Emit(Event{Kind: EventTerrainDone, Terrain: int(t.id)})
	}()
}

// Wait blocks until all lemmings in this terrain have died.
func (t *Terrain) Wait() {
	t.wg.Wait()
}

// failUnborn records every lemming from packIndex onwards as failed to
// start. The semaphore slot was never held, so nothing is released.
func (t *Terrain) failUnborn(from int64, err error) {
	for i := from; i < t.cfg.Pack; i++ {
		t.metrics.LemmingsFailed.Add(1)
		t.bus.Emit(Event{
			Kind:    EventLemmingFailed,
			Terrain: int(t.id),
			Pack:    int(i),
			Err:     err,
		})
	}
}

// spawnLemming acquires the semaphore and then lives one lemming's life.
// Launch schedules lemmings itself; spawnLemming exists for callers that
// manage their own goroutines and must call t.wg.Add(1) first.
//
// Semaphore discipline: Release is only registered AFTER Acquire succeeds.
// If Acquire fails, the slot was never held, so calling Release would
// corrupt the semaphore count by freeing a slot another goroutine holds.
func (t *Terrain) spawnLemming(ctx context.Context, packIndex int64) {
	defer t.wg.Done()
	if err := t.sem.AcquireWith(ctx); err != nil {
		t.failUnborn(packIndex, err)
		return
	}
	t.live(ctx, packIndex)
}

// live runs one lemming whose semaphore slot is already held, then
// releases the slot. It owns the lemming lifecycle contract: exactly one
// EventLemmingBorn and one EventLemmingDied (or one EventLemmingFailed)
// per lemming, emitted from here and nowhere else. EventLemmingDied is
// emitted before the slot is released, so a successor's birth can never
// be observed before its predecessor's death.
func (t *Terrain) live(ctx context.Context, packIndex int64) {
	defer func() {
		// In normal operation Release always succeeds because exactly one
		// slot is held. A non-nil error indicates a logic bug elsewhere.
		_ = t.sem.Release()
	}()

	client, err := t.newClient()
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
	defer client.CloseIdleConnections()

	lemming := NewLemming(t.id, packIndex, t.cfg, t.pool, client, t.bus, t.metrics)

	t.metrics.LemmingsAlive.Add(1)
	t.bus.Emit(Event{
		Kind:      EventLemmingBorn,
		LemmingID: lemming.identity.ID,
		Terrain:   int(t.id),
		Pack:      int(packIndex),
		Identity:  &lemming.identity,
	})

	ll := lemming.Run(ctx)

	t.metrics.LemmingsAlive.Add(-1)
	t.metrics.LemmingsCompleted.Add(1)
	t.send(ll)

	t.bus.Emit(Event{
		Kind:      EventLemmingDied,
		LemmingID: lemming.identity.ID,
		Terrain:   int(t.id),
		Pack:      int(packIndex),
		Duration:  ll.Duration,
		Life:      &ll,
	})
}

// newClient builds the lemming's private HTTP client: its own cookie jar
// and its own connection pool, exactly like a separate browser. Keep-alive
// connections are reused across the lemming's visits and closed when it
// dies, so TLS handshakes and connection setup cost what they would cost
// for real visitors.
func (t *Terrain) newClient() (*http.Client, error) {
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		return nil, err
	}
	timeout := t.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &http.Client{
		Jar:       jar,
		Timeout:   timeout,
		Transport: newLemmingTransport(),
	}, nil
}

// newLemmingTransport returns a transport sized like one browser: up to
// six connections per host, HTTP/2 when the server offers it.
func newLemmingTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          6,
		MaxIdleConnsPerHost:   6,
		MaxConnsPerHost:       6,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// String returns a human readable identifier for logging.
func (t *Terrain) String() string {
	return fmt.Sprintf("terrain-%d", t.id)
}
