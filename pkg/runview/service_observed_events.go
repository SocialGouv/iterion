package runview

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ObservedEventFeed is a lossy, original-observation fan-out feed, not a replay
// subscription. Producer startup replays its backlog; later clients receive only
// post-registration events. Drops counts only this client's overflow. Payload
// Data is borrowed/read-only, matching EventBroker ownership.
//
// Done and Err belong to the shared producer, not this client's reference.
// Non-last Close closes this client's Events but may return before Done closes.
// Events can close earlier than Done and retain queued original events. Await
// Done before treating Err as final. Last Close joins owned producer cleanup.
// Shutdown joins source/subscription workers, not all AfterFunc callbacks.
// Explicit feeds may stop before Drain persists its final interruption event.
type ObservedEventFeed interface {
	Events() <-chan *store.Event
	Done() <-chan struct{}
	Err() error
	Drops() int
	Close() error
}

// AcquireObservedEventFeed uses an already-authorized retained observation. It
// never resolves a new origin by public ID and does not prove authorization.
// Only the native filesystem store without a configured borrowed StreamSource
// supports this capability. Unknown/absent originals and unsupported variants
// typed-refuse. Historical empty-birth observations keep their original limits.
// This API does not replace StreamSource, per-client replay, alerts or logs.
func (s *Service) AcquireObservedEventFeed(ctx context.Context, original store.RunObservation) (ObservedEventFeed, error) {
	if s == nil || s.observedEvents == nil {
		return nil, store.ErrRunDataCapability
	}
	h, err := s.observedEvents.acquire(ctx, original)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (s *Service) initObservedEventFeeds() {
	if s.streamSrc != nil {
		return
	}
	if native, ok := s.store.(*store.FilesystemRunStore); !ok || native == nil {
		return
	}
	lifetime, cancel := context.WithCancelCause(context.Background())
	r, err := newObservedEventBridges(lifetime, s)
	if err != nil {
		cancel(err)
		return
	}
	r.stopOwnerLifetime = cancel
	s.observedEvents = r
}
