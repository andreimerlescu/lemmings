package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// EventKind identifies what happened. Typed as a string so event logs
// are human-readable without a lookup table.
type EventKind string

const (
	// Lemming lifecycle
	EventLemmingBorn   EventKind = "lemming.born"
	EventLemmingDied   EventKind = "lemming.died"
	EventLemmingFailed EventKind = "lemming.failed"

	// Visit lifecycle. EventVisitComplete is a visit that passed every
	// check; EventVisitError is a visit that failed or was cancelled.
	// Both carry the full *Visit.
	EventVisitComplete EventKind = "visit.complete"
	EventVisitError    EventKind = "visit.error"

	// Waiting room. EventWaitingRoomQueued fires when a lemming is first
	// queued and again whenever its Position changes. EventWaitingRoom
	// fires exactly once when the stay ends — admitted or died in line —
	// carrying the total Duration held. (Its "entered" name predates the
	// queued event; it is kept so existing consumers keep working.)
	EventWaitingRoomQueued EventKind = "waiting_room.queued"
	EventWaitingRoom       EventKind = "waiting_room.entered"

	// Terrain lifecycle
	EventTerrainOnline EventKind = "terrain.online"
	EventTerrainDone   EventKind = "terrain.done"

	// Channel pressure
	EventLogOverflow EventKind = "log.overflow"
	EventLogDropped  EventKind = "log.dropped"

	// Swarm lifecycle
	EventSwarmStarted EventKind = "swarm.started"
	EventSwarmDone    EventKind = "swarm.done"

	// Dashboard
	EventDashboardAuth EventKind = "dashboard.auth"
)

// Event is the unit of communication on the EventBus.
//
// Fields are populated depending on EventKind — not all fields are relevant
// to every event. Zero values are safe to ignore.
//
// The BytesIn and Duration fields are populated on EventVisitComplete,
// EventVisitError, and EventWaitingRoom so that observers such as the
// PrometheusObserver can record metrics without dereferencing Visit.
//
// Warning: Subscribers must not retain a reference to Event, or to the
// Visit and Life it points at, after their subscriber function returns.
// Copy the fields you need. The emitter may reuse or mutate those values
// once Emit returns.
type Event struct {
	Kind       EventKind `json:"kind"`
	OccurredAt time.Time `json:"occurred_at"`

	// Lemming identity
	LemmingID string `json:"lemming_id,omitempty"`
	Terrain   int    `json:"terrain"`
	Pack      int    `json:"pack"`

	// Visit detail — populated on EventVisitComplete and EventVisitError.
	// On visit events Duration is the visit's server latency (see Visit).
	URL        string        `json:"url,omitempty"`
	StatusCode int           `json:"status_code,omitempty"`
	BytesIn    int64         `json:"bytes_in,omitempty"`
	Duration   time.Duration `json:"duration_ns,omitempty"`

	// Position is the waiting room queue position on waiting room events.
	Position int `json:"position,omitempty"`

	// Visit is the complete visit record on EventVisitComplete and
	// EventVisitError. Nil on every other kind.
	Visit *Visit `json:"-"`

	// Identity is the newborn lemming's persona on EventLemmingBorn.
	// Nil on every other kind.
	Identity *Identity `json:"-"`

	// Life is the lemming's final record on the Terrain's EventLemmingDied.
	// Nil on every other kind.
	Life *LifeLog `json:"-"`

	// Error — non-nil on failure events and failed auth attempts
	Err error `json:"-"`
}

// Subscriber is a function that receives events from the bus.
//
// Subscribers must not block — if processing takes time, the subscriber
// should hand off to its own goroutine or buffered channel internally.
// A blocking subscriber stalls every goroutine that calls Emit, which
// under swarm load means stalling thousands of lemming goroutines
// simultaneously.
type Subscriber func(Event)

// busView is an immutable snapshot of the bus published for Emit.
type busView struct {
	subscribers []Subscriber
	closed      bool
}

// EventBus is a synchronous fan-out event dispatcher.
//
// All subscribers receive every event in registration order. Subscribers
// are called on the goroutine that calls Emit. The bus is safe for
// concurrent use — Subscribe, Emit, and Close may all be called from
// multiple goroutines simultaneously.
//
// Emit is lock-free and allocation-free: writers (Subscribe, unsubscribe,
// Close) mutate the bus under a mutex and publish an immutable view that
// Emit loads atomically. A subscriber may therefore unsubscribe itself,
// subscribe another callback, or close the bus from inside its callback.
//
// Usage:
//
//	bus := NewEventBus()
//	unsub := bus.Subscribe(func(e Event) {
//	    fmt.Println(e.Kind)
//	})
//	defer unsub()
//	bus.Emit(Event{Kind: EventLemmingBorn})
//
// Warning: closing the bus does not drain in-flight Emit calls. Ensure
// all emitters have stopped before calling Close if ordering guarantees
// are required.
type EventBus struct {
	mu          sync.Mutex
	subscribers []Subscriber
	closed      bool
	view        atomic.Pointer[busView]
}

// NewEventBus constructs an empty EventBus ready to accept subscribers.
func NewEventBus() *EventBus {
	b := &EventBus{
		subscribers: make([]Subscriber, 0, 8),
	}
	b.publish()
	return b
}

// publish stores an immutable copy of the current subscriber list.
// Must be called with b.mu held (or before the bus is shared).
func (b *EventBus) publish() {
	b.view.Store(&busView{
		subscribers: append([]Subscriber(nil), b.subscribers...),
		closed:      b.closed,
	})
}

// Subscribe registers a subscriber to receive all future events.
//
// Returns an unsubscribe function — call it to stop receiving events.
// The slot is set to nil rather than removed, preserving the indices of
// other subscribers registered after this one. Subscribing a nil function
// or subscribing to a closed bus is a no-op that returns a no-op.
//
// Usage:
//
//	unsub := bus.Subscribe(myHandler)
//	defer unsub()
//
// Safe to call concurrently with Emit and other Subscribe calls.
func (b *EventBus) Subscribe(fn Subscriber) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed || fn == nil {
		return func() {}
	}
	b.subscribers = append(b.subscribers, fn)
	idx := len(b.subscribers) - 1
	b.publish()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if idx < len(b.subscribers) && b.subscribers[idx] != nil {
			b.subscribers[idx] = nil
			b.publish()
		}
	}
}

// Emit dispatches an event to all registered non-nil subscribers.
//
// OccurredAt is set to time.Now() if not already populated by the caller.
// Nil subscriber slots (unsubscribed) are skipped without cost.
// No-ops if the bus has been closed.
//
// Warning: Emit is synchronous. All subscriber functions run on the calling
// goroutine before Emit returns. Slow subscribers block the caller.
func (b *EventBus) Emit(e Event) {
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

// Close marks the bus as closed. Subsequent Emit calls are no-ops.
//
// Subscribers are not notified of closure — emit EventSwarmDone before
// closing if subscribers need to perform final cleanup.
//
// Safe to call multiple times — subsequent calls after the first are no-ops.
func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.publish()
}

// Filter returns a Subscriber that only calls fn when the event Kind
// is in the allowed set.
//
// Usage:
//
//	bus.Subscribe(Filter(handler, EventLemmingBorn, EventLemmingDied))
//
// An empty kinds list produces a subscriber that never fires.
func Filter(fn Subscriber, kinds ...EventKind) Subscriber {
	allowed := make(map[EventKind]bool, len(kinds))
	for _, k := range kinds {
		allowed[k] = true
	}
	return func(e Event) {
		if allowed[e.Kind] {
			fn(e)
		}
	}
}

// Tee returns a Subscriber that fans an event out to all provided subscribers.
//
// Nil entries in the subscribers list are skipped. Useful for attaching
// multiple observers to the same event stream without registering them
// separately on the bus.
//
// Usage:
//
//	bus.Subscribe(Tee(dashboardHandler, prometheusHandler))
func Tee(subscribers ...Subscriber) Subscriber {
	return func(e Event) {
		for _, fn := range subscribers {
			if fn != nil {
				fn(e)
			}
		}
	}
}

// EventLog is an in-memory ordered log of events with a fixed capacity.
//
// When the log is full, the oldest event is overwritten by the newest.
// Recording is O(1): the log is a ring buffer, so a full log does not
// shift its contents on every event. EventLog is safe for concurrent use.
//
// Usage:
//
//	log := NewEventLog(1000)
//	bus.Subscribe(log.AsSubscriber())
//	events := log.Snapshot()
//
// Warning: Snapshot returns a copy of the internal slice at the moment of
// the call. Events recorded after Snapshot returns are not visible in the
// returned slice.
type EventLog struct {
	mu     sync.RWMutex
	events []Event
	start  int // index of the oldest event once the ring is full
	cap    int
}

// NewEventLog constructs an EventLog with the given capacity.
//
// When full, Record overwrites the oldest event with the newest.
// A capacity of 0 is valid but produces a log that never retains events.
func NewEventLog(capacity int) *EventLog {
	if capacity < 0 {
		capacity = 0
	}
	return &EventLog{
		events: make([]Event, 0, capacity),
		cap:    capacity,
	}
}

// Record appends an event to the log, evicting the oldest if at capacity.
//
// Safe to call concurrently with Snapshot and other Record calls.
func (l *EventLog) Record(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cap == 0 {
		return
	}
	// Retained events must not pin the records they point to.
	e.Visit, e.Life, e.Identity = nil, nil, nil
	if len(l.events) < l.cap {
		l.events = append(l.events, e)
		return
	}
	l.events[l.start] = e
	l.start = (l.start + 1) % l.cap
}

// Snapshot returns a copy of all currently recorded events in order,
// oldest first.
//
// The returned slice is independent of the log's internal state —
// mutating it does not affect the log.
func (l *EventLog) Snapshot() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]Event, 0, len(l.events))
	out = append(out, l.events[l.start:]...)
	out = append(out, l.events[:l.start]...)
	return out
}

// AsSubscriber returns the EventLog as a Subscriber function suitable
// for direct registration on an EventBus.
//
// Usage:
//
//	log := NewEventLog(1000)
//	bus.Subscribe(log.AsSubscriber())
func (l *EventLog) AsSubscriber() Subscriber {
	return func(e Event) {
		l.Record(e)
	}
}
