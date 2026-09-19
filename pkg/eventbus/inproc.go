package eventbus

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// subscriberBufferSize is the per-subscriber channel buffer. A consumer that
// fills its buffer drops events (lossy fan-out) — the publisher must never
// block, exactly as runview.EventBroker and the native events tail do. The
// producer's reconciliation path (the dispatcher poll for board events) is
// the backstop for a dropped notification.
const subscriberBufferSize = 256

// InProcBus is the local single-host Bus: an in-process fan-out with one
// buffered channel + worker goroutine per subscriber. It mirrors the
// watch_coordinator lifecycle (buffered chan → single worker → drop-on-full)
// and runview.EventBroker's lossy semantics. Zero external dependencies.
type InProcBus struct {
	mu           sync.RWMutex
	subs         []*inprocSub
	logger       *iterlog.Logger
	cancelBudget time.Duration // 0 → DefaultSubscribeCancelBudget; test seam only.
}

type inprocSub struct {
	name   string
	filter trigger.Matcher
	h      Handler
	ch     chan trigger.Event
	stop   chan struct{}
	done   chan struct{}
	ctx    context.Context    // cancelled by the subscriber's cancel func
	cancel context.CancelFunc // unblocks an in-flight handler on teardown
	drops  atomic.Int64
}

// NewInProcBus creates an empty in-process bus. logger may be nil.
func NewInProcBus(logger *iterlog.Logger) *InProcBus {
	return &InProcBus{logger: logger}
}

// subscribeCancelBudget bounds the wait for a per-subscriber worker to return
// after cancel is called; a handler that ignores its context cannot hold
// shutdown past this window. The value pairs with DefaultSubscribeCancelBudget
// and pkg/server's background join budget (#1257) so the grace period's
// arithmetic upstream stays one number.
func (b *InProcBus) subscribeCancelBudget() time.Duration {
	if b.cancelBudget > 0 {
		return b.cancelBudget
	}
	return DefaultSubscribeCancelBudget
}

// Publish fans ev out to every subscriber whose filter matches. Non-blocking:
// a full subscriber buffer drops the event and bumps its drop counter.
func (b *InProcBus) Publish(_ context.Context, ev trigger.Event) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if !s.filter.Match(ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			s.drops.Add(1)
			if b.logger != nil {
				b.logger.Warn("eventbus: subscriber %q buffer full, dropping %s/%s", s.name, ev.Source, ev.Kind)
			}
		}
	}
	return nil
}

// Subscribe registers h under name, pre-filtered by filter, and starts its
// worker goroutine. The returned cancel stops the worker and unregisters
// the subscriber (idempotent).
func (b *InProcBus) Subscribe(name string, filter trigger.Matcher, h Handler) (func(), error) {
	ctx, cancelCtx := context.WithCancel(context.Background())
	s := &inprocSub{
		name:   name,
		filter: filter,
		h:      h,
		ch:     make(chan trigger.Event, subscriberBufferSize),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancelCtx,
	}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()

	go b.worker(s)

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// Cancel the handler context first so an in-flight handler
			// (a launch blocked on store/LLM I/O) observes the teardown,
			// then signal the worker to exit and wait for it within a
			// bounded budget — a handler ignoring its ctx is named in a
			// warning rather than waited out, so a defective subscriber
			// cannot hold the process past the grace period.
			s.cancel()
			close(s.stop)
			budget := b.subscribeCancelBudget()
			timer := time.NewTimer(budget)
			select {
			case <-s.done:
				timer.Stop()
			case <-timer.C:
				if b.logger != nil {
					b.logger.Warn("eventbus: subscriber %q handler did not return within %s of cancel; its last write falls outside the grace period", s.name, budget)
				}
			}
			b.mu.Lock()
			for i, ex := range b.subs {
				if ex == s {
					b.subs = append(b.subs[:i], b.subs[i+1:]...)
					break
				}
			}
			b.mu.Unlock()
		})
	}
	return cancel, nil
}

func (b *InProcBus) worker(s *inprocSub) {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case ev := <-s.ch:
			if err := deliver(s.ctx, s.h, ev); err != nil && b.logger != nil {
				b.logger.Warn("eventbus: subscriber %q handler error on %s/%s: %v", s.name, ev.Source, ev.Kind, err)
			}
		}
	}
}

// Drops reports how many events were dropped for the named subscriber because
// its buffer was full (test/observability helper). Returns -1 for an unknown
// name.
func (b *InProcBus) Drops(name string) int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if s.name == name {
			return s.drops.Load()
		}
	}
	return -1
}

var _ Bus = (*InProcBus)(nil)
