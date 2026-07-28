// E2E coverage for the board → dispatcher flow that the whats-next bot
// drives: a "PO" bot creates issues on the native board via boardops
// (the same dispatcher the MCP stdio/HTTP transports use), then the
// dispatcher's polling loop picks them up and runs the assigned bot.
// No external CLI, no LLM, no MCP transport — just the data flow.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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
	c.Refresh() // kick an immediate poll tick rather than waiting for the cadence

	// Wait for two distinct dispatches. Generous deadline: the dispatch
	// pipeline makes two off-actor hops per issue plus a runner round-trip, so
	// under -race + load a 3s budget can blow.
	countDispatched := func() int {
		dispatchMu.Lock()
		defer dispatchMu.Unlock()
		return len(dispatchedRuns)
	}
	waitUntil(t, 10*time.Second, "both issues to be dispatched",
		func() bool { return countDispatched() >= 2 },
		func() string {
			return fmt.Sprintf("dispatches=%d want>=2, snapshot=%+v", countDispatched(), c.Snapshot())
		})

	// Both issues must end up in a terminal state with their claim released.
	waitUntil(t, 5*time.Second, "every issue to close and release its claim",
		func() bool {
			list, _ := ns.List(native.ListFilter{})
			for _, iss := range list {
				st := ns.Board().StateByName(iss.State)
				if st == nil || !st.Terminal || iss.Claim != "" {
					return false
				}
			}
			return true
		},
		func() string { return fmt.Sprintf("snapshot=%+v", c.Snapshot()) })
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
	// The handler BLOCKS until the test releases it, modelling a real workflow
	// that takes time to run. An instant-return handler races the off-actor
	// in-progress transition (ready→in_progress runs in launchDispatchSetup):
	// a run that finishes before that transition lands makes the completed-state
	// guard skip the move and release the claim, leaving the issue back in
	// `ready` with no claim — a 25%-flake that does NOT occur in prod, where
	// runs last seconds. Blocking keeps the run observable in the Running
	// snapshot (the running entry is allocated on the actor at dispatch).
	proceed := make(chan struct{})
	runner.Handler = func(ctx context.Context, spec dispatcher.DispatchSpec) error {
		dispatchMu.Lock()
		dispatchedRunIDs = append(dispatchedRunIDs, spec.RunID)
		dispatchMu.Unlock()
		select {
		case <-proceed:
		case <-ctx.Done():
		}
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
	// Kick an immediate poll tick instead of waiting up to one polling
	// interval for the move to be noticed — makes the dispatch deterministic
	// rather than timing-budget-dependent.
	c.Refresh()

	// Generous deadline: the dispatch pipeline makes two off-actor hops
	// (discovery → setup → worker) plus a runner round-trip; under -race +
	// load a 3s budget can blow. 10s mirrors TestDispatcherE2E_CancelInFlight.
	waitUntil(t, 10*time.Second, "a dispatch after the move to ready",
		func() bool {
			dispatchMu.Lock()
			defer dispatchMu.Unlock()
			return len(dispatchedRunIDs) >= 1
		},
		func() string { return fmt.Sprintf("snapshot=%+v", c.Snapshot()) })

	// Cross-check the dispatch was for our issue by inspecting the dispatcher
	// snapshot (DispatchSpec doesn't carry IssueID). The handler is still
	// blocked, so the run is reliably in flight and our issue must appear in
	// the Running set — no instant-finish race.
	//
	// `proceed` is released by the deferred cleanup on a failure path too: the
	// handler also selects on ctx.Done(), which Stop() cancels.
	defer close(proceed) // release the run so it can complete + the claim is freed
	waitUntil(t, 5*time.Second, "the dispatched run to appear in the Running snapshot",
		func() bool {
			for _, r := range c.Snapshot().Running {
				if r.IssueID == iss.ID {
					return true
				}
			}
			return false
		},
		func() string { return fmt.Sprintf("issue %s, snapshot=%+v", iss.ID, c.Snapshot()) })
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
