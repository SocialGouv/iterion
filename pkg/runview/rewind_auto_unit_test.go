package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// autoBot split in two: the main keeps the prompt, the schemas and the
// workflow; the nodes live in lib/nodes.bot.
const autoUnitMain = `import "lib/nodes.bot"

prompt shared_review:
  """
  Review the work carefully.
  """

schema note:
  value: string

schema check:
  value: string
  ok: bool

workflow autobot:
  entry: survey
  survey -> plan
  plan -> implement
  implement -> verify
  verify -> done when ok
  verify -> fail
`

const autoUnitNodes = `agent survey:
  model: "claude-opus-4-7"
  output: note

agent plan:
  model: "claude-opus-4-7"
  output: note

agent implement:
  model: "claude-opus-4-7"
  input: note
  output: note

judge verify:
  model: "claude-opus-4-7"
  system: shared_review
  output: check
`

// seedAutoUnitRun is seedAutoRun for the two-file bot: the run recorded its
// unit file by file, as a launch does.
func seedAutoUnitRun(t *testing.T, checkpointNode string, executedOutputs ...string) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(autoUnitMain), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "nodes.bot"), []byte(autoUnitNodes), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	runID := "run-auto-unit"
	if _, err := st.CreateRun(context.Background(), runID, "autobot", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	run.FilePath = botPath
	run.WorkflowSource = autoUnitMain
	run.WorkflowSources = []store.WorkflowSourceFile{{Path: "main.bot", Text: autoUnitMain}, {Path: "lib/nodes.bot", Text: autoUnitNodes}}
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:  checkpointNode,
		Outputs: outputsOf(executedOutputs...),
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save run: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, botPath, runID
}

// TestRewindAuto_TargetsAnEditedFragment: an edit in a fragment is seen
// like one in the main — the run recorded its unit, and the current unit
// is read from disk — so the pivot is the node the fragment declares.
func TestRewindAuto_TargetsAnEditedFragment(t *testing.T) {
	svc, botPath, runID := seedAutoUnitRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, filepath.Join(filepath.Dir(botPath), "lib", "nodes.bot"), "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.NodeID != "implement" || !result.AutoTargeted {
		t.Fatalf("pivot = %q (auto %v), want implement", result.NodeID, result.AutoTargeted)
	}
	// And an edit in the main of the same unit is seen as before.
	svc, botPath, runID = seedAutoUnitRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, botPath, "verify -> done when ok", "verify -> done when not ok")
	result, err = svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind after a main edit: %v", err)
	}
	if result.NodeID != "verify" {
		t.Fatalf("pivot = %q, want verify", result.NodeID)
	}
}

// TestRewindAuto_RefusesAUnitRunWithoutItsFiles: a run of a bot in several
// files that recorded its main alone cannot be diffed on the main — the
// fragment's edit would be invisible — so --auto refuses and names --node.
func TestRewindAuto_RefusesAUnitRunWithoutItsFiles(t *testing.T) {
	svc, botPath, runID := seedAutoUnitRun(t, "verify", "survey", "plan", "implement", "verify")
	st := svc.RunStore()
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.WorkflowSources = nil
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	editBot(t, filepath.Join(filepath.Dir(botPath), "lib", "nodes.bot"), "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

	_, err = svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if !errors.Is(err, ErrRewindUnitSourcesIncomplete) {
		t.Fatalf("err = %v, want ErrRewindUnitSourcesIncomplete", err)
	}
}
