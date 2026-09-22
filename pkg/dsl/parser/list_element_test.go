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
		// The string|ident lists (a sandbox's mounts, a network's rules):
		// the inline form used to APPEND the refused element as an empty
		// string — an empty egress rule, worse than a dropped one.
		{"rules, number inline", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [1, github.com]\n", []string{"github.com"}, "",
			func(r *ParseResult) []string { return r.File.Workflows[0].Sandbox.Network.Rules }},
		{"rules, number dash form", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules:\n        - 1\n        - github.com\n", []string{"github.com"}, "",
			func(r *ParseResult) []string { return r.File.Workflows[0].Sandbox.Network.Rules }},
		{"mounts, list element inline", "workflow w:\n  entry: done\n  sandbox:\n    image: \"img\"\n    mounts: [[a], \"/x:/x\"]\n", []string{"/x:/x"}, "",
			func(r *ParseResult) []string { return r.File.Workflows[0].Sandbox.Mounts }},
		// A refused element that opens a bracket is skipped whole, and the
		// element after it is read.
		{"tools, nested bracket", "agent a:\n  tools: [[a], bash]\n", []string{"bash"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].Tools }},
		{"skills, nested brace", "agent a:\n  skills: [{a: b}, house.style]\n", []string{"house.style"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].Skills }},
		// A string list used to append the refused token's text (`1`) as a
		// string beside the diagnostic.
		{"images, number", "agent a:\n  images: [1, \"x.png\"]\n", []string{"x.png"}, "delete the element",
			func(r *ParseResult) []string { return r.File.Agents[0].Images }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 1 {
				t.Fatalf("want exactly one diagnostic, got %v", res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Code != DiagExpectedToken || !strings.Contains(strings.ToLower(d.Hint), c.hint) {
				t.Fatalf("got %s %q / hint %q", d.Code, d.Message, d.Hint)
			}
			if c.hint != "" && !strings.Contains(d.Message, "in the list") {
				t.Fatalf("got %s %q", d.Code, d.Message)
			}
			if got := c.pick(res); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("the other elements were not read: %v, want %v", got, c.want)
			}
		})
	}
}

// A refused fallback action takes the rest of its line with it: `action: 123
// 456` is ONE diagnostic, never one plus a phantom route property `456`.
func TestARefusedFallbackActionTakesItsLine(t *testing.T) {
	res := Parse("x.bot", "agent a:\n  model: \"m\"\n  fallbacks:\n    r:\n      on: [any]\n      action: 123 456\n      metered: true\n")
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Line != 6 {
		t.Fatalf("want one diagnostic on line 6, got %v", res.Diagnostics)
	}
	if fd := res.File.Agents[0].Fallbacks[0]; fd.Action != "" || !fd.Metered {
		t.Fatalf("the route after the refusal: %+v", fd)
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
