package runstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// These stream tests need metadata capability preflight, but never write blob
// objects. Both historical and protocol-one event rows use the real Mongo store.
type runtimeBoundaryBlob struct{ nopBlob }

func (b runtimeBoundaryBlob) PutArtifactForFormat(c context.Context, _ int, r, n string, v int, d []byte) error {
	return b.PutArtifact(c, r, n, v, d)
}
func (b runtimeBoundaryBlob) GetArtifactForFormat(c context.Context, _ int, r, n string, v int) ([]byte, error) {
	return b.GetArtifact(c, r, n, v)
}
func (b runtimeBoundaryBlob) ListArtifactVersionsForFormat(c context.Context, _ int, r, n string) ([]int, error) {
	return b.ListArtifactVersions(c, r, n)
}
func (b runtimeBoundaryBlob) DeleteRunArtifactsForFormat(c context.Context, _ int, r string) error {
	return b.DeleteRun(c, r)
}
func (b runtimeBoundaryBlob) DeleteRunArtifacts(c context.Context, r string) error {
	return b.DeleteRun(c, r)
}
func (b runtimeBoundaryBlob) RunObjectsForFormat(int) (blob.RunObjects, error) { return b, nil }
func (b runtimeBoundaryBlob) RunObjectsForScope(store.IncarnationDataScope) (blob.RunObjects, error) {
	return b, nil
}
func (b runtimeBoundaryBlob) ArtifactsForScope(store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	return b, nil
}
func runtimeBoundaryExactFixture(t *testing.T) (*mongostore.Store, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires local Mongo replica set")
	}
	ctx := store.WithIdentity(t.Context(), "runtime-boundary", "reader")
	st, e := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("runtime_boundary_%d", time.Now().UnixNano()), Blob: runtimeBoundaryBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = st.DB().Drop(c)
		_ = st.Close(c)
	})
	return st, ctx
}

type runtimeBoundarySelection struct {
	MongoSelection
	backEvents func(context.Context, int64, bool, int, func([]*store.Event) bool) error
	backLogs   func(context.Context, int64, func(int64, []byte) bool) error
	terminal   func(context.Context) (bool, error)
}

func (s *runtimeBoundarySelection) BackfillEvents(c context.Context, n int64, a bool, b int, f func([]*store.Event) bool) error {
	if s.backEvents != nil {
		return s.backEvents(c, n, a, b, f)
	}
	return s.MongoSelection.BackfillEvents(c, n, a, b, f)
}
func (s *runtimeBoundarySelection) BackfillLogs(c context.Context, n int64, f func(int64, []byte) bool) error {
	if s.backLogs != nil {
		return s.backLogs(c, n, f)
	}
	return s.MongoSelection.BackfillLogs(c, n, f)
}
func (s *runtimeBoundarySelection) Terminal(c context.Context) (bool, error) {
	if s.terminal != nil {
		return s.terminal(c)
	}
	return s.MongoSelection.Terminal(c)
}
func runtimeBoundaryFixture(t *testing.T) (*runtimeBoundarySelection, context.Context) {
	t.Helper()
	st, _, c := newMongoFixture(t)
	if _, e := st.CreateRun(c, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	selection, e := st.ResolveStreamSelection(c, "r")
	if e != nil {
		t.Fatal(e)
	}
	return &runtimeBoundarySelection{MongoSelection: selection, terminal: func(context.Context) (bool, error) { return false, nil }}, c
}
func TestMongoResolvedFatalJoinedCancellation(t *testing.T) {
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			s, c := runtimeBoundaryFixture(t)
			fatal := errors.Join(context.Canceled, store.ErrRunDataIdentityChanged)
			s.backEvents = func(context.Context, int64, bool, int, func([]*store.Event) bool) error { return fatal }
			s.backLogs = func(context.Context, int64, func(int64, []byte) bool) error { return fatal }
			src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
			defer src.Close()
			var errs <-chan error
			var closeSub func() error
			if logs {
				sub, e := src.SubscribeLogs(c, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				errs = sub.Errors()
				closeSub = sub.Close
			} else {
				sub, e := src.SubscribeEvents(c, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				errs = sub.Errors()
				closeSub = sub.Close
			}
			defer closeSub()
			select {
			case e, ok := <-errs:
				if !ok || !errors.Is(e, store.ErrRunDataIdentityChanged) {
					t.Fatalf("fatal join lost %v %v", ok, e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("fatal did not arrive")
			}
		})
	}
}
func TestMongoResolvedCallbackMissingOriginalStops(t *testing.T) {
	s, c := runtimeBoundaryFixture(t)
	var calls atomic.Int32
	again := make(chan struct{})
	s.backEvents = func(context.Context, int64, bool, int, func([]*store.Event) bool) error {
		if calls.Add(1) == 2 {
			close(again)
		}
		return store.ErrRunNotFound
	}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
	defer src.Close()
	sub, e := src.SubscribeEvents(c, "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("unexpected event")
		}
	case <-again:
		t.Fatalf("original disappearance retried %d times", calls.Load())
	case <-time.After(5 * time.Second):
		t.Fatal("missing original neither stopped nor retried")
	}
	found := false
	for e := range sub.Errors() {
		found = found || errors.Is(e, store.ErrRunNotFound)
	}
	if !found {
		t.Fatal("missing fatal not retained")
	}
}
func TestMongoResolvedFatalSurvivesWarnings(t *testing.T) {
	s, c := runtimeBoundaryFixture(t)
	var calls atomic.Int32
	s.backEvents = func(context.Context, int64, bool, int, func([]*store.Event) bool) error {
		if calls.Add(1) <= 4 {
			return errors.New("transient replay transport")
		}
		return store.ErrRunDataIdentityChanged
	}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
	defer src.Close()
	sub, e := src.SubscribeEvents(c, "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("saturated errors blocked closure")
	}
	found := false
	for e := range sub.Errors() {
		found = found || errors.Is(e, store.ErrRunDataIdentityChanged)
	}
	if !found || calls.Load() != 5 {
		t.Fatalf("fatal lost after real reconnects found%v calls%d", found, calls.Load())
	}
}
func TestMongoResolvedPollRefusalUnblocksDelivery(t *testing.T) {
	old := mongoTerminalPollInterval
	mongoTerminalPollInterval = 20 * time.Millisecond
	defer func() { mongoTerminalPollInterval = old }()
	s, c := runtimeBoundaryFixture(t)
	var shipped atomic.Int32
	s.backEvents = func(ctx context.Context, _ int64, _ bool, _ int, emit func([]*store.Event) bool) error {
		for i := 0; i < 9; i++ {
			shipped.Store(int32(i + 1))
			if !emit([]*store.Event{{RunID: "r", Seq: int64(i)}}) {
				return nil
			}
		}
		return nil
	}
	s.terminal = func(context.Context) (bool, error) {
		if shipped.Load() == 9 {
			return false, store.ErrRunDataIdentityChanged
		}
		return false, nil
	}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
	defer src.Close()
	sub, e := src.SubscribeEvents(c, "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case e, ok := <-sub.Errors():
		if !ok || !errors.Is(e, store.ErrRunDataIdentityChanged) {
			t.Fatalf("poll refusal lost during blocked ship: %v %v", ok, e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll refusal failed to release blocked ship")
	}
}
func TestMongoResolvedLiveHistoricalValidation(t *testing.T) {
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			st, _, base := newMongoFixture(t)
			a, e := st.CreateRun(base, "r", "wf", nil)
			if e != nil {
				t.Fatal(e)
			}
			real, e := st.ResolveStreamSelection(base, "r")
			if e != nil {
				t.Fatal(e)
			}
			s := &runtimeBoundarySelection{MongoSelection: real, terminal: func(context.Context) (bool, error) { return false, nil }}
			var replaced atomic.Bool
			replace := func() error {
				if !replaced.CompareAndSwap(false, true) {
					return nil
				}
				if e := st.DeleteRun(base, "r"); e != nil {
					return e
				}
				if n, e := st.PruneDeletionMarkers(base, time.Now().Add(time.Hour)); e != nil || n != 1 {
					return fmt.Errorf("prune %d: %w", n, e)
				}
				b, e := st.CreateRun(base, "r", "wf", nil)
				if e != nil {
					return e
				}
				if b.BirthID == a.BirthID {
					return errors.New("successor reused birth")
				}
				if logs {
					return st.AppendRunLog(base, "r", 0, []byte("B"))
				}
				_, e = st.AppendEvent(base, "r", store.Event{Data: map[string]any{"marker": "B"}})
				return e
			}
			s.backEvents = func(c context.Context, n int64, a bool, b int, emit func([]*store.Event) bool) error {
				e := real.BackfillEvents(c, n, a, b, emit)
				if e == nil {
					return replace()
				}
				return e
			}
			s.backLogs = func(c context.Context, n int64, emit func(int64, []byte) bool) error {
				e := real.BackfillLogs(c, n, emit)
				if e == nil {
					return replace()
				}
				return e
			}
			src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
			defer src.Close()
			ctx, cancel := context.WithTimeout(base, 5*time.Second)
			defer cancel()
			if logs {
				sub, e := src.SubscribeLogs(ctx, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				defer sub.Close()
				select {
				case ch, ok := <-sub.Chunks():
					if ok {
						t.Fatalf("successor live bytes leaked: %q", ch.Data)
					}
					found := false
					for e := range sub.Errors() {
						found = found || errors.Is(e, store.ErrRunDataIdentityChanged)
					}
					if !found {
						t.Fatal("live bytes closed without typed refusal")
					}
				case e, ok := <-sub.Errors():
					if !ok || !errors.Is(e, store.ErrRunDataIdentityChanged) {
						t.Fatalf("expected refusal %v %v", ok, e)
					}
				case <-ctx.Done():
					t.Fatal("live refusal timeout")
				}
			} else {
				sub, e := src.SubscribeEvents(ctx, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				defer sub.Close()
				select {
				case rows, ok := <-sub.Events():
					if ok {
						t.Fatalf("successor live event leaked: %#v", rows)
					}
					found := false
					for e := range sub.Errors() {
						found = found || errors.Is(e, store.ErrRunDataIdentityChanged)
					}
					if !found {
						t.Fatal("live events closed without typed refusal")
					}
				case e, ok := <-sub.Errors():
					if !ok || !errors.Is(e, store.ErrRunDataIdentityChanged) {
						t.Fatalf("expected refusal %v %v", ok, e)
					}
				case <-ctx.Done():
					t.Fatal("live refusal timeout")
				}
			}
			if !replaced.Load() {
				t.Fatal("replacement never occurred")
			}
		})
	}
}
func TestMongoResolvedWatchFirstBoundaryExact(t *testing.T) {
	for _, protocol := range []store.RunDataProtocol{0, 1} {
		for _, f := range []store.ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("protocol%d/format%d", protocol, f), func(t *testing.T) {
				st, base := runtimeBoundaryExactFixture(t)
				r, e := st.CreateRunWithValueFormat(base, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f, DataProtocol: protocol})
				if protocol == 1 && errors.Is(e, store.ErrRunDataCapability) {
					t.Skip("protocol-one admission is OFF")
				}
				if e != nil {
					t.Fatal(e)
				}
				c, e := store.BindRunValueFormat(base, r)
				if e != nil {
					t.Fatal(e)
				}
				binding, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: protocol, BirthID: r.BirthID})
				if e != nil {
					t.Fatal(e)
				}
				c, e = store.WithRunDataBinding(c, binding)
				if e != nil {
					t.Fatal(e)
				}

				real, e := st.ResolveStreamSelection(c, "r")
				if e != nil {
					t.Fatal(e)
				}
				s := &runtimeBoundarySelection{MongoSelection: real, terminal: func(context.Context) (bool, error) { return false, nil }}
				var inserted atomic.Bool
				s.backEvents = func(ctx context.Context, from int64, after bool, size int, emit func([]*store.Event) bool) error {
					if e := real.BackfillEvents(ctx, from, after, size, emit); e != nil {
						return e
					}
					if inserted.CompareAndSwap(false, true) {
						data := map[string]any{"marker": "between-replay-and-tail"}
						if f == 2 {
							data["number"] = json.Number("1e400")
						}
						_, e := st.AppendEvent(c, "r", store.Event{Data: data})
						return e
					}
					return nil
				}
				src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
				defer src.Close()
				sub, e := src.SubscribeEvents(c, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				defer sub.Close()
				select {
				case rows, ok := <-sub.Events():
					if !ok || len(rows) != 1 || rows[0].Seq != 0 || rows[0].Data["marker"] != "between-replay-and-tail" {
						t.Fatalf("watch-first gap/wrong event %#v %v", rows, ok)
					}
					if f == 2 && rows[0].Data["number"] != json.Number("1e400") {
						t.Fatalf("live exact lost %#v", rows[0].Data)
					}
				case e := <-sub.Errors():
					t.Fatal("unexpected replay error", e)
				case <-time.After(5 * time.Second):
					t.Fatal("watch-first lost inserted event")
				}
			})
		}
	}
}
func TestMongoResolvedReconnectRetainsSelection(t *testing.T) {
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			st, _, c := newMongoFixture(t)
			if _, e := st.CreateRun(c, "r", "wf", nil); e != nil {
				t.Fatal(e)
			}
			var injected atomic.Bool
			var resolves atomic.Int32
			src := NewMongoResolved(func(ctx context.Context, id string) (MongoSelection, error) {
				resolves.Add(1)
				real, e := st.ResolveStreamSelection(ctx, id)
				if e != nil {
					return nil, e
				}
				s := &runtimeBoundarySelection{MongoSelection: real, terminal: func(context.Context) (bool, error) { return false, nil }}
				replace := func() error {
					if e := st.DeleteRun(c, "r"); e != nil {
						return e
					}
					if n, e := st.PruneDeletionMarkers(c, time.Now().Add(time.Hour)); e != nil || n != 1 {
						return fmt.Errorf("prune %d %v", n, e)
					}
					if _, e := st.CreateRun(c, "r", "wf", nil); e != nil {
						return e
					}
					if logs {
						return st.AppendRunLog(c, "r", 0, []byte("B"))
					}
					_, e := st.AppendEvent(c, "r", store.Event{Data: map[string]any{"marker": "B"}})
					return e
				}
				s.backEvents = func(ctx context.Context, n int64, a bool, b int, emit func([]*store.Event) bool) error {
					if e := real.BackfillEvents(ctx, n, a, b, emit); e != nil {
						return e
					}
					if injected.CompareAndSwap(false, true) {
						if e := replace(); e != nil {
							return e
						}
						return errors.New("synthetic retry after acknowledged empty A replay")
					}
					return nil
				}
				s.backLogs = func(ctx context.Context, n int64, emit func(int64, []byte) bool) error {
					if e := real.BackfillLogs(ctx, n, emit); e != nil {
						return e
					}
					if injected.CompareAndSwap(false, true) {
						if e := replace(); e != nil {
							return e
						}
						return errors.New("synthetic retry after acknowledged empty A replay")
					}
					return nil
				}
				return s, nil
			}, quietLogger())
			defer src.Close()
			var errs <-chan error
			var events <-chan []*store.Event
			var chunks <-chan LogChunk
			var closeSub func() error
			if logs {
				sub, e := src.SubscribeLogs(c, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				chunks = sub.Chunks()
				errs = sub.Errors()
				closeSub = sub.Close
			} else {
				sub, e := src.SubscribeEvents(c, "r", 0)
				if e != nil {
					t.Fatal(e)
				}
				events = sub.Events()
				errs = sub.Errors()
				closeSub = sub.Close
			}
			defer closeSub()
			found := false
			deadline := time.After(5 * time.Second)
			for !found {
				select {
				case rows, ok := <-events:
					if ok {
						t.Fatalf("reconnect adopted successor event %#v", rows)
					}
					events = nil
				case ch, ok := <-chunks:
					if ok {
						t.Fatalf("reconnect adopted successor bytes %q", ch.Data)
					}
					chunks = nil
				case e, ok := <-errs:
					if !ok {
						t.Fatal("closed before original refusal")
					}
					if errors.Is(e, store.ErrRunDataIdentityChanged) {
						found = true
					}
				case <-deadline:
					t.Fatal("reconnect did not refuse original")
				}
			}
			if resolves.Load() != 1 {
				t.Fatalf("selection resolved %d times", resolves.Load())
			}
		})
	}
}
func TestMongoResolvedProgressAfterDelivery(t *testing.T) {
	old := mongoTerminalPollInterval
	mongoTerminalPollInterval = 20 * time.Millisecond
	defer func() { mongoTerminalPollInterval = old }()
	s, c := runtimeBoundaryFixture(t)
	var calls, at atomic.Int32
	var failed atomic.Bool
	position := make(chan mongoEventPosition, 1)
	s.backEvents = func(ctx context.Context, seq int64, after bool, _ int, emit func([]*store.Event) bool) error {
		if calls.Add(1) > 1 {
			position <- mongoEventPosition{seq: seq, after: after}
			return store.ErrRunDataIdentityChanged
		}
		for i := 0; i < 9; i++ {
			at.Store(int32(i + 1))
			if !emit([]*store.Event{{RunID: "r", Seq: int64(i)}}) {
				return nil
			}
		}
		return nil
	}
	s.terminal = func(context.Context) (bool, error) {
		if at.Load() == 9 && failed.CompareAndSwap(false, true) {
			return false, errors.New("transient poll failure while ninth ship is blocked")
		}
		return false, nil
	}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
	defer src.Close()
	sub, e := src.SubscribeEvents(c, "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case p := <-position:
		if p.seq != 7 || !p.after {
			t.Fatalf("unshipped event advanced reconnect: %#v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no retry progress")
	}
}

// The first replay is empty; completion must still run and check the final
// replay. A transient failure there retries rather than certifying EOF.
func TestMongoResolvedTerminalReplayRefusal(t *testing.T) {
	for _, logs := range []bool{false, true} {
		for _, transient := range []bool{false, true} {
			t.Run(fmt.Sprintf("logs=%v/transient=%v", logs, transient), func(t *testing.T) {
				s, c := runtimeBoundaryFixture(t)
				var calls atomic.Int32
				result := func() error {
					n := calls.Add(1)
					if n == 1 || transient && n == 3 {
						return nil
					}
					if transient && n == 2 {
						return errors.New("transient final replay")
					}
					return store.ErrRunDataIdentityChanged
				}
				s.terminal = func(context.Context) (bool, error) { return true, nil }
				s.backEvents = func(context.Context, int64, bool, int, func([]*store.Event) bool) error { return result() }
				s.backLogs = func(context.Context, int64, func(int64, []byte) bool) error { return result() }
				src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return s, nil }, quietLogger())
				defer src.Close()
				var errs <-chan error
				var closeSub func() error
				if logs {
					sub, e := src.SubscribeLogs(c, "r", 0)
					if e != nil {
						t.Fatal(e)
					}
					errs = sub.Errors()
					closeSub = sub.Close
				} else {
					sub, e := src.SubscribeEvents(c, "r", 0)
					if e != nil {
						t.Fatal(e)
					}
					errs = sub.Errors()
					closeSub = sub.Close
				}
				defer closeSub()
				deadline := time.After(5 * time.Second)
				for {
					select {
					case e, ok := <-errs:
						if !ok {
							t.Fatal("terminal replay certified EOF without refusal")
						}
						if errors.Is(e, store.ErrRunDataIdentityChanged) {
							want := int32(2)
							if transient {
								want = 4
							}
							if calls.Load() != want {
								t.Fatalf("replay calls %d want %d", calls.Load(), want)
							}
							return
						}
					case <-deadline:
						t.Fatal("terminal replay did not finish")
					}
				}
			})
		}
	}
}
