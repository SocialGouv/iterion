package cli

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
)

type r53CustomChild struct{ closes int }

func (e *r53CustomChild) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (e *r53CustomChild) Close() error { e.closes++; return nil }
func TestReviewR53ChildCustomOwnershipUnchanged(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	d := t.TempDir()
	if e := os.WriteFile(filepath.Join(d, "child.bot"), []byte("workflow child:\n  worktree: none\n  sandbox: none\n  entry: done\n"), 0600); e != nil {
		t.Fatal(e)
	}
	st, e := store.New(filepath.Join(d, "state"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.CreateRun(context.Background(), "parent", "parent", nil); e != nil {
		t.Fatal(e)
	}
	custom := &r53CustomChild{}
	run := subbotRunnerForCLI(filepath.Join(d, "parent.bot"), st.Root(), st, iterlog.Nop(), RunOptions{Executor: custom})
	ctx, _ := store.WithRunValueFormat(context.Background(), store.RunValueFormat{RunID: "parent", Format: 1})
	_, e = run(ctx, runtime.SubbotRequest{Source: "child.bot", ParentRunID: "parent", NodeID: "child", ReattachKey: "child", WorkDir: d})
	if e != nil {
		t.Fatal(e)
	}
	if custom.closes != 1 {
		t.Fatalf("legacy CLI child ownership changed: Close%d want1", custom.closes)
	}
}
