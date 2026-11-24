package store

import (
	"context"
	"errors"
	"testing"
)

func TestEventScopeCrossHandleSequenceProbe(t *testing.T) {
	a, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.CreateRun(t.Context(), "events", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(a.root)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range []*FilesystemRunStore{a, b, a, b} {
		e, err := s.AppendEvent(context.Background(), r.ID, Event{Type: EventNodeStarted, NodeID: "n"})
		if err != nil {
			t.Fatal(err)
		}
		if e.Seq != int64(i) {
			t.Fatalf("append %d: got seq%d want%d", i, e.Seq, i)
		}
	}
}

func TestEventScopeConcurrentHandlesKeepFileOrder(t *testing.T) {
	t.Parallel()
	a, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.CreateRun(t.Context(), "events", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(a.root)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, s := range []*FilesystemRunStore{a, b} {
		go func(s *FilesystemRunStore) {
			<-start
			for i := 0; i < 16; i++ {
				if _, err := s.AppendEvent(t.Context(), r.ID, Event{Type: EventNodeStarted, NodeID: "n"}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(s)
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	events, err := a.LoadEvents(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 32 {
		t.Fatalf("got %d events, want 32", len(events))
	}
	for i, e := range events {
		if e.Seq != int64(i) {
			t.Fatalf("file offset %d: seq %d", i, e.Seq)
		}
	}
}

func TestEventScopeOriginalBindingAndCancellation(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "events", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(id, birth string) context.Context {
		t.Helper()
		b, err := CaptureRunDataBinding(RunDataHeader{RunID: id, Format: LegacyValueFormat, Protocol: r.DataProtocol, BirthID: birth})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := WithRunDataBinding(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	current := bind(r.ID, r.BirthID)
	if _, err := s.AppendEvent(current, r.ID, Event{Type: EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	operations := map[string]func(context.Context) error{
		"append": func(ctx context.Context) error {
			_, e := s.AppendEvent(ctx, r.ID, Event{Type: EventNodeStarted})
			return e
		},
		"load":  func(ctx context.Context) error { _, e := s.LoadEvents(ctx, r.ID); return e },
		"range": func(ctx context.Context) error { _, e := s.LoadEventsRange(ctx, r.ID, 0, 0, 10); return e },
		"scan": func(ctx context.Context) error {
			return s.ScanEvents(ctx, r.ID, func(*Event) bool { t.Error("refused scan delivered an event"); return false })
		},
		"checkpoint": func(ctx context.Context) error { return s.SaveRun(ctx, r) },
	}
	cancelled, cancel := context.WithCancel(current)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"stale", bind(r.ID, r.BirthID+"stale"), ErrRunDataIdentityChanged},
		{"cross-run", bind("other", r.BirthID), ErrRunDataIdentityChanged},
		{"cancelled", cancelled, context.Canceled},
	} {
		for name, op := range operations {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				if err := op(tc.ctx); !errors.Is(err, tc.want) {
					t.Fatalf("want %v, got %v", tc.want, err)
				}
			})
		}
	}
	rows, err := s.LoadEvents(current, r.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("refusal modified events: %v %v", rows, err)
	}
}
