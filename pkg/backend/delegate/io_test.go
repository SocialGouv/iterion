package delegate

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIOTaskRoundTrip(t *testing.T) {
	original := Task{
		NodeID:                "node-1",
		SystemPrompt:          "you are helpful",
		UserPrompt:            "do the thing",
		AllowedTools:          []string{"Bash", "Read"},
		OutputSchema:          json.RawMessage(`{"type":"object"}`),
		Model:                 "anthropic/claude-sonnet-4-6",
		HasTools:              true,
		ToolMaxSteps:          10,
		MaxTokens:             8192,
		WorkDir:               "/workspace",
		BaseDir:               "/workspace",
		ReasoningEffort:       "high",
		CompactThresholdRatio: 0.85,
		CompactPreserveRecent: 4,
		SessionID:             "sess-abc",
		ForkSession:           true,
		InteractionEnabled:    true,
		ResumeAnswer:          "yes please",
	}

	ioTask := ToIOTask(original)
	data, err := json.Marshal(ioTask)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded IOTask
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := FromIOTask(decoded)

	if got.NodeID != original.NodeID {
		t.Errorf("NodeID = %q, want %q", got.NodeID, original.NodeID)
	}
	if got.BaseDir != original.BaseDir {
		t.Errorf("BaseDir = %q, want %q", got.BaseDir, original.BaseDir)
	}
	if got.WorkDir != original.WorkDir {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, original.WorkDir)
	}
	if got.Model != original.Model {
		t.Errorf("Model = %q, want %q", got.Model, original.Model)
	}
	if got.ReasoningEffort != original.ReasoningEffort {
		t.Errorf("ReasoningEffort = %q, want %q", got.ReasoningEffort, original.ReasoningEffort)
	}
	if got.MaxTokens != original.MaxTokens {
		t.Errorf("MaxTokens = %d, want %d", got.MaxTokens, original.MaxTokens)
	}
	if got.HasTools != original.HasTools {
		t.Errorf("HasTools = %v", got.HasTools)
	}
	if got.SessionID != original.SessionID {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.ForkSession != original.ForkSession {
		t.Errorf("ForkSession = %v", got.ForkSession)
	}
	if got.InteractionEnabled != original.InteractionEnabled {
		t.Errorf("InteractionEnabled = %v", got.InteractionEnabled)
	}
	if got.ResumeAnswer != original.ResumeAnswer {
		t.Errorf("ResumeAnswer = %q", got.ResumeAnswer)
	}
	if got.Sandbox != nil {
		t.Errorf("Sandbox should be nil after round-trip; runner is inside the sandbox")
	}
	if got.ToolDefs != nil {
		t.Errorf("ToolDefs should be nil after round-trip; runner registers them locally")
	}
}

func TestIOResultRoundTrip(t *testing.T) {
	original := Result{
		Output:      map[string]interface{}{"text": "hello"},
		Tokens:      1234,
		Duration:    5 * time.Second,
		ExitCode:    0,
		BackendName: "claw",
		SessionID:   "sess-xyz",
	}
	ioRes := ToIOResult(original)
	data, _ := json.Marshal(ioRes)
	var decoded IOResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := FromIOResult(decoded)

	if got.Tokens != original.Tokens {
		t.Errorf("Tokens = %d, want %d", got.Tokens, original.Tokens)
	}
	if got.Duration != original.Duration {
		t.Errorf("Duration = %v, want %v (millisecond precision expected)", got.Duration, original.Duration)
	}
	if got.SessionID != original.SessionID {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if txt, _ := got.Output["text"].(string); txt != "hello" {
		t.Errorf("Output[text] = %q, want hello", txt)
	}
}

func TestIOResultDurationMillisecondPrecision(t *testing.T) {
	// Sub-millisecond durations round to zero on the wire — document
	// the precision boundary so callers know why a 500µs run reports
	// Duration=0 after round-trip.
	original := Result{Duration: 500 * time.Microsecond}
	got := FromIOResult(ToIOResult(original))
	if got.Duration != 0 {
		t.Errorf("expected sub-ms duration to round to 0, got %v", got.Duration)
	}
}
