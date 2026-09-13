package ir

import (
	"reflect"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// expandGroups instantiates every `use <group> as <prefix>` by cloning the
// group's nodes with `<prefix>.<name>` IDs, rewiring its internal edges, and
// substituting `{{params.X}}` with the bound values — all BEFORE the node
// compile passes run, so a group is a pure compile-time macro that never
// reaches the IR or runtime. External edges (authored in the workflow) address
// an instance's nodes via the dotted reference `prefix.node`. Cross-instance ID
// collisions are caught downstream by validateNodeNames (duplicate node ID).
func (c *compiler) expandGroups() {
	if len(c.file.Uses) == 0 {
		return
	}
	groups := make(map[string]*ast.GroupDecl, len(c.file.Groups))
	names := make(map[string]map[string]bool, len(c.file.Groups))
	for _, g := range c.file.Groups {
		groups[g.Name] = g
		names[g.Name] = groupNodeNames(g) // compute the node-name set once per group
	}
	var wf *ast.WorkflowDecl
	if len(c.file.Workflows) > 0 {
		wf = c.file.Workflows[0]
	}
	c.groupPromptTemplates = make(map[string]bool)
	prefixes := make(map[string]bool, len(c.file.Uses))
	for _, use := range c.file.Uses {
		g, ok := groups[use.Group]
		if !ok {
			c.errorf(DiagUseUnknownGroup, "use references unknown group %q", use.Group)
			continue
		}
		// A group with no node expands to nothing, and nothing else says so
		// unless a node of the instance is referenced: the group is a
		// declaration the studio has not filled in yet, or its body landed
		// at the wrong indentation after a blank line.
		if len(g.Agents)+len(g.Judges)+len(g.Routers)+len(g.Humans)+len(g.Tools)+len(g.Computes) == 0 {
			c.warnfAtSpan(DiagEmptyGroupUse, use.Span,
				"use %q as %q expands an empty group: %q declares no node, so the instance is nothing",
				use.Group, use.Prefix, use.Group)
		}
		// Two `use` blocks with one prefix would expand to the same node
		// ids; reported HERE, on the repeated `use` line — the line to
		// change — rather than as duplicate ids positioned on the group
		// body, which both instances share and which is not the mistake.
		if prefixes[use.Prefix] {
			c.errorfAtSpan(DiagDuplicateNodeID, use.Span,
				"use %q as %q: prefix %q is already used by an earlier `use` — every instantiation needs its own prefix",
				use.Group, use.Prefix, use.Prefix)
			continue
		}
		prefixes[use.Prefix] = true
		binds := c.bindGroupParams(g, use)
		c.instantiateGroup(g, names[use.Group], use.Prefix, binds, wf)
	}
	c.retainConcreteGroupPrompts()
}

// bindGroupParams maps each declared param to its bound value from the use's
// `with {}` block, flagging unknown keys and missing params (C117).
func (c *compiler) bindGroupParams(g *ast.GroupDecl, use *ast.UseDecl) map[string]string {
	declared := make(map[string]bool, len(g.Params))
	for _, p := range g.Params {
		declared[p] = true
	}
	binds := make(map[string]string, len(use.With))
	for _, w := range use.With {
		if !declared[w.Key] {
			c.errorf(DiagUseParamMismatch, "use %q as %q: unknown parameter %q (group declares: %s)",
				use.Group, use.Prefix, w.Key, strings.Join(g.Params, ", "))
			continue
		}
		binds[w.Key] = w.Value
	}
	for _, p := range g.Params {
		if _, ok := binds[p]; !ok {
			c.errorf(DiagUseParamMismatch, "use %q as %q: missing required parameter %q", use.Group, use.Prefix, p)
		}
	}
	return binds
}

// instantiateGroup clones the group's nodes + internal edges under the given
// prefix into the file's top-level slices and the workflow's edge list.
// internal is the group's node-name set, precomputed once by expandGroups.
func (c *compiler) instantiateGroup(g *ast.GroupDecl, internal map[string]bool, prefix string, binds map[string]string, wf *ast.WorkflowDecl) {
	pid := func(name string) string {
		if internal[name] {
			return prefix + "." + name
		}
		return name // terminals (done/fail) and external refs stay as-is
	}
	x := newGroupExpansion(c, internal, prefix, binds)
	copy := x.clone(reflect.ValueOf(g), "", "").Interface().(*ast.GroupDecl)
	for _, n := range copy.Agents {
		n.Name = pid(n.Name)
	}
	for _, n := range copy.Judges {
		n.Name = pid(n.Name)
	}
	for _, n := range copy.Routers {
		n.Name = pid(n.Name)
	}
	for _, n := range copy.Humans {
		n.Name = pid(n.Name)
	}
	for _, n := range copy.Tools {
		n.Name = pid(n.Name)
	}
	for _, n := range copy.Computes {
		n.Name = pid(n.Name)
	}
	c.file.Agents = append(c.file.Agents, copy.Agents...)
	c.file.Judges = append(c.file.Judges, copy.Judges...)
	c.file.Routers = append(c.file.Routers, copy.Routers...)
	c.file.Humans = append(c.file.Humans, copy.Humans...)
	c.file.Tools = append(c.file.Tools, copy.Tools...)
	c.file.Computes = append(c.file.Computes, copy.Computes...)
	if wf != nil {
		for _, e := range copy.Edges {
			e.From, e.To = pid(e.From), pid(e.To)
			wf.Edges = append(wf.Edges, e)
		}
	}
}

// substParams replaces every `{{params.<key>}}` marker in s with its bound
// value, in ONE pass over s: the scan walks the source once and a
// substituted value is never re-examined, so a bound value that reads like
// another parameter reference is inserted verbatim. That also makes the
// result independent of the bind map's iteration order.
//
// A `{{params.x}}` with no binding — and any other `{{...}}` namespace —
// is copied through unchanged, leaving the reference validator to report
// it. An unterminated `{{` ends the scan with the remainder copied as-is.
func substParams(s string, binds map[string]string) string {
	if s == "" || !strings.Contains(s, "{{params.") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '{' && s[i+1] == '{' {
			end := strings.Index(s[i:], "}}")
			if end == -1 {
				b.WriteString(s[i:])
				return b.String()
			}
			raw := s[i : i+end+2]
			if key, ok := strings.CutPrefix(raw[2:len(raw)-2], "params."); ok {
				if v, bound := binds[key]; bound {
					b.WriteString(v)
					i += end + 2
					continue
				}
			}
			b.WriteString(raw)
			i += end + 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// groupNodeNames returns the set of node names declared inside a group.
func groupNodeNames(g *ast.GroupDecl) map[string]bool {
	set := make(map[string]bool)
	for _, a := range g.Agents {
		set[a.Name] = true
	}
	for _, j := range g.Judges {
		set[j.Name] = true
	}
	for _, r := range g.Routers {
		set[r.Name] = true
	}
	for _, h := range g.Humans {
		set[h.Name] = true
	}
	for _, t := range g.Tools {
		set[t.Name] = true
	}
	for _, cd := range g.Computes {
		set[cd.Name] = true
	}
	return set
}
