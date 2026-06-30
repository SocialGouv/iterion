package ir

import (
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
	for _, g := range c.file.Groups {
		groups[g.Name] = g
	}
	var wf *ast.WorkflowDecl
	if len(c.file.Workflows) > 0 {
		wf = c.file.Workflows[0]
	}
	for _, use := range c.file.Uses {
		g, ok := groups[use.Group]
		if !ok {
			c.errorf(DiagUseUnknownGroup, "use references unknown group %q", use.Group)
			continue
		}
		binds := c.bindGroupParams(g, use)
		c.instantiateGroup(g, use.Prefix, binds, wf)
	}
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
func (c *compiler) instantiateGroup(g *ast.GroupDecl, prefix string, binds map[string]string, wf *ast.WorkflowDecl) {
	internal := groupNodeNames(g)
	pid := func(name string) string {
		if internal[name] {
			return prefix + "." + name
		}
		return name // terminals (done/fail) and external refs stay as-is
	}
	subst := func(s string) string {
		if s == "" || !strings.Contains(s, "{{params.") {
			return s
		}
		for k, v := range binds {
			s = strings.ReplaceAll(s, "{{params."+k+"}}", v)
		}
		return s
	}

	for _, a := range g.Agents {
		na := *a
		na.Name = pid(a.Name)
		c.file.Agents = append(c.file.Agents, &na)
	}
	for _, j := range g.Judges {
		nj := *j
		nj.Name = pid(j.Name)
		c.file.Judges = append(c.file.Judges, &nj)
	}
	for _, r := range g.Routers {
		nr := *r
		nr.Name = pid(r.Name)
		nr.Over = subst(r.Over)
		c.file.Routers = append(c.file.Routers, &nr)
	}
	for _, h := range g.Humans {
		nh := *h
		nh.Name = pid(h.Name)
		nh.ReviewURL = subst(h.ReviewURL)
		c.file.Humans = append(c.file.Humans, &nh)
	}
	for _, t := range g.Tools {
		nt := *t
		nt.Name = pid(t.Name)
		nt.Command = subst(t.Command)
		nt.Script = subst(t.Script)
		nt.Goal = subst(t.Goal)
		nt.Postcondition = subst(t.Postcondition)
		c.file.Tools = append(c.file.Tools, &nt)
	}
	for _, cd := range g.Computes {
		nc := *cd
		nc.Name = pid(cd.Name)
		if len(cd.Expr) > 0 {
			nc.Expr = make([]*ast.ComputeExpr, len(cd.Expr))
			for i, e := range cd.Expr {
				ne := *e
				ne.Expr = subst(e.Expr)
				nc.Expr[i] = &ne
			}
		}
		c.file.Computes = append(c.file.Computes, &nc)
	}

	if wf == nil {
		return
	}
	for _, e := range g.Edges {
		ne := &ast.Edge{
			From: pid(e.From),
			To:   pid(e.To),
			Span: e.Span,
		}
		if e.When != nil {
			w := *e.When
			w.Expr = subst(e.When.Expr)
			ne.When = &w
		}
		if e.Loop != nil {
			l := *e.Loop
			l.MaxIterationsExpr = subst(e.Loop.MaxIterationsExpr)
			ne.Loop = &l
		}
		for _, we := range e.With {
			ne.With = append(ne.With, &ast.WithEntry{Key: we.Key, Value: subst(we.Value), Span: we.Span})
		}
		wf.Edges = append(wf.Edges, ne)
	}
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
