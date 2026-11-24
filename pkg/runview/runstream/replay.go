package runstream

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// This file holds the small building blocks every backend shares:
// paginated event replay, the reconnect-with-backoff pump, and the
// offset-deduped log-chunk shipper.

// ReplayEvents preserves the historical highest-sequence result. With no rows
// at MinInt64 there is no representable predecessor: the integer result is
// nonauthoritative and ErrReplayProgressUnrepresentable preserves any load error.
// New callers should use ReplayEventsCursor for total int64 cursor semantics.
func ReplayEvents(ctx context.Context, st store.RunStore, runID string, fromSeq int64, pipe EventPipe) (int64, error) {
	p, err := ReplayEventsCursor(ctx, st, runID, EventCursor{Seq: fromSeq}, pipe)
	if p.Found {
		return p.LastSeq, err
	}
	if fromSeq == math.MinInt64 {
		return 0, errors.Join(ErrReplayProgressUnrepresentable, err)
	}
	return fromSeq - 1, err
}

// ReplayEventsCursor pages persisted rows and advances only after successful
// delivery. After-Max ends this finite replay, not its source's live tail.
func ReplayEventsCursor(ctx context.Context, st store.RunStore, runID string, cursor EventCursor, pipe EventPipe) (EventReplayProgress, error) {
	progress := EventReplayProgress{Cursor: cursor}
	for {
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		from := progress.Cursor.Seq
		if progress.Cursor.After {
			if from == math.MaxInt64 {
				return progress, nil
			}
			from++
		}
		page, err := st.LoadEventsRange(ctx, runID, from, 0, MaxEventsPerPage())
		if len(page) > 0 {
			if !pipe.Ship(ctx, page) {
				return progress, ctx.Err()
			}
			last := page[len(page)-1].Seq
			progress = EventReplayProgress{Cursor: EventCursor{Seq: last, After: true}, LastSeq: last, Found: true}
		}
		if err != nil {
			return progress, err
		}
		if len(page) < MaxEventsPerPage() {
			return progress, nil
		}
	}
}

// reconnectLoop drives attempt until it reports stop, succeeds after a
// cancellation (err == nil), or the ctx ends — retrying transient
// errors with exponential backoff (250ms → 30s ceiling). warn surfaces
// each retried error together with the chosen delay.
func reconnectLoop(ctx context.Context, warn func(delay time.Duration, err error), attempt func() (stop bool, err error)) {
	const baseBackoff = 250 * time.Millisecond
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		stop, err := attempt()
		if stop || err == nil {
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		warn(backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// ShipLogChunk delivers one log chunk deduped/sliced against the
// delivered high-water offset: fully-covered chunks are dropped,
// partially-covered ones are sliced at the boundary. data must be safe
// for the consumer to retain. Returns the new high-water offset and
// false when the subscription is over.
func ShipLogChunk(ctx context.Context, pipe LogPipe, off int64, data []byte, delivered int64) (int64, bool) {
	end := off + int64(len(data))
	if end <= delivered {
		return delivered, true
	}
	if off < delivered {
		data = data[delivered-off:]
		off = delivered
	}
	if !pipe.Ship(ctx, LogChunk{Offset: off, Data: data}) {
		return delivered, false
	}
	return end, true
}
