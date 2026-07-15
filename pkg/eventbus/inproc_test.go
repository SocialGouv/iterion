package eventbus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/trigger"
)

func TestInProcBusFanOutAndFilter(t *testing.T) {
	bus := NewInProcBus(nil)

	var mu sync.Mutex
	var boardN, allN int
	done := make(chan struct{}, 2)

	cancelBoard, _ := bus.Subscribe("board", trigger.Matcher{Sources: []trigger.Source{trigger.SourceBoard}}, func(_ context.Context, ev trigger.Event) error {
		mu.Lock()
		boardN++
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer cancelBoard()
	cancelAll, _ := bus.Subscribe("all", trigger.Matcher{}, func(_ context.Context, ev trigger.Event) error {
		mu.Lock()
		allN++
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer cancelAll()

	// A board event reaches both subscribers; a forge event reaches only "all".
	_ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard, Kind: "card.moved"})
	waitN(t, done, 2)
	_ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceForge, Kind: "pull_request"})
	waitN(t, done, 1)

	mu.Lock()
	defer mu.Unlock()
	if boardN != 1 {
		t.Fatalf("board subscriber got %d, want 1", boardN)
	}
	if allN != 2 {
		t.Fatalf("all subscriber got %d, want 2", allN)
	}
}

func TestInProcBusDropsOnFullBuffer(t *testing.T) {
	bus := NewInProcBus(nil)
	release := make(chan struct{})
	// A handler that blocks on the first event, so the buffer (256) then
	// fills and subsequent publishes drop.
	cancel, _ := bus.Subscribe("slow", trigger.Matcher{}, func(_ context.Context, _ trigger.Event) error {
		<-release
		return nil
	})
	defer cancel()

	// 1 in-flight + 256 buffered + N overflow. Publish well past the buffer.
	for i := 0; i < subscriberBufferSize+50; i++ {
		_ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard})
	}
	// Give the worker a moment to pull the first into flight.
	time.Sleep(20 * time.Millisecond)
	if d := bus.Drops("slow"); d <= 0 {
		t.Fatalf("expected drops > 0 on a full buffer, got %d", d)
	}
	close(release)
}

// cancel() must unblock an in-flight handler by cancelling its context, then
// return — otherwise a handler stuck on store/LLM I/O would hang shutdown.
func TestInProcBusCancelUnblocksInFlightHandler(t *testing.T) {
	bus := NewInProcBus(nil)
	entered := make(chan struct{})
	cancel, _ := bus.Subscribe("blocker", trigger.Matcher{}, func(ctx context.Context, _ trigger.Event) error {
		close(entered)
		<-ctx.Done() // blocks until the subscriber's context is cancelled
		return ctx.Err()
	})
	_ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}

	returned := make(chan struct{})
	go func() { cancel(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel() hung — in-flight handler was not unblocked by context cancellation")
	}
}

func waitN(t *testing.T, done <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, n)
		}
	}
}
