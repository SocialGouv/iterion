package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const pausingUnitMain = "import \"lib/gate.bot\"\n\nworkflow pause_demo:\n  entry: gate\n  gate -> done when approve\n  gate -> fail when not approve\n"

const pausingUnitGate = "schema gate_out:\n  approve: bool\n\nprompt gate_prompt:\n  Approve?\n\nhuman gate:\n  instructions: gate_prompt\n  output: gate_out\n  interaction: human\n"

// TestLaunchRecordsTheUnitFilesAndAForcedResumeRestampsThem: a run of a
// bot in several files records every file of its unit beside the main's
// text, and a forced resume over an edited fragment restamps the files
// and the hash together — so `rewind --auto` diffs against what the run
// last executed, and the next resume compares the identity it recorded.
func TestLaunchRecordsTheUnitFilesAndAForcedResumeRestampsThem(t *testing.T) {
	root := t.TempDir()
	writeUnit(t, root, map[string]string{"main.bot": pausingUnitMain, "lib/gate.bot": pausingUnitGate})
	mainBot := filepath.Join(root, "main.bot")
	svc, err := NewService(root, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launched, err := svc.Launch(context.Background(), LaunchSpec{FilePath: mainBot})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-launched.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run did not reach its human pause")
	}
	run, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkflowSource != pausingUnitMain {
		t.Fatalf("recorded main %q", run.WorkflowSource)
	}
	if len(run.WorkflowSources) != 2 || run.WorkflowSources[0].Path != "main.bot" || run.WorkflowSources[0].Text != pausingUnitMain || run.WorkflowSources[1].Path != "lib/gate.bot" || run.WorkflowSources[1].Text != pausingUnitGate {
		t.Fatalf("recorded unit files %+v, want the main then the fragment", run.WorkflowSources)
	}

	edited := "## the gate, edited while the run was parked\n" + pausingUnitGate
	writeUnit(t, root, map[string]string{"lib/gate.bot": edited})
	forced, err := svc.Resume(context.Background(), ResumeSpec{RunID: launched.RunID, FilePath: mainBot, Answers: map[string]any{"approve": true}, Force: true})
	if err != nil {
		t.Fatalf("forced Resume: %v", err)
	}
	select {
	case <-forced.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("forced resume did not finish")
	}
	run, err = svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status %q (error %q), want finished", run.Status, run.Error)
	}
	if len(run.WorkflowSources) != 2 || run.WorkflowSources[1].Text != edited {
		t.Fatalf("the forced resume did not restamp the fragment: %+v", run.WorkflowSources)
	}
	_, hash, _, err := CompileWorkflowPath(mainBot)
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkflowHash != hash {
		t.Fatalf("restamped hash %s, want the edited unit's %s", run.WorkflowHash, hash)
	}
}

// TestASingleFileRunRecordsNoUnitFiles: a bot in one file records its text
// where it always did and nothing more, so no run document of the fleet
// changes shape.
func TestASingleFileRunRecordsNoUnitFiles(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "pause_demo.bot")
	if err := os.WriteFile(botPath, []byte(pausingBot), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launched, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-launched.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run did not reach its human pause")
	}
	run, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.WorkflowSource, "human gate:") || run.WorkflowSources != nil {
		t.Fatalf("single-file run recorded source %q files %v", run.WorkflowSource, run.WorkflowSources)
	}
}
