package parser

import (
	"slices"
	"strings"
	"testing"
)

// `project_root:` is profile 1's: profile 2 refuses it by name, at its own
// line, without a cascade and without setting it — and the remedy does not
// pretend `visibility:` is a drop-in replacement (the two are different
// axes; C171 only refuses their combination).
func TestProjectRootIsRefusedInProfileTwo(t *testing.T) {
	body := "agent a:\n  description: \"x\"\n  memory:\n    enabled: true\n    project_root: true\n"
	v1 := Parse("x.bot", body)
	if len(v1.Diagnostics) != 0 {
		t.Fatalf("profile 1: %v", v1.Diagnostics)
	}
	if mb := v1.File.Agents[0].Memory; mb == nil || mb.ProjectRoot == nil || !*mb.ProjectRoot {
		t.Fatalf("profile 1 did not keep project_root: %+v", v1.File.Agents[0].Memory)
	}
	v2 := Parse("x.bot", "dsl: 2\n"+body)
	if len(v2.Diagnostics) != 1 {
		t.Fatalf("profile 2: want exactly one diagnostic, got %v", v2.Diagnostics)
	}
	d := v2.Diagnostics[0]
	if d.Code != DiagRemovedInProfile || d.Line != 6 {
		t.Fatalf("profile 2: got %s at line %d, want E043 at line 6", d.Code, d.Line)
	}
	if !strings.Contains(d.Message, "removed from dsl profile 2") || !strings.Contains(d.Hint, "not a drop-in replacement") {
		t.Fatalf("E043 message/hint: %q / %q", d.Message, d.Hint)
	}
	if mb := v2.File.Agents[0].Memory; mb == nil || mb.ProjectRoot != nil || mb.Enabled == nil {
		t.Fatalf("profile 2 kept project_root or lost its neighbour: %+v", mb)
	}
}

// `join` was a keyword with no rule behind it — a dead reserved word. It is
// an ordinary identifier now, in every profile: a node name, an edge
// endpoint, and (as ever) the expression built-in inside a string.
func TestJoinIsAnOrdinaryIdentifier(t *testing.T) {
	if slices.Contains(Keywords(), "join") {
		t.Fatalf("join is still a keyword")
	}
	src := "compute join:\n  expr:\n    lines: \"join(input.files, ',')\"\n\nworkflow w:\n  entry: join\n  join -> done\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	if res.File.Computes[0].Name != "join" || res.File.Workflows[0].Edges[0].From != "join" {
		t.Fatalf("join as a name did not survive: %+v", res.File.Workflows[0].Edges)
	}
}
