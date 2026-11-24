package tool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// fakeWatchStore is an in-memory WatchStore keyed by runID.
type fakeWatchStore struct {
	watched map[string][]string
}

func newFakeWatchStore() *fakeWatchStore {
	return &fakeWatchStore{watched: map[string][]string{}}
}

func (f *fakeWatchStore) AddWatchedIssues(_ context.Context, runID string, ids []string) ([]string, error) {
	for _, id := range ids {
		if !slices.Contains(f.watched[runID], id) {
			f.watched[runID] = append(f.watched[runID], id)
		}
	}
	return slices.Clone(f.watched[runID]), nil
}

func (f *fakeWatchStore) RemoveWatchedIssues(_ context.Context, runID string, ids []string) ([]string, error) {
	out := f.watched[runID][:0:0]
	for _, cur := range f.watched[runID] {
		if !slices.Contains(ids, cur) {
			out = append(out, cur)
		}
	}
	f.watched[runID] = out
	return slices.Clone(out), nil
}

func TestRegisterClawWatchTools_FiltersByCaps(t *testing.T) {
	reg := NewRegistry()
	cfg := &WatchConfig{Store: newFakeWatchStore(), RunID: "run-1", Capabilities: []string{"watch.subscribe"}}
	if err := RegisterClawWatchTools(reg, cfg); err != nil {
		t.Fatalf("RegisterClawWatchTools: %v", err)
	}
	if _, err := reg.Resolve("mcp.iterion_watch.subscribe"); err != nil {
		t.Errorf("subscribe should be exposed with watch.subscribe: %v", err)
	}
	if _, err := reg.Resolve("mcp.iterion_watch.unsubscribe"); err == nil {
		t.Errorf("unsubscribe should NOT be exposed without watch.unsubscribe")
	}
}

func TestRegisterClawWatchTools_ExecuteMutatesRun(t *testing.T) {
	store := newFakeWatchStore()
	reg := NewRegistry()
	cfg := &WatchConfig{
		Store:        store,
		RunID:        "run-7",
		Capabilities: []string{"watch.subscribe", "watch.unsubscribe"},
	}
	if err := RegisterClawWatchTools(reg, cfg); err != nil {
		t.Fatal(err)
	}

	sub, err := reg.Resolve("mcp.iterion_watch.subscribe")
	if err != nil {
		t.Fatalf("subscribe missing: %v", err)
	}
	out, err := sub.Execute(WithHostCapabilities(context.Background(), []string{"watch.subscribe", "watch.unsubscribe"}, nil), json.RawMessage(`{"issue_id":"native:abc"}`))
	if err != nil {
		t.Fatalf("subscribe execute: %v", err)
	}
	if !strings.Contains(out, "native:abc") {
		t.Fatalf("expected issue in output, got %s", out)
	}
	if got := store.watched["run-7"]; len(got) != 1 || got[0] != "native:abc" {
		t.Fatalf("run-7 watched = %v, want [native:abc]", got)
	}

	// Subscription is bound to cfg.RunID — a different run is untouched.
	if got := store.watched["run-other"]; len(got) != 0 {
		t.Fatalf("subscription leaked to another run: %v", got)
	}

	unsub, err := reg.Resolve("mcp.iterion_watch.unsubscribe")
	if err != nil {
		t.Fatalf("unsubscribe missing: %v", err)
	}
	if _, err := unsub.Execute(WithHostCapabilities(context.Background(), []string{"watch.subscribe", "watch.unsubscribe"}, nil), json.RawMessage(`{"issue_id":"native:abc"}`)); err != nil {
		t.Fatalf("unsubscribe execute: %v", err)
	}
	if got := store.watched["run-7"]; len(got) != 0 {
		t.Fatalf("run-7 watched after unsubscribe = %v, want empty", got)
	}
}

func TestRegisterClawWatchTools_RejectsEmptyIssueID(t *testing.T) {
	reg := NewRegistry()
	cfg := &WatchConfig{Store: newFakeWatchStore(), RunID: "run-1", Capabilities: []string{"watch.subscribe"}}
	if err := RegisterClawWatchTools(reg, cfg); err != nil {
		t.Fatal(err)
	}
	sub, _ := reg.Resolve("mcp.iterion_watch.subscribe")
	if _, err := sub.Execute(WithHostCapabilities(context.Background(), []string{"watch.subscribe", "watch.unsubscribe"}, nil), json.RawMessage(`{"issue_id":""}`)); err == nil {
		t.Fatalf("expected error for empty issue_id")
	}
}

func TestRegisterClawWatchTools_NilCfgIsNoOp(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawWatchTools(reg, nil); err != nil {
		t.Fatalf("nil cfg should be no-op, got %v", err)
	}
	if err := RegisterClawWatchTools(reg, &WatchConfig{}); err != nil {
		t.Fatalf("nil store should be no-op, got %v", err)
	}
	// Empty RunID also disables (no run to bind subscriptions to).
	if err := RegisterClawWatchTools(reg, &WatchConfig{Store: newFakeWatchStore(), Capabilities: []string{"watch.subscribe"}}); err != nil {
		t.Fatalf("empty RunID should be no-op, got %v", err)
	}
	if _, err := reg.Resolve("mcp.iterion_watch.subscribe"); err == nil {
		t.Fatalf("no tools should be registered when RunID empty")
	}
}
