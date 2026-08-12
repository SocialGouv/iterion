package cli_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	cli "github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_RunWithSubbot is the regression for the resume path missing the
// SubbotRunner wiring: `iterion resume` built its engine WITHOUT
// WithSubbotRunner, so ANY resumed run whose remaining graph contained a
// subbot node died with "no SubbotRunner is wired" — runs with subbots were
// unresumable as a class (contradicting the documented "all failure scenarios
// are resumable" contract).
//
// Flow: parent = prep(tool) -> run_child(subbot) -> done. The child fails
// while a marker file exists (prep has checkpointed, so the parent persists
// failed_resumable), then the marker is removed and resume must re-run the
// subbot to completion.
func TestResume_RunWithSubbot(t *testing.T) {
	hermeticSandbox(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "BOOM")
	outFile := filepath.Join(dir, "child-out.txt")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	child := fmt.Sprintf(`schema out:
  ok: bool
tool work:
  command: `+"`if [ -f %s ]; then echo boom >&2; exit 1; fi; printf done > %s; printf '{\"ok\":true}'`"+`
  output: out
workflow child:
  worktree: none
  entry: work
  work -> done
`, marker, outFile)
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

	// 1. Initial run: the subbot child fails -> parent failed_resumable
	//    (prep checkpointed first, so the failure is past the first node).
	p1, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     parentPath,
		StoreDir: storeDir,
		RunID:    "subbot-resume-1",
	}, p1)
	if err == nil {
		t.Fatal("initial run should fail (child hits the marker)")
	}
	s, _ := store.New(storeDir)
	r, err := s.LoadRun(context.Background(), "subbot-resume-1")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}

	// 2. Clear the failure trigger and resume: the subbot must run.
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	p2, _ := newTestPrinter(cli.OutputHuman)
	if err := cli.RunResumeWithFile(context.Background(), parentPath, cli.ResumeOptions{
		RunID:    "subbot-resume-1",
		StoreDir: storeDir,
	}, p2); err != nil {
		t.Fatalf("resume failed (SubbotRunner not wired on the resume path?): %v", err)
	}

	r, err = s.LoadRun(context.Background(), "subbot-resume-1")
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("status after resume = %s, want finished", r.Status)
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Errorf("child tool never ran on resume (output missing): %v", err)
	}
}
