package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/store"
)

type twoLoopBackend struct {
	mu    sync.Mutex
	calls map[string]int
	trace []string
}

func (b *twoLoopBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls[task.NodeID]++
	n := b.calls[task.NodeID]
	b.trace = append(b.trace, task.NodeID)
	out := map[string]any{"again": false}
	switch task.NodeID {
	case "node_x":
		// Each visit needs both corrections: invalid, invalid, valid.
		out = map[string]any{"verdict": fmt.Sprintf("wrong-%d", n)}
		if n%3 == 0 {
			out = map[string]any{"verdict": "pass"}
		}
	case "node_q", "node_s":
		out = map[string]any{"again": n == 1}
	}
	return delegate.Result{Output: out, BackendName: delegate.BackendClaudeCode, Tokens: 10}, nil
}

func TestR2TwoLoopCycleSharesOneAllowance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "c.bot")
	if err := os.WriteFile(path, []byte(twoLoopCycleBot), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := unit.LoadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	cr := ir.Compile(u.Merged)
	b := &twoLoopBackend{calls: map[string]int{}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	exec := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	storeDir := t.TempDir()
	err = cli.RunRun(context.Background(), cli.RunOptions{File: path, StoreDir: storeDir, RunID: "two-loop", Executor: exec, NoInteractive: true, MergeInto: "none"}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	s, _ := store.New(storeDir)
	r, _ := s.LoadRun(context.Background(), "two-loop")
	if err != nil || r.Status != store.RunStatusFinished || b.calls["node_x"] != 6 {
		t.Fatalf("second invocation inherited allowance: calls=%d status=%s err=%v", b.calls["node_x"], r.Status, err)
	}
	t.Logf("trace=%v", b.trace)
	t.Logf("iterion run: node_x calls=%d status=%s code=%s err=%.200v", b.calls["node_x"], r.Status, r.FailureCode, err)
	for k, ep := range r.OutputCorrections {
		t.Logf("ledger[%s] invocation=%q attempts=%d status=%s", k, ep.InvocationID, ep.Attempts, ep.Status)
	}
}

const twoLoopCycleBot = `schema flag_s:
  again: bool

schema x_s:
  verdict: string [enum: "pass"]

agent node_p:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: flag_s

agent node_x:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: x_s

agent node_q:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: flag_s

agent node_r:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: flag_s

agent node_s:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: flag_s

workflow two_loop_cycle:
  worktree: none
  sandbox: none
  entry: node_p

  node_p -> node_x
  node_x -> node_q
  node_q -> node_r when again as l2(3)
  node_q -> done when not again
  node_r -> node_s
  node_s -> node_p when again as l1(3)
  node_s -> done when not again
`

func TestR2ScenarioDoesNotCompleteToolOutput(t *testing.T) {
	exec := newScenarioExecutor()
	exec.schemas = map[string]*ir.Schema{"shape": {Name: "shape", Fields: []*ir.SchemaField{{Name: "missing", Type: ir.FieldTypeString}}}}
	tool := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool"}, SchemaFields: ir.SchemaFields{OutputSchema: "shape"}}
	exec.on("tool", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	output, err := exec.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, invented := output["missing"]; invented {
		t.Fatalf("tool scenario invented missing field: %v", output)
	}
}
