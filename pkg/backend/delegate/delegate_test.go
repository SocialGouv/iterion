package delegate

import (
	"context"
	"encoding/json"
	"errors"
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
		{"native lowercase bash unlocks workspace-write", []string{"read_file", "bash"}, "workspace-write"},
		{"edit is mutating -> workspace-write", []string{"Read", "Edit"}, "workspace-write"},
		{"native file_edit is mutating", []string{"read_file", "file_edit"}, "workspace-write"},
		{"write is mutating -> workspace-write", []string{"Write"}, "workspace-write"},
		{"native write_file is mutating", []string{"write_file"}, "workspace-write"},
		{"notebookedit is mutating -> workspace-write", []string{"NotebookEdit"}, "workspace-write"},
		{"native notebook_edit is mutating", []string{"notebook_edit"}, "workspace-write"},
		{"read-only reviewer stays read-only", []string{"Read", "Glob", "Grep"}, "read-only"},
		{"single read tool stays read-only", []string{"Grep"}, "read-only"},
		{"unknown name preserves possible writer semantics", []string{"SomeFutureTool"}, "workspace-write"},
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
	t.Run("readonly wins over conflicting full_access", func(t *testing.T) {
		got := codexSandboxForTask(Task{FullAccess: true, Readonly: true, AllowedTools: []string{"Read"}})
		if got != "read-only" {
			t.Errorf("= %q, want read-only", got)
		}
	})
	t.Run("readonly forces read-only even with mutating tools", func(t *testing.T) {
		if got := codexSandboxForTask(Task{Readonly: true, AllowedTools: []string{"bash", "write_file"}}); got != "read-only" {
			t.Errorf("readonly task = %q, want read-only", got)
		}
	})
	t.Run("default task preserves unrestricted native tool semantics", func(t *testing.T) {
		if got := codexSandboxForTask(Task{}); got != "workspace-write" {
			t.Errorf("empty task = %q, want workspace-write", got)
		}
	})
	t.Run("restricted tools stay least-privilege", func(t *testing.T) {
		if got := codexSandboxForTask(Task{AllowedTools: []string{"bash"}}); got != "workspace-write" {
			t.Errorf("Bash without full_access = %q, want workspace-write", got)
		}
		if got := codexSandboxForTask(Task{AllowedTools: []string{"read_file", "grep"}}); got != "read-only" {
			t.Errorf("read-only allowlist = %q, want read-only", got)
		}
	})
}

func TestCodexNeedsTwoPassForEveryStructuredTask(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	tests := []struct {
		name string
		task Task
		want bool
	}{
		{"no schema needs no formatter", Task{}, false},
		{"default empty tools", Task{OutputSchema: schema}, true},
		{"writer list", Task{OutputSchema: schema, AllowedTools: []string{"write_file"}}, true},
		{"readonly reader list", Task{OutputSchema: schema, Readonly: true, AllowedTools: []string{"read_file"}}, true},
		{"readonly empty tools can still read or shell", Task{OutputSchema: schema, Readonly: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexNeedsTwoPass(tt.task); got != tt.want {
				t.Fatalf("codexNeedsTwoPass(%+v) = %v, want %v", tt.task, got, tt.want)
			}
		})
	}
}

func TestCodexRejectsOuterSandboxInsteadOfEscapingToHost(t *testing.T) {
	b := &CodexBackend{Logger: testLogger()}
	result, err := b.Execute(context.Background(), Task{Sandbox: &recordingRun{}})
	if err == nil || !strings.Contains(err.Error(), "cannot run inside Iterion recording sandbox") {
		t.Fatalf("error = %v, want explicit outer-sandbox rejection", err)
	}
	if result.BackendName != BackendCodex || result.ExitCode != -1 {
		t.Fatalf("result = %+v, want codex failure metadata", result)
	}
}

func TestCodexTerminalFailure(t *testing.T) {
	empty := ""
	errText := "Error: stream disconnected before completion"
	authText := "Error: unexpected status 401 Unauthorized"
	rateText := "Error: unexpected status 429 rate limit exceeded"
	bareUsageText := "You've hit your usage limit. Try again later."
	bareQuotaText := "Quota exceeded. Check your plan and billing details."
	bareCapacityText := "Selected model is at capacity. Please retry later."
	bareDemandText := "We're currently experiencing high demand. Please retry shortly."
	valid := "done"
	discussion := "I fixed the network error and completed the task."
	failedTests := "Failed tests: TestWidget and TestParser"
	errorDiscussion := "Error: network error handling is documented in recovery.go"
	quotaDiscussion := "Quota exceeded handling is documented in the operator guide."
	tests := []struct {
		name          string
		rm            *codexsdk.ResultMessage
		stderr        string
		wantError     bool
		wantTransient bool
		wantRateLimit bool
	}{
		{"empty result with disconnected stderr", &codexsdk.ResultMessage{Result: &empty}, "stream disconnected", true, true, false},
		{"network error text cannot satisfy string schema", &codexsdk.ResultMessage{Result: &errText}, "", true, true, false},
		{"auth error fails without formatting", &codexsdk.ResultMessage{Result: &authText}, "", true, false, false},
		{"rate limit is typed", &codexsdk.ResultMessage{Result: &rateText}, "", true, false, true},
		{"bare usage limit is typed", &codexsdk.ResultMessage{Result: &bareUsageText}, "", true, false, true},
		{"bare quota notice is typed", &codexsdk.ResultMessage{Result: &bareQuotaText}, "", true, false, true},
		{"bare capacity notice is typed", &codexsdk.ResultMessage{Result: &bareCapacityText}, "", true, false, true},
		{"bare high-demand notice is typed", &codexsdk.ResultMessage{Result: &bareDemandText}, "", true, false, true},
		{"valid result ignores unrelated stderr", &codexsdk.ResultMessage{Result: &valid}, "diagnostic", false, false, false},
		{"discussion of recovered network error is valid", &codexsdk.ResultMessage{Result: &discussion}, "", false, false, false},
		{"ordinary failed-tests summary is valid", &codexsdk.ResultMessage{Result: &failedTests}, "", false, false, false},
		{"ordinary error-prefixed discussion is valid", &codexsdk.ResultMessage{Result: &errorDiscussion}, "", false, false, false},
		{"ordinary quota discussion is valid", &codexsdk.ResultMessage{Result: &quotaDiscussion}, "", false, false, false},
		{"structured result survives stale stderr", &codexsdk.ResultMessage{Result: &empty, StructuredOutput: map[string]any{"ok": true}}, "stream disconnected", false, false, false},
		{"structured result wins over error-like text", &codexsdk.ResultMessage{Result: &errText, StructuredOutput: map[string]any{"summary": "Error: stream disconnected"}}, "", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := codexTerminalFailure(tt.rm, tt.stderr)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %v", err, tt.wantError)
			}
			_, transient := err.(*ErrTransient)
			if transient != tt.wantTransient {
				t.Errorf("transient = %v, want %v (err=%v)", transient, tt.wantTransient, err)
			}
			_, rateLimited := err.(*ErrRateLimited)
			if rateLimited != tt.wantRateLimit {
				t.Errorf("rateLimited = %v, want %v (err=%v)", rateLimited, tt.wantRateLimit, err)
			}
		})
	}
}

func TestCodexFormattingStderrCapturePreservesTransientClassification(t *testing.T) {
	var capture codexStderrCapture
	capture.AppendLine("stream disconnected before completion")
	empty := ""

	err := codexTerminalFailure(&codexsdk.ResultMessage{Result: &empty}, capture.String())
	var transient *ErrTransient
	if !errors.As(err, &transient) {
		t.Fatalf("codexTerminalFailure() error = %T %v, want *ErrTransient", err, err)
	}
	if transient.Reason != "network" {
		t.Fatalf("transient reason = %q, want network", transient.Reason)
	}
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
		BackendGrok:       SystemPromptAppendToNative,
		BackendClaw:       SystemPromptAuthoredBase,
		BackendCodex:      SystemPromptStandalone,
		BackendKimi:       SystemPromptStandalone,
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
