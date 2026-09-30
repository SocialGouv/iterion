package ir

import "github.com/SocialGouv/iterion/pkg/dsl/ast"

// Sandbox opt-out diagnostic. Sandboxing is the DEFAULT: a workflow
// with no sandbox: block runs as `sandbox: auto`. An explicit
// `sandbox: none` remains supported (some flows genuinely need the
// host — e.g. bots that use the executing pod's own identity), but it
// removes the isolation boundary between the bot and the host's
// credentials/filesystem, so it must be a deliberate, visible choice.
const (
	DiagSandboxOptOut DiagCode = "C128" // workflow (or node) explicitly opts out of sandboxing (warning)
)

// validateSandboxOptOut warns on every explicit `sandbox: none`
// declaration. The workflow-level one is the real opt-out: the warning is
// advisory (never blocks compilation), the point being that an unsandboxed
// run is a reviewed decision, not an invisible default. A node-level one is
// parsed but not honoured at run time — every node shares the run's sandbox —
// so it is flagged as ineffective, unless the workflow already opted out (the
// node then does run on the host, and the workflow-level warning says so).
func (c *compiler) validateSandboxOptOut(w *Workflow) {
	workflowOptsOut := w.Sandbox != nil && w.Sandbox.Mode == "none"
	if workflowOptsOut {
		c.warnfAt(DiagSandboxOptOut, "", "",
			"workflow opts out of sandboxing (sandbox: none): every tool and shell command runs directly on the host/runner with its credentials and filesystem; sandboxing is the default — remove the block to run sandboxed, or keep the opt-out only if this flow genuinely needs the host")
		return
	}
	for _, n := range w.Nodes {
		spec := nodeSandboxSpec(n)
		if spec != nil && spec.Mode == "none" {
			c.emit(SeverityWarning, DiagSandboxOptOut, n.NodeID(), "", ast.Span{},
				"Drop the node-level `sandbox: none`: it is not honoured. To run on the host, set `sandbox: none` on the workflow or launch with `--sandbox none`.",
				"node declares sandbox: none, but a node-level sandbox is not honoured at run time: this node runs in the workflow's sandbox")
		}
	}
}

// nodeSandboxSpec returns a node's sandbox override, or nil when the
// node type carries none.
func nodeSandboxSpec(n Node) *SandboxSpec {
	switch t := n.(type) {
	case *AgentNode:
		return t.Sandbox
	case *JudgeNode:
		return t.Sandbox
	case *ToolNode:
		return t.Sandbox
	}
	return nil
}
