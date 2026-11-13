package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func TestResolvePromptConfig(t *testing.T) {
	// nil block → all on.
	p := ResolvePromptConfig(nil)
	if p != DefaultPromptConfig() {
		t.Errorf("nil block: %+v, want all-on defaults", p)
	}

	// minimal → all off.
	p = ResolvePromptConfig(&config.RuntimePromptConfig{Minimal: boolPtr(true)})
	if p != MinimalPromptConfig() {
		t.Errorf("minimal: %+v, want all-off", p)
	}

	// minimal + explicit true → only that section on.
	p = ResolvePromptConfig(&config.RuntimePromptConfig{
		Minimal:             boolPtr(true),
		ProjectInstructions: boolPtr(true),
	})
	want := MinimalPromptConfig()
	want.ProjectInstructions = true
	if p != want {
		t.Errorf("minimal+explicit: %+v, want only ProjectInstructions on", p)
	}

	// explicit false alone → everything else stays on.
	p = ResolvePromptConfig(&config.RuntimePromptConfig{GitStatus: boolPtr(false)})
	want = DefaultPromptConfig()
	want.GitStatus = false
	if p != want {
		t.Errorf("explicit false: %+v, want defaults minus GitStatus", p)
	}

	// MemoryMaxBytes carried through.
	p = ResolvePromptConfig(&config.RuntimePromptConfig{MemoryMaxBytes: 2048})
	if p.MemoryMaxBytes != 2048 {
		t.Errorf("MemoryMaxBytes = %d, want 2048", p.MemoryMaxBytes)
	}
}

func TestApplyPromptSectionOverrides(t *testing.T) {
	// --minimal-prompt alone.
	cfg := &Config{}
	if err := ApplyPromptSectionOverrides(cfg, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if *cfg.Prompt != MinimalPromptConfig() {
		t.Errorf("minimal: %+v", *cfg.Prompt)
	}

	// --prompt-sections implies minimal; kebab/camel/underscore all accepted.
	cfg = &Config{}
	if err := ApplyPromptSectionOverrides(cfg, false, []string{"git-status", "mcpTools", "project_instructions"}, nil); err != nil {
		t.Fatal(err)
	}
	want := MinimalPromptConfig()
	want.GitStatus = true
	want.McpTools = true
	want.ProjectInstructions = true
	if *cfg.Prompt != want {
		t.Errorf("only-list: %+v, want %+v", *cfg.Prompt, want)
	}

	// --disable-prompt-sections keeps the other defaults.
	cfg = &Config{}
	if err := ApplyPromptSectionOverrides(cfg, false, nil, []string{"environment"}); err != nil {
		t.Fatal(err)
	}
	want = DefaultPromptConfig()
	want.Environment = false
	if *cfg.Prompt != want {
		t.Errorf("disable-list: %+v, want %+v", *cfg.Prompt, want)
	}

	// Unknown section name errors.
	cfg = &Config{}
	err := ApplyPromptSectionOverrides(cfg, false, []string{"bogus"}, nil)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("expected unknown-section error, got %v", err)
	}

	// MemoryMaxBytes survives a minimal reset.
	cfg = &Config{Prompt: &PromptConfig{MemoryMaxBytes: 4096}}
	if err := ApplyPromptSectionOverrides(cfg, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Prompt.MemoryMaxBytes != 4096 {
		t.Errorf("MemoryMaxBytes lost on minimal reset: %d", cfg.Prompt.MemoryMaxBytes)
	}
}

func TestSystemPromptSectionGating(t *testing.T) {
	summary := "previously discussed the widget refactor"

	newLoop := func(prompt *PromptConfig) *ConversationLoop {
		sess := NewSession()
		sess.CompactionSummary = summary
		return &ConversationLoop{
			Config:  &Config{Prompt: prompt},
			Session: sess,
		}
	}

	behavioralHeaders := []string{
		"# Operating posture",
		"# Communicating with the user",
		"# Task management",
		"# Doing tasks",
		"# Tool usage policy",
		"# Git safety",
		"# Context management",
	}

	// Defaults (nil prompt config): every behavioral section + compaction
	// summary present, in declaration order after the base sentence.
	got := newLoop(nil).systemPrompt()
	if !strings.Contains(got, summary) {
		t.Errorf("default prompt config: compaction summary missing:\n%s", got)
	}
	if !strings.HasPrefix(got, systemPromptBase) {
		t.Errorf("system prompt must start with the base sentence")
	}
	prev := len(systemPromptBase)
	for _, header := range behavioralHeaders {
		idx := strings.Index(got, header)
		if idx < 0 {
			t.Errorf("default prompt config: %s missing:\n%s", header, got)
			continue
		}
		if idx < prev {
			t.Errorf("%s out of order (index %d < %d)", header, idx, prev)
		}
		prev = idx
	}

	// Single-section isolation: disabling one behavioral section removes
	// exactly that section.
	solo := DefaultPromptConfig()
	solo.GitSafety = false
	got = newLoop(&solo).systemPrompt()
	if strings.Contains(got, "# Git safety") {
		t.Errorf("git-safety disabled but still rendered:\n%s", got)
	}
	if !strings.Contains(got, "# Task management") {
		t.Errorf("disabling git-safety must not drop other sections:\n%s", got)
	}

	// Minimal: every behavioral section + compaction summary gated off.
	minimal := MinimalPromptConfig()
	got = newLoop(&minimal).systemPrompt()
	if strings.Contains(got, summary) {
		t.Errorf("minimal prompt config: compaction summary leaked:\n%s", got)
	}
	for _, header := range behavioralHeaders {
		if strings.Contains(got, header) {
			t.Errorf("minimal prompt config: %s leaked:\n%s", header, got)
		}
	}
	if got != systemPromptBase {
		t.Errorf("minimal + no assembler: want bare base sentence, got:\n%s", got)
	}

	// nil Config on the loop: falls back to defaults without panicking.
	sess := NewSession()
	sess.CompactionSummary = summary
	loop := &ConversationLoop{Session: sess}
	if got := loop.systemPrompt(); !strings.Contains(got, summary) {
		t.Errorf("nil Config: expected default gating, got:\n%s", got)
	}
}

func TestSystemPromptCustomAndAppend(t *testing.T) {
	newLoop := func(cfg *Config) *ConversationLoop {
		return &ConversationLoop{Config: cfg, Session: NewSession()}
	}

	// SystemPrompt replaces identity + posture + behavioral sections.
	got := newLoop(&Config{SystemPrompt: "You are TestBot."}).systemPrompt()
	if !strings.HasPrefix(got, "You are TestBot.") {
		t.Errorf("custom prompt must replace the base sentence:\n%s", got)
	}
	if strings.Contains(got, systemPromptBase) || strings.Contains(got, "# Operating posture") ||
		strings.Contains(got, "# Task management") {
		t.Errorf("custom prompt must suppress the authored base sections:\n%s", got)
	}

	// AppendSystemPrompt lands at the very end of the assembled prompt.
	got = newLoop(&Config{AppendSystemPrompt: "Always answer in French."}).systemPrompt()
	if !strings.HasSuffix(got, "Always answer in French.") {
		t.Errorf("appended prompt must close the assembled prompt:\n%s", got)
	}
	if !strings.HasPrefix(got, systemPromptBase) {
		t.Errorf("append must not replace the base:\n%s", got)
	}

	// Both combined: custom base + appended suffix.
	got = newLoop(&Config{SystemPrompt: "Custom base.", AppendSystemPrompt: "Suffix."}).systemPrompt()
	if !strings.HasPrefix(got, "Custom base.") || !strings.HasSuffix(got, "Suffix.") {
		t.Errorf("custom+append composition wrong:\n%s", got)
	}
}
