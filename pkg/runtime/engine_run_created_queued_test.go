package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// queuedCreatingStore mimics the cloud store's CreateRun/CreateChildRun,
// which insert a fresh row as `queued` (the publisher's shape) — the
// filesystem store inserts `running`, which is why the direct path never
// showed the gap locally.
type queuedCreatingStore struct {
	*store.FilesystemRunStore
}

func (q *queuedCreatingStore) CreateRun(ctx context.Context, id, workflowName string, inputs map[string]any) (*store.Run, error) {
	run, err := q.FilesystemRunStore.CreateRun(ctx, id, workflowName, inputs)
	if err != nil {
		return nil, err
	}
	if err := q.UpdateRunStatus(ctx, id, store.RunStatusQueued, ""); err != nil {
		return nil, err
	}
	run.Status = store.RunStatusQueued
	return run, nil
}

func (q *queuedCreatingStore) CreateChildRun(ctx context.Context, id, workflowName, parentRunID string, inputs map[string]any) (*store.Run, error) {
	run, err := q.FilesystemRunStore.CreateChildRun(ctx, id, workflowName, parentRunID, inputs)
	if err != nil {
		return nil, err
	}
	if err := q.UpdateRunStatus(ctx, id, store.RunStatusQueued, ""); err != nil {
		return nil, err
	}
	run.Status = store.RunStatusQueued
	return run, nil
}

// TestRunDirectCreateQueuedBecomesRunning pins the direct path against a
// store that creates rows as `queued`: the engine is about to execute the
// run, so the row must read `running` at once — as the pickup path makes it.
// Before this, a subbot child on a pod stayed `queued` for its whole life
// and ended `finished` without ever having been running.
func TestRunDirectCreateQueuedBecomesRunning(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &queuedCreatingStore{FilesystemRunStore: fs}
	src := `schema out:
  ok: bool

tool work:
  command: ` + "`printf '{\"ok\":true}'`" + `
  output: out

workflow w:
  entry: work
  work -> done
`
	pr := parser.Parse("w.bot", src)
	if pr.File == nil {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile: %v", cr.Diagnostics)
	}
	var seen []store.RunStatus
	exec := newStubExecutor()
	exec.on("work", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	observe := WithOnNodeFinished(func(rid, nodeID string, out map[string]any) {
		if run, lerr := st.LoadRun(context.Background(), rid); lerr == nil {
			seen = append(seen, run.Status)
		}
	})
	for _, child := range []bool{false, true} {
		runID := "direct-queued"
		opts := []EngineOption{WithWorkDir(t.TempDir()), WithSandboxOverride("none"), observe}
		if child {
			runID = "direct-queued-child"
			opts = append(opts, WithParentRunID("parent-1"))
		}
		eng := New(cr.Workflow, st, exec, opts...)
		seen = nil
		if err := eng.Run(context.Background(), runID, nil); err != nil {
			t.Fatalf("child=%v run: %v", child, err)
		}
		if len(seen) == 0 || seen[0] != store.RunStatusRunning {
			t.Fatalf("child=%v: status observed while the first node ran = %v, want running (the row was created queued)", child, seen)
		}
		final, err := st.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != store.RunStatusFinished {
			t.Fatalf("child=%v: final status = %s", child, final.Status)
		}
	}
}
