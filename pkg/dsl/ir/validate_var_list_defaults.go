package ir

import (
	"fmt"
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// DiagVarListDefaultRouting is the one diagnostic of the var reading: a
// site whose answer moved when the default stopped being text.
const DiagVarListDefaultRouting DiagCode = "C155" // a routing expression or a tool body reads a `string[]`/`json` var whose default now reads as a value of its type (warning)

// listDefaultShift is a var whose default the run reads as a VALUE OF ITS
// DECLARED TYPE where it used to read as the compiler's raw text.
type listDefaultShift struct {
	vt  VarType
	was string
	now string
}

// varListDefaultShifts finds every var whose default CHANGES under the run's
// reading (ResolveVarText). The test is on the VALUE, not on its Go type: a
// `json` var declared `= "\"hello\""` still reads as a string and still
// changed — from the seven characters the author wrote to the five they
// meant.
//
// Read with NO environment, deliberately: a diagnostic that consulted the
// compiling host's env would say different things on two machines, and the
// value with no env set is exactly what `${X:-default}` resolves to on a
// plain run.
func varListDefaultShifts(w *Workflow) map[string]listDefaultShift {
	var shifts map[string]listDefaultShift
	for name, v := range w.Vars {
		if v == nil || !v.HasDefault {
			continue
		}
		if v.Type != VarJSON && v.Type != VarStringArray {
			continue
		}
		text, isText := v.Default.(string)
		if !isText {
			continue
		}
		read, err := ResolveVarText(text, v.Type, noEnvLookup)
		if err != nil {
			continue
		}
		// The reading the engine gave this text before it was shared with
		// the override path: expanded, never narrowed.
		was := ExpandWithDefault(text, noEnvLookup)
		if read == was {
			continue
		}
		if shifts == nil {
			shifts = map[string]listDefaultShift{}
		}
		shifts[name] = listDefaultShift{vt: v.Type, was: was, now: jsonText(read)}
	}
	return shifts
}

// shiftSite is what the reader at a site does with the value, and therefore
// what changed for it.
type shiftSite int

const (
	// siteExpression: a `when`, a compute field, a fallback gate, a loop cap.
	siteExpression shiftSite = iota
	// siteShell: a `command:` or `postcondition:` body, where a list is argv.
	siteShell
	// siteScript: a `script:` body, where a list is a language literal.
	siteScript
	// siteRaw: the `{{!…}}` raw form, which emits the value's text with
	// no shell quoting and no language quoting at all.
	siteRaw
)

// validateVarListDefaults warns at every site whose reading of a
// `string[]`/`json` var moved when the run started seeding its default the
// way it seeds an override (#1285).
//
// The cost of that agreement is paid by a program written against the OLD
// reading, and it is paid in SILENCE. Three readers, three consequences:
//
//   - an EXPRESSION answers differently — `length(vars.tags)` counted the
//     characters of "a,b" and counts two elements, `vars.tags == 'a,b'`
//     matched and never does. A bounded loop changes its bound, a `when`
//     edge leaves from the other side, and nothing fails.
//   - a `command:` loses the value: a `string[]` renders one shell word per
//     element, so `TAGS={{vars.tags}} …` assigns the first element and runs
//     the second as a command — measured, with the node reporting SUCCESS.
//   - a `script:` receives a list or an object where it received a quoted
//     string, so `json.loads(…)` on it now fails where iterating it now
//     works.
//
// It fires only where the VALUE moved: a `json` var whose text is not JSON
// reads as that text before and after, and says nothing here.
func (c *compiler) validateVarListDefaults(w *Workflow) {
	shifts := varListDefaultShifts(w)
	if len(shifts) == 0 {
		return
	}
	report := func(site shiftSite, nodeID, edge, loc string, names map[string]bool) {
		for _, name := range sortedKeys(names) {
			s := shifts[name]
			if site == siteShell && s.vt != VarStringArray {
				// The shell arm is about ARITY, and only a `string[]`
				// moves it. A `json` value reaches a shell body as ONE
				// token of its JSON text before and after — at most the
				// whitespace the author typed is gone, which no program
				// reading JSON can tell.
				continue
			}
			c.warnfAt(DiagVarListDefaultRouting, nodeID, edge,
				"%s reads var %q (%s), whose default the run reads as %s with no value supplied, where the source text read as %q — %s. Supply the var at launch, write the site against the value, or declare the var `string` when the text is what is wanted.",
				loc, name, s.vt, s.now, s.was, shiftConsequence(site, s))
		}
	}

	for _, e := range w.Edges {
		names := map[string]bool{}
		if e.Expression != nil {
			collectShiftedVars(e.Expression, shifts, names)
		}
		// A loop cap in EXPRESSION form is read as a number; the template
		// form (`as retry("{{vars.n}}")`) needs an integer either way and
		// was broken before as it is now, so it is not a change.
		if loop := w.Loops[e.LoopName]; loop != nil && loop.MaxIterationsAST != nil {
			collectShiftedVars(loop.MaxIterationsAST, shifts, names)
		}
		report(siteExpression, e.From, edgeID(e.From, e.To), fmt.Sprintf("edge %s -> %s", e.From, e.To), names)
	}

	for _, n := range w.Nodes {
		switch node := n.(type) {
		case *ComputeNode:
			for _, ce := range node.Exprs {
				if ce == nil || ce.AST == nil {
					continue
				}
				names := map[string]bool{}
				collectShiftedVars(ce.AST, shifts, names)
				report(siteExpression, node.ID, "", fmt.Sprintf("compute %q field %q", node.ID, ce.Key), names)
			}
		case *ToolNode:
			body := append(append([]*Ref{}, node.CommandRefs...), node.PostcondRefs...)
			shell, rawShell := map[string]bool{}, map[string]bool{}
			splitShiftedRefs(body, shifts, shell, rawShell)
			report(siteShell, node.ID, "", fmt.Sprintf("tool %q shell body", node.ID), shell)
			report(siteRaw, node.ID, "", fmt.Sprintf("tool %q shell body", node.ID), rawShell)
			script, rawScript := map[string]bool{}, map[string]bool{}
			splitShiftedRefs(node.ScriptRefs, shifts, script, rawScript)
			report(siteScript, node.ID, "", fmt.Sprintf("tool %q script", node.ID), script)
			report(siteRaw, node.ID, "", fmt.Sprintf("tool %q script", node.ID), rawScript)
		}
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		for _, fb := range nn.GetFallbacks() {
			if fb.When == "" {
				continue
			}
			parsed, err := expr.Parse(fb.When)
			if err != nil {
				continue // checkFallbackWhen already refused it
			}
			names := map[string]bool{}
			collectShiftedVars(parsed, shifts, names)
			report(siteExpression, nn.NodeID(), "", fmt.Sprintf("%s %q fallback %s when:", nn.NodeKind(), nn.NodeID(), fallbackLabel(fb)), names)
		}
	}
}

// shiftConsequence names what THIS reader does differently, so the warning
// says something the author can act on rather than repeating the cause.
func shiftConsequence(site shiftSite, s listDefaultShift) string {
	switch site {
	case siteRaw:
		return "the `{{!…}}` raw form emits the value's own text, with no quoting of any kind, so a program reading it as one string now reads the JSON form of a list"
	case siteShell:
		return "a `string[]` renders one shell word per element, and none at all when it is empty, so the command's arity moves: an assignment keeps only the first element and the rest become a command of their own"
	case siteScript:
		return "a script body receives the value as a language literal — a list or an object where it received a quoted string"
	default:
		if s.vt == VarStringArray {
			return "an expression written against the text answers differently: `length()` counts elements instead of characters, and an `==` against a string literal never matches"
		}
		return "an expression written against the text answers differently: a member or an index now resolves, `length()` counts elements, and an `==` against a string literal never matches"
	}
}

// collectShiftedVars adds every `vars.<name>` the expression reads that the
// shift set names. A drilled read — `vars.cfg.enabled`, the canonical way
// to route on a `json` var — counts: the whole value moved, so every read
// of it did.
func collectShiftedVars(ast *expr.AST, shifts map[string]listDefaultShift, into map[string]bool) {
	if ast == nil {
		return
	}
	for _, r := range ast.Refs() {
		ns, path := expr.NormalizePath(r.Namespace, r.Path)
		if ns != "vars" || len(path) == 0 {
			continue
		}
		if _, ok := shifts[path[0]]; ok {
			into[path[0]] = true
		}
	}
}

// splitShiftedRefs reads a template's parsed refs — a tool body's
// `{{vars.x}}` — and separates the default form from the `{{!…}}` raw one:
// the two render through different functions, so they do not change in the
// same way and must not be told the same thing. A nil raw set folds both
// into quoted.
func splitShiftedRefs(refs []*Ref, shifts map[string]listDefaultShift, quoted, raw map[string]bool) {
	for _, r := range refs {
		if r == nil || r.Kind != RefVars || len(r.Path) == 0 {
			continue
		}
		if _, ok := shifts[r.Path[0]]; !ok {
			continue
		}
		if r.Unquoted && raw != nil {
			raw[r.Path[0]] = true
			continue
		}
		quoted[r.Path[0]] = true
	}
}

// sortedKeys renders a set in a stable order so two compilations of one
// source emit their diagnostics in the same sequence.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
