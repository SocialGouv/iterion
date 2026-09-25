package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// Compile the whole unit so imported reviewers, fallback routes, human
// interactions and supervisors are checked along with the visible main file.
// Explicit deployment overrides remain supported; this checks shipped defaults.
func TestCatalogCurrentModelDefaults(t *testing.T) {
	paths, err := teamBotFiles()
	if err != nil || len(paths) == 0 {
		t.Fatalf("catalog discovery: %v (%d files)", err, len(paths))
	}
	checked, models, anthropic, openai := 0, 0, 0, 0
	for _, path := range paths {
		u := unit.LoadDir(path)
		if u.Merged == nil {
			t.Fatalf("%s: no parsed unit", path)
		}
		for _, d := range u.Diagnostics {
			if d.Severity == parser.SeverityError {
				t.Fatalf("%s: %s", path, d.Error())
			}
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Fatalf("%s: no compiled workflow", path)
		}
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Fatalf("%s: %s", path, d.Error())
			}
		}
		checked++
		check := func(where, spec string) {
			resolved := ir.ExpandWithDefault(spec, func(string) string { return "" })
			if resolved == "" {
				return
			} // implicit backend defaults are tested with the backend
			models++
			id := strings.TrimPrefix(strings.TrimPrefix(resolved, "anthropic/"), "openai/")
			switch {
			case strings.HasPrefix(id, "claude-") || id == "opus" || id == "sonnet" || id == "haiku" || id == "fable":
				anthropic++
				if id != "claude-opus-5-5" {
					t.Errorf("%s %s: Anthropic default %q, want Opus 5.5", path, where, resolved)
				}
			case strings.HasPrefix(id, "gpt-"):
				openai++
				if id != "gpt-6-astra" && id != "gpt-6-sol" && id != "gpt-6-luna" {
					t.Errorf("%s %s: OpenAI default %q, want GPT-6", path, where, resolved)
				}
			}
		}
		for id, node := range cr.Workflow.Nodes {
			switch n := node.(type) {
			case ir.LLMNode:
				check(id+".model", n.GetLLMFields().Model)
				check(id+".interaction_model", n.GetInteractionFields().InteractionModel)
				for _, fb := range n.GetFallbacks() {
					check(id+".fallback."+fb.Name, fb.Model)
				}
			case *ir.RouterNode:
				check(id+".model", n.Model)
			case *ir.HumanNode:
				check(id+".model", n.Model)
				check(id+".interaction_model", n.InteractionModel)
			case *ir.ToolNode:
				if n.Recovery != nil {
					check(id+".recovery.model", n.Recovery.Model)
				}
			}
		}
		for _, sup := range cr.Workflow.Supervisors {
			check("supervisor."+sup.Name, sup.Model)
		}
	}
	if checked != len(paths) || anthropic == 0 || openai == 0 {
		t.Fatalf("incomplete audit: workflows %d/%d, Anthropic %d, OpenAI %d", checked, len(paths), anthropic, openai)
	}
	t.Logf("checked %d workflows and %d model paths (%d Anthropic, %d OpenAI)", checked, models, anthropic, openai)
}
