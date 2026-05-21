package delegate

import (
	"strings"
	"testing"
)

func TestBuildSystemPromptNoAugmentation(t *testing.T) {
	task := Task{SystemPrompt: "base prompt"}
	got := task.BuildSystemPrompt()
	if got != "base prompt" {
		t.Fatalf("expected unchanged prompt, got %q", got)
	}
}

func TestBuildSystemPromptInteractionOnly(t *testing.T) {
	task := Task{SystemPrompt: "base prompt", InteractionEnabled: true}
	got := task.BuildSystemPrompt()
	if !strings.HasPrefix(got, "base prompt") {
		t.Fatalf("interaction suffix should be appended after base, got %q", got)
	}
	if strings.Contains(got, "## Calibration") {
		t.Fatalf("calibration section should be absent when CursorFragments is empty")
	}
}

func TestBuildSystemPromptCursorsOnly(t *testing.T) {
	task := Task{
		SystemPrompt: "base prompt",
		CursorFragments: []string{
			"**Ambition:** Stick strictly to the stated request.",
			"**Depth:** Trace all call sites and edge cases.",
		},
	}
	got := task.BuildSystemPrompt()
	if !strings.Contains(got, "## Calibration") {
		t.Fatalf("missing calibration section: %q", got)
	}
	if !strings.Contains(got, "**Ambition:**") || !strings.Contains(got, "**Depth:**") {
		t.Fatalf("cursor fragments not rendered: %q", got)
	}
	idxBase := strings.Index(got, "base prompt")
	idxCalib := strings.Index(got, "## Calibration")
	if idxBase >= idxCalib {
		t.Fatalf("base prompt must precede calibration section")
	}
}

func TestBuildSystemPromptInteractionAndCursors(t *testing.T) {
	task := Task{
		SystemPrompt:       "base prompt",
		InteractionEnabled: true,
		CursorFragments:    []string{"**Ambition:** test"},
	}
	got := task.BuildSystemPrompt()
	if !strings.Contains(got, "## Calibration") {
		t.Fatalf("missing calibration section")
	}
	idxInteract := strings.Index(got, "_needs_interaction")
	idxCalib := strings.Index(got, "## Calibration")
	if idxInteract <= 0 || idxCalib <= 0 || idxInteract >= idxCalib {
		t.Fatalf("interaction protocol must precede calibration (interact=%d, calib=%d)", idxInteract, idxCalib)
	}
}
