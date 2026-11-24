package runstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func readerOrigin(t *testing.T, id string) store.RunObservation {
	t.Helper()
	o, e := store.CaptureRunObservation(store.RunDataHeader{RunID: id, Format: 1, BirthID: "A"}, "tenant", true)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func readerStamped(t *testing.T, o store.RunObservation, seq int64) *store.Event {
	t.Helper()
	e, err := store.WithEventObservation(store.Event{RunID: o.Header().RunID, Seq: seq}, o)
	if err != nil {
		t.Fatal(err)
	}
	return &e
}
func TestReaderOriginReplayStickyFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			c, cancel := context.WithCancel(t.Context())
			defer cancel()
			o := readerOrigin(t, "r")
			p := NewEventPipe()
			pos := mongoEventPosition{seq: 4}
			callbacks := 0
			s := &runtimeBoundarySelection{backEvents: func(_ context.Context, _ int64, _ bool, _ int, emit func([]*store.Event) bool) error {
				callbacks++
				if emit([]*store.Event{readerStamped(t, o, 4), {RunID: "r", Seq: 5}}) {
					t.Fatal("bad batch accepted")
				}
				callbacks++
				if emit([]*store.Event{readerStamped(t, o, 6)}) {
					t.Fatal("callback reopened after refusal")
				}
				if cancelled {
					cancel()
					return context.Canceled
				}
				return nil
			}}
			e := replayObservedEvents(c, s, &pos, p, o, true)
			if !errors.Is(e, store.ErrRunDataCapability) || cancelled && !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			if pos.seq != 4 || pos.after || callbacks != 2 || len(p.ch) != 0 {
				t.Fatal(pos, callbacks, len(p.ch))
			}
		})
	}
}
func TestReaderOriginReplayClosedDelivery(t *testing.T) {
	o := readerOrigin(t, "r")
	p := NewEventPipe()
	for i := 0; i < cap(p.ch); i++ {
		p.ch <- nil
	}
	p.Close()
	pos := mongoEventPosition{}
	s := &runtimeBoundarySelection{backEvents: func(_ context.Context, _ int64, _ bool, _ int, emit func([]*store.Event) bool) error {
		if emit([]*store.Event{readerStamped(t, o, 0)}) {
			t.Fatal("closed delivery accepted")
		}
		if emit([]*store.Event{readerStamped(t, o, 1)}) {
			t.Fatal("delivery reopened")
		}
		return nil
	}}
	e := replayObservedEvents(t.Context(), s, &pos, p, o, true)
	if !errors.Is(e, errEventDeliveryStopped) || IsPermanentError(e) || pos.after {
		t.Fatal(e, pos)
	}
}

type readerFileOptIn struct {
	store.FilesystemStreamSelection
	original      store.RunObservation
	gets, decodes int
	bad           bool
}

func (s *readerFileOptIn) CheckEventObservationSelection(actual any) error {
	if actual != s {
		return store.ErrRunDataCapability
	}
	return nil
}
func (s *readerFileOptIn) EventStreamObservation() store.RunObservation {
	s.gets++
	if s.gets > 1 {
		return store.RunObservation{}
	}
	return s.original
}
func (s *readerFileOptIn) DecodeObservedEvent(c context.Context, b []byte) (*store.Event, error) {
	s.decodes++
	if s.bad {
		return &store.Event{RunID: "r"}, nil
	}
	e, err := s.FilesystemStreamSelection.(store.ObservedFilesystemEventDecoder).DecodeObservedEvent(c, b)
	return e, err
}
func TestReaderOriginFileActualDrain(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			dir := t.TempDir()
			s, e := store.New(dir)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CreateRun(t.Context(), "r", "wf", nil); e != nil {
				t.Fatal(e)
			}
			if _, e = s.AppendEvent(t.Context(), "r", store.Event{}); e != nil {
				t.Fatal(e)
			}
			selected, _, e := s.OpenFilesystemStream(t.Context(), "r", store.FilesystemStreamOpenOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer selected.Close()
			o, _, e := store.CaptureEventStreamObservation(selected)
			if e != nil {
				t.Fatal(e)
			}
			wrapper := &readerFileOptIn{FilesystemStreamSelection: selected, original: o, bad: bad}
			pipe := NewEventPipe()
			d := newFileEventDrain(wrapper, pipe, 0)
			defer d.Close()
			e = d.Drain(t.Context())
			if bad {
				if !errors.Is(e, store.ErrRunDataCapability) || len(pipe.ch) != 0 {
					t.Fatal(e)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				events := <-pipe.ch
				got, ok := events[0].EventObservation()
				if !ok || got != o {
					t.Fatal(got)
				}
				if e = d.Drain(t.Context()); e != nil {
					t.Fatal(e)
				}
			}
			if wrapper.gets != 1 {
				t.Fatal("getter reread", wrapper.gets)
			}
		})
	}
}
func TestReaderOriginFileSourceBlankDelivery(t *testing.T) {
	src, _, dir := newFileSourceFixture(t)
	defer src.Close()
	p := filepath.Join(dir, "runs", "r")
	if e := os.MkdirAll(p, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "events.jsonl"), []byte("{\"seq\":0}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	sub, e := src.SubscribeEvents(t.Context(), "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case events := <-sub.Events():
		if len(events) != 1 || events[0].RunID != "r" {
			t.Fatal(events)
		}
		o, ok := events[0].EventObservation()
		if !ok || o.ParentPresent() {
			t.Fatal(o)
		}
	case e := <-sub.Errors():
		t.Fatal(e)
	case <-time.After(3 * time.Second):
		t.Fatal("no observed orphan event")
	}
}

type readerLifecycleStore interface {
	CreateRunWithValueFormat(context.Context, store.RunCreateSpec) (*store.Run, error)
	AppendEvent(context.Context, string, store.Event) (*store.Event, error)
	UpdateRunStatus(context.Context, string, store.RunStatus, string) error
}

func TestReaderOriginSourcesLifecycle(t *testing.T) {
	// Poll globals stay serialized; actual watch/replay/poll joins are unchanged.
	oldFile, oldMongo := fileTerminalPollInterval, mongoTerminalPollInterval
	fileTerminalPollInterval = 25 * time.Millisecond
	mongoTerminalPollInterval = 25 * time.Millisecond
	defer func() { fileTerminalPollInterval = oldFile; mongoTerminalPollInterval = oldMongo }()
	for _, backend := range []string{"fs", "mongo"} {
		for _, p := range []store.RunDataProtocol{0, 1} {
			for _, f := range []store.ValueFormat{1, 2} {
				t.Run(fmt.Sprintf("%s/p%d-f%d", backend, p, f), func(t *testing.T) {
					if e := store.RequireRunDataStorage(p); e != nil {
						if p == 1 && errors.Is(e, store.ErrRunDataCapability) {
							t.Skip("admission OFF")
						}
						t.Fatal(e)
					}
					var st readerLifecycleStore
					var src Source
					var c context.Context
					if backend == "fs" {
						s, e := store.New(t.TempDir())
						if e != nil {
							t.Fatal(e)
						}
						st = s
						src = NewFileSource(s, "", quietLogger())
						c = t.Context()
					} else {
						s, ctx := runtimeBoundaryExactFixture(t)
						st = s
						src = NewMongoResolved(func(c context.Context, id string) (MongoSelection, error) { return s.ResolveStreamSelection(c, id) }, quietLogger())
						c = ctx
					}
					defer src.Close()
					r, e := st.CreateRunWithValueFormat(c, store.RunCreateSpec{ID: "r", Format: f, DataProtocol: p})
					if e != nil {
						t.Fatal(e)
					}
					c, e = store.BindRunValueFormat(c, r)
					if e != nil {
						t.Fatal(e)
					}
					binding, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: p, BirthID: r.BirthID})
					if e != nil {
						t.Fatal(e)
					}
					c, e = store.WithRunDataBinding(c, binding)
					if e != nil {
						t.Fatal(e)
					}
					c, cancel := context.WithTimeout(c, 8*time.Second)
					defer cancel()
					expected, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: f, Protocol: p, BirthID: r.BirthID}, r.TenantID, true)
					if e != nil {
						t.Fatal(e)
					}
					appendEvent := func() {
						t.Helper()
						data := map[string]any{"n": json.Number("17")}
						if f == 2 {
							data["n"] = json.Number("1e400")
						}
						if _, e := st.AppendEvent(c, "r", store.Event{Data: data}); e != nil {
							t.Fatal(e)
						}
					}
					appendEvent()
					sub, e := src.SubscribeEvents(c, "r", 0)
					if e != nil {
						t.Fatal(e)
					}
					defer sub.Close()
					count := 0
					receive := func() {
						t.Helper()
						select {
						case batch, ok := <-sub.Events():
							if !ok {
								t.Fatal("closed early")
							}
							for _, v := range batch {
								o, known := v.EventObservation()
								if !known || o != expected || v.Seq != int64(count) {
									t.Fatal(v, o, expected)
								}
								if f == 2 && fmt.Sprint(v.Data["n"]) != "1e400" {
									t.Fatal(v.Data)
								}
								count++
							}
						case err := <-sub.Errors():
							t.Fatal(err)
						case <-c.Done():
							t.Fatal(c.Err())
						}
					}
					receive()
					appendEvent()
					receive()
					appendEvent()
					if e = st.UpdateRunStatus(c, "r", store.RunStatusFinished, ""); e != nil {
						t.Fatal(e)
					}
					for count < 3 {
						receive()
					}
					select {
					case batch, ok := <-sub.Events():
						if ok {
							t.Fatal("extra batch", batch)
						}
					case <-c.Done():
						t.Fatal("terminal join", c.Err())
					}
					for e := range sub.Errors() {
						if e != nil {
							t.Fatal(e)
						}
					}
				})
			}
		}
	}
}

type readerMongoOptIn struct {
	MongoSelection
	original store.RunObservation
	gets     atomic.Int32
	replays  atomic.Int32
	second   chan struct{}
}

func (s *readerMongoOptIn) CheckEventObservationSelection(actual any) error {
	if actual != s {
		return store.ErrRunDataCapability
	}
	return nil
}
func (s *readerMongoOptIn) EventStreamObservation() store.RunObservation {
	if s.gets.Add(1) != 1 {
		return store.RunObservation{}
	}
	return s.original
}
func (s *readerMongoOptIn) BackfillEvents(c context.Context, from int64, after bool, size int, emit func([]*store.Event) bool) error {
	n := s.replays.Add(1)
	e := s.MongoSelection.BackfillEvents(c, from, after, size, emit)
	if e != nil {
		return e
	}
	if n == 1 {
		return errors.New("reader test reconnect after real backfill")
	}
	if n == 2 {
		close(s.second)
	}
	return nil
}
func TestReaderOriginMongoReconnectRetainsObservation(t *testing.T) {
	st, c := runtimeBoundaryExactFixture(t)
	r, e := st.CreateRun(c, "r", "wf", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.AppendEvent(c, "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	selected, e := st.ResolveStreamSelection(c, "r")
	if e != nil {
		t.Fatal(e)
	}
	original := selected.EventStreamObservation()
	if original.Header().BirthID != r.BirthID {
		t.Fatal(original)
	}
	wrapper := &readerMongoOptIn{MongoSelection: selected, original: original, second: make(chan struct{})}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return wrapper, nil }, quietLogger())
	defer src.Close()
	c, cancel := context.WithTimeout(c, 8*time.Second)
	defer cancel()
	sub, e := src.SubscribeEvents(c, "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case <-wrapper.second:
	case <-c.Done():
		t.Fatal("reconnect not reached", c.Err())
	}
	// The second round opened its actual watch before invoking its backfill.
	if _, e = st.AppendEvent(c, "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	count := 0
	for count < 2 {
		select {
		case rows, ok := <-sub.Events():
			if !ok {
				t.Fatal("closed before replay/live")
			}
			for _, v := range rows {
				o, ok := v.EventObservation()
				if !ok || o != original || v.Seq != int64(count) {
					t.Fatal(v, o)
				}
				count++
			}
		case <-c.Done():
			t.Fatal(c.Err())
		}
	}
	if wrapper.gets.Load() != 1 || wrapper.replays.Load() < 2 {
		t.Fatal(wrapper.gets.Load(), wrapper.replays.Load())
	}
}
