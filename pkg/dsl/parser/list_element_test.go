package parser

import (
	"reflect"
	"strings"
	"testing"
)

// An element of a list of names that is not a name — a quoted string in an
// ident list, a number in a tool or skill list — is refused where it
// stands, with one diagnostic naming it, and the other elements are read.
// It used to be left out in silence: `servers: ["forge"]` read as an empty
// list and the server was never wired, `tools: [1, bash]` read as `[bash]`.
func TestAListElementThatIsNotANameIsRefusedNotDropped(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
		hint string
		pick func(res *ParseResult) []string
	}{
		{"servers, quoted", "agent a:\n  mcp:\n    servers: [\"forge\", other]\n", []string{"other"}, "without quotes",
			func(r *ParseResult) []string { return r.File.Agents[0].MCP.Servers }},
		{"disable, number", "workflow w:\n  entry: done\n  mcp:\n    disable: [1, other]\n", []string{"other"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Workflows[0].MCP.Disable }},
		{"watches, quoted dash form", "supervisor s:\n  watches:\n    - \"a\"\n    - b\n", []string{"b"}, "without quotes",
			func(r *ParseResult) []string { return r.File.Supervisors[0].Watches }},
		{"fallback on, quoted", "agent a:\n  fallbacks:\n    r:\n      on: [\"auth\", any]\n", []string{"any"},
			"without quotes", func(r *ParseResult) []string { return r.File.Agents[0].Fallbacks[0].On }},
		{"needs, quoted", "agent a:\n  needs: [\"gpu\", cpu]\n", []string{"cpu"}, "without quotes",
			func(r *ParseResult) []string { return r.File.Agents[0].Needs }},
		{"tools, number", "agent a:\n  tools: [1, bash]\n", []string{"bash"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].Tools }},
		{"artifact_labels, float", "agent a:\n  artifact_labels: [plan, 1.5]\n", []string{"plan"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].ArtifactLabels }},
		{"skills, number inline", "agent a:\n  skills: [1, house.style]\n", []string{"house.style"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].Skills }},
		{"skills, number dash form", "agent a:\n  skills:\n    - 1\n    - house.style\n", []string{"house.style"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].Skills }},
		{"agent_tools, number", "tool t:\n  command: \"x\"\n  recovery:\n    agent_tools: [bash, 2]\n", []string{"bash"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Tools[0].Recovery.AgentTools }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 1 {
				t.Fatalf("want exactly one diagnostic, got %v", res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Code != DiagExpectedToken || !strings.Contains(d.Message, "in the list") || !strings.Contains(strings.ToLower(d.Hint), c.hint) {
				t.Fatalf("got %s %q / hint %q", d.Code, d.Message, d.Hint)
			}
			if got := c.pick(res); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("the other elements were not read: %v, want %v", got, c.want)
			}
		})
	}
}

// A group's parameter list is a list of names too: `group g("a", b):` used
// to read as `group g(b):` in silence, and every `{{params.a}}` of the
// group's prompts then stayed unbound.
func TestAGroupParameterThatIsNotANameIsRefusedNotDropped(t *testing.T) {
	for _, src := range []string{
		"group g(\"a\", b):\n  agent x:\n    model: \"m\"\n",
		"group g(b, 1):\n  agent x:\n    model: \"m\"\n",
	} {
		res := Parse("x.bot", src)
		if len(res.Diagnostics) != 1 {
			t.Fatalf("%q: want exactly one diagnostic, got %v", src, res.Diagnostics)
		}
		if d := res.Diagnostics[0]; d.Code != DiagExpectedToken || !strings.Contains(d.Message, "parameter name") {
			t.Fatalf("%q: got %s %q", src, d.Code, d.Message)
		}
		if got := res.File.Groups[0].Params; !reflect.DeepEqual(got, []string{"b"}) {
			t.Fatalf("%q: the other parameter was not read: %v", src, got)
		}
	}
}
