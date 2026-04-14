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
// Fields are populated depending on EventKind — not all fields
// are relevant to every event. Zero values are safe to ignore.
type Event struct {
	Kind       EventKind
	OccurredAt time.Time

	// Lemming identity
	LemmingID string
	Terrain   int
	Pack      int

	// Visit detail
	URL        string
	StatusCode int
	BytesIn    int64
	Duration   time.Duration

	// Error — non-nil on failure events
	Err error
}

// Subscriber is a function that receives events from the bus.
// Subscribers must not block — if processing takes time, the subscriber
// should hand off to its own goroutine internally.
type Subscriber func(Event)

// EventBus is a synchronous fan-out event dispatcher.
// All subscribers receive every event in registration order.
// Subscribers are called on the goroutine that calls Emit.
type EventBus struct {
	mu          sync.RWMutex
	subscribers []Subscriber
	closed      bool
}

// NewEventBus constructs an empty EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make([]Subscriber, 0, 8),
	}
}

// Subscribe registers a subscriber to receive all future events.
// Returns an unsubscribe function — call it to deregister.
// Safe to call concurrently.
func (b *EventBus) Subscribe(fn Subscriber) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.subscribers = append(b.subscribers, fn)
	idx := len(b.subscribers) - 1

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		// Replace with nil rather than reslice to preserve indices
		// of other subscribers registered after this one.
		if idx < len(b.subscribers) {
			b.subscribers[idx] = nil
		}
	}
}

// Emit dispatches an event to all registered subscribers.
// The OccurredAt field is set here if not already populated.
// Nil subscribers (unsubscribed slots) are skipped.
// No-ops if the bus is closed.
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
// Subscribers are not notified of closure — callers should emit
// EventSwarmDone before closing.
func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
}

// Filter returns a Subscriber that only calls fn when the event
// matches one of the provided kinds. Useful for building focused
// listeners without if-chains inside subscriber functions.
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

// Tee returns a Subscriber that fans an event out to multiple subscribers.
// Useful for attaching both the dashboard and a future Prometheus exporter
// to the same event kind without registering them separately.
func Tee(subscribers ...Subscriber) Subscriber {
	return func(e Event) {
		for _, fn := range subscribers {
			if fn != nil {
				fn(e)
			}
		}
	}
}

// EventLog is an in-memory ordered log of events with a size cap.
// Used by the dashboard to replay recent events to newly authenticated
// sessions without re-running the swarm.
type EventLog struct {
	mu     sync.RWMutex
	events []Event
	cap    int
}

// NewEventLog constructs an EventLog with the given capacity.
// When full, oldest events are dropped to make room for new ones.
func NewEventLog(capacity int) *EventLog {
	return &EventLog{
		events: make([]Event, 0, capacity),
		cap:    capacity,
	}
}

// Record appends an event to the log, evicting the oldest if at capacity.
func (l *EventLog) Record(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.events) >= l.cap {
		// Shift left — drop oldest
		copy(l.events, l.events[1:])
		l.events = l.events[:len(l.events)-1]
	}
	l.events = append(l.events, e)
}

// Snapshot returns a copy of all currently recorded events in order.
func (l *EventLog) Snapshot() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// AsSubscriber returns the EventLog as a Subscriber function so it can
// be registered directly on the EventBus.
func (l *EventLog) AsSubscriber() Subscriber {
	return func(e Event) {
		l.Record(e)
	}
}
