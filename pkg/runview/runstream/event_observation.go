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

// eventShipRefused names why pipe refused rows shipped under c. The consumer's
// Close stops delivery; otherwise c ended — a round the terminal poller ended
// is transport control, and the subscription resumes from the last shipped
// event.
func eventShipRefused(pipe EventPipe, c context.Context) error {
	select {
	case <-pipe.Done():
		return errors.Join(errEventDeliveryStopped, c.Err())
	default:
		if err := c.Err(); err != nil {
			return err
		}
		return errors.New("runstream: ship refused under a live context and an open pipe")
	}
}

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
	var firstFatal, refused error
	backfillErr := s.BackfillEvents(c, pos.seq, pos.after, MaxEventsPerPage(), func(rows []*store.Event) bool {
		if firstFatal != nil || refused != nil {
			return false
		}
		if e := checkObservedBatch(rows, original, observed); e != nil {
			firstFatal = e
			return false
		}
		if !pipe.Ship(c, rows) {
			refused = eventShipRefused(pipe, c)
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
	if refused != nil {
		return errors.Join(refused, backfillErr)
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
