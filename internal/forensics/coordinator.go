package forensics

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
)

// Coordinator manages the bounded async forensics worker pool.
// Lemmings call Submit (O(1), non-blocking) on failure so slow browser captures
// never stall load-generation goroutines or the synchronous EventBus.
type Coordinator struct {
	store    *Store
	capturer Capturer
	bus      *events.Bus
	jobs     chan CaptureRequest
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	inFlight atomic.Int64
	stopOnce sync.Once
	workCtx  context.Context
	cancel   context.CancelFunc
}

// CoordinatorConfig tunes the forensics pool.
type CoordinatorConfig struct {
	Workers int
	Queue   int
}

// NewCoordinator starts a bounded worker pool draining capture requests into store.
func NewCoordinator(
	parent context.Context,
	store *Store,
	capturer Capturer,
	bus *events.Bus,
	cfg CoordinatorConfig,
) *Coordinator {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	if workers > 8 {
		workers = 8
	}
	queueSize := cfg.Queue
	if queueSize <= 0 {
		queueSize = 64
	}
	store.SetRuntime(capturer.Mode(), capturer.BrowserInfo())

	if parent == nil {
		parent = context.Background()
	}
	// Detach parent cancellation so SIGINT/SIGTERM does not immediately abort
	// in-flight or queued captures before DrainAndClose(drainCtx) drains them.
	workCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	c := &Coordinator{
		store:    store,
		capturer: capturer,
		bus:      bus,
		jobs:     make(chan CaptureRequest, queueSize),
		workCtx:  workCtx,
		cancel:   cancel,
	}

	for range workers {
		c.wg.Add(1)
		go c.worker()
	}
	return c
}

// Store returns the underlying Artifact Store.
func (c *Coordinator) Store() *Store { return c.store }

// Capturer returns the configured Capturer (for Scouts or diagnostics).
func (c *Coordinator) Capturer() Capturer { return c.capturer }

// Submit queues req for async capture if deduplication and global caps permit.
// Always returns immediately without blocking the calling lemming goroutine.
func (c *Coordinator) Submit(req CaptureRequest) bool {
	fp := Fingerprint(req.Path, req.StatusCode, req.TriggerReason)
	ok, reason := c.store.TryReserve(fp)
	if !ok {
		c.emitDrop(req, reason)
		return false
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.store.ReleaseReservation(fp, false)
		return false
	}
	select {
	case c.jobs <- req:
		c.mu.Unlock()
		return true
	default:
		c.mu.Unlock()
		c.store.ReleaseReservation(fp, true)
		c.emitDrop(req, DropQueueFull)
		return false
	}
}

func (c *Coordinator) emitDrop(req CaptureRequest, reason string) {
	if c.bus == nil {
		return
	}
	c.bus.Emit(events.Event{
		Kind:          events.ArtifactDropped,
		LemmingID:     req.Session.LemmingID,
		LemmingNumber: req.Session.LemmingNumber,
		Terrain:       req.Session.Terrain,
		Pack:          req.Session.Pack,
		Method:        req.Method,
		URL:           req.SafeURL,
		StatusCode:    req.StatusCode,
		PlanName:      req.Session.PlanName,
		Role:          req.Session.Role,
		DropReason:    reason,
	})
}

func (c *Coordinator) worker() {
	defer c.wg.Done()
	for req := range c.jobs {
		if c.workCtx.Err() != nil {
			continue
		}
		c.inFlight.Add(1)
		art, err := c.capturer.Capture(c.workCtx, req)
		c.inFlight.Add(-1)
		if err != nil {
			c.store.RecordCaptureError()
			// Still persist the HTTP/session artifact even if CDP screenshot failed,
			// so the admin never loses session state, cookie jar status, or error body.
			if art.ID == "" {
				continue
			}
		}
		summary := c.store.Save(art)
		if c.bus != nil {
			method := art.Session.Method
			if method == "" {
				method = req.Method
			}
			c.bus.Emit(events.Event{
				Kind:          events.ArtifactCaptured,
				LemmingID:     art.Session.LemmingID,
				LemmingNumber: art.Session.LemmingNumber,
				Terrain:       art.Session.Terrain,
				Pack:          art.Session.Pack,
				Method:        method,
				URL:           art.URL,
				StatusCode:    art.StatusCode,
				PlanName:      art.Session.PlanName,
				Role:          art.Session.Role,
				ArtifactID:    art.ID,
				CaptureMode:   art.CaptureMode,
				ConsoleErrors: art.ConsoleErrors,
				ReviewStatus:  string(art.ReviewStatus),
				Payload:       summary,
			})
		}
	}
}

// DrainAndClose stops accepting new jobs, waits for queued captures to finish up
// to drainCtx's deadline (cancelling stragglers afterward), joins all workers,
// and closes the underlying Capturer (shutting down Chrome cleanly).
func (c *Coordinator) DrainAndClose(drainCtx context.Context) error {
	var closeErr error
	c.stopOnce.Do(func() {
		if drainCtx == nil {
			drainCtx = context.Background()
		}
		c.mu.Lock()
		c.closed = true
		close(c.jobs)
		c.mu.Unlock()

		done := make(chan struct{})
		go func() {
			c.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-drainCtx.Done():
			c.cancel()
			<-done
		}
		c.cancel()
		closeErr = c.capturer.Close()
	})
	return closeErr
}
