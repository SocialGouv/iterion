package runstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// A refused ship names its cause: the consumer's Close stops delivery, while a
// drain context the terminal poller ended is the round's transport control,
// which the subscription retries.
func TestFileDrainShipRefusalNamesItsCause(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed=%v", closed), func(t *testing.T) {
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			if _, err = s.AppendEvent(t.Context(), "r", store.Event{}); err != nil {
				t.Fatal(err)
			}
			selected, _, err := s.OpenFilesystemStream(t.Context(), "r", store.FilesystemStreamOpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer selected.Close()
			pipe := NewEventPipe()
			defer pipe.Close()
			// A consumer that reads nothing: the next ship blocks.
			for i := 0; i < cap(pipe.ch); i++ {
				pipe.ch <- nil
			}
			if closed {
				pipe.Close()
			}
			d := newFileEventDrain(selected, pipe, 0)
			defer d.Close()
			round, end := context.WithCancel(t.Context())
			defer end()
			stop := time.AfterFunc(200*time.Millisecond, end)
			defer stop.Stop()
			err = d.Drain(round)
			if got := errors.Is(err, errEventDeliveryStopped); got != closed {
				t.Fatalf("Drain = %v: delivery stopped %v, want %v", err, got, closed)
			}
			if !closed && !errors.Is(err, context.Canceled) {
				t.Fatalf("Drain = %v, want the round's cancellation", err)
			}
		})
	}
}

type decodeCountingSelection struct {
	*runtimeBoundarySelection
	decoded atomic.Int32
}

func (s *decodeCountingSelection) DecodeEvent(c context.Context, raw bson.Raw) (*store.Event, error) {
	evt, err := s.runtimeBoundarySelection.DecodeEvent(c, raw)
	s.decoded.Add(1)
	return evt, err
}

// The change-stream loop: a ship the terminal poller's transient failure
// refuses — the consumer reads nothing, so the ninth event's ship blocks —
// resumes from the last shipped event, as the replay does.
func TestMongoResolvedStreamProgressAfterDelivery(t *testing.T) {
	old := mongoTerminalPollInterval
	mongoTerminalPollInterval = 20 * time.Millisecond
	defer func() { mongoTerminalPollInterval = old }()
	st, _, c := newMongoFixture(t)
	if _, err := st.CreateRun(c, "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	selection, err := st.ResolveStreamSelection(c, "r")
	if err != nil {
		t.Fatal(err)
	}
	s := &decodeCountingSelection{runtimeBoundarySelection: &runtimeBoundarySelection{MongoSelection: selection}}
	var calls atomic.Int32
	var failed atomic.Bool
	replayed := make(chan struct{})
	position := make(chan mongoEventPosition, 1)
	s.backEvents = func(_ context.Context, seq int64, after bool, _ int, _ func([]*store.Event) bool) error {
		if calls.Add(1) > 1 {
			position <- mongoEventPosition{seq: seq, after: after}
			return store.ErrRunDataIdentityChanged
		}
		close(replayed)
		return nil
	}
	s.terminal = func(context.Context) (bool, error) {
		if s.decoded.Load() == 9 && failed.CompareAndSwap(false, true) {
			return false, errors.New("transient poll failure while ninth ship is blocked")
		}
		return false, nil
	}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
	defer src.Close()
	sub, err := src.SubscribeEvents(c, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case <-replayed:
	case <-time.After(5 * time.Second):
		t.Fatal("the first round never replayed")
	}
	var seqs []int64
	for i := 0; i < 9; i++ {
		evt, err := st.AppendEvent(c, "r", store.Event{Type: store.EventType("probe")})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, evt.Seq)
	}
	// The pipe holds eight: the eighth event is the last one shipped.
	select {
	case p := <-position:
		if p.seq != seqs[7] || !p.after {
			t.Fatalf("unshipped event advanced reconnect: %#v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no retry progress (decoded %d)", s.decoded.Load())
	}
}
