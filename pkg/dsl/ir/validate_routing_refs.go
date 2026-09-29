package ir

import (
	"fmt"
	"strings"
)

// validateRoutingFieldRefs holds the compiler to the executor's line on the
// routing fields — `model:`, `backend:`, `provider:` and `interaction_model:`
// on a node and the three route fields of each `fallbacks:` entry, a human
// or review node's companion model, a verified action's recovery model, the
// workflow's `default_backend:`. The
// executor resolves `{{vars.…}}` there (resolveRoutingField), nothing else:
// the route is decided before the node runs, so no input, output, artifact
// or loop exists to read. An undeclared var is C033 like everywhere — at any
// depth, the first segment being the name. A dotted path under a `json` var
// resolves: the executor DRILLS the document (drillTemplatePath), and the
// launch-time screen reads it the same way, so there is nothing to warn
// about. Every other `{{…}}` span is C148 — a foreign namespace, a misspelt
// one (`{{var.m}}`, `{{Vars.m}}`, `{{env.HOME}}`), a span with no path, a
// dotted path under a scalar or list var (a string holds no members — the
// drill finds nothing and the text reaches the backend as written), the
// `{{!…}}` raw form, an opening with no close. A flat `{{vars.x}}` span
// that DOES resolve joins them when its var is declared `string[]` or
// `json` (checkRoutingVarListType, #1605): a list var resolves to its JSON
// spelling, a backend name nobody registered. The scan
// mirrors the executor's resolver, which treats any `{{…}}` as a reference
// and keeps what it cannot resolve as written: left alone, that text reached
// the backend (an invalid spec on claw, a model literally named `{{var.m}}`
// on the claude CLI) after the workspace and the sandbox had been paid for.
//
// C148 is a WARNING, like C147 and C144: a bot in the field that carries
// the shape compiled yesterday and runs today until that node is reached,
// and its failure there stays loud — the warning makes the defect visible
// before a run without breaking an upgrade. A supervisor's `model:` is not
// a routing field of a node: it is decided at spawn, without the run's
// vars, and nothing renders a template in it — any `{{…}}` there is C148
// too, said as such (the supervisor degrades, the run is not refused).
func (c *compiler) validateRoutingFieldRefs(w *Workflow) {
	for _, node := range w.Nodes {
		for _, rf := range routingFields(node) {
			loc := fmt.Sprintf("%s %q %s", node.NodeKind(), node.NodeID(), rf.name)
			spans, unterminated := templateSpans(rf.value)
			for _, span := range spans {
				// Any depth of vars path, and not the `{{!…}}` raw form:
				// the first segment is the declaration C033 checks. A
				// dotted path resolves at dispatch by DRILLING into a
				// `json` var's document (drillTemplatePath); under a scalar
				// or list var it can never resolve — a string holds no
				// members — and that shape is C148's, with the mechanism
				// said straight. The `{{!…}}` raw form splits `!vars` as a
				// namespace the resolver has not, and stays with the
				// foreign spans.
				refs, err := ParseRefs(span)
				if err == nil && len(refs) == 1 && refs[0].Kind == RefVars && !refs[0].Unquoted {
					c.validateVarsRef(w, refContext{Ref: refs[0], NodeID: node.NodeID(), Location: loc})
					if len(refs[0].Path) > 1 {
						if v, declared := w.Vars[refs[0].Path[0]]; declared && v != nil && v.Type != VarJSON {
							c.warnfAt(DiagRoutingFieldRef, node.NodeID(), "",
								"%s: %s drills into %q, a %s var — the executor resolves a dotted vars path by drilling a json var's document, and a %s holds no members, so the text reaches the backend as written and the node fails at its first delegation (declare the var `json` and move the member into it, write the id, a ${VAR:-default}, or a flat declared var)",
								loc, span, refs[0].Path[0], v.Type, v.Type)
						}
					} else {
						c.checkRoutingVarListType(w, node.NodeID(), loc, rf.name, refs[0].Path[0])
					}
					continue
				}
				c.warnfAt(DiagRoutingFieldRef, node.NodeID(), "",
					"%s: %s is not a vars reference — only {{vars.<name>}} resolves in %s, the route being decided before the node runs; the text reaches the backend as written and the node fails at its first delegation (write the id, a ${VAR:-default}, or a declared var)",
					loc, span, rf.name)
			}
			if unterminated {
				c.warnfAt(DiagRoutingFieldRef, node.NodeID(), "",
					"%s: an opening {{ has no closing }} — the text reaches the backend as written and the node fails at its first delegation", loc)
			}
		}
	}
	// The workflow's `default_backend:` is the backend of every node that
	// names none — a routing field the executor reads the same way.
	spans, unterminated := templateSpans(w.DefaultBackend)
	for _, span := range spans {
		refs, err := ParseRefs(span)
		if err == nil && len(refs) == 1 && refs[0].Kind == RefVars && !refs[0].Unquoted {
			c.validateVarsRef(w, refContext{Ref: refs[0], Location: "workflow default_backend"})
			if len(refs[0].Path) > 1 {
				if v, declared := w.Vars[refs[0].Path[0]]; declared && v != nil && v.Type != VarJSON {
					c.warnf(DiagRoutingFieldRef,
						"workflow default_backend: %s drills into %q, a %s var — only a json var's document has members to drill; the text becomes the backend name at dispatch and every node that names no backend fails at its first delegation",
						span, refs[0].Path[0], v.Type)
				}
			} else {
				c.checkRoutingVarListType(w, "", "workflow default_backend", "default_backend", refs[0].Path[0])
			}
			continue
		}
		c.warnf(DiagRoutingFieldRef,
			"workflow default_backend: %s is not a vars reference — only {{vars.<name>}} resolves there; the text becomes the backend name at dispatch and every node that names no backend fails at its first delegation (write the id, a ${VAR:-default}, or a declared var)",
			span)
	}
	if unterminated {
		c.warnf(DiagRoutingFieldRef, "workflow default_backend: an opening {{ has no closing }} — the text becomes the backend name at dispatch")
	}

	// A supervisor is an enhancement that degrades rather than blocking the
	// run (its monitors are dropped at spawn with a warning, C191), so this
	// is a warning: the run starts, the supervisor is inert and said to be.
	for _, sup := range w.Supervisors {
		if spans, unterminated := templateSpans(sup.Model); len(spans) > 0 || unterminated {
			c.warnf(DiagRoutingFieldRef,
				"supervisor %q model: %q holds a template, and a supervisor's model is not rendered — it is decided at spawn, without the run's vars, so this supervisor will evaluate nothing; write the model id or a ${VAR:-default} (#1450)",
				sup.Name, sup.Model)
		}
	}
}

// checkRoutingVarListType completes the arm that accepts a declared vars
// reference (#1605): the var's NAME resolves, but its declared TYPE can
// still be unroutable. A routing field is a scalar by nature — its text
// becomes a name at dispatch — and a `string[]` var resolves to the
// list's JSON spelling (`["claw","claude_code"]`), which the backend
// registry rejects only after the workspace and the sandbox have been
// paid for. That is exactly the failure C148 exists to describe for the
// other unresolvable shapes, so it is C148 too, and a warning for the
// same reason the whole family is: the launch may still override the var
// with a scalar — the compiler cannot know the VALUE, only the declared
// type.
//
// A `json` var is not a list by declaration: its default document decides
// (executed: `b: json = "\"claude_code\""` resolves to the ROUTABLE
// scalar "claude_code", no override involved). So the json arm reads the
// static default and stays SILENT on a scalar document — a string, a
// number, a bool — warning only on a non-scalar document (a list, an
// object, null) and on one compile time cannot read (no default, or a
// default carrying an env reference the launch expands).
func (c *compiler) checkRoutingVarListType(w *Workflow, nodeID, loc, field, varName string) {
	v := w.Vars[varName]
	if v == nil {
		return // C033 owns the undeclared name
	}
	warn := func() {
		c.warnfAt(DiagRoutingFieldRef, nodeID, "",
			"%s: {{vars.%s}} resolves, but the var's declared type is `%s` — %s is a NAME at dispatch, and a list or object value resolves to its JSON spelling ([\"a\",\"b\"]), a name no backend, model or provider answers to; the node fails at its first delegation (declare the var `string`, or override it at launch with a scalar)",
			loc, varName, v.Type, field)
	}
	switch v.Type {
	case VarStringArray:
		warn()
	case VarJSON:
		if !v.HasDefault {
			warn() // no static document to read — the launch supplies the value
			return
		}
		d, ok := v.Default.(string)
		if !ok {
			// A non-string literal (number, bool): a scalar document.
			if _, nonScalar := v.Default.([]any); nonScalar {
				warn()
			}
			if _, nonScalar := v.Default.(map[string]any); nonScalar {
				warn()
			}
			return
		}
		if carriesLiveReference(d) {
			warn() // the document depends on the launch environment
			return
		}
		// Read the default EXACTLY as the run reads it — ResolveVarText,
		// engine-supplied names sentineled — so the verdict rests on the
		// value a bare launch starts with, never on a re-parse of the
		// text (a profile-1 `\"` default is not JSON at all, and reads
		// as the string it is).
		val, err := ResolveVarText(d, VarJSON, compileTimeVarLookup)
		if err == nil {
			switch val.(type) {
			case string, bool, float64:
				return // a scalar document resolves to a routable scalar
			}
		}
		warn()
	}
}

// templateSpans returns every `{{…}}` span of s as written, the way the
// executor's resolver walks it, and whether an opening `{{` was left
// without its `}}`.
func templateSpans(s string) (spans []string, unterminated bool) {
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start == -1 {
			return spans, false
		}
		end := strings.Index(rest[start:], "}}")
		if end == -1 {
			return spans, true
		}
		spans = append(spans, rest[start:start+end+2])
		rest = rest[start+end+2:]
	}
}

// routingField is one routing value of a node, with the name a diagnostic
// gives it.
type routingField struct {
	name  string
	value string
}

// routingFields lists the routing values of a node: the fields the executor
// resolves through resolveRoutingField, and only those.
func routingFields(node Node) []routingField {
	var out []routingField
	add := func(name, value string) {
		if value != "" {
			out = append(out, routingField{name: name, value: value})
		}
	}
	switch n := node.(type) {
	case LLMNode: // agent, judge
		f := n.GetLLMFields()
		add("model", f.Model)
		add("backend", f.Backend)
		add("provider", f.Provider)
		// The companion model of an llm / llm_or_human interaction: the
		// executor copies it onto a synthetic human node's model.
		add("interaction_model", n.GetInteractionFields().InteractionModel)
		for _, fb := range n.GetFallbacks() {
			add("fallbacks."+fb.Name+".model", fb.Model)
			add("fallbacks."+fb.Name+".backend", fb.Backend)
			add("fallbacks."+fb.Name+".provider", fb.Provider)
		}
	case *RouterNode: // mode: llm
		add("model", n.Model)
		add("backend", n.Backend)
		add("provider", n.Provider)
	case *HumanNode:
		add("model", n.Model)
		add("interaction_model", n.InteractionModel)
	case *ToolNode:
		if n.Recovery != nil {
			add("recovery.model", n.Recovery.Model)
		}
	}
	return out
}
