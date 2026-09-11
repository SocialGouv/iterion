package parser

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func edgesWithoutSpans(edges []*ast.Edge) []ast.Edge {
	out := make([]ast.Edge, 0, len(edges))
	for _, e := range edges {
		c := *e
		c.Span = ast.Span{}
		out = append(out, c)
	}
	return out
}

// `a -> b -> c` is the chain everyone writes in prose and in READMEs; it
// reads as the N edges it names, in a workflow and in a group, dotted
// endpoints included, and the clauses at the end of the line belong to the
// LAST segment — exactly what the explicit lines would say.
func TestAChainOfArrowsReadsAsItsEdges(t *testing.T) {
	cases := []struct {
		name, chained, explicit string
	}{
		{"three plain", "a -> b -> c\n", "a -> b\nb -> c\n"},
		{"clauses on the last segment", "a -> b -> c when ok as fix(3) with { k: \"v\" }\n", "a -> b\nb -> c when ok as fix(3) with { k: \"v\" }\n"},
		{"else on the last segment", "a -> b -> done else\n", "a -> b\nb -> done else\n"},
		{"dotted endpoints", "r1.a -> r1.b -> c\n", "r1.a -> r1.b\nr1.b -> c\n"},
		{"four", "a -> b -> c -> done\n", "a -> b\nb -> c\nc -> done\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			head := "workflow w:\n  entry: a\n  "
			got := Parse("c.bot", head+strings.ReplaceAll(c.chained, "\n", "\n  "))
			want := Parse("e.bot", head+strings.ReplaceAll(c.explicit, "\n", "\n  "))
			if len(got.Diagnostics) != 0 || len(want.Diagnostics) != 0 {
				t.Fatalf("diagnostics chained=%v explicit=%v", got.Diagnostics, want.Diagnostics)
			}
			// The span-free JSON mirror is the oracle: the clauses carry
			// positions, which legitimately differ between the two texts.
			ja, _ := ast.MarshalFile(got.File)
			jb, _ := ast.MarshalFile(want.File)
			if !bytes.Equal(ja, jb) {
				t.Fatalf("chained reads as\n%s\nexplicit reads as\n%s", ja, jb)
			}
			// Each segment keeps its own position: the source of the second
			// edge is the token `b`, on the same line.
			if e := got.File.Workflows[0].Edges; len(e) > 1 && e[1].Span.Start.Column <= e[0].Span.Start.Column {
				t.Fatalf("second segment's span does not start at its own source: %+v", e[1].Span)
			}
		})
	}
	// The same chain inside a group.
	src := "group g:\n  agent a:\n    description: \"x\"\n  agent b:\n    description: \"y\"\n  agent c:\n    description: \"z\"\n  a -> b -> c when ok\n"
	res := Parse("g.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("group: %v", res.Diagnostics)
	}
	e := res.File.Groups[0].Edges
	if len(e) != 2 || e[0].From != "a" || e[0].To != "b" || e[0].When != nil || e[1].From != "b" || e[1].To != "c" || e[1].When == nil {
		t.Fatalf("group edges: %+v", edgesWithoutSpans(e))
	}
}

// A clause in the middle of a chain is refused by name (E032): the clauses
// belong to the last segment, and guessing which segment the author meant
// would be a program the author did not write. One diagnostic, the line is
// dropped, the next line is read.
func TestAClauseBeforeAnArrowIsRefused(t *testing.T) {
	src := "workflow w:\n  entry: a\n  a -> b when ok -> c\n  b -> done\n"
	res := Parse("c.bot", src)
	if len(res.Diagnostics) != 1 {
		t.Fatalf("want one diagnostic, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != DiagClauseBeforeArrow || d.Line != 3 || !strings.Contains(d.Hint, "LAST segment") {
		t.Fatalf("got %s at line %d (%q), want E032 at line 3 with the remedy", d.Code, d.Line, d.Hint)
	}
	e := res.File.Workflows[0].Edges
	if len(e) != 1 || e[0].From != "b" {
		t.Fatalf("the line after the refused chain was not read as its own edge: %+v", edgesWithoutSpans(e))
	}
}

// A number or a bool in a `with { … }` map is the string it spells: the
// mapping carries text into the target's input, and quoting a `3` was the
// one thing every author forgot.
func TestWithMapValuesMayBeBareLiterals(t *testing.T) {
	bare := "workflow w:\n  entry: a\n  a -> b with { n: 3, f: 1.5, ok: true, no: false, s: \"x\" }\n"
	quoted := "workflow w:\n  entry: a\n  a -> b with { n: \"3\", f: \"1.5\", ok: \"true\", no: \"false\", s: \"x\" }\n"
	got, want := Parse("b.bot", bare), Parse("q.bot", quoted)
	if len(got.Diagnostics) != 0 || len(want.Diagnostics) != 0 {
		t.Fatalf("diagnostics bare=%v quoted=%v", got.Diagnostics, want.Diagnostics)
	}
	g, w := got.File.Workflows[0].Edges[0].With, want.File.Workflows[0].Edges[0].With
	if len(g) != 5 {
		t.Fatalf("bare map read %d entries", len(g))
	}
	for i := range g {
		if g[i].Key != w[i].Key || g[i].Value != w[i].Value {
			t.Fatalf("entry %d: bare %q=%q, quoted %q=%q", i, g[i].Key, g[i].Value, w[i].Key, w[i].Value)
		}
	}
	// The same map on a group instantiation.
	use := "group g(n):\n  agent a:\n    description: \"{{params.n}}\"\n\nuse g as r1 with { n: 3 }\n"
	res := Parse("u.bot", use)
	if len(res.Diagnostics) != 0 || len(res.File.Uses) != 1 || len(res.File.Uses[0].With) != 1 || res.File.Uses[0].With[0].Value != "3" {
		t.Fatalf("use … with: %v %+v", res.Diagnostics, res.File.Uses)
	}
	// An identifier is still not a value: it would read as a reference.
	bad := Parse("x.bot", "workflow w:\n  entry: a\n  a -> b with { n: three }\n")
	if len(bad.Diagnostics) == 0 {
		t.Fatalf("a bare identifier was accepted as a with value")
	}
}
