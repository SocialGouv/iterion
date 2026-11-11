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

// launchRowsTree is a CLI parent whose subbot child fails at its gate tool
// while marker exists: parent = prep(tool) -> run_child(subbot) -> done,
// child = gate(tool) -> act(agent: claw on claude-sonnet-4-5) -> done. Once
// the marker goes, the child's agent — claw, offline — fails naming the
// model it dispatched on.
type launchRowsTree struct {
	parentPath, childPath, storeDir, marker string
}

func newLaunchRowsTree(t *testing.T) launchRowsTree {
	t.Helper()
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
	return launchRowsTree{
		childPath:  writeFixture(t, dir, "child.bot", child),
		parentPath: writeFixture(t, dir, "parent.bot", parent),
		storeDir:   filepath.Join(dir, "store"),
		marker:     marker,
	}
}

// launch runs the parent under modelFor; the child fails at its gate, so
// the run persists failed_resumable. It returns the root's document.
func (tr launchRowsTree) launch(t *testing.T, runID string, modelFor ...string) *store.Run {
	t.Helper()
	p, _ := newTestPrinter(cli.OutputHuman)
	if err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     tr.parentPath,
		StoreDir: tr.storeDir,
		RunID:    runID,
		ModelFor: modelFor,
	}, p); err == nil {
		t.Fatal("initial run should fail (the child's gate hits the marker)")
	}
	r := tr.load(t, runID)
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}
	return r
}

func (tr launchRowsTree) load(t *testing.T, runID string) *store.Run {
	t.Helper()
	s, err := store.New(tr.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run %s: %v", runID, err)
	}
	return r
}

// child is the document of the root's subbot child.
func (tr launchRowsTree) child(t *testing.T, rootID string) *store.Run {
	t.Helper()
	return tr.load(t, tr.load(t, rootID).SubbotChildren["run_child"])
}

func (tr launchRowsTree) unblock(t *testing.T) {
	t.Helper()
	if err := os.Remove(tr.marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
}

// dispatched names the one of models the child's error names: the model
// its agent dispatched on.
func dispatched(t *testing.T, err error, models ...string) string {
	t.Helper()
	if err == nil {
		t.Fatal("precondition: the child's claw agent fails offline (no credential)")
	}
	var named []string
	for _, m := range models {
		if strings.Contains(err.Error(), m) {
			named = append(named, m)
		}
	}
	if len(named) != 1 {
		t.Fatalf("precondition: the error names exactly one of %q, got %q: %v", models, named, err)
	}
	return named[0]
}

// A CLI launch records its --model/--backend/--effort-for rows as the run's
// launch rows, and `iterion resume` hands them to the run's subbot children
// under its own flags — the cloud runner's and the studio's rule, where a
// child folds the root run's recorded launch rows. Without them a child of
// a resume dispatches on its authored model although its launch pinned
// another. The resume here carries no flag.
func TestResume_ASubbotChildFoldsTheRunsRecordedLaunchRows(t *testing.T) {
	tr := newLaunchRowsTree(t)
	r := tr.launch(t, "launch-rows-resume-1", "act=openai/gpt-6-sol")
	if want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}; !reflect.DeepEqual(r.LaunchModelOverrides, want) {
		t.Fatalf("run launch rows = %+v, want the launch flags %+v", r.LaunchModelOverrides, want)
	}
	// The child's document records the tree's launch rows too: a resume of
	// the child on its own hands them to its children.
	if want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}; !reflect.DeepEqual(tr.child(t, r.ID).LaunchModelOverrides, want) {
		t.Fatalf("child launch rows = %+v, want the tree's %+v", tr.child(t, r.ID).LaunchModelOverrides, want)
	}
	tr.unblock(t)
	p2, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunResumeWithFile(context.Background(), tr.parentPath, cli.ResumeOptions{RunID: r.ID, StoreDir: tr.storeDir}, p2)
	if got := dispatched(t, err, "gpt-6-sol", "claude-sonnet-4-5"); got != "gpt-6-sol" {
		t.Fatalf("resumed child dispatched on %s, want the run's launch row gpt-6-sol (error: %v)", got, err)
	}
}

// A run document written before the raw launch rows existed records none;
// with no routing policy (the CLI lane folds none) its rows ARE its launch
// rows (runview.SubbotRootLaunchRows), and `iterion resume` hands them to
// the run's subbot children as a run launched today does.
func TestResume_ASubbotChildOfARunWithoutRecordedLaunchRowsFoldsItsRows(t *testing.T) {
	tr := newLaunchRowsTree(t)
	r := tr.launch(t, "launch-rows-legacy-1", "act=openai/gpt-6-sol")
	r.LaunchModelOverrides = nil
	s, err := store.New(tr.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got := tr.load(t, r.ID); len(got.LaunchModelOverrides) != 0 || len(got.ModelOverrides) == 0 {
		t.Fatalf("precondition: the document records its rows and no raw launch row (rows %+v, launch rows %+v)", got.ModelOverrides, got.LaunchModelOverrides)
	}
	tr.unblock(t)
	p2, _ := newTestPrinter(cli.OutputHuman)
	err = cli.RunResumeWithFile(context.Background(), tr.parentPath, cli.ResumeOptions{RunID: r.ID, StoreDir: tr.storeDir}, p2)
	if got := dispatched(t, err, "gpt-6-sol", "claude-sonnet-4-5"); got != "gpt-6-sol" {
		t.Fatalf("resumed child dispatched on %s, want the document's row gpt-6-sol (error: %v)", got, err)
	}
}

// A CLI child's document records the rows its root's document records as
// its rows too — this lane folds no policy, so they are the child's fold —
// and `iterion resume` of the child on its own runs its nodes on them, as
// a lone resume of a cloud or in-process child replays its fold.
func TestResume_ASubbotChildResumedAloneRunsTheRootsLaunchRows(t *testing.T) {
	tr := newLaunchRowsTree(t)
	r := tr.launch(t, "launch-rows-lone-1", "act=openai/gpt-6-sol")
	child := tr.child(t, r.ID)
	tr.unblock(t)
	p2, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunResumeWithFile(context.Background(), tr.childPath, cli.ResumeOptions{RunID: child.ID, StoreDir: tr.storeDir, Force: true}, p2)
	if got := dispatched(t, err, "gpt-6-sol", "claude-sonnet-4-5"); got != "gpt-6-sol" {
		t.Fatalf("the child resumed alone dispatched on %s, want the root's launch row gpt-6-sol (error: %v)", got, err)
	}
	if want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}; !reflect.DeepEqual(child.ModelOverrides, want) {
		t.Fatalf("child rows = %+v, want the root's launch rows %+v", child.ModelOverrides, want)
	}
}

// A resume's flags are that attempt's alone: they outrank the recorded rows
// for the children the attempt spawns, and neither the root's document nor
// a child's records them — the child's records the root's launch rows,
// byte for byte, never the flags merged over them as one flat list.
func TestResume_ASubbotChildDocumentRecordsTheRootsLaunchRowsNotTheAttemptsFlags(t *testing.T) {
	tr := newLaunchRowsTree(t)
	r := tr.launch(t, "launch-rows-flags-1", "act=openai/gpt-6-sol")
	tr.unblock(t)
	p2, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunResumeWithFile(context.Background(), tr.parentPath, cli.ResumeOptions{RunID: r.ID, StoreDir: tr.storeDir, ModelFor: []string{"*=openai/gpt-5.5-mini"}}, p2)
	if got := dispatched(t, err, "gpt-6-sol", "gpt-5.5-mini", "claude-sonnet-4-5"); got != "gpt-5.5-mini" {
		t.Fatalf("the attempt's child dispatched on %s, want the attempt's flag gpt-5.5-mini over the recorded row (error: %v)", got, err)
	}
	want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}
	if root := tr.load(t, r.ID); !reflect.DeepEqual(root.LaunchModelOverrides, want) || !reflect.DeepEqual(root.ModelOverrides, want) {
		t.Fatalf("root document rows %+v, launch rows %+v, want both its launch's %+v", root.ModelOverrides, root.LaunchModelOverrides, want)
	}
	child := tr.child(t, r.ID)
	if !reflect.DeepEqual(child.LaunchModelOverrides, want) || !reflect.DeepEqual(child.ModelOverrides, want) {
		t.Fatalf("child document rows %+v, launch rows %+v, want both the root's launch rows %+v", child.ModelOverrides, child.LaunchModelOverrides, want)
	}
}
