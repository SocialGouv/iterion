package runstream

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type reviewTerminalSelection struct {
	store.FilesystemStreamSelection
	dir string
}

func (s *reviewTerminalSelection) WatchHints() (string, string)           { return s.dir, s.dir }
func (s *reviewTerminalSelection) Terminal(context.Context) (bool, error) { return true, nil }

// This is an internal composition witness, not an external FileSource leak.
func TestReaderReviewTerminalJoinedRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	s := &reviewTerminalSelection{dir: t.TempDir()}
	f := &FileSource{logger: quietLogger()}
	final := false
	var failed error
	f.runSelectedFile(ctx, s, false, func(c context.Context) error {
		if c == ctx {
			final = true
			return errors.Join(store.ErrRunDataIdentityChanged, errEventDeliveryStopped)
		}
		return nil
	}, func(error) { t.Error("unexpected retry") }, func(e error) { failed = e })
	if !final {
		t.Fatal("final drain was not reached", ctx.Err())
	}
	if !errors.Is(failed, store.ErrRunDataIdentityChanged) {
		t.Fatal("joined permanent final refusal lost", failed)
	}
}

func TestReaderReviewReplayNilAndStopCause(t *testing.T) {
	o := readerOrigin(t, "r")
	for _, nilrow := range []bool{true, false} {
		p := NewEventPipe()
		defer p.Close()
		pos := mongoEventPosition{seq: 7}
		if !nilrow {
			for i := 0; i < cap(p.ch); i++ {
				p.ch <- nil
			}
			p.Close()
		}
		calls := 0
		s := &runtimeBoundarySelection{backEvents: func(_ context.Context, _ int64, _ bool, _ int, emit func([]*store.Event) bool) error {
			calls++
			rows := []*store.Event{readerStamped(t, o, 7)}
			if nilrow {
				rows = append(rows, nil)
			}
			if emit(rows) {
				t.Error("refused batch accepted")
			}
			if emit([]*store.Event{readerStamped(t, o, 8)}) {
				t.Error("reopened after false")
			}
			return store.ErrInvalidRunDataIdentity
		}}
		e := replayObservedEvents(t.Context(), s, &pos, p, o, true)
		if !errors.Is(e, store.ErrInvalidRunDataIdentity) || !IsPermanentError(e) || pos.seq != 7 || pos.after || calls != 1 {
			t.Fatal(e, pos, calls)
		}
		if nilrow && (!errors.Is(e, store.ErrRunDataCapability) || len(p.ch) != 0) {
			t.Fatal(e, len(p.ch))
		}
		if !nilrow && !errors.Is(e, errEventDeliveryStopped) {
			t.Fatal(e)
		}
	}
}

type reviewNilCap struct{}

func (*reviewNilCap) CheckEventObservationSelection(any) error     { panic("nil admission invoked") }
func (*reviewNilCap) EventStreamObservation() store.RunObservation { panic("nil getter invoked") }

type reviewRejectedCap struct{}

func (*reviewRejectedCap) CheckEventObservationSelection(any) error {
	return store.ErrRunDataCapability
}
func (*reviewRejectedCap) EventStreamObservation() store.RunObservation {
	panic("getter before admission")
}
func TestReaderReviewAdmissionBeforeGetter(t *testing.T) {
	var nilcap *reviewNilCap
	for _, s := range []any{nilcap, &reviewRejectedCap{}} {
		if _, _, e := store.CaptureEventStreamObservation(s); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
	}
}

func TestReaderReviewMongoPublicTypedNil(t *testing.T) {
	var selected *runtimeBoundarySelection
	m := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return selected, nil }, quietLogger())
	var panicValue any
	var gotErr error
	func() { defer func() { panicValue = recover() }(); _, gotErr = m.SubscribeEvents(t.Context(), "r", 0) }()
	// A panic occurs before subscription ownership is handed to a worker. Do not
	// call Source.Close on that failed old path: its registered wait count leaks.
	if panicValue != nil {
		t.Fatalf("public subscription called a typed-nil selection before admission: %v", panicValue)
	}
	defer m.Close()
	if !errors.Is(gotErr, store.ErrRunDataCapability) {
		t.Fatal(gotErr)
	}
}
