package model

import (
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// clawServesToolDefs reports whether the task's ToolDefs — the node's
// resolved tool list, where the board / runs grant tools ride — is
// non-empty. AllowedTools is the allowlist the CLI backends read; claw never
// builds its wire from it, so a claw task holding AllowedTools without
// ToolDefs serves none of them. Execute adds its own tools beside ToolDefs
// (codemode, the memory store), and none of them serves a capabilities
// grant, so for the inert-grant gate ToolDefs is the surface that counts;
// the gate and the Tools block read this one predicate so they never
// disagree.
func clawServesToolDefs(task delegate.Task) bool {
	return len(task.ToolDefs) > 0
}

// grantFamily is one capability grant whose tools buildTask appends to a
// restricted claw union: the label the diagnostics name it by, and the
// MCP FQNs the node's capabilities unlock in it.
type grantFamily struct {
	label string
	tools func(caps []string) []string
}

// clawGrantFamilies lists the grants a claw node's ToolDefs serve, in the
// fixed order the diagnostics name them.
var clawGrantFamilies = []grantFamily{
	{label: "board", tools: delegate.BoardToolsFor},
	{label: "runs read", tools: delegate.RunToolsFor},
}

// grantFamiliesNarrowedAway returns the labels of the grant families that
// held at least one tool in before and hold none in after — a grant a
// prompt command's `allowed-tools:` left with nothing to serve. Each FQN is
// folded to the ToolDef.Name spelling (mcp__iterion_board__x →
// mcp_iterion_board_x) with the same tool.FoldCLISpelling the narrowing
// applies. A family absent from before is not reported: the narrowing did
// not remove it.
func grantFamiliesNarrowedAway(before, after []delegate.ToolDef, caps []string) []string {
	var lost []string
	for _, family := range clawGrantFamilies {
		names := map[string]bool{}
		for _, fqn := range family.tools(caps) {
			names[tool.FoldCLISpelling(fqn)] = true
		}
		if len(names) == 0 {
			continue
		}
		if holdsAny(before, names) && !holdsAny(after, names) {
			lost = append(lost, family.label)
		}
	}
	return lost
}

// holdsAny reports whether defs carries a tool whose name is in names.
func holdsAny(defs []delegate.ToolDef, names map[string]bool) bool {
	for _, td := range defs {
		if names[td.Name] {
			return true
		}
	}
	return false
}
