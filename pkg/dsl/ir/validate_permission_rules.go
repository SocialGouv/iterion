package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// Permission rule-list diagnostics.
const (
	// DiagInvalidPermissionRule: a declared allow/ask/deny entry is not a
	// rule the gate's parser accepts (error).
	DiagInvalidPermissionRule DiagCode = "C154"
	// DiagPermissionClassRisk: a deny list leaves a shell-capable built-in
	// uncovered, or an allow row grants a whole tool class (warning).
	DiagPermissionClassRisk DiagCode = "C167"
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
	c.checkShellClassCoverage(w)

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

// checkShellClassCoverage warns (C167) on the two shapes the tool classes
// exist to close.
//
// FIRST, the uncovered-shell cell: a node's effective rules bound some of
// the shell-capable built-ins it can call but not all. The canonical shape
// is `deny: ["Bash"]` beside claw's `repl` — a second shell no spelling row
// can bound, because aliasing repl onto bash would let `allow: ["repl"]`
// grant the whole shell, and allow widens. What the author wrote degrades:
// the bounded tool is refused (or asked), the unnamed one falls through to
// the mode default — an approval prompt in ask mode, a silent pass wherever
// an allow row or the mode reaches it first. The warning names the
// uncovered members out of the registry's capability metadata and suggests
// the class row that bounds them, present and future.
//
// It fires only where the cell is real:
//
//   - the gate is ON (an inert list is C111's verdict, not a second warning
//     on the same defect);
//   - the node runs on claw, whose registry resolves the names (the one
//     backend toolcatalog.ConstrainsTools calls constraining);
//   - SOME bound is declared — with no deny and no ask rows at all the mode
//     default already bounds every unmatched tool, and the author wrote no
//     bound to be incomplete.
//
// A bound is a deny OR an ask row: an ask match pauses the run for a human
// in every mode (an explicit ask precedes the mode default), so an asked
// tool executes nothing unattended. An allow row bounds nothing.
//
// The granted half reads the same surface the run time does: a declared
// `tools:` list contributes its shell-capable members; no declaration on
// claw contributes the registry's full built-in set, every member included.
//
// Coverage is at the tool level (permission.RuleCovers): a row bounds the
// tools its matcher names, whatever (arg) pattern it carries. An argument
// scope is the author's finer contract over a tool; the warning exists for
// the tool NO row names at all.
//
// SECOND, the allow-class row: `allow:` widens, so an allow row naming a
// class grants every member the registry will ever tag — the risk is the
// author's to accept, and the warning says so (and points at `ask:`, which
// widens protectively: a future member is asked, not run). An ask row
// naming a class warns nothing: an approval gate over future members is the
// gate holding, not loosening. Nor does an allow row warn when the ask list
// beside it already holds a row of the same class — the ask decides first
// in Evaluate, every member call pauses, and the allow grants nothing the
// warning could describe.
func (c *compiler) checkShellClassCoverage(w *Workflow) {
	members, err := toolcatalog.ClassMembers(toolcatalog.ClassShell)
	if err != nil {
		return // the class table is a registry literal; unreachable, and an unknown class is C154's refusal upstream
	}
	classRule := toolcatalog.ClassRuleFor(toolcatalog.CapShellExec)

	// The allow-class cell, at the workflow level: the row's widening is a
	// property of the row, read wherever the workflow's own mode arms it.
	if gateModeIsOn(w.Permission) {
		for _, raw := range workflowRulesOfKind(w, "allow") {
			c.warnAllowClassRow(w, nil, raw, workflowRulesOfKind(w, "ask"))
		}
	}

	for _, n := range w.Nodes {
		tn, isTool := n.(*ToolNode)
		if isTool {
			// A Verified Action's rung-4 recovery agent carries `agent_tools:`
			// verbatim and inherits the workflow's rules — the same cell on the
			// surface that agent will hold: a declared list is its tool set,
			// an undeclared one the registry's ambient built-ins (the same
			// declared/ambient split the runtime resolves, and the node's own
			// arm below reads).
			if tn.Recovery == nil || !NodeUsesLLM(tn) {
				continue
			}
			if !gateModeIsOn(w.Permission) || !toolcatalog.ConstrainsTools(effectiveNodeBackend("", w.DefaultBackend)) {
				continue
			}
			bounds := make([]string, 0, len(w.PermissionDeny)+len(w.PermissionAsk))
			bounds = append(bounds, w.PermissionDeny...)
			bounds = append(bounds, w.PermissionAsk...)
			c.warnUncoveredShellTools(tn.ID, "tool", tn.Recovery.AgentTools, toolcatalog.ToolsDeclared(tn.Recovery.AgentTools), bounds, members, classRule)
			continue
		}
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		mode := EffectivePermission(nn.GetPermission(), w.Permission)
		if !gateModeIsOn(mode) {
			continue
		}
		// The node's own allow rows, under the mode that arms them.
		for _, raw := range nn.GetPermissionAllow() {
			c.warnAllowClassRow(w, nn, raw, EffectivePermissionRules(nn.GetPermissionAsk(), w.PermissionAsk))
		}
		backend := effectiveNodeBackend(nn.GetLLMFields().Backend, w.DefaultBackend)
		if !toolcatalog.ConstrainsTools(backend) {
			// The node does not run on claw, but a fallback route might — the
			// list is the SAME list there, and a route exists to serve when the
			// primary is already failing (the reach C135 and C159 cover).
			// Reported once, against the first claw route: every route shares
			// the list, so repeating it per route would say nothing new.
			for _, fb := range nn.GetFallbacks() {
				if !toolcatalog.ConstrainsTools(sourceBackend.routeName(fb.Backend)) {
					continue
				}
				deny := EffectivePermissionRules(nn.GetPermissionDeny(), w.PermissionDeny)
				ask := EffectivePermissionRules(nn.GetPermissionAsk(), w.PermissionAsk)
				bounds := make([]string, 0, len(deny)+len(ask))
				bounds = append(bounds, deny...)
				bounds = append(bounds, ask...)
				c.warnUncoveredShellTools(nn.NodeID(), nn.NodeKind().String(), nn.GetTools(), toolcatalog.ToolsDeclared(nn.GetTools()), bounds, members, classRule)
				break
			}
			continue
		}
		// The rules the node actually runs under: its own list of a kind when
		// declared — it REPLACES the workflow's — else the workflow's. Both
		// bounds count: deny refuses, ask pauses for a human in every mode.
		deny := EffectivePermissionRules(nn.GetPermissionDeny(), w.PermissionDeny)
		ask := EffectivePermissionRules(nn.GetPermissionAsk(), w.PermissionAsk)
		bounds := make([]string, 0, len(deny)+len(ask))
		bounds = append(bounds, deny...)
		bounds = append(bounds, ask...)
		c.warnUncoveredShellTools(nn.NodeID(), nn.NodeKind().String(), nn.GetTools(), toolcatalog.ToolsDeclared(nn.GetTools()), bounds, members, classRule)
	}
}

// warnUncoveredShellTools reports one node's uncovered shell-capable tools.
// declaredTools marks a `tools:` list the author wrote (a declared-empty
// list grants nothing); without it the node runs claw's ambient built-in
// set, every member included. bounds is the effective deny AND ask rows —
// the two kinds that stop an unattended execution.
func (c *compiler) warnUncoveredShellTools(nodeID, kind string, tools []string, declaredTools bool, bounds []string, members []string, classRule string) {
	if len(bounds) == 0 {
		return
	}
	granted := grantedShellMembers(tools, declaredTools, members)
	if len(granted) == 0 {
		return
	}
	var covered, uncovered []string
	for _, m := range granted {
		if denyCovers(bounds, m) {
			covered = append(covered, m)
			continue
		}
		uncovered = append(uncovered, m)
	}
	if len(uncovered) == 0 {
		return
	}
	// `tools: []` on claw is re-populated by the runtime (interaction,
	// auto_memory); none of those appends is shell-capable today, and the
	// declared-empty case returned above through grantedShellMembers.
	if len(covered) == 0 {
		c.warnfAt(DiagPermissionClassRisk, nodeID, "",
			"%s %q can call %s — the built-ins that execute code through a shell — and no deny or ask rule bounds any of them; a rule naming a tool bounds that tool, not the others that execute code the same way (claw's repl runs python, node and shell). Deny the class instead: %q covers %s, and a built-in registering with the capability later is covered with no workflow edit",
			kind, nodeID, quoteJoin(granted), classRule, quoteJoin(members))
		return
	}
	c.warnfAt(DiagPermissionClassRisk, nodeID, "",
		"%s %q bounds %s but can still call %s — the other built-ins that execute code through a shell; a rule naming a tool bounds that tool, not its class (claw's repl runs python, node and shell). Deny the class instead: %q covers %s, and a built-in registering with the capability later is covered with no workflow edit",
		kind, nodeID, quoteJoin(covered), quoteJoin(uncovered), classRule, quoteJoin(members))
}

// warnAllowClassRow reports one allow row that grants a whole class, at the
// workflow span or the node that declares it. `allow:` widens, so the row
// grants every member the registry will ever tag; ask widens protectively
// and is not warned. askRows is the ask list the row runs beside — the
// node's effective one at a node, the workflow's at the workflow. An ask row
// of the same class decides before allow in Evaluate, so every member call
// pauses and the allow row grants nothing: the widening the warning names is
// already held by the ask gate, and re-warning would describe a grant that
// does not happen.
func (c *compiler) warnAllowClassRow(w *Workflow, nn LLMNode, raw string, askRows []string) {
	class, ok := permission.RuleNamesClass(raw)
	if !ok {
		return
	}
	for _, ask := range askRows {
		if askClass, ok := permission.RuleNamesClass(ask); ok && askClass == class {
			return
		}
	}
	if nn == nil {
		c.warnfAtSpan(DiagPermissionClassRisk, c.workflowSpan(w.Name),
			"workflow %q: allow rule %q grants the whole tool class — the members registered now and every one the registry tags later, each auto-approved where no deny or ask row decides first. Move the grant to ask: to keep future members behind an approval, or keep allow: and accept the widening",
			w.Name, raw)
		return
	}
	c.warnfAt(DiagPermissionClassRisk, nn.NodeID(), "",
		"%s %q: allow rule %q grants the whole tool class — the members registered now and every one the registry tags later, each auto-approved where no deny or ask row decides first. Move the grant to ask: to keep future members behind an approval, or keep allow: and accept the widening",
		nn.NodeKind().String(), nn.NodeID(), raw)
}

// grantedShellMembers intersects a node's callable surface with the class —
// the members the node can actually reach, sorted. A declared list
// contributes its own entries (canonicalised); an undeclared one on claw
// contributes the whole registry.
func grantedShellMembers(tools []string, declared bool, members []string) []string {
	if !declared {
		return members
	}
	set := map[string]bool{}
	for _, t := range tools {
		set[toolcatalog.CanonicalToolName(strings.TrimSpace(t))] = true
	}
	var out []string
	for _, m := range members {
		if set[m] {
			out = append(out, m)
		}
	}
	return out
}

// denyCovers reports whether any bound rule (deny or ask) bounds the member
// at the tool level — the same matcher the gate itself answers a call with.
func denyCovers(rules []string, member string) bool {
	for _, raw := range rules {
		if permission.RuleCovers(raw, member) {
			return true
		}
	}
	return false
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
