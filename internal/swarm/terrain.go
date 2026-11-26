package swarm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/sema"
)

// Terrain represents one goroutine group ("region/state") owning cfg.Pack lemmings.
// Terrain is the EXCLUSIVE owner of the lemming lifecycle event contract:
// exactly one LemmingBorn and one LemmingDied (or one LemmingFailed) per lemming.
type Terrain struct {
	id         int64
	cfg        Config
	pool       *discovery.URLPool
	send       func(LifeLog)
	bus        *events.Bus
	sem        sema.Semaphore
	metrics    *Metrics
	forensics  *forensics.Coordinator
	nextNumber func() int64
	fpKey      []byte
	wg         sync.WaitGroup
}

// NewTerrain constructs a Terrain. No lemmings are spawned until Launch.
func NewTerrain(
	id int64,
	cfg Config,
	pool *discovery.URLPool,
	send func(LifeLog),
	bus *events.Bus,
	sem sema.Semaphore,
	metrics *Metrics,
	coord *forensics.Coordinator,
	nextNumber func() int64,
	fpKey []byte,
) *Terrain {
	return &Terrain{
		id:         id,
		cfg:        cfg,
		pool:       pool,
		send:       send,
		bus:        bus,
		sem:        sem,
		metrics:    metrics,
		forensics:  coord,
		nextNumber: nextNumber,
		fpKey:      fpKey,
	}
}

// Launch brings cfg.Pack lemmings online behind the shared semaphore and returns immediately.
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
		if t.bus != nil {
			t.bus.Emit(events.Event{Kind: events.TerrainDone, Terrain: int(t.id)})
		}
	}()
}

// Wait blocks until all lemmings in this terrain have died and emitted TerrainDone.
func (t *Terrain) Wait() {
	t.wg.Wait()
}

func (t *Terrain) failUnborn(from int64, err error) {
	for i := from; i < t.cfg.Pack; i++ {
		if t.metrics != nil {
			t.metrics.LemmingsFailed.Add(1)
		}
		if t.bus != nil {
			t.bus.Emit(events.Event{
				Kind:    events.LemmingFailed,
				Terrain: int(t.id),
				Pack:    int(i),
				Err:     err,
			})
		}
	}
}

func (t *Terrain) live(ctx context.Context, packIndex int64) {
	defer func() {
		_ = t.sem.Release() // slot acquired by Launch
	}()

	client, jar, err := t.newClient()
	if err != nil {
		if t.metrics != nil {
			t.metrics.LemmingsFailed.Add(1)
		}
		if t.bus != nil {
			t.bus.Emit(events.Event{
				Kind:    events.LemmingFailed,
				Terrain: int(t.id),
				Pack:    int(packIndex),
				Err:     err,
			})
		}
		return
	}
	defer client.CloseIdleConnections()

	var number int64
	if t.nextNumber != nil {
		number = t.nextNumber()
	} else {
		number = t.id*max(t.cfg.Pack, 1) + packIndex + 1
	}

	lemming := NewLemming(number, t.id, packIndex, t.cfg, t.pool, client, jar, t.bus, t.metrics, t.forensics)
	ident := lemming.Identity()

	if t.metrics != nil {
		t.metrics.LemmingsStarted.Add(1)
		t.metrics.LemmingsAlive.Add(1)
	}
	if t.bus != nil {
		t.bus.Emit(events.Event{
			Kind:          events.LemmingBorn,
			LemmingID:     ident.ID,
			LemmingNumber: ident.Number,
			Terrain:       int(t.id),
			Pack:          int(packIndex),
			Role:          ident.Role,
			PlanName:      ident.PlanName,
			Payload:       &ident,
		})
	}

	ll := lemming.Run(ctx)

	if t.metrics != nil {
		t.metrics.LemmingsAlive.Add(-1)
		t.metrics.LemmingsCompleted.Add(1)
		t.metrics.recordDeath(&ll)
	}
	if t.send != nil {
		t.send(ll)
	}
	if t.bus != nil {
		t.bus.Emit(events.Event{
			Kind:          events.LemmingDied,
			LemmingID:     ident.ID,
			LemmingNumber: ident.Number,
			Terrain:       int(t.id),
			Pack:          int(packIndex),
			Duration:      ll.Duration,
			Role:          ll.Role,
			PlanName:      ll.PlanName,
			PlanPassed:    ll.PlanPassed,
			Payload:       &ll,
		})
	}
}

func (t *Terrain) newClient() (*http.Client, *InspectingJar, error) {
	jar, err := NewInspectingJar(t.fpKey)
	if err != nil {
		return nil, nil, err
	}
	timeout := t.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	return &http.Client{
		Jar:       jar,
		Timeout:   timeout,
		Transport: newLemmingTransport(),
	}, jar, nil
}

func newLemmingTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: discovery.SafeDialContext(&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}),
		TLSClientConfig:       discovery.MinTLSConfig(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          6,
		MaxIdleConnsPerHost:   6,
		MaxConnsPerHost:       6,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// String returns a human-readable identifier for this terrain.
func (t *Terrain) String() string {
	return fmt.Sprintf("terrain-%d", t.id)
}
