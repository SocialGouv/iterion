package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// #3746: two delegations in one directory — the parallel branches of a
// fan-out share their worktree — each keep their own todo_write checklist.
// Keyed by the workspace alone, one branch read the other's list and worked
// its items. Mutant: the registration ignores the context's scope.
func TestClawTodoWriteIsScopedPerDelegation(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CLAW_TODOS_DIR", t.TempDir())
	reg := NewRegistry()
	if err := RegisterClawTodo(reg); err != nil {
		t.Fatal(err)
	}
	td, err := reg.Resolve("todo_write")
	if err != nil {
		t.Fatalf("resolve todo_write: %v", err)
	}
	call := func(ctx context.Context, input map[string]any) string {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		out, err := td.Execute(ctx, raw)
		if err != nil {
			t.Fatalf("todo_write %v: %v", input["action"], err)
		}
		return out
	}
	write := func(ctx context.Context, content string) {
		call(ctx, map[string]any{"action": "write", "todos": []any{map[string]any{"id": "1", "content": content, "status": "pending", "priority": "high"}}})
	}
	branchA := WithTodoScope(context.Background(), "branch-a")
	branchB := WithTodoScope(context.Background(), "branch-b")
	write(branchA, "rule on batch r0-b01")
	write(branchB, "rule on batch r0-b19")
	if got := call(branchA, map[string]any{"action": "read"}); !strings.Contains(got, "r0-b01") || strings.Contains(got, "r0-b19") {
		t.Fatalf("branch A reads %q, want its own checklist only", got)
	}
	if got := call(branchB, map[string]any{"action": "read"}); !strings.Contains(got, "r0-b19") || strings.Contains(got, "r0-b01") {
		t.Fatalf("branch B reads %q, want its own checklist only", got)
	}
	// No scope wired (a library dispatch): the per-workspace file, apart
	// from both scoped lists.
	if got := call(context.Background(), map[string]any{"action": "read"}); strings.Contains(got, "r0-b01") || strings.Contains(got, "r0-b19") {
		t.Fatalf("the unscoped list reads %q, want neither branch's items", got)
	}
}
