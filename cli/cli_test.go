package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/cli"
	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/runtime"
	"github.com/SocialGouv/iterion/store"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

const minimalWorkflow = `
prompt sys:
  You are a reviewer.

prompt usr:
  Review the PR: {{input.description}}

schema review_input:
  description: string

schema review_output:
  approved: bool
  summary: string

agent reviewer:
  model: "test-model"
  input: review_input
  output: review_output
  system: sys
  user: usr
  session: fresh

workflow test_workflow:
  vars:
    description: string = "default"

  entry: reviewer

  reviewer -> done when approved
  reviewer -> fail when not approved
`

const humanWorkflow = `
prompt sys:
  You are a reviewer.

prompt usr:
  Review: {{input.description}}

prompt human_instr:
  Please provide your feedback.

schema input_schema:
  description: string

schema output_schema:
  approved: bool
  summary: string

schema human_output:
  feedback: string

agent reviewer:
  model: "test-model"
  input: input_schema
  output: output_schema
  system: sys
  user: usr
  session: fresh

human clarify:
  interaction: human
  input: output_schema
  output: human_output
  instructions: human_instr
  min_answers: 1

workflow test_human_workflow:
  entry: reviewer

  reviewer -> clarify when not approved
  reviewer -> done when approved
  clarify -> done
`

// writeFixture writes content to a temp file and returns its path.
func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestPrinter creates a Printer with a buffer for capturing output.
func newTestPrinter(format cli.OutputFormat) (*cli.Printer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &cli.Printer{W: buf, Format: format}, buf
}

// ---------------------------------------------------------------------------
// Tests — validate
// ---------------------------------------------------------------------------

func TestValidate_Valid(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "test.iter", minimalWorkflow)

	p, buf := newTestPrinter(cli.OutputHuman)
	err := cli.RunValidate(path, p)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "result: OK") {
		t.Errorf("expected OK in output, got:\n%s", out)
	}
	if !strings.Contains(out, "test_workflow") {
		t.Errorf("expected workflow name in output, got:\n%s", out)
	}
}

func TestValidate_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "test.iter", minimalWorkflow)

	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunValidate(path, p)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result cli.ValidateResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v", err)
	}
	if !result.Valid {
		t.Error("expected valid=true")
	}
	if result.WorkflowName != "test_workflow" {
		t.Errorf("expected workflow_name=test_workflow, got %q", result.WorkflowName)
	}
}

func TestValidate_Invalid(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "bad.iter", "this is not valid iterion DSL")

	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunValidate(path, p)
	if err == nil {
		t.Fatal("expected error for invalid file")
	}
}

func TestValidate_FileNotFound(t *testing.T) {
	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunValidate("/nonexistent/file.iter", p)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ---------------------------------------------------------------------------
// Tests — run
// ---------------------------------------------------------------------------

// approveExecutor always returns approved=true.
type approveExecutor struct{}

func (e *approveExecutor) Execute(_ context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	out := make(map[string]interface{})
	for k, v := range input {
		out[k] = v
	}
	out["approved"] = true
	out["summary"] = "looks good"
	return out, nil
}

func TestRun_Success(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "test.iter", minimalWorkflow)
	storeDir := filepath.Join(dir, "store")

	p, buf := newTestPrinter(cli.OutputHuman)
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     path,
		StoreDir: storeDir,
		RunID:    "test-run-1",
		Executor: &approveExecutor{},
	}, p)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "FINISHED") {
		t.Errorf("expected FINISHED in output, got:\n%s", out)
	}

	// Verify run is persisted.
	s, _ := store.New(storeDir)
	r, err := s.LoadRun("test-run-1")
	if err != nil {
		t.Fatalf("cannot load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}

func TestRun_SuccessJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "test.iter", minimalWorkflow)
	storeDir := filepath.Join(dir, "store")

	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     path,
		StoreDir: storeDir,
		RunID:    "test-run-json",
		Executor: &approveExecutor{},
	}, p)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON output: %v", err)
	}
	if result["status"] != "finished" {
		t.Errorf("expected status=finished, got %v", result["status"])
	}
}

func TestRun_WithVars(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "test.iter", minimalWorkflow)
	storeDir := filepath.Join(dir, "store")

	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     path,
		StoreDir: storeDir,
		RunID:    "test-run-vars",
		Vars:     map[string]string{"description": "my PR"},
		Executor: &approveExecutor{},
	}, p)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify inputs were persisted.
	s, _ := store.New(storeDir)
	r, err := s.LoadRun("test-run-vars")
	if err != nil {
		t.Fatal(err)
	}
	if r.Inputs["description"] != "my PR" {
		t.Errorf("expected description=my PR, got %v", r.Inputs["description"])
	}
}

func TestRun_NoFile(t *testing.T) {
	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunRun(context.Background(), cli.RunOptions{}, p)
	if err == nil {
		t.Fatal("expected error when no file provided")
	}
}

// ---------------------------------------------------------------------------
// Tests — run with human pause
// ---------------------------------------------------------------------------

// rejectExecutor returns approved=false so the workflow hits a human node.
type rejectExecutor struct{}

func (e *rejectExecutor) Execute(_ context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	out := make(map[string]interface{})
	for k, v := range input {
		out[k] = v
	}
	out["approved"] = false
	out["summary"] = "needs work"
	return out, nil
}

func TestRun_HumanPause(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "test.iter", humanWorkflow)
	storeDir := filepath.Join(dir, "store")

	p, buf := newTestPrinter(cli.OutputHuman)
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     path,
		StoreDir: storeDir,
		RunID:    "test-run-pause",
		Executor: &rejectExecutor{},
	}, p)

	// Should NOT return an error — ErrRunPaused is handled.
	if err != nil {
		t.Fatalf("expected nil error for paused run, got: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "PAUSED") {
		t.Errorf("expected PAUSED in output, got:\n%s", out)
	}

	// Verify run state.
	s, _ := store.New(storeDir)
	r, err := s.LoadRun("test-run-pause")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("expected paused_waiting_human, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Tests — inspect
// ---------------------------------------------------------------------------

func TestInspect_ListRuns(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-a", "wf1", nil)
	_, _ = s.CreateRun("run-b", "wf2", nil)

	p, buf := newTestPrinter(cli.OutputHuman)
	err := cli.RunInspect(cli.InspectOptions{StoreDir: storeDir}, p)
	if err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "run-a") || !strings.Contains(out, "run-b") {
		t.Errorf("expected both runs in output, got:\n%s", out)
	}
}

func TestInspect_ListRunsJSON(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-x", "wf1", nil)

	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunInspect(cli.InspectOptions{StoreDir: storeDir}, p)
	if err != nil {
		t.Fatal(err)
	}

	var runs []interface{}
	if err := json.Unmarshal(buf.Bytes(), &runs); err != nil {
		t.Fatalf("cannot parse JSON: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("expected 1 run, got %d", len(runs))
	}
}

func TestInspect_SingleRun(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-1", "my_workflow", map[string]interface{}{"key": "val"})

	p, buf := newTestPrinter(cli.OutputHuman)
	err := cli.RunInspect(cli.InspectOptions{StoreDir: storeDir, RunID: "run-1"}, p)
	if err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "my_workflow") {
		t.Errorf("expected workflow name in output, got:\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("expected status in output, got:\n%s", out)
	}
}

func TestInspect_SingleRunJSON(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-json", "wf1", nil)

	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunInspect(cli.InspectOptions{StoreDir: storeDir, RunID: "run-json"}, p)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("cannot parse JSON: %v", err)
	}
	if _, ok := result["run"]; !ok {
		t.Error("expected 'run' key in JSON output")
	}
}

func TestInspect_WithEvents(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-ev", "wf1", nil)
	_, _ = s.AppendEvent("run-ev", store.Event{Type: store.EventRunStarted})
	_, _ = s.AppendEvent("run-ev", store.Event{Type: store.EventNodeStarted, NodeID: "agent1"})

	p, buf := newTestPrinter(cli.OutputHuman)
	err := cli.RunInspect(cli.InspectOptions{StoreDir: storeDir, RunID: "run-ev", Events: true}, p)
	if err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "run_started") {
		t.Errorf("expected run_started event in output, got:\n%s", out)
	}
	if !strings.Contains(out, "agent1") {
		t.Errorf("expected node ID in output, got:\n%s", out)
	}
}

func TestInspect_NotFound(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	_, _ = store.New(storeDir)

	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunInspect(cli.InspectOptions{StoreDir: storeDir, RunID: "nonexistent"}, p)
	if err == nil {
		t.Fatal("expected error for missing run")
	}
}

// ---------------------------------------------------------------------------
// Tests — resume
// ---------------------------------------------------------------------------

func TestResume_Success(t *testing.T) {
	dir := t.TempDir()
	iterPath := writeFixture(t, dir, "test.iter", humanWorkflow)
	storeDir := filepath.Join(dir, "store")

	// First, run the workflow to get it paused.
	p1, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:     iterPath,
		StoreDir: storeDir,
		RunID:    "resume-test",
		Executor: &rejectExecutor{},
	}, p1)
	if err != nil {
		t.Fatalf("expected nil error for paused run, got: %v", err)
	}

	// Verify it's paused.
	s, _ := store.New(storeDir)
	r, _ := s.LoadRun("resume-test")
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("expected paused, got %s", r.Status)
	}

	// Write answers file.
	answersPath := filepath.Join(dir, "answers.json")
	answersData, _ := json.Marshal(map[string]interface{}{"feedback": "looks good now"})
	os.WriteFile(answersPath, answersData, 0o644)

	// Resume.
	p2, buf := newTestPrinter(cli.OutputHuman)
	err = cli.RunResumeWithFile(context.Background(), iterPath, cli.ResumeOptions{
		RunID:       "resume-test",
		StoreDir:    storeDir,
		AnswersFile: answersPath,
		Executor:    &approveExecutor{},
	}, p2)

	if err != nil {
		t.Fatalf("resume error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "FINISHED") {
		t.Errorf("expected FINISHED in output, got:\n%s", out)
	}

	// Verify run is finished.
	r, _ = s.LoadRun("resume-test")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

func TestResume_NotPaused(t *testing.T) {
	dir := t.TempDir()
	iterPath := writeFixture(t, dir, "test.iter", humanWorkflow)
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("not-paused", "wf", nil)

	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunResumeWithFile(context.Background(), iterPath, cli.ResumeOptions{
		RunID:    "not-paused",
		StoreDir: storeDir,
		Answers:  map[string]string{"feedback": "ok"},
	}, p)

	if err == nil {
		t.Fatal("expected error for non-paused run")
	}
	if !strings.Contains(err.Error(), "not paused") {
		t.Errorf("expected 'not paused' error, got: %v", err)
	}
}

func TestResume_NoAnswers(t *testing.T) {
	dir := t.TempDir()
	iterPath := writeFixture(t, dir, "test.iter", humanWorkflow)
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	r, _ := s.CreateRun("no-answers", "test_human_workflow", nil)
	_ = s.UpdateRunStatus(r.ID, store.RunStatusPausedWaitingHuman, "")
	_ = s.SaveCheckpoint(r.ID, &store.Checkpoint{NodeID: "clarify", InteractionID: "int-1"})

	p, _ := newTestPrinter(cli.OutputHuman)
	err := cli.RunResumeWithFile(context.Background(), iterPath, cli.ResumeOptions{
		RunID:    "no-answers",
		StoreDir: storeDir,
	}, p)

	if err == nil {
		t.Fatal("expected error when no answers provided")
	}
}

// ---------------------------------------------------------------------------
// Tests — helpers
// ---------------------------------------------------------------------------

func TestParseVarFlags(t *testing.T) {
	flags := []string{"key1=val1", "key2=val with spaces"}
	vars, err := cli.ParseVarFlags(flags)
	if err != nil {
		t.Fatal(err)
	}
	if vars["key1"] != "val1" {
		t.Errorf("expected val1, got %q", vars["key1"])
	}
	if vars["key2"] != "val with spaces" {
		t.Errorf("expected 'val with spaces', got %q", vars["key2"])
	}
}

func TestParseVarFlags_Invalid(t *testing.T) {
	_, err := cli.ParseVarFlags([]string{"noequals"})
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
}

func TestParseAnswerFlags(t *testing.T) {
	flags := []string{"q1=answer1", "q2=answer2"}
	answers, err := cli.ParseAnswerFlags(flags)
	if err != nil {
		t.Fatal(err)
	}
	if answers["q1"] != "answer1" {
		t.Errorf("expected answer1, got %q", answers["q1"])
	}
}

func TestParseAnswersFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "answers.json")
	data, _ := json.Marshal(map[string]interface{}{
		"feedback": "great work",
		"score":    42,
	})
	os.WriteFile(path, data, 0o644)

	answers, err := cli.ParseAnswersFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if answers["feedback"] != "great work" {
		t.Errorf("expected 'great work', got %v", answers["feedback"])
	}
}

// Ensure runtime.NodeExecutor is satisfied by our test executors.
var _ runtime.NodeExecutor = (*approveExecutor)(nil)
var _ runtime.NodeExecutor = (*rejectExecutor)(nil)
