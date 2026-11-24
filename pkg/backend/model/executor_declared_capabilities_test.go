package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The runtime half of #1801's empty-list fix: a node whose `capabilities:`
// is the DECLARED empty list holds no host capability — the workflow's
// `board.*` grant is NOT inherited — where a node with no `capabilities:`
// line at all inherits it. The executor reads the distinction off nilness
// (`if effectiveCaps == nil`), so the DSL only has to carry it through.
func TestEffectiveToolNames_DeclaredEmptyCapabilitiesHoldNoBoardGrant(t *testing.T) {
	t.Parallel()
	node := func(id string, caps []string) *ir.AgentNode {
		return &ir.AgentNode{
			BaseNode:     ir.BaseNode{ID: id},
			LLMFields:    ir.LLMFields{Backend: "claw"},
			Tools:        []string{"read_file"},
			Capabilities: caps,
		}
	}
	wf := &ir.Workflow{
		Capabilities: []string{"board.read"},
		Nodes: map[string]ir.Node{
			"inherits":    node("inherits", nil),
			"holds_none":  node("holds_none", []string{}),
			"explicit_ok": node("explicit_ok", []string{"board.read"}),
		},
	}
	e := NewClawExecutor(NewRegistry(), wf)
	boardTools := func(names []string) []string {
		var out []string
		for _, n := range names {
			if strings.HasPrefix(n, "mcp__iterion_board__") {
				out = append(out, n)
			}
		}
		return out
	}
	if got := boardTools(e.EffectiveToolNames(wf.Nodes["inherits"], false)); len(got) == 0 {
		t.Fatal("a node with an absent capabilities: must inherit the workflow's board.read grant")
	}
	if got := boardTools(e.EffectiveToolNames(wf.Nodes["explicit_ok"], false)); len(got) == 0 {
		t.Fatal("a node naming board.read must hold the board tools")
	}
	if got := boardTools(e.EffectiveToolNames(wf.Nodes["holds_none"], false)); len(got) != 0 {
		t.Fatalf("`capabilities: []` is no grant — the workflow's board.read must NOT be inherited, got %v", got)
	}
}
