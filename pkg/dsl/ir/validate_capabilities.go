package ir

import (
	"regexp"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/runops"
)

// Capability diagnostics.
const (
	DiagUnknownCapability   DiagCode = "C080" // unknown capability name (warning, registry is open)
	DiagMalformedCapability DiagCode = "C081" // capability name does not match the required shape
	DiagBoardCapInSandbox   DiagCode = "C082" // board.* capability requested while sandboxed (HTTP transport needed)
)

// Watch capability names. A node with watch.subscribe / watch.unsubscribe can
// opt its run into the runtime watch fan-out (MVP3b): once subscribed to a
// native-board issue, the run receives a queued message whenever that issue
// changes state. This is the source of truth for the strings; the claw tool
// registrar (pkg/backend/tool/claw_watch_tools.go) mirrors them. Currently
// wired for the claw backend only — see RegisterClawWatchTools.
const (
	CapWatchSubscribe   = "watch.subscribe"
	CapWatchUnsubscribe = "watch.unsubscribe"
)

// KnownCapabilities is the set of capabilities the iterion runtime understands
// at compile time. The validator emits a C080 warning for any cap declared on
// an agent or judge that is not in this set — but does not reject it, so
// future capabilities can be registered out-of-tree without DSL changes.
//
// Capability naming convention: lowercase `domain` or `domain.action`,
// e.g. `board.create`, `board.read`. Enforced by C081.
var KnownCapabilities = map[string]bool{
	boardops.CapBoardRead:    true,
	boardops.CapBoardCreate:  true,
	boardops.CapBoardMove:    true,
	boardops.CapBoardAssign:  true,
	boardops.CapBoardLabel:   true,
	boardops.CapBoardClose:   true,
	boardops.CapBoardComment: true,
	CapWatchSubscribe:        true,
	CapWatchUnsubscribe:      true,
	runops.CapRunsRead:       true,
}

// capShapeRe enforces the lowercase `domain` or `domain.action` shape.
var capShapeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)

// validateCapabilities walks every agent/judge node in the workflow and
// validates the shape and known-ness of each capability. Workflow-level
// capabilities are validated through inheritance: when a node has no
// explicit list, it inherits the workflow's, so workflow-level shape errors
// are surfaced via every inheriting node — that's intentional: an author
// who explicitly grants a malformed cap workflow-wide should see one error
// per affected node.
//
// In addition, a warning (C082) is emitted when board.* capabilities are
// granted to nodes running under a sandbox: the host stdio MCP server
// can't reach the sandboxed process, so the runtime falls back to an
// HTTP MCP transport. That fallback needs the iterion server's
// BoardHTTPEndpoint+BoardRunToken plumbing — without it the cap silently
// no-ops in the sandbox. The warning is intentional rather than blocking
// because non-sandboxed runs of the same workflow are perfectly fine.
func (c *compiler) validateCapabilities(w *Workflow) {
	// Workflow-level: emit one diagnostic per malformed entry, attributed
	// to no specific node.
	for _, cap := range w.Capabilities {
		c.validateOneCapability(cap, "", "workflow")
	}
	sandboxActive := workflowSandboxRequiresHTTPBoard(w)
	for _, n := range w.Nodes {
		ln, ok := n.(LLMNode)
		if !ok {
			continue
		}
		caps := ln.GetCapabilities()
		kind := ln.NodeKind().String()
		for _, cap := range caps {
			c.validateOneCapability(cap, n.NodeID(), kind)
			if sandboxActive && isBoardCapability(cap) {
				c.warnfAt(DiagBoardCapInSandbox, n.NodeID(), "",
					"%s capability %q runs under a sandbox: the runtime needs the HTTP board MCP transport (server-side BoardHTTPEndpoint+BoardRunToken). Without it the capability silently no-ops inside the container.",
					kind, cap)
			}
		}
	}
}

// workflowSandboxRequiresHTTPBoard returns true when the workflow asks
// for any sandbox flavour other than the explicit "none" opt-out. The
// node-level Sandbox override is honoured by the runtime but the
// compile-time check uses the workflow-level signal — the warning fires
// once per board-capability declaration and the operator already knows
// whether they overrode sandbox per node.
func workflowSandboxRequiresHTTPBoard(w *Workflow) bool {
	if w == nil || w.Sandbox == nil {
		return false
	}
	switch w.Sandbox.Mode {
	case "", "none":
		return false
	}
	return true
}

// isBoardCapability matches the board.* prefix used by every native
// kanban capability (board.read, board.create, board.move, …).
func isBoardCapability(cap string) bool {
	return cap == "board" || (len(cap) > len("board.") && cap[:len("board.")] == "board.")
}

func (c *compiler) validateOneCapability(cap, nodeID, scope string) {
	if cap == "" {
		return
	}
	if !capShapeRe.MatchString(cap) {
		c.errorfAt(DiagMalformedCapability, nodeID, "",
			"%s capability %q must match shape 'domain' or 'domain.action' (lowercase letters, digits, underscores)",
			scope, cap)
		return
	}
	if !KnownCapabilities[cap] {
		c.warnfAt(DiagUnknownCapability, nodeID, "",
			"%s capability %q is not in the built-in registry (this is a warning, not an error — the cap will be passed through to the executor)",
			scope, cap)
	}
}
