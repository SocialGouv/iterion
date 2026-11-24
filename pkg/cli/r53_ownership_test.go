package cli_test

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewR53ResumeOwnsGrant(t *testing.T)        { r53ResumeOwnership(t, false) }
func TestReviewR53ResumeExternalExecutor(t *testing.T) { r53ResumeOwnership(t, true) }

type r53ExternalExecutor struct {
	approveExecutor
	closed int
}

func (e *r53ExternalExecutor) Close() error {
	e.closed++
	return nil
}
func r53ResumeOwnership(t *testing.T, external bool) {
	if !external {
		if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
			t.Skip("incarnation admission remains disabled")
		}
	}
	hermeticSandbox(t)
	ctx := context.Background()
	dir := t.TempDir()
	bot := writeFixture(t, dir, "operator.bot", `workflow operator_resume:
  worktree: none
  sandbox: none
  entry: done
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
	r.Checkpoint = &store.Checkpoint{NodeID: "done"}
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
	custom := &r53ExternalExecutor{}
	if external {
		opts.Executor = custom
	}
	if e = cli.RunResumeWithFile(ctx, bot, opts, p); e != nil {
		t.Fatal(e)
	}
	if custom.closed != 0 {
		t.Fatalf("external executor closed %d times", custom.closed)
	}
	after := count()
	if after != before {
		t.Fatalf("owned grant leaked descriptors: before=%d after=%d", before, after)
	}
}
