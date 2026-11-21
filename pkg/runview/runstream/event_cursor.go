package runstream

import (
	"context"
	"errors"
	"math"
)

// EventCursor addresses persisted events without requiring a predecessor or
// successor outside int64. After excludes Seq; otherwise Seq is inclusive.
type EventCursor struct {
	Seq   int64
	After bool
}

func (c EventCursor) Contains(seq int64) bool { return seq > c.Seq || seq == c.Seq && !c.After }

// EventCursorSource adds exclusive anchors without changing the inclusive Source API.
// Out-of-band live alerts retain the source's existing delivery policy.
type EventCursorSource interface {
	SubscribeEventsCursor(context.Context, string, EventCursor) (EventSubscription, error)
}

var ErrEventCursorUnsupported = errors.New("runstream: source does not support this event cursor")
var ErrReplayProgressUnrepresentable = errors.New("runstream: legacy replay progress cannot represent an empty predecessor")

// SubscribeEventsCursor uses a source's cursor capability, or a representable
// inclusive equivalent. Unsupported after-Max is never silently saturated.
func SubscribeEventsCursor(ctx context.Context, source Source, id string, cursor EventCursor) (EventSubscription, error) {
	if capable, ok := source.(EventCursorSource); ok {
		return capable.SubscribeEventsCursor(ctx, id, cursor)
	}
	if !cursor.After {
		return source.SubscribeEvents(ctx, id, cursor.Seq)
	}
	if cursor.Seq == math.MaxInt64 {
		return nil, ErrEventCursorUnsupported
	}
	return source.SubscribeEvents(ctx, id, cursor.Seq+1)
}

// EventReplayProgress describes only successfully shipped persisted rows.
// Found separates an empty replay from a delivered Seq of zero or MinInt64.
type EventReplayProgress struct {
	Cursor  EventCursor
	LastSeq int64
	Found   bool
}
