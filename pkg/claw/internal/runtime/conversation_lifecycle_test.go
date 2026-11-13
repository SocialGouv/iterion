package runtime

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lifehooks "github.com/SocialGouv/iterion/pkg/claw/internal/hooks"
)

// TestConversation_PreToolUseBlock_SkipsTool verifies that a PreToolUse hook
// returning ActionBlock causes ExecuteTool to short-circuit with a synthetic
// error tool_result rather than executing the underlying tool.
func TestConversation_PreToolUseBlock_SkipsTool(t *testing.T) {
	runner := lifehooks.NewRunner(lifehooks.WithLogger(io.Discard))

	var preCalls atomic.Int32
	var postCalls atomic.Int32
	runner.Register(lifehooks.PreToolUse, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		preCalls.Add(1)
		if hctx.ToolName != "glob" {
			t.Errorf("expected ToolName=glob, got %q", hctx.ToolName)
		}
		return lifehooks.Decision{Action: lifehooks.ActionBlock, Reason: "no globs allowed"}, nil
	})
	runner.Register(lifehooks.PostToolUse, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		postCalls.Add(1)
		return lifehooks.Decision{Action: lifehooks.ActionContinue}, nil
	})
	// PostToolUseFailure should fire (since we synthesized an error).
	runner.Register(lifehooks.PostToolUseFailure, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		postCalls.Add(1)
		return lifehooks.Decision{Action: lifehooks.ActionContinue}, nil
	})

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		LifecycleHooks: runner,
	}

	// Pattern that, if executed, would NOT error but would return some result.
	// We rely on the Block decision to short-circuit before that point.
	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.go",
	})

	if !result.IsError {
		t.Errorf("expected IsError=true on Block, got false; result=%+v", result)
	}
	if got := result.Content[0].Text; !strings.Contains(got, "no globs allowed") {
		t.Errorf("expected reason in result text, got %q", got)
	}
	if preCalls.Load() != 1 {
		t.Errorf("expected PreToolUse to fire exactly once, got %d", preCalls.Load())
	}
	// PostToolUseFailure should fire so observers see the rejection
	// symmetrically; PostToolUse (success) must NOT fire.
	if postCalls.Load() != 1 {
		t.Errorf("expected exactly one Post* hook to fire (failure variant), got %d", postCalls.Load())
	}
}

// TestConversation_PreToolUseAllow_RunsTool verifies that a PreToolUse hook
// returning ActionContinue lets execution proceed normally and PostToolUse
// fires after the tool completes successfully.
func TestConversation_PreToolUseAllow_RunsTool(t *testing.T) {
	runner := lifehooks.NewRunner(lifehooks.WithLogger(io.Discard))

	var preCalls, postSuccess, postFailure atomic.Int32
	runner.Register(lifehooks.PreToolUse, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		preCalls.Add(1)
		return lifehooks.Decision{Action: lifehooks.ActionContinue}, nil
	})
	runner.Register(lifehooks.PostToolUse, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		postSuccess.Add(1)
		if hctx.ToolName != "glob" {
			t.Errorf("expected ToolName=glob, got %q", hctx.ToolName)
		}
		if hctx.ToolError != nil {
			t.Errorf("PostToolUse should have nil ToolError, got %v", hctx.ToolError)
		}
		return lifehooks.Decision{Action: lifehooks.ActionContinue}, nil
	})
	runner.Register(lifehooks.PostToolUseFailure, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		postFailure.Add(1)
		return lifehooks.Decision{Action: lifehooks.ActionContinue}, nil
	})

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		LifecycleHooks: runner,
	}

	// "*.go" succeeds (the working directory has Go files; even if it
	// matched nothing the tool would still return a non-error empty result).
	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.go",
	})

	if result.IsError {
		t.Errorf("expected IsError=false on Continue, got true; text=%q", result.Content[0].Text)
	}
	if preCalls.Load() != 1 {
		t.Errorf("expected PreToolUse to fire exactly once, got %d", preCalls.Load())
	}
	if postSuccess.Load() != 1 {
		t.Errorf("expected PostToolUse to fire exactly once, got %d", postSuccess.Load())
	}
	if postFailure.Load() != 0 {
		t.Errorf("expected PostToolUseFailure NOT to fire, got %d", postFailure.Load())
	}
}

// TestConversation_NoLifecycleHooks_NoChange verifies that ExecuteTool with
// LifecycleHooks=nil behaves identically to before — no panics, hooks-related
// fields are simply skipped.
func TestConversation_NoLifecycleHooks_NoChange(t *testing.T) {
	loop := &ConversationLoop{
		Config:      &Config{},
		Permissions: DefaultPermissions(),
		// LifecycleHooks left nil.
	}

	result := loop.ExecuteTool(context.Background(), "glob", map[string]any{
		"pattern": "*.go",
	})
	if result.Type != "tool_result" {
		t.Fatalf("expected tool_result, got %q", result.Type)
	}
}

// TestExecuteTool_PropagatesCtxCancellation verifies that the ctx passed to
// ExecuteTool flows into LifecycleHooks.Fire so handlers observe upstream
// cancellation. Previously, lifecycle helpers used context.Background()
// which made Stop signals invisible to long-running pre/post hooks.
func TestExecuteTool_PropagatesCtxCancellation(t *testing.T) {
	runner := lifehooks.NewRunner(lifehooks.WithLogger(io.Discard))

	var observedErr error
	runner.Register(lifehooks.PreToolUse, func(ctx context.Context, hctx lifehooks.Context) (lifehooks.Decision, error) {
		select {
		case <-ctx.Done():
			observedErr = ctx.Err()
		case <-time.After(50 * time.Millisecond):
			observedErr = nil
		}
		return lifehooks.Decision{Action: lifehooks.ActionContinue}, nil
	})

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		LifecycleHooks: runner,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before call

	_ = loop.ExecuteTool(ctx, "glob", map[string]any{"pattern": "*.go"})

	if observedErr != context.Canceled {
		t.Fatalf("expected pre-hook to observe context.Canceled, got %v", observedErr)
	}
}
