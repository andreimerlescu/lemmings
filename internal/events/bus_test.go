package events

import (
	"encoding/json"
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

func TestBus_ConcertAssetQueueAndScoutEvents(t *testing.T) {
	if AssetComplete != "asset.complete" {
		t.Errorf("AssetComplete = %q, want asset.complete", AssetComplete)
	}
	if AssetError != "asset.error" {
		t.Errorf("AssetError = %q, want asset.error", AssetError)
	}
	if QueuePoll != "waiting_room.poll" {
		t.Errorf("QueuePoll = %q, want waiting_room.poll", QueuePoll)
	}
	if ScoutVisit != "scout.visit" {
		t.Errorf("ScoutVisit = %q, want scout.visit", ScoutVisit)
	}

	bus := NewBus()
	defer bus.Close()
	log := NewLog(16)
	bus.Subscribe(log.AsSubscriber())

	var assetCount, pollCount, scoutCount atomic.Int64
	bus.Subscribe(Filter(func(e Event) {
		assetCount.Add(1)
	}, AssetComplete, AssetError))
	bus.Subscribe(Filter(func(e Event) {
		pollCount.Add(1)
	}, QueuePoll))
	bus.Subscribe(Filter(func(e Event) {
		scoutCount.Add(1)
	}, ScoutVisit))

	bus.Emit(Event{
		Kind:       AssetComplete,
		URL:        "https://example.com/assets/arcade.css",
		StatusCode: 200,
		AssetClass: "style",
		AdmitPass:  true,
		Payload:    "ephemeral",
	})
	bus.Emit(Event{
		Kind:       AssetError,
		URL:        "https://example.com/assets/logo.svg",
		StatusCode: 403,
		AssetClass: "image",
		Failed:     true,
		Banned:     true,
	})
	bus.Emit(Event{
		Kind:       QueuePoll,
		URL:        "https://example.com/queue/status",
		StatusCode: 200,
		Position:   4,
		QueuePolls: 3,
	})
	bus.Emit(Event{
		Kind:       ScoutVisit,
		URL:        "https://example.com/games",
		StatusCode: 200,
	})

	if assetCount.Load() != 2 || pollCount.Load() != 1 || scoutCount.Load() != 1 {
		t.Fatalf("filtered counts asset=%d poll=%d scout=%d, want 2, 1, 1",
			assetCount.Load(), pollCount.Load(), scoutCount.Load())
	}

	snap := log.Snapshot()
	if len(snap) != 4 {
		t.Fatalf("expected 4 logged events, got %d", len(snap))
	}
	if snap[0].AssetClass != "style" || !snap[0].AdmitPass || snap[0].Payload != nil {
		t.Errorf("snap[0] mismatch: %+v", snap[0])
	}
	if snap[1].AssetClass != "image" || !snap[1].Banned {
		t.Errorf("snap[1] mismatch: %+v", snap[1])
	}
	if snap[2].QueuePolls != 3 || snap[2].Position != 4 {
		t.Errorf("snap[2] mismatch: %+v", snap[2])
	}
	if snap[3].Kind != ScoutVisit || snap[3].StatusCode != 200 {
		t.Errorf("snap[3] mismatch: %+v", snap[3])
	}

	raw, err := json.Marshal(snap[0])
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.Kind != AssetComplete || decoded.AssetClass != "style" || !decoded.AdmitPass {
		t.Errorf("JSON round-trip mismatch: %+v", decoded)
	}
}

func TestEvent_PlanAndCohortScalarFieldsJSON(t *testing.T) {
	ev := Event{
		Kind:       VisitComplete,
		Method:     "POST",
		URL:        "https://example.com/api/checkout",
		StatusCode: 201,
		PlanName:   "buyer",
		StepIndex:  3,
		Role:       "plan",
		PlanPassed: true,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal map: %v", err)
	}
	for k, want := range map[string]any{
		"method":      "POST",
		"plan_name":   "buyer",
		"step_index":  float64(3),
		"role":        "plan",
		"plan_passed": true,
	} {
		if m[k] != want {
			t.Errorf("JSON key %q = %#v, want %#v", k, m[k], want)
		}
	}
	var decoded Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal Event: %v", err)
	}
	if decoded.Method != "POST" || decoded.PlanName != "buyer" || decoded.StepIndex != 3 || decoded.Role != "plan" || !decoded.PlanPassed {
		t.Errorf("decoded mismatch: %+v", decoded)
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
