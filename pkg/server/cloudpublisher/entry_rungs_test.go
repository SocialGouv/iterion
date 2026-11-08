package cloudpublisher

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The entry rungs land after the operator chain, in the grants' order
// (forfaits before metered keys — the acquisition's order), carrying the
// frozen triggers as On and the slot each grant sealed as Provider. A grant
// without an explicit binding composes nothing.
func TestComposeEntryRungs_orderAndCarriage(t *testing.T) {
	// The node declares its tools explicitly: a claw rung crossing the
	// harness boundary on a tool-less node is the composed-pair refusal the
	// screen correctly raises (declare `tools: []` for none).
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{
			Backend: "claude_code",
			Model:   "claude-opus-5-5",
		},
		Tools: []string{},
	}}}
	grants := []*credpool.Grant{
		// No binding: funds whatever its slot natively serves — no rung.
		{Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"}, DonorID: "free"},
		{Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"}, Backend: "claude_code", Model: "claude-sonnet-4-5", DonorID: "free2"},
		// The gateway entry: metered, last — the paid destination rung.
		{Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)}, Backend: "claw", Model: "openai_compatible/scaleway/deepseek-v4.1-flash", DonorID: "gw"},
	}
	var refusals []string
	composeEntryRungs(wf, model.RouteView{Workflow: wf}, grants, []string{"usage_window"}, nil, func(r string) {
		refusals = append(refusals, r)
	})
	if len(refusals) != 0 {
		t.Fatalf("screen refusals = %v, want none on an unsandboxed one-node program", refusals)
	}
	agent := wf.Nodes["implement"].(*ir.AgentNode)
	var entries []ir.Fallback
	for _, fb := range agent.Fallbacks {
		if fb.Model == "claude-sonnet-4-5" || fb.Model == "openai_compatible/scaleway/deepseek-v4.1-flash" {
			entries = append(entries, fb)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("entry rungs = %d, want 2 — the binding-less grant composes nothing", len(entries))
	}
	if entries[0].Model != "claude-sonnet-4-5" || entries[1].Model != "openai_compatible/scaleway/deepseek-v4.1-flash" {
		t.Errorf("entry order = %s, %s — want the forfait rung first, the paid gateway rung last", entries[0].Model, entries[1].Model)
	}
	if !slices.Equal(entries[0].On, []string{"usage_window"}) || !slices.Equal(entries[1].On, []string{"usage_window"}) {
		t.Errorf("On = %v / %v, want the frozen triggers on both rungs", entries[0].On, entries[1].On)
	}
	if entries[0].Provider != slotOfKind("claude_code") {
		t.Errorf("forfait rung Provider = %q, want the slot it sealed (%q)", entries[0].Provider, slotOfKind("claude_code"))
	}
	if entries[1].Provider != slotOfProvider(secrets.ProviderOpenAICompatible) {
		t.Errorf("gateway rung Provider = %q, want %q", entries[1].Provider, slotOfProvider(secrets.ProviderOpenAICompatible))
	}
	if !entries[1].Metered {
		t.Error("the gateway rung must carry Metered — it spends a metered credential")
	}
	if entries[0].Metered {
		t.Error("the forfait rung must not carry Metered")
	}
	// An empty trigger list composes nothing: the frozen "never pre-empt"
	// answer is no rungs at all (the M4 round-trip lesson).
	wf2 := policyWf()
	composeEntryRungs(wf2, model.RouteView{Workflow: wf2}, grants, []string{}, nil, nil)
	agent2 := wf2.Nodes["implement"].(*ir.AgentNode)
	for _, fb := range agent2.Fallbacks {
		if fb.Model == "claude-sonnet-4-5" || fb.Model == "openai_compatible/scaleway/deepseek-v4.1-flash" {
			t.Error("an empty frozen trigger list must compose no entry rung")
		}
	}
}
