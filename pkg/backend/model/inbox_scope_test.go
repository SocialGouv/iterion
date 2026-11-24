package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type scopeInboxFixture struct {
	store.RunStore
	failLoad bool
	loads    int
}

func (s *scopeInboxFixture) QueuedInboxVersion(string) uint64 { return 7 }
func (s *scopeInboxFixture) LoadPendingQueuedMessages(ctx context.Context, id string) ([]store.QueuedUserMessage, error) {
	s.loads++
	if s.failLoad {
		s.failLoad = false
		return nil, errors.New("temporary read failure")
	}
	return s.RunStore.LoadPendingQueuedMessages(ctx, id)
}
func inboxScopeFixture(t *testing.T) (*store.FilesystemRunStore, *store.Run, context.Context) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "inbox", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: store.LegacyValueFormat, Protocol: r.DataProtocol, BirthID: r.BirthID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	return s, r, ctx
}
func appendScopeMessage(t *testing.T, s store.RunStore, r *store.Run, id string) {
	t.Helper()
	if err := s.AppendQueuedMessage(t.Context(), r.ID, store.QueuedUserMessage{ID: id, RunID: r.ID, Text: id, QueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}
func TestInboxRetainsBindTimeIdentity(t *testing.T) {
	t.Parallel()
	s, r, _ := inboxScopeFixture(t)
	appendScopeMessage(t, s, r, "first")
	b, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: store.LegacyValueFormat, BirthID: "different-birth"})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	h := (&StoreInboxBinder{Store: s}).Bind(stale, r.ID)
	if got := h.Drain(t.Context()); len(got) != 0 {
		t.Fatalf("stale hook delivered %v", got)
	}
	pending, err := s.LoadPendingQueuedMessages(t.Context(), r.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("stale hook changed inbox: %d %v", len(pending), err)
	}
}
func TestInboxRetriesFailedLoadAtSameVersion(t *testing.T) {
	t.Parallel()
	s, r, _ := inboxScopeFixture(t)
	appendScopeMessage(t, s, r, "retry")
	wrapper := &scopeInboxFixture{RunStore: s, failLoad: true}
	h := (&StoreInboxBinder{Store: wrapper}).Bind(t.Context(), r.ID)
	if got := h.Drain(t.Context()); len(got) != 0 {
		t.Fatal(got)
	}
	if got := h.Drain(t.Context()); len(got) != 1 || got[0] != "retry" {
		t.Fatalf("failed load suppressed retry: %v, loads=%d", got, wrapper.loads)
	}
}
func TestInboxBoundHookBypassesLegacyDoorbell(t *testing.T) {
	t.Parallel()
	s, r, ctx := inboxScopeFixture(t)
	wrapper := &scopeInboxFixture{RunStore: s}
	h := (&StoreInboxBinder{Store: wrapper}).Bind(ctx, r.ID)
	if got := h.Drain(t.Context()); len(got) != 0 {
		t.Fatal(got)
	}
	appendScopeMessage(t, s, r, "arrived")
	if got := h.Drain(t.Context()); len(got) != 1 || got[0] != "arrived" {
		t.Fatalf("legacy counter hid bound inbox: %v", got)
	}
}
func TestInboxCancellationDoesNotConsumeDelivery(t *testing.T) {
	t.Parallel()
	s, r, ctx := inboxScopeFixture(t)
	appendScopeMessage(t, s, r, "cancel")
	h := (&StoreInboxBinder{Store: s}).Bind(ctx, r.ID)
	if got := h.Drain(t.Context()); len(got) != 1 {
		t.Fatal(got)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	h.Consume(cancelled)
	rows, err := s.ListQueuedMessages(t.Context(), r.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != store.QueuedMessageStatusDelivered {
		t.Fatalf("cancel consumed: %v %v", rows, err)
	}
	h.Consume(t.Context())
	rows, err = s.ListQueuedMessages(t.Context(), r.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != store.QueuedMessageStatusConsumed {
		t.Fatalf("retry consume: %v %v", rows, err)
	}
}
