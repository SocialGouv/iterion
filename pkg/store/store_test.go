package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// mustCreateRun creates a running run or fails the test.
func mustCreateRun(t *testing.T, s *FilesystemRunStore, id string) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun(%q): %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// Run persistence
// ---------------------------------------------------------------------------

func TestCreateAndLoadRun(t *testing.T) {
	s := tmpStore(t)

	inputs := map[string]any{"repo": "iterion", "branch": "main"}
	r, err := s.CreateRun(context.Background(), "run-001", "pr_refine", inputs)
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
	loaded, err := s.LoadRun(context.Background(), "run-001")
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
	s.CreateRun(context.Background(), "run-002", "ci_fix", nil)

	// Pause.
	if err := s.UpdateRunStatus(context.Background(), "run-002", RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("UpdateRunStatus pause: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-002")
	if r.Status != RunStatusPausedWaitingHuman {
		t.Errorf("Status = %q, want paused_waiting_human", r.Status)
	}
	if r.FinishedAt != nil {
		t.Error("FinishedAt should be nil while paused")
	}

	// Finish.
	if err := s.UpdateRunStatus(context.Background(), "run-002", RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus finish: %v", err)
	}
	r, _ = s.LoadRun(context.Background(), "run-002")
	if r.Status != RunStatusFinished {
		t.Errorf("Status = %q, want finished", r.Status)
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt should be set after finish")
	}
}

func TestUpdateRunStatusFailed(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-003", "ci_fix", nil)

	if err := s.UpdateRunStatus(context.Background(), "run-003", RunStatusFailed, "budget_exceeded"); err != nil {
		t.Fatalf("UpdateRunStatus fail: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-003")
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
// Otherwise the studio keys the duration ticker on a stale terminal
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
			s.CreateRun(context.Background(), "run-resume", "wf", nil)

			if err := s.UpdateRunStatus(context.Background(), "run-resume", tc.terminal, "transient"); err != nil {
				t.Fatalf("UpdateRunStatus %s: %v", tc.terminal, err)
			}
			r, _ := s.LoadRun(context.Background(), "run-resume")
			if r.FinishedAt == nil {
				t.Fatalf("precondition: FinishedAt should be set after %s", tc.terminal)
			}

			if err := s.UpdateRunStatus(context.Background(), "run-resume", RunStatusRunning, ""); err != nil {
				t.Fatalf("UpdateRunStatus running: %v", err)
			}
			r, _ = s.LoadRun(context.Background(), "run-resume")
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
	if _, err := s.CreateRun(context.Background(), "run-stale", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Simulate what an older binary would leave on disk: status=running
	// with finished_at populated. Write directly to bypass the
	// UpdateRunStatus normalization.
	r, err := s.LoadRun(context.Background(), "run-stale")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	terminal := time.Now().UTC().Add(-time.Hour)
	r.Status = RunStatusRunning
	r.FinishedAt = &terminal
	if err := s.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// LoadRun must zero FinishedAt for status=running.
	healed, err := s.LoadRun(context.Background(), "run-stale")
	if err != nil {
		t.Fatalf("LoadRun (heal): %v", err)
	}
	if healed.FinishedAt != nil {
		t.Errorf("FinishedAt should be cleared by LoadRun, got %v", healed.FinishedAt)
	}

	// The heal is in-memory only: a read never writes run.json back (see
	// TestLoadRunDoesNotWriteBack — a reader is not the run's owner), so the
	// stale field stays on disk until the next legitimate write, and every
	// LoadRun heals it again.
	raw, err := os.ReadFile(s.runJSONPath("run-stale"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var onDisk Run
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if onDisk.FinishedAt == nil {
		t.Error("LoadRun must not write the heal back to disk")
	}
	again, err := s.LoadRun(context.Background(), "run-stale")
	if err != nil {
		t.Fatalf("LoadRun (heal again): %v", err)
	}
	if again.FinishedAt != nil {
		t.Errorf("FinishedAt should be healed on every read, got %v", again.FinishedAt)
	}
}

func TestListRuns(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "alpha", "w1", nil)
	s.CreateRun(context.Background(), "beta", "w2", nil)
	s.CreateRun(context.Background(), "gamma", "w3", nil)

	ids, err := s.ListRuns(context.Background())
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
	s.CreateRun(context.Background(), "run-evt", "wf", nil)

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
		_, err := s.AppendEvent(context.Background(), "run-evt", Event{
			Type:   typ,
			NodeID: "agent_a",
			Data:   map[string]any{"info": string(typ)},
		})
		if err != nil {
			t.Fatalf("AppendEvent %s: %v", typ, err)
		}
	}

	events, err := s.LoadEvents(context.Background(), "run-evt")
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

func TestAppendEventStampsLogOffsetFromCallback(t *testing.T) {
	s := tmpStore(t)
	if _, err := s.CreateRun(context.Background(), "run-log", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Simulate the runview Service's per-run log buffer: a counter
	// that the AppendEvent path reads via the installed callback.
	var pos int64
	s.SetLogPositionFn(func(runID string) int64 {
		if runID != "run-log" {
			t.Errorf("callback runID = %q, want run-log", runID)
		}
		return pos
	})

	pos = 0
	first, err := s.AppendEvent(context.Background(), "run-log", Event{Type: EventRunStarted})
	if err != nil {
		t.Fatalf("AppendEvent #1: %v", err)
	}
	if first.LogOffset != 0 {
		t.Errorf("first.LogOffset = %d, want 0", first.LogOffset)
	}

	pos = 1234
	second, err := s.AppendEvent(context.Background(), "run-log", Event{Type: EventNodeStarted, NodeID: "x"})
	if err != nil {
		t.Fatalf("AppendEvent #2: %v", err)
	}
	if second.LogOffset != 1234 {
		t.Errorf("second.LogOffset = %d, want 1234", second.LogOffset)
	}

	// Caller-supplied LogOffset is preserved (e.g. Mongo-mode replay
	// or synthetic test events).
	pos = 9999
	third, err := s.AppendEvent(context.Background(), "run-log", Event{
		Type:      EventNodeFinished,
		NodeID:    "x",
		LogOffset: 5555,
	})
	if err != nil {
		t.Fatalf("AppendEvent #3: %v", err)
	}
	if third.LogOffset != 5555 {
		t.Errorf("third.LogOffset = %d, want 5555 (caller value preserved)", third.LogOffset)
	}

	// No callback installed → LogOffset stays 0.
	s.SetLogPositionFn(nil)
	fourth, err := s.AppendEvent(context.Background(), "run-log", Event{Type: EventRunFinished})
	if err != nil {
		t.Fatalf("AppendEvent #4: %v", err)
	}
	if fourth.LogOffset != 0 {
		t.Errorf("fourth.LogOffset = %d, want 0 (no callback)", fourth.LogOffset)
	}
}

func TestAllEventTypesPersistable(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-all-evt", "wf", nil)

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
		_, err := s.AppendEvent(context.Background(), "run-all-evt", Event{
			Type:     typ,
			BranchID: "branch-0",
			NodeID:   "node-x",
			Data:     map[string]any{"type": string(typ)},
		})
		if err != nil {
			t.Fatalf("AppendEvent %s: %v", typ, err)
		}
	}

	events, err := s.LoadEvents(context.Background(), "run-all-evt")
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
	events, err := s.LoadEvents(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if events != nil {
		t.Errorf("expected nil, got %v", events)
	}
}

func TestEventDataRoundTrip(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-data", "wf", nil)

	data := map[string]any{
		"model":         "claude-opus-4-20250514",
		"input_tokens":  float64(1500),
		"output_tokens": float64(300),
		"cost_usd":      0.042,
		"tools_used":    []any{"read_file", "edit_file"},
	}
	_, err := s.AppendEvent(context.Background(), "run-data", Event{
		Type: EventLLMStepFinished,
		Data: data,
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, _ := s.LoadEvents(context.Background(), "run-data")
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
	s.CreateRun(context.Background(), "run-art", "wf", nil)

	a := &Artifact{
		RunID:   "run-art",
		NodeID:  "reviewer",
		Version: 0,
		Data:    map[string]any{"verdict": "approved", "comments": "LGTM"},
	}
	if err := s.WriteArtifact(context.Background(), a); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}

	loaded, err := s.LoadArtifact(context.Background(), "run-art", "reviewer", 0)
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
	s.CreateRun(context.Background(), "run-ver", "wf", nil)

	for v := 0; v < 3; v++ {
		a := &Artifact{
			RunID:   "run-ver",
			NodeID:  "planner",
			Version: v,
			Data:    map[string]any{"iteration": float64(v)},
		}
		if err := s.WriteArtifact(context.Background(), a); err != nil {
			t.Fatalf("WriteArtifact v%d: %v", v, err)
		}
	}

	// Load specific version.
	a1, err := s.LoadArtifact(context.Background(), "run-ver", "planner", 1)
	if err != nil {
		t.Fatalf("LoadArtifact v1: %v", err)
	}
	if a1.Data["iteration"] != float64(1) {
		t.Errorf("v1 iteration = %v, want 1", a1.Data["iteration"])
	}

	// Load latest.
	latest, err := s.LoadLatestArtifact(context.Background(), "run-ver", "planner")
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
	s.CreateRun(context.Background(), "run-human", "wf", nil)

	now := time.Now().UTC()
	i := &Interaction{
		ID:          "int-001",
		RunID:       "run-human",
		NodeID:      "human_review",
		RequestedAt: now,
		Questions: map[string]any{
			"approve": "Do you approve this PR?",
			"comment": "Any comments?",
		},
	}
	if err := s.WriteInteraction(context.Background(), i); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}

	loaded, err := s.LoadInteraction(context.Background(), "run-human", "int-001")
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
	loaded.Answers = map[string]any{
		"approve": true,
		"comment": "Ship it!",
	}
	if err := s.WriteInteraction(context.Background(), loaded); err != nil {
		t.Fatalf("WriteInteraction update: %v", err)
	}

	reloaded, _ := s.LoadInteraction(context.Background(), "run-human", "int-001")
	if reloaded.AnsweredAt == nil {
		t.Fatal("AnsweredAt should be set")
	}
	if reloaded.Answers["comment"] != "Ship it!" {
		t.Errorf("answer = %v", reloaded.Answers["comment"])
	}
}

func TestListInteractions(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-li", "wf", nil)

	for _, id := range []string{"int-a", "int-b", "int-c"} {
		s.WriteInteraction(context.Background(), &Interaction{
			ID:          id,
			RunID:       "run-li",
			NodeID:      "human",
			RequestedAt: time.Now().UTC(),
		})
	}

	ids, err := s.ListInteractions(context.Background(), "run-li")
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
	ids, err := s.ListInteractions(context.Background(), "nonexistent")
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
	run, _ := s.CreateRun(context.Background(), "replay-001", "pr_refine_single_model", map[string]any{
		"repo":   "iterion",
		"branch": "feat/store",
	})

	// 2. Emit events through a typical lifecycle.
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventRunStarted})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventNodeStarted, NodeID: "context_builder"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventLLMRequest, NodeID: "context_builder"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventLLMStepFinished, NodeID: "context_builder", Data: map[string]any{"tokens": float64(500)}})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventArtifactWritten, NodeID: "context_builder"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventNodeFinished, NodeID: "context_builder"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventEdgeSelected, Data: map[string]any{"from": "context_builder", "to": "reviewer"}})

	// 3. Write artifact.
	s.WriteArtifact(context.Background(), &Artifact{
		RunID:   run.ID,
		NodeID:  "context_builder",
		Version: 0,
		Data:    map[string]any{"diff_summary": "Added store package"},
	})

	// 4. Human pause.
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventNodeStarted, NodeID: "human_review"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventHumanInputRequested, NodeID: "human_review"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventRunPaused})
	s.UpdateRunStatus(context.Background(), run.ID, RunStatusPausedWaitingHuman, "")

	s.WriteInteraction(context.Background(), &Interaction{
		ID:          "int-replay",
		RunID:       run.ID,
		NodeID:      "human_review",
		RequestedAt: time.Now().UTC(),
		Questions:   map[string]any{"approve": "Approve?"},
	})

	// 5. Resume.
	answered := time.Now().UTC()
	s.WriteInteraction(context.Background(), &Interaction{
		ID:          "int-replay",
		RunID:       run.ID,
		NodeID:      "human_review",
		RequestedAt: time.Now().UTC(),
		AnsweredAt:  &answered,
		Questions:   map[string]any{"approve": "Approve?"},
		Answers:     map[string]any{"approve": true},
	})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventHumanAnswersRecorded, NodeID: "human_review"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventRunResumed})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventNodeFinished, NodeID: "human_review"})
	s.AppendEvent(context.Background(), run.ID, Event{Type: EventRunFinished})
	s.UpdateRunStatus(context.Background(), run.ID, RunStatusFinished, "")

	// --- Verify full reload ---

	// Run.
	reRun, err := s.LoadRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if reRun.Status != RunStatusFinished {
		t.Errorf("Status = %q", reRun.Status)
	}

	// Events.
	events, err := s.LoadEvents(context.Background(), run.ID)
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
	art, err := s.LoadArtifact(context.Background(), run.ID, "context_builder", 0)
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if art.Data["diff_summary"] != "Added store package" {
		t.Errorf("artifact data = %v", art.Data)
	}

	// Interaction.
	inter, err := s.LoadInteraction(context.Background(), run.ID, "int-replay")
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
	s1.CreateRun(context.Background(), "reload-001", "wf", nil)
	s1.AppendEvent(context.Background(), "reload-001", Event{Type: EventRunStarted})
	s1.AppendEvent(context.Background(), "reload-001", Event{Type: EventNodeStarted, NodeID: "a"})
	s1.WriteArtifact(context.Background(), &Artifact{RunID: "reload-001", NodeID: "a", Version: 0, Data: map[string]any{"x": "y"}})

	// Second store instance: read data back (fresh seq counters).
	s2, _ := New(dir)

	run, err := s2.LoadRun(context.Background(), "reload-001")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.WorkflowName != "wf" {
		t.Errorf("WorkflowName = %q", run.WorkflowName)
	}

	events, _ := s2.LoadEvents(context.Background(), "reload-001")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}

	art, _ := s2.LoadArtifact(context.Background(), "reload-001", "a", 0)
	if art.Data["x"] != "y" {
		t.Errorf("artifact data = %v", art.Data)
	}

	// On a fresh store opening a pre-existing run, the seq counter must be
	// seeded from events.jsonl on the first AppendEvent so that we don't emit
	// duplicate sequence numbers (which would break monotonic ordering and
	// any consumer that dedups by Seq). Two events were appended above with
	// Seq 0 and 1, so the next append must be Seq 2.
	evt, _ := s2.AppendEvent(context.Background(), "reload-001", Event{Type: EventRunFinished})
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
	_, err := s.CreateRun(context.Background(), "../../etc", "wf", nil)
	if err == nil {
		t.Fatal("expected error for path traversal in run ID")
	}

	// WriteArtifact with traversal in NodeID.
	s.CreateRun(context.Background(), "safe-run", "wf", nil)
	err = s.WriteArtifact(context.Background(), &Artifact{
		RunID:  "safe-run",
		NodeID: "../../../etc",
		Data:   map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for path traversal in node ID")
	}

	// WriteInteraction with slash in ID.
	err = s.WriteInteraction(context.Background(), &Interaction{
		ID:    "foo/bar",
		RunID: "safe-run",
	})
	if err == nil {
		t.Fatal("expected error for path separator in interaction ID")
	}

	// LoadArtifact with traversal.
	_, err = s.LoadArtifact(context.Background(), "safe-run", "../../secret", 0)
	if err == nil {
		t.Fatal("expected error for path traversal in LoadArtifact")
	}

	// LoadInteraction with traversal.
	_, err = s.LoadInteraction(context.Background(), "safe-run", "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal in LoadInteraction")
	}
}

// ---------------------------------------------------------------------------
// Artifact index (R1: O(1) LoadLatestArtifact)
// ---------------------------------------------------------------------------

func TestArtifactIndexUpdatedOnWrite(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-idx", "wf", nil)

	// Write two versions for the same node.
	for v := 0; v < 3; v++ {
		if err := s.WriteArtifact(context.Background(), &Artifact{
			RunID:   "run-idx",
			NodeID:  "analyzer",
			Version: v,
			Data:    map[string]any{"v": float64(v)},
		}); err != nil {
			t.Fatalf("WriteArtifact v%d: %v", v, err)
		}
	}

	r, err := s.LoadRun(context.Background(), "run-idx")
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
	s.CreateRun(context.Background(), "run-fast", "wf", nil)

	for v := 0; v < 3; v++ {
		s.WriteArtifact(context.Background(), &Artifact{
			RunID:   "run-fast",
			NodeID:  "planner",
			Version: v,
			Data:    map[string]any{"v": float64(v)},
		})
	}

	latest, err := s.LoadLatestArtifact(context.Background(), "run-fast", "planner")
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
	s.CreateRun(context.Background(), "run-noindex", "wf", nil)

	// Write artifacts then manually clear the index to simulate an old-format run.
	for v := 0; v < 2; v++ {
		s.WriteArtifact(context.Background(), &Artifact{
			RunID:   "run-noindex",
			NodeID:  "reviewer",
			Version: v,
			Data:    map[string]any{"v": float64(v)},
		})
	}

	// Clear the index in run.json.
	r, _ := s.LoadRun(context.Background(), "run-noindex")
	r.ArtifactIndex = nil
	s.SaveRun(context.Background(), r)

	// LoadLatestArtifact should still work via directory scan.
	latest, err := s.LoadLatestArtifact(context.Background(), "run-noindex", "reviewer")
	if err != nil {
		t.Fatalf("LoadLatestArtifact fallback: %v", err)
	}
	if latest.Version != 1 {
		t.Errorf("Version = %d, want 1", latest.Version)
	}
}

func TestArtifactIndexMultipleNodes(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-multi", "wf", nil)

	s.WriteArtifact(context.Background(), &Artifact{RunID: "run-multi", NodeID: "a", Version: 0, Data: map[string]any{"n": "a"}})
	s.WriteArtifact(context.Background(), &Artifact{RunID: "run-multi", NodeID: "b", Version: 0, Data: map[string]any{"n": "b"}})
	s.WriteArtifact(context.Background(), &Artifact{RunID: "run-multi", NodeID: "a", Version: 1, Data: map[string]any{"n": "a2"}})

	r, _ := s.LoadRun(context.Background(), "run-multi")
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
	s.CreateRun(context.Background(), "run-cp", "wf", nil)

	questions := map[string]any{
		"approve": "Do you approve?",
		"comment": "Any feedback?",
	}
	cp := &Checkpoint{
		NodeID:               "human_review",
		InteractionID:        "run-cp_human_review",
		Outputs:              map[string]map[string]any{"agent": {"result": "ok"}},
		LoopCounters:         map[string]int{},
		ArtifactVersions:     map[string]int{},
		Vars:                 map[string]any{"repo": "iterion"},
		InteractionQuestions: questions,
	}

	if err := s.PauseRun(context.Background(), "run-cp", cp); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	r, err := s.LoadRun(context.Background(), "run-cp")
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

func TestRunFilesStore(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "run-files-1", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	t.Run("EnsureRunFilesDir is idempotent + returns absolute path under runs/<id>/", func(t *testing.T) {
		dir1, err := s.EnsureRunFilesDir(ctx, "run-files-1")
		if err != nil {
			t.Fatalf("EnsureRunFilesDir: %v", err)
		}
		if !filepath.IsAbs(dir1) {
			t.Errorf("expected absolute path, got %q", dir1)
		}
		if !strings.Contains(dir1, "runs/run-files-1/artifact_files") && !strings.Contains(dir1, "runs\\run-files-1\\artifact_files") {
			t.Errorf("expected per-run subpath, got %q", dir1)
		}
		dir2, err := s.EnsureRunFilesDir(ctx, "run-files-1")
		if err != nil {
			t.Fatalf("EnsureRunFilesDir (second call): %v", err)
		}
		if dir1 != dir2 {
			t.Errorf("EnsureRunFilesDir not idempotent: %q vs %q", dir1, dir2)
		}
	})

	t.Run("ListRunFiles returns empty when nothing has been written", func(t *testing.T) {
		got, err := s.ListRunFiles(ctx, "run-files-1")
		if err != nil {
			t.Fatalf("ListRunFiles: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty list, got %d entries: %v", len(got), got)
		}
	})

	t.Run("ListRunFiles returns nil for a run that never created the dir", func(t *testing.T) {
		if _, err := s.CreateRun(ctx, "run-files-empty", "wf", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.ListRunFiles(ctx, "run-files-empty")
		if err != nil {
			t.Fatalf("ListRunFiles: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("Round-trip: write a file via direct fs + read via OpenRunFile", func(t *testing.T) {
		dir, err := s.EnsureRunFilesDir(ctx, "run-files-1")
		if err != nil {
			t.Fatalf("EnsureRunFilesDir: %v", err)
		}
		// Tools write through the bind-mount; in tests we write directly.
		nested := filepath.Join(dir, "renovacy", "dbug-1.0-to-1.7.md")
		if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(nested, []byte("# upgrade notes\nbreaking: none\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		// List finds it.
		listed, err := s.ListRunFiles(ctx, "run-files-1")
		if err != nil {
			t.Fatalf("ListRunFiles: %v", err)
		}
		if len(listed) != 1 || listed[0].Path != "renovacy/dbug-1.0-to-1.7.md" {
			t.Fatalf("ListRunFiles = %v, want 1 entry renovacy/dbug-1.0-to-1.7.md", listed)
		}
		if listed[0].Size != int64(len("# upgrade notes\nbreaking: none\n")) {
			t.Errorf("size = %d, want %d", listed[0].Size, len("# upgrade notes\nbreaking: none\n"))
		}
		// Open returns content.
		rc, info, err := s.OpenRunFile(ctx, "run-files-1", "renovacy/dbug-1.0-to-1.7.md")
		if err != nil {
			t.Fatalf("OpenRunFile: %v", err)
		}
		defer rc.Close()
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(body) != "# upgrade notes\nbreaking: none\n" {
			t.Errorf("body = %q", string(body))
		}
		if info.Path != "renovacy/dbug-1.0-to-1.7.md" {
			t.Errorf("info.Path = %q", info.Path)
		}
	})

	t.Run("OpenRunFile rejects path traversal", func(t *testing.T) {
		// Plant a sensitive file outside the area to make the test
		// concrete. If escape succeeds, OpenRunFile would read it.
		runDir := s.runDir("run-files-1")
		secret := filepath.Join(runDir, "events.jsonl")
		_ = os.WriteFile(secret, []byte("SHOULD-NOT-LEAK"), 0o644)

		// Various escape attempts.
		for _, attempt := range []string{
			"../events.jsonl",
			"../../events.jsonl",
			"renovacy/../../events.jsonl",
			"/etc/passwd",
			"renovacy/../../../etc/passwd",
		} {
			if _, _, err := s.OpenRunFile(ctx, "run-files-1", attempt); err == nil {
				t.Errorf("path %q should have been rejected", attempt)
			}
		}
	})

	t.Run("OpenRunFile returns not-found on non-existent path", func(t *testing.T) {
		if _, _, err := s.OpenRunFile(ctx, "run-files-1", "no-such-file.md"); err == nil {
			t.Errorf("expected error for missing file")
		}
	})

	t.Run("AsRunFilesStore returns the FilesystemRunStore", func(t *testing.T) {
		var rs RunStore = s
		fs := AsRunFilesStore(rs)
		if fs == nil {
			t.Fatalf("AsRunFilesStore returned nil for FilesystemRunStore")
		}
	})
}

// ---------------------------------------------------------------------------
// Tool blobs (sidecar per-tool-call I/O bodies)
// ---------------------------------------------------------------------------

func TestToolBlobRoundtrip(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-tools-1"

	// Build a 32 KB body so we exercise both partial and full reads.
	body := make([]byte, 32*1024)
	for i := range body {
		body[i] = byte('a' + (i % 26))
	}

	size, err := s.WriteToolBlob(ctx, runID, "toolu_abc", "output", body)
	if err != nil {
		t.Fatalf("WriteToolBlob: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("WriteToolBlob size = %d, want %d", size, len(body))
	}

	t.Run("read full", func(t *testing.T) {
		got, total, eof, err := s.ReadToolBlob(ctx, runID, "toolu_abc", "output", 0, 0)
		if err != nil {
			t.Fatalf("ReadToolBlob: %v", err)
		}
		if total != int64(len(body)) {
			t.Errorf("total = %d, want %d", total, len(body))
		}
		if !eof {
			t.Errorf("eof = false, want true on full read")
		}
		if !bytes.Equal(got, body) {
			t.Errorf("bytes mismatch")
		}
	})

	t.Run("read paginated", func(t *testing.T) {
		const chunk = 8 * 1024
		var assembled []byte
		var offset int64
		for {
			got, total, eof, err := s.ReadToolBlob(ctx, runID, "toolu_abc", "output", offset, chunk)
			if err != nil {
				t.Fatalf("ReadToolBlob chunk: %v", err)
			}
			if total != int64(len(body)) {
				t.Errorf("total = %d, want %d", total, len(body))
			}
			assembled = append(assembled, got...)
			offset += int64(len(got))
			if eof {
				break
			}
			if len(got) == 0 {
				t.Fatalf("zero-length non-eof read at offset %d", offset)
			}
		}
		if !bytes.Equal(assembled, body) {
			t.Errorf("paginated reassembly mismatch")
		}
	})

	t.Run("read past end returns eof", func(t *testing.T) {
		got, total, eof, err := s.ReadToolBlob(ctx, runID, "toolu_abc", "output", int64(len(body))+10, 1024)
		if err != nil {
			t.Fatalf("ReadToolBlob past end: %v", err)
		}
		if len(got) != 0 || !eof {
			t.Errorf("past end: got %d bytes, eof=%v, want 0 bytes + eof=true", len(got), eof)
		}
		if total != int64(len(body)) {
			t.Errorf("total = %d, want %d", total, len(body))
		}
	})

	t.Run("rejects invalid kind", func(t *testing.T) {
		if _, err := s.WriteToolBlob(ctx, runID, "toolu_abc", "stdout", []byte("x")); err == nil {
			t.Error("expected error for kind=stdout")
		}
		if _, _, _, err := s.ReadToolBlob(ctx, runID, "toolu_abc", "stdout", 0, 0); err == nil {
			t.Error("expected read error for kind=stdout")
		}
	})

	t.Run("missing blob returns wrapped ErrNotExist", func(t *testing.T) {
		_, _, _, err := s.ReadToolBlob(ctx, runID, "toolu_nope", "input", 0, 0)
		if err == nil {
			t.Fatal("expected error for missing blob")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want one wrapping os.ErrNotExist", err)
		}
	})

	t.Run("AsToolBlobStore returns the FilesystemRunStore", func(t *testing.T) {
		var rs RunStore = s
		tbs := AsToolBlobStore(rs)
		if tbs == nil {
			t.Fatalf("AsToolBlobStore returned nil for FilesystemRunStore")
		}
	})
}

// ---------------------------------------------------------------------------
// UpdateRunStatusIf — compare-and-set on status
// ---------------------------------------------------------------------------

func TestUpdateRunStatusIfMatches(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-cas")

	changed, err := s.UpdateRunStatusIf(ctx, "run-cas", RunStatusCancelled, "user stop", []RunStatus{RunStatusRunning})
	if err != nil {
		t.Fatalf("UpdateRunStatusIf: %v", err)
	}
	if !changed {
		t.Fatalf("changed = false, want true (current status was in expectedFrom)")
	}

	r, err := s.LoadRun(ctx, "run-cas")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusCancelled {
		t.Errorf("Status = %q, want cancelled", r.Status)
	}
	if r.Error != "user stop" {
		t.Errorf("Error = %q, want %q", r.Error, "user stop")
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set for a terminal transition")
	}
}

// When the live status is not in expectedFrom, the write must be a no-op
// and changed=false — this is the race guard that prevents a stale
// transition from clobbering a concurrent one.
func TestUpdateRunStatusIfNoMatch(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-cas2")

	changed, err := s.UpdateRunStatusIf(ctx, "run-cas2", RunStatusCancelled, "stop", []RunStatus{RunStatusFinished})
	if err != nil {
		t.Fatalf("UpdateRunStatusIf: %v", err)
	}
	if changed {
		t.Fatalf("changed = true, want false (current status running not in {finished})")
	}

	r, err := s.LoadRun(ctx, "run-cas2")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusRunning {
		t.Errorf("Status = %q, want running (must be untouched)", r.Status)
	}
	if r.Error != "" {
		t.Errorf("Error = %q, want empty (no write should have landed)", r.Error)
	}
}

func TestUpdateRunStatusIfMissingRun(t *testing.T) {
	s := tmpStore(t)
	_, err := s.UpdateRunStatusIf(context.Background(), "does-not-exist", RunStatusCancelled, "", []RunStatus{RunStatusRunning})
	if err == nil {
		t.Fatal("UpdateRunStatusIf on missing run: err = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// SaveCheckpoint / FailRunResumable — checkpoint persistence
// ---------------------------------------------------------------------------

func TestSaveCheckpointRoundTrip(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-cp")

	cp := &Checkpoint{
		NodeID:       "node-a",
		LoopCounters: map[string]int{"node-a": 2},
		Vars:         map[string]any{"k": "v"},
	}
	if err := s.SaveCheckpoint(ctx, "run-cp", cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	r, err := s.LoadRun(ctx, "run-cp")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("Checkpoint = nil after SaveCheckpoint, want persisted")
	}
	if r.Checkpoint.NodeID != "node-a" {
		t.Errorf("Checkpoint.NodeID = %q, want node-a", r.Checkpoint.NodeID)
	}
	if got := r.Checkpoint.LoopCounters["node-a"]; got != 2 {
		t.Errorf("Checkpoint.LoopCounters[node-a] = %d, want 2", got)
	}
	if got := r.Checkpoint.Vars["k"]; got != "v" {
		t.Errorf("Checkpoint.Vars[k] = %v, want v", got)
	}
}

func TestCheckpointPausedNodeID(t *testing.T) {
	if got := (*Checkpoint)(nil).PausedNodeID(); got != "" {
		t.Fatalf("nil checkpoint paused node = %q", got)
	}
	sequential := &Checkpoint{NodeID: "review"}
	if got := sequential.PausedNodeID(); got != "review" {
		t.Fatalf("sequential paused node = %q, want review", got)
	}
	parallel := &Checkpoint{
		NodeID: "dispatch",
		Parallel: &ParallelCheckpoint{
			PendingNodeID: "branch_gate",
		},
	}
	if got := parallel.PausedNodeID(); got != "branch_gate" {
		t.Fatalf("parallel paused node = %q, want branch_gate", got)
	}
}

// failed_resumable must RETAIN the checkpoint (the resume entry point).
// This is the behaviour that makes a failed run recoverable.
func TestFailRunResumable(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-fr")

	cp := &Checkpoint{NodeID: "node-z"}
	if err := s.FailRunResumable(ctx, "run-fr", cp, "rate limited", ""); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	r, err := s.LoadRun(ctx, "run-fr")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusFailedResumable {
		t.Errorf("Status = %q, want failed_resumable", r.Status)
	}
	if r.Error != "rate limited" {
		t.Errorf("Error = %q, want %q", r.Error, "rate limited")
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "node-z" {
		t.Errorf("Checkpoint = %+v, want preserved with NodeID node-z", r.Checkpoint)
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set")
	}
}

// A terminal `failed` run keeps its checkpoint too: the run is over (no
// auto-resume) but the recovery point survives so an explicit rewind can
// still pick it up. FailRunTerminal is the atomic write used by the DSL
// fail-node path.
func TestFailRunTerminal(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-ft")

	cp := &Checkpoint{NodeID: "node-f"}
	if err := s.FailRunTerminal(ctx, "run-ft", cp, "workflow reached fail node", ""); err != nil {
		t.Fatalf("FailRunTerminal: %v", err)
	}

	r, err := s.LoadRun(ctx, "run-ft")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusFailed {
		t.Errorf("Status = %q, want failed", r.Status)
	}
	if r.Error != "workflow reached fail node" {
		t.Errorf("Error = %q, want %q", r.Error, "workflow reached fail node")
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "node-f" {
		t.Errorf("Checkpoint = %+v, want preserved with NodeID node-f", r.Checkpoint)
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set")
	}
}

// An operator cancel outranks a terminal failure racing in behind it:
// FailRunTerminal on a cancelled run is a no-op.
func TestFailRunTerminalCancelledWins(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-ft-cancel")
	if err := s.UpdateRunStatus(ctx, "run-ft-cancel", RunStatusCancelled, "operator stop"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	if err := s.FailRunTerminal(ctx, "run-ft-cancel", &Checkpoint{NodeID: "node-f"}, "late failure", ""); err != nil {
		t.Fatalf("FailRunTerminal: %v", err)
	}
	r, err := s.LoadRun(ctx, "run-ft-cancel")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusCancelled {
		t.Errorf("Status = %q, want cancelled (cancel outranks a late failure)", r.Status)
	}
}

// UpdateRunStatus preserves an existing checkpoint on EVERY transition:
// a status change never destroys the recovery point (ADR-095) —
// `iterion fork` reads a terminal parent's checkpoint, and the park
// writers that follow a running claim rely on the resume point
// surviving it.
func TestUpdateRunStatusPreservesCheckpoint(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-keep-cp")

	if err := s.SaveCheckpoint(ctx, "run-keep-cp", &Checkpoint{NodeID: "node-a"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, "run-keep-cp", RunStatusFailed, "boom"); err != nil {
		t.Fatalf("UpdateRunStatus(failed): %v", err)
	}
	r, err := s.LoadRun(ctx, "run-keep-cp")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "node-a" {
		t.Errorf("Checkpoint = %+v after failed transition, want preserved", r.Checkpoint)
	}

	if err := s.UpdateRunStatus(ctx, "run-keep-cp", RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus(finished): %v", err)
	}
	r, err = s.LoadRun(ctx, "run-keep-cp")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "node-a" {
		t.Errorf("Checkpoint = %+v after finished transition, want preserved (fork reads it)", r.Checkpoint)
	}
}

// ---------------------------------------------------------------------------
// AddWatchedIssues / RemoveWatchedIssues — persisted set arithmetic
// ---------------------------------------------------------------------------

func TestAddAndRemoveWatchedIssues(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-watch")

	got, err := s.AddWatchedIssues(ctx, "run-watch", []string{"a", "b"})
	if err != nil {
		t.Fatalf("AddWatchedIssues: %v", err)
	}
	if !sliceEq(got, []string{"a", "b"}) {
		t.Errorf("first add returned %v, want [a b]", got)
	}

	// Overlapping second add: dedup union, existing leads.
	got, err = s.AddWatchedIssues(ctx, "run-watch", []string{"b", "c"})
	if err != nil {
		t.Fatalf("AddWatchedIssues #2: %v", err)
	}
	if !sliceEq(got, []string{"a", "b", "c"}) {
		t.Errorf("second add returned %v, want [a b c]", got)
	}

	// Persistence: the union survives a reload.
	r, err := s.LoadRun(ctx, "run-watch")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if !sliceEq(r.WatchedIssueIDs, []string{"a", "b", "c"}) {
		t.Errorf("persisted WatchedIssueIDs = %v, want [a b c]", r.WatchedIssueIDs)
	}

	got, err = s.RemoveWatchedIssues(ctx, "run-watch", []string{"a"})
	if err != nil {
		t.Fatalf("RemoveWatchedIssues: %v", err)
	}
	if !sliceEq(got, []string{"b", "c"}) {
		t.Errorf("remove returned %v, want [b c]", got)
	}

	// Removing the rest empties the set; the field must drop to nil so
	// it stays omitted from the persisted record.
	got, err = s.RemoveWatchedIssues(ctx, "run-watch", []string{"b", "c"})
	if err != nil {
		t.Fatalf("RemoveWatchedIssues #2: %v", err)
	}
	if !sliceEq(got, nil) {
		t.Errorf("remove-all returned %v, want nil", got)
	}
	r, err = s.LoadRun(ctx, "run-watch")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if !sliceEq(r.WatchedIssueIDs, nil) {
		t.Errorf("persisted WatchedIssueIDs = %v, want nil after remove-all", r.WatchedIssueIDs)
	}
}

// ---------------------------------------------------------------------------
// LoadEventsRange — half-open [from,to) windowing with a limit cap
// ---------------------------------------------------------------------------

func TestLoadEventsRangeWindowAndLimit(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-range"
	// AppendEvent assigns Seq 0,1,2,3,4 in order.
	for i := 0; i < 5; i++ {
		if _, err := s.AppendEvent(ctx, runID, Event{Type: EventNodeStarted}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}

	seqs := func(evts []*Event) []int64 {
		out := make([]int64, len(evts))
		for i, e := range evts {
			out[i] = e.Seq
		}
		return out
	}

	tests := []struct {
		name     string
		from, to int64
		limit    int
		want     []int64
	}{
		{"half-open upper bound", 1, 3, 0, []int64{1, 2}},
		{"limit caps the head", 0, 0, 2, []int64{0, 1}},
		{"from past the end is empty", 10, 0, 0, nil},
		{"open upper bound from offset", 2, 0, 0, []int64{2, 3, 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := s.LoadEventsRange(ctx, runID, tt.from, tt.to, tt.limit)
			if err != nil {
				t.Fatalf("LoadEventsRange: %v", err)
			}
			got := seqs(out)
			if tt.want == nil {
				if len(got) != 0 {
					t.Errorf("got seqs %v, want empty", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got seqs %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFailRunResumableNeverOverwritesAnOperatorCancel: an operator cancel is
// terminal. An interruption and a cancel race whenever a pod is torn down
// while somebody stops the run, and a resumable failure landing last would
// auto-resume a run that was deliberately stopped.
func TestFailRunResumableNeverOverwritesAnOperatorCancel(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(context.Background(), id, RunStatusCancelled, "operator cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRunResumable(context.Background(), id, &Checkpoint{NodeID: "n1"}, "interrupted", ""); err != nil {
		t.Fatalf("FailRunResumable on a cancelled run should be a no-op, got %v", err)
	}
	run, err := s.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusCancelled {
		t.Errorf("status = %q, want cancelled — a deliberately stopped run would auto-resume", run.Status)
	}
}
