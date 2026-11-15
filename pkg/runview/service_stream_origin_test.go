package runview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// originRaceStore schedules a replacement of the run behind an ID at one
// chosen store read, then forwards that read unchanged to the native store
// with the caller's context. It counts reads; the identity policy under test
// stays in the native store and in the stream source.
type originRaceStore struct {
	store.RunStore
	mu sync.Mutex
	// beforeEvents runs once, inside the first LoadEventsRange call.
	beforeEvents func() error
	eventsDone   bool
	// beforeLoad runs once, inside the first LoadRun call made while armed.
	beforeLoad func() error
	armed      atomic.Bool
	armedLoads atomic.Int32
	// refused flips once LoadEventsRange returned a permanent refusal;
	// loadsAfterRefusal counts the LoadRun calls that follow it.
	refused           atomic.Bool
	loadsAfterRefusal atomic.Int32
	err               error
}

func (s *originRaceStore) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.err = errors.Join(s.err, err)
	s.mu.Unlock()
}

func (s *originRaceStore) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *originRaceStore) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	s.mu.Lock()
	run := s.beforeEvents != nil && !s.eventsDone
	s.eventsDone = true
	s.mu.Unlock()
	if run {
		s.record(s.beforeEvents())
	}
	page, err := s.RunStore.LoadEventsRange(ctx, id, from, to, limit)
	if runstream.IsPermanentError(err) {
		s.refused.Store(true)
	}
	return page, err
}

func (s *originRaceStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.refused.Load() {
		s.loadsAfterRefusal.Add(1)
	}
	if s.armed.Load() {
		if s.armedLoads.Add(1) == 1 && s.beforeLoad != nil {
			s.record(s.beforeLoad())
		}
	}
	return s.RunStore.LoadRun(ctx, id)
}

func newOriginStreamService(t *testing.T) (*Service, *store.FilesystemRunStore, *originRaceStore) {
	t.Helper()
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	race := &originRaceStore{RunStore: native}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive(), WithStore(race))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	return svc, native, race
}

// seedOriginRun creates runID with marker as its workflow name, n events
// carrying the marker as node ID, then moves it to status.
func seedOriginRun(ctx context.Context, st *store.FilesystemRunStore, runID, marker string, f store.ValueFormat, status store.RunStatus, n int) error {
	r, err := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: runID, WorkflowName: marker, Format: f})
	if err != nil {
		return err
	}
	bound, err := store.BindRunValueFormat(ctx, r)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if _, err := st.AppendEvent(bound, runID, store.Event{Type: store.EventNodeStarted, NodeID: marker}); err != nil {
			return err
		}
	}
	if status != "" && status != r.Status {
		return st.UpdateRunStatus(ctx, runID, status, "")
	}
	return nil
}

// replaceOriginRun ends, deletes and prunes runID, then creates its successor
// under the same ID — the only way a new birth takes an existing ID.
func replaceOriginRun(ctx context.Context, st *store.FilesystemRunStore, runID, marker string, f store.ValueFormat, status store.RunStatus, n int) error {
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	if !r.Status.IsTerminal() {
		if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFinished, ""); err != nil {
			return err
		}
	}
	if err := st.DeleteRun(ctx, runID); err != nil {
		return err
	}
	if _, err := st.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	return seedOriginRun(ctx, st, runID, marker, f, status, n)
}

func boundOriginCtx(t *testing.T, svc *Service, runID string) context.Context {
	t.Helper()
	origin, err := svc.CaptureRunOrigin(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunObservation(t.Context(), origin)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// drainOriginSubscription reads a subscription to its end and returns the
// delivered events and every error it published.
func drainOriginSubscription(t *testing.T, sub runstream.EventSubscription) ([]*store.Event, []error) {
	t.Helper()
	var got []*store.Event
	var errs []error
	events, errc := sub.Events(), sub.Errors()
	deadline := time.After(10 * time.Second)
	for events != nil || errc != nil {
		select {
		case batch, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			got = append(got, batch...)
		case err, ok := <-errc:
			if !ok {
				errc = nil
				continue
			}
			errs = append(errs, err)
		case <-deadline:
			t.Fatal("subscription never ended")
		}
	}
	return got, errs
}

func hasError(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func TestCaptureRunOriginKeepsTheStoredTenant(t *testing.T) {
	svc, native, _ := newOriginStreamService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.ExactValueFormat, store.RunStatusFinished, 0); err != nil {
		t.Fatal(err)
	}
	r, err := native.LoadRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID = "stored"
	if err := native.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	origin, err := svc.CaptureRunOrigin(store.WithIdentity(ctx, "requester", "u"), "r")
	if err != nil {
		t.Fatal(err)
	}
	if !origin.Observed() || !origin.ParentPresent() || origin.TenantID() != "stored" {
		t.Fatalf("origin = observed %v present %v tenant %q", origin.Observed(), origin.ParentPresent(), origin.TenantID())
	}
	if h := origin.Header(); h.RunID != "r" || h.Format != store.ExactValueFormat || h.BirthID == "" || h.BirthID != r.BirthID {
		t.Fatalf("header = %+v, run birth %q", h, r.BirthID)
	}
	if _, err := svc.CaptureRunOrigin(ctx, "missing"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}

// A permanent replay refusal ends a bound subscription at once: no live tail
// and no terminal catch-up follow it, and no successor event is delivered.
func TestStreamSourceReplayRefusalIsFatal(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			svc, native, race := newOriginStreamService(t)
			ctx := t.Context()
			if err := seedOriginRun(ctx, native, "r", "A", f, store.RunStatusFinished, 2); err != nil {
				t.Fatal(err)
			}
			bound := boundOriginCtx(t, svc, "r")
			race.beforeEvents = func() error {
				return replaceOriginRun(context.Background(), native, "r", "B", f, store.RunStatusFinished, 2)
			}
			sub, err := svc.StreamSource().SubscribeEvents(bound, "r", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			got, errs := drainOriginSubscription(t, sub)
			if err := race.failure(); err != nil {
				t.Fatal(err)
			}
			if !race.refused.Load() {
				t.Fatal("the native replay refusal was not reached")
			}
			for _, ev := range got {
				if ev.NodeID == "B" {
					t.Fatalf("successor event delivered: %+v", ev)
				}
			}
			if !hasError(errs, store.ErrRunDataIdentityChanged) {
				t.Fatalf("errors = %v, want the identity refusal", errs)
			}
			if n := race.loadsAfterRefusal.Load(); n != 0 {
				t.Fatalf("%d run loads followed the replay refusal: the subscription kept tailing", n)
			}
		})
	}
}

// The terminal catch-up of an external run reads the run under the
// subscription context: a successor that replaced the bound run while it was
// tailing fails the subscription at the next poll instead of being tailed.
// The barrier is the poll's own LoadRun; a second poll after the replacement
// is the proof the subscription kept tailing the successor.
func TestStreamSourceCatchUpRefusesSuccessor(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = time.Millisecond
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			svc, native, race := newOriginStreamService(t)
			ctx := t.Context()
			if err := seedOriginRun(ctx, native, "r", "A", f, store.RunStatusRunning, 1); err != nil {
				t.Fatal(err)
			}
			bound := boundOriginCtx(t, svc, "r")
			race.beforeLoad = func() error {
				return replaceOriginRun(context.Background(), native, "r", "B", f, store.RunStatusRunning, 0)
			}
			race.armed.Store(true)
			sub, err := svc.StreamSource().SubscribeEvents(bound, "r", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			var errs []error
			events, errc := sub.Events(), sub.Errors()
			deadline := time.After(10 * time.Second)
			for events != nil {
				if race.armedLoads.Load() >= 2 {
					t.Fatal("the catch-up polled again after the run was replaced: it tailed the successor")
				}
				select {
				case _, ok := <-events:
					if !ok {
						events = nil
					}
				case err, ok := <-errc:
					if ok {
						errs = append(errs, err)
					} else {
						errc = nil
					}
				case <-time.After(5 * time.Millisecond):
				case <-deadline:
					t.Fatal("subscription neither failed nor polled again")
				}
			}
			if errc != nil {
				for err := range errc {
					errs = append(errs, err)
				}
			}
			if err := race.failure(); err != nil {
				t.Fatal(err)
			}
			if race.armedLoads.Load() != 1 {
				t.Fatalf("catch-up loads = %d, want exactly the barrier's", race.armedLoads.Load())
			}
			if !hasError(errs, store.ErrRunDataIdentityChanged) {
				t.Fatalf("errors = %v, want the identity refusal", errs)
			}
		})
	}
}

// A bound log subscription is admitted against the run it binds before any
// buffer or tailer is looked up: a successor is refused synchronously.
func TestStreamSourceLogAdmissionRefusesSuccessor(t *testing.T) {
	svc, native, _ := newOriginStreamService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	if err := replaceOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if sub != nil {
		_ = sub.Close()
	}
	if !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("SubscribeLogs = %v, want the identity refusal", err)
	}
	if svc.GetLogBuffer("r") != nil {
		t.Fatal("a refused subscription stood up a log tailer")
	}
}

// The service source endorses only itself for bound subscriptions.
func TestStreamSourceBoundCapability(t *testing.T) {
	svc, _ := newStreamTestService(t)
	src := svc.StreamSource()
	if err := runstream.RequireBoundObservationSource(src); err != nil {
		t.Fatal(err)
	}
	if err := src.(runstream.BoundObservationSource).CheckBoundObservationSource(svc.StreamSource()); !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatalf("another handle endorsed: %v", err)
	}
}
