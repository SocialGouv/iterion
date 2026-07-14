package delegate

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestIOTaskRoundTrip(t *testing.T) {
	original := Task{
		NodeID:           "node-1",
		Iteration:        3,
		SystemPrompt:     "you are helpful",
		SystemPromptMode: SystemPromptAuthoredBase,
		UserPrompt:       "do the thing",
		UserContent: []ContentBlock{
			{Type: "text", Text: "do the thing"},
			{Type: "image", MediaType: "image/png", Data: "aW1hZ2U=", Path: "assets/image.png", Name: "diagram"},
		},
		AllowedTools:          []string{"Bash", "Read"},
		Readonly:              true,
		OutputSchema:          json.RawMessage(`{"type":"object"}`),
		Model:                 "anthropic/claude-sonnet-4-6",
		HasTools:              true,
		ToolMaxSteps:          10,
		MaxTokens:             8192,
		WorkDir:               "/workspace",
		BaseDir:               "/workspace",
		RepoRoot:              "/repo",
		ReasoningEffort:       "high",
		Ultracode:             true,
		SecretsHygiene:        true,
		SecretFiles:           []SecretFileHint{{Name: "aws", Path: "/run/secrets/aws", Env: "AWS_SHARED_CREDENTIALS_FILE"}},
		CursorFragments:       []string{"**style:** prefer small changes", "**tests:** verify behavior"},
		PresetFragment:        "Focus on maintainability.",
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
	if got.Iteration != original.Iteration {
		t.Errorf("Iteration = %d, want %d", got.Iteration, original.Iteration)
	}
	if got.SystemPromptMode != original.SystemPromptMode {
		t.Errorf("SystemPromptMode = %d, want %d", got.SystemPromptMode, original.SystemPromptMode)
	}
	if !reflect.DeepEqual(got.UserContent, original.UserContent) {
		t.Errorf("UserContent = %#v, want %#v", got.UserContent, original.UserContent)
	}
	if got.BaseDir != original.BaseDir {
		t.Errorf("BaseDir = %q, want %q", got.BaseDir, original.BaseDir)
	}
	if got.RepoRoot != original.RepoRoot {
		t.Errorf("RepoRoot = %q, want %q", got.RepoRoot, original.RepoRoot)
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
	if got.Ultracode != original.Ultracode {
		t.Errorf("Ultracode = %v, want %v", got.Ultracode, original.Ultracode)
	}
	if got.SecretsHygiene != original.SecretsHygiene {
		t.Errorf("SecretsHygiene = %v, want %v", got.SecretsHygiene, original.SecretsHygiene)
	}
	if !reflect.DeepEqual(got.SecretFiles, original.SecretFiles) {
		t.Errorf("SecretFiles = %#v, want %#v", got.SecretFiles, original.SecretFiles)
	}
	if !reflect.DeepEqual(got.CursorFragments, original.CursorFragments) {
		t.Errorf("CursorFragments = %#v, want %#v", got.CursorFragments, original.CursorFragments)
	}
	if got.PresetFragment != original.PresetFragment {
		t.Errorf("PresetFragment = %q, want %q", got.PresetFragment, original.PresetFragment)
	}
	if got.HasTools != original.HasTools {
		t.Errorf("HasTools = %v", got.HasTools)
	}
	if got.Readonly != original.Readonly {
		t.Errorf("Readonly = %v, want %v", got.Readonly, original.Readonly)
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
		Output:      map[string]any{"text": "hello"},
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

// TestIOTaskCarriesCapabilitiesAndBoardWiring guards the data-integrity
// fix for the capability + board-wiring fields that were previously
// dropped at the IPC boundary, so a capability-gated agent running
// inside a sandbox lost its board access and provider routing.
func TestIOTaskCarriesCapabilitiesAndBoardWiring(t *testing.T) {
	original := Task{
		NodeID:             "agent-1",
		Capabilities:       []string{"board.create", "board.read"},
		StoreDir:           "/tmp/iterion-store",
		BoardHTTPEndpoint:  "http://host.docker.internal:7000/api/v1/mcp/board",
		BoardRunToken:      "deadbeef",
		ProviderHint:       "anthropic",
		SessionFingerprint: "anthropic-direct",
	}
	wire := ToIOTask(original)
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded IOTask
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := FromIOTask(decoded)

	if len(got.Capabilities) != 2 ||
		got.Capabilities[0] != "board.create" ||
		got.Capabilities[1] != "board.read" {
		t.Errorf("Capabilities lost: %v", got.Capabilities)
	}
	if got.StoreDir != original.StoreDir {
		t.Errorf("StoreDir = %q", got.StoreDir)
	}
	if got.BoardHTTPEndpoint != original.BoardHTTPEndpoint {
		t.Errorf("BoardHTTPEndpoint = %q", got.BoardHTTPEndpoint)
	}
	if got.BoardRunToken != original.BoardRunToken {
		t.Errorf("BoardRunToken = %q", got.BoardRunToken)
	}
	if got.ProviderHint != original.ProviderHint {
		t.Errorf("ProviderHint = %q", got.ProviderHint)
	}
	if got.SessionFingerprint != original.SessionFingerprint {
		t.Errorf("SessionFingerprint = %q", got.SessionFingerprint)
	}
}
