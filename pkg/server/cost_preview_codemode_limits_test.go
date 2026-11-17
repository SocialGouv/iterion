package server

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The launch form's codemode bounds (CM-5f, #3346) are built from this
// block: the placeholder shows the default, the ceiling draws the warning.
// It is READ FROM scripthost — the package whose Clamp enforces the same
// numbers at spawn — so a hand-written table in the studio cannot drift from
// what a run actually experiences. This witness pins that provenance: the
// response's numbers ARE the two functions' answers.
func TestPreviewCostCarriesTheCodemodeLimitsFromScriptHost(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(minimalAgentSource)+`}`)
	if got.CodemodeLimits == nil {
		t.Fatal("the preview response carries no codemode_limits — the launch form would fall back to a hand-kept table")
	}
	def := scripthost.DefaultLimits()
	ceil := scripthost.Ceiling()

	if d := got.CodemodeLimits.Default; d.ToolCalls != def.ToolCalls || d.Memory != def.Memory ||
		d.ScriptBytes != def.ScriptBytes || d.ResultBytes != def.ResultBytes ||
		d.ToolResultBytes != def.ToolResultBytes || d.Wall != def.Wall.String() || d.CPU != def.CPU.String() {
		t.Errorf("default = %+v, want scripthost.DefaultLimits() %+v (durations as %q/%q)", d, def, def.Wall, def.CPU)
	}
	if c := got.CodemodeLimits.Ceiling; c.ToolCalls != ceil.ToolCalls || c.Memory != ceil.Memory ||
		c.ScriptBytes != ceil.ScriptBytes || c.ResultBytes != ceil.ResultBytes ||
		c.ToolResultBytes != ceil.ToolResultBytes || c.Wall != ceil.Wall.String() || c.CPU != ceil.CPU.String() {
		t.Errorf("ceiling = %+v, want scripthost.Ceiling() %+v", c, ceil)
	}
	// The two must differ on the bounds a form actually edits — a block that
	// returned the same numbers twice would caption every preset as legal
	// and make the warning unreachable.
	if got.CodemodeLimits.Default.Memory == got.CodemodeLimits.Ceiling.Memory {
		t.Error("default and ceiling carry the same memory bound; the form's warning could never fire")
	}
	if got.CodemodeLimits.Ceiling.Wall != (10 * time.Minute).String() {
		t.Errorf("ceiling wall = %q, want %q — the DSL orthography the form's parse expects", got.CodemodeLimits.Ceiling.Wall, (10 * time.Minute).String())
	}
	// The ONE bound with a floor (CM-5f, adversarial round): a value under it
	// is raised at spawn, so the form must know it before the launch — the
	// "screen shows 1000 bytes, the run runs at 64 MiB" divergence.
	if got.CodemodeLimits.MemoryFloor != scripthost.MemoryFloor().Memory {
		t.Errorf("memory_floor = %d, want scripthost.MemoryFloor() %d", got.CodemodeLimits.MemoryFloor, scripthost.MemoryFloor().Memory)
	}
	if got.CodemodeLimits.MemoryFloor >= got.CodemodeLimits.Default.Memory {
		t.Errorf("memory_floor %d is not below the default %d — a floor that equals the default would make the warning unreachable", got.CodemodeLimits.MemoryFloor, got.CodemodeLimits.Default.Memory)
	}
}
