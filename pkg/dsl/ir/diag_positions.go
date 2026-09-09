package ir

import (
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// attachPositions stamps a source position onto every diagnostic that names
// a node or an edge: the declaration's own `<kind> <name>:` line for a node,
// the edge's line for an edge. Compile diagnostics are produced from the
// AST, whose spans the parser already records, so the position costs one
// lookup — and it is what turns "error [C019]" into a location an editor,
// an agent or `iterion validate` can jump to.
//
// Best-effort by design: a diagnostic that already carries a position keeps
// it, a global one (no workflow, duplicate loop name) has none, and a
// group-expanded node (`<prefix>.<name>`) points at the group member it was
// cloned from.
func (c *compiler) attachPositions() {
	if c.file == nil {
		return
	}
	nodePos := map[string]ast.Pos{}
	add := func(name string, sp ast.Span) {
		if name == "" || sp.Start.Line == 0 {
			return
		}
		if _, seen := nodePos[name]; !seen {
			nodePos[name] = sp.Start
		}
	}
	f := c.file
	for _, d := range f.Agents {
		add(d.Name, d.Span)
	}
	for _, d := range f.Judges {
		add(d.Name, d.Span)
	}
	for _, d := range f.Routers {
		add(d.Name, d.Span)
	}
	for _, d := range f.Humans {
		add(d.Name, d.Span)
	}
	for _, d := range f.Tools {
		add(d.Name, d.Span)
	}
	for _, d := range f.Computes {
		add(d.Name, d.Span)
	}
	for _, d := range f.Emits {
		add(d.Name, d.Span)
	}
	for _, d := range f.Waits {
		add(d.Name, d.Span)
	}
	for _, d := range f.AwaitAnswers {
		add(d.Name, d.Span)
	}
	for _, d := range f.Fails {
		add(d.Name, d.Span)
	}
	for _, d := range f.Subbots {
		add(d.Name, d.Span)
	}
	for _, u := range f.Uses {
		for _, g := range f.Groups {
			if g.Name != u.Group {
				continue
			}
			for _, d := range g.Agents {
				add(u.Prefix+"."+d.Name, d.Span)
			}
			for _, d := range g.Judges {
				add(u.Prefix+"."+d.Name, d.Span)
			}
			for _, d := range g.Routers {
				add(u.Prefix+"."+d.Name, d.Span)
			}
			for _, d := range g.Humans {
				add(u.Prefix+"."+d.Name, d.Span)
			}
			for _, d := range g.Tools {
				add(u.Prefix+"."+d.Name, d.Span)
			}
			for _, d := range g.Computes {
				add(u.Prefix+"."+d.Name, d.Span)
			}
		}
	}

	edgePos := map[string]ast.Pos{}
	for _, w := range f.Workflows {
		for _, e := range w.Edges {
			if e.Span.Start.Line == 0 {
				continue
			}
			id := e.From + "->" + e.To
			if _, seen := edgePos[id]; !seen {
				edgePos[id] = e.Span.Start
			}
		}
	}

	for i := range c.diags {
		d := &c.diags[i]
		if d.Line != 0 {
			continue
		}
		if d.EdgeID != "" {
			if p, ok := edgePos[d.EdgeID]; ok {
				d.File, d.Line, d.Column = p.File, p.Line, p.Column
				continue
			}
		}
		if p, ok := nodePos[d.NodeID]; ok {
			d.File, d.Line, d.Column = p.File, p.Line, p.Column
		}
	}

	// Several validators walk maps, so two compilations of one file used to
	// list the same findings in a different order. Positioned findings read
	// in source order; the global ones (no position) go LAST, since they are
	// usually consequences of a positioned one (`entry node not found`
	// because the declaration above failed); a tie on the position is broken
	// by code, node, edge and message — every field a finding has — so the
	// order never depends on map iteration and a `--json` diff of two runs
	// is quiet.
	sort.SliceStable(c.diags, func(i, j int) bool {
		a, b := c.diags[i], c.diags[j]
		if (a.Line == 0) != (b.Line == 0) {
			return a.Line != 0
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if a.EdgeID != b.EdgeID {
			return a.EdgeID < b.EdgeID
		}
		return a.Message < b.Message
	})
}
