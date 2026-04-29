package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
)

func mustSafetyHook(t *testing.T, patterns []string) hooks.Handler {
	t.Helper()
	h, err := SafetyHook(patterns)
	if err != nil {
		t.Fatalf("SafetyHook(%v): %v", patterns, err)
	}
	return h
}

func TestSafetyHook_BlocksRmRfRoot(t *testing.T) {
	h := mustSafetyHook(t, DefaultDangerousCommandPatterns())
	dec, err := h(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "bash",
		ToolInput: map[string]any{"command": "rm -rf /"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != hooks.ActionBlock {
		t.Fatalf("expected Block, got %v", dec.Action)
	}
}

func TestSafetyHook_BlocksForkBomb(t *testing.T) {
	h := mustSafetyHook(t, DefaultDangerousCommandPatterns())
	dec, err := h(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "bash",
		ToolInput: map[string]any{"command": ":(){ :|:& };:"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != hooks.ActionBlock {
		t.Fatalf("expected Block, got %v", dec.Action)
	}
}

func TestSafetyHook_AllowsBenignCommand(t *testing.T) {
	h := mustSafetyHook(t, DefaultDangerousCommandPatterns())
	dec, err := h(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "bash",
		ToolInput: map[string]any{"command": "ls -la /tmp"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != hooks.ActionContinue {
		t.Errorf("expected Continue for benign command, got %v", dec.Action)
	}
}

func TestSafetyHook_NoCommandKey_Continues(t *testing.T) {
	h := mustSafetyHook(t, DefaultDangerousCommandPatterns())
	dec, err := h(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "read_file",
		ToolInput: map[string]any{"path": "/tmp/x"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != hooks.ActionContinue {
		t.Errorf("expected Continue when no command field, got %v", dec.Action)
	}
}

func TestSafetyHook_InvalidPatternErrors(t *testing.T) {
	h, err := SafetyHook([]string{"[invalid("})
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
	if h != nil {
		t.Errorf("expected nil handler when patterns failed to compile")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error message should mention invalid pattern, got %q", err.Error())
	}
}

func TestSafetyHook_PartialFailureRejects(t *testing.T) {
	// Even one bad pattern should fail loudly so operators do not run
	// with an incomplete safety net.
	h, err := SafetyHook([]string{`(?i)\brm -rf /`, "[invalid("})
	if err == nil {
		t.Fatal("expected error when at least one pattern is invalid")
	}
	if h != nil {
		t.Errorf("expected nil handler when any pattern fails to compile")
	}
}

func TestNewDefaultLifecycleHooks_BlocksDangerousCommand(t *testing.T) {
	r := NewDefaultLifecycleHooks(EventHooks{})
	dec, _ := r.Fire(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "bash",
		ToolInput: map[string]any{"command": "rm -rf /"},
	})
	if dec.Action != hooks.ActionBlock {
		t.Errorf("default runner should block rm -rf /, got %v", dec.Action)
	}
}
