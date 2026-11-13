package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
)

// TestExecuteBashWithEnvAppendsEntries verifies that extraEnv values
// reach the spawned bash subprocess and are visible to commands run
// in it. The legacy ExecuteBash entry point delegates here with nil
// extraEnv, so the test also locks in the no-extra-env baseline.
func TestExecuteBashWithEnvAppendsEntries(t *testing.T) {
	// These run a real bash: an ambient CLAW_BASH_TIMEOUT must not reach them.
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	const probe = "ITERION_BASH_ENV_PROBE_VAL"

	// Without extraEnv the var is unset.
	out, err := ExecuteBashWithEnv(
		context.Background(),
		map[string]any{"command": "echo \"${" + probe + ":-UNSET}\""},
		permissions.ModeAllow, "", nil,
	)
	if err != nil {
		t.Fatalf("ExecuteBashWithEnv (no extra env): %v", err)
	}
	if got := strings.TrimSpace(out); got != "UNSET" {
		t.Errorf("baseline: got %q, want UNSET", got)
	}

	// With extraEnv the var resolves.
	out, err = ExecuteBashWithEnv(
		context.Background(),
		map[string]any{"command": "echo \"${" + probe + ":-UNSET}\""},
		permissions.ModeAllow, "", []string{probe + "=hello"},
	)
	if err != nil {
		t.Fatalf("ExecuteBashWithEnv (with extra env): %v", err)
	}
	if got := strings.TrimSpace(out); got != "hello" {
		t.Errorf("with extra env: got %q, want hello", got)
	}
}

// TestExecuteBashWithEnvOverridesParentValue confirms that a later
// extraEnv entry wins over the inherited parent value, matching Go's
// exec.Cmd convention. This is what lets a caller surface a
// project-managed PATH that prepends the devbox bin directory.
func TestExecuteBashWithEnvOverridesParentValue(t *testing.T) {
	// These run a real bash: an ambient CLAW_BASH_TIMEOUT must not reach them.
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	t.Setenv("ITERION_BASH_ENV_OVERRIDE", "from-parent")

	out, err := ExecuteBashWithEnv(
		context.Background(),
		map[string]any{"command": "echo \"$ITERION_BASH_ENV_OVERRIDE\""},
		permissions.ModeAllow, "", []string{"ITERION_BASH_ENV_OVERRIDE=from-extra"},
	)
	if err != nil {
		t.Fatalf("ExecuteBashWithEnv: %v", err)
	}
	if got := strings.TrimSpace(out); got != "from-extra" {
		t.Errorf("override: got %q, want from-extra", got)
	}
}

// TestExecuteBashLegacyEntryStillNoExtraEnv guards the documented
// behaviour of the legacy ExecuteBash entry point: it must NOT pick
// up extra env (callers wanting that should switch to
// ExecuteBashWithEnv).
func TestExecuteBashLegacyEntryStillNoExtraEnv(t *testing.T) {
	// These run a real bash: an ambient CLAW_BASH_TIMEOUT must not reach them.
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	const probe = "ITERION_BASH_ENV_LEGACY"
	out, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "echo \"${" + probe + ":-UNSET}\""},
		permissions.ModeAllow, "",
	)
	if err != nil {
		t.Fatalf("ExecuteBash: %v", err)
	}
	if got := strings.TrimSpace(out); got != "UNSET" {
		t.Errorf("legacy: got %q, want UNSET", got)
	}
}
