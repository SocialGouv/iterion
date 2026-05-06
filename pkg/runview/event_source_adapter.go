package runview

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/runview/eventstream"
)

// NewEventSourceAdapter wraps an eventstream.Source so it satisfies
// the EventStreamSource interface used by the Service + the server
// Config. The adapter exists because Go doesn't bridge between two
// structurally-equivalent interfaces when they appear in return
// positions (eventstream.Subscription vs EventStreamSubscription) —
// the explicit method here forwards the call and the implicit
// concrete-to-interface conversion makes the types line up.
//
// Cloud-mode bootstrap calls this once on the Mongo source returned
// by eventstream.NewMongo and feeds the result to WithEventSource.
//
// Plan §F (T-21).
func NewEventSourceAdapter(src eventstream.Source) EventStreamSource {
	return &eventstreamAdapter{src: src}
}

type eventstreamAdapter struct {
	src eventstream.Source
}

// Subscribe forwards to the underlying source. The returned
// eventstream.Subscription has matching method signatures with
// EventStreamSubscription, so Go's interface-from-interface
// conversion is implicit.
func (a *eventstreamAdapter) Subscribe(ctx context.Context, runID string, fromSeq int64) (EventStreamSubscription, error) {
	return a.src.Subscribe(ctx, runID, fromSeq)
}
