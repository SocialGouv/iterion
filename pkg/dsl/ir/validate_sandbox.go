package ir

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
// declaration — workflow-level and node-level overrides alike. The
// warning is advisory (never blocks compilation): the point is that an
// unsandboxed run is a reviewed decision, not an invisible default.
func (c *compiler) validateSandboxOptOut(w *Workflow) {
	if w.Sandbox != nil && w.Sandbox.Mode == "none" {
		c.warnfAt(DiagSandboxOptOut, "", "",
			"workflow opts out of sandboxing (sandbox: none): every tool and shell command runs directly on the host/runner with its credentials and filesystem; sandboxing is the default — remove the block to run sandboxed, or keep the opt-out only if this flow genuinely needs the host")
	}
	for _, n := range w.Nodes {
		spec := nodeSandboxSpec(n)
		if spec != nil && spec.Mode == "none" {
			c.warnfAt(DiagSandboxOptOut, n.NodeID(), "",
				"node opts out of sandboxing (sandbox: none): its tools and shell commands run directly on the host/runner; keep the opt-out only if this node genuinely needs the host")
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
