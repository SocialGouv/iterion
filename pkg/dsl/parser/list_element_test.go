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

// Every string|ident property that owns its line does the same: the tail of
// a refused value is never read as the next property, and the property after
// it is read. The `env:` map's block form used to make KEYS of the tail
// (`KEY1: 123 456` gave a key `456` and destroyed the next entry).
func TestARefusedStringOrIdentValueTakesItsLine(t *testing.T) {
	cases := []struct {
		name string
		src  string
		ok   func(r *ParseResult) bool
	}{
		{"human posture", "human h:\n  posture: 123 456\n  merge_strategy: squash\n", func(r *ParseResult) bool { return r.File.Humans[0].MergeStrategy == "squash" }},
		{"human merge_strategy", "human h:\n  merge_strategy: 123 456\n  merge_into: current\n", func(r *ParseResult) bool { return r.File.Humans[0].MergeInto == "current" }},
		{"human merge_into", "human h:\n  merge_into: 123 456\n  max_turns: 4\n", func(r *ParseResult) bool { return r.File.Humans[0].MaxTurns == 4 }},
		{"fail code", "fail f:\n  code: 123 456\n  message: \"m\"\n", func(r *ParseResult) bool { return r.File.Fails[0].Message == "m" }},
		{"await_answers from", "await_answers g:\n  from: 123 456\n  timeout: \"1s\"\n", func(r *ParseResult) bool { return r.File.AwaitAnswers[0].Timeout == "1s" }},
		{"tool connection", "tool t:\n  command: \"x\"\n  connection: 123 456\n  description: \"d\"\n", func(r *ParseResult) bool { return r.File.Tools[0].Description == "d" }},
		{"secret as", "secrets:\n  s:\n    as: 123 456\n    description: \"d\"\n", func(r *ParseResult) bool { return r.File.Secrets.Fields[0].Description == "d" }},
		{"secret env", "secrets:\n  s:\n    env: 123 456\n    description: \"d\"\n", func(r *ParseResult) bool { return r.File.Secrets.Fields[0].Description == "d" }},
		{"network preset", "workflow w:\n  entry: done\n  sandbox:\n    image: \"img\"\n    network:\n      preset: 123 456\n      rules: [github.com]\n", func(r *ParseResult) bool {
			return reflect.DeepEqual(r.File.Workflows[0].Sandbox.Network.Rules, []string{"github.com"})
		}},
		{"env block value", "workflow w:\n  entry: done\n  sandbox:\n    image: \"img\"\n    env:\n      KEY1: 123 456\n      KEY2: \"v2\"\n", func(r *ParseResult) bool {
			return reflect.DeepEqual(r.File.Workflows[0].Sandbox.Env, map[string]string{"KEY2": "v2"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 1 {
				t.Fatalf("want exactly one diagnostic, got %v", res.Diagnostics)
			}
			if !c.ok(res) {
				t.Fatalf("the property after the refusal was not read: %v", res.File)
			}
		})
	}
}

// A trailing comma closes the list: `[bash,]` is `[bash]`, as the JSON value
// form and the old string|ident list already read it. The list loop used to
// hand the `]` to the element reader, which refused it — "delete the element
// or write a name", about the closer the author wrote — and then missed it.
func TestATrailingCommaClosesTheList(t *testing.T) {
	for _, c := range []struct {
		name string
		src  string
		pick func(r *ParseResult) []string
	}{
		{"tools", "agent a:\n  tools: [bash,]\n  description: \"after\"\n", func(r *ParseResult) []string { return r.File.Agents[0].Tools }},
		{"needs", "agent a:\n  needs: [gpu,]\n  description: \"after\"\n", func(r *ParseResult) []string { return r.File.Agents[0].Needs }},
		{"images", "agent a:\n  images: [\"x.png\",]\n  description: \"after\"\n", func(r *ParseResult) []string { return r.File.Agents[0].Images }},
		{"rules", "workflow w:\n  entry: done\n  sandbox:\n    network:\n      rules: [github.com,]\n", func(r *ParseResult) []string { return r.File.Workflows[0].Sandbox.Network.Rules }},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 0 || len(c.pick(res)) != 1 {
				t.Fatalf("diagnostics %v, list %v", res.Diagnostics, c.pick(res))
			}
		})
	}
	// An empty element between two commas is still a refusal, said once,
	// and both neighbours are read.
	res := Parse("x.bot", "agent a:\n  tools: [bash, , read_file]\n")
	if got := res.File.Agents[0].Tools; !reflect.DeepEqual(got, []string{"bash", "read_file"}) || len(res.Diagnostics) == 0 {
		t.Fatalf("tools %v, diagnostics %v", got, res.Diagnostics)
	}
}

// In the `- item` form, a refused element that opens a bracket is said ONCE:
// its residue on the line is the same mistake, not a second one, and the
// next items are read.
func TestADashFormElementThatOpensABracketIsSaidOnce(t *testing.T) {
	res := Parse("x.bot", "supervisor s:\n  watches:\n    - a\n    - [b]\n    - c\n  cooldown: \"2m\"\n")
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Line != 4 {
		t.Fatalf("want one diagnostic on line 4, got %v", res.Diagnostics)
	}
	if s := res.File.Supervisors[0]; !reflect.DeepEqual(s.Watches, []string{"a", "c"}) || s.Cooldown != "2m" {
		t.Fatalf("watches %v, cooldown %q", s.Watches, s.Cooldown)
	}
	// A GOOD element followed by residue is still the mistake it was.
	res = Parse("x.bot", "supervisor s:\n  watches:\n    - a b\n")
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "one `- item` per line") {
		t.Fatalf("got %v", res.Diagnostics)
	}
}

// A list element with no comma before it is said once, then read; a stray
// token inside the list is refused where it stands; nothing runs into the
// next property. `[bash foo]` used to draw "expected ]" and then "unknown
// property ']'", and a resync that ran at depth 0 ate `bash` in silence.
func TestAMissingCommaInAListIsSaidAndTheElementsAreRead(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		want  []string
		diags int
	}{
		{"two names, no comma", "agent a:\n  tools: [bash foo]\n  description: \"after\"\n", []string{"bash", "foo"}, 1},
		{"refused then a name, no comma", "agent a:\n  tools: [1 bash]\n  description: \"after\"\n", []string{"bash"}, 2},
		{"refused then a stray closer", "agent a:\n  tools: [1}, bash]\n  description: \"after\"\n", []string{"bash"}, 3},
		{"needs, refused then a name", "agent a:\n  needs: [1 cpu]\n  description: \"after\"\n", []string{"cpu"}, 2},
		{"images, refused then a string", "agent a:\n  images: [1 \"x.png\"]\n  description: \"after\"\n", []string{"x.png"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != c.diags {
				t.Fatalf("want %d diagnostics, got %v", c.diags, res.Diagnostics)
			}
			for _, d := range res.Diagnostics {
				if d.Line != 2 {
					t.Fatalf("a diagnostic ran into another line: %v", d)
				}
			}
			a := res.File.Agents[0]
			got := a.Tools
			switch {
			case strings.Contains(c.name, "needs"):
				got = a.Needs
			case strings.Contains(c.name, "images"):
				got = a.Images
			}
			if !reflect.DeepEqual(got, c.want) || a.Description != "after" {
				t.Fatalf("list %v (want %v), description %q", got, c.want, a.Description)
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
