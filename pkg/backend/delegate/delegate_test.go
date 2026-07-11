package delegate

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	r.Register("test_backend", &mockBackend{})

	_, err := r.Resolve("test_backend")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = r.Resolve("unknown")
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestDefaultRegistry(t *testing.T) {
	r := DefaultRegistry(nil)

	_, err := r.Resolve(BackendClaudeCode)
	if err != nil {
		t.Fatalf("claude_code not found: %v", err)
	}

	_, err = r.Resolve(BackendCodex)
	if err != nil {
		t.Fatalf("codex not found: %v", err)
	}
}

func TestParseSDKOutput_StructuredOutput(t *testing.T) {
	structured := map[string]any{"approved": true, "summary": "looks good"}
	output, rawLen, fallback := parseSDKOutput(nil, structured, nil)
	if output["approved"] != true {
		t.Errorf("expected approved=true, got %v", output["approved"])
	}
	if output["summary"] != "looks good" {
		t.Errorf("expected summary='looks good', got %v", output["summary"])
	}
	if rawLen != 0 {
		t.Errorf("expected rawLen=0 for structured output, got %d", rawLen)
	}
	if fallback {
		t.Error("expected no fallback for structured output")
	}
}

func TestParseSDKOutput_ResultTextJSON(t *testing.T) {
	text := `{"approved": true}`
	output, rawLen, fallback := parseSDKOutput(&text, nil, nil)
	if output["approved"] != true {
		t.Errorf("expected approved=true, got %v", output)
	}
	if rawLen != len(text) {
		t.Errorf("expected rawLen=%d, got %d", len(text), rawLen)
	}
	if fallback {
		t.Error("expected no fallback for JSON text")
	}
}

func TestParseSDKOutput_ResultTextPlain(t *testing.T) {
	text := "This is plain text output."
	output, rawLen, fallback := parseSDKOutput(&text, nil, nil)
	if output["text"] != text {
		t.Errorf("expected text=%q, got %v", text, output["text"])
	}
	if rawLen != len(text) {
		t.Errorf("expected rawLen=%d, got %d", len(text), rawLen)
	}
	if fallback {
		t.Error("expected no fallback when no schema")
	}
}

func TestParseSDKOutput_ResultTextPlainWithSchema(t *testing.T) {
	text := "This is plain text output."
	schema := json.RawMessage(`{"type":"object"}`)
	output, _, fallback := parseSDKOutput(&text, nil, schema)
	if output["text"] != text {
		t.Errorf("expected text=%q, got %v", text, output["text"])
	}
	if !fallback {
		t.Error("expected fallback when schema is set but output is plain text")
	}
}

func TestParseSDKOutput_MarkdownJSON(t *testing.T) {
	text := "Here is the result:\n```json\n{\"verdict\": \"pass\"}\n```"
	output, _, fallback := parseSDKOutput(&text, nil, nil)
	if output["verdict"] != "pass" {
		t.Errorf("expected verdict=pass, got %v", output)
	}
	if fallback {
		t.Error("expected no fallback for markdown JSON")
	}
}

func TestParseSDKOutput_Empty(t *testing.T) {
	output, rawLen, fallback := parseSDKOutput(nil, nil, nil)
	if len(output) != 0 {
		t.Errorf("expected empty output, got %v", output)
	}
	if rawLen != 0 {
		t.Errorf("expected rawLen=0, got %d", rawLen)
	}
	if fallback {
		t.Error("expected no fallback for empty output")
	}
}

func TestParseSDKOutput_StructuredOutputNonMap(t *testing.T) {
	// Structured output that is not a map but can be marshaled to one.
	type result struct {
		Approved bool   `json:"approved"`
		Summary  string `json:"summary"`
	}
	structured := result{Approved: true, Summary: "ok"}
	output, _, fallback := parseSDKOutput(nil, structured, nil)
	if output["approved"] != true {
		t.Errorf("expected approved=true, got %v", output["approved"])
	}
	if fallback {
		t.Error("expected no fallback for struct output")
	}
}

func TestExtractJSONFromMarkdown(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"no fences", "plain text", ""},
		{"json block", "text\n```json\n{\"a\":1}\n```\nmore", `{"a":1}`},
		{"bare block", "```\n{\"b\":2}\n```", `{"b":2}`},
		{"multiple blocks", "```\n{\"first\":1}\n```\n```\n{\"second\":2}\n```", `{"second":2}`},
		{"non-json block", "```\nnot json\n```", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSONFromMarkdown(tt.input)
			if got != tt.expect {
				t.Errorf("extractJSONFromMarkdown(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestValidateWorkDir(t *testing.T) {
	base := t.TempDir()
	sub := base + "/sub"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Same dir should pass.
	if err := validateWorkDir(base, base); err != nil {
		t.Errorf("expected nil for same dir, got %v", err)
	}
	// Subdir should pass.
	if err := validateWorkDir(sub, base); err != nil {
		t.Errorf("expected nil for subdir, got %v", err)
	}
	// Outside should fail.
	if err := validateWorkDir("/var", base); err == nil {
		t.Error("expected error for outside dir")
	}
	// Empty baseDir should always pass.
	if err := validateWorkDir("/anywhere", ""); err != nil {
		t.Errorf("expected nil for empty baseDir, got %v", err)
	}
}

func TestMapReasoningEffort(t *testing.T) {
	tests := []struct {
		input  string
		expect codexsdk.Effort
	}{
		{"low", codexsdk.EffortLow},
		{"medium", codexsdk.EffortMedium},
		{"high", codexsdk.EffortHigh},
		{"xhigh", codexsdk.EffortHigh},
		{"max", codexsdk.EffortMax},
		{"unknown", codexsdk.EffortMedium},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapReasoningEffort(tt.input)
			if got != tt.expect {
				t.Errorf("mapReasoningEffort(%q) = %v, want %v", tt.input, got, tt.expect)
			}
		})
	}
}

func TestCodexSandboxForAllowedTools(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		want    string
	}{
		{"empty allowlist defaults to read-only (fail-safe)", nil, "read-only"},
		{"bash unlocks workspace-write, not full-access", []string{"Read", "Bash"}, "workspace-write"},
		{"edit is mutating -> workspace-write", []string{"Read", "Edit"}, "workspace-write"},
		{"write is mutating -> workspace-write", []string{"Write"}, "workspace-write"},
		{"notebookedit is mutating -> workspace-write", []string{"NotebookEdit"}, "workspace-write"},
		{"read-only reviewer stays read-only", []string{"Read", "Glob", "Grep"}, "read-only"},
		{"single read tool stays read-only", []string{"Grep"}, "read-only"},
		{"unknown name falls through to read-only", []string{"SomeFutureTool"}, "read-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexSandboxForAllowedTools(tt.allowed)
			if got != tt.want {
				t.Errorf("codexSandboxForAllowedTools(%v) = %q, want %q", tt.allowed, got, tt.want)
			}
		})
	}
}

func TestCodexSandboxForTask(t *testing.T) {
	t.Run("full_access opts into danger-full-access", func(t *testing.T) {
		if got := codexSandboxForTask(Task{FullAccess: true}); got != "danger-full-access" {
			t.Errorf("FullAccess=true = %q, want danger-full-access", got)
		}
	})
	t.Run("full_access wins over the tool-derived mode", func(t *testing.T) {
		got := codexSandboxForTask(Task{FullAccess: true, AllowedTools: []string{"Read"}})
		if got != "danger-full-access" {
			t.Errorf("= %q, want danger-full-access", got)
		}
	})
	t.Run("without opt-in, stays least-privilege", func(t *testing.T) {
		if got := codexSandboxForTask(Task{AllowedTools: []string{"Bash"}}); got != "workspace-write" {
			t.Errorf("Bash without full_access = %q, want workspace-write", got)
		}
		if got := codexSandboxForTask(Task{}); got != "read-only" {
			t.Errorf("empty task = %q, want read-only", got)
		}
	})
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"under limit", "hello", 10, "hello"},
		{"at limit", "hello", 5, "hello"},
		{"over limit ASCII", "hello world", 5, "hello..."},
		// "héllo" is 6 bytes (h, 0xc3, 0xa9, l, l, o). Truncating at 2 bytes
		// would split the é; truncate must back up to a rune boundary.
		{"over limit backs up off a rune boundary", "héllo", 2, "h..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncate(tt.s, tt.max); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}
}

func TestFormattingPassUsed_MockBackend(t *testing.T) {
	// Verify that FormattingPassUsed is correctly propagated through Result.
	r := NewRegistry()
	r.Register("mock", &mockBackend{
		response: Result{
			Output:             map[string]any{"approved": true},
			FormattingPassUsed: true,
			BackendName:        "mock",
		},
	})

	backend, err := r.Resolve("mock")
	if err != nil {
		t.Fatal(err)
	}

	result, err := backend.Execute(context.Background(), Task{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.FormattingPassUsed {
		t.Error("expected FormattingPassUsed=true")
	}
	if result.ParseFallback {
		t.Error("expected ParseFallback=false when formatting pass was used")
	}
}

func TestParseSDKOutput_NoFallbackWhenFormattingPassHandles(t *testing.T) {
	// When a two-pass backend returns structured output from Pass 2,
	// parseSDKOutput should return fallback=false since the SDK provides
	// native structured output.
	structured := map[string]any{"verdict": "pass", "score": 9.5}
	schema := json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"},"score":{"type":"number"}}}`)

	output, _, fallback := parseSDKOutput(nil, structured, schema)
	if fallback {
		t.Error("expected no fallback when SDK returns structured output")
	}
	if output["verdict"] != "pass" {
		t.Errorf("expected verdict=pass, got %v", output["verdict"])
	}
}

// mockBackend implements Backend for testing.
type mockBackend struct {
	response Result
	err      error
}

func (m *mockBackend) Execute(_ context.Context, _ Task) (Result, error) {
	return m.response, m.err
}

func TestSystemPromptModeForBackend(t *testing.T) {
	cases := map[string]SystemPromptMode{
		BackendClaudeCode: SystemPromptAppendToNative,
		BackendClaw:       SystemPromptAuthoredBase,
		BackendCodex:      SystemPromptStandalone,
		"unknown":         SystemPromptStandalone,
	}
	for backend, want := range cases {
		if got := SystemPromptModeForBackend(backend); got != want {
			t.Errorf("SystemPromptModeForBackend(%q) = %d, want %d", backend, got, want)
		}
	}
	// The zero value must be Standalone so a Task that never sets the mode
	// keeps legacy behaviour.
	if SystemPromptStandalone != 0 {
		t.Errorf("SystemPromptStandalone must be the zero value, got %d", SystemPromptStandalone)
	}
}

func TestBuildSystemPrompt_Modes(t *testing.T) {
	const author = "You are a code reviewer. Emit a JSON verdict."

	// Standalone (codex/legacy) and AppendToNative (claude_code) both emit the
	// author text verbatim — for claude_code the native prompt is the base and
	// iterion routes this to --append-system-prompt, so it must NOT carry the
	// iterion-authored agentic base.
	for _, mode := range []SystemPromptMode{SystemPromptStandalone, SystemPromptAppendToNative} {
		got := Task{SystemPrompt: author, SystemPromptMode: mode}.BuildSystemPrompt()
		if got != author {
			t.Errorf("mode %d: got %q, want author verbatim %q", mode, got, author)
		}
		if strings.Contains(got, agenticOperatingPosture) {
			t.Errorf("mode %d: must NOT contain the iterion agentic base", mode)
		}
	}

	// AuthoredBase (claw) prepends the agentic posture before the author text,
	// because claw has no native system prompt of its own.
	got := Task{SystemPrompt: author, SystemPromptMode: SystemPromptAuthoredBase}.BuildSystemPrompt()
	if !strings.HasPrefix(got, agenticOperatingPosture) {
		t.Errorf("AuthoredBase: must start with the agentic base, got %q", got[:min(80, len(got))])
	}
	if !strings.Contains(got, author) {
		t.Error("AuthoredBase: must still contain the author text")
	}
	if strings.Index(got, agenticOperatingPosture) >= strings.Index(got, author) {
		t.Error("AuthoredBase: agentic base must come before the author text")
	}

	// Suffixes (interaction, ultracode, calibration) are appended after the
	// base in every mode — verify on AuthoredBase that they trail the author.
	full := Task{
		SystemPrompt:       author,
		SystemPromptMode:   SystemPromptAuthoredBase,
		InteractionEnabled: true,
		Ultracode:          true,
		CursorFragments:    []string{"rigor: be exacting"},
	}.BuildSystemPrompt()
	authorAt := strings.Index(full, author)
	for _, suffix := range []string{interactionSystemInstruction, ultracodeOrchestrationInstruction, "## Calibration", "rigor: be exacting"} {
		at := strings.Index(full, suffix)
		if at < 0 {
			t.Errorf("AuthoredBase+suffixes: missing %q", suffix)
			continue
		}
		if at < authorAt {
			t.Errorf("AuthoredBase+suffixes: %q must come after the author text", suffix)
		}
	}
}
