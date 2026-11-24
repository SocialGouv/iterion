package cli

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// validate_explain.go is the `--explain` half of the capability-drift
// disclosure (iterion#2815): on claude_code a `tools:` list becomes
// --disallowedTools over a roster iterion does not own, so what the list
// grants — and what leaves the node whatever it says — is shown here, once
// per node, as a READING: never a diagnostic (ARB-50 withdrew the per-node
// warning — four of the five unconditional losses answer to no .bot edit,
// and a warning 78 of 79 nodes cannot satisfy teaches operators to ignore
// diagnostics), and never a verdict on the program.
//
// The answer comes from the SAME projection the delegate's spawn argv is
// built from (delegate.ClaudeCodeToolSurface), so the disclosure cannot
// drift from the enforcement.

// ValidateExplainNode is one claude_code LLM node's tool surface, as the
// delegate would spawn it.
type ValidateExplainNode struct {
	NodeID  string                     `json:"node_id"`
	Surface delegate.ClaudeToolSurface `json:"surface"`
}

// explainClaudeCodeSurfaces reads every LLM node whose SOURCE-resolved route
// is claude_code — the compiler's reading (ir.SourceNodeBackendName), not the
// host's environment, so `validate` says the same thing on every machine.
func explainClaudeCodeSurfaces(wf *ir.Workflow) []ValidateExplainNode {
	var out []ValidateExplainNode
	for _, n := range wf.Nodes {
		llm, ok := n.(ir.LLMNode)
		if !ok {
			continue
		}
		if ir.SourceNodeBackendName(llm.GetLLMFields().Backend, wf.DefaultBackend) != backendcaps.ClaudeCode {
			continue
		}
		tools := llm.GetTools()
		out = append(out, ValidateExplainNode{
			NodeID: n.NodeID(),
			Surface: delegate.ClaudeCodeToolSurface(delegate.Task{
				AllowedTools:  tools,
				ToolsDeclared: toolcatalog.ToolsDeclared(tools),
				Readonly:      llm.GetLLMFields().Readonly,
				// The same derivation the task builder applies
				// (executor_build_task.go): the raw spelling, not the
				// canonical fold.
				DiagnosticShell: slices.Contains(tools, "diagnostic_shell"),
			}),
		})
	}
	return out
}

// printExplain renders the --explain reading: one block per claude_code node,
// the granted names first, then what leaves it anyway, the reason per name.
func printExplain(p *Printer, nodes []ValidateExplainNode) {
	if len(nodes) == 0 {
		return
	}
	p.Blank()
	p.Line("  Capability drift — claude_code (the roster a tools: list bounds is the CLI's, not iterion's):")
	for _, n := range nodes {
		s := n.Surface
		if s.Declared {
			p.Line("    node %q:", n.NodeID)
		} else {
			p.Line("    node %q (no tools: declaration — the ambient roster stays):", n.NodeID)
		}
		if len(s.Granted) > 0 {
			p.Line("      the spawn keeps: %s", strings.Join(s.Granted, ", "))
		} else {
			p.Line("      the spawn keeps no native tool")
		}
		var always, declared []delegate.ClaudeToolWithholding
		for _, w := range s.Withheld {
			if w.Always {
				always = append(always, w)
			} else {
				declared = append(declared, w)
			}
		}
		if len(always) > 0 {
			p.Line("      withheld whatever the list says:")
			for _, w := range always {
				name := w.Name
				if !w.OnRoster {
					name += " (not on this CLI's roster — withheld defensively)"
				}
				p.Line("        %s — %s", name, w.Reason)
			}
		}
		if len(declared) > 0 {
			p.Line("      the declaration lets go:")
			for _, w := range declared {
				p.Line("        %s — %s", w.Name, w.Reason)
			}
		}
	}
	p.Line("    MCP tools survive any list, being on no roster; the operator knobs (background tasks, the orchestration-tools switch) are not reflected here.")
}
