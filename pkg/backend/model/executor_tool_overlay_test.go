package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A tool command's `${ITERION_*:-default}` resolves through the bot-var
// overlay like every other expansion of the DSL (ADR-093). It used to read the
// process env alone, so a review table published "high" beside a reviewer
// that ran at the stored "max".
func TestToolCommandEnvReadsTheBotVarOverlay(t *testing.T) {
	t.Setenv("ITERION_TEST_TOOL_DIAL", "")
	ir.SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_TEST_TOOL_DIAL" {
			return "max", true
		}
		return "", false
	})
	defer ir.SetEnvOverlay(nil)

	if got := expandBracedEnv("EFFORT=${ITERION_TEST_TOOL_DIAL:-high}"); got != "EFFORT=max" {
		t.Errorf("expanded %q, want EFFORT=max — the tool command ignored the stored bot var", got)
	}
	// A defaultless ref the overlay answers is an env ref, not a template.
	if got := expandBracedEnv("x=${ITERION_TEST_TOOL_DIAL}"); got != "x=max" {
		t.Errorf("expanded %q, want x=max", got)
	}
}

// Presence keeps the process semantics the script-template guard relies on:
// a name neither the overlay nor the process knows stays verbatim, and the
// overlay never reaches a non-ITERION_ name.
func TestToolCommandEnvOverlayKeepsTheTemplateGuard(t *testing.T) {
	ir.SetEnvOverlay(func(name string) (string, bool) { return "leak", true })
	defer ir.SetEnvOverlay(nil)

	if got := expandBracedEnv("`${fname}`"); got != "`${fname}`" {
		t.Errorf("expanded %q — a script template was eaten through the overlay", got)
	}
	if got := expandBracedEnv("${ITERION_TEST_TOOL_DIAL_EMPTY_OVERLAY:-d}"); got != "leak" {
		t.Errorf("expanded %q, want the overlay value for an ITERION_ name", got)
	}
}
