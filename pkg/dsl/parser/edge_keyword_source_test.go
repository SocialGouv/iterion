package parser

import "testing"

// A node may bear any keyword as its name — declared, referenced by
// `entry:`, and used as the SOURCE of an edge, in a workflow and in a group
// alike, and as a group-instance prefix of a dotted endpoint. The two
// containers of edges decide property-or-edge on the arrow that follows a
// complete reference, never on the name's token type. Generated from the
// lexer's own keyword table, so a keyword added later is covered; `done`
// and `fail` are the reserved targets and stay refused as names (E011).
func TestAKeywordNamedNodeIsAnEdgeSourceInEveryContainer(t *testing.T) {
	n := 0
	for _, k := range Keywords() {
		if k == "done" || k == "fail" {
			continue
		}
		n++
		t.Run(k, func(t *testing.T) {
			src := "agent " + k + ":\n  description: \"x\"\n\nworkflow w:\n  entry: " + k + "\n  " + k + " -> done\n"
			res := Parse("w.bot", src)
			if len(res.Diagnostics) != 0 {
				t.Fatalf("workflow: %v", res.Diagnostics)
			}
			wf := res.File.Workflows[0]
			if wf.Entry != k || len(wf.Edges) != 1 || wf.Edges[0].From != k || wf.Edges[0].To != "done" {
				t.Fatalf("workflow read entry %q, edges %+v", wf.Entry, wf.Edges)
			}

			src = "group g:\n  agent " + k + ":\n    description: \"x\"\n  agent b:\n    description: \"y\"\n  " + k + " -> b\n\nuse g as " + k + "\n\nworkflow w:\n  entry: " + k + "." + k + "\n  " + k + ".b -> done\n"
			res = Parse("g.bot", src)
			if len(res.Diagnostics) != 0 {
				t.Fatalf("group: %v", res.Diagnostics)
			}
			g := res.File.Groups[0]
			if len(g.Agents) != 2 || len(g.Edges) != 1 || g.Edges[0].From != k || g.Edges[0].To != "b" {
				t.Fatalf("group read agents %d, edges %+v", len(g.Agents), g.Edges)
			}
			wf = res.File.Workflows[0]
			if wf.Entry != k+"."+k || len(wf.Edges) != 1 || wf.Edges[0].From != k+".b" {
				t.Fatalf("instance-prefixed edge read as entry %q, edges %+v", wf.Entry, wf.Edges)
			}
		})
	}
	if n < 100 {
		t.Fatalf("only %d keywords probed — the table is not being read", n)
	}
}

// The property and the declaration still win when no arrow follows: a
// workflow's `entry:` stays a property, a group's `agent x:` a member.
func TestPropertiesAndDeclarationsStillWinWithoutAnArrow(t *testing.T) {
	src := "agent entry:\n  description: \"x\"\n\nworkflow w:\n  entry: entry\n  budget:\n    max_cost_usd: 1\n  entry -> done\n"
	res := Parse("w.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("%v", res.Diagnostics)
	}
	wf := res.File.Workflows[0]
	if wf.Entry != "entry" || wf.Budget == nil || len(wf.Edges) != 1 {
		t.Fatalf("entry %q budget %v edges %d", wf.Entry, wf.Budget != nil, len(wf.Edges))
	}
	src = "group g:\n  agent agent:\n    description: \"x\"\n  agent tool:\n    description: \"y\"\n  agent -> tool\n"
	res = Parse("g.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("%v", res.Diagnostics)
	}
	if g := res.File.Groups[0]; len(g.Agents) != 2 || len(g.Edges) != 1 || g.Edges[0].From != "agent" || g.Edges[0].To != "tool" {
		t.Fatalf("group: %+v", res.File.Groups[0])
	}
}
