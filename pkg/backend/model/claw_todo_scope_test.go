package model

import (
	"context"
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
