package runner

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §4: the escape estimate classifies a node's route with the
// materializing screen's own reading (ir.PolicyLadderReading) — the
// message's vars resolve a `{{vars.model}}` node, and a gateway step of its
// `provider:` chain routes it to the gateway — so under team_gateway such a
// node escapes nothing: the screen lands no stage on it, and un-parking the
// run for one would strand it on the capped wire. Unstamped, the same
// gateway node escapes, as the materializer lands the crossing.
func TestLadderEscapes_ReadTheNodeRouteTheScreenReads(t *testing.T) {
	stages := queue.RunFallback{{Backend: "codex", Provider: "chatgpt_forfait", Policy: true, On: []string{"usage_window"}}}
	teamGateway := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway}
	for _, row := range []struct {
		name     string
		model    string
		provider string
		def      string
		vars     map[string]any
	}{
		{name: "{{vars.model}} whose declared default is a gateway route", model: "{{vars.model}}", def: "openai_compatible/scaleway/glm-5.3"},
		{name: "{{vars.model}} whose launch value is a gateway route", model: "{{vars.model}}", def: "anthropic/claude-opus-5", vars: map[string]any{"model": "openai_compatible/scaleway/glm-5.3"}},
		{name: "a gateway step in the provider chain over a vendor model", model: "anthropic/claude-opus-5", provider: "openai_compatible:openai_compatible/scaleway/glm-5.3"},
	} {
		t.Run(row.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{
				"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: row.model, Provider: row.provider}},
			}}
			if row.def != "" {
				wf.Vars = map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: row.def}}
			}
			if open := ladderEscapes(wf, &queue.RunMessage{Fallback: stages, Vars: row.vars}, false); !open["gw"] {
				t.Fatalf("unstamped escapes = %v, want the gateway node — the control", open)
			}
			if held := ladderEscapes(wf, &queue.RunMessage{Fallback: stages, Vars: row.vars, ContentBoundary: teamGateway}, false); held["gw"] {
				t.Fatalf("stamped escapes = %v: a node the run routes to the gateway takes no stage under team_gateway", held)
			}
		})
	}
}
