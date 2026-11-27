package unparse

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// checkNames reports the first name the writer would put down bare that
// the lexer does not read as one identifier: a declaration's name, a schema
// or vars field, a loop or iteration name. Such a name — `fix loop`,
// `user-input`, typed into a studio field that took anything — tears the
// text apart, and the re-read then refuses it at columns of a line the
// author never wrote; this says which name, where.
func checkNames(f *ast.File) error {
	bad := func(what, name string) error {
		return fmt.Errorf("%s %q is not a name the .bot syntax reads: a name is letters, digits and _, starting with a letter or _ (rename it, e.g. %q)", what, name, suggestName(name))
	}
	check := func(what, name string) error {
		if !isBareIdent(name) {
			return bad(what, name)
		}
		return nil
	}
	if err := checkVars("vars field", f.Vars, check); err != nil {
		return err
	}
	if f.Secrets != nil {
		for _, s := range f.Secrets.Fields {
			if err := check("secret", s.Name); err != nil {
				return err
			}
		}
	}
	if f.Presets != nil {
		for _, p := range f.Presets.Entries {
			if err := check("preset", p.Name); err != nil {
				return err
			}
			for _, v := range p.Values {
				if err := check("preset "+p.Name+" key", v.Key); err != nil {
					return err
				}
			}
		}
	}
	if err := checkAttachments("attachment", f.Attachments, check); err != nil {
		return err
	}
	for _, d := range f.MCPServers {
		if err := check("mcp_server", d.Name); err != nil {
			return err
		}
	}
	for _, p := range f.Prompts {
		if p.Inline {
			continue // written in its node as a quoted string: no name is written
		}
		if err := check("prompt", p.Name); err != nil {
			return err
		}
	}
	for _, s := range f.Schemas {
		if err := check("schema", s.Name); err != nil {
			return err
		}
		for _, fl := range s.Fields {
			if err := check("schema "+s.Name+" field", fl.Name); err != nil {
				return err
			}
		}
	}
	for _, c := range f.Cursors {
		if err := check("cursor", c.Name); err != nil {
			return err
		}
		for _, v := range c.Values {
			if err := check("cursor "+c.Name+" value", v.Name); err != nil {
				return err
			}
		}
	}
	for _, s := range f.Supervisors {
		if err := check("supervisor", s.Name); err != nil {
			return err
		}
	}
	if err := checkNodeNames("", f.Agents, f.Judges, f.Routers, f.Humans, f.Tools, f.Computes, check); err != nil {
		return err
	}
	for _, d := range f.Emits {
		if err := check("emit", d.Name); err != nil {
			return err
		}
		if err := checkWithKeys("emit "+d.Name+" with key", d.With, check); err != nil {
			return err
		}
	}
	for _, d := range f.Waits {
		if err := check("wait", d.Name); err != nil {
			return err
		}
	}
	for _, d := range f.AwaitAnswers {
		if err := check("await_answers", d.Name); err != nil {
			return err
		}
	}
	for _, d := range f.Fails {
		if err := check("fail", d.Name); err != nil {
			return err
		}
		if d.Code != "" {
			if err := check("fail "+d.Name+" code", d.Code); err != nil {
				return err
			}
		}
	}
	for _, d := range f.Subbots {
		if err := check("subbot", d.Name); err != nil {
			return err
		}
		if err := checkWithKeys("subbot "+d.Name+" with key", d.With, check); err != nil {
			return err
		}
	}
	for _, g := range f.Groups {
		if err := check("group", g.Name); err != nil {
			return err
		}
		for _, p := range g.Params {
			if err := check("group "+g.Name+" parameter", p); err != nil {
				return err
			}
		}
		if err := checkNodeNames("group "+g.Name+", ", g.Agents, g.Judges, g.Routers, g.Humans, g.Tools, g.Computes, check); err != nil {
			return err
		}
		if err := checkEdgeNames("group "+g.Name, g.Edges, check); err != nil {
			return err
		}
	}
	for _, u := range f.Uses {
		if err := check("use "+u.Group+" prefix", u.Prefix); err != nil {
			return err
		}
		if err := checkWithKeys("use "+u.Group+" with key", u.With, check); err != nil {
			return err
		}
	}
	for _, w := range f.Workflows {
		if err := check("workflow", w.Name); err != nil {
			return err
		}
		if err := checkVars("workflow "+w.Name+" vars field", w.Vars, check); err != nil {
			return err
		}
		if err := checkAttachments("workflow "+w.Name+" attachment", w.Attachments, check); err != nil {
			return err
		}
		if err := checkEdgeNames("workflow "+w.Name, w.Edges, check); err != nil {
			return err
		}
	}
	return nil
}

func checkVars(what string, v *ast.VarsBlock, check func(string, string) error) error {
	if v == nil {
		return nil
	}
	for _, fl := range v.Fields {
		if err := check(what, fl.Name); err != nil {
			return err
		}
	}
	return nil
}

func checkAttachments(what string, a *ast.AttachmentsBlock, check func(string, string) error) error {
	if a == nil {
		return nil
	}
	for _, fl := range a.Fields {
		if err := check(what, fl.Name); err != nil {
			return err
		}
	}
	return nil
}

// checkLLMNames holds the names an agent or judge writes of its own: its
// fallback routes' and its cursor settings' keys.
func checkLLMNames(node string, d *ast.LLMDecl, check func(string, string) error) error {
	if err := checkPublish(node, d.Publish, check); err != nil {
		return err
	}
	if d.SessionSlot != "" {
		if err := check(node+" session_slot", d.SessionSlot); err != nil {
			return err
		}
	}
	for _, fb := range d.Fallbacks {
		if fb == nil || fb.Name == "" {
			continue // said by checkFallbackNames, in its own words
		}
		if err := check(node+" fallback route", fb.Name); err != nil {
			return err
		}
	}
	if d.Cursors != nil {
		for _, s := range d.Cursors.Settings {
			if err := check(node+" cursor setting", s.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkNodeNames(where string, agents []*ast.AgentDecl, judges []*ast.JudgeDecl, routers []*ast.RouterDecl, humans []*ast.HumanDecl, tools []*ast.ToolNodeDecl, computes []*ast.ComputeDecl, check func(string, string) error) error {
	for _, d := range agents {
		if err := check(where+"agent", d.Name); err != nil {
			return err
		}
		if err := checkLLMNames(where+"agent "+d.Name, &d.LLMDecl, check); err != nil {
			return err
		}
	}
	for _, d := range judges {
		if err := check(where+"judge", d.Name); err != nil {
			return err
		}
		if err := checkLLMNames(where+"judge "+d.Name, &d.LLMDecl, check); err != nil {
			return err
		}
	}
	for _, d := range routers {
		if err := check(where+"router", d.Name); err != nil {
			return err
		}
		if d.As != "" {
			if err := check(where+"router "+d.Name+" item binding (as)", d.As); err != nil {
				return err
			}
		}
	}
	for _, d := range humans {
		if err := check(where+"human", d.Name); err != nil {
			return err
		}
		if err := checkPublish(where+"human "+d.Name, d.Publish, check); err != nil {
			return err
		}
	}
	for _, d := range tools {
		if err := check(where+"tool", d.Name); err != nil {
			return err
		}
		if err := checkPublish(where+"tool "+d.Name, d.Publish, check); err != nil {
			return err
		}
	}
	for _, d := range computes {
		if err := check(where+"compute", d.Name); err != nil {
			return err
		}
		if err := checkPublish(where+"compute "+d.Name, d.Publish, check); err != nil {
			return err
		}
		for _, e := range d.Expr {
			if e == nil {
				continue
			}
			if err := check(where+"compute "+d.Name+" expr key", e.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPublish holds a node's artifact name, written bare and read back as
// {{artifacts.<name>}}; an empty one is absent.
func checkPublish(node, publish string, check func(string, string) error) error {
	if publish == "" {
		return nil
	}
	return check(node+" publish", publish)
}

// checkWithKeys holds the keys of a `with { key: value }` block, written
// bare.
func checkWithKeys(what string, with []*ast.WithEntry, check func(string, string) error) error {
	for _, e := range with {
		if e == nil {
			continue
		}
		if err := check(what, e.Key); err != nil {
			return err
		}
	}
	return nil
}

// checkEdgeNames holds the names an edge line carries of its own: a loop's
// and an iteration's. Its endpoints are node names, held at their
// declarations.
func checkEdgeNames(where string, edges []*ast.Edge, check func(string, string) error) error {
	for _, e := range edges {
		if e == nil {
			continue
		}
		edge := fmt.Sprintf("%s, edge %s -> %s,", where, e.From, e.To)
		if e.Loop != nil {
			if err := check(edge+" loop", e.Loop.Name); err != nil {
				return err
			}
		}
		if e.Foreach != nil {
			if err := check(edge+" foreach", e.Foreach.Name); err != nil {
				return err
			}
			if err := check(edge+" foreach item", e.Foreach.Item); err != nil {
				return err
			}
		}
		if err := checkWithKeys(edge+" with key", e.With, check); err != nil {
			return err
		}
	}
	return nil
}

// suggestName is name with every character the lexer does not read in a
// name turned into _, and a leading digit prefixed with one: the spelling
// the refusal offers.
func suggestName(name string) string {
	out := []rune{}
	for _, r := range name {
		if isBareIdent(string(r)) || (len(out) > 0 && isBareIdent("_"+string(r))) {
			out = append(out, r)
			continue
		}
		if len(out) == 0 && isBareIdent("_"+string(r)) {
			out = append(out, '_', r)
			continue
		}
		out = append(out, '_')
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
