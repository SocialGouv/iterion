package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func tmpStore(t *testing.T) *RunStore {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Run persistence
// ---------------------------------------------------------------------------

func TestCreateAndLoadRun(t *testing.T) {
	s := tmpStore(t)

	inputs := map[string]interface{}{"repo": "iterion", "branch": "main"}
	r, err := s.CreateRun("run-001", "pr_refine", inputs)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.ID != "run-001" {
		t.Errorf("ID = %q, want run-001", r.ID)
	}
	if r.Status != RunStatusRunning {
		t.Errorf("Status = %q, want running", r.Status)
	}

	// Reload from disk.
	loaded, err := s.LoadRun("run-001")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if loaded.WorkflowName != "pr_refine" {
		t.Errorf("WorkflowName = %q, want pr_refine", loaded.WorkflowName)
	}
	if loaded.Inputs["repo"] != "iterion" {
		t.Errorf("Inputs[repo] = %v, want iterion", loaded.Inputs["repo"])
	}
}

func TestUpdateRunStatus(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-002", "ci_fix", nil)

	// Pause.
	if err := s.UpdateRunStatus("run-002", RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("UpdateRunStatus pause: %v", err)
	}
	r, _ := s.LoadRun("run-002")
	if r.Status != RunStatusPausedWaitingHuman {
		t.Errorf("Status = %q, want paused_waiting_human", r.Status)
	}
	if r.FinishedAt != nil {
		t.Error("FinishedAt should be nil while paused")
	}

	// Finish.
	if err := s.UpdateRunStatus("run-002", RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus finish: %v", err)
	}
	r, _ = s.LoadRun("run-002")
	if r.Status != RunStatusFinished {
		t.Errorf("Status = %q, want finished", r.Status)
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt should be set after finish")
	}
}

func TestUpdateRunStatusFailed(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-003", "ci_fix", nil)

	if err := s.UpdateRunStatus("run-003", RunStatusFailed, "budget_exceeded"); err != nil {
		t.Fatalf("UpdateRunStatus fail: %v", err)
	}
	r, _ := s.LoadRun("run-003")
	if r.Status != RunStatusFailed {
		t.Errorf("Status = %q, want failed", r.Status)
	}
	if r.Error != "budget_exceeded" {
		t.Errorf("Error = %q, want budget_exceeded", r.Error)
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt should be set after failure")
	}
}

func TestListRuns(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("alpha", "w1", nil)
	s.CreateRun("beta", "w2", nil)
	s.CreateRun("gamma", "w3", nil)

	ids, err := s.ListRuns()
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("len = %d, want 3", len(ids))
	}
	// Sorted alphabetically.
	if ids[0] != "alpha" || ids[1] != "beta" || ids[2] != "gamma" {
		t.Errorf("ids = %v, want [alpha beta gamma]", ids)
	}
}

// ---------------------------------------------------------------------------
// Event persistence
// ---------------------------------------------------------------------------

func TestAppendAndLoadEvents(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-evt", "wf", nil)

	types := []EventType{
		EventRunStarted,
		EventNodeStarted,
		EventLLMRequest,
		EventLLMStepFinished,
		EventArtifactWritten,
		EventNodeFinished,
		EventEdgeSelected,
		EventRunFinished,
	}

	for _, typ := range types {
		_, err := s.AppendEvent("run-evt", Event{
			Type:   typ,
			NodeID: "agent_a",
			Data:   map[string]interface{}{"info": string(typ)},
		})
		if err != nil {
			t.Fatalf("AppendEvent %s: %v", typ, err)
		}
	}

	events, err := s.LoadEvents("run-evt")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != len(types) {
		t.Fatalf("len = %d, want %d", len(events), len(types))
	}

	// Verify monotonic sequence.
	for i, evt := range events {
		if evt.Seq != int64(i) {
			t.Errorf("events[%d].Seq = %d, want %d", i, evt.Seq, i)
		}
		if evt.RunID != "run-evt" {
			t.Errorf("events[%d].RunID = %q, want run-evt", i, evt.RunID)
		}
		if evt.Type != types[i] {
			t.Errorf("events[%d].Type = %q, want %q", i, evt.Type, types[i])
		}
		if evt.Timestamp.IsZero() {
			t.Errorf("events[%d].Timestamp is zero", i)
		}
	}
}

func TestAllEventTypesPersistable(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-all-evt", "wf", nil)

	allTypes := []EventType{
		EventRunStarted,
		EventBranchStarted,
		EventNodeStarted,
		EventLLMRequest,
		EventLLMRetry,
		EventLLMStepFinished,
		EventToolCalled,
		EventToolError,
		EventArtifactWritten,
		EventHumanInputRequested,
		EventRunPaused,
		EventHumanAnswersRecorded,
		EventRunResumed,
		EventJoinReady,
		EventNodeFinished,
		EventEdgeSelected,
		EventBudgetWarning,
		EventRunFinished,
		EventRunFailed,
	}

	for _, typ := range allTypes {
		_, err := s.AppendEvent("run-all-evt", Event{
			Type:     typ,
			BranchID: "branch-0",
			NodeID:   "node-x",
			Data:     map[string]interface{}{"type": string(typ)},
		})
		if err != nil {
			t.Fatalf("AppendEvent %s: %v", typ, err)
		}
	}

	events, err := s.LoadEvents("run-all-evt")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != len(allTypes) {
		t.Fatalf("len = %d, want %d", len(events), len(allTypes))
	}
	for i, evt := range events {
		if evt.Type != allTypes[i] {
			t.Errorf("events[%d].Type = %q, want %q", i, evt.Type, allTypes[i])
		}
	}
}

func TestLoadEventsEmpty(t *testing.T) {
	s := tmpStore(t)
	events, err := s.LoadEvents("nonexistent")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if events != nil {
		t.Errorf("expected nil, got %v", events)
	}
}

func TestEventDataRoundTrip(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-data", "wf", nil)

	data := map[string]interface{}{
		"model":         "claude-opus-4-20250514",
		"input_tokens":  float64(1500),
		"output_tokens": float64(300),
		"cost_usd":      0.042,
		"tools_used":    []interface{}{"read_file", "edit_file"},
	}
	_, err := s.AppendEvent("run-data", Event{
		Type: EventLLMStepFinished,
		Data: data,
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, _ := s.LoadEvents("run-data")
	if len(events) != 1 {
		t.Fatalf("len = %d, want 1", len(events))
	}
	got := events[0].Data
	if got["model"] != "claude-opus-4-20250514" {
		t.Errorf("model = %v", got["model"])
	}
	if got["cost_usd"] != 0.042 {
		t.Errorf("cost_usd = %v", got["cost_usd"])
	}
}

// ---------------------------------------------------------------------------
// Artifact persistence
// ---------------------------------------------------------------------------

func TestWriteAndLoadArtifact(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-art", "wf", nil)

	a := &Artifact{
		RunID:   "run-art",
		NodeID:  "reviewer",
		Version: 0,
		Data:    map[string]interface{}{"verdict": "approved", "comments": "LGTM"},
	}
	if err := s.WriteArtifact(a); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}

	loaded, err := s.LoadArtifact("run-art", "reviewer", 0)
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if loaded.NodeID != "reviewer" {
		t.Errorf("NodeID = %q", loaded.NodeID)
	}
	if loaded.Data["verdict"] != "approved" {
		t.Errorf("verdict = %v", loaded.Data["verdict"])
	}
	if loaded.WrittenAt.IsZero() {
		t.Error("WrittenAt should be set")
	}
}

func TestArtifactVersioning(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-ver", "wf", nil)

	for v := 0; v < 3; v++ {
		a := &Artifact{
			RunID:   "run-ver",
			NodeID:  "planner",
			Version: v,
			Data:    map[string]interface{}{"iteration": float64(v)},
		}
		if err := s.WriteArtifact(a); err != nil {
			t.Fatalf("WriteArtifact v%d: %v", v, err)
		}
	}

	// Load specific version.
	a1, err := s.LoadArtifact("run-ver", "planner", 1)
	if err != nil {
		t.Fatalf("LoadArtifact v1: %v", err)
	}
	if a1.Data["iteration"] != float64(1) {
		t.Errorf("v1 iteration = %v, want 1", a1.Data["iteration"])
	}

	// Load latest.
	latest, err := s.LoadLatestArtifact("run-ver", "planner")
	if err != nil {
		t.Fatalf("LoadLatestArtifact: %v", err)
	}
	if latest.Version != 2 {
		t.Errorf("latest.Version = %d, want 2", latest.Version)
	}
}

// ---------------------------------------------------------------------------
// Interaction persistence
// ---------------------------------------------------------------------------

func TestWriteAndLoadInteraction(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-human", "wf", nil)

	now := time.Now().UTC()
	i := &Interaction{
		ID:          "int-001",
		RunID:       "run-human",
		NodeID:      "human_review",
		RequestedAt: now,
		Questions: map[string]interface{}{
			"approve": "Do you approve this PR?",
			"comment": "Any comments?",
		},
	}
	if err := s.WriteInteraction(i); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}

	loaded, err := s.LoadInteraction("run-human", "int-001")
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if loaded.NodeID != "human_review" {
		t.Errorf("NodeID = %q", loaded.NodeID)
	}
	if loaded.Questions["approve"] != "Do you approve this PR?" {
		t.Errorf("question = %v", loaded.Questions["approve"])
	}

	// Record answers.
	answered := now.Add(5 * time.Minute)
	loaded.AnsweredAt = &answered
	loaded.Answers = map[string]interface{}{
		"approve": true,
		"comment": "Ship it!",
	}
	if err := s.WriteInteraction(loaded); err != nil {
		t.Fatalf("WriteInteraction update: %v", err)
	}

	reloaded, _ := s.LoadInteraction("run-human", "int-001")
	if reloaded.AnsweredAt == nil {
		t.Fatal("AnsweredAt should be set")
	}
	if reloaded.Answers["comment"] != "Ship it!" {
		t.Errorf("answer = %v", reloaded.Answers["comment"])
	}
}

func TestListInteractions(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-li", "wf", nil)

	for _, id := range []string{"int-a", "int-b", "int-c"} {
		s.WriteInteraction(&Interaction{
			ID:          id,
			RunID:       "run-li",
			NodeID:      "human",
			RequestedAt: time.Now().UTC(),
		})
	}

	ids, err := s.ListInteractions("run-li")
	if err != nil {
		t.Fatalf("ListInteractions: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("len = %d, want 3", len(ids))
	}
	if ids[0] != "int-a" || ids[1] != "int-b" || ids[2] != "int-c" {
		t.Errorf("ids = %v", ids)
	}
}

func TestListInteractionsEmpty(t *testing.T) {
	s := tmpStore(t)
	ids, err := s.ListInteractions("nonexistent")
	if err != nil {
		t.Fatalf("ListInteractions: %v", err)
	}
	if ids != nil {
		t.Errorf("expected nil, got %v", ids)
	}
}

// ---------------------------------------------------------------------------
// Full run replay scenario
// ---------------------------------------------------------------------------

func TestFullRunReplay(t *testing.T) {
	s := tmpStore(t)

	// 1. Create run.
	run, _ := s.CreateRun("replay-001", "pr_refine_single_model", map[string]interface{}{
		"repo":   "iterion",
		"branch": "feat/store",
	})

	// 2. Emit events through a typical lifecycle.
	s.AppendEvent(run.ID, Event{Type: EventRunStarted})
	s.AppendEvent(run.ID, Event{Type: EventNodeStarted, NodeID: "context_builder"})
	s.AppendEvent(run.ID, Event{Type: EventLLMRequest, NodeID: "context_builder"})
	s.AppendEvent(run.ID, Event{Type: EventLLMStepFinished, NodeID: "context_builder", Data: map[string]interface{}{"tokens": float64(500)}})
	s.AppendEvent(run.ID, Event{Type: EventArtifactWritten, NodeID: "context_builder"})
	s.AppendEvent(run.ID, Event{Type: EventNodeFinished, NodeID: "context_builder"})
	s.AppendEvent(run.ID, Event{Type: EventEdgeSelected, Data: map[string]interface{}{"from": "context_builder", "to": "reviewer"}})

	// 3. Write artifact.
	s.WriteArtifact(&Artifact{
		RunID:   run.ID,
		NodeID:  "context_builder",
		Version: 0,
		Data:    map[string]interface{}{"diff_summary": "Added store package"},
	})

	// 4. Human pause.
	s.AppendEvent(run.ID, Event{Type: EventNodeStarted, NodeID: "human_review"})
	s.AppendEvent(run.ID, Event{Type: EventHumanInputRequested, NodeID: "human_review"})
	s.AppendEvent(run.ID, Event{Type: EventRunPaused})
	s.UpdateRunStatus(run.ID, RunStatusPausedWaitingHuman, "")

	s.WriteInteraction(&Interaction{
		ID:          "int-replay",
		RunID:       run.ID,
		NodeID:      "human_review",
		RequestedAt: time.Now().UTC(),
		Questions:   map[string]interface{}{"approve": "Approve?"},
	})

	// 5. Resume.
	answered := time.Now().UTC()
	s.WriteInteraction(&Interaction{
		ID:          "int-replay",
		RunID:       run.ID,
		NodeID:      "human_review",
		RequestedAt: time.Now().UTC(),
		AnsweredAt:  &answered,
		Questions:   map[string]interface{}{"approve": "Approve?"},
		Answers:     map[string]interface{}{"approve": true},
	})
	s.AppendEvent(run.ID, Event{Type: EventHumanAnswersRecorded, NodeID: "human_review"})
	s.AppendEvent(run.ID, Event{Type: EventRunResumed})
	s.AppendEvent(run.ID, Event{Type: EventNodeFinished, NodeID: "human_review"})
	s.AppendEvent(run.ID, Event{Type: EventRunFinished})
	s.UpdateRunStatus(run.ID, RunStatusFinished, "")

	// --- Verify full reload ---

	// Run.
	reRun, err := s.LoadRun(run.ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if reRun.Status != RunStatusFinished {
		t.Errorf("Status = %q", reRun.Status)
	}

	// Events.
	events, err := s.LoadEvents(run.ID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 14 {
		t.Errorf("events count = %d, want 14", len(events))
	}
	// First event is run_started, last is run_finished.
	if events[0].Type != EventRunStarted {
		t.Errorf("first event = %q", events[0].Type)
	}
	if events[len(events)-1].Type != EventRunFinished {
		t.Errorf("last event = %q", events[len(events)-1].Type)
	}

	// Artifact.
	art, err := s.LoadArtifact(run.ID, "context_builder", 0)
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if art.Data["diff_summary"] != "Added store package" {
		t.Errorf("artifact data = %v", art.Data)
	}

	// Interaction.
	inter, err := s.LoadInteraction(run.ID, "int-replay")
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if inter.Answers["approve"] != true {
		t.Errorf("answer = %v", inter.Answers["approve"])
	}

	// File layout verification.
	runDir := filepath.Join(s.Root(), "runs", run.ID)
	for _, rel := range []string{
		"run.json",
		"events.jsonl",
		"artifacts/context_builder/0.json",
		"interactions/int-replay.json",
	} {
		p := filepath.Join(runDir, rel)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file %s: %v", rel, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Fresh store reloads persisted data (simulates process restart)
// ---------------------------------------------------------------------------

func TestReloadFromDisk(t *testing.T) {
	dir := t.TempDir()

	// First store instance: write data.
	s1, _ := New(dir)
	s1.CreateRun("reload-001", "wf", nil)
	s1.AppendEvent("reload-001", Event{Type: EventRunStarted})
	s1.AppendEvent("reload-001", Event{Type: EventNodeStarted, NodeID: "a"})
	s1.WriteArtifact(&Artifact{RunID: "reload-001", NodeID: "a", Version: 0, Data: map[string]interface{}{"x": "y"}})

	// Second store instance: read data back (fresh seq counters).
	s2, _ := New(dir)

	run, err := s2.LoadRun("reload-001")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.WorkflowName != "wf" {
		t.Errorf("WorkflowName = %q", run.WorkflowName)
	}

	events, _ := s2.LoadEvents("reload-001")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}

	art, _ := s2.LoadArtifact("reload-001", "a", 0)
	if art.Data["x"] != "y" {
		t.Errorf("artifact data = %v", art.Data)
	}

	// Appending new events starts from seq 0 on fresh store (independent counter).
	evt, _ := s2.AppendEvent("reload-001", Event{Type: EventRunFinished})
	if evt.Seq != 0 {
		t.Errorf("fresh store seq = %d, want 0", evt.Seq)
	}
}
