package runstream

import (
	"context"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

// pipe is the producer-side subscription helper shared by every
// backend: a delivery channel, an error channel, a done signal for
// Close, and once-guarded teardown. The channel closing is the
// consumer's "stream over" signal.
type pipe[T any] struct {
	ch      chan T
	errs    chan error
	done    chan struct{}
	onClose func() // optional; runs once inside Close (e.g. a ctx cancel)
	closeO  sync.Once
	finO    sync.Once
}

func newPipe[T any](onClose func()) *pipe[T] {
	return &pipe[T]{
		ch:      make(chan T, 8),
		errs:    make(chan error, 4),
		done:    make(chan struct{}),
		onClose: onClose,
	}
}

// Ship delivers one value, aborting on cancellation. Returns false when
// the subscription is over and the producer must stop.
func (p *pipe[T]) Ship(ctx context.Context, v T) bool {
	select {
	case p.ch <- v:
		return true
	case <-ctx.Done():
		return false
	case <-p.done:
		return false
	}
}

// Warn surfaces an error — best-effort, dropped when the errors channel
// is saturated. For a fatal error, Warn then return (Finish closes the
// channels, which is the "stream over" signal the consumer acts on).
func (p *pipe[T]) Warn(err error) {
	select {
	case p.errs <- err:
	default:
	}
}

// Finish closes the channels and runs cleanup exactly once. Called by
// the producer goroutine on exit.
func (p *pipe[T]) Finish(cleanup func()) {
	p.finO.Do(func() {
		if cleanup != nil {
			cleanup()
		}
		close(p.ch)
		close(p.errs)
	})
}

// Done is the producer-side cancellation signal armed by Close.
func (p *pipe[T]) Done() <-chan struct{} { return p.done }

func (p *pipe[T]) Errors() <-chan error { return p.errs }

func (p *pipe[T]) Close() error {
	p.closeO.Do(func() {
		close(p.done)
		if p.onClose != nil {
			p.onClose()
		}
	})
	return nil
}

// EventPipe / LogPipe adapt the generic helper to the subscription
// interfaces (Go can't declare methods on a generic instantiation, so
// each stream kind gets a thin named wrapper).
type EventPipe struct{ *pipe[[]*store.Event] }

// NewEventPipe returns a producer-managed EventSubscription.
func NewEventPipe() EventPipe { return EventPipe{newPipe[[]*store.Event](nil)} }

// NewEventPipeWithClose is NewEventPipe with an onClose hook — used by
// producers that block outside Ship (a Mongo change-stream Next) and
// need the consumer's Close to cancel their context.
func NewEventPipeWithClose(onClose func()) EventPipe {
	return EventPipe{newPipe[[]*store.Event](onClose)}
}

func (p EventPipe) Events() <-chan []*store.Event { return p.ch }

type LogPipe struct{ *pipe[LogChunk] }

// NewLogPipe returns a producer-managed LogSubscription.
func NewLogPipe() LogPipe { return LogPipe{newPipe[LogChunk](nil)} }

// NewLogPipeWithClose is NewLogPipe with an onClose hook (see
// NewEventPipeWithClose).
func NewLogPipeWithClose(onClose func()) LogPipe {
	return LogPipe{newPipe[LogChunk](onClose)}
}

func (p LogPipe) Chunks() <-chan LogChunk { return p.ch }

var (
	_ EventSubscription = EventPipe{}
	_ LogSubscription   = LogPipe{}
)
