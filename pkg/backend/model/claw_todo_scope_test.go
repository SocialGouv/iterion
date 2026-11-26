package model

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// #3746: the todo_write scope separates the delegations that share one
// worktree — two fan-out branches of a run, two rounds of a loop, two runs
// — and stays stable for one delegation, so a re-read after compaction
// finds its own list.
func TestClawTodoScopeSeparatesConcurrentDelegations(t *testing.T) {
	t.Parallel()
	run := WithRunID(context.Background(), "run-1")
	scope := func(ctx context.Context, homeScope, node string) string {
		return clawTodoScope(ctx, delegate.Task{HomeScope: homeScope, NodeID: node})
	}
	a := scope(run, RunnerHomeScopeForBranch("run-1", "branch_dispatch_1"), "adjudicate")
	b := scope(run, RunnerHomeScopeForBranch("run-1", "branch_dispatch_2"), "adjudicate")
	if a == "" || b == "" || a == b {
		t.Fatalf("two branches of one fan-out share todo scope %q / %q", a, b)
	}
	if again := scope(run, RunnerHomeScopeForBranch("run-1", "branch_dispatch_1"), "adjudicate"); again != a {
		t.Fatalf("one delegation's scope moved: %q then %q", a, again)
	}
	next := scope(WithLoopIteration(run, 1), RunnerHomeScopeForBranch("run-1", "branch_dispatch_1"), "adjudicate")
	if next == a {
		t.Fatalf("the next loop round reuses the previous round's todo scope %q", a)
	}
	if other := scope(run, RunnerHomeScopeForRun("run-1"), "refute"); other == a {
		t.Fatalf("another node shares todo scope %q", a)
	}
	if run2 := scope(WithRunID(context.Background(), "run-2"), "", "adjudicate"); run2 == "" || run2 == scope(run, "", "adjudicate") {
		t.Fatalf("two runs on one checkout share todo scope %q", run2)
	}
	if none := scope(context.Background(), "", "adjudicate"); none != "" {
		t.Fatalf("an unwired dispatch got scope %q, want the per-workspace default", none)
	}
}

// The sandboxed path (iterion.cloud's): the runner inside the container has
// no run id and no home scope, so it must key todo_write by the scope the
// launcher sends across the IPC. Red when the scope is computed after the
// placement branch, or when the IOTask drops it.
func TestClawTodoScopeCrossesTheSandboxIPC(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	run := WithRunID(context.Background(), "run-1")
	launch := func(branch string, iteration int) delegate.Task {
		task := withClawTodoScope(run, delegate.Task{NodeID: "adjudicate", Iteration: iteration, HomeScope: RunnerHomeScopeForBranch("run-1", branch)})
		raw, err := json.Marshal(b.runnerIOTask(task, nil))
		if err != nil {
			t.Fatal(err)
		}
		var io delegate.IOTask
		if err := json.Unmarshal(raw, &io); err != nil {
			t.Fatal(err)
		}
		// The runner's own context: no run id, no home scope.
		return withClawTodoScope(context.Background(), delegate.FromIOTask(io))
	}
	a, bb, next := launch("branch_dispatch_0", 0), launch("branch_dispatch_1", 0), launch("branch_dispatch_0", 1)
	if a.TodoScope == "" || bb.TodoScope == "" || next.TodoScope == "" {
		t.Fatalf("the runner lost the todo scope: %q %q %q", a.TodoScope, bb.TodoScope, next.TodoScope)
	}
	if a.TodoScope == bb.TodoScope || a.TodoScope == next.TodoScope {
		t.Fatalf("two branches or two loop rounds share a todo scope across the IPC: %q %q %q", a.TodoScope, bb.TodoScope, next.TodoScope)
	}
	if again := launch("branch_dispatch_0", 0); again.TodoScope != a.TodoScope {
		t.Fatalf("one delegation's scope moved across the IPC: %q then %q", a.TodoScope, again.TodoScope)
	}
}
