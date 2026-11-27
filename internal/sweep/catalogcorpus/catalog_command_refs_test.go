package catalogcorpus

import (
	"regexp"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// outputsRef matches an {{outputs.<node>.<field>}} reference.
var outputsRef = regexp.MustCompile(`\{\{\s*!?\s*outputs\.[A-Za-z0-9_.]+\s*\}\}`)

// TestCatalogToolCommandsResolveTheirRefs pins a seam that fails SILENTLY.
//
// A tool node's `command:` is resolved by resolveCommandTemplate
// (pkg/backend/model/executor_tool.go), which substitutes {{input.X}},
// {{vars.X}}, {{secrets.X}} and {{run.id}} — and nothing else. An
// {{outputs.<node>.<field>}} ref belongs on an EDGE mapping, where it does
// resolve; written inside a command body it survives as literal text, and the
// shell happily passes that text on.
//
// Nothing catches it: the bot parses, compiles, validates and runs. The
// failure only appears in production, as a value that is a template string
// pretending to be data.
//
// It has already shipped once. bots/review-pr's stale-anchor guard compared
// REVIEWED_SHA={{outputs.diff_precheck.reviewed_sha}} against the real HEAD;
// the literal never equals a sha, so every review skipped publishing — and
// since that publish carries the merge-gate status, and revi/review is a
// required check, every PR on the repo became unmergeable with no error
// anywhere. Found by reading a run log, not by any test.
func TestCatalogToolCommandsResolveTheirRefs(t *testing.T) {
	// Every shipped workflow, not just each bundle's main: a subbot source
	// is precisely where an unresolvable ref hides from a main-bot-only
	// sweep. The corpus table carries it, floored per root — the unbounded
	// walk this replaced had only a total emptiness check, and read the
	// test fixtures beside the shipped corpus.
	targets := catalogWorkflowFiles(t)

	checked, params := 0, 0
	for _, path := range targets {
		pr := parseBotUnit(path)
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			// Named, not silent: a bot that stops compiling would otherwise
			// drop out of this lint without a trace.
			t.Logf("%s: not inspected (does not compile to a workflow)", path)
			continue
		}
		for _, n := range cr.Workflow.Nodes {
			tool, ok := n.(*ir.ToolNode)
			if !ok {
				continue
			}
			// Postcondition rides the same resolver (executor_verified_action.go),
			// and it is ADR-044's truth oracle: a literal there mis-decides the
			// whole recovery ladder.
			for _, body := range []string{tool.Command, tool.Script, tool.Postcondition} {
				if body == "" {
					continue
				}
				checked++
				if m := outputsRef.FindString(body); m != "" {
					t.Errorf("%s: node %q passes %s inside its command/script body; "+
						"only {{input.X}}, {{vars.X}}, {{secrets.X}} and {{run.id}} are "+
						"substituted there, so this "+
						"reaches the shell as literal text. Map it on the edge into this "+
						"node and read it as {{input.<field>}}.", path, tool.ID, m)
				}
			}
			// The fourth body: an action's `params:`. The engine refuses a
			// tool node BEFORE its body runs when the body reads an
			// {{outputs.<node>.<field>}} whose producer ran and whose own
			// `output:` schema declares the field, and the output does not
			// carry it (model.UnresolvedOutputRefError, #3421). The three
			// bodies above hold no such reference at all; a parameter may
			// read a fan-out item or an untyped output, which that refusal
			// never judges — and none reads a schema-declared field, so no
			// shipped tool node can be stopped by it.
			for _, param := range tool.Params {
				for _, ref := range param.Refs {
					if ref == nil || ref.Kind != ir.RefOutputs || len(ref.Path) < 2 {
						continue
					}
					params++
					producer, field := ref.Path[0], ref.Path[1]
					schema := cr.Workflow.Schemas[ir.NodeOutputSchema(cr.Workflow.Nodes[producer])]
					if schema == nil {
						continue
					}
					for _, f := range schema.Fields {
						if f != nil && f.Name == field {
							t.Errorf("%s: node %q reads %s in its `params:`, a field node %q declares in `output: %s`: "+
								"if that producer ever answers without it, this node is refused before its action runs. "+
								"Map it on the edge into this node and read it as {{input.<field>}}, where the hole is the edge's to explain.",
								path, tool.ID, ref.Raw, producer, schema.Name)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no tool bodies inspected — the IR shape likely changed")
	}
	// rgaa-audit's publish_create reads four fields of its fan-out item.
	if params == 0 {
		t.Fatal("no action parameter reading {{outputs.…}} inspected — the IR shape likely changed")
	}
}
