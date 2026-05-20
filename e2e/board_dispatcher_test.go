// E2E coverage for the board → dispatcher flow that the whats-next bot
// drives: a "PO" bot creates issues on the native board via boardops
// (the same dispatcher the MCP stdio/HTTP transports use), then the
// dispatcher's polling loop picks them up and runs the assigned bot.
// No external CLI, no LLM, no MCP transport — just the data flow.

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// TestBoardDispatcher_E2E_BotCreatesAndDispatches simulates whats-next
// dispatching issues onto the board: every issue created with state=ready
// must trigger a dispatcher dispatch. The runner then closes the issue.
func TestBoardDispatcher_E2E_BotCreatesAndDispatches(t *testing.T) {
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	caps := boardops.NewCapabilities("board.create,board.move,board.read,board.close")

	var dispatchMu sync.Mutex
	dispatchedRuns := map[string]bool{}
	runner.Handler = func(_ context.Context, spec dispatcher.DispatchSpec) error {
		dispatchMu.Lock()
		dispatchedRuns[spec.RunID] = true
		dispatchMu.Unlock()
		// Find the currently-claimed issue from the dispatcher snapshot.
		// The snapshot carries IssueID — DispatchSpec does not.
		for _, r := range c.Snapshot().Running {
			if r.RunID != spec.RunID {
				continue
			}
			args, _ := json.Marshal(map[string]any{"id": r.IssueID})
			if _, err := boardops.Call(ns, caps, "close_issue", args); err != nil {
				t.Errorf("close_issue: %v", err)
			}
			break
		}
		return nil
	}

	// PO bot creates two issues at state=ready (eligible).
	for _, title := range []string{"Refactor X", "Implement Y"} {
		args, _ := json.Marshal(map[string]any{
			"title":    title,
			"state":    native.StateReady,
			"assignee": "feature_dev",
			"labels":   []string{"horizon:short-term", "source:whats-next"},
		})
		if _, err := boardops.Call(ns, caps, "create_issue", args); err != nil {
			t.Fatalf("create_issue %q: %v", title, err)
		}
	}

	// Wait for two distinct dispatches.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		dispatchMu.Lock()
		got := len(dispatchedRuns)
		dispatchMu.Unlock()
		if got >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	dispatchMu.Lock()
	if len(dispatchedRuns) < 2 {
		dispatchMu.Unlock()
		t.Fatalf("expected 2 dispatches, saw %d (snapshot=%+v)", len(dispatchedRuns), c.Snapshot())
	}
	dispatchMu.Unlock()

	// Both issues must end up in a terminal state with their claim released.
	wait := time.Now().Add(2 * time.Second)
	for time.Now().Before(wait) {
		list, _ := ns.List(native.ListFilter{})
		all := true
		for _, iss := range list {
			st := ns.Board().StateByName(iss.State)
			if st == nil || !st.Terminal || iss.Claim != "" {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("issues did not all close. snapshot=%+v", c.Snapshot())
}

// TestBoardDispatcher_E2E_BotMovesIssueToReady covers the
// drafts → eligible flow: a bot creates an issue in `backlog` (the
// default, non-eligible) and the dispatcher must NOT dispatch it. After
// the bot moves the issue to `ready`, the next poll cycle picks it up.
func TestBoardDispatcher_E2E_BotMovesIssueToReady(t *testing.T) {
	c, ns, runner, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	caps := boardops.NewCapabilities("board.create,board.move,board.read")

	var dispatchMu sync.Mutex
	var dispatchedRunIDs []string
	runner.Handler = func(_ context.Context, spec dispatcher.DispatchSpec) error {
		dispatchMu.Lock()
		dispatchedRunIDs = append(dispatchedRunIDs, spec.RunID)
		dispatchMu.Unlock()
		return nil
	}

	args, _ := json.Marshal(map[string]any{
		"title": "Draft for triage",
		"state": native.StateBacklog,
	})
	res, err := boardops.Call(ns, caps, "create_issue", args)
	if err != nil {
		t.Fatalf("create_issue: %v", err)
	}
	var iss native.Issue
	_ = json.Unmarshal(res, &iss)

	// Give the dispatcher a few polls to confirm it does NOT dispatch.
	time.Sleep(300 * time.Millisecond)
	dispatchMu.Lock()
	pre := len(dispatchedRunIDs)
	dispatchMu.Unlock()
	if pre != 0 {
		t.Fatalf("issue in backlog should not dispatch, saw %d dispatches", pre)
	}

	// Now the bot promotes it to `ready` via transition_issue.
	args, _ = json.Marshal(map[string]string{"id": iss.ID, "to": native.StateReady})
	if _, err := boardops.Call(ns, caps, "transition_issue", args); err != nil {
		t.Fatalf("transition_issue: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		dispatchMu.Lock()
		got := len(dispatchedRunIDs)
		dispatchMu.Unlock()
		if got >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("dispatcher never dispatched after move-to-ready. snapshot=%+v", c.Snapshot())
		case <-time.After(30 * time.Millisecond):
		}
	}

	// Cross-check the dispatch was for our issue by inspecting the
	// dispatcher snapshot (DispatchSpec doesn't carry IssueID).
	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		for _, r := range c.Snapshot().Running {
			if r.IssueID == iss.ID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	// If the run already finished, the snapshot is empty — verify the
	// issue is no longer in `ready` (it was claimed and released).
	got, _ := ns.Get(iss.ID)
	if got != nil && got.State == native.StateReady && got.Claim == "" {
		t.Fatalf("issue %s never moved out of ready", iss.ID)
	}
}

// TestBoardDispatcher_E2E_CapabilityGate verifies that a "PO" bot
// missing board.create gets denied at boardops boundary — the same
// gate the MCP transports enforce.
func TestBoardDispatcher_E2E_CapabilityGate(t *testing.T) {
	_, ns, _, cleanup := newDispatcherFixture(t, 50*time.Millisecond)
	defer cleanup()

	caps := boardops.NewCapabilities("board.read") // no create
	args, _ := json.Marshal(map[string]any{"title": "x", "state": native.StateReady})
	_, err := boardops.Call(ns, caps, "create_issue", args)
	if err == nil || !strings.Contains(err.Error(), "capability denied") {
		t.Fatalf("expected capability-denied error, got %v", err)
	}
}
