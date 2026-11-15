package runstream

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
)

// Delivery may stop with a live context when the receiving pipe is closed.
// It is local completion, not corruption and not a reconnect request.
var errEventDeliveryStopped = errors.New("runstream: event delivery stopped")

func checkObservedBatch(rows []*store.Event, original store.RunObservation, observed bool) error {
	for _, evt := range rows {
		if evt == nil {
			return store.ErrRunDataCapability
		}
		if observed {
			o, ok := evt.EventObservation()
			if !ok {
				return store.ErrRunDataCapability
			}
			if o != original || evt.RunID != original.Header().RunID {
				return store.ErrRunDataIdentityChanged
			}
		}
	}
	return nil
}

func replayObservedEvents(c context.Context, s MongoSelection, pos *mongoEventPosition, pipe EventPipe, original store.RunObservation, observed bool) error {
	var firstFatal error
	stopped := false
	backfillErr := s.BackfillEvents(c, pos.seq, pos.after, MaxEventsPerPage(), func(rows []*store.Event) bool {
		if firstFatal != nil || stopped {
			return false
		}
		if e := checkObservedBatch(rows, original, observed); e != nil {
			firstFatal = e
			return false
		}
		if !pipe.Ship(c, rows) {
			stopped = true
			return false
		}
		if len(rows) > 0 {
			*pos = mongoEventPosition{seq: rows[len(rows)-1].Seq, after: true}
		}
		return true
	})
	if firstFatal != nil {
		return errors.Join(firstFatal, backfillErr, c.Err())
	}
	if stopped {
		return errors.Join(errEventDeliveryStopped, backfillErr, c.Err())
	}
	return backfillErr

}

func nilEventSelection(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return r.IsNil()
	}
	return false
}
