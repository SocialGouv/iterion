package hookstesting

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/hooks"
)

func TestNewHookRunnerWithOverride_Nil(t *testing.T) {
	runner := NewHookRunnerWithOverride(nil, "")
	if runner == nil {
		t.Fatal("expected non-nil runner for nil decision")
	}
	result := runner.RunPreToolUse(context.Background(), "bash", `{"command":"echo"}`)
	if result.PermissionOverride != nil {
		t.Error("nil decision should produce nil override")
	}
}

func TestNewHookRunnerWithOverride_Allow(t *testing.T) {
	testutil.IsolateShellHome(t)
	allow := hooks.PermissionAllow
	runner := NewHookRunnerWithOverride(&allow, "test reason")
	if runner == nil {
		t.Fatal("expected non-nil runner")
	}
	result := runner.RunPreToolUse(context.Background(), "bash", `{"command":"echo"}`)
	if result.PermissionOverride == nil {
		t.Fatal("expected non-nil permission override")
	}
	if *result.PermissionOverride != hooks.PermissionAllow {
		t.Errorf("expected PermissionAllow, got %q", *result.PermissionOverride)
	}
	if result.PermissionReason != "test reason" {
		t.Errorf("expected reason 'test reason', got %q", result.PermissionReason)
	}
}

func TestNewHookRunnerWithOverride_Deny(t *testing.T) {
	testutil.IsolateShellHome(t)
	deny := hooks.PermissionDeny
	runner := NewHookRunnerWithOverride(&deny, "blocked")
	result := runner.RunPreToolUse(context.Background(), "bash", `{"command":"rm -rf /"}`)
	if result.PermissionOverride == nil || *result.PermissionOverride != hooks.PermissionDeny {
		t.Error("expected PermissionDeny override")
	}
}

func TestNewHookRunnerWithOverride_Ask(t *testing.T) {
	testutil.IsolateShellHome(t)
	ask := hooks.PermissionAsk
	runner := NewHookRunnerWithOverride(&ask, "needs confirmation")
	result := runner.RunPreToolUse(context.Background(), "bash", `{"command":"echo"}`)
	if result.PermissionOverride == nil || *result.PermissionOverride != hooks.PermissionAsk {
		t.Error("expected PermissionAsk override")
	}
}
