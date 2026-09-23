package bots

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A field added to an AGENT node's input schema and mapped on its edge still
// reaches the model only if some prompt renders it: the template resolver
// exposes `input.X` on demand (pkg/backend/model/template_resolver.go), so a
// field no template references is a field the model never sees.
//
// Paid on #1598. `build_skipped` was added to four `review_input` schemas,
// mapped on four edges, and the review procedure was amended to branch on it —
// and no `review_user` prompt rendered it. The model kept seeing only
// "build passed: false" and kept short-circuiting to `clean: true`, which was
// the exact regression the change claimed to have fixed. Schema plus edge plus
// procedure looked like three-out-of-three; it was three out of four, and the
// three greppable hits made it read as done.
//
// Scope, deliberately, and each bound is measured:
//
//   - AGENT nodes only. A `human` node renders its input to a person through
//     the console, not through a prompt template — campaign's `review_input`
//     feeds `human handoff_review`, and demanding a `{{input.…}}` for it would
//     be a false positive in a blocking guard, which costs as much as a hole.
//   - `review_input` only. Swept catalogue-wide: 10 of 88 agent input schemas
//     carry at least one unrendered field today (adr-cartograph, copilot ×2,
//     docs-refresh, evolve ×3, product-docs, sec-audit-source, wiki-gen). That
//     is real debt and it has its own ticket; none of it is `review_input`, so
//     this guard is green on the class it protects without conscripting ten
//     unrelated bots into this change.
func TestReviewInputFieldsReachAPrompt(t *testing.T) {
	mains, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, rel := range mains {
		pr := parseBotUnit(rel)
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue
		}

		// Only prompt bodies count. A `{{input.X}}` sitting in a tool node's
		// command would satisfy a source-wide grep while telling the reviewing
		// model nothing.
		names := make([]string, 0, len(cr.Workflow.Prompts))
		for name := range cr.Workflow.Prompts {
			names = append(names, name)
		}
		sort.Strings(names)
		var prompts strings.Builder
		for _, name := range names {
			if p := cr.Workflow.Prompts[name]; p != nil {
				prompts.WriteString(p.Body)
				prompts.WriteString("\n")
			}
		}
		body := prompts.String()

		nodeNames := make([]string, 0, len(cr.Workflow.Nodes))
		for name := range cr.Workflow.Nodes {
			nodeNames = append(nodeNames, name)
		}
		sort.Strings(nodeNames)

		for _, node := range nodeNames {
			agent, ok := cr.Workflow.Nodes[node].(*ir.AgentNode)
			if !ok || agent.InputSchema != "review_input" {
				continue
			}
			schema := cr.Workflow.Schemas[agent.InputSchema]
			if schema == nil {
				continue
			}
			checked++
			t.Run(rel+"/"+node, func(t *testing.T) {
				for _, f := range schema.Fields {
					if f == nil {
						continue
					}
					if !strings.Contains(body, "{{input."+f.Name+"}}") {
						t.Errorf("review_input declares %q and no prompt renders {{input.%s}} — "+
							"the field is mapped on the edge and invisible to the model, so any "+
							"procedure step branching on it is dead text", f.Name, f.Name)
					}
				}
			})
		}
	}

	// The failure mode of a discovery is to match nothing and leave a vacuous
	// guard looking green.
	if checked < 4 {
		t.Fatalf("found %d agent nodes taking a review_input schema, want >= 4 — the discovery "+
			"is stale and this guard proves nothing", checked)
	}
}
