package main

import (
	"sync"
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

	// Visit lifecycle
	EventVisitComplete EventKind = "visit.complete"
	EventVisitError    EventKind = "visit.error"

	// Waiting room
	EventWaitingRoom EventKind = "waiting_room.entered"

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
// EventVisitError, EventLemmingDied, and EventWaitingRoom so that observers
// such as the PrometheusObserver can record metrics without needing to
// read from a separate channel.
//
// Warning: Subscribers must not retain a reference to Event after their
// subscriber function returns. Events are value types passed by copy — but
// the Err field is an interface and the underlying error value is shared.
type Event struct {
	Kind       EventKind
	OccurredAt time.Time

	// Lemming identity
	LemmingID string
	Terrain   int
	Pack      int

	// Visit detail — populated on EventVisitComplete and EventVisitError
	URL        string
	StatusCode int
	BytesIn    int64
	Duration   time.Duration

	// Error — non-nil on failure events and failed auth attempts
	Err error
}

// Subscriber is a function that receives events from the bus.
//
// Subscribers must not block — if processing takes time, the subscriber
// should hand off to its own goroutine or buffered channel internally.
// A blocking subscriber stalls every goroutine that calls Emit, which
// under swarm load means stalling thousands of lemming goroutines
// simultaneously.
type Subscriber func(Event)

// EventBus is a synchronous fan-out event dispatcher.
//
// All subscribers receive every event in registration order. Subscribers
// are called on the goroutine that calls Emit. The bus is safe for
// concurrent use — Subscribe, Emit, and Close may all be called from
// multiple goroutines simultaneously.
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
	mu          sync.RWMutex
	subscribers []Subscriber
	closed      bool
}

// NewEventBus constructs an empty EventBus ready to accept subscribers.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make([]Subscriber, 0, 8),
	}
}

// Subscribe registers a subscriber to receive all future events.
//
// Returns an unsubscribe function — call it to stop receiving events.
// The slot is set to nil rather than removed, preserving the indices of
// other subscribers registered after this one.
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

	b.subscribers = append(b.subscribers, fn)
	idx := len(b.subscribers) - 1

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if idx < len(b.subscribers) {
			b.subscribers[idx] = nil
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
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, fn := range b.subscribers {
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
// When the log is full, the oldest event is evicted to make room for the
// newest. EventLog is safe for concurrent use.
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
	cap    int
}

// NewEventLog constructs an EventLog with the given capacity.
//
// When full, Record evicts the oldest event before appending the newest.
// A capacity of 0 is valid but produces a log that never retains events.
func NewEventLog(capacity int) *EventLog {
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

	if len(l.events) >= l.cap {
		copy(l.events, l.events[1:])
		l.events = l.events[:len(l.events)-1]
	}
	l.events = append(l.events, e)
}

// Snapshot returns a copy of all currently recorded events in order,
// oldest first.
//
// The returned slice is independent of the log's internal state —
// mutating it does not affect the log.
func (l *EventLog) Snapshot() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]Event, len(l.events))
	copy(out, l.events)
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
