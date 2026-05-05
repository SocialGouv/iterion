package queue

import (
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
		Vars:           map[string]interface{}{"k": "v"},
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
	// conscious commit, not an accident.
	if SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1 (bump intentionally)", SchemaVersion)
	}
}
