package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueuedInboxOperationsKeepOriginalBinding(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "inbox", "wf", nil)
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
	msg := QueuedUserMessage{ID: "original", RunID: r.ID, Text: "original", QueuedAt: time.Now()}
	if err := s.AppendQueuedMessage(current, r.ID, msg); err != nil {
		t.Fatal(err)
	}
	other, err := New(s.root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := other.QueuedInboxVersionContext(current, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	msg.ID = "second"
	msg.Text = "second"
	if inserted, err := s.AppendQueuedMessageOnce(current, r.ID, msg); err != nil || !inserted {
		t.Fatalf("append once %v %v", inserted, err)
	}
	after, err := other.QueuedInboxVersionContext(current, r.ID)
	if err != nil || after <= before {
		t.Fatalf("cross-handle doorbell %d -> %d %v", before, after, err)
	}
	if rows, err := s.LoadPendingQueuedMessages(current, r.ID); err != nil || len(rows) != 2 {
		t.Fatalf("current read %v %v", rows, err)
	}
	operations := map[string]func(context.Context) error{
		"append":      func(ctx context.Context) error { return s.AppendQueuedMessage(ctx, r.ID, msg) },
		"append-once": func(ctx context.Context) error { _, e := s.AppendQueuedMessageOnce(ctx, r.ID, msg); return e },
		"transition": func(ctx context.Context) error {
			return s.UpdateQueuedMessageStatus(ctx, r.ID, "original", QueuedMessageStatusConsumed)
		},
		"pending": func(ctx context.Context) error { _, e := s.LoadPendingQueuedMessages(ctx, r.ID); return e },
		"list":    func(ctx context.Context) error { _, e := s.ListQueuedMessages(ctx, r.ID); return e },
		"version": func(ctx context.Context) error { _, e := s.QueuedInboxVersionContext(ctx, r.ID); return e },
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
	rows, err := s.ListQueuedMessages(current, r.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("refusal modified inbox %v %v", rows, err)
	}
	for _, row := range rows {
		if row.Status != QueuedMessageStatusQueued {
			t.Fatal("refusal transitioned message")
		}
	}
}
