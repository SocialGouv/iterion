package cli_test

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentR53ResumeBorrowOnFailure(t *testing.T) { independentResumeOwnership(t, true) }

type independentExternalExecutor struct {
	closed int
	calls  int
}

func (e *independentExternalExecutor) Close() error {
	e.closed++
	return nil
}
func (e *independentExternalExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	e.calls++
	return nil, errors.New("independent execution refusal")
}
func independentResumeOwnership(t *testing.T, external bool) {
	if !external {
		if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
			t.Skip("incarnation admission remains disabled")
		}
	}
	hermeticSandbox(t)
	ctx := context.Background()
	dir := t.TempDir()
	bot := writeFixture(t, dir, "operator.bot", `tool action:
  command: "echo never"
workflow operator_resume:
  worktree: none
  sandbox: none
  entry: action
  action -> done
`)
	sd := filepath.Join(dir, "store")
	s, e := store.New(sd)
	if e != nil {
		t.Fatal(e)
	}
	protocol := store.IncarnationRunData
	if external {
		protocol = store.HistoricalRunData
	}
	r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "operator_resume", Format: 2, DataProtocol: protocol})
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: 2, Protocol: r.DataProtocol, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunDataBinding(ctx, b)
	if e != nil {
		t.Fatal(e)
	}
	ctx, _, e = store.LoadRunWithValueFormat(ctx, s, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	r.Checkpoint = &store.Checkpoint{NodeID: "action"}
	r.Status = store.RunStatusPausedOperator
	if e = s.SaveRun(ctx, r); e != nil {
		t.Fatal("setup SaveRun", e)
	}
	count := func() int {
		es, e := os.ReadDir("/proc/self/fd")
		if e != nil {
			t.Fatal(e)
		}
		n := 0
		for _, v := range es {
			x, _ := os.Readlink("/proc/self/fd/" + v.Name())
			if strings.HasPrefix(x, sd) {
				t.Log("fd", x)
				n++
			}
		}
		return n
	}
	before := count()
	p, _ := newTestPrinter(cli.OutputHuman)
	opts := cli.ResumeOptions{RunID: r.ID, StoreDir: sd}
	custom := &independentExternalExecutor{}
	if external {
		opts.Executor = custom
	}
	if e = cli.RunResumeWithFile(ctx, bot, opts, p); e == nil {
		t.Fatal("expected execution refusal")
	}
	if custom.calls == 0 {
		t.Fatal("custom executor not exercised")
	}
	if custom.closed != 0 {
		t.Fatalf("external executor closed %d times", custom.closed)
	}
	after := count()
	if after != before {
		t.Fatalf("owned grant leaked descriptors: before=%d after=%d", before, after)
	}
}
