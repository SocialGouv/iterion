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
// it, a global one (no workflow) has none, and a
// group-expanded node (`<prefix>.<name>`) points at the group member it was
// cloned from.
func (c *compiler) attachPositions() {
	if c.file == nil {
		return
	}
	nodePos := map[string]ast.Pos{}
	c.eachNodeDecl(func(id string, sp ast.Span, _ ast.PropertySpans) {
		if sp.Start.Line == 0 {
			return
		}
		if _, seen := nodePos[id]; !seen {
			nodePos[id] = sp.Start
		}
	})

	f := c.file
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

// eachNodeDecl visits every node declaration of the file under the id the
// compiled graph knows it by, with its header span and its per-property
// span table, in declaration order; the first declaration of an id comes
// first. A group member instantiated by a use is visited under its
// instance id: expandGroups clones it into the file before any validator
// runs, so the file's own lists hold every node the graph has.
func (c *compiler) eachNodeDecl(visit func(id string, sp ast.Span, props ast.PropertySpans)) {
	f := c.file
	if f == nil {
		return
	}
	for _, d := range f.Agents {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Judges {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Routers {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Humans {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Tools {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Computes {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Emits {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Waits {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.AwaitAnswers {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Fails {
		visit(d.Name, d.Span, d.Properties)
	}
	for _, d := range f.Subbots {
		visit(d.Name, d.Span, d.Properties)
	}
}

// propertySpan is the span of the property prop of the declaration a
// validator names as (scope, id) — the workflow's when scope is "workflow",
// the node's otherwise: the line the author wrote the value on, for a
// diagnostic about that value (#1094). False when no span was recorded for
// it: a declaration the parser did not build (the JSON transport), a
// property the declaration does not write, a compiler without a file.
func (c *compiler) propertySpan(scope, id, prop string) (ast.Span, bool) {
	if c.file == nil {
		return ast.Span{}, false
	}
	if scope == "workflow" {
		for _, wf := range c.file.Workflows {
			if wf.Name == id {
				return wf.Properties.Lookup(prop)
			}
		}
		return ast.Span{}, false
	}
	if c.nodeTables == nil {
		c.nodeTables = map[string]ast.PropertySpans{}
		c.eachNodeDecl(func(id string, _ ast.Span, props ast.PropertySpans) {
			if _, seen := c.nodeTables[id]; !seen {
				c.nodeTables[id] = props
			}
		})
	}
	return c.nodeTables[id].Lookup(prop)
}

// emitAtProperty attributes a diagnostic about the value of property prop
// of the declaration (scope, id) to that property's line and names the
// property. Where the parser recorded no span for it — the declaration does
// not write the property (a value it inherits from the workflow), or was
// not built by the parser — the diagnostic goes to the declaration (the
// workflow's header, or the node by its id through attachPositions) and
// names no property: a property the declaration has no line for is not
// claimed.
func (c *compiler) emitAtProperty(sev Severity, code DiagCode, scope, id, prop string, format string, args ...any) {
	sp, ok := c.propertySpan(scope, id, prop)
	if !ok {
		prop = ""
	}
	nodeID := id
	if scope == "workflow" {
		nodeID = ""
		if sp.Start.Line == 0 {
			sp = c.workflowSpan(id)
		}
	}
	c.emit(sev, code, nodeID, "", sp, prop, "", format, args...)
}

// firstWrittenProperty is, of the properties named, the one the workflow
// writes first in its source — by line, then by column, for a document that
// writes several on one line — what a diagnostic about several of its lines
// points at; the first named when none has a recorded line.
func (c *compiler) firstWrittenProperty(workflow string, props []string) string {
	best, bestAt := props[0], ast.Pos{}
	for _, prop := range props {
		sp, ok := c.propertySpan("workflow", workflow, prop)
		if !ok {
			continue
		}
		if bestAt.Line == 0 || sp.Start.Line < bestAt.Line || (sp.Start.Line == bestAt.Line && sp.Start.Column < bestAt.Column) {
			best, bestAt = prop, sp.Start
		}
	}
	return best
}

// errorfAtProperty is emitAtProperty for an error.
func (c *compiler) errorfAtProperty(code DiagCode, scope, id, prop string, format string, args ...any) {
	c.emitAtProperty(SeverityError, code, scope, id, prop, format, args...)
}

// warnfAtProperty is emitAtProperty for a warning.
func (c *compiler) warnfAtProperty(code DiagCode, scope, id, prop string, format string, args ...any) {
	c.emitAtProperty(SeverityWarning, code, scope, id, prop, format, args...)
}

// workflowSpan is the source span of the workflow declaration named `name`
// — its `workflow <name>:` header — for a diagnostic about the declaration
// raised from the compiled Workflow, where the AST is out of reach; one
// about a property's value reads propertySpan. Zero when the compiler has
// no file: the diagnostic then carries no position, as a global one does.
func (c *compiler) workflowSpan(name string) ast.Span {
	if c.file == nil {
		return ast.Span{}
	}
	for _, wf := range c.file.Workflows {
		if wf.Name == name {
			return wf.Span
		}
	}
	return ast.Span{}
}

// mcpServerSpan is the source span of the top-level `mcp_server <name>:`
// declaration, for a diagnostic raised on a compiled server. Zero when the
// compiler has no file or the server was declared inline (an `mcp:` block's
// own entry), which keeps no span of its own.
func (c *compiler) mcpServerSpan(name string) ast.Span {
	if c.file == nil {
		return ast.Span{}
	}
	for _, d := range c.file.MCPServers {
		if d.Name == name {
			return d.Span
		}
	}
	return ast.Span{}
}
