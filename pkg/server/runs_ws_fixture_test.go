package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
)

// testOwnedRunStreams joins the producers a server fixture starts before its
// temporary store disappears. Subscription Close only requests cancellation;
// channels close after replay and producer cleanup have actually returned.
type testOwnedRunStreams struct {
	runstream.Source
	mu       sync.Mutex
	closed   bool
	events   []runstream.EventSubscription
	logs     []runstream.LogSubscription
	once     sync.Once
	closeErr error
}

func (s *testOwnedRunStreams) SubscribeEvents(ctx context.Context, id string, from int64) (runstream.EventSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, context.Canceled
	}
	sub, e := s.Source.SubscribeEvents(ctx, id, from)
	if e == nil {
		s.events = append(s.events, sub)
	}
	return sub, e
}
func (s *testOwnedRunStreams) SubscribeLogs(ctx context.Context, id string, from int64) (runstream.LogSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, context.Canceled
	}
	sub, e := s.Source.SubscribeLogs(ctx, id, from)
	if e == nil {
		s.logs = append(s.logs, sub)
	}
	return sub, e
}
func (s *testOwnedRunStreams) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		events, logs := s.events, s.logs
		s.mu.Unlock()
		for _, sub := range events {
			s.closeErr = errors.Join(s.closeErr, sub.Close())
		}
		for _, sub := range logs {
			s.closeErr = errors.Join(s.closeErr, sub.Close())
		}
		s.closeErr = errors.Join(s.closeErr, s.Source.Close())
		for _, sub := range events {
			for range sub.Events() {
			}
		}
		for _, sub := range logs {
			for range sub.Chunks() {
			}
		}
	})
	return s.closeErr
}

// testHeldRunSource exposes the real pipe's cancel/finish distinction without
// a wall-clock guess or a filesystem cleanup retry.
type testHeldRunSource struct {
	event runstream.EventPipe
	log   runstream.LogPipe
}

func (s *testHeldRunSource) SubscribeEvents(context.Context, string, int64) (runstream.EventSubscription, error) {
	return s.event, nil
}
func (s *testHeldRunSource) SubscribeLogs(context.Context, string, int64) (runstream.LogSubscription, error) {
	return s.log, nil
}
func (s *testHeldRunSource) Close() error { return nil }
func TestRunServerFixtureJoinsItsStreamProducers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &testHeldRunSource{runstream.NewEventPipe(), runstream.NewLogPipe()}
		source := &testOwnedRunStreams{Source: inner}
		if _, e := source.SubscribeEvents(t.Context(), "r", 0); e != nil {
			t.Fatal(e)
		}
		if _, e := source.SubscribeLogs(t.Context(), "r", 0); e != nil {
			t.Fatal(e)
		}
		finished := make(chan error, 1)
		go func() { finished <- source.Close() }()
		synctest.Wait()
		select {
		case e := <-finished:
			t.Fatalf("cleanup returned before producers ended: %v", e)
		default:
		}
		select {
		case <-inner.event.Done():
		default:
			t.Fatal("event was not canceled")
		}
		select {
		case <-inner.log.Done():
		default:
			t.Fatal("log was not canceled")
		}
		if _, e := source.SubscribeEvents(t.Context(), "late", 0); !errors.Is(e, context.Canceled) {
			t.Fatal("late event admitted", e)
		}
		if _, e := source.SubscribeLogs(t.Context(), "late", 0); !errors.Is(e, context.Canceled) {
			t.Fatal("late log admitted", e)
		}
		inner.event.Finish(nil)
		synctest.Wait()
		select {
		case e := <-finished:
			t.Fatalf("cleanup returned before log ended: %v", e)
		default:
		}
		inner.log.Finish(nil)
		synctest.Wait()
		if e := <-finished; e != nil {
			t.Fatal(e)
		}
		if e := source.Close(); e != nil {
			t.Fatal(e)
		}
	})
}

type testAdmissionRunSource struct {
	*testHeldRunSource
	entered, release chan struct{}
}

func (s *testAdmissionRunSource) SubscribeEvents(ctx context.Context, id string, from int64) (runstream.EventSubscription, error) {
	close(s.entered)
	<-s.release
	return s.testHeldRunSource.SubscribeEvents(ctx, id, from)
}
func TestRunServerFixtureJoinsSubscriptionBeingAdmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &testAdmissionRunSource{testHeldRunSource: &testHeldRunSource{runstream.NewEventPipe(), runstream.NewLogPipe()}, entered: make(chan struct{}), release: make(chan struct{})}
		defer inner.event.Finish(nil)
		source := &testOwnedRunStreams{Source: inner}
		admitted := make(chan error, 1)
		go func() { _, e := source.SubscribeEvents(t.Context(), "r", 0); admitted <- e }()
		synctest.Wait()
		select {
		case <-inner.entered:
		default:
			t.Fatal("subscription never entered source")
		}
		finished := make(chan error, 1)
		closing := make(chan struct{})
		go func() { close(closing); finished <- source.Close() }()
		<-closing
		// Mutex contention is not durably blocking to synctest. Release
		// admission before Wait; the producer still cannot finish.
		close(inner.release)
		synctest.Wait()
		if e := <-admitted; e != nil {
			t.Fatal(e)
		}
		select {
		case e := <-finished:
			t.Fatalf("cleanup failed to join admitted producer: %v", e)
		default:
		}
		select {
		case <-inner.event.Done():
		default:
			t.Fatal("admitted producer not canceled")
		}
		inner.event.Finish(nil)
		synctest.Wait()
		if e := <-finished; e != nil {
			t.Fatal(e)
		}
	})
}

// Preserve the fixture's producer ownership when forwarding the additive seam.
func (s *testOwnedRunStreams) SubscribeEventsCursor(ctx context.Context, id string, cursor runstream.EventCursor) (runstream.EventSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, context.Canceled
	}
	sub, e := runstream.SubscribeEventsCursor(ctx, s.Source, id, cursor)
	if e == nil {
		s.events = append(s.events, sub)
	}
	return sub, e
}
