package server

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An issue transition fans out to the runs watching that issue. The fan-out
// used to list EVERY run in the store and load each one to find them (#2636
// names the class); it asks the store for the watchers now, so a transition
// costs the number of watchers, never the size of the store — and still
// reaches exactly the deliverable watchers of the issue.
func TestWatchFanOutAsksForTheWatchersNotTheStore(t *testing.T) {
	ctx := context.Background()
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	counting := &countingRunStore{RunStore: base}
	svc := newTestRunviewService(t, "", runview.WithStore(counting))
	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	wc := &watchCoordinator{runs: svc, native: ns, logger: iterlog.New(iterlog.LevelError, os.Stderr)}

	const issue = "native:card-1"
	for _, id := range []string{"watching", "bystander", "watching-other", "done"} {
		if _, err := base.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
	}
	for id, issues := range map[string][]string{
		"watching":       {issue},
		"watching-other": {"native:card-2"},
		"done":           {issue},
	} {
		if _, err := base.AddWatchedIssues(ctx, id, issues); err != nil {
			t.Fatalf("AddWatchedIssues %s: %v", id, err)
		}
	}
	// A terminal watcher consumes no queued message: it is skipped, not written to.
	if err := base.UpdateRunStatus(ctx, "done", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus done: %v", err)
	}

	counting.listRuns.Store(0)
	counting.listWatching.Store(0)
	wc.fanOut(native.Event{Type: native.EvtIssueState, IssueID: issue, Payload: map[string]any{"from": "todo", "to": "doing"}})

	if n := counting.listRuns.Load(); n != 0 {
		t.Fatalf("the fan-out walked the whole store %d times, want 0 — it asks for the watchers (#2636)", n)
	}
	if n := counting.listWatching.Load(); n != 1 {
		t.Fatalf("the fan-out asked for the watchers %d times, want exactly 1", n)
	}

	queued := func(id string) []store.QueuedUserMessage {
		t.Helper()
		msgs, err := svc.ListQueuedMessages(ctx, id)
		if err != nil {
			t.Fatalf("ListQueuedMessages %s: %v", id, err)
		}
		return msgs
	}
	if msgs := queued("watching"); len(msgs) != 1 || !strings.Contains(msgs[0].Text, issue) {
		t.Fatalf("the running watcher of %s got %+v, want one notification naming the issue", issue, msgs)
	}
	for _, id := range []string{"bystander", "watching-other", "done"} {
		if msgs := queued(id); len(msgs) != 0 {
			t.Errorf("%s got %+v, want nothing — it does not watch %s, or cannot consume a message", id, msgs, issue)
		}
	}
}
