package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestFeatureDevPerseveranceCoach guards feature-dev's supervisor (the
// perseverance coach) non-vacuously. The supervisor cross-reference
// diagnostics (C190 unknown watched node, C193 unknown system prompt)
// are WARNINGS by design — a typo'd `watches:` or `system:` still
// compiles clean and the coach silently never arms. This test pins what
// the bot promises: a supervisor exists, it watches the campaign agent
// node, its policy prompt is declared, and both the policy and the
// campaign contract carry the coaching clauses they were shipped for.
func TestFeatureDevPerseveranceCoach(t *testing.T) {
	const path = "feature-dev/main.bot"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pr := parser.Parse(path, string(src))
	if pr.File == nil {
		t.Fatalf("%s: parser produced no File", path)
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("%s: compile error: %s", path, d.Error())
		}
	}
	wf := cr.Workflow
	if wf == nil {
		t.Fatalf("%s: compile produced no workflow", path)
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

	policy, ok := wf.Prompts[sup.System]
	if !ok || policy == nil {
		t.Fatalf("supervisor %q system prompt %q is not declared (C193 is only a warning — fix the reference)", sup.Name, sup.System)
	}
	for _, clause := range []string{"GIVING UP", "EXPEDIENT", "BANK NOW", "termination"} {
		if !strings.Contains(policy.Body, clause) {
			t.Errorf("supervisor policy %q lost its %q clause", sup.System, clause)
		}
	}

	// The static half of the same shipment: the campaign contract's
	// PERSISTENCE clause (anti-premature-impossible + bank-before-abandon).
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
