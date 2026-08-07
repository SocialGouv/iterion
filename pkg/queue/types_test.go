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
	// v=6 (2026-08-05) added AutoMemory so a launch-time `--auto-memory`
	// decision reaches the runner instead of being replaced by the workflow's
	// own value — a drop there turns a hermetic `off` into a run that reads
	// and writes shared memory. v=7 (2026-08-07) added Fallback so a
	// launch-time `--fallback` route (ADR-087) reaches the runner; dropped,
	// the pod runs with no alternative and loses the run to the very forfait
	// wall the operator set the route to survive.
	if SchemaVersion != 7 {
		t.Errorf("SchemaVersion = %d, want 7 (bump intentionally)", SchemaVersion)
	}
}

// TestFallbackRouteSurvivesRoundTrip: the run-level fallback route only
// exists on the wire — it is not persisted on the run document. If it
// does not survive encode/decode, a cloud launch runs with no
// alternative route and dies on the very forfait wall the operator set
// it to survive, in silence. Absent = the caller expressed none, which
// must decode as nil rather than an empty route (an empty route would
// re-issue the identical call that just failed).
func TestFallbackRouteSurvivesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *FallbackRoute
	}{
		{"none", nil},
		{"backend and model", &FallbackRoute{Backend: "claw", Model: "openai/gpt-5.5"}},
		{"with provider hint", &FallbackRoute{Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(&RunMessage{V: SchemaVersion, Fallback: tc.in})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got RunMessage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tc.in == nil {
				if got.Fallback != nil {
					t.Fatalf("absent route decoded as %+v, want nil", got.Fallback)
				}
				if bytes.Contains(raw, []byte("fallback")) {
					t.Errorf("no route set but the key was emitted: %s", raw)
				}
				return
			}
			if got.Fallback == nil || *got.Fallback != *tc.in {
				t.Errorf("round-trip = %+v, want %+v", got.Fallback, tc.in)
			}
		})
	}
}
