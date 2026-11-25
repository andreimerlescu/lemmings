package events

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestBus_SubscribeEmitUnsubscribe(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var count atomic.Int64
	unsub := bus.Subscribe(func(e Event) {
		if e.Kind == LemmingBorn && e.LemmingNumber == 7 {
			count.Add(1)
		}
	})

	bus.Emit(Event{Kind: LemmingBorn, LemmingNumber: 7})
	if count.Load() != 1 {
		t.Fatalf("expected 1 event, got %d", count.Load())
	}

	unsub()
	bus.Emit(Event{Kind: LemmingBorn, LemmingNumber: 7})
	if count.Load() != 1 {
		t.Fatalf("unsubscribed handler should not fire, got %d", count.Load())
	}
}

func TestBus_FilterAndTee(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var born, died atomic.Int64
	bus.Subscribe(Tee(
		Filter(func(Event) { born.Add(1) }, LemmingBorn),
		Filter(func(Event) { died.Add(1) }, LemmingDied),
	))

	bus.Emit(Event{Kind: LemmingBorn})
	bus.Emit(Event{Kind: VisitComplete})
	bus.Emit(Event{Kind: LemmingDied})

	if born.Load() != 1 || died.Load() != 1 {
		t.Errorf("born=%d died=%d, want 1 and 1", born.Load(), died.Load())
	}
}

func TestBus_ConcurrentEmitAndSubscribe(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var received atomic.Int64
	bus.Subscribe(func(Event) { received.Add(1) })

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				bus.Emit(Event{Kind: VisitComplete})
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				unsub := bus.Subscribe(func(Event) {})
				unsub()
			}
		}()
	}
	wg.Wait()

	if received.Load() != 1600 {
		t.Errorf("expected 1600 events received, got %d", received.Load())
	}
}

func TestLog_RingBufferAndPayloadStrip(t *testing.T) {
	l := NewLog(3)
	for i := int64(1); i <= 5; i++ {
		l.Record(Event{Kind: ArtifactCaptured, LemmingNumber: i, Payload: "heavy-payload"})
	}
	snap := l.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected ring capacity 3, got %d", len(snap))
	}
	for idx, want := range []int64{3, 4, 5} {
		if snap[idx].LemmingNumber != want {
			t.Errorf("snap[%d].LemmingNumber = %d, want %d", idx, snap[idx].LemmingNumber, want)
		}
		if snap[idx].Payload != nil {
			t.Errorf("snap[%d].Payload should be stripped to avoid retaining heavy objects", idx)
		}
	}
}

func TestBus_SubscriberMayUnsubscribeItself(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var calls atomic.Int64
	var unsub func()
	unsub = bus.Subscribe(func(Event) {
		calls.Add(1)
		unsub()
	})

	bus.Emit(Event{Kind: LemmingBorn})
	bus.Emit(Event{Kind: LemmingBorn})
	if calls.Load() != 1 {
		t.Errorf("self-unsubscribing handler called %d times, want 1", calls.Load())
	}
}

func BenchmarkBus_Emit_Concurrent(b *testing.B) {
	bus := NewBus()
	defer bus.Close()
	bus.Subscribe(func(Event) {})
	evt := Event{Kind: VisitComplete, StatusCode: 200}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bus.Emit(evt)
		}
	})
}
