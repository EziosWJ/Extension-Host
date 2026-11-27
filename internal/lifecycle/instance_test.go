package lifecycle

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

func newTestInstance() *Instance {
	return &Instance{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		subscriptions: make(map[*Subscription]chan []byte),
		done:          make(chan struct{}),
	}
}

func TestSubscriberCountAndDuplicateClose(t *testing.T) {
	inst := newTestInstance()

	var counts []int
	inst.SetSubscriberObserver(func() {
		counts = append(counts, inst.SubscriberCount())
	})

	if got := inst.SubscriberCount(); got != 0 {
		t.Fatalf("initial SubscriberCount() = %d, want 0", got)
	}

	sub1, err := inst.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe #1 failed: %v", err)
	}
	if got := inst.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount() after Subscribe #1 = %d, want 1", got)
	}

	sub2, err := inst.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe #2 failed: %v", err)
	}
	if got := inst.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount() after Subscribe #2 = %d, want 2", got)
	}

	sub1.Close()
	if got := inst.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount() after sub1.Close() = %d, want 1", got)
	}

	// Closing the same subscription twice or calling unsubscribe directly again
	// must not trigger duplicate observer notifications.
	sub1.Close()
	inst.unsubscribe(sub1)
	if got := inst.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount() after duplicate close = %d, want 1", got)
	}

	sub2.Close()
	if got := inst.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount() after sub2.Close() = %d, want 0", got)
	}

	want := []int{1, 2, 1, 0}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("observer counts = %v, want %v", counts, want)
	}
}

func TestSubscriberObserverCloseSubscriptionsAndNoDeadlock(t *testing.T) {
	t.Run("CloseSubscriptions fires observer when active subscribers exist", func(t *testing.T) {
		inst := newTestInstance()

		var counts []int
		inst.SetSubscriberObserver(func() {
			// Calling SubscriberCount() directly inside the observer must not deadlock.
			counts = append(counts, inst.SubscriberCount())
		})

		sub1, err := inst.Subscribe()
		if err != nil {
			t.Fatalf("Subscribe #1 failed: %v", err)
		}
		sub2, err := inst.Subscribe()
		if err != nil {
			t.Fatalf("Subscribe #2 failed: %v", err)
		}

		inst.CloseSubscriptions()
		if got := inst.SubscriberCount(); got != 0 {
			t.Fatalf("SubscriberCount() after CloseSubscriptions = %d, want 0", got)
		}

		select {
		case <-sub1.Done:
		default:
			t.Fatal("sub1.Done should be closed after CloseSubscriptions")
		}
		select {
		case <-sub2.Done:
		default:
			t.Fatal("sub2.Done should be closed after CloseSubscriptions")
		}

		// Subsequent CloseSubscriptions or subscription Close calls must not notify again.
		inst.CloseSubscriptions()
		sub1.Close()
		sub2.Close()

		if _, err := inst.Subscribe(); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("Subscribe after CloseSubscriptions err = %v, want ErrNotRunning", err)
		}

		want := []int{1, 2, 0}
		if !reflect.DeepEqual(counts, want) {
			t.Fatalf("observer counts = %v, want %v", counts, want)
		}
	})

	t.Run("CloseSubscriptions does not fire observer when zero subscribers", func(t *testing.T) {
		inst := newTestInstance()

		calls := 0
		inst.SetSubscriberObserver(func() {
			calls++
		})

		inst.CloseSubscriptions()
		if calls != 0 {
			t.Fatalf("observer calls = %d, want 0", calls)
		}
	})

	t.Run("SetSubscriberObserver nil clears observer", func(t *testing.T) {
		inst := newTestInstance()

		calls := 0
		inst.SetSubscriberObserver(func() {
			calls++
		})
		inst.SetSubscriberObserver(nil)

		sub, err := inst.Subscribe()
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}
		sub.Close()
		if calls != 0 {
			t.Fatalf("observer calls = %d, want 0", calls)
		}
	})
}

func TestEventBuffer128AndNonBlockingEmit(t *testing.T) {
	if eventBuffer != 128 {
		t.Fatalf("eventBuffer = %d, want 128", eventBuffer)
	}

	inst := newTestInstance()
	sub, err := inst.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	defer sub.Close()

	// Emit 128 events without reading yet; all must fit in the buffer.
	for i := 0; i < eventBuffer; i++ {
		if err := inst.Emit("tick", i); err != nil {
			t.Fatalf("Emit(%d) failed: %v", i, err)
		}
	}

	// Emitting a 129th event without reading must stay non-blocking and drop the excess event.
	done := make(chan error, 1)
	go func() {
		done <- inst.Emit("tick", eventBuffer)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("129th Emit failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("129th Emit blocked when subscriber buffer was full")
	}

	// Verify all 128 buffered events arrive in order without drops.
	for i := 0; i < eventBuffer; i++ {
		select {
		case raw := <-sub.C:
			var decoded struct {
				Event string `json:"event"`
				Data  int    `json:"data"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal event %d: %v", i, err)
			}
			if decoded.Event != "tick" || decoded.Data != i {
				t.Fatalf("event %d = %+v, want {Event:tick Data:%d}", i, decoded, i)
			}
		default:
			t.Fatalf("missing buffered event at index %d", i)
		}
	}

	// Confirm the 129th event was dropped rather than queued.
	select {
	case extra := <-sub.C:
		t.Fatalf("unexpected extra event in buffer: %s", string(extra))
	default:
	}
}

func TestSubscriberObserverConcurrent(t *testing.T) {
	inst := newTestInstance()

	var observerCalls sync.WaitGroup
	var mu sync.Mutex
	seen := 0
	inst.SetSubscriberObserver(func() {
		_ = inst.SubscriberCount()
		mu.Lock()
		seen++
		mu.Unlock()
	})

	const workers = 16
	observerCalls.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer observerCalls.Done()
			sub, err := inst.Subscribe()
			if err != nil {
				t.Errorf("Subscribe failed: %v", err)
				return
			}
			sub.Close()
		}()
	}
	observerCalls.Wait()

	if got := inst.SubscriberCount(); got != 0 {
		t.Fatalf("final SubscriberCount() = %d, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != workers*2 {
		t.Fatalf("observer calls = %d, want %d", seen, workers*2)
	}
}
