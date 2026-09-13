package parser

import (
	"strings"
	"testing"
)

// An unknown property arrives with the registry's remedy in its hint: the
// closest accepted name, the block the name belongs to, or the kind's list.
func TestUnknownPropertyCarriesTheRegistryRemedy(t *testing.T) {
	cases := []struct {
		name, src, msg, hint string
	}{
		{"typo on a tool", "tool t:\n  comand: \"x\"\n", "unknown tool property 'comand'", "Did you mean `command`?"},
		{"network property on the sandbox", "workflow w:\n  entry: a\n  sandbox:\n    rules: [\"a\"]\n", "unknown sandbox property 'rules'", "indent it under `network:`"},
		{"workflow property inside budget", "workflow w:\n  budget:\n    entry: a\n", "unknown budget property 'entry'", "belongs to the enclosing `workflow`"},
		{"another kind's property", "compute c:\n  command: \"x\"\n", "unknown compute property 'command'", "compute does not take it"},
		{"agent list", "agent a:\n  zzz: 1\n", "unknown agent property 'zzz'", "agent accepts: description, model"},
		// A block with several hosts names the host it is IN, never another:
		// an mcp: block under a workflow is not an agent's.
		{"agent property in a workflow's mcp", "workflow w:\n  entry: a\n  mcp:\n    model: \"x\"\n", "unknown mcp property 'model'", "`model` is a property of agent/fallback/human/judge/"},
		{"agent property in an agent's mcp", "agent a:\n  mcp:\n    model: \"x\"\n", "unknown mcp property 'model'", "belongs to the enclosing `agent`"},
		{"workflow property in an agent's mcp", "agent a:\n  mcp:\n    entry: x\n", "unknown mcp property 'entry'", "`entry` is a property of workflow"},
		{"tool property in a tool's sandbox", "tool t:\n  command: \"x\"\n  sandbox:\n    goal: \"g\"\n", "unknown sandbox property 'goal'", "belongs to the enclosing `tool`"},
		// A nested single-host block names its own host even under another.
		{"sandbox property in a network under a workflow", "workflow w:\n  entry: a\n  sandbox:\n    image: \"i\"\n    network:\n      image: \"x\"\n", "unknown sandbox.network property 'image'", "belongs to the enclosing `sandbox`"},
	}
	for _, c := range cases {
		res := Parse("t.bot", c.src)
		var found bool
		for _, d := range res.Diagnostics {
			if d.Code != DiagUnknownProperty {
				continue
			}
			found = true
			if d.Message != c.msg {
				t.Errorf("%s: message %q, want %q", c.name, d.Message, c.msg)
			}
			if !strings.Contains(d.Hint, c.hint) {
				t.Errorf("%s: hint %q does not contain %q", c.name, d.Hint, c.hint)
			}
			if strings.Contains(d.Hint, "enclosing") && !strings.Contains(c.hint, "enclosing") {
				t.Errorf("%s: hint %q names an enclosing kind it cannot know", c.name, d.Hint)
			}
		}
		if !found {
			t.Errorf("%s: no E012 in %v", c.name, res.Diagnostics)
		}
	}
}

// A `name:` line the workflow does not know is an unknown PROPERTY (E012,
// with the remedy), not an edge that lacks its arrow — and its indented body,
// when it has one, is dropped whole rather than reported line by line. The
// edges around it still parse.
func TestUnknownWorkflowPropertyIsE012NotAMissingArrow(t *testing.T) {
	src := "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  budjet:\n    max_cost_usd: 1\n  a -> done\n"
	res := Parse("t.bot", src)
	if len(res.Diagnostics) != 1 {
		t.Fatalf("want exactly one diagnostic, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != DiagUnknownProperty || d.Message != "unknown workflow property 'budjet'" {
		t.Fatalf("got %s %q", d.Code, d.Message)
	}
	if !strings.Contains(d.Hint, "Did you mean `budget`?") {
		t.Fatalf("hint %q lacks the suggestion", d.Hint)
	}
	if d.Line != 5 {
		t.Fatalf("positioned at line %d, want 5", d.Line)
	}
	wf := res.File.Workflows[0]
	if len(wf.Edges) != 1 || wf.Entry != "a" {
		t.Fatalf("the surrounding members did not parse: entry %q, %d edges", wf.Entry, len(wf.Edges))
	}
}

// An edge line is still an edge — the colon peek must not swallow it.
func TestEdgesStillParseAfterTheColonPeek(t *testing.T) {
	src := "workflow w:\n  entry: a\n  a -> b when ok\n  a -> done else\n  b -> a as fix(3)\n"
	res := Parse("t.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	if got := len(res.File.Workflows[0].Edges); got != 3 {
		t.Fatalf("want 3 edges, got %d", got)
	}
}
