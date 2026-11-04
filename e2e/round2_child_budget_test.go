package e2e

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

const budgetParent = `subbot child:
  source: "child.bot"

workflow budget_parent:
  worktree: none
  sandbox: none
  entry: child
  budget:
    max_tokens: 1000

  child -> done
`

const budgetChild = `schema verdict_s:
  verdict: string

agent step1:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: verdict_s

agent step2:
  model: "claude-opus-4-7"
  backend: "claude_code"
  output: verdict_s

workflow budget_child:
  worktree: none
  sandbox: none
  entry: step1
  budget:
    max_tokens: 15

  step1 -> step2
  step2 -> done
`

type tenTokens struct {
	mu    sync.Mutex
	calls int
}

func (b *tenTokens) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	return delegate.Result{Output: map[string]any{"verdict": "ok"}, BackendName: delegate.BackendClaudeCode, Tokens: 10}, nil
}

func TestR2ChildBudgetExceededReachesTheParent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.bot")
	_ = os.WriteFile(parent, []byte(budgetParent), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "child.bot"), []byte(budgetChild), 0o644)
	u, _ := unit.LoadDir(filepath.Join(dir, "child.bot"))
	cr := ir.Compile(u.Merged)
	b := &tenTokens{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	exec := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	storeDir := t.TempDir()
	p := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}
	err := cli.RunRun(context.Background(), cli.RunOptions{File: parent, StoreDir: storeDir, RunID: "bp", Executor: exec, NoInteractive: true, MergeInto: "none"}, p)
	s, _ := store.New(storeDir)
	r, _ := s.LoadRun(context.Background(), "bp")
	var re *runtime.RuntimeError
	code := ""
	if errors.As(err, &re) {
		code = string(re.Code)
	}
	hint := runtime.RemedyOf(err).Hint
	if !strings.Contains(hint, "child.bot") || !strings.Contains(hint, "child") {
		t.Errorf("remedy does not name child budget: %q", hint)
	}
	t.Logf("run: parent status=%s storedCode=%s returnedCode=%s calls=%d err=%.220v", r.Status, r.FailureCode, code, b.calls, err)
	for i := 1; i <= 2; i++ {
		before := b.calls
		err = cli.RunResumeWithFile(context.Background(), parent, cli.ResumeOptions{RunID: "bp", StoreDir: storeDir, Executor: exec, MergeInto: "none", Budget: cli.BudgetOverrides{MaxTokens: 100000 * i}}, p)
		r, _ = s.LoadRun(context.Background(), "bp")
		t.Logf("resume #%d with the PARENT cap raised to %d tokens: status=%s storedCode=%s extraCalls=%d err=%.200v", i, 100000*i, r.Status, r.FailureCode, b.calls-before, err)
	}
}
