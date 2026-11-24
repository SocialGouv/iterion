package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/claw/hooks"
	"github.com/SocialGouv/iterion/pkg/claw/internal/apikit"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"strings"
	"testing"
)

// --- BLOCKER tests: PostToolUse result handling ---

func TestExecuteToolPostHookDenialEscalatesError(t *testing.T) {
	testutil.IsolateShellHome(t)
	// When PostToolUse hook returns Denied, ExecuteTool must set IsError=true
	// and merge hook messages into output.
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			// Use a shell command that outputs JSON with deny.
			PostToolUse: []string{`echo '{"continue":false,"reason":"post-hook denied"}'`},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.nonexistent_pattern_for_test_12345",
	})

	if !result.IsError {
		t.Error("expected IsError=true when PostToolUse hook denies")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "Hook feedback (error)") {
		t.Errorf("expected 'Hook feedback (error)' in output, got %q", text)
	}
	if !strings.Contains(text, "post-hook denied") {
		t.Errorf("expected hook denial message in output, got %q", text)
	}
}

func TestExecuteToolPostHookFailureEscalatesError(t *testing.T) {
	testutil.IsolateShellHome(t)
	// When PostToolUse hook fails (non-zero exit), escalate to isError=true.
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PostToolUse: []string{"exit 1"},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.nonexistent_pattern_for_test_12345",
	})

	if !result.IsError {
		t.Error("expected IsError=true when PostToolUse hook fails")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "Hook feedback (error)") {
		t.Errorf("expected 'Hook feedback (error)' in output, got %q", text)
	}
}

// --- BLOCKER tests: PreToolUse IsCancelled check ---

func TestExecuteToolPreHookCancelledBlocksTool(t *testing.T) {
	testutil.IsolateShellHome(t)
	// When PreToolUse hook is cancelled, tool should not execute.
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			// Use a hook that exits with code 2 (denial).
			PreToolUse: []string{"exit 2"},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.nonexistent_pattern_for_test_12345",
	})

	if !result.IsError {
		t.Error("expected IsError=true when PreToolUse hook denies")
	}
}

// --- Pre-hook message merging on success path ---

func TestExecuteToolPreHookMessagesMergedOnSuccess(t *testing.T) {
	testutil.IsolateShellHome(t)
	// Pre-hook messages should be merged into successful tool output.
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PreToolUse: []string{`echo '{"systemMessage":"pre-hook info"}'`},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.nonexistent_pattern_for_test_12345",
	})

	text := result.Content[0].Text
	if !strings.Contains(text, "Hook feedback") {
		t.Errorf("expected pre-hook message merged into output, got %q", text)
	}
	if !strings.Contains(text, "pre-hook info") {
		t.Errorf("expected 'pre-hook info' in merged output, got %q", text)
	}
}

// --- ask_user path: post-hooks + telemetry ---

func TestExecuteToolAskUserCallsPostHooks(t *testing.T) {
	testutil.IsolateShellHome(t)
	// Verify that the ask_user path calls post-hooks.
	// We use a post-hook that would set IsError if denial is returned.
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PostToolUse: []string{`echo '{"continue":false,"reason":"post denied ask_user"}'`},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "ask_user", map[string]any{
		"question": "What is your name?",
	})

	// ask_user with post-hook denial should escalate isError
	if !result.IsError {
		t.Error("expected IsError=true when PostToolUse hook denies ask_user")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "post denied ask_user") {
		t.Errorf("expected post-hook denial message in ask_user output, got %q", text)
	}
}

func TestExecuteToolAskUserCallsTelemetry(t *testing.T) {
	// Verify that ask_user path records tool_execute_end telemetry.
	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("test-session", sink)
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		Tracer:      tracer,
	}

	_ = loop.ExecuteTool(context.Background(), "ask_user", map[string]any{
		"question": "Hello?",
	})

	events := sink.Events()
	found := false
	for _, ev := range events {
		// SessionTracer.Record emits session_trace events with the name in SessionTrace.Name.
		if ev.Type == apikit.EventTypeSessionTrace && ev.SessionTrace != nil && ev.SessionTrace.Name == "tool_execute_end" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected tool_execute_end telemetry event for ask_user path")
	}
}

// --- runPostToolHooks return value ---

func TestRunPostToolHooksReturnsResult(t *testing.T) {
	testutil.IsolateShellHome(t)
	// Verify runPostToolHooks returns a HookRunResult, not void.
	loop := &ConversationLoop{
		Config: &Config{},
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PostToolUse: []string{`echo '{"continue":false,"reason":"hook denied"}'`},
		}),
	}

	result := loop.runPostToolHooks(context.Background(), "test_tool", "{}", "output", false)
	if !result.IsDenied() {
		t.Error("expected IsDenied()=true from runPostToolHooks")
	}
	if len(result.Messages) == 0 {
		t.Error("expected messages from denied hook")
	}
}

func TestRunPostToolHooksNilRunnerReturnsAllow(t *testing.T) {
	loop := &ConversationLoop{Config: &Config{}}

	result := loop.runPostToolHooks(context.Background(), "test_tool", "{}", "output", false)
	if result.IsDenied() || result.IsFailed() || result.IsCancelled() {
		t.Error("nil HookRunner should return Allow result")
	}
}

// --- Regression: successful tool with no hooks should not modify output ---

func TestExecuteToolNoHooksUnchangedOutput(t *testing.T) {
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.nonexistent_pattern_for_test_12345",
	})

	if result.IsError {
		t.Errorf("expected no error, got error: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if strings.Contains(text, "Hook feedback") {
		t.Errorf("expected no hook feedback in output without hooks, got %q", text)
	}
}

// --- PreToolUse permission override extraction (Phase 3) ---

func TestExecuteToolPreHookPermissionOverrideExtracted(t *testing.T) {
	testutil.IsolateShellHome(t)
	// Verify that a pre-hook returning permissionDecision=allow does not
	// break the tool execution flow. The override is extracted but not yet
	// wired to the permission check (Phase 4).
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PreToolUse: []string{`echo '{"permissionDecision":"allow","permissionDecisionReason":"safe tool"}'`},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.nonexistent_pattern_for_test_12345",
	})

	if result.IsError {
		t.Errorf("expected no error when pre-hook returns permissionDecision=allow, got: %s", result.Content[0].Text)
	}
}

// --- PostToolUseFailure hooks fire on error ---

func TestExecuteToolErrorFiresPostToolUseFailure(t *testing.T) {
	testutil.IsolateShellHome(t)
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PostToolUseFailure: []string{`echo '{"systemMessage":"failure hook ran"}'`},
		}),
	}

	// Call an unknown tool to trigger an error.
	result := loop.ExecuteTool(context.Background(), "nonexistent_tool", map[string]any{})

	if !result.IsError {
		t.Error("expected IsError for unknown tool")
	}
	text := result.Content[0].Text
	// The failure hook ran and its message should be merged.
	if !strings.Contains(text, "failure hook ran") {
		t.Errorf("expected PostToolUseFailure hook message in output, got %q", text)
	}
}
