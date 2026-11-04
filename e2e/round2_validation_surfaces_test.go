package e2e

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestR2SchemaModeOnRunResumeAndChild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	const child = `schema result_s:
  answer: bool

tool worker:
  command: "printf '{}'"
  output: result_s

workflow advisory:
  worktree: none
  sandbox: none
  entry: worker
  worker -> done
`
	const parent = `subbot child:
  source: "child.bot"

workflow parent:
  worktree: none
  sandbox: none
  entry: child
  child -> done
`
	for _, nested := range []bool{false, true} {
		for _, strict := range []string{"", "true"} {
			t.Setenv("ITERION_STRICT_OUTPUT_VALIDATION", strict)
			dir := t.TempDir()
			path := filepath.Join(dir, "child.bot")
			if err := os.WriteFile(path, []byte(child), 0600); err != nil {
				t.Fatal(err)
			}
			if nested {
				path = filepath.Join(dir, "parent.bot")
				if err := os.WriteFile(path, []byte(parent), 0600); err != nil {
					t.Fatal(err)
				}
			}
			u, err := unit.LoadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			compiled := ir.Compile(u.Merged)
			if compiled.HasErrors() {
				t.Fatalf("fixture compile: %v", compiled.Diagnostics)
			}
			exec := model.NewClawExecutor(model.NewRegistry(), compiled.Workflow)
			storeDir := t.TempDir()
			printer := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}
			opts := cli.RunOptions{File: path, StoreDir: storeDir, RunID: "mode", Executor: exec, NoInteractive: true, MergeInto: "none"}
			err = cli.RunRun(context.Background(), opts, printer)
			st, loadErr := store.New(storeDir)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			run, loadErr := st.LoadRun(context.Background(), "mode")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if strict == "" {
				if err != nil || run.Status != store.RunStatusFinished {
					t.Errorf("default differs on CLI nested=%v: %v", nested, err)
				}
			} else {
				if err == nil || run.FailureCode != store.FailureSchemaValidation {
					t.Errorf("strict differs on CLI nested=%v: %v", nested, err)
				}
				t.Setenv("ITERION_STRICT_OUTPUT_VALIDATION", "")
				if err := cli.RunResumeWithFile(context.Background(), path, cli.ResumeOptions{RunID: "mode", StoreDir: storeDir, Executor: exec, MergeInto: "none"}, printer); err != nil {
					t.Errorf("resume differs from default nested=%v: %v", nested, err)
				}
			}
		}
	}
}
