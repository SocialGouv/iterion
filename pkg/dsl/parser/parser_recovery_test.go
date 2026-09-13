package parser

import (
	"strings"
	"testing"
)

// A misspelt block header anywhere — a node body, a nested block — draws ONE
// diagnostic, and its indented body goes with it: the body's lines are not
// re-read as the enclosing kind's properties (an E003 per line, then a C003
// at compile for a `user:` taken as the agent's prompt reference), and the
// members after it still parse.
func TestAMisspeltBlockHeaderDropsItsBodyEverywhere(t *testing.T) {
	cases := []struct {
		name, src, hint string
		check           func(t *testing.T, res *ParseResult)
	}{
		{"agent body", "agent a:\n  model: \"m\"\n  sandbx:\n    image: \"i\"\n    user: \"u\"\n  system: s\n", "Did you mean `sandbox`?",
			func(t *testing.T, res *ParseResult) {
				if got := res.File.Agents[0].System; got != "s" {
					t.Errorf("the member after the block did not parse: system = %q", got)
				}
			}},
		{"tool body", "tool t:\n  command: \"c\"\n  recovry:\n    max_repair_attempts: 2\n  description: \"d\"\n", "Did you mean `recovery`?",
			func(t *testing.T, res *ParseResult) {
				if got := res.File.Tools[0].Description; got != "d" {
					t.Errorf("the member after the block did not parse: description = %q", got)
				}
			}},
		{"router body (peeked site)", "router r:\n  mode: condition\n  bogus:\n    x: 1\n    y: 2\n  description: \"d\"\n", "router accepts:",
			func(t *testing.T, res *ParseResult) {
				if got := res.File.Routers[0].Description; got != "d" {
					t.Errorf("the member after the block did not parse: description = %q", got)
				}
			}},
		{"nested block", "workflow w:\n  entry: a\n  sandbox:\n    image: \"i\"\n    netwrk:\n      mode: allowlist\n    user: \"u\"\n", "Did you mean `network`?",
			func(t *testing.T, res *ParseResult) {
				if got := res.File.Workflows[0].Sandbox.User; got != "u" {
					t.Errorf("the member after the nested block did not parse: user = %q", got)
				}
			}},
		{"secret entry", "secrets:\n  s:\n    valu:\n      x: 1\n    optional: true\n", "Did you mean `value`?",
			func(t *testing.T, res *ParseResult) {
				if !res.File.Secrets.Fields[0].Optional {
					t.Errorf("the member after the block did not parse: optional is false")
				}
			}},
	}
	for _, c := range cases {
		res := Parse("t.bot", c.src)
		if len(res.Diagnostics) != 1 {
			t.Errorf("%s: want exactly one diagnostic, got %v", c.name, res.Diagnostics)
			continue
		}
		d := res.Diagnostics[0]
		if d.Code != DiagUnknownProperty || !strings.Contains(d.Hint, c.hint) {
			t.Errorf("%s: got %s %q (hint %q)", c.name, d.Code, d.Message, d.Hint)
		}
		c.check(t, res)
	}
}

// A lexer diagnosis inside a dropped body is still reported: the tab is the
// author's mistake, and swallowing it would only make it resurface after the
// header is fixed.
func TestALexerErrorInsideADroppedBodyIsStillReported(t *testing.T) {
	res := Parse("t.bot", "agent a:\n  model: \"m\"\n  sandbx:\n    image: \"i\"\n\tuser: \"u\"\n  system: s\n")
	var codes []string
	for _, d := range res.Diagnostics {
		codes = append(codes, string(d.Code))
	}
	if strings.Join(codes, ",") != "E012,E003" {
		t.Fatalf("want E012 (the header) then E003 (the tab), got %v", res.Diagnostics)
	}
	if got := res.File.Agents[0].System; got != "s" {
		t.Fatalf("the member after the block did not parse: system = %q", got)
	}
}

// A declaration a group cannot hold is refused by name, not read as the
// source of an edge that lacks its arrow — while a node NAMED like a keyword
// is still a valid edge endpoint.
func TestADeclarationAGroupCannotHoldIsRefusedByName(t *testing.T) {
	res := Parse("g.bot", "group g:\n  emit e:\n    event: \"x\"\n  agent a:\n    model: \"m\"\n")
	if len(res.Diagnostics) != 1 {
		t.Fatalf("want one diagnostic, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != DiagUnexpectedToken || !strings.Contains(d.Message, "'emit' cannot be declared inside a group") {
		t.Fatalf("got %s %q", d.Code, d.Message)
	}
	if !strings.Contains(d.Hint, "Move the `emit` declaration to the top level") {
		t.Fatalf("the remedy is the catalogue's, not this site's: %q", d.Hint)
	}
	if len(res.File.Groups) != 1 || len(res.File.Groups[0].Agents) != 1 {
		t.Fatalf("the member after the refused declaration did not parse: %+v", res.File.Groups)
	}
	res = Parse("g.bot", "group g:\n  agent emit:\n    model: \"m\"\n  emit -> done\n")
	if len(res.Diagnostics) != 0 || len(res.File.Groups[0].Edges) != 1 {
		t.Fatalf("an edge from a node named `emit` must still parse: %v, %d edges", res.Diagnostics, len(res.File.Groups[0].Edges))
	}
}
