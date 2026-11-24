package cli_test

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

const fanParent = `schema tally:
  count: int

schema side_s:
  ok: bool

subbot child:
  source: "child.bot"

compute other:
  output: side_s
  expr:
    ok: "true"

router fork:
  mode: fan_out_all

compute gather:
  await: wait_all
  output: tally
  expr:
    count: "1"

workflow budget_parent:
  worktree: none
  sandbox: none
  entry: fork
  budget:
    max_tokens: 1000

  fork -> child
  fork -> other
  child -> gather
  other -> gather
  gather -> done
`

const trunkParent = `subbot child:
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

func TestR3ChildBudgetHintTrunkVersusFanOut(t *testing.T) {
	for _, tc := range []struct{ name, parent string }{{"trunk", trunkParent}, {"fan_out_all", fanParent}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			dir := t.TempDir()
			parent := filepath.Join(dir, "parent.bot")
			_ = os.WriteFile(parent, []byte(tc.parent), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "child.bot"), []byte(budgetChild), 0o644)
			u, err := unit.LoadDir(filepath.Join(dir, "child.bot"))
			if err != nil {
				t.Fatal(err)
			}
			cr := ir.Compile(u.Merged)
			b := &tenTokens{}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			exec := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			storeDir := t.TempDir()
			p := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}
			err = cli.RunRun(context.Background(), cli.RunOptions{File: parent, StoreDir: storeDir, RunID: "bp", Executor: exec, NoInteractive: true, MergeInto: "none"}, p)
			s, _ := store.New(storeDir)
			r, _ := s.LoadRun(context.Background(), "bp")
			var re *runtime.RuntimeError
			code := ""
			if errors.As(err, &re) {
				code = string(re.Code)
			}
			t.Logf("%s: status=%s storedCode=%s returnedCode=%s calls=%d", tc.name, r.Status, r.FailureCode, code, b.calls)
			if !strings.Contains(runtime.RemedyOf(err).Hint, "CHILD bot") || r.FailureCode != store.FailureBudgetExceeded {
				t.Errorf("child hint/code lost: %s / %s", r.FailureCode, runtime.RemedyOf(err).Hint)
			}
			t.Logf("%s: persisted error=%.400s", tc.name, r.Error)
		})
	}
}
