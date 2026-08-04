package bots

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/types"
)

// TestCatalogHumanLLMModelSpecsCarryAProvider pins a seam that only fails at
// the moment a run needs it most.
//
// A human node's llm / llm_or_human half runs through the DIRECT generation
// path (GenerateObjectDirect), which takes a "provider/model-id" spec and has
// no backend to infer the provider from. An agent node is different: its
// claude_code backend happily accepts a bare "claude-opus-5", which is why
// the bare form reads correct everywhere else in a bot and survives every
// review.
//
// Nothing catches the mismatch until the escalation path actually fires:
// Vetty's `escalate` node carried a bare default from birth, every prior run
// took the clean/committed routes around it, and the FIRST needs_decision
// bump in production (a plugin-react major, 2026-08-04) crashed the run with
// `invalid spec "claude-opus-5"` instead of asking the human — the exact
// moment the workflow existed to hand over.
func TestCatalogHumanLLMModelSpecsCarryAProvider(t *testing.T) {
	var targets []string
	for _, root := range []string{".", "../examples"} {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
				return nil
			}
			targets = append(targets, path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery likely broke")
	}

	inspected := 0
	for _, path := range targets {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", path, err)
			continue
		}
		pr := parser.Parse(path, string(src))
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			t.Logf("%s: not inspected (does not compile to a workflow)", path)
			continue
		}
		for _, n := range cr.Workflow.Nodes {
			h, ok := n.(*ir.HumanNode)
			if !ok {
				continue
			}
			if h.Interaction != types.InteractionLLM && h.Interaction != types.InteractionLLMOrHuman {
				continue
			}
			for _, spec := range []string{h.InteractionModel, h.Model} {
				if spec == "" {
					continue
				}
				inspected++
				if bareModelSpec(spec) {
					t.Errorf("%s: human node %q (interaction: %s) declares model %q — the direct "+
						"generation path needs \"provider/model-id\" (e.g. \"anthropic/claude-opus-5\"); "+
						"a bare name crashes the run at the exact moment it tries to escalate",
						path, h.ID, h.Interaction, spec)
				}
			}
		}
	}
	if inspected == 0 {
		t.Fatal("no llm-interaction human model specs inspected — either the fleet dropped them all or the IR shape changed")
	}
}

// bareModelSpec reports whether a model spec (possibly an ${ENV:-default}
// substitution) lacks the provider/ prefix. Only the DEFAULT half of a
// substitution is judged: the env override is the operator's to get right at
// set time, the default is what every unconfigured deployment runs.
func bareModelSpec(spec string) bool {
	s := strings.TrimSpace(spec)
	// ${VAR:-default} → default; ${VAR} alone → nothing checkable statically.
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}")
		_, def, ok := strings.Cut(inner, ":-")
		if !ok {
			return false
		}
		s = strings.TrimSpace(def)
	}
	if s == "" {
		return false
	}
	return !strings.Contains(s, "/")
}
