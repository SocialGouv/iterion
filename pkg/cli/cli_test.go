package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
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

func (e *approveExecutor) Execute(_ context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
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

func (e *rejectExecutor) Execute(_ context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
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
// Tests — inspect (per-node selection)
// ---------------------------------------------------------------------------

// seedSimpleRun creates a run with one node ("agent1") that started
// and finished, plus an llm_prompt + llm_step_finished pair, a
// tool_called, and an artifact. Returns the storeDir path.
func seedSimpleRun(t *testing.T, runID string) (storeDir string, s store.RunStore) {
	t.Helper()
	dir := t.TempDir()
	storeDir = filepath.Join(dir, "store")
	var err error
	s, err = store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(runID, "wf1", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AppendEvent(runID, store.Event{Type: store.EventRunStarted})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventNodeStarted,
		NodeID: "agent1",
		Data:   map[string]interface{}{"kind": "agent"},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventLLMPrompt,
		NodeID: "agent1",
		Data: map[string]interface{}{
			"system_prompt": "you are a reviewer",
			"user_message":  "review the diff",
		},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventLLMStepFinished,
		NodeID: "agent1",
		Data: map[string]interface{}{
			"response_text": "looks good",
			"input_tokens":  120,
			"output_tokens": 30,
			"finish_reason": "stop",
		},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventToolCalled,
		NodeID: "agent1",
		Data: map[string]interface{}{
			"tool_name":   "Read",
			"input":       "path=/tmp/foo",
			"duration_ms": float64(42),
		},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventArtifactWritten,
		NodeID: "agent1",
		Data:   map[string]interface{}{"version": float64(0)},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventNodeFinished,
		NodeID: "agent1",
		Data: map[string]interface{}{
			"_tokens":   float64(150),
			"_cost_usd": float64(0.0021),
			"output":    map[string]interface{}{"summary": "ok"},
		},
	})
	if err := s.WriteArtifact(&store.Artifact{
		RunID:   runID,
		NodeID:  "agent1",
		Version: 0,
		Data:    map[string]interface{}{"summary": "first"},
	}); err != nil {
		t.Fatal(err)
	}
	return storeDir, s
}

func seedRunningNodeRun(t *testing.T, runID string) string {
	t.Helper()
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(runID, "wf1", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AppendEvent(runID, store.Event{Type: store.EventRunStarted})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventNodeStarted,
		NodeID: "agent1",
		Data:   map[string]interface{}{"kind": "agent"},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventLLMPrompt,
		NodeID: "agent1",
		Data: map[string]interface{}{
			"system_prompt": "you are live",
			"user_message":  "continue",
		},
	})
	_, _ = s.AppendEvent(runID, store.Event{
		Type:   store.EventToolCalled,
		NodeID: "agent1",
		Data: map[string]interface{}{
			"tool_name": "Read",
			"input":     "file.txt",
		},
	})
	return storeDir
}

func TestInspect_RunningNodeSectionsIncludeLiveEvents(t *testing.T) {
	storeDir := seedRunningNodeRun(t, "run-live")

	assertSection := func(section cli.InspectSection, key string, wantType store.EventType) {
		t.Helper()
		p, buf := newTestPrinter(cli.OutputJSON)
		if err := cli.RunInspect(cli.InspectOptions{
			StoreDir: storeDir,
			RunID:    "run-live",
			Node:     "agent1",
			Section:  section,
		}, p); err != nil {
			t.Fatalf("%s: %v", section, err)
		}
		var r map[string]interface{}
		if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
			t.Fatalf("%s decode: %v\n%s", section, err, buf.String())
		}
		if r["status"] != "running" {
			t.Fatalf("%s status = %v, want running", section, r["status"])
		}
		if got := r["last_seq"]; got != float64(3) {
			t.Fatalf("%s last_seq = %v, want 3", section, got)
		}
		items, ok := r[key].([]interface{})
		if !ok || len(items) == 0 {
			t.Fatalf("%s expected non-empty %s, got %v", section, key, r[key])
		}
		if wantType != "" {
			found := false
			for _, item := range items {
				m := item.(map[string]interface{})
				if m["type"] == string(wantType) {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s events missing %s: %v", section, wantType, items)
			}
		}
	}

	assertSection(cli.SectionTrace, "trace", "")
	assertSection(cli.SectionTools, "tools", "")
	assertSection(cli.SectionEvents, "events", store.EventToolCalled)
}

func TestInspect_NodeBasic_JSON(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-node")
	p, buf := newTestPrinter(cli.OutputJSON)

	err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-node",
		Node:     "agent1",
	}, p)
	if err != nil {
		t.Fatal(err)
	}

	var report map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("cannot parse JSON: %v\n%s", err, buf.String())
	}
	if report["node_id"] != "agent1" {
		t.Errorf("node_id = %v, want agent1", report["node_id"])
	}
	if report["execution_id"] != "exec:main:agent1:0" {
		t.Errorf("execution_id = %v, want exec:main:agent1:0", report["execution_id"])
	}
	if report["status"] != "finished" {
		t.Errorf("status = %v, want finished", report["status"])
	}
	if _, ok := report["trace"].([]interface{}); !ok {
		t.Errorf("expected trace array, got %T", report["trace"])
	}
	tools, ok := report["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Errorf("expected 1 tool call, got %v", report["tools"])
	}
	arts, ok := report["artifacts"].([]interface{})
	if !ok || len(arts) != 1 {
		t.Errorf("expected 1 artifact, got %v", report["artifacts"])
	}
	if got := report["tokens"]; got == nil || got.(float64) != 150 {
		t.Errorf("tokens = %v, want 150", got)
	}
}

func TestInspect_NodeBasic_Human(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-h")
	p, buf := newTestPrinter(cli.OutputHuman)

	err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-h",
		Node:     "agent1",
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Node: agent1", "exec:main:agent1:0", "Tool calls", "Artifacts", "Trace"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got:\n%s", want, out)
		}
	}
}

func TestInspect_NodeWithBranchIter(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	if _, err := s.CreateRun("run-iter", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// Two iterations of the same node on main branch.
	_, _ = s.AppendEvent("run-iter", store.Event{Type: store.EventNodeStarted, NodeID: "loop", Data: map[string]interface{}{"kind": "agent"}})
	_, _ = s.AppendEvent("run-iter", store.Event{Type: store.EventNodeFinished, NodeID: "loop"})
	_, _ = s.AppendEvent("run-iter", store.Event{Type: store.EventNodeStarted, NodeID: "loop", Data: map[string]interface{}{"kind": "agent"}})
	_, _ = s.AppendEvent("run-iter", store.Event{Type: store.EventNodeFinished, NodeID: "loop"})

	// Iter 0 must resolve.
	iter0 := 0
	p0, buf0 := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir:  storeDir,
		RunID:     "run-iter",
		Node:      "loop",
		Iteration: &iter0,
	}, p0); err != nil {
		t.Fatalf("iter 0: %v", err)
	}
	var r0 map[string]interface{}
	if err := json.Unmarshal(buf0.Bytes(), &r0); err != nil {
		t.Fatal(err)
	}
	if r0["execution_id"] != "exec:main:loop:0" {
		t.Errorf("iter 0 exec = %v", r0["execution_id"])
	}

	// Iter 1 must resolve.
	iter1 := 1
	p1, buf1 := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir:  storeDir,
		RunID:     "run-iter",
		Node:      "loop",
		Iteration: &iter1,
	}, p1); err != nil {
		t.Fatalf("iter 1: %v", err)
	}
	var r1 map[string]interface{}
	if err := json.Unmarshal(buf1.Bytes(), &r1); err != nil {
		t.Fatal(err)
	}
	if r1["execution_id"] != "exec:main:loop:1" {
		t.Errorf("iter 1 exec = %v", r1["execution_id"])
	}
	// Without --iteration, latest iteration should win.
	pl, bufl := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-iter",
		Node:     "loop",
	}, pl); err != nil {
		t.Fatalf("default iter: %v", err)
	}
	var rl map[string]interface{}
	if err := json.Unmarshal(bufl.Bytes(), &rl); err != nil {
		t.Fatal(err)
	}
	if rl["execution_id"] != "exec:main:loop:1" {
		t.Errorf("default iter exec = %v, want exec:main:loop:1", rl["execution_id"])
	}
}

func TestInspect_NodeUnknown(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-unk")
	p, _ := newTestPrinter(cli.OutputJSON)
	err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-unk",
		Node:     "nope",
	}, p)
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
	if !strings.Contains(err.Error(), "no execution found") {
		t.Errorf("error = %v, want 'no execution found'", err)
	}
	if !strings.Contains(err.Error(), "exec:main:agent1:0") {
		t.Errorf("error should suggest available execs, got: %v", err)
	}
}

func TestInspect_NodeAmbiguousBranches(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-ambig", "wf", nil)
	// Same node on two branches.
	_, _ = s.AppendEvent("run-ambig", store.Event{Type: store.EventNodeStarted, NodeID: "n", BranchID: "b1", Data: map[string]interface{}{"kind": "agent"}})
	_, _ = s.AppendEvent("run-ambig", store.Event{Type: store.EventNodeStarted, NodeID: "n", BranchID: "b2", Data: map[string]interface{}{"kind": "agent"}})

	p, _ := newTestPrinter(cli.OutputJSON)
	err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-ambig",
		Node:     "n",
	}, p)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "b1") || !strings.Contains(err.Error(), "b2") {
		t.Errorf("expected both candidate branches in error, got: %v", err)
	}

	// With --branch the resolver succeeds.
	p2, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-ambig",
		Node:     "n",
		Branch:   "b1",
	}, p2); err != nil {
		t.Fatalf("with branch: %v", err)
	}
	if !strings.Contains(buf.String(), "exec:b1:n:0") {
		t.Errorf("expected exec:b1:n:0 in output: %s", buf.String())
	}
}

func TestInspect_NodeExecID(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-execid")
	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunInspect(cli.InspectOptions{
		StoreDir:    storeDir,
		RunID:       "run-execid",
		ExecutionID: "exec:main:agent1:0",
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r["node_id"] != "agent1" {
		t.Errorf("node_id = %v", r["node_id"])
	}
}

func TestInspect_SectionTrace(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-sec")
	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-sec",
		Node:     "agent1",
		Section:  cli.SectionTrace,
	}, p); err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	trace, ok := r["trace"].([]interface{})
	if !ok || len(trace) != 1 {
		t.Fatalf("expected 1 trace step, got %v", r["trace"])
	}
	step := trace[0].(map[string]interface{})
	if step["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", step["finish_reason"])
	}
	if step["input_tokens"].(float64) != 120 {
		t.Errorf("input_tokens = %v", step["input_tokens"])
	}
	// Other sections should be omitted (omitempty).
	if _, ok := r["tools"]; ok {
		t.Errorf("tools should be omitted when --section=trace")
	}
	if _, ok := r["artifacts"]; ok {
		t.Errorf("artifacts should be omitted when --section=trace")
	}
}

func TestInspect_SectionArtifactsIncludesBody(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-art")
	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-art",
		Node:     "agent1",
		Section:  cli.SectionArtifacts,
	}, p); err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	arts := r["artifacts"].([]interface{})
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	body, ok := arts[0].(map[string]interface{})["data"].(map[string]interface{})
	if !ok || body["summary"] != "first" {
		t.Errorf("expected artifact body in --section=artifacts, got %v", arts[0])
	}
}

func TestInspect_ArtifactsScopedToSelectedExecution(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	if _, err := s.CreateRun("run-art-scope", "wf", nil); err != nil {
		t.Fatal(err)
	}

	// Same node writes artifacts in two main-branch iterations and one
	// sibling branch. The inspector must report only versions referenced by
	// the selected execution's artifact_written events, not every persisted
	// version under artifacts/<node>/.
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventNodeStarted, NodeID: "loop", Data: map[string]interface{}{"kind": "agent"}})
	if err := s.WriteArtifact(&store.Artifact{RunID: "run-art-scope", NodeID: "loop", Version: 0, Data: map[string]interface{}{"summary": "main iter 0"}}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventArtifactWritten, NodeID: "loop", Data: map[string]interface{}{"version": float64(0)}})
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventNodeFinished, NodeID: "loop"})

	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventNodeStarted, NodeID: "loop", BranchID: "feature", Data: map[string]interface{}{"kind": "agent"}})
	if err := s.WriteArtifact(&store.Artifact{RunID: "run-art-scope", NodeID: "loop", Version: 1, Data: map[string]interface{}{"summary": "feature iter 0"}}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventArtifactWritten, NodeID: "loop", BranchID: "feature", Data: map[string]interface{}{"version": float64(1)}})
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventNodeFinished, NodeID: "loop", BranchID: "feature"})

	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventNodeStarted, NodeID: "loop", Data: map[string]interface{}{"kind": "agent"}})
	if err := s.WriteArtifact(&store.Artifact{RunID: "run-art-scope", NodeID: "loop", Version: 2, Data: map[string]interface{}{"summary": "main iter 1"}}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventArtifactWritten, NodeID: "loop", Data: map[string]interface{}{"version": float64(2)}})
	_, _ = s.AppendEvent("run-art-scope", store.Event{Type: store.EventNodeFinished, NodeID: "loop"})

	assertArtifacts := func(name string, opts cli.InspectOptions, wantVersion float64, wantSummary string) {
		t.Helper()
		p, buf := newTestPrinter(cli.OutputJSON)
		if err := cli.RunInspect(opts, p); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var r map[string]interface{}
		if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
			t.Fatalf("%s decode: %v\n%s", name, err, buf.String())
		}
		arts, ok := r["artifacts"].([]interface{})
		if !ok || len(arts) != 1 {
			t.Fatalf("%s: expected exactly 1 artifact, got %v", name, r["artifacts"])
		}
		art := arts[0].(map[string]interface{})
		if art["version"] != wantVersion {
			t.Fatalf("%s: version = %v, want %v", name, art["version"], wantVersion)
		}
		body := art["data"].(map[string]interface{})
		if body["summary"] != wantSummary {
			t.Fatalf("%s: summary = %v, want %q", name, body["summary"], wantSummary)
		}
	}

	iter0 := 0
	iter1 := 1
	base := cli.InspectOptions{StoreDir: storeDir, RunID: "run-art-scope", Node: "loop", Section: cli.SectionArtifacts}
	main0 := base
	main0.Iteration = &iter0
	assertArtifacts("main iter 0", main0, 0, "main iter 0")

	main1 := base
	main1.Iteration = &iter1
	assertArtifacts("main iter 1", main1, 2, "main iter 1")

	feature := base
	feature.Branch = "feature"
	feature.Iteration = &iter0
	assertArtifacts("feature iter 0", feature, 1, "feature iter 0")
}

func TestInspect_NodeWithLogSlice(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	if _, err := s.CreateRun("run-log", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// Use explicit timestamps so the log slice window is deterministic.
	// Events are stored in UTC (mirrors store.AppendEvent), but the
	// run.log file format the iterion logger writes uses the host's
	// LOCAL HH:MM:SS — so we derive the synthetic log line prefixes
	// from .Local() to keep this test correct on non-UTC hosts too.
	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	_, _ = s.AppendEvent("run-log", store.Event{
		Type:      store.EventNodeStarted,
		NodeID:    "agent1",
		Timestamp: startedAt,
		Data:      map[string]interface{}{"kind": "agent"},
	})
	_, _ = s.AppendEvent("run-log", store.Event{
		Type:      store.EventNodeFinished,
		NodeID:    "agent1",
		Timestamp: finishedAt,
	})

	before := startedAt.Add(-1 * time.Second).Local().Format("15:04:05")
	mid1 := startedAt.Add(10 * time.Second).Local().Format("15:04:05")
	mid2 := startedAt.Add(11 * time.Second).Local().Format("15:04:05")
	after := finishedAt.Add(1 * time.Second).Local().Format("15:04:05")

	logPath := filepath.Join(storeDir, "runs", "run-log", "run.log")
	contents := before + " ✅ before window\n" +
		mid1 + " ▶ inside window\n" +
		mid2 + " 🔍 still inside\n" +
		after + " ❌ after window\n"
	if err := os.WriteFile(logPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-log",
		Node:     "agent1",
		Section:  cli.SectionLog,
	}, p); err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	logSlice, ok := r["log"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected log object, got %v", r["log"])
	}
	if logSlice["best_effort"] != true {
		t.Errorf("best_effort should be true, got %v", logSlice["best_effort"])
	}
	body, _ := logSlice["body"].(string)
	if !strings.Contains(body, "inside window") {
		t.Errorf("expected 'inside window' in log slice body, got: %q", body)
	}
	if strings.Contains(body, "before window") || strings.Contains(body, "after window") {
		t.Errorf("log slice should not contain out-of-window lines, got: %q", body)
	}
}

// TestInspect_LogSliceNonUTCTimezone is a regression test for the TZ
// mismatch between events.jsonl (UTC) and run.log (host-local). The
// previous slicer forced UTC on both sides and silently returned
// log lines from the wrong hour on non-UTC hosts. This test pins
// time.Local to a non-UTC zone and asserts the slice still matches
// the log lines the iterion logger would have written for that exec.
func TestInspect_LogSliceNonUTCTimezone(t *testing.T) {
	// Restore time.Local at end so we don't pollute other tests.
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	// CET is UTC+1 (no DST in January) — picks an offset distinct
	// from any UTC-equivalent tz so a UTC-vs-local bug is observable.
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("Europe/Paris tz unavailable: %v", err)
	}
	time.Local = loc

	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	if _, err := s.CreateRun("run-tz", "wf", nil); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	_, _ = s.AppendEvent("run-tz", store.Event{
		Type:      store.EventNodeStarted,
		NodeID:    "agent1",
		Timestamp: startedAt,
		Data:      map[string]interface{}{"kind": "agent"},
	})
	_, _ = s.AppendEvent("run-tz", store.Event{
		Type:      store.EventNodeFinished,
		NodeID:    "agent1",
		Timestamp: finishedAt,
	})

	// What the iterion logger actually writes: time.Now().Format("15:04:05")
	// in the host's local TZ (here, Europe/Paris → 13:00:xx for a UTC
	// 12:00:xx instant).
	before := startedAt.Add(-1 * time.Second).In(loc).Format("15:04:05")
	mid := startedAt.Add(10 * time.Second).In(loc).Format("15:04:05")
	after := finishedAt.Add(1 * time.Second).In(loc).Format("15:04:05")

	logPath := filepath.Join(storeDir, "runs", "run-tz", "run.log")
	contents := before + " ✅ before window\n" +
		mid + " ▶ inside window\n" +
		after + " ❌ after window\n"
	if err := os.WriteFile(logPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-tz",
		Node:     "agent1",
		Section:  cli.SectionLog,
	}, p); err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	logSlice, ok := r["log"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected log object, got %v", r["log"])
	}
	body, _ := logSlice["body"].(string)
	if !strings.Contains(body, "inside window") {
		t.Errorf("non-UTC host: expected 'inside window' in slice, got: %q", body)
	}
	if strings.Contains(body, "before window") || strings.Contains(body, "after window") {
		t.Errorf("non-UTC host: out-of-window lines leaked into slice: %q", body)
	}
}

func TestInspect_LogSliceCrossesMidnight(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	if _, err := s.CreateRun("run-midnight", "wf", nil); err != nil {
		t.Fatal(err)
	}
	loc := time.Local
	startedAt := time.Date(2026, 1, 1, 23, 59, 50, 0, loc)
	finishedAt := time.Date(2026, 1, 2, 0, 0, 10, 0, loc)
	_, _ = s.AppendEvent("run-midnight", store.Event{
		Type:      store.EventNodeStarted,
		NodeID:    "agent1",
		Timestamp: startedAt,
		Data:      map[string]interface{}{"kind": "agent"},
	})
	_, _ = s.AppendEvent("run-midnight", store.Event{
		Type:      store.EventNodeFinished,
		NodeID:    "agent1",
		Timestamp: finishedAt,
	})

	logPath := filepath.Join(storeDir, "runs", "run-midnight", "run.log")
	contents := "23:59:49 ❌ before window\n" +
		"23:59:50 ▶ starts before midnight\n" +
		"23:59:59 🔍 still before midnight\n" +
		"00:00:00 ✅ after midnight\n" +
		"00:00:10 ✅ end boundary\n" +
		"00:00:11 ❌ after window\n" +
		"00:01:00 ❌ much later\n"
	if err := os.WriteFile(logPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-midnight",
		Node:     "agent1",
		Section:  cli.SectionLog,
	}, p); err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	logSlice, ok := r["log"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected log object, got %v", r["log"])
	}
	body, _ := logSlice["body"].(string)
	for _, want := range []string{"starts before midnight", "still before midnight", "after midnight", "end boundary"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in cross-midnight slice, got: %q", want, body)
		}
	}
	for _, unwanted := range []string{"before window", "after window", "much later"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("cross-midnight slice contains out-of-window %q: %q", unwanted, body)
		}
	}
}

func TestInspect_ListNodes(t *testing.T) {
	storeDir, _ := seedSimpleRun(t, "run-list")
	p, buf := newTestPrinter(cli.OutputJSON)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir:  storeDir,
		RunID:     "run-list",
		ListNodes: true,
	}, p); err != nil {
		t.Fatal(err)
	}
	var r map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	execs, ok := r["executions"].([]interface{})
	if !ok || len(execs) != 1 {
		t.Fatalf("expected 1 execution, got %v", r["executions"])
	}
	first := execs[0].(map[string]interface{})
	if first["execution_id"] != "exec:main:agent1:0" {
		t.Errorf("execution_id = %v", first["execution_id"])
	}
}

func TestInspect_FlagValidation(t *testing.T) {
	cases := []struct {
		name string
		opts cli.InspectOptions
		want string
	}{
		{
			name: "node and exec exclusive",
			opts: cli.InspectOptions{RunID: "x", Node: "n", ExecutionID: "exec:main:n:0"},
			want: "mutually exclusive",
		},
		{
			name: "branch without node",
			opts: cli.InspectOptions{RunID: "x", Branch: "b"},
			want: "require --node",
		},
		{
			name: "section without node",
			opts: cli.InspectOptions{RunID: "x", Section: cli.SectionTrace},
			want: "--section requires",
		},
		{
			name: "list-nodes with node",
			opts: cli.InspectOptions{RunID: "x", ListNodes: true, Node: "n"},
			want: "--list-nodes is mutually exclusive",
		},
		{
			name: "invalid section",
			opts: cli.InspectOptions{RunID: "x", Node: "n", Section: cli.InspectSection("garbage")},
			want: "invalid --section",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestPrinter(cli.OutputJSON)
			err := cli.RunInspect(tc.opts, p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestInspect_LegacyPathUnchanged(t *testing.T) {
	// Regression: with no per-node flags, RunInspect must keep its
	// legacy run-level summary behaviour.
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	s, _ := store.New(storeDir)
	_, _ = s.CreateRun("run-legacy", "wf", nil)

	p, buf := newTestPrinter(cli.OutputHuman)
	if err := cli.RunInspect(cli.InspectOptions{
		StoreDir: storeDir,
		RunID:    "run-legacy",
	}, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Inspect: run-legacy") {
		t.Errorf("legacy human output missing header, got:\n%s", out)
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
	if !strings.Contains(err.Error(), "cannot be resumed") {
		t.Errorf("expected 'cannot be resumed' error, got: %v", err)
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
