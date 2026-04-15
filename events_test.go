package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestEvent_VisitComplete_CarriesByteAndDuration verifies that an Event
// constructed for EventVisitComplete can carry BytesIn and Duration fields
// and that those values survive the bus round-trip.
func TestEvent_VisitComplete_CarriesByteAndDuration(t *testing.T) {
    bus := NewEventBus()
    var received Event

    bus.Subscribe(func(e Event) {
        if e.Kind == EventVisitComplete {
            received = e
        }
    })

    bus.Emit(Event{
        Kind:       EventVisitComplete,
        LemmingID:  "test-lemming",
        URL:        "http://example.com/",
        StatusCode: 200,
        BytesIn:    2048,
        Duration:   42 * time.Millisecond,
    })

    if received.BytesIn != 2048 {
        t.Errorf("expected BytesIn=2048, got %d", received.BytesIn)
    }
    if received.Duration != 42*time.Millisecond {
        t.Errorf("expected Duration=42ms, got %v", received.Duration)
    }
}

// TestEvent_WaitingRoom_CarriesDuration verifies that an EventWaitingRoom
// event carries the Duration field representing time spent in the queue.
func TestEvent_WaitingRoom_CarriesDuration(t *testing.T) {
    bus := NewEventBus()
    var received Event

    bus.Subscribe(func(e Event) {
        if e.Kind == EventWaitingRoom {
            received = e
        }
    })

    bus.Emit(Event{
        Kind:     EventWaitingRoom,
        URL:      "http://example.com/",
        Duration: 90 * time.Second,
    })

    if received.Duration != 90*time.Second {
        t.Errorf("expected Duration=90s, got %v", received.Duration)
    }
}

// ── EventBus ──────────────────────────────────────────────────────────────────

// TestNewEventBus verifies that a freshly constructed EventBus is open,
// has no subscribers, and can accept events without panicking.
func TestNewEventBus(t *testing.T) {
	bus := NewEventBus()
	if bus == nil {
		t.Fatal("NewEventBus returned nil")
	}
	if bus.closed {
		t.Fatal("new EventBus should not be closed")
	}
	if len(bus.subscribers) != 0 {
		t.Fatalf("new EventBus should have 0 subscribers, got %d", len(bus.subscribers))
	}
	// Emit on a bus with no subscribers must not panic
	bus.Emit(Event{Kind: EventLemmingBorn})
}

// TestEventBus_Subscribe verifies that a registered subscriber receives
// every event emitted after registration.
func TestEventBus_Subscribe(t *testing.T) {
	bus := NewEventBus()
	var received []Event
	var mu sync.Mutex

	bus.Subscribe(func(e Event) {
		mu.Lock()
		received = append(received, e)
		mu.Unlock()
	})

	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Emit(Event{Kind: EventLemmingDied})
	bus.Emit(Event{Kind: EventTerrainOnline})

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Fatalf("expected 3 events, got %d", len(received))
	}
	if received[0].Kind != EventLemmingBorn {
		t.Errorf("expected first event %q, got %q", EventLemmingBorn, received[0].Kind)
	}
	if received[1].Kind != EventLemmingDied {
		t.Errorf("expected second event %q, got %q", EventLemmingDied, received[1].Kind)
	}
	if received[2].Kind != EventTerrainOnline {
		t.Errorf("expected third event %q, got %q", EventTerrainOnline, received[2].Kind)
	}
}

// TestEventBus_Subscribe_MultipleSubscribers verifies that all registered
// subscribers receive every event in registration order.
func TestEventBus_Subscribe_MultipleSubscribers(t *testing.T) {
	bus := NewEventBus()
	results := make([]int, 3)
	var mu sync.Mutex

	for i := 0; i < 3; i++ {
		i := i
		bus.Subscribe(func(e Event) {
			mu.Lock()
			results[i]++
			mu.Unlock()
		})
	}

	bus.Emit(Event{Kind: EventLemmingBorn})

	mu.Lock()
	defer mu.Unlock()
	for i, count := range results {
		if count != 1 {
			t.Errorf("subscriber %d: expected 1 event, got %d", i, count)
		}
	}
}

// TestEventBus_Unsubscribe verifies that calling the returned unsubscribe
// function causes the subscriber to stop receiving events.
func TestEventBus_Unsubscribe(t *testing.T) {
	bus := NewEventBus()
	var count int
	var mu sync.Mutex

	unsub := bus.Subscribe(func(e Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	bus.Emit(Event{Kind: EventLemmingBorn})
	unsub()
	bus.Emit(Event{Kind: EventLemmingDied})

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 event before unsubscribe, got %d", count)
	}
}

// TestEventBus_Unsubscribe_SlotNilledNotRemoved verifies that unsubscribing
// nils the slot rather than reslicing, preserving indices of later subscribers.
func TestEventBus_Unsubscribe_SlotNilledNotRemoved(t *testing.T) {
	bus := NewEventBus()
	var secondCount int
	var mu sync.Mutex

	unsub := bus.Subscribe(func(e Event) {})
	bus.Subscribe(func(e Event) {
		mu.Lock()
		secondCount++
		mu.Unlock()
	})

	unsub() // nil first slot

	// Second subscriber must still receive events after first is unsubscribed
	bus.Emit(Event{Kind: EventLemmingBorn})

	mu.Lock()
	defer mu.Unlock()
	if secondCount != 1 {
		t.Fatalf("second subscriber should still receive events after first unsubscribes, got %d", secondCount)
	}
}

// TestEventBus_Emit_SetsOccurredAt verifies that Emit populates OccurredAt
// when the event has a zero time value.
func TestEventBus_Emit_SetsOccurredAt(t *testing.T) {
	bus := NewEventBus()
	var received Event

	bus.Subscribe(func(e Event) {
		received = e
	})

	before := time.Now()
	bus.Emit(Event{Kind: EventLemmingBorn}) // zero OccurredAt
	after := time.Now()

	if received.OccurredAt.IsZero() {
		t.Fatal("OccurredAt should have been set by Emit")
	}
	if received.OccurredAt.Before(before) || received.OccurredAt.After(after) {
		t.Errorf("OccurredAt %v not within expected range [%v, %v]",
			received.OccurredAt, before, after)
	}
}

// TestEventBus_Emit_PreservesOccurredAt verifies that Emit does not overwrite
// OccurredAt when it has already been set by the caller.
func TestEventBus_Emit_PreservesOccurredAt(t *testing.T) {
	bus := NewEventBus()
	var received Event

	bus.Subscribe(func(e Event) {
		received = e
	})

	fixed := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	bus.Emit(Event{Kind: EventLemmingBorn, OccurredAt: fixed})

	if !received.OccurredAt.Equal(fixed) {
		t.Errorf("OccurredAt should be preserved: expected %v, got %v", fixed, received.OccurredAt)
	}
}

// TestEventBus_Emit_AfterClose verifies that Emit is a no-op after the
// bus has been closed. Subscribers registered before Close must not
// receive events emitted after it.
func TestEventBus_Emit_AfterClose(t *testing.T) {
	bus := NewEventBus()
	var count int
	var mu sync.Mutex

	bus.Subscribe(func(e Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Close()
	bus.Emit(Event{Kind: EventLemmingDied})
	bus.Emit(Event{Kind: EventTerrainOnline})

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 event before close, got %d after close", count)
	}
}

// TestEventBus_Close_Idempotent verifies that calling Close multiple times
// does not panic or cause any error.
func TestEventBus_Close_Idempotent(t *testing.T) {
	bus := NewEventBus()
	bus.Close()
	bus.Close() // must not panic
	bus.Close()
}

// TestEventBus_Emit_Concurrent verifies that concurrent Emit calls from
// multiple goroutines do not cause data races. Run with -race.
func TestEventBus_Emit_Concurrent(t *testing.T) {
	bus := NewEventBus()
	var count int64
	var mu sync.Mutex

	bus.Subscribe(func(e Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	const goroutines = 100
	const eventsEach = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < eventsEach; j++ {
				bus.Emit(Event{Kind: EventVisitComplete})
			}
		}()
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	expected := int64(goroutines * eventsEach)
	if count != expected {
		t.Fatalf("expected %d events, got %d", expected, count)
	}
}

// TestEventBus_Subscribe_Concurrent verifies that concurrent Subscribe and
// Emit calls do not race. Run with -race.
func TestEventBus_Subscribe_Concurrent(t *testing.T) {
	bus := NewEventBus()
	var wg sync.WaitGroup

	// Concurrent subscribers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unsub := bus.Subscribe(func(e Event) {})
			bus.Emit(Event{Kind: EventLemmingBorn})
			unsub()
		}()
	}

	wg.Wait() // must complete without race detector firing
}

// ── Filter ────────────────────────────────────────────────────────────────────

// TestFilter_PassesMatchingKinds verifies that Filter calls the inner
// subscriber only for events whose Kind is in the allowed set.
func TestFilter_PassesMatchingKinds(t *testing.T) {
	var received []EventKind
	var mu sync.Mutex

	fn := Filter(func(e Event) {
		mu.Lock()
		received = append(received, e.Kind)
		mu.Unlock()
	}, EventLemmingBorn, EventLemmingDied)

	fn(Event{Kind: EventLemmingBorn})
	fn(Event{Kind: EventTerrainOnline}) // not in filter
	fn(Event{Kind: EventLemmingDied})
	fn(Event{Kind: EventLogDropped}) // not in filter

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 events through filter, got %d", len(received))
	}
	if received[0] != EventLemmingBorn {
		t.Errorf("expected %q, got %q", EventLemmingBorn, received[0])
	}
	if received[1] != EventLemmingDied {
		t.Errorf("expected %q, got %q", EventLemmingDied, received[1])
	}
}

// TestFilter_BlocksNonMatchingKinds verifies that Filter never calls the
// inner subscriber when no kinds match.
func TestFilter_BlocksNonMatchingKinds(t *testing.T) {
	called := false
	fn := Filter(func(e Event) {
		called = true
	}, EventLemmingBorn)

	fn(Event{Kind: EventTerrainOnline})
	fn(Event{Kind: EventLogDropped})
	fn(Event{Kind: EventSwarmDone})

	if called {
		t.Fatal("Filter called inner subscriber for non-matching kind")
	}
}

// TestFilter_EmptyKindList verifies that a Filter with no allowed kinds
// never passes any event through.
func TestFilter_EmptyKindList(t *testing.T) {
	called := false
	fn := Filter(func(e Event) {
		called = true
	}) // no kinds specified

	fn(Event{Kind: EventLemmingBorn})
	fn(Event{Kind: EventSwarmDone})

	if called {
		t.Fatal("Filter with empty kind list should never call inner subscriber")
	}
}

// ── Tee ───────────────────────────────────────────────────────────────────────

// TestTee_FansToAllSubscribers verifies that Tee delivers every event to
// all provided subscribers.
func TestTee_FansToAllSubscribers(t *testing.T) {
	counts := make([]int, 3)
	var mu sync.Mutex

	subs := make([]Subscriber, 3)
	for i := 0; i < 3; i++ {
		i := i
		subs[i] = func(e Event) {
			mu.Lock()
			counts[i]++
			mu.Unlock()
		}
	}

	fn := Tee(subs...)
	fn(Event{Kind: EventLemmingBorn})
	fn(Event{Kind: EventLemmingDied})

	mu.Lock()
	defer mu.Unlock()
	for i, c := range counts {
		if c != 2 {
			t.Errorf("subscriber %d: expected 2 events, got %d", i, c)
		}
	}
}

// TestTee_NilSubscribersSkipped verifies that Tee skips nil subscribers
// without panicking, matching the bus's own nil-skip behaviour.
func TestTee_NilSubscribersSkipped(t *testing.T) {
	var count int
	fn := Tee(
		nil,
		func(e Event) { count++ },
		nil,
	)
	fn(Event{Kind: EventLemmingBorn}) // must not panic
	if count != 1 {
		t.Fatalf("expected 1 call from non-nil subscriber, got %d", count)
	}
}

// TestTee_Empty verifies that Tee with no subscribers does not panic.
func TestTee_Empty(t *testing.T) {
	fn := Tee()
	fn(Event{Kind: EventLemmingBorn}) // must not panic
}

// ── EventLog ──────────────────────────────────────────────────────────────────

// TestNewEventLog verifies construction with correct initial state.
func TestNewEventLog(t *testing.T) {
	log := NewEventLog(128)
	if log == nil {
		t.Fatal("NewEventLog returned nil")
	}
	if log.cap != 128 {
		t.Errorf("expected cap 128, got %d", log.cap)
	}
	if len(log.events) != 0 {
		t.Errorf("new EventLog should be empty, got %d events", len(log.events))
	}
}

// TestEventLog_Record_Appends verifies that events are appended in order.
func TestEventLog_Record_Appends(t *testing.T) {
	log := NewEventLog(10)
	log.Record(Event{Kind: EventLemmingBorn})
	log.Record(Event{Kind: EventLemmingDied})

	snap := log.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 events, got %d", len(snap))
	}
	if snap[0].Kind != EventLemmingBorn {
		t.Errorf("expected %q, got %q", EventLemmingBorn, snap[0].Kind)
	}
	if snap[1].Kind != EventLemmingDied {
		t.Errorf("expected %q, got %q", EventLemmingDied, snap[1].Kind)
	}
}

// TestEventLog_Record_EvictsOldestAtCapacity verifies that when the log
// is full, the oldest event is dropped to make room for the newest.
func TestEventLog_Record_EvictsOldestAtCapacity(t *testing.T) {
	const cap = 3
	log := NewEventLog(cap)

	kinds := []EventKind{
		EventLemmingBorn,
		EventLemmingDied,
		EventTerrainOnline,
		EventSwarmDone, // this should evict EventLemmingBorn
	}

	for _, k := range kinds {
		log.Record(Event{Kind: k})
	}

	snap := log.Snapshot()
	if len(snap) != cap {
		t.Fatalf("expected %d events, got %d", cap, len(snap))
	}
	if snap[0].Kind != EventLemmingDied {
		t.Errorf("oldest event should have been evicted: expected %q at [0], got %q",
			EventLemmingDied, snap[0].Kind)
	}
	if snap[cap-1].Kind != EventSwarmDone {
		t.Errorf("newest event should be last: expected %q, got %q",
			EventSwarmDone, snap[cap-1].Kind)
	}
}

// TestEventLog_Snapshot_ReturnsCopy verifies that mutating the returned
// slice does not affect the log's internal state.
func TestEventLog_Snapshot_ReturnsCopy(t *testing.T) {
	log := NewEventLog(10)
	log.Record(Event{Kind: EventLemmingBorn})

	snap := log.Snapshot()
	snap[0] = Event{Kind: EventSwarmDone} // mutate the copy

	snap2 := log.Snapshot()
	if snap2[0].Kind != EventLemmingBorn {
		t.Error("Snapshot should return a copy — mutating it should not affect the log")
	}
}

// TestEventLog_Snapshot_EmptyLog verifies that Snapshot on an empty log
// returns a non-nil empty slice.
func TestEventLog_Snapshot_EmptyLog(t *testing.T) {
	log := NewEventLog(10)
	snap := log.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot on empty log should return non-nil slice")
	}
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot, got %d events", len(snap))
	}
}

// TestEventLog_AsSubscriber verifies that the subscriber function returned
// by AsSubscriber correctly records events into the log.
func TestEventLog_AsSubscriber(t *testing.T) {
	log := NewEventLog(10)
	sub := log.AsSubscriber()

	sub(Event{Kind: EventLemmingBorn})
	sub(Event{Kind: EventTerrainOnline})

	snap := log.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 events via AsSubscriber, got %d", len(snap))
	}
}

// TestEventLog_Record_Concurrent verifies that concurrent Record calls
// do not cause data races. Run with -race.
func TestEventLog_Record_Concurrent(t *testing.T) {
	log := NewEventLog(1000)
	var wg sync.WaitGroup
	const goroutines = 100

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			log.Record(Event{
				Kind:      EventVisitComplete,
				LemmingID: fmt.Sprintf("lemming-%d", n),
			})
		}(i)
	}

	wg.Wait()
	snap := log.Snapshot()
	if len(snap) > 1000 {
		t.Errorf("log exceeded capacity: got %d events", len(snap))
	}
}

// TestEventBus_Filter_Integration verifies Filter and Subscribe work
// correctly together end-to-end on a live bus.
func TestEventBus_Filter_Integration(t *testing.T) {
	bus := NewEventBus()
	var received []EventKind
	var mu sync.Mutex

	bus.Subscribe(Filter(func(e Event) {
		mu.Lock()
		received = append(received, e.Kind)
		mu.Unlock()
	}, EventLemmingBorn, EventLemmingDied))

	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Emit(Event{Kind: EventTerrainOnline}) // filtered out
	bus.Emit(Event{Kind: EventLemmingDied})
	bus.Emit(Event{Kind: EventLogDropped}) // filtered out

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 events through filter on live bus, got %d", len(received))
	}
}

// TestEventBus_Tee_Integration verifies Tee and Subscribe work correctly
// together end-to-end on a live bus.
func TestEventBus_Tee_Integration(t *testing.T) {
	bus := NewEventBus()
	var aCount, bCount int
	var mu sync.Mutex

	a := func(e Event) { mu.Lock(); aCount++; mu.Unlock() }
	b := func(e Event) { mu.Lock(); bCount++; mu.Unlock() }

	bus.Subscribe(Tee(a, b))
	bus.Emit(Event{Kind: EventLemmingBorn})
	bus.Emit(Event{Kind: EventLemmingDied})

	mu.Lock()
	defer mu.Unlock()
	if aCount != 2 {
		t.Errorf("subscriber a: expected 2, got %d", aCount)
	}
	if bCount != 2 {
		t.Errorf("subscriber b: expected 2, got %d", bCount)
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkEventBus_Emit_OneSubscriber measures Emit throughput with a
// single subscriber. Establishes the baseline cost of a dispatch.
func BenchmarkEventBus_Emit_OneSubscriber(b *testing.B) {
	bus := NewEventBus()
	bus.Subscribe(func(e Event) {})
	evt := Event{Kind: EventVisitComplete, LemmingID: "bench-lemming"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus.Emit(evt)
	}
}

// BenchmarkEventBus_Emit_HundredSubscribers measures Emit throughput with
// 100 subscribers — representative of a large production deployment with
// multiple dashboard clients and a Prometheus exporter attached.
func BenchmarkEventBus_Emit_HundredSubscribers(b *testing.B) {
	bus := NewEventBus()
	for i := 0; i < 100; i++ {
		bus.Subscribe(func(e Event) {})
	}
	evt := Event{Kind: EventVisitComplete, LemmingID: "bench-lemming"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus.Emit(evt)
	}
}

// BenchmarkEventBus_Emit_Concurrent measures Emit throughput under full
// swarm-like concurrency — 1000 goroutines emitting simultaneously.
// This is the worst-case scenario the EventBus faces in production.
//
//go:build bench
func BenchmarkEventBus_Emit_Concurrent(b *testing.B) {
	bus := NewEventBus()
	bus.Subscribe(func(e Event) {})
	evt := Event{Kind: EventVisitComplete}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bus.Emit(evt)
		}
	})
}

// BenchmarkEventLog_Record measures the cost of recording a single event
// into a log at capacity — the worst case that triggers the eviction path.
func BenchmarkEventLog_Record(b *testing.B) {
	log := NewEventLog(1000)
	// Fill to capacity first so every Record call hits the eviction path
	for i := 0; i < 1000; i++ {
		log.Record(Event{Kind: EventVisitComplete})
	}
	evt := Event{Kind: EventLemmingBorn, LemmingID: "bench"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		log.Record(evt)
	}
}

// BenchmarkEventLog_Snapshot measures the cost of snapshotting a full log.
// The dashboard calls this on every cold-join — it should stay sub-millisecond
// even at maximum capacity.
func BenchmarkEventLog_Snapshot(b *testing.B) {
	log := NewEventLog(1000)
	for i := 0; i < 1000; i++ {
		log.Record(Event{Kind: EventVisitComplete})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = log.Snapshot()
	}
}
