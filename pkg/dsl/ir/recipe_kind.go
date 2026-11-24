package ir

import "strings"

// ToolRecipeKind classifies how a tool node's recipe is executed. The
// classification belongs here, with the fields it reads (Command,
// CommandRefs, Script, Action), because it decides more than dispatch:
// whether the ADR-044 self-repair rung can run at all, which the launch
// walks must answer the same way dispatch does (#2783).
type ToolRecipeKind int

const (
	ToolRecipeScript   ToolRecipeKind = iota // `script:` body via an interpreter
	ToolRecipeShell                          // `command:` with template refs or shell metacharacters
	ToolRecipeRegistry                       // bare `command:` resolved as a registered tool name
	ToolRecipeAction                         // `action:` — a connector operation (ADR-098)
)

// ToolRecipeKindOf reports how t's recipe runs. `action:` and `script:` take
// precedence over `command:` (validation makes all three mutually
// exclusive); a command carrying template refs or shell metacharacters runs
// via sh, else it is a bare registered-tool name.
func ToolRecipeKindOf(t *ToolNode) ToolRecipeKind {
	switch {
	case t.Action != "":
		return ToolRecipeAction
	case t.Script != "":
		return ToolRecipeScript
	case len(t.CommandRefs) > 0 || commandLooksLikeShell(t.Command):
		return ToolRecipeShell
	default:
		return ToolRecipeRegistry
	}
}

// commandLooksLikeShell reports whether a command string is a shell command
// rather than a bare tool name. Tool names are simple identifiers (e.g.
// "read_file", "bash"); shell commands carry spaces, operators or path
// separators.
func commandLooksLikeShell(cmd string) bool {
	return strings.ContainsAny(cmd, " \t|&;><$`(){}\"'/")
}

// ToolRecipeSelfRepairable reports whether t's recipe is one the self-repair
// rung can act on: rung 3 asks a model for a CORRECTED COMMAND and re-runs
// it, so it needs a command or a script to correct. A registry recipe (a
// bare tool name) has none — dispatch runs it through the standard path and
// skips the rung — and an action recipe refuses the whole ladder (C262).
func ToolRecipeSelfRepairable(t *ToolNode) bool {
	switch ToolRecipeKindOf(t) {
	case ToolRecipeScript, ToolRecipeShell:
		return true
	}
	return false
}

// ToolRecoverySelfRepairRung reports whether a tool node's Verified Action
// recovery can reach rung 3 — the self-repair generation, a DIRECT model
// call on the recovery spec with no tool surface.
//
// The ONE predicate for that question, read by dispatch's own gate
// (model.ClawExecutor.executeVerifiedToolNode), by the launch walks that
// price the rung's provider (model.recoveryRungsOf) and by the graph
// property (NodeUsesLLM). Three conditions, each dispatch's own: the rungs
// exist under `policy: recover` alone — an inert `recovery:` block (C106)
// spends nothing; the bound must be budgeted; and the recipe must be one a
// corrected command can replace. Read with any of the three dropped, a
// launch is refused by name for a provider no rung can ever spend.
func ToolRecoverySelfRepairRung(n Node) bool {
	t, ok := n.(*ToolNode)
	return ok && t.Policy == PolicyRecover && t.Recovery != nil &&
		t.Recovery.MaxRepairAttempts > 0 && ToolRecipeSelfRepairable(t)
}
