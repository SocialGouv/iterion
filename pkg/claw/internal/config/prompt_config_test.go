package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// Settings.Prompt (typed unmarshal + field-wise merge) is the single decode
// point for the prompt block: the "prompt" settings block is decoded when a
// settings file unmarshals into Settings.
func TestSettingsPromptDecode(t *testing.T) {
	var s Settings
	data := []byte(`{"prompt": {"minimal": true, "gitStatus": false, "memoryMaxBytes": 1024}}`)
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Prompt == nil || s.Prompt.Minimal == nil || !*s.Prompt.Minimal {
		t.Errorf("Minimal not decoded: %+v", s.Prompt)
	}
	if s.Prompt.GitStatus == nil || *s.Prompt.GitStatus {
		t.Errorf("GitStatus = %v, want false", s.Prompt.GitStatus)
	}
	if s.Prompt.Environment != nil {
		t.Errorf("Environment = %v, want nil (unset)", s.Prompt.Environment)
	}
	if s.Prompt.MemoryMaxBytes != 1024 {
		t.Errorf("MemoryMaxBytes = %d, want 1024", s.Prompt.MemoryMaxBytes)
	}
}

func TestMergePromptConfigLayered(t *testing.T) {
	// Global sets gitStatus: false; project sets minimal: true.
	// Both must survive the merge (field-wise, not wholesale).
	dst := &Settings{Prompt: &RuntimePromptConfig{GitStatus: boolPtr(false)}}
	src := &Settings{Prompt: &RuntimePromptConfig{Minimal: boolPtr(true)}}
	merge(dst, src)
	if dst.Prompt.GitStatus == nil || *dst.Prompt.GitStatus {
		t.Errorf("GitStatus lost in merge: %v", dst.Prompt.GitStatus)
	}
	if dst.Prompt.Minimal == nil || !*dst.Prompt.Minimal {
		t.Errorf("Minimal not merged: %v", dst.Prompt.Minimal)
	}
}

func TestMergePromptConfigProjectWithoutPromptKeepsGlobal(t *testing.T) {
	dst := &Settings{Prompt: &RuntimePromptConfig{Environment: boolPtr(false)}}
	src := &Settings{Model: "m"} // project file without a prompt block
	merge(dst, src)
	if dst.Prompt == nil || dst.Prompt.Environment == nil || *dst.Prompt.Environment {
		t.Errorf("global prompt block erased by project file: %+v", dst.Prompt)
	}
}

func TestMergePromptConfigExplicitFalseWins(t *testing.T) {
	dst := &Settings{Prompt: &RuntimePromptConfig{Posture: boolPtr(true)}}
	src := &Settings{Prompt: &RuntimePromptConfig{Posture: boolPtr(false)}}
	merge(dst, src)
	if dst.Prompt.Posture == nil || *dst.Prompt.Posture {
		t.Errorf("explicit false did not win: %v", dst.Prompt.Posture)
	}
}

func TestMergeAllowImmediateStructuredOutputExplicitFalseWins(t *testing.T) {
	dst := &Settings{AllowImmediateStructuredOutput: boolPtr(true)}
	src := &Settings{AllowImmediateStructuredOutput: boolPtr(false)}
	merge(dst, src)
	if dst.AllowImmediateStructuredOutput == nil || *dst.AllowImmediateStructuredOutput {
		t.Errorf("AllowImmediateStructuredOutput = %v, want false", dst.AllowImmediateStructuredOutput)
	}
}

func TestValidatePromptBlock(t *testing.T) {
	// Valid block: no diagnostics.
	res := ValidateSettingsJSON([]byte(`{"prompt": {"minimal": true, "gitStatus": false}}`), "settings.json")
	if !res.IsClean() {
		t.Errorf("valid prompt block produced diagnostics: %s", FormatDiagnostics(&res))
	}

	// Typo gets a suggestion.
	res = ValidateSettingsJSON([]byte(`{"prompt": {"gitstatus": false}}`), "settings.json")
	if !res.HasErrors() {
		t.Fatal("expected unknown-key error for prompt.gitstatus")
	}
	if !strings.Contains(res.Errors[0].String(), "gitStatus") {
		t.Errorf("expected gitStatus suggestion, got: %s", res.Errors[0])
	}

	// Wrong type.
	res = ValidateSettingsJSON([]byte(`{"prompt": {"minimal": "yes"}}`), "settings.json")
	if !res.HasErrors() {
		t.Fatal("expected wrong-type error for prompt.minimal string")
	}
}

func TestParseFrontmatterPromptKeys(t *testing.T) {
	input := "---\nmodel: claude-sonnet-4-6\nminimalPrompt: true\npromptSections:\n- project-instructions\n- posture\ndisablePromptSections: git-status, environment\n---\nBody."
	cfg, body, err := ParseFrontmatter([]byte(input))
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if cfg.Model == nil || *cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %v", cfg.Model)
	}
	if cfg.MinimalPrompt == nil || !*cfg.MinimalPrompt {
		t.Errorf("MinimalPrompt = %v, want true", cfg.MinimalPrompt)
	}
	if len(cfg.PromptSections) != 2 || cfg.PromptSections[0] != "project-instructions" || cfg.PromptSections[1] != "posture" {
		t.Errorf("PromptSections = %v", cfg.PromptSections)
	}
	if len(cfg.DisablePromptSections) != 2 || cfg.DisablePromptSections[0] != "git-status" || cfg.DisablePromptSections[1] != "environment" {
		t.Errorf("DisablePromptSections = %v", cfg.DisablePromptSections)
	}
	if string(body) != "Body." {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatterInlineBracketList(t *testing.T) {
	cfg, _, err := ParseFrontmatter([]byte("---\npromptSections: [environment, git-status]\n---\n"))
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if len(cfg.PromptSections) != 2 || cfg.PromptSections[0] != "environment" || cfg.PromptSections[1] != "git-status" {
		t.Errorf("PromptSections = %v", cfg.PromptSections)
	}
}
