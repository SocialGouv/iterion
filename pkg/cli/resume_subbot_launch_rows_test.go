package cli_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cli "github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A CLI launch records its --model/--backend/--effort-for rows as the run's
// launch rows, and `iterion resume` hands them to the run's subbot children
// under its own flags — the cloud runner's and the studio's rule, where a
// child folds the root run's recorded launch rows. Without them a child of
// a resume dispatches on its authored model although its launch pinned
// another.
//
// Flow: parent = prep(tool) -> run_child(subbot) -> done. The child's gate
// tool fails while a marker exists (the parent persists failed_resumable);
// the marker goes, the run resumes WITHOUT flags, and the child's agent —
// claw, offline — fails naming the model it dispatched on.
func TestResume_ASubbotChildFoldsTheRunsRecordedLaunchRows(t *testing.T) {
	hermeticSandbox(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	marker := filepath.Join(dir, "BOOM")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	child := fmt.Sprintf(`prompt usr:
  Go.

schema out:
  ok: bool

tool gate:
  command: `+"`if [ -f %s ]; then echo boom >&2; exit 1; fi; printf '{\"ok\":true}'`"+`
  output: out

agent act:
  backend: "claw"
  model: "claude-sonnet-4-5"
  user: usr
  output: out

workflow child:
  worktree: none
  entry: gate
  gate -> act
  act -> done
`, marker)
	parent := `schema pout:
  ready: bool
schema out:
  ok: bool
tool prep:
  command: ` + "`printf '{\"ready\":true}'`" + `
  output: pout
subbot run_child:
  source: "child.bot"
  output: out
workflow parent:
  worktree: none
  entry: prep
  prep -> run_child
  run_child -> done
`
	writeFixture(t, dir, "child.bot", child)
	parentPath := writeFixture(t, dir, "parent.bot", parent)
	storeDir := filepath.Join(dir, "store")

	p1, _ := newTestPrinter(cli.OutputHuman)
	if err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     parentPath,
		StoreDir: storeDir,
		RunID:    "launch-rows-resume-1",
		ModelFor: []string{"act=openai/gpt-6-sol"},
	}, p1); err == nil {
		t.Fatal("initial run should fail (the child's gate hits the marker)")
	}
	s, _ := store.New(storeDir)
	r, err := s.LoadRun(context.Background(), "launch-rows-resume-1")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}
	if want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}; !reflect.DeepEqual(r.LaunchModelOverrides, want) {
		t.Fatalf("run launch rows = %+v, want the launch flags %+v", r.LaunchModelOverrides, want)
	}
	// The child's document records the tree's launch rows too: a resume of
	// the child on its own hands them to its children.
	childRun, err := s.LoadRun(context.Background(), r.SubbotChildren["run_child"])
	if err != nil {
		t.Fatalf("load the child run: %v", err)
	}
	if want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}; !reflect.DeepEqual(childRun.LaunchModelOverrides, want) {
		t.Fatalf("child launch rows = %+v, want the tree's %+v", childRun.LaunchModelOverrides, want)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	p2, _ := newTestPrinter(cli.OutputHuman)
	err = cli.RunResumeWithFile(context.Background(), parentPath, cli.ResumeOptions{
		RunID:    "launch-rows-resume-1",
		StoreDir: storeDir,
	}, p2)
	if err == nil {
		t.Fatal("precondition: the child's claw agent fails offline (no credential)")
	}
	gpt, sonnet := strings.Contains(err.Error(), "gpt-6-sol"), strings.Contains(err.Error(), "claude-sonnet-4-5")
	if !gpt || sonnet {
		t.Fatalf("resumed child dispatched on gpt-6-sol=%v claude-sonnet-4-5=%v, want the run's launch row gpt-6-sol (error: %v)", gpt, sonnet, err)
	}
}
