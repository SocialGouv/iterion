package delegate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestGrokMapModel(t *testing.T) {
	cases := map[string]string{
		"grok-4.5":         "grok-4.5",
		"grok-4.5-build":   "grok-4.5-build",
		"xai/grok-4.5":     "grok-4.5",
		"grok/grok-3-mini": "grok-3-mini",
		"  xai/grok-3  ":   "grok-3",
		"":                 "",
		// Unknown prefix kept intact (CLI may understand provider-qualified ids).
		"openai/gpt-5": "openai/gpt-5",
	}
	for in, want := range cases {
		if got := grokMapModel(in); got != want {
			t.Errorf("grokMapModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGrokMapEffort(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"high", []string{"--reasoning-effort", "high"}},
		{"ultracode", []string{"--reasoning-effort", "high"}},
		{"  Medium  ", []string{"--reasoning-effort", "medium"}},
	}
	for _, tc := range cases {
		got := grokMapEffort(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("grokMapEffort(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGrokBuildArgs(t *testing.T) {
	b := &CLIAgentBackend{Protocol: grokProtocol, Logger: testLogger()}
	task := Task{
		SystemPrompt:     "be a careful reviewer",
		SystemPromptMode: SystemPromptAppendToNative,
		UserPrompt:       "review this PR",
		Model:            "xai/grok-4.5",
		ReasoningEffort:  "high",
	}
	system := task.BuildSystemPrompt()
	args, stdin := b.buildArgs(grokProtocol, task, task.UserPrompt, system)

	// -p prompt, --output-format json, -m stripped model, --rules system,
	// --reasoning-effort high, then ExtraArgs (permission + always-approve).
	want := []string{
		"-p", "review this PR",
		"--output-format", "json",
		"-m", "grok-4.5",
		"--rules", "be a careful reviewer",
		"--reasoning-effort", "high",
		"--permission-mode", "bypassPermissions",
		"--always-approve",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v\nwant %#v", args, want)
	}
	if stdin != "" {
		t.Fatalf("stdin = %q, want empty (prompt via -p)", stdin)
	}
	// AppendToNative must NOT fold the agentic base into --rules.
	if system != "be a careful reviewer" {
		t.Fatalf("system = %q, want author text only", system)
	}
}

func TestParseGrokOutput_JSONEnvelope(t *testing.T) {
	stdout := `{
  "text": "all good",
  "sessionId": "sess-abc",
  "usage": {"input_tokens": 100, "output_tokens": 20, "total_tokens": 120}
}`
	text, sid, tokens := parseGrokOutput(stdout)
	if text != "all good" {
		t.Errorf("text = %q", text)
	}
	if sid != "sess-abc" {
		t.Errorf("sessionID = %q", sid)
	}
	if tokens != 120 {
		t.Errorf("tokens = %d, want 120", tokens)
	}
}

func TestParseGrokOutput_StreamingJSON(t *testing.T) {
	stdout := `{"type":"thought","data":"hmm"}
{"type":"text","data":"Hi"}
{"type":"text","data":" there"}
{"type":"end","stopReason":"EndTurn","sessionId":"sess-stream","usage":{"input_tokens":10,"output_tokens":5}}`
	text, sid, tokens := parseGrokOutput(stdout)
	if text != "Hi there" {
		t.Errorf("text = %q, want %q", text, "Hi there")
	}
	if sid != "sess-stream" {
		t.Errorf("sessionID = %q", sid)
	}
	if tokens != 15 {
		t.Errorf("tokens = %d, want 15", tokens)
	}
}

func TestParseGrokOutput_Empty(t *testing.T) {
	text, sid, tokens := parseGrokOutput("")
	if text != "" || sid != "" || tokens != 0 {
		t.Fatalf("empty input: text=%q sid=%q tokens=%d", text, sid, tokens)
	}
}

func TestParseGrokOutput_FallbackRaw(t *testing.T) {
	// Non-JSON / unrecognised stream → hand back raw so schema fallback can try.
	raw := "just plain text from an older build"
	text, _, _ := parseGrokOutput(raw)
	if text != raw {
		t.Errorf("text = %q, want raw fallback", text)
	}
}

func TestGrokExecute_FakeBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script fixture")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "grok")
	// Echo a json envelope and exit 0. Ignore argv shape beyond existence.
	script := "#!/bin/sh\nprintf '%s\\n' '{\"text\":\"{\\\"ok\\\":true}\",\"sessionId\":\"s1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b := NewGrokBackend(testLogger(), bin)
	res, err := b.Execute(context.Background(), Task{
		NodeID:           "n1",
		UserPrompt:       "hi",
		SystemPrompt:     "task",
		SystemPromptMode: SystemPromptAppendToNative,
		Model:            "grok-4.5",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.BackendName != BackendGrok {
		t.Errorf("BackendName = %q", res.BackendName)
	}
	if res.SessionID != "s1" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
	if res.Tokens != 3 {
		t.Errorf("Tokens = %d, want 3", res.Tokens)
	}
}

func TestDefaultRegistry_IncludesGrok(t *testing.T) {
	r := DefaultRegistry(testLogger())
	if _, err := r.Resolve(BackendGrok); err != nil {
		t.Fatalf("DefaultRegistry missing grok: %v", err)
	}
}
