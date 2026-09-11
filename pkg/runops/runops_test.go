package runops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRunGetReturnsBoundedFailureDiagnosis(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(ctx, "run-failed", "hierarchical_architect", map[string]any{"secret": "must-not-leak"})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.Error = "candidate artifact missing"
	run.Checkpoint = &store.Checkpoint{NodeID: "seal_experience"}
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.AppendEvent(ctx, run.ID, store.Event{
		Type: store.EventRunFailed, NodeID: "seal_experience",
		Data: map[string]any{"code": "EXECUTION_FAILED", "error": "candidate artifact missing"},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := Call(ctx, rs, NewCapabilities(CapRunsRead), "run_get", json.RawMessage(`{"run_id":"run-failed"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != string(store.RunStatusFailedResumable) || got["failing_node"] != "seal_experience" || got["error_code"] != "EXECUTION_FAILED" {
		t.Fatalf("unexpected diagnosis: %s", raw)
	}
	if _, leaked := got["inputs"]; leaked {
		t.Fatalf("run_get leaked inputs: %s", raw)
	}
	if _, leaked := got["work_dir"]; leaked {
		t.Fatalf("run_get leaked work_dir: %s", raw)
	}
}

func TestRunOpsRequireCapability(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), rs, Capabilities{}, "runs_list", nil); err == nil {
		t.Fatal("runs_list succeeded without runs.read")
	}
	if got := ToolsFor(Capabilities{}); len(got) != 0 {
		t.Fatalf("tools without capability = %v", got)
	}
}

func TestRunGetDoesNotExposeStorePathOnMissingRun(t *testing.T) {
	root := t.TempDir()
	rs, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Call(context.Background(), rs, NewCapabilities(CapRunsRead), "run_get", json.RawMessage(`{"run_id":"native:not-a-run"}`))
	if err == nil {
		t.Fatal("run_get unexpectedly found a task id as a run")
	}
	if err.Error() != "run not found" {
		t.Fatalf("public error = %q, want bounded not-found", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "run.json") {
		t.Fatalf("public error leaked store path: %q", err)
	}
}

func TestRunEventsProjectsDiagnosticsWithoutPayloads(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.CreateRun(ctx, "run-events", "wf", map[string]any{"secret": "run-input"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.AppendEvent(ctx, "run-events", store.Event{
		Type:   store.EventToolCalled,
		NodeID: "seal_experience",
		Data: map[string]any{
			"tool": "shell:seal_experience", "duration_ms": 12,
			"input": "operator-secret", "output": map[string]any{"secret": "tool-secret"},
			"prompt": "model-secret", "error": "candidate missing",
		},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := Call(ctx, rs, NewCapabilities(CapRunsRead), "run_events", json.RawMessage(`{"run_id":"run-events"}`))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"operator-secret", "tool-secret", "model-secret", `"input"`, `"output"`, `"prompt"`} {
		if contains := strings.Contains(text, forbidden); contains {
			t.Fatalf("run_events leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "candidate missing") || !strings.Contains(text, "shell:seal_experience") {
		t.Fatalf("diagnostic fields missing: %s", text)
	}
}
