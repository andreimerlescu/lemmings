// Package events implements the synchronous, lock-free-emit EventBus and
// bounded ring-buffer EventLog that connect every Lemmings bounded context.
package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// Kind identifies what happened in the domain. Typed as a string so event logs
// and SSE streams remain human-readable without a lookup table.
type Kind string

const (
	// Lemming lifecycle — owned exclusively by Terrain.
	LemmingBorn   Kind = "lemming.born"
	LemmingDied   Kind = "lemming.died"
	LemmingFailed Kind = "lemming.failed"

	// Visit lifecycle. VisitComplete is a visit that passed every check;
	// VisitError is a visit that failed or was cancelled.
	VisitComplete Kind = "visit.complete"
	VisitError    Kind = "visit.error"

	// Asset lifecycle. AssetComplete is a same-origin subresource fetch that
	// succeeded; AssetError is an asset fetch that failed or was denied.
	AssetComplete Kind = "asset.complete"
	AssetError    Kind = "asset.error"

	// Waiting room lifecycle.
	WaitingRoomQueued Kind = "waiting_room.queued"
	WaitingRoom       Kind = "waiting_room.entered"
	QueuePoll         Kind = "waiting_room.poll"

	// Terrain lifecycle.
	TerrainOnline Kind = "terrain.online"
	TerrainDone   Kind = "terrain.done"

	// Channel pressure.
	LogOverflow Kind = "log.overflow"
	LogDropped  Kind = "log.dropped"

	// Swarm lifecycle.
	SwarmStarted Kind = "swarm.started"
	SwarmDone    Kind = "swarm.done"

	// Dashboard authentication audit.
	DashboardAuth Kind = "dashboard.auth"

	// Error forensics & browser Scout lifecycle.
	ArtifactCaptured Kind = "artifact.captured"
	ArtifactReviewed Kind = "artifact.reviewed"
	ArtifactDropped  Kind = "artifact.dropped"
	ScoutStarted     Kind = "scout.started"
	ScoutVisit       Kind = "scout.visit"
	ScoutDone        Kind = "scout.done"
)

// Event is the unit of communication on the EventBus.
//
// Fields are populated depending on Kind; zero values are safe to ignore.
// Scalar fields allow metric and log subscribers to inspect events without
// dereferencing Payload.
//
// Ownership contract: subscribers must not retain references to Payload after
// their callback returns. Copy any fields you need.
type Event struct {
	Kind       Kind      `json:"kind"`
	OccurredAt time.Time `json:"occurred_at"`

	// Lemming identity.
	LemmingID     string `json:"lemming_id,omitempty"`
	LemmingNumber int64  `json:"lemming_number,omitempty"`
	Terrain       int    `json:"terrain"`
	Pack          int    `json:"pack"`

	// Visit summary fields.
	URL        string        `json:"url,omitempty"`
	StatusCode int           `json:"status_code,omitempty"`
	BytesIn    int64         `json:"bytes_in,omitempty"`
	Duration   time.Duration `json:"duration_ns,omitempty"`
	Failed     bool          `json:"failed,omitempty"`
	Cancelled  bool          `json:"cancelled,omitempty"`
	AssetClass string        `json:"asset_class,omitempty"`
	Banned     bool          `json:"banned,omitempty"`
	AdmitPass  bool          `json:"admit_pass,omitempty"`

	// Waiting room queue position and polling count.
	Position   int `json:"position,omitempty"`
	QueuePolls int `json:"queue_polls,omitempty"`

	// Forensics summary fields.
	ArtifactID    string `json:"artifact_id,omitempty"`
	CaptureMode   string `json:"capture_mode,omitempty"`
	DropReason    string `json:"drop_reason,omitempty"`
	ConsoleErrors int    `json:"console_errors,omitempty"`
	ReviewStatus  string `json:"review_status,omitempty"`

	// Payload carries optional bounded-context detail (e.g., *swarm.Visit,
	// *swarm.Identity, *swarm.LifeLog, *forensics.Summary) for in-process
	// subscribers. Never JSON-marshaled and cleared by EventLog.Record.
	Payload any `json:"-"`

	// Err is non-nil on failure events and failed dashboard auth attempts.
	Err error `json:"-"`
}

// Subscriber receives events from an EventBus.
// Subscribers run synchronously on the emitter's goroutine and must be O(1)
// and non-blocking. Hand slow work (disk, network, CDP) to an owned worker.
type Subscriber func(Event)

// busView is an immutable snapshot of subscribers published for lock-free Emit.
type busView struct {
	subscribers []Subscriber
	closed      bool
}

// Bus is a synchronous fan-out event dispatcher safe for concurrent use.
// Emit is lock-free and allocation-free via atomic.Pointer[busView].
type Bus struct {
	mu          sync.Mutex
	subscribers []Subscriber
	closed      bool
	view        atomic.Pointer[busView]
}

// NewBus constructs an empty Bus ready to accept subscribers.
func NewBus() *Bus {
	b := &Bus{
		subscribers: make([]Subscriber, 0, 8),
	}
	b.publishLocked()
	return b
}

func (b *Bus) publishLocked() {
	b.view.Store(&busView{
		subscribers: append([]Subscriber(nil), b.subscribers...),
		closed:      b.closed,
	})
}

// Subscribe registers fn to receive all future events and returns an idempotent
// unsubscribe function. Subscribing nil or subscribing to a closed bus returns
// a no-op unsubscribe function.
func (b *Bus) Subscribe(fn Subscriber) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed || fn == nil {
		return func() {}
	}
	b.subscribers = append(b.subscribers, fn)
	idx := len(b.subscribers) - 1
	b.publishLocked()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if idx < len(b.subscribers) && b.subscribers[idx] != nil {
			b.subscribers[idx] = nil
			b.publishLocked()
		}
	}
}

// Emit dispatches e synchronously to all active subscribers in registration order.
func (b *Bus) Emit(e Event) {
	v := b.view.Load()
	if v.closed {
		return
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	for _, fn := range v.subscribers {
		if fn != nil {
			fn(e)
		}
	}
}

// Close marks the bus closed so subsequent Emit calls are no-ops. Idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.publishLocked()
}

// Filter returns a Subscriber that invokes fn only when e.Kind is in kinds.
func Filter(fn Subscriber, kinds ...Kind) Subscriber {
	allowed := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		allowed[k] = true
	}
	return func(e Event) {
		if allowed[e.Kind] {
			fn(e)
		}
	}
}

// Tee returns a Subscriber that forwards each event to all non-nil subscribers.
func Tee(subscribers ...Subscriber) Subscriber {
	return func(e Event) {
		for _, fn := range subscribers {
			if fn != nil {
				fn(e)
			}
		}
	}
}

// Log is a concurrency-safe, fixed-capacity ring buffer of recent events.
type Log struct {
	mu     sync.RWMutex
	events []Event
	start  int
	cap    int
}

// NewLog constructs a Log holding at most capacity events.
func NewLog(capacity int) *Log {
	if capacity < 0 {
		capacity = 0
	}
	return &Log{
		events: make([]Event, 0, capacity),
		cap:    capacity,
	}
}

// Record appends e to the ring buffer, clearing Payload so large objects are never pinned.
func (l *Log) Record(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cap == 0 {
		return
	}
	e.Payload = nil
	if len(l.events) < l.cap {
		l.events = append(l.events, e)
		return
	}
	l.events[l.start] = e
	l.start = (l.start + 1) % l.cap
}

// Snapshot returns an independent copy of recorded events, oldest first.
func (l *Log) Snapshot() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]Event, 0, len(l.events))
	out = append(out, l.events[l.start:]...)
	out = append(out, l.events[:l.start]...)
	return out
}

// AsSubscriber adapts l into a Bus Subscriber.
func (l *Log) AsSubscriber() Subscriber {
	return func(e Event) {
		l.Record(e)
	}
}
