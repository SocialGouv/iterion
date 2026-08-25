package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestFeatureDevPerseveranceCoach guards feature-dev's supervisor (the
// perseverance coach) non-vacuously. The supervisor cross-reference
// diagnostics (C190 unknown watched node, C191 malformed monitor, C193
// unknown system prompt) are WARNINGS by design — a typo'd `watches:`,
// `monitors:` or `system:` still compiles clean and the coach silently
// never arms. This test pins what the bot promises: a supervisor
// exists, it watches the campaign agent node with pre-seeded monitors,
// its policy prompt is declared, and the campaign contract keeps the
// PERSISTENCE clause the coach enforces. (Compile errors are already
// caught for every bot by TestCatalogBotsParseAndCompileClean.)
func TestFeatureDevPerseveranceCoach(t *testing.T) {
	wf := compileBot(t, "feature-dev")
	if wf == nil {
		t.Fatal("feature-dev did not compile")
	}

	if len(wf.Supervisors) == 0 {
		t.Fatal("feature-dev declares no supervisor — the perseverance coach (v2.2.0) is gone")
	}
	sup := wf.Supervisors[0]

	watchesCampaign := false
	for _, nodeID := range sup.Watches {
		n, ok := wf.Nodes[nodeID]
		if !ok {
			t.Errorf("supervisor %q watches %q, which is not a declared node (C190 is only a warning — fix the id)", sup.Name, nodeID)
			continue
		}
		if _, isAgent := n.(*ir.AgentNode); !isAgent {
			t.Errorf("supervisor %q watches %q (%T) — only agent nodes can be steered", sup.Name, nodeID, n)
		}
		if nodeID == "campaign" {
			watchesCampaign = true
		}
	}
	if !watchesCampaign {
		t.Errorf("supervisor %q does not watch the campaign node (watches: %v)", sup.Name, sup.Watches)
	}

	if sup.MaxEvals <= 0 {
		t.Errorf("supervisor %q has no max_evals bound — eval spend must be capped", sup.Name)
	}
	if sup.Model != "" {
		t.Errorf("supervisor %q pins model %q — catalog bots leave it to auto-detect / ITERION_DEFAULT_SUPERVISOR_MODEL", sup.Name, sup.Model)
	}

	// Pre-seeded monitors are what closes the first-wake blind window
	// (measured in docs/bot-runs/feature-dev.md): every declared spec
	// must satisfy the grammar the spawn-time parser enforces, and the
	// set must include at least one give-up marker.
	if len(sup.Monitors) == 0 {
		t.Error("supervisor declares no pre-seeded monitors — the coach is blind until its first eval")
	}
	hasTextMarker := false
	for _, spec := range sup.Monitors {
		if err := ir.CheckMonitorSpec(spec); err != nil {
			t.Errorf("supervisor monitor %q is malformed (C191 is only a warning): %v", spec, err)
		}
		if strings.Contains(spec, "text_contains=") {
			hasTextMarker = true
		}
	}
	if !hasTextMarker {
		t.Error("no text_contains give-up marker among the pre-seeded monitors")
	}

	policy, ok := wf.Prompts[sup.System]
	if !ok || policy == nil {
		t.Fatalf("supervisor %q system prompt %q is not declared (C193 is only a warning — fix the reference)", sup.Name, sup.System)
	}
	// One sentinel: the policy must still be the coaching policy, not a
	// stub. Wording beyond this is free to evolve.
	if !strings.Contains(policy.Body, "INTERVENE") {
		t.Errorf("supervisor policy %q no longer reads as the coaching policy (INTERVENE sentinel missing)", sup.System)
	}

	// The static half of the same shipment: the campaign contract's
	// PERSISTENCE clause (anti-premature-infeasibility + bank-before-abandon).
	campaign, _ := wf.Nodes["campaign"].(*ir.AgentNode)
	if campaign == nil {
		t.Fatal("feature-dev has no campaign agent node")
	}
	sys, ok := wf.Prompts[campaign.SystemPrompt]
	if !ok || sys == nil {
		t.Fatalf("campaign system prompt %q is not declared", campaign.SystemPrompt)
	}
	if !strings.Contains(sys.Body, "PERSISTENCE") {
		t.Error("campaign contract lost its PERSISTENCE clause")
	}
}
