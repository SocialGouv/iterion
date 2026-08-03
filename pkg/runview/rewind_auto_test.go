package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// autoBot is the fixture every --auto test edits: a straight chain with
// a shared prompt and a conditional edge, so each detection path has
// something to bite on.
const autoBot = `prompt shared_review:
  """
  Review the work carefully.
  """

schema note:
  value: string

schema check:
  value: string
  ok: bool

agent survey:
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

workflow autobot:
  entry: survey
  survey -> plan
  plan -> implement
  implement -> verify
  verify -> done when ok
  verify -> fail
`

// seedAutoRun writes the fixture, parks a run that executed every node,
// and returns the service, the bot path, and the run id.
func seedAutoRun(t *testing.T, checkpointNode string, executedOutputs ...string) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(autoBot), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	runID := "run-auto"
	if _, err := st.CreateRun(context.Background(), runID, "autobot", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	run.FilePath = botPath
	// The source AS LAUNCHED — the whole point of Run.WorkflowSource.
	run.WorkflowSource = autoBot
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

func editBot(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bot: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(s, old, new, 1)), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
}

// TestRewindAuto_TargetsEditedNode is the headline case: change one
// node's model, and --auto rewinds to exactly that node.
func TestRewindAuto_TargetsEditedNode(t *testing.T) {
	svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	if result.NodeID != "implement" {
		t.Errorf("auto pivot = %q, want implement", result.NodeID)
	}
	if !result.AutoTargeted {
		t.Error("expected AutoTargeted=true")
	}
	if len(result.Changes) == 0 {
		t.Fatal("expected the detected changes to be reported back")
	}
	found := false
	for _, c := range result.Changes {
		if c.Kind == "agent" && c.Name == "implement" && c.Change == "modified" {
			found = true
		}
	}
	if !found {
		t.Errorf("changes = %v, want agent implement (modified)", result.Changes)
	}
}

// TestRewindAuto_SharedPromptTargetsReferencingNode: editing a prompt
// body must blame the node that references it, not the prompt.
func TestRewindAuto_SharedPromptTargetsReferencingNode(t *testing.T) {
	svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, botPath, "Review the work carefully.", "Review the work with extreme suspicion.")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	if result.NodeID != "verify" {
		t.Errorf("auto pivot = %q, want verify (the only node referencing shared_review)", result.NodeID)
	}
}

// TestRewindAuto_PicksEarliestOfSeveralEdits: two nodes edited on the
// same chain must rewind to the upstream one, so a single pass tests
// both.
func TestRewindAuto_PicksEarliestOfSeveralEdits(t *testing.T) {
	svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, botPath, "agent plan:\n  model: \"claude-opus-4-7\"", "agent plan:\n  model: \"claude-opus-5\"")
	editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	if result.NodeID != "plan" {
		t.Errorf("auto pivot = %q, want plan (upstream-most edit)", result.NodeID)
	}
}

// TestRewindAuto_EditedEdgeBlamesSourceNode: an edge is re-selected by
// the node it leaves, so that node is the pivot.
func TestRewindAuto_EditedEdgeBlamesSourceNode(t *testing.T) {
	svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, botPath, "  plan -> implement", `  plan -> implement with {value: "{{outputs.plan.value}}"}`)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	if result.NodeID != "plan" {
		t.Errorf("auto pivot = %q, want plan (the edge's source node re-selects it)", result.NodeID)
	}
}

// TestRewindAuto_IgnoresUnexecutedNodes: an edit to a node the run never
// reached is not a reason to rewind.
func TestRewindAuto_IgnoresUnexecutedNodes(t *testing.T) {
	// Only survey and plan ran — and the run is parked on plan, so
	// `verify` is genuinely unreached (cp.NodeID counts as executed too).
	svc, botPath, runID := seedAutoRun(t, "plan", "survey", "plan")
	editBot(t, botPath, "judge verify:\n  model: \"claude-opus-4-7\"", "judge verify:\n  model: \"claude-opus-5\"")

	_, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if !errors.Is(err, ErrRewindNoChange) {
		t.Fatalf("err = %v, want ErrRewindNoChange (verify never executed)", err)
	}
}

// TestRewindAuto_NoEditIsNotAnError_ButRefuses: rewinding with nothing
// changed must say so rather than silently picking a node.
func TestRewindAuto_NoEditRefuses(t *testing.T) {
	svc, _, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")

	_, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if !errors.Is(err, ErrRewindNoChange) {
		t.Fatalf("err = %v, want ErrRewindNoChange", err)
	}
}

// TestRewindAuto_WithoutRecordedSource: a run launched before the source
// was persisted must fail with a message that points at --node.
func TestRewindAuto_WithoutRecordedSource(t *testing.T) {
	svc, _, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	st := svc.RunStore()
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.WorkflowSource = ""
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}

	_, err = svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if !errors.Is(err, ErrRewindNoSourceRecorded) {
		t.Fatalf("err = %v, want ErrRewindNoSourceRecorded", err)
	}
}

// TestRewindAuto_ExplicitNodeWins: --node overrides the diff entirely.
func TestRewindAuto_ExplicitNodeWins(t *testing.T) {
	svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "plan", Auto: true})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.NodeID != "plan" {
		t.Errorf("pivot = %q, want plan (explicit --node wins over --auto)", result.NodeID)
	}
	if result.AutoTargeted {
		t.Error("AutoTargeted must be false when the caller supplied the node")
	}
}

// TestRewindAuto_LineShiftIsNotAChange guards the span-sensitivity trap:
// inserting a comment at the top moves every declaration's position, and
// must not read as "everything changed".
func TestRewindAuto_LineShiftIsNotAChange(t *testing.T) {
	svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
	b, err := os.ReadFile(botPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(botPath, append([]byte("## a new comment line\n\n"), b...), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if !errors.Is(err, ErrRewindNoChange) {
		t.Fatalf("err = %v, want ErrRewindNoChange — a line shift is not a semantic edit", err)
	}
}

// loopedAutoBot puts two nodes on a cycle, the shape that made --auto
// panic: each is the other's ancestor, so a dominance rule that ignores
// cycles eliminates both and leaves nothing to elect.
const loopedAutoBot = `schema note:
  value: string

schema check:
  value: string
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: note

agent implement:
  model: "claude-opus-4-7"
  output: note

judge verify:
  model: "claude-opus-4-7"
  output: check

workflow loopauto:
  entry: survey
  survey -> implement
  implement -> verify
  verify -> done when ok
  verify -> implement as fix(3)
`

// TestRewindAuto_TwoEditsOnOneLoop is the regression guard for the panic:
// editing two nodes that sit on the same cycle must elect one, not crash
// and not silently give up.
func TestRewindAuto_TwoEditsOnOneLoop(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(loopedAutoBot), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-loop-auto"
	if _, err := st.CreateRun(context.Background(), runID, "loopauto", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.WorkflowSource = loopedAutoBot
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:       "verify",
		Outputs:      outputsOf("survey", "implement", "verify"),
		LoopCounters: map[string]int{"fix": 1},
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")
	editBot(t, botPath, "judge verify:\n  model: \"claude-opus-4-7\"", "judge verify:\n  model: \"claude-opus-5\"")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto on a loop: %v", err)
	}
	// Both edits are on the cycle; replaying either replays the loop, so
	// the one execution reaches first is elected.
	if result.NodeID != "implement" {
		t.Errorf("pivot = %q, want implement (nearest the entry on the cycle)", result.NodeID)
	}
}

// TestRewindAuto_LoopEditIsNotMaskedByAnIndependentOne guards the silent
// half of the same bug: with cycle-mates eliminating each other, an
// unrelated third candidate would win and the loop edit would vanish.
func TestRewindAuto_LoopEditIsNotMaskedByAnIndependentOne(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(loopedAutoBot), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-loop-mask"
	if _, err := st.CreateRun(context.Background(), runID, "loopauto", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.WorkflowSource = loopedAutoBot
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "verify",
		Outputs: outputsOf("survey", "implement", "verify"),
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// survey is strictly upstream of the loop; implement is on it.
	editBot(t, botPath, "agent survey:\n  model: \"claude-opus-4-7\"", "agent survey:\n  model: \"claude-opus-5\"")
	editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	if result.NodeID != "survey" {
		t.Fatalf("pivot = %q, want survey — the strictly-upstream edit must win", result.NodeID)
	}
	// And the loop edit is covered, because implement is downstream.
	if !contains(result.DroppedNodes, "implement") {
		t.Error("implement was not invalidated; the loop edit would go untested")
	}
}

// supervisedBot exercises the three declaration kinds ast.MarshalFile
// does not mirror. Editing any of them used to be invisible to --auto.
const supervisedBot = `schema note:
  value: string

prompt watch_policy:
  """
  Intervene when the agent stalls.
  """

supervisor watchdog:
  watches: [implement]
  model: "claude-opus-4-7"
  system: watch_policy
  cooldown: "30s"

agent survey:
  model: "claude-opus-4-7"
  output: note

agent implement:
  model: "claude-opus-4-7"
  output: note

workflow supervised:
  entry: survey
  survey -> implement
  implement -> done
`

func seedSupervisedRun(t *testing.T, src string) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-sup"
	if _, err := st.CreateRun(context.Background(), runID, "supervised", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.WorkflowSource = src
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "implement",
		Outputs: outputsOf("survey", "implement", "split", "worker_a", "worker_b", "merge"),
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, botPath, runID
}

// TestRewindAuto_SupervisorEditTargetsWatchedNode: a supervisor's
// reference runs OUTWARDS (`watches: [implement]`), so the name-search
// used for shared declarations would never find it.
func TestRewindAuto_SupervisorEditTargetsWatchedNode(t *testing.T) {
	svc, botPath, runID := seedSupervisedRun(t, supervisedBot)
	editBot(t, botPath, `cooldown: "30s"`, `cooldown: "5s"`)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	if result.NodeID != "implement" {
		t.Errorf("pivot = %q, want implement (the watched node)", result.NodeID)
	}
	var sawSupervisor bool
	for _, c := range result.Changes {
		if c.Kind == "supervisor" && c.Name == "watchdog" {
			sawSupervisor = true
		}
	}
	if !sawSupervisor {
		t.Errorf("changes = %v, want the supervisor edit detected", result.Changes)
	}
}

// TestRewindAuto_UnchangedSupervisorIsNotAChange guards the other
// direction: the fallback encoder must be stable, or every rewind on a
// supervised bot would report a phantom edit.
func TestRewindAuto_UnchangedSupervisorIsNotAChange(t *testing.T) {
	svc, _, runID := seedSupervisedRun(t, supervisedBot)

	_, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if !errors.Is(err, ErrRewindNoChange) {
		t.Fatalf("err = %v, want ErrRewindNoChange — an untouched supervisor must not read as edited", err)
	}
}

// TestRewindAuto_EdgeDataMappingIsPartOfIdentity: two sibling edges can
// differ only in their `with` mapping. Without it in the key they collide
// and editing the first is invisible.
func TestRewindAuto_EdgeDataMappingIsPartOfIdentity(t *testing.T) {
	const forkBot = `schema note:
  value: string

agent survey:
  model: "claude-opus-4-7"
  output: note

router split:
  mode: fan_out_all

agent worker_a:
  model: "claude-opus-4-7"
  input: note
  output: note

agent worker_b:
  model: "claude-opus-4-7"
  input: note
  output: note

agent merge:
  model: "claude-opus-4-7"
  output: note
  await: wait_all

workflow forked:
  entry: survey
  survey -> split
  split -> worker_a with {value: "ALPHA"}
  split -> worker_b with {value: "BETA"}
  worker_a -> merge
  worker_b -> merge
  merge -> done
`
	svc, botPath, runID := seedSupervisedRun(t, forkBot)
	editBot(t, botPath, `split -> worker_a with {value: "ALPHA"}`, `split -> worker_a with {value: "GAMMA"}`)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, Auto: true})
	if err != nil {
		t.Fatalf("Rewind --auto: %v", err)
	}
	// The edge leaves `split`, and split is a fan-out router, so the
	// pivot is the router itself.
	if result.NodeID != "split" {
		t.Errorf("pivot = %q, want split", result.NodeID)
	}
}
