package bundlelint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The public contract against the manifest and the parents (ADR-099):
// warnings, never errors — the contract is bound to its program by the
// compiler (C300–C303); here two other readers of the bot are held to it.
const (
	// DiagContractManifestMismatch: the manifest's public face contradicts
	// the contract — a `launch.primary` entry that is not a contract input,
	// a `produces[].node` that produces no contract output. `launch.hidden`
	// is not held: it lists the inputs the launch form never renders — the
	// operator's knobs — which a public face leaves out on purpose.
	DiagContractManifestMismatch Code = "C254"
	// DiagSubbotContractMismatch: a `subbot` node's `with:` misses an input
	// the child's contract requires, or its `output:` schema names a field
	// the child's contract does not produce — the runtime projects the
	// subbot's output from the contract's ports (#1280), so the schema a
	// parent (and a dry run) read is held to what actually arrives.
	DiagSubbotContractMismatch Code = "C255"
)

// checkContractManifest holds the manifest to the contract the workflow
// keeps (C254). A bot without a contract is held to nothing here.
func checkContractManifest(diags *[]Diag, m *bundle.Manifest, w *ir.Workflow) {
	if m == nil || w == nil || w.Contract == nil {
		return
	}
	c := w.Contract
	inputs := map[string]bool{}
	for _, p := range c.Inputs {
		inputs[p.Name] = true
	}
	if m.Launch != nil {
		for _, name := range m.Launch.Primary {
			name = strings.TrimSpace(name)
			if name == "" || inputs[name] {
				continue
			}
			*diags = append(*diags, Diag{
				Code: DiagContractManifestMismatch, Severity: SeverityWarning,
				Field:   "launch.primary." + name,
				Message: fmt.Sprintf("launch.primary names %q, which contract %q declares no input for: the launch form and the contract disagree on what the bot takes", name, c.Name),
				Hint:    "declare the input in the contract (it must be a declared var), or drop the name from launch.primary: (an operator-only var belongs under launch.hidden:)",
			})
		}
	}
	if len(c.Outputs) == 0 {
		return
	}
	producers := map[string]bool{}
	var names []string
	for _, p := range c.Outputs {
		if p.FromNode != "" && !producers[p.FromNode] {
			producers[p.FromNode] = true
			names = append(names, p.FromNode)
		}
	}
	sort.Strings(names)
	for i, a := range m.Produces {
		if a.Node == "" || producers[a.Node] {
			continue
		}
		*diags = append(*diags, Diag{
			Code: DiagContractManifestMismatch, Severity: SeverityWarning,
			Field:   fmt.Sprintf("produces[%d].node", i),
			Message: fmt.Sprintf("produces[%d] hands off the output of node %q, which produces no output of contract %q (its outputs come from %s): the hand-off and the contract disagree on what the bot produces", i, a.Node, c.Name, strings.Join(names, ", ")),
			Hint:    "bind a contract output to that node (`from: <node>.<field>`), or hand off the node the contract names",
		})
	}
}

// checkSubbotContracts holds each `subbot` node to its child's contract
// (C255), for the children the caller read and compiled: a required input
// — one whose var has no default, the compiler's word (C300) — that the
// parent's `with:` does not pass, and the parent's `output:` schema held to
// the contract's output ports — the runtime projects the subbot's output
// from those ports (#1280), so a schema field no port produces never
// arrives, and a scalar field typed differently declares a shape the child
// does not send. A child without a contract, or one the caller could not
// read, is held to nothing (C253 names an unread child).
func checkSubbotContracts(diags *[]Diag, w *ir.Workflow, children map[string]*ir.PublicContract) {
	if w == nil || len(children) == 0 {
		return
	}
	ids := make([]string, 0, len(w.Nodes))
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sb, ok := w.Nodes[id].(*ir.SubbotNode)
		if !ok {
			continue
		}
		cc := children[id]
		if cc == nil {
			continue
		}
		passed := map[string]bool{}
		for _, m := range sb.With {
			if m != nil {
				passed[m.Key] = true
			}
		}
		for _, in := range cc.Inputs {
			if !in.Required || passed[in.Name] {
				continue
			}
			hint := fmt.Sprintf("pass it: `with { %s: <value> }` (the parent declares no var %s to forward), or give the child's var a default", in.Name, in.Name)
			if w.Vars[in.Name] != nil {
				hint = fmt.Sprintf("pass it: `with { %s: \"{{vars.%s}}\" }`, or give the child's var a default", in.Name, in.Name)
			}
			*diags = append(*diags, Diag{
				Code: DiagSubbotContractMismatch, Severity: SeverityWarning,
				Field:   "subbot." + id + ".with." + in.Name,
				Message: fmt.Sprintf("subbot %q: the child's contract %q requires input %q (%s), which the with: block does not pass", id, cc.Name, in.Name, in.Type),
				Hint:    hint,
			})
		}
		// The output arm: the parent's `output:` schema against the child's
		// ports. Types are compared only when BOTH sides spell a scalar
		// builtin ("string", "bool", "int", "float", with their `[]` forms):
		// a port typed with a schema name — or a file port — projects the
		// producer's whole output object, and a parent's `json` field accepts
		// it whatever its shape, so a literal comparison there would warn on
		// a correct parent.
		if sb.OutputSchema == "" {
			continue
		}
		schema := w.Schemas[sb.OutputSchema]
		if schema == nil {
			continue
		}
		ports := map[string]*ir.PublicPort{}
		for _, out := range cc.Outputs {
			ports[out.Name] = out
		}
		produced := "its outputs: " + portNames(cc.Outputs)
		if len(cc.Outputs) == 0 {
			produced = "it declares no output"
		}
		for _, f := range schema.Fields {
			out, ok := ports[f.Name]
			if !ok {
				*diags = append(*diags, Diag{
					Code: DiagSubbotContractMismatch, Severity: SeverityWarning,
					Field:   "subbot." + id + ".output." + f.Name,
					Message: fmt.Sprintf("subbot %q: output schema %q expects field %q, which the child's contract %q does not produce (%s)", id, sb.OutputSchema, f.Name, cc.Name, produced),
					Hint:    "name a field the child's contract produces, or add the output to the child's contract",
				})
				continue
			}
			if scalarPortType(out.Type) && scalarPortType(f.Type.String()) && out.Type != f.Type.String() {
				*diags = append(*diags, Diag{
					Code: DiagSubbotContractMismatch, Severity: SeverityWarning,
					Field:   "subbot." + id + ".output." + f.Name,
					Message: fmt.Sprintf("subbot %q: output schema %q expects field %q as %s, the child's contract %q produces it as %s", id, sb.OutputSchema, f.Name, f.Type.String(), cc.Name, out.Type),
					Hint:    "give the parent's field the child's type",
				})
			}
		}
	}
}

// scalarPortType reports whether t spells one of the scalar builtins a
// schema field and a contract port share — "string", "bool", "int", "float",
// with their `[]` forms. Those are the only spellings a literal comparison
// can judge: `json` accepts any shape, and a schema name names the child's
// own declaration, not anything the parent can spell.
func scalarPortType(t string) bool {
	switch strings.TrimSuffix(t, "[]") {
	case "string", "bool", "int", "float":
		return true
	}
	return false
}

func portNames(ports []*ir.PublicPort) string {
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}
