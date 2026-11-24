package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceFeedReviewReentrantClose(t *testing.T) {
	s, _ := serviceFeedFixture(t)
	o := bridgeOrigin(t)
	sub := bridgeSub()
	result := make(chan error, 1)
	var closes atomic.Int32
	s.observedEvents.open = func(context.Context) (runstream.Source, error) {
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return sub, nil }, close: func() error {
			closes.Add(1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			h, err := s.AcquireObservedEventFeed(ctx, o)
			if h != nil {
				h.Close()
				result <- errors.New("reentrant admission accepted")
			} else {
				result <- err
			}
			return nil
		}}, nil
	}
	h := serviceFeedAcquire(t, s, o)
	// Prove the entry actually transports a known event before shutdown.
	sub.events <- []*store.Event{bridgeEvent(t, o)}
	serviceFeedReceive(t, h, o)
	done := make(chan struct{})
	go func() { s.StopBackground(t.Context()); close(done) }()
	select {
	case err := <-result:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("reentrant admission did not refuse promptly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Source.Close callback blocked on registry mutex")
	}
	bridgeWait(t, done)
	bridgeWait(t, h.Done())
	if closes.Load() != 1 {
		t.Fatalf("owned source close count %d", closes.Load())
	}
}

func TestServiceFeedReviewPublicExpiredRetry(t *testing.T) {
	s, _ := serviceFeedFixture(t)
	o := bridgeOrigin(t)
	sub := bridgeSub()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var closes atomic.Int32
	s.observedEvents.open = func(context.Context) (runstream.Source, error) {
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return sub, nil }, close: func() error { closes.Add(1); close(entered); <-release; return nil }}, nil
	}
	h := serviceFeedAcquire(t, s, o)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	s.StopBackground(expired)
	bridgeWait(t, entered)
	select {
	case <-h.Done():
		t.Fatal("cleanup falsely joined after expired stop")
	default:
	}
	if next, err := s.AcquireObservedEventFeed(t.Context(), o); next != nil || err == nil {
		t.Fatal("admission reopened after expired stop", err)
	}
	returned := make(chan struct{})
	go func() { s.StopBackground(t.Context()); close(returned) }()
	select {
	case <-returned:
		t.Fatal("public retry omitted outstanding cleanup join")
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	bridgeWait(t, returned)
	bridgeWait(t, h.Done())
	if closes.Load() != 1 || sub.closes.Load() != 1 {
		t.Fatalf("cleanup not exact: source=%d subscription=%d", closes.Load(), sub.closes.Load())
	}
}
