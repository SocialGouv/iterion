package bots

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestEveryCapabilityGrantReachesTheModelOnItsClawRoutes holds one promise
// across the whole catalogue: a `capabilities:` grant is a surface the model
// can actually call, on every route the node may take.
//
// On claw the node's `tools:` list IS its whole surface. The MCP appends that
// turn a capability into callable tool names are each gated on an
// already-restricted list — `if HasBoardCapability(caps) && len(tools) > 0`
// (model.assembleEffectiveTools) — and claw resolves its ToolDefs behind the
// same `len(tools) > 0`, so a claw node that declares no tools lands with no
// tool loop at all: the grant, and the node's `tool_max_steps:`, are silent
// no-ops. On the CLI backends an empty list means "no restriction" instead,
// and the MCP server stays registered, so the grant arrives.
//
// Asked of the ENGINE, per route, not re-derived: `EffectiveToolNamesOnRoutes`
// is the seam pre-run admission reads, and `BoardToolsFor`/`RunToolsFor` are
// the same functions the append calls — so this guard reddens exactly when the
// append does not fire, and cannot drift from the rule that decides it.
//
// Per route rather than over their union: a node whose claw route holds
// nothing and whose CLI fallback holds everything would pass on the union
// while every run that takes the primary loses the capability.
func TestEveryCapabilityGrantReachesTheModelOnItsClawRoutes(t *testing.T) {
	var inert []string
	checked := 0
	for _, sw := range shippedWorkflows(t) {
		exec := model.NewProgramExecutor(sw.wf)
		ids := make([]string, 0, len(sw.wf.Nodes))
		for id := range sw.wf.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			node := sw.wf.Nodes[id]
			llm, ok := node.(ir.LLMNode)
			if !ok {
				continue
			}
			caps := llm.GetCapabilities()
			if caps == nil {
				caps = sw.wf.Capabilities
			}
			var owed []string
			if delegate.HasBoardCapability(caps) {
				owed = append(owed, delegate.BoardToolsFor(caps)...)
			}
			if delegate.HasRunsReadCapability(caps) {
				owed = append(owed, delegate.RunToolsFor(caps)...)
			}
			if len(owed) == 0 {
				continue
			}
			routes := append([]string{exec.EffectiveBackendName(node)}, exec.EffectiveFallbackBackends(node)...)
			for _, route := range routes {
				if route != delegate.BackendClaw {
					continue
				}
				checked++
				held := exec.EffectiveToolNamesOnRoutes(node, false, []string{route})
				var missing []string
				for _, want := range owed {
					if !slices.Contains(held, want) {
						missing = append(missing, want)
					}
				}
				if len(missing) > 0 {
					inert = append(inert, fmt.Sprintf("%s%s on %s: capabilities %v grant nothing (declared tools %v, effective %v, missing %v)",
						sw.prefix, id, route, caps, llm.GetTools(), held, missing))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no capability-granting claw route in the catalogue — this guard lost its subject; it must see at least review-pr/converge_gpt")
	}
	if len(inert) > 0 {
		t.Errorf("%d capability grant(s) the claw route cannot expose — declare the node's `tools:` list, which is its whole surface there, or drop the grant:\n  %s",
			len(inert), strings.Join(inert, "\n  "))
	}
	t.Logf("%d capability-granting claw route(s) checked", checked)
}
