package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
)

func TestRegisterSettingsHooks_BlocksOnExit2(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A PreToolUse command hook matching Bash that denies (exit 2).
	settings := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo nope >&2; exit 2"}]}]}}`
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	r := hooks.NewRunner()
	if n := registerSettingsHooks(r, ws, nil); n != 1 {
		t.Fatalf("registerSettingsHooks wired %d events, want 1", n)
	}

	// Fire PreToolUse for Bash → must Block with the hook's stderr as reason.
	dec, err := r.Fire(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": "ls"},
	})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if dec.Action != hooks.ActionBlock {
		t.Fatalf("Action = %v, want Block", dec.Action)
	}

	// A non-matching tool name continues.
	dec2, _ := r.Fire(context.Background(), hooks.Context{
		Event:    hooks.PreToolUse,
		ToolName: "Read",
	})
	if dec2.Action != hooks.ActionContinue {
		t.Fatalf("non-matching tool Action = %v, want Continue", dec2.Action)
	}
}

func TestRegisterSettingsHooks_NoFile(t *testing.T) {
	r := hooks.NewRunner()
	if n := registerSettingsHooks(r, t.TempDir(), nil); n != 0 {
		t.Fatalf("no settings.json should wire 0 events, got %d", n)
	}
}
