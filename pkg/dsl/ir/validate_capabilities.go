package ir

import (
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/boardcaps"
	"github.com/SocialGouv/iterion/pkg/connection"
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
	boardcaps.BoardRead:    true,
	boardcaps.BoardCreate:  true,
	boardcaps.BoardMove:    true,
	boardcaps.BoardAssign:  true,
	boardcaps.BoardLabel:   true,
	boardcaps.BoardClose:   true,
	boardcaps.BoardComment: true,
	CapWatchSubscribe:      true,
	CapWatchUnsubscribe:    true,
	runops.CapRunsRead:     true,
	// The connector MCP facade (ADR-098 §2, #2758): an agent node declaring
	// `capabilities: [connectors]` is wired to the host's facade endpoint,
	// through which it reaches the tenant's `agent`-capable connections.
	connection.CapConnectors: true,
}

// capShapeRe enforces the lowercase `domain` or `domain.action` shape.
var capShapeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)

// NativeConnectorIDs is the set of connector ids a NATIVE action transport
// owns (ADR-151, iterion#2834): an in-process, credential-free connector the
// engine executes itself rather than reaching over HTTP. Membership decides
// one compile-time question — `connection:` is optional for these (C261 does
// not fire) — and nothing else; the transport registry that executes them
// lives in pkg/backend/model. A new native connector adds its id to the
// vocabulary leaf as part of its seam registration (G110: registration IS
// the seam — the compiler's half of it), never an `if` at a call site.
//
// The set itself lives in pkg/boardcaps: the compiler AND the connector
// validator read it, and reading it must not link the dispatcher into a
// binary that compiles a .bot (iterion#2857).
var NativeConnectorIDs = boardcaps.NativeConnectorIDs

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
// HTTP MCP transport. That fallback needs the BoardHTTPEndpoint+
// BoardRunToken plumbing from the host the sandbox answers to — the
// studio/server for an in-process run, the runner pod for a cloud run
// (#2632); without it (a bare CLI run) the delegate disables the node's
// board tools with a loud warning. The warning is intentional rather than
// blocking because non-sandboxed runs of the same workflow are perfectly
// fine.
func (c *compiler) validateCapabilities(w *Workflow) {
	// Workflow-level: emit one diagnostic per malformed entry, attributed
	// to no specific node.
	for _, cap := range w.Capabilities {
		c.validateOneCapability(cap, "", "workflow")
	}
	sandboxActive := workflowSandboxRequiresHTTPBoard(w)
	for _, n := range w.Nodes {
		// A tool node declares `capabilities:` too (ADR-151, #78=A) and gets
		// the SAME shape/known checks — one authorization surface, never a
		// second regime. What it does NOT get is C082: the sandbox warning
		// exists because an LLM node's board tools reach the host through an
		// MCP transport the sandbox may not have, while a tool node's board
		// action runs IN the engine — the transport question is exactly what
		// the native seam removes (#2632's second half). What it gets
		// instead is C329: on a command:/script: node no consumer reads the
		// list, and an inert grant reads as configured.
		if tn, ok := n.(*ToolNode); ok {
			for _, cap := range tn.Capabilities {
				c.validateOneCapability(cap, tn.ID, "tool")
			}
			// The grant has exactly ONE reader: the action recipe's NATIVE
			// transport. A shell recipe has none — and neither does an HTTP
			// action, whose authorization is the connection's credential:
			// the warning's own sentence is the condition.
			nativeAction := tn.Action != "" && NativeConnectorIDs[actionConnectorOf(tn.Action)]
			if len(tn.Capabilities) > 0 && !nativeAction {
				c.warnfAt(DiagToolCapabilitiesInert, tn.ID, "",
					"tool %q declares `capabilities:` but %s — only the action: recipe's native transport reads the grant (ADR-151), so this list opens nothing",
					tn.ID, toolGrantReaderAbsent(tn))
			}
			// A native board action whose grant is declared NOWHERE is
			// refused CAPABILITY_DENIED at run time; the compiler can say it
			// earlier and name the capability (the C328 posture: a warning
			// with the remedy, never a refusal — a hand-built catalog of one
			// node is none of this check's business).
			if nativeAction {
				if want, known := boardcaps.ActionCapability[tn.Action]; known {
					eff := tn.Capabilities
					if eff == nil {
						eff = w.Capabilities
					}
					if !capSet(eff)[want] {
						c.warnfAt(DiagNativeActionWithoutGrant, tn.ID, "",
							"tool %q: `action: %s` needs the %q capability, declared neither on the node nor on the workflow — at run time the native transport refuses it (CAPABILITY_DENIED). Declare %q in the node's (or the workflow's) capabilities: list",
							tn.ID, tn.Action, want, want)
					}
				}
			}
			continue
		}
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
					"%s capability %q runs under a sandbox: the runtime needs the HTTP board MCP transport (BoardHTTPEndpoint+BoardRunToken) from the host the sandbox answers to — the studio/server locally, the runner pod on iterion.cloud. Without it the node's board tools are disabled with a delegate warning.",
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

// actionConnectorOf splits the connector id off an action id.
func actionConnectorOf(action string) string {
	connector, _, _ := strings.Cut(action, ".")
	return connector
}

// toolGrantReaderAbsent words WHY a tool node's grant has no reader, for
// C329: the recipe decides, not the list.
func toolGrantReaderAbsent(tn *ToolNode) string {
	if tn.Action != "" {
		return "its action: is served over HTTP, whose authorization is the connection's credential"
	}
	return "its recipe is command:/script:"
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
