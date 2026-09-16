package parser

import "testing"

// `contract` is a keyword for the declaration and the workflow property,
// and a name like any other everywhere else: a node named contract is
// declared, is the entry, is the source of an edge — the arrow, not the
// token's type, says a line is an edge — beside the workflow's own
// `contract: c` line.
func TestANodeNamedContractKeepsItsEdges(t *testing.T) {
	src := "vars:\n  goal: string\n\nprompt u:\n  Do {{vars.goal}}.\n\nagent contract:\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: u\n\ncontract c:\n  inputs:\n    goal: string\n\nworkflow w:\n  contract: c\n  entry: contract\n  contract -> done\n"
	pr := Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("a node named contract does not parse: %s", d.Error())
		}
	}
	if len(pr.File.Agents) != 1 || pr.File.Agents[0].Name != "contract" {
		t.Fatalf("the agent named contract came out as %+v", pr.File.Agents)
	}
	wf := pr.File.Workflows[0]
	if wf.Contract != "c" || wf.Entry != "contract" {
		t.Fatalf("the workflow's contract and entry came out as %q / %q", wf.Contract, wf.Entry)
	}
	if len(wf.Edges) != 1 || wf.Edges[0].From != "contract" || wf.Edges[0].To != "done" {
		t.Fatalf("the edge from the node named contract came out as %+v", wf.Edges)
	}
}
