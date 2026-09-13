package parser

import (
	"reflect"
	"strings"
	"testing"
)

// A list may be written the way YAML taught everyone — one `- item` per
// line under the property — and reads as the same list as the inline
// `[a, b]`, on every list path and in every profile.
func TestDashListReadsAsTheInlineList(t *testing.T) {
	inline := "agent a:\n  description: \"x\"\n  tools: [bash, mcp.srv.*, \"kebab-tool\"]\n  skills: [\"changelog-writer\", house.style]\n  needs: [godot, blender]\n\nsupervisor s:\n  watches: [a]\n  monitors: [\"tool_error\", \"cost>3\"]\n  system: p\n\nprompt p:\n  Watch.\n\nworkflow w:\n  entry: a\n  allow: [\"Read(**)\", \"Bash(go test:*)\"]\n  sandbox:\n    network:\n      mode: allowlist\n      rules: [\"!**.evil.site\", github]\n  a -> done\n"
	dash := "agent a:\n  description: \"x\"\n  tools:\n    - bash\n    - mcp.srv.*\n    ## a comment between items\n    - \"kebab-tool\"\n  skills:\n    - \"changelog-writer\"\n    - house.style\n  needs:\n    - godot\n    - blender\n\nsupervisor s:\n  watches:\n    - a\n  monitors:\n    - \"tool_error\"\n    - \"cost>3\"\n  system: p\n\nprompt p:\n  Watch.\n\nworkflow w:\n  entry: a\n  allow:\n    - \"Read(**)\"\n    - \"Bash(go test:*)\"\n  sandbox:\n    network:\n      mode: allowlist\n      rules:\n        - \"!**.evil.site\"\n        - github\n  a -> done\n"
	for _, profile := range []string{"", "dsl: 2\n"} {
		want := Parse("i.bot", profile+inline)
		got := Parse("d.bot", profile+dash)
		if len(want.Diagnostics) != 0 || len(got.Diagnostics) != 0 {
			t.Fatalf("profile %q: diagnostics inline=%v dash=%v", profile, want.Diagnostics, got.Diagnostics)
		}
		ag, ad := want.File.Agents[0], got.File.Agents[0]
		if !reflect.DeepEqual(ag.Tools, ad.Tools) || !reflect.DeepEqual(ag.Skills, ad.Skills) || !reflect.DeepEqual(ag.Needs, ad.Needs) {
			t.Fatalf("profile %q: agent lists differ: %+v vs %+v", profile, ag, ad)
		}
		sg, sd := want.File.Supervisors[0], got.File.Supervisors[0]
		if !reflect.DeepEqual(sg.Watches, sd.Watches) || !reflect.DeepEqual(sg.Monitors, sd.Monitors) {
			t.Fatalf("profile %q: supervisor lists differ: %+v vs %+v", profile, sg, sd)
		}
		wg, wd := want.File.Workflows[0], got.File.Workflows[0]
		if !reflect.DeepEqual(wg.Allow, wd.Allow) || !reflect.DeepEqual(wg.Sandbox.Network.Rules, wd.Sandbox.Network.Rules) {
			t.Fatalf("profile %q: workflow lists differ", profile)
		}
		if len(ad.Tools) != 3 || ad.Tools[1] != "mcp.srv.*" || len(wd.Sandbox.Network.Rules) != 2 {
			t.Fatalf("dash lists did not read every element: %v / %v", ad.Tools, wd.Sandbox.Network.Rules)
		}
	}
}

// The mistakes around the form draw ONE diagnostic each, named: a property
// left without its list, a `- item` block under a single-valued property,
// an element indented deeper, and something after the element on its line.
func TestDashListMistakesDrawOneDiagnosticEach(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		code  DiagCode
		msg   string
		agent bool // the agent's next property must still be read
	}{
		{"no list at all", "agent a:\n  tools:\n  description: \"x\"\n", DiagExpectedToken, "expected a list", true},
		{"list under a scalar", "agent a:\n  description:\n    - one\n    - two\n  model: \"m\"\n", DiagExpectedToken, "takes a single value, not a `- item` list", true},
		{"deeper item", "agent a:\n  tools:\n    - bash\n      - grep\n  description: \"x\"\n", DiagBadIndentation, "nothing may be indented deeper", true},
		{"two on a line", "agent a:\n  tools:\n    - bash grep\n  description: \"x\"\n", DiagUnexpectedToken, "one `- item` per line", true},
		{"list under a scalar, comment after the colon", "agent a:\n  description: ## note\n    - one\n    - two\n  model: \"m\"\n", DiagExpectedToken, "takes a single value, not a `- item` list", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			if len(res.Diagnostics) != 1 {
				t.Fatalf("want exactly one diagnostic, got %v", res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Code != c.code || !strings.Contains(d.Message, c.msg) {
				t.Fatalf("got %s %q, want %s containing %q", d.Code, d.Message, c.code, c.msg)
			}
			if c.agent {
				a := res.File.Agents[0]
				if a.Description == "" && a.Model == "" {
					t.Fatalf("the property after the mistake was lost: %+v", a)
				}
			}
		})
	}
}

// A dash is a list-item opener only first on its line and followed by a
// space: an arrow is still an arrow, and text inside a prompt body, a block
// scalar or a raw string never becomes a token.
func TestDashIsATokenOnlyWhereItOpensAnItem(t *testing.T) {
	src := "prompt p:\n  - a bullet\n  - another\n\ntool t:\n  command: |\n    - not a list\n    echo -n x\n  description: `- raw`\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	if res.File.Prompts[0].Body != "- a bullet\n- another" {
		t.Fatalf("prompt body: %q", res.File.Prompts[0].Body)
	}
	if !strings.HasPrefix(res.File.Tools[0].Command, "- not a list\n") || res.File.Tools[0].Description != "- raw" {
		t.Fatalf("scalar text changed: %q / %q", res.File.Tools[0].Command, res.File.Tools[0].Description)
	}
	// A bare `-x` (no space) is still not a token of the language.
	bad := Parse("x.bot", "agent a:\n  tools:\n    -bash\n")
	if len(bad.Diagnostics) == 0 {
		t.Fatalf("`-bash` was accepted")
	}
	// Nor is a ` - ` in the middle of a line: the lexer refuses the
	// character itself (E001), it does not become an item opener.
	mid := Parse("x.bot", "agent a:\n  tools: [bash - grep]\n")
	var lexerRefusal bool
	for _, d := range mid.Diagnostics {
		if d.Code == DiagUnexpectedToken && strings.Contains(d.Message, "unexpected character") {
			lexerRefusal = true
		}
	}
	if !lexerRefusal {
		t.Fatalf("a mid-line dash was not refused by the lexer: %v", mid.Diagnostics)
	}
}

// A trailing `##` comment ends a line the way a newline does — after the
// property's colon and after an element — and the list still reads whole,
// on every list path and in every profile. The lexer emits the comment in
// place of the newline it consumes, which used to hide the block from the
// list readers and to refuse the `-` of the next item as an unexpected
// character.
func TestDashListSurvivesTrailingComments(t *testing.T) {
	src := "agent a:\n  description: \"x\"\n  tools: ## allowed\n    - bash ## the shell\n    - \"kebab-tool\" ## quoted\n    - grep\n  skills: ## s\n    - house.style ## c\n    - \"changelog-writer\"\n  needs: ## n\n    - godot ## g\n    - blender\n\nsupervisor s:\n  watches: ## w\n    - a ## the agent\n  monitors:\n    - \"tool_error\" ## e\n    - \"cost>3\"\n  system: p\n\nprompt p:\n  Watch.\n\nworkflow w:\n  entry: a\n  allow: ## rules\n    - \"Read(**)\" ## r\n    - \"Bash(go test:*)\"\n  sandbox:\n    network:\n      mode: allowlist\n      rules: ## hosts\n        - \"!**.evil.site\" ## no\n        - github\n  a -> done\n"
	for _, profile := range []string{"", "dsl: 2\n"} {
		res := Parse("x.bot", profile+src)
		if len(res.Diagnostics) != 0 {
			t.Fatalf("profile %q: %v", profile, res.Diagnostics)
		}
		a := res.File.Agents[0]
		if !reflect.DeepEqual(a.Tools, []string{"bash", "kebab-tool", "grep"}) || !reflect.DeepEqual(a.Skills, []string{"house.style", "changelog-writer"}) || !reflect.DeepEqual(a.Needs, []string{"godot", "blender"}) {
			t.Fatalf("profile %q: agent lists: tools=%v skills=%v needs=%v", profile, a.Tools, a.Skills, a.Needs)
		}
		s := res.File.Supervisors[0]
		if !reflect.DeepEqual(s.Watches, []string{"a"}) || !reflect.DeepEqual(s.Monitors, []string{"tool_error", "cost>3"}) {
			t.Fatalf("profile %q: supervisor lists: watches=%v monitors=%v", profile, s.Watches, s.Monitors)
		}
		w := res.File.Workflows[0]
		if len(w.Allow) != 2 || len(w.Sandbox.Network.Rules) != 2 || w.Sandbox.Network.Rules[1] != "github" {
			t.Fatalf("profile %q: workflow lists: allow=%v rules=%v", profile, w.Allow, w.Sandbox.Network.Rules)
		}
	}
}
