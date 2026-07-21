package usernotify

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

type captureSink struct {
	mu    sync.Mutex
	name  string
	fail  bool
	seen  []Notification
	calls int
}

func (c *captureSink) Name() string { return c.name }

func (c *captureSink) Deliver(_ context.Context, n Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.fail {
		return errors.New("sink down")
	}
	c.seen = append(c.seen, n)
	return nil
}

func (c *captureSink) last(t *testing.T) Notification {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		t.Fatal("no notification delivered")
	}
	return c.seen[len(c.seen)-1]
}

// pausedRun persists a run paused on a human interaction and returns the
// run.paused outcome event exactly as the runner/runview emitters build it.
func pausedRun(t *testing.T, st *store.FilesystemRunStore, id string) trigger.Event {
	t.Helper()
	ctx := context.Background()
	r, err := st.CreateRun(ctx, id, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.TenantID = "team-1"
	r.OwnerID = "owner-1"
	r.Name = "Review release"
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	interactionID := id + "_ask"
	if err := st.WriteInteraction(ctx, &store.Interaction{
		ID:        interactionID,
		RunID:     id,
		NodeID:    "ask",
		Questions: map[string]any{"instructions": "Please approve the release notes before shipping."},
	}); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	if err := st.PauseRun(ctx, id, &store.Checkpoint{NodeID: "ask", InteractionID: interactionID}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}
	return trigger.BuildRunOutcome(ctx, st, id, runtime.ErrRunPaused)
}

func TestDispatcherHumanInputNotifiesOwner(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, NewMemPrefsStore(), NewMemSentStore(), "https://iterion.example/", nil, sink)

	ev := pausedRun(t, st, "run-p1")
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	n := sink.last(t)
	if n.Kind != KindHumanInputRequested {
		t.Fatalf("kind = %q", n.Kind)
	}
	if len(n.UserIDs) != 1 || n.UserIDs[0] != "owner-1" {
		t.Fatalf("recipients = %v, want [owner-1]", n.UserIDs)
	}
	if n.TenantID != "team-1" {
		t.Fatalf("tenant = %q", n.TenantID)
	}
	if n.Title != "Input needed: Review release" {
		t.Fatalf("title = %q", n.Title)
	}
	if n.Body != "Please approve the release notes before shipping." {
		t.Fatalf("body = %q", n.Body)
	}
	if n.Link != "https://iterion.example/runs/run-p1" {
		t.Fatalf("link = %q", n.Link)
	}
	if n.Tag != "run-p1" {
		t.Fatalf("tag = %q", n.Tag)
	}
	if n.Data["interaction_id"] != "run-p1_ask" || n.Data["node_id"] != "ask" {
		t.Fatalf("data = %v", n.Data)
	}
}

func TestDispatcherOperatorPauseNotNotified(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, nil, nil, "", nil, sink)

	// run.paused with no interaction_id = operator soft-pause.
	ev := trigger.Event{
		ID:      "run:run-op:paused_operator",
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunPaused,
		Subject: trigger.Subject{Type: "run", ID: "run-op"},
		Payload: map[string]any{"run_id": "run-op", "owner_id": "owner-1"},
	}
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sink.calls != 0 {
		t.Fatalf("expected no delivery, got %d", sink.calls)
	}
}

func TestDispatcherTeamOptIn(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	prefs := NewMemPrefsStore()
	ctx := context.Background()
	// user-2 opted into team-wide; user-3 explicitly own-only; a user of
	// another team must never leak in.
	_ = prefs.Set(ctx, &Prefs{TenantID: "team-1", UserID: "user-2", Scope: ScopeTeam})
	_ = prefs.Set(ctx, &Prefs{TenantID: "team-1", UserID: "user-3", Scope: ScopeOwn})
	_ = prefs.Set(ctx, &Prefs{TenantID: "team-9", UserID: "user-9", Scope: ScopeTeam})
	// The owner opting into team scope must not be duplicated.
	_ = prefs.Set(ctx, &Prefs{TenantID: "team-1", UserID: "owner-1", Scope: ScopeTeam})

	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, prefs, nil, "", nil, sink)

	ev := pausedRun(t, st, "run-team")
	if err := d.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	n := sink.last(t)
	got := map[string]bool{}
	for _, u := range n.UserIDs {
		if got[u] {
			t.Fatalf("duplicate recipient %s in %v", u, n.UserIDs)
		}
		got[u] = true
	}
	if !got["owner-1"] || !got["user-2"] || len(n.UserIDs) != 2 {
		t.Fatalf("recipients = %v, want owner-1 + user-2", n.UserIDs)
	}
}

func TestDispatcherEpisodeDedup(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, nil, NewMemSentStore(), "", nil, sink)

	ev := pausedRun(t, st, "run-dedup")
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Same episode replayed (bus + sweep race) → no second delivery.
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle replay: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("calls = %d, want 1", sink.calls)
	}
}

func TestDispatcherFailedDeliveryReleasesClaim(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture", fail: true}
	sent := NewMemSentStore()
	d := NewDispatcher(st, nil, sent, "", nil, sink)

	ev := pausedRun(t, st, "run-retry")
	if err := d.Handle(context.Background(), ev); err == nil {
		t.Fatal("expected error when every sink fails")
	}
	// The claim was released → a retry (the sweep) delivers.
	sink.mu.Lock()
	sink.fail = false
	sink.mu.Unlock()
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle retry: %v", err)
	}
	if got := sink.last(t); got.RunID != "run-retry" {
		t.Fatalf("unexpected notification: %+v", got)
	}
}

func TestDispatcherTerminalKinds(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	r, err := st.CreateRun(ctx, "run-done", "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.OwnerID = "owner-1"
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-done", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, nil, nil, "", nil, sink)
	ev := trigger.BuildRunOutcome(ctx, st, "run-done", nil)
	if err := d.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	n := sink.last(t)
	if n.Kind != KindRunFinished {
		t.Fatalf("kind = %q", n.Kind)
	}
	if len(n.UserIDs) != 1 || n.UserIDs[0] != "owner-1" {
		t.Fatalf("recipients = %v", n.UserIDs)
	}
}
