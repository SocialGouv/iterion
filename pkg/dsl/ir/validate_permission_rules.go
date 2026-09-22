package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// Permission rule-list diagnostics.
const (
	// DiagInvalidPermissionRule: a declared allow/ask/deny entry is not a
	// rule the gate's parser accepts (error).
	DiagInvalidPermissionRule DiagCode = "C154"
)

// permissionRuleKinds names the three lists in the order they are declared,
// so every message about them reads the same way.
var permissionRuleKinds = []string{"allow", "ask", "deny"}

// nodePermissionRules returns a node's own list of the named kind.
func nodePermissionRules(nn LLMNode, kind string) []string {
	switch kind {
	case "allow":
		return nn.GetPermissionAllow()
	case "ask":
		return nn.GetPermissionAsk()
	default:
		return nn.GetPermissionDeny()
	}
}

// gateModeIsOn reports whether a resolved permission mode arms the gate.
// An unrecognised word is NOT on: C110 already refuses it, and at run time
// permission.ParseMode fails the node — so a reader that counted it as
// gated would describe a run that never happens.
func gateModeIsOn(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "ask", "deny":
		return true
	}
	return false
}

// gateModeIsKnown reports whether a mode is one of the accepted words
// (empty included, which means inherit).
func gateModeIsKnown(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "off", "ask", "deny":
		return true
	}
	return false
}

// validatePermissionRules answers two questions about every declared
// allow/ask/deny list.
//
// FIRST, is every entry a rule at all (C154, error). The check runs the
// gate's own parser, so it has no spelling to keep in step with; an
// unparseable rule fails the node the moment the gate is armed, after the
// run has paid for a workspace.
//
// SECOND, can the list ever decide a tool call (C111, warning)? It fails
// that in four ways, which are one defect class with four causes because
// the fix differs:
//
//   - the gate is off where the list lives — a node's own list under an
//     effective mode of off;
//   - the workflow declares lists but the graph has no reader at all;
//   - readers exist but none runs gated;
//   - a workflow list is SHADOWED — every reader declares its own list of
//     that kind, which REPLACES it. A shape that did not exist before
//     per-node lists did.
//
// Two properties keep the warnings from talking an author out of a live
// bound, and each was paid for:
//
//   - a READER is not only a declared agent/judge node. A `tool` node whose
//     Verified-Action recovery can reach its agent rung dispatches a
//     synthetic agent (executor_verified_action.go) that carries no lists of
//     its own and is therefore gated by the workflow's — the most dangerous
//     agent in the graph, since it runs after a deterministic recipe already
//     failed. NodeUsesLLM is the repo's own answer to "does this node reach a
//     model", and its ToolNode arm is exactly that rung;
//   - the SHADOW question is mode-INDEPENDENT. `--permission` can arm a node
//     the DSL leaves off (ITERION_PERMISSION cannot: mode resolution is
//     cmp.Or(override, node, workflow, env), so an explicit off outranks the
//     environment but not the flag), and that node
//     then inherits every workflow list it does not declare. So a reader
//     counts against shadowing whatever its mode says, which makes the
//     shadow verdict true under any override rather than true-under-the-DSL.
//
// The mode-dependent verdicts carry the override caveat in their own words
// instead.
func (c *compiler) validatePermissionRules(w *Workflow) {
	c.checkPermissionRuleSyntax(w)

	// An unresolvable mode makes every reachability verdict below rest on a
	// run that cannot start. C110 is what the author must fix first; C111
	// then says nothing about the workflow rather than guess a shape.
	unknownMode := !gateModeIsKnown(w.Permission)

	// readsWorkflowList[kind]: some reader would take the workflow's list of
	// that kind. Mode-independent, per the second property above.
	readsWorkflowList := map[string]bool{}
	readers, gatedReaders := 0, 0

	for _, n := range w.Nodes {
		if tn, ok := n.(*ToolNode); ok {
			if NodeUsesLLM(tn) {
				readers++
				for _, kind := range permissionRuleKinds {
					readsWorkflowList[kind] = true
				}
				if gateModeIsOn(w.Permission) {
					gatedReaders++
				}
			}
			continue
		}
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		readers++
		mode := EffectivePermission(nn.GetPermission(), w.Permission)
		if !gateModeIsKnown(mode) {
			unknownMode = true
			continue
		}

		declared := []string{}
		for _, kind := range permissionRuleKinds {
			if permissionRulesDeclared(nodePermissionRules(nn, kind)) {
				declared = append(declared, kind)
			} else {
				readsWorkflowList[kind] = true
			}
		}
		if !gateModeIsOn(mode) {
			if len(declared) > 0 {
				c.warnfAt(DiagPermissionRulesNoGate, nn.NodeID(), "",
					"%s %q declares %s permission rules but its effective permission gate is %s under the DSL; rules are inert unless --permission arms the gate (or ITERION_PERMISSION, where no mode is spelled — an explicit off outranks the environment)",
					nn.NodeKind(), nn.NodeID(), joinRuleKinds(declared), modeLabel(strings.ToLower(strings.TrimSpace(mode))))
			}
			continue
		}
		gatedReaders++
	}

	declaredAtWorkflow := []string{}
	for _, kind := range permissionRuleKinds {
		if permissionRulesDeclared(workflowRulesOfKind(w, kind)) {
			declaredAtWorkflow = append(declaredAtWorkflow, kind)
		}
	}
	if len(declaredAtWorkflow) == 0 || unknownMode {
		return
	}

	if readers == 0 {
		// The one verdict no run-time override can falsify: nothing in the
		// graph issues an LLM tool call, so nothing evaluates a policy.
		c.warnfAtSpan(DiagPermissionRulesNoGate, c.workflowSpan(w.Name),
			"workflow %q declares %s permission rules but no node issues LLM tool calls (no agent or judge node, and no tool node whose recovery reaches its agent rung); nothing can ever read them",
			w.Name, joinRuleKinds(declaredAtWorkflow))
		return
	}
	if gatedReaders == 0 {
		// Name what is off. Printing the workflow mode when the workflow IS
		// gated contradicts the clause it is attached to.
		why := fmt.Sprintf("the effective permission is %s", modeLabel(strings.ToLower(strings.TrimSpace(w.Permission))))
		if gateModeIsOn(w.Permission) {
			why = fmt.Sprintf("the workflow declares permission: %s but every reader overrides it to off", strings.ToLower(strings.TrimSpace(w.Permission)))
		}
		c.warnfAtSpan(DiagPermissionRulesNoGate, c.workflowSpan(w.Name),
			"workflow %q declares %s permission rules but no reader runs with the gate on under the DSL (%s); rules are inert unless --permission arms the gate (or ITERION_PERMISSION, where no mode is spelled — an explicit off outranks the environment)",
			w.Name, joinRuleKinds(declaredAtWorkflow), why)
		return
	}

	shadowed := []string{}
	for _, kind := range declaredAtWorkflow {
		if !readsWorkflowList[kind] {
			shadowed = append(shadowed, kind)
		}
	}
	if len(shadowed) > 0 {
		c.warnfAtSpan(DiagPermissionRulesNoGate, c.workflowSpan(w.Name),
			"workflow %q declares %s permission rules, but every reader declares its own %s, which REPLACES the workflow list; the workflow's rules are inert — remove them, or restate them in the node lists that need them",
			w.Name, joinRuleKinds(shadowed), joinRuleKinds(shadowed))
	}
}

// checkPermissionRuleSyntax refuses (C154) an entry the gate's parser
// cannot read, at every site a list may be declared. The runtime builds the
// policy from these exact strings through the same parser, so an entry that
// fails here fails the node at dispatch — later, and after the run has
// already spent.
func (c *compiler) checkPermissionRuleSyntax(w *Workflow) {
	report := func(at func(format string, args ...any), scope, kind, raw string, err error) {
		at("%s declares an unreadable %s permission rule %q: %v", scope, kind, raw, err)
	}
	for _, kind := range permissionRuleKinds {
		for _, raw := range workflowRulesOfKind(w, kind) {
			if err := permission.ValidateRule(raw); err != nil {
				report(func(format string, args ...any) {
					c.errorfAtSpan(DiagInvalidPermissionRule, c.workflowSpan(w.Name), format, args...)
				}, fmt.Sprintf("workflow %q", w.Name), kind, raw, err)
			}
		}
	}
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		kindName, id := nn.NodeKind().String(), nn.NodeID()
		for _, kind := range permissionRuleKinds {
			for _, raw := range nodePermissionRules(nn, kind) {
				if err := permission.ValidateRule(raw); err != nil {
					report(func(format string, args ...any) {
						c.errorfAt(DiagInvalidPermissionRule, id, "", format, args...)
					}, fmt.Sprintf("%s %q", kindName, id), kind, raw, err)
				}
			}
		}
	}
}

// workflowRulesOfKind returns the workflow's list of the named kind.
func workflowRulesOfKind(w *Workflow, kind string) []string {
	switch kind {
	case "allow":
		return w.PermissionAllow
	case "ask":
		return w.PermissionAsk
	default:
		return w.PermissionDeny
	}
}

// joinRuleKinds renders one or more rule-list names as `allow:`/`deny:`.
func joinRuleKinds(kinds []string) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k+":")
	}
	return strings.Join(out, "/")
}
