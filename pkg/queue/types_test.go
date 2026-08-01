package queue

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunMessage_RoundTripJSON(t *testing.T) {
	src := RunMessage{
		V:              SchemaVersion,
		RunID:          "run_abc",
		WorkflowName:   "demo",
		WorkflowHash:   "sha256:deadbeef",
		IRCompiled:     json.RawMessage(`{"nodes":[]}`),
		Vars:           map[string]any{"k": "v"},
		BotID:          "review-pr",
		BackendConfig:  BackendConfig{Default: BackendClaw},
		Trace:          TraceContext{TraceID: "0123456789abcdef0123456789abcdef"},
		PublishedAtRFC: "2026-05-05T11:00:00Z",
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var dst RunMessage
	if err := json.Unmarshal(b, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.RunID != "run_abc" {
		t.Errorf("RunID: got %q", dst.RunID)
	}
	if dst.BackendConfig.Default != BackendClaw {
		t.Errorf("BackendConfig.Default: got %q", dst.BackendConfig.Default)
	}
	if dst.BotID != "review-pr" {
		t.Errorf("BotID: got %q", dst.BotID)
	}
	if string(dst.IRCompiled) != `{"nodes":[]}` {
		t.Errorf("IRCompiled lost on round-trip: %q", dst.IRCompiled)
	}
}

func TestRunMessage_ValidateIRRefBackend(t *testing.T) {
	good := &RunMessage{
		V:            SchemaVersion,
		RunID:        "run_1",
		WorkflowName: "demo",
		IRRef:        &IRRef{StorageKey: "ir/run_1.json", Backend: IRBackendS3},
	}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	bad := &RunMessage{
		V:            SchemaVersion,
		RunID:        "run_1",
		WorkflowName: "demo",
		IRRef:        &IRRef{StorageKey: "ir/run_1.json", Backend: "filesystem"},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error on unknown IRRef.Backend")
	}
}

func TestRunMessage_ValidateHappyPath(t *testing.T) {
	m := &RunMessage{
		V:            SchemaVersion,
		RunID:        "run_1",
		WorkflowName: "demo",
		IRCompiled:   json.RawMessage(`{}`),
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestRunMessage_ValidateSchemaMismatch(t *testing.T) {
	m := &RunMessage{
		V:            999,
		RunID:        "run_1",
		WorkflowName: "demo",
		IRCompiled:   json.RawMessage(`{}`),
	}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error on schema version mismatch")
	}
	if !strings.Contains(err.Error(), "schema version") {
		t.Errorf("error should mention schema version: %v", err)
	}
}

func TestRunMessage_ValidateRequiresRunID(t *testing.T) {
	m := &RunMessage{
		V:            SchemaVersion,
		WorkflowName: "demo",
		IRCompiled:   json.RawMessage(`{}`),
	}
	if err := m.Validate(); err == nil {
		t.Fatal("expected error on empty RunID")
	}
}

func TestRunMessage_ValidateRequiresWorkflowName(t *testing.T) {
	m := &RunMessage{
		V:          SchemaVersion,
		RunID:      "run_1",
		IRCompiled: json.RawMessage(`{}`),
	}
	if err := m.Validate(); err == nil {
		t.Fatal("expected error on empty WorkflowName")
	}
}

func TestRunMessage_ValidateExactlyOneIR_Both(t *testing.T) {
	m := &RunMessage{
		V:            SchemaVersion,
		RunID:        "run_1",
		WorkflowName: "demo",
		IRCompiled:   json.RawMessage(`{}`),
		IRRef:        &IRRef{StorageKey: "ir/run_1.json", Backend: IRBackendS3},
	}
	if err := m.Validate(); err == nil {
		t.Fatal("expected error when both IRCompiled and IRRef set")
	}
}

func TestRunMessage_ValidateExactlyOneIR_Neither(t *testing.T) {
	m := &RunMessage{
		V:            SchemaVersion,
		RunID:        "run_1",
		WorkflowName: "demo",
	}
	if err := m.Validate(); err == nil {
		t.Fatal("expected error when neither IRCompiled nor IRRef set")
	}
}

func TestRunMessage_ValidateIRRefStorageKeyRequired(t *testing.T) {
	m := &RunMessage{
		V:            SchemaVersion,
		RunID:        "run_1",
		WorkflowName: "demo",
		IRRef:        &IRRef{Backend: IRBackendS3}, // missing StorageKey
	}
	// IRRef without StorageKey is treated as unset → "neither set"
	// validation should fire.
	if err := m.Validate(); err == nil {
		t.Fatal("expected error when IRRef has no StorageKey")
	}
}

func TestRunMessage_ValidateNilReceiver(t *testing.T) {
	var m *RunMessage
	if err := m.Validate(); err == nil {
		t.Fatal("expected error on nil receiver")
	}
}

func TestSchemaVersionConstant(t *testing.T) {
	// Pinning the constant is a deliberate guard: bumping it should be a
	// conscious commit, not an accident. v=4 (2026-07-11) added Budget
	// so launch-time budget overrides reach the cloud runner. v=5
	// (2026-07-20) added Contributions so enabled-plugin skills and DSL
	// `skills:` library references reach the runner pod's empty iterion home.
	// v=6 (2026-08-01) added ModelOverrides so a launch-time model/backend/
	// effort choice reaches the pod instead of being dropped at the queue.
	if SchemaVersion != 6 {
		t.Errorf("SchemaVersion = %d, want 6 (bump intentionally)", SchemaVersion)
	}
}

// TestModelOverridesSurviveTheWire pins the field that carries an operator's
// model choice to the runner. A silent drop here is invisible — the run record
// still shows the chosen model while the pod executes the DSL default — so the
// round-trip is asserted rather than assumed.
func TestModelOverridesSurviveTheWire(t *testing.T) {
	in := &RunMessage{
		V:            SchemaVersion,
		RunID:        "r1",
		WorkflowName: "wf",
		IRCompiled:   json.RawMessage(`{}`),
		ModelOverrides: []ModelOverride{
			{Selector: "reviewer_*", Model: "anthropic/claude-opus-5", Effort: "xhigh"},
			{Selector: "agent", Backend: "claw", Provider: "anthropic"},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RunMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(out.ModelOverrides) != 2 {
		t.Fatalf("ModelOverrides len = %d, want 2 (dropped on the wire)", len(out.ModelOverrides))
	}
	if got := out.ModelOverrides[0]; got.Selector != "reviewer_*" || got.Model != "anthropic/claude-opus-5" || got.Effort != "xhigh" {
		t.Errorf("first override round-tripped as %+v", got)
	}
	if got := out.ModelOverrides[1]; got.Backend != "claw" || got.Provider != "anthropic" {
		t.Errorf("second override round-tripped as %+v", got)
	}
}

// TestModelOverridesOmittedWhenEmpty keeps the common launch (no overrides)
// off the wire entirely rather than publishing an empty array.
func TestModelOverridesOmittedWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(&RunMessage{V: SchemaVersion, RunID: "r1", WorkflowName: "wf"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("model_overrides")) {
		t.Errorf("empty overrides should be omitted, got %s", raw)
	}
}
