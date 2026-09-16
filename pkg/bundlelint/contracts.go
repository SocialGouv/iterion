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
	// the child's contract requires. The parent's `output:` schema is not
	// held to the child's contract: a subbot's output is the child's
	// terminal node output, which the contract's outputs do not define.
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
// parent's `with:` does not pass. A child without a contract, or one the
// caller could not read, is held to nothing (C253 names an unread child).
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
	}
}
