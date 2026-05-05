package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func tmpStore(t *testing.T) *FilesystemRunStore {
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

// Resume from failed_resumable / cancelled must clear FinishedAt.
// Otherwise the editor keys the duration ticker on a stale terminal
// timestamp and the elapsed-time display freezes mid-run.
func TestUpdateRunStatusClearsFinishedAtOnResume(t *testing.T) {
	cases := []struct {
		name     string
		terminal RunStatus
	}{
		{"from_failed_resumable", RunStatusFailedResumable},
		{"from_cancelled", RunStatusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			s.CreateRun("run-resume", "wf", nil)

			if err := s.UpdateRunStatus("run-resume", tc.terminal, "transient"); err != nil {
				t.Fatalf("UpdateRunStatus %s: %v", tc.terminal, err)
			}
			r, _ := s.LoadRun("run-resume")
			if r.FinishedAt == nil {
				t.Fatalf("precondition: FinishedAt should be set after %s", tc.terminal)
			}

			if err := s.UpdateRunStatus("run-resume", RunStatusRunning, ""); err != nil {
				t.Fatalf("UpdateRunStatus running: %v", err)
			}
			r, _ = s.LoadRun("run-resume")
			if r.Status != RunStatusRunning {
				t.Errorf("Status = %q, want running", r.Status)
			}
			if r.FinishedAt != nil {
				t.Errorf("FinishedAt should be cleared on resume, got %v", r.FinishedAt)
			}
			if r.Error != "" {
				t.Errorf("Error should be cleared on resume, got %q", r.Error)
			}
		})
	}
}

// LoadRun heals legacy runs persisted by an older binary that left
// FinishedAt set across a resume into Running.
func TestLoadRunHealsStaleFinishedAt(t *testing.T) {
	s := tmpStore(t)
	if _, err := s.CreateRun("run-stale", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Simulate what an older binary would leave on disk: status=running
	// with finished_at populated. Write directly to bypass the
	// UpdateRunStatus normalization.
	r, err := s.LoadRun("run-stale")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	terminal := time.Now().UTC().Add(-time.Hour)
	r.Status = RunStatusRunning
	r.FinishedAt = &terminal
	if err := s.SaveRun(r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// LoadRun must zero FinishedAt for status=running.
	healed, err := s.LoadRun("run-stale")
	if err != nil {
		t.Fatalf("LoadRun (heal): %v", err)
	}
	if healed.FinishedAt != nil {
		t.Errorf("FinishedAt should be cleared by LoadRun, got %v", healed.FinishedAt)
	}

	// And the heal must be persisted to disk so subsequent reloads stay
	// clean even without going through LoadRun in the same process.
	raw, err := os.ReadFile(s.runJSONPath("run-stale"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var onDisk Run
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if onDisk.FinishedAt != nil {
		t.Errorf("FinishedAt should be persisted as nil on disk, got %v", onDisk.FinishedAt)
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
		EventLLMPrompt,
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
		EventBudgetExceeded,
		EventRunFinished,
		EventRunFailed,
		EventRunCancelled,
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

	// On a fresh store opening a pre-existing run, the seq counter must be
	// seeded from events.jsonl on the first AppendEvent so that we don't emit
	// duplicate sequence numbers (which would break monotonic ordering and
	// any consumer that dedups by Seq). Two events were appended above with
	// Seq 0 and 1, so the next append must be Seq 2.
	evt, _ := s2.AppendEvent("reload-001", Event{Type: EventRunFinished})
	if evt.Seq != 2 {
		t.Errorf("seeded seq after reload = %d, want 2", evt.Seq)
	}
}

// ---------------------------------------------------------------------------
// Path traversal rejection
// ---------------------------------------------------------------------------

func TestPathTraversalRejected(t *testing.T) {
	s := tmpStore(t)

	// CreateRun with traversal in ID.
	_, err := s.CreateRun("../../etc", "wf", nil)
	if err == nil {
		t.Fatal("expected error for path traversal in run ID")
	}

	// WriteArtifact with traversal in NodeID.
	s.CreateRun("safe-run", "wf", nil)
	err = s.WriteArtifact(&Artifact{
		RunID:  "safe-run",
		NodeID: "../../../etc",
		Data:   map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for path traversal in node ID")
	}

	// WriteInteraction with slash in ID.
	err = s.WriteInteraction(&Interaction{
		ID:    "foo/bar",
		RunID: "safe-run",
	})
	if err == nil {
		t.Fatal("expected error for path separator in interaction ID")
	}

	// LoadArtifact with traversal.
	_, err = s.LoadArtifact("safe-run", "../../secret", 0)
	if err == nil {
		t.Fatal("expected error for path traversal in LoadArtifact")
	}

	// LoadInteraction with traversal.
	_, err = s.LoadInteraction("safe-run", "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal in LoadInteraction")
	}
}

// ---------------------------------------------------------------------------
// Artifact index (R1: O(1) LoadLatestArtifact)
// ---------------------------------------------------------------------------

func TestArtifactIndexUpdatedOnWrite(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-idx", "wf", nil)

	// Write two versions for the same node.
	for v := 0; v < 3; v++ {
		if err := s.WriteArtifact(&Artifact{
			RunID:   "run-idx",
			NodeID:  "analyzer",
			Version: v,
			Data:    map[string]interface{}{"v": float64(v)},
		}); err != nil {
			t.Fatalf("WriteArtifact v%d: %v", v, err)
		}
	}

	r, err := s.LoadRun("run-idx")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.ArtifactIndex == nil {
		t.Fatal("ArtifactIndex should be set")
	}
	if got, want := r.ArtifactIndex["analyzer"], 2; got != want {
		t.Errorf("ArtifactIndex[analyzer] = %d, want %d", got, want)
	}
}

func TestLoadLatestArtifactUsesIndex(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-fast", "wf", nil)

	for v := 0; v < 3; v++ {
		s.WriteArtifact(&Artifact{
			RunID:   "run-fast",
			NodeID:  "planner",
			Version: v,
			Data:    map[string]interface{}{"v": float64(v)},
		})
	}

	latest, err := s.LoadLatestArtifact("run-fast", "planner")
	if err != nil {
		t.Fatalf("LoadLatestArtifact: %v", err)
	}
	if latest.Version != 2 {
		t.Errorf("Version = %d, want 2", latest.Version)
	}
	if latest.Data["v"] != float64(2) {
		t.Errorf("Data[v] = %v, want 2", latest.Data["v"])
	}
}

func TestLoadLatestArtifactFallbackWithoutIndex(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-noindex", "wf", nil)

	// Write artifacts then manually clear the index to simulate an old-format run.
	for v := 0; v < 2; v++ {
		s.WriteArtifact(&Artifact{
			RunID:   "run-noindex",
			NodeID:  "reviewer",
			Version: v,
			Data:    map[string]interface{}{"v": float64(v)},
		})
	}

	// Clear the index in run.json.
	r, _ := s.LoadRun("run-noindex")
	r.ArtifactIndex = nil
	s.SaveRun(r)

	// LoadLatestArtifact should still work via directory scan.
	latest, err := s.LoadLatestArtifact("run-noindex", "reviewer")
	if err != nil {
		t.Fatalf("LoadLatestArtifact fallback: %v", err)
	}
	if latest.Version != 1 {
		t.Errorf("Version = %d, want 1", latest.Version)
	}
}

func TestArtifactIndexMultipleNodes(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-multi", "wf", nil)

	s.WriteArtifact(&Artifact{RunID: "run-multi", NodeID: "a", Version: 0, Data: map[string]interface{}{"n": "a"}})
	s.WriteArtifact(&Artifact{RunID: "run-multi", NodeID: "b", Version: 0, Data: map[string]interface{}{"n": "b"}})
	s.WriteArtifact(&Artifact{RunID: "run-multi", NodeID: "a", Version: 1, Data: map[string]interface{}{"n": "a2"}})

	r, _ := s.LoadRun("run-multi")
	if r.ArtifactIndex["a"] != 1 {
		t.Errorf("ArtifactIndex[a] = %d, want 1", r.ArtifactIndex["a"])
	}
	if r.ArtifactIndex["b"] != 0 {
		t.Errorf("ArtifactIndex[b] = %d, want 0", r.ArtifactIndex["b"])
	}
}

// ---------------------------------------------------------------------------
// Checkpoint with embedded interaction questions (R4)
// ---------------------------------------------------------------------------

func TestCheckpointInteractionQuestionsRoundTrip(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun("run-cp", "wf", nil)

	questions := map[string]interface{}{
		"approve": "Do you approve?",
		"comment": "Any feedback?",
	}
	cp := &Checkpoint{
		NodeID:               "human_review",
		InteractionID:        "run-cp_human_review",
		Outputs:              map[string]map[string]interface{}{"agent": {"result": "ok"}},
		LoopCounters:         map[string]int{},
		ArtifactVersions:     map[string]int{},
		Vars:                 map[string]interface{}{"repo": "iterion"},
		InteractionQuestions: questions,
	}

	if err := s.PauseRun("run-cp", cp); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	r, err := s.LoadRun("run-cp")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("Checkpoint should be set")
	}
	if r.Checkpoint.InteractionQuestions == nil {
		t.Fatal("InteractionQuestions should be set")
	}
	if r.Checkpoint.InteractionQuestions["approve"] != "Do you approve?" {
		t.Errorf("InteractionQuestions[approve] = %v", r.Checkpoint.InteractionQuestions["approve"])
	}
}

// ---------------------------------------------------------------------------
// Path traversal rejection
// ---------------------------------------------------------------------------

func TestSanitizePathComponent(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"valid_id", true},
		{"run-001", true},
		{"run_with_underscores", true},
		{"", false},
		{"..", false},
		{"foo/../bar", false},
		{"foo/bar", false},
		{"foo\\bar", false},
		{"foo\x00bar", false},
	}
	for _, tt := range tests {
		err := sanitizePathComponent("test", tt.input)
		if tt.ok && err != nil {
			t.Errorf("sanitize(%q) = %v, want nil", tt.input, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("sanitize(%q) = nil, want error", tt.input)
		}
	}
}

// ---------------------------------------------------------------------------
// Atomic write — torn-write resilience
// ---------------------------------------------------------------------------

// TestWriteFileAtomic_NoTornWrites verifies the atomic-write helper never
// leaves a partial run.json visible to readers: the destination either
// contains the prior valid bytes or the new valid bytes, never a truncated
// in-between state.
//
// Strategy: write a large payload many times in succession; between writes,
// read the file and ensure it parses as valid JSON. The temp file must never
// be observed under the destination name.
func TestWriteFileAtomic_NoTornWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.json")

	// First write establishes the file.
	initial := map[string]any{"version": 0, "payload": "x"}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Repeatedly overwrite with progressively larger payloads, reading
	// between writes to assert the destination is always parseable.
	for i := 1; i <= 50; i++ {
		// Build a payload large enough that a torn write would be obvious
		// (>4KB, exceeds typical page boundary).
		filler := make([]byte, 4096+i*128)
		for j := range filler {
			filler[j] = byte('a' + (j % 26))
		}
		next := map[string]any{"version": i, "payload": string(filler)}
		data, _ := json.MarshalIndent(next, "", "  ")
		if err := writeFileAtomic(path, data, 0o600); err != nil {
			t.Fatalf("write iter %d: %v", i, err)
		}

		// Read back and confirm it's a fully formed JSON object.
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read iter %d: %v", i, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(got, &parsed); err != nil {
			t.Fatalf("torn write detected at iter %d: %v\nbytes: %q", i, err, got[:min(64, len(got))])
		}
		if v, _ := parsed["version"].(float64); int(v) != i {
			t.Fatalf("iter %d: read version %v, want %d", i, parsed["version"], i)
		}

		// The .tmp file must not survive a successful write.
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("iter %d: leftover %s.tmp (stat err: %v)", i, path, err)
		}
	}
}

// TestWriteFileAtomic_PreservesPriorOnFailure verifies that if the rename
// itself never happens (simulated here by checking that the tmp file is the
// only thing modified during the in-between state), readers continue to see
// the prior valid contents.
func TestWriteFileAtomic_PreservesPriorOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.json")

	priorPayload := map[string]any{"version": 0, "data": "prior"}
	data, _ := json.MarshalIndent(priorPayload, "", "  ")
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Manually create a stale .tmp file (simulates a crash mid-write where
	// the rename never happened). The next successful write must overwrite
	// it cleanly, and the destination must still hold the prior payload
	// in the meantime.
	if err := os.WriteFile(path+".tmp", []byte("PARTIAL"), 0o600); err != nil {
		t.Fatalf("seed stale tmp: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("destination corrupted by stale tmp: %v", err)
	}
	if parsed["data"] != "prior" {
		t.Errorf("destination = %v, want prior payload", parsed)
	}

	// A subsequent write should succeed and overwrite the stale tmp.
	newPayload, _ := json.MarshalIndent(map[string]any{"version": 1, "data": "new"}, "", "  ")
	if err := writeFileAtomic(path, newPayload, 0o600); err != nil {
		t.Fatalf("recovery write: %v", err)
	}
	got, _ = os.ReadFile(path)
	_ = json.Unmarshal(got, &parsed)
	if parsed["data"] != "new" {
		t.Errorf("after recovery write: data = %v, want new", parsed["data"])
	}
}

// ---------------------------------------------------------------------------
// Self-ignoring .gitignore at store root
// ---------------------------------------------------------------------------

func TestNewWritesSelfIgnoringGitignore(t *testing.T) {
	t.Run("creates .gitignore when absent", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := New(dir); err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
		if err != nil {
			t.Fatalf("read .gitignore: %v", err)
		}
		if string(got) != "**\n" {
			t.Errorf(".gitignore = %q, want %q", string(got), "**\n")
		}
	})

	t.Run("preserves existing .gitignore", func(t *testing.T) {
		dir := t.TempDir()
		custom := []byte("# user-managed\nfoo\n")
		path := filepath.Join(dir, ".gitignore")
		if err := os.WriteFile(path, custom, 0o644); err != nil {
			t.Fatalf("seed .gitignore: %v", err)
		}
		if _, err := New(dir); err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read .gitignore: %v", err)
		}
		if string(got) != string(custom) {
			t.Errorf("existing .gitignore was overwritten: got %q, want %q", string(got), string(custom))
		}
	})
}
