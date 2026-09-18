package ir

import (
	"fmt"
	"strings"
)

// validateRoutingFieldRefs holds the compiler to the executor's line on the
// routing fields — `model:`, `backend:`, `provider:` and `interaction_model:`
// on a node and the three route fields of each `fallbacks:` entry, a human
// or review node's companion model, a verified action's recovery model. The
// executor resolves `{{vars.…}}` there (resolveRoutingField), nothing else:
// the route is decided before the node runs, so no input, output, artifact
// or loop exists to read. An undeclared var is C033 like everywhere; every
// other `{{…}}` span is C148, an error — a foreign namespace, a misspelt one
// (`{{var.m}}`, `{{Vars.m}}`, `{{env.HOME}}`), a span with no path, an
// opening with no close. The scan mirrors the executor's resolver, which
// treats any `{{…}}` as a reference and keeps what it cannot resolve as
// written: left alone, that text reached the backend (an invalid spec on
// claw, a model literally named `{{var.m}}` on the claude CLI) after the
// workspace and the sandbox had been paid for.
//
// A supervisor's `model:` is not a routing field of a node: it is decided
// at spawn, without the run's vars, and nothing renders a template in it —
// any `{{…}}` there is C148 too, as a warning (the supervisor degrades, the
// run is not refused), said as such.
func (c *compiler) validateRoutingFieldRefs(w *Workflow) {
	for _, node := range w.Nodes {
		for _, rf := range routingFields(node) {
			loc := fmt.Sprintf("%s %q %s", node.NodeKind(), node.NodeID(), rf.name)
			spans, unterminated := templateSpans(rf.value)
			for _, span := range spans {
				// Exactly one path segment: vars are scalars, so the executor
				// looks `{{vars.m.id}}` up as the key "m.id", which no
				// declaration can be — the span would reach the backend as
				// written with `validateVarsRef` content with "m".
				refs, err := ParseRefs(span)
				if err == nil && len(refs) == 1 && refs[0].Kind == RefVars && len(refs[0].Path) == 1 {
					c.validateVarsRef(w, refContext{Ref: refs[0], NodeID: node.NodeID(), Location: loc})
					continue
				}
				c.errorfAt(DiagRoutingFieldRef, node.NodeID(), "",
					"%s: %s is not a vars reference — only {{vars.<name>}} resolves in %s, the route being decided before the node runs (write the id, a ${VAR:-default}, or a declared var)",
					loc, span, rf.name)
			}
			if unterminated {
				c.errorfAt(DiagRoutingFieldRef, node.NodeID(), "",
					"%s: an opening {{ has no closing }} — the text would reach the backend as written", loc)
			}
		}
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
