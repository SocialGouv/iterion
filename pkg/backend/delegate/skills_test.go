package delegate

import (
	"strings"
	"testing"
)

func TestBuildSystemPromptSkillsSection(t *testing.T) {
	task := Task{
		SystemPrompt: "base prompt",
		SkillHints: []SkillHint{
			{Name: "changelog-writer", Description: "Writes changelogs"},
			{Name: "semver-bump"},
		},
	}
	got := task.BuildSystemPrompt()
	if !strings.Contains(got, "## Skills") {
		t.Fatalf("missing skills section: %q", got)
	}
	if !strings.Contains(got, "- changelog-writer: Writes changelogs") {
		t.Errorf("described skill not rendered: %q", got)
	}
	if !strings.Contains(got, "- semver-bump\n") {
		t.Errorf("undescribed skill not rendered: %q", got)
	}
}

func TestBuildSystemPromptNoSkillsSection(t *testing.T) {
	task := Task{SystemPrompt: "base prompt"}
	if strings.Contains(task.BuildSystemPrompt(), "## Skills") {
		t.Fatal("skills section should be absent when SkillHints is empty")
	}
}

func TestBuildSystemPromptSkillsBeforeFocus(t *testing.T) {
	task := Task{
		SystemPrompt:   "base prompt",
		SkillHints:     []SkillHint{{Name: "s1"}},
		PresetFragment: "focus bias",
	}
	got := task.BuildSystemPrompt()
	if i, j := strings.Index(got, "## Skills"), strings.Index(got, "## Focus"); i < 0 || j < 0 || i >= j {
		t.Fatalf("## Skills must precede ## Focus (skills=%d focus=%d): %q", i, j, got)
	}
}
