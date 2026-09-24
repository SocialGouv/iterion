package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// `fallbacks:` diagnostics (ADR-087).
const (
	DiagFallbackMalformed   DiagCode = "C173" // a fallback route declares nothing routable, or changes backend without a model (error)
	DiagFallbackUnknownOn   DiagCode = "C175" // unknown `on:` trigger token (warning)
	DiagFallbackUnsafeCross DiagCode = "C176" // a capability crossing that cannot degrade safely (error)
	DiagFallbackDrift       DiagCode = "C177" // a capability that silently changes across the chain (warning)
)

// KnownFallbackTriggers is the soft set of `on:` tokens. It mirrors
// delegate.FallbackCategory, deliberately duplicated as literals rather
// than imported: pkg/dsl/ir must not depend on pkg/backend/delegate (the
// package layout forbids it). Unknown tokens WARN rather than fail so an out-of-tree
// runtime can extend the vocabulary.
//
// `unclassified` is absent on purpose: it is not a condition an author
// can name, it is the absence of one. The runtime always routes on it
// (refusing would strand a run on exactly the failures iterion could not
// describe), which makes naming it meaningless.
var KnownFallbackTriggers = map[string]bool{
	"usage_window":        true,
	"auth":                true,
	"unavailable":         true,
	"transient_exhausted": true,
	"any":                 true,
}

// gateEnforcingModes are the permission modes each backend can enforce. A
// `fallbacks:` route on any other backend/mode pair would
// run the node UNGATED — the anti-prompt-injection boundary silently
// disappearing at the moment the run is under stress.
//
// Membership is earned by a live denial, never declared: grok and kimi are
// here because a real model's real tool call was blocked by iterion's own
// hook process, with a filesystem sentinel — not model prose — as the
// authority (e2e/live_feat_permission_kimi_test.go,
// e2e/live_feat_permission_grok_test.go). Both are deny-only: their hook is
// an EXTERNAL process, so it can hard-deny but cannot pause the parent
// iterion run the way claude_code's in-process hook does, and `ask` would
// therefore silently become `deny`.
var gateEnforcingModes = map[string]map[string]bool{
	"claude_code": {"ask": true, "deny": true},
	"claw":        {"ask": true, "deny": true},
	"pi":          {"ask": true, "deny": true},
	"grok":        {"deny": true},
	"kimi":        {"deny": true},
}

// reasoningEffortBackends are the backends that carry a
// reasoning-effort dial. Deliberately its OWN set rather than a reuse
// of gateEnforcingModes: that set answers "can this backend enforce
// the permission gate", and the two memberships differ — grok passes
// `--reasoning-effort` and codex has `model_reasoning_effort`, so
// reusing the gate set made C177 assert a fact the engine contradicts.
// A diagnostic that is wrong teaches authors to ignore it.
var reasoningEffortBackends = map[string]bool{
	"claude_code": true,
	"claw":        true,
	"pi":          true,
	"grok":        true,
	"codex":       true,
	"opencode":    true,
}

// clawBackendName is the literal value of the in-process backend.
// Hardcoded because ir must not import pkg/backend/delegate.
const clawBackendName = "claw"

// validateFallbacks checks every LLM node's `fallbacks:` block.
//
// Two of the checks are ERRORS rather than warnings, and both are cases
// where the degraded run would be silently WRONG rather than merely
// worse:
//
//   - a route that changes backend without pinning its own `model:` —
//     the model-spec forms are mutually incompatible (claw wants
//     `provider/model`, claude_code strips only an `anthropic/` prefix
//     and hard-fails on anything else), so the inherited model cannot
//     be valid on the new backend;
//   - a route that crosses the claw⇄CLI boundary on a node with an
//     EMPTY `tools:` list — the list inverts meaning there (empty means
//     zero tools on claw, and the FULL unrestricted native toolset on a
//     CLI backend under bypassPermissions), so a read-only reviewer
//     would become an agent able to edit the code it is judging.
//
// Everything else is a warning: the run proceeds, degraded but visible.
func (c *compiler) validateFallbacks(w *Workflow) {
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		fbs := nn.GetFallbacks()
		f := nn.GetLLMFields()
		kind, id := nn.NodeKind().String(), nn.NodeID()
		nodeBackend := effectiveNodeBackend(f.Backend, w.DefaultBackend)
		effectivePermission := EffectivePermission(nn.GetPermission(), w.Permission)

		// The primary route is a capability crossing too. Screening only
		// fallbacks let `backend: grok` + `permission: deny` compile and run
		// silently ungated — worse than a loud C176 refusal.
		if nodeBackend != "" {
			if reason := UngatedCrossingReasonForAskRules(nodeBackend, effectivePermission, EffectiveAskRules(nn, w)); reason != "" {
				c.errorfAt(DiagFallbackUnsafeCross, id, "",
					"%s %q: primary route %s", kind, id, reason)
			}
		}
		// An external-hook backend cannot be gated inside a container: the CLI
		// home and the hook binary are host-side. That refusal is correct, but
		// it fires at the AGENT NODE, mid-run — and the shipped default is
		// `sandbox: auto`, so the common shape (no `sandbox:` block) reaches it
		// every time. Warn at compile time so the operator learns the coupling
		// before launching, not after. A warning rather than an error because
		// `ITERION_SANDBOX_DEFAULT=none` and `--sandbox none` both make the run
		// legal without the workflow saying anything.
		c.checkGatedCLIBackendSandbox(kind, id, nn, nodeBackend, effectivePermission, w)

		if len(fbs) == 0 {
			continue
		}

		seen := map[string]bool{}
		for i, fb := range fbs {
			c.checkFallbackShape(kind, id, fb, seen)
			c.checkFallbackAction(kind, id, fb, i == len(fbs)-1)
			c.checkFallbackWhen(w, kind, id, fb)
			c.checkFallbackTriggers(kind, id, fb)
			c.checkFallbackCrossing(kind, id, fb, nn, nodeBackend, w.Permission, EffectiveAskRules(nn, w))
		}
	}
}

// externalHookGateBackends are the gate-enforcing backends whose hook is an
// out-of-process CLI hook, and which therefore need a host-side run. Kept
// separate from gateEnforcingModes: that set answers "can this backend enforce
// the gate at all", this one answers "what does enforcing it cost the run".
var externalHookGateBackends = map[string]bool{"grok": true, "kimi": true}

// checkGatedCLIBackendSandbox warns when a gated node routes to an
// external-hook backend on a workflow that has not opted out of the sandbox.
func (c *compiler) checkGatedCLIBackendSandbox(kind, id string, nn LLMNode, nodeBackend, permission string, w *Workflow) {
	mode := strings.ToLower(strings.TrimSpace(permission))
	if mode == "" || mode == "off" {
		return
	}
	// nodeSandboxSpec (validate_sandbox.go) takes a Node; every LLMNode is one.
	if sandboxOptsOut(nodeSandboxSpec(nn.(Node))) || sandboxOptsOut(w.Sandbox) {
		return
	}
	routes := []string{}
	if externalHookGateBackends[nodeBackend] {
		routes = append(routes, nodeBackend)
	}
	for _, fb := range nn.GetFallbacks() {
		if route := sourceBackend.routeName(fb.Backend); externalHookGateBackends[route] {
			routes = append(routes, route)
		}
	}
	if len(routes) > 0 {
		c.warnfAt(DiagGatedCLIBackendSandbox, id, "",
			"%s %q: permission: %s is enforced on %s through a host-side CLI hook, which a sandboxed run cannot reach — and the shipped default is sandbox: auto, so this node will FAIL at run time unless the workflow declares sandbox: none (or the run is launched with --sandbox none / ITERION_SANDBOX_DEFAULT=none)",
			kind, id, mode, strings.Join(dedupeStrings(routes), ", "))
	}

	// claw enforces the gate sandboxed too (the policy crosses the IPC as
	// a pre-task envelope) — EXCEPT an Ask decision, which has no seam to
	// pause the parent run from inside the container. A policy that can
	// ever produce one (mode ask, or any explicit ask rule, which
	// outranks mode deny) fails a sandboxed claw route at run time; warn
	// here so the coupling is learned before launch. Same
	// warning-not-error rationale as above: --sandbox none and
	// ITERION_SANDBOX_DEFAULT=none make the run legal without the
	// workflow saying anything.
	askRules := EffectiveAskRules(nn, w)
	if mode == "ask" || len(askRules) > 0 {
		clawRoutes := []string{}
		if nodeBackend == clawBackendName {
			clawRoutes = append(clawRoutes, nodeBackend)
		}
		for _, fb := range nn.GetFallbacks() {
			if sourceBackend.routeName(fb.Backend) == clawBackendName {
				clawRoutes = append(clawRoutes, clawBackendName)
			}
		}
		if len(clawRoutes) > 0 {
			c.warnfAt(DiagGatedCLIBackendSandbox, id, "",
				"%s %q: the permission policy can produce an Ask decision (mode %s%s), which a sandboxed claw route cannot pause for — this node will FAIL at run time on %s unless the workflow declares sandbox: none (or the run is launched with --sandbox none / ITERION_SANDBOX_DEFAULT=none)",
				kind, id, mode, askRuleSuffix(len(askRules)), strings.Join(dedupeStrings(clawRoutes), ", "))
		}
	}
}

// askRuleSuffix names the ask-rule contribution in the C136 message.
func askRuleSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" + %d ask rule(s)", n)
}

// sandboxOptsOut reports whether a spec explicitly declines the sandbox.
func sandboxOptsOut(spec *SandboxSpec) bool {
	return spec != nil && strings.EqualFold(strings.TrimSpace(spec.Mode), "none")
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// checkFallbackAction validates the `action:` field. A skip route is
// terminal by definition, so declaring anything after it is an authoring
// mistake the run could never reach, and declaring a backend/model on it
// contradicts what it does.
func (c *compiler) checkFallbackAction(kind, id string, fb Fallback, isLast bool) {
	switch fb.Action {
	case "":
		return
	case FallbackActionSkip:
	default:
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s declares unknown action %q; the only action is %q (a terminal degrade — omit action: for an ordinary route)",
			kind, id, fallbackLabel(fb), fb.Action, FallbackActionSkip)
		return
	}
	if fb.Backend != "" || fb.Model != "" || fb.Provider != "" || fb.Metered {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s declares action: skip together with a backend/model/provider/metered — a skip route executes and spends nothing, so the route fields are dead config hiding what actually happens",
			kind, id, fallbackLabel(fb))
	}
	if !isLast {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s declares action: skip but is not the LAST route — skip is terminal, so every route after it is unreachable",
			kind, id, fallbackLabel(fb))
	}
}

// checkFallbackWhen validates that a route's `when:` gate parses, only
// reads vars — the one namespace resolvable at dispatch time, where the
// gate is evaluated (a route has no node input or outputs of its own) —
// and only DECLARED vars: at runtime an absent var evaluates to nil →
// false, so a typo would silently disarm the policy the route encodes.
func (c *compiler) checkFallbackWhen(w *Workflow, kind, id string, fb Fallback) {
	if fb.When == "" {
		return
	}
	astExpr, err := expr.Parse(fb.When)
	if err != nil {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s has an unparseable when: expression (note: string literals use single quotes, e.g. vars.policy == 'skip'): %v",
			kind, id, fallbackLabel(fb), err)
		return
	}
	for _, ref := range astExpr.Refs() {
		if ref.Namespace != "vars" {
			c.errorfAt(DiagFallbackMalformed, id, "",
				"%s %q: fallback %s when: references %s.%s — a route gate is evaluated at dispatch, where only vars.* resolves",
				kind, id, fallbackLabel(fb), ref.Namespace, strings.Join(ref.Path, "."))
			continue
		}
		if len(ref.Path) > 0 && w != nil {
			if _, ok := w.Vars[ref.Path[0]]; !ok {
				c.errorfAt(DiagFallbackMalformed, id, "",
					"%s %q: fallback %s when: targets undeclared variable %q — at run time an absent var reads as false and silently disarms the route",
					kind, id, fallbackLabel(fb), ref.Path[0])
			}
		}
	}
}

// checkFallbackShape validates the route on its own terms.
func (c *compiler) checkFallbackShape(kind, id string, fb Fallback, seen map[string]bool) {
	label := fallbackLabel(fb)
	if fb.Backend == "" && fb.Model == "" && fb.Provider == "" && fb.Action == "" {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s declares neither backend, model, provider nor action — it would re-issue the identical call that just failed",
			kind, id, label)
		return
	}
	if fb.Name != "" {
		if seen[fb.Name] {
			c.errorfAt(DiagFallbackMalformed, id, "",
				"%s %q: duplicate fallback name %q — names are the stable id a fall-through is reported by, so they must be unique",
				kind, id, fb.Name)
		}
		seen[fb.Name] = true
	}
	// A backend change with an inherited model cannot work: the
	// model-spec forms are mutually incompatible across backends.
	if route := sourceBackend.routeName(fb.Backend); route != "" && fb.Model == "" {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s switches to backend %q without its own model: — model specs are not portable across backends (claw needs `provider/model`, claude_code accepts only a bare id or an `anthropic/` prefix)",
			kind, id, label, route)
	}
}

// checkFallbackTriggers validates the `on:` filter vocabulary.
func (c *compiler) checkFallbackTriggers(kind, id string, fb Fallback) {
	for _, on := range fb.On {
		if strings.Contains(on, "${") {
			continue // resolved at run time
		}
		if !KnownFallbackTriggers[on] {
			c.warnfAt(DiagFallbackUnknownOn, id, "",
				"%s %q: fallback %s declares unknown trigger %q; known triggers are usage_window, auth, unavailable, transient_exhausted, any",
				kind, id, fallbackLabel(fb), on)
		}
	}
}

// checkFallbackCrossing validates what the route changes about the
// node's capabilities, using the same predicates the launch-time
// run-level route is screened by — so an operator cannot reach through
// `--fallback` a crossing the compiler refuses in the .bot.
func (c *compiler) checkFallbackCrossing(kind, id string, fb Fallback, nn LLMNode, nodeBackend, workflowPermission string, askRules []string) {
	// The route's backend, read as the run reads it: a dial's default is
	// the route this chain takes, and screening only the literal spelling
	// left every dialled route unscreened.
	routeBackend := sourceBackend.routeName(fb.Backend)
	if routeBackend == "" {
		return
	}
	label := fallbackLabel(fb)

	// The permission gate is the anti-prompt-injection boundary. Whether
	// a ROUTE can enforce it is a property of the route's own backend,
	// so this check does NOT depend on the node's backend being
	// statically knowable — the auto-resolved shape is the shipped
	// default and must not escape it.
	if reason := UngatedCrossingReasonForAskRules(routeBackend, EffectivePermission(nn.GetPermission(), workflowPermission), askRules); reason != "" {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s %s", kind, id, label, reason)
	}

	// The remaining checks compare the two backends, so they need the
	// node's own to be knowable.
	if nodeBackend == "" {
		return
	}
	if reason := toolsInversionReason(nodeBackend, routeBackend, nn.GetTools()); reason != "" {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s %s", kind, id, label, reason)
	}
	if reason := sessionContinuityCrossingReason(nn.GetSession(), nodeBackend, routeBackend); reason != "" {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s %s", kind, id, label, reason)
	}

	// A route's ability to RESOLVE the node's declared tools is checked in
	// validate_tools.go (C135): it is a property of the list rather than of
	// the crossing, and it applies to the node's own backend too.

	// Reasoning effort degrades rather than misleads, so it warns.
	if effort := nn.GetLLMFields().ReasoningEffort; effort != "" && !reasoningEffortBackends[routeBackend] {
		c.warnfAt(DiagFallbackDrift, id, "",
			"%s %q: fallback %s runs on backend %q, which has no reasoning-effort dial; the node's reasoning_effort: %s is ignored on that route",
			kind, id, label, routeBackend, effort)
	}
}

// EffectivePermission resolves a node's gate mode: its own override
// wins, an empty one inherits the workflow block — which is the
// DOCUMENTED place to declare it, and therefore the shape a check that
// reads only the node-level field silently misses.
func EffectivePermission(nodePermission, workflowPermission string) string {
	if p := strings.TrimSpace(nodePermission); p != "" {
		return p
	}
	return strings.TrimSpace(workflowPermission)
}

// EffectivePermissionRules resolves ONE permission rule list for a node:
// a non-empty node list REPLACES the workflow list of the same kind, an
// empty one inherits it. Replacement rather than union is the whole point
// of the per-node form — a union can only widen, and the shape that needs
// expressing (a converge node that must NOT hold the shell its reviewers
// need) is a narrowing.
//
// "Declared" is len > 0, not nil-vs-empty: the AST's JSON seam carries
// these lists with `omitempty`, which erases that distinction, so a rule
// built on it would hold in the .bot and break through the studio.
//
// This is the single implementation of the precedence. The compiler's
// screens and the executor's policy builder both call it, so a node whose
// rules the compiler judged can never be the node the runtime gates.
func EffectivePermissionRules(nodeRules, workflowRules []string) []string {
	if permissionRulesDeclared(nodeRules) {
		return nodeRules
	}
	return workflowRules
}

// permissionRulesDeclared is the single answer to "did the author declare
// this list". Both the resolver above and C111 read it, so "declared"
// cannot come to mean two things in the compiler and the runtime.
func permissionRulesDeclared(rules []string) bool { return len(rules) > 0 }

// EffectiveAskRules is EffectivePermissionRules for the ask list, taking
// the node and the workflow so no caller can pair a node with the wrong
// workflow list. Every admission screen that asks "can this route pause
// for this node's asks" reads it: passing w.PermissionAsk directly would
// make a node-declared ask: invisible to the screen, which is exactly the
// silent ungating C176 exists to prevent.
func EffectiveAskRules(nn LLMNode, w *Workflow) []string {
	if w == nil {
		return nn.GetPermissionAsk()
	}
	return EffectivePermissionRules(nn.GetPermissionAsk(), w.PermissionAsk)
}

// UngatedCrossingReason returns why a route may not serve a gated node,
// or "" when it may. Shared by C176 and every launch-time screen so an
// operator override cannot reach a crossing the compiler refuses.
func UngatedCrossingReason(routeBackend, permission string, hasAskRules bool) string {
	mode := strings.ToLower(strings.TrimSpace(permission))
	if mode == "" || mode == "off" {
		return ""
	}
	if gateEnforcingModes[routeBackend][mode] {
		if !hasAskRules || gateEnforcingModes[routeBackend]["ask"] {
			return ""
		}
		return fmt.Sprintf(
			"runs on backend %q, which can enforce permission: %s but cannot pause for the explicit ask: rules in force for this node — the run would not preserve the declared gate",
			routeBackend, mode)
	}
	return fmt.Sprintf(
		"runs on backend %q, which cannot enforce the effective permission: %s gate — the run would be UNGATED",
		routeBackend, permission)
}

// UngatedCrossingReasonForAskRules is the rules-aware variant used by every
// workflow admission surface. Kimi and Grok can enforce permission: deny but
// cannot pause for an ask. A Claw-only alias such as diagnostic_shell cannot
// be invoked by either CLI at all, so treating that particular ask as
// reachable would reject a safe route. Unknown, wildcard, MCP and native
// rule names remain reachable by default: this relaxation is fail-closed.
func UngatedCrossingReasonForAskRules(routeBackend, permission string, askRules []string) string {
	return UngatedCrossingReason(routeBackend, permission, askRulesReachableByBackend(routeBackend, askRules))
}

func askRulesReachableByBackend(routeBackend string, askRules []string) bool {
	if len(askRules) == 0 {
		return false
	}
	if !externalHookGateBackends[strings.ToLower(strings.TrimSpace(routeBackend))] {
		return true
	}
	for _, rule := range askRules {
		name, _, _ := strings.Cut(strings.TrimSpace(rule), "(")
		if !toolcatalog.IsClawOnlyAlias(name) {
			return true
		}
	}
	return false
}

// toolsInversionReason returns why a route may not cross the claw⇄CLI
// boundary on this node, or "" when it may.
//
// The two directions are not symmetric:
//
//   - claw → CLI un-restricts the node WHATEVER it declared. Under the
//     always-on bypassPermissions a CLI agent ignores the lowercase
//     `tools:` list and carries the full native toolset, so a reviewer
//     restricted to read_file gains Edit/Write the moment the chain
//     falls through — and the parallel-branch admission was already
//     computed on the claw reading.
//   - CLI → claw restricts, which is only a hazard when the list is
//     UNDECLARED: on claw that means zero tools, so the node becomes a
//     schema-shaped narrator with no way to verify anything. A DECLARED
//     `tools: []` says the same thing on both sides and is not an inversion
//     (a route that cannot receive the list at all is C270's subject).
//
// Both backends must be statically known for the comparison to mean
// anything.
func toolsInversionReason(nodeBackend, routeBackend string, tools []string) string {
	if nodeBackend == "" || routeBackend == "" {
		return ""
	}
	nodeClaw := nodeBackend == clawBackendName
	routeClaw := routeBackend == clawBackendName
	if nodeClaw == routeClaw {
		return ""
	}
	if nodeClaw {
		return "routes a claw node to a CLI backend, where the tools: list does not mean what it means on claw — claude_code narrows only its own 14-name native roster (every MCP tool and every name outside it survives), codex maps the list onto a sandbox mode, and pi/kimi/grok never receive it at all — so the route changes what this node can do, and the parallel-branch admission was already decided on the claw reading; declare the node on the CLI backend instead, or route to another claw model"
	}
	// A DECLARED list — `tools: []` included — says the same thing on both
	// sides, so there is nothing to invert. Only an UNDECLARED list does:
	// "everything" on the CLI side, "nothing" on claw. A declared-empty list
	// on a route that drops it is C270's subject, not this one.
	if toolcatalog.ToolsDeclared(tools) {
		return ""
	}
	return "crosses the claw⇄CLI boundary on a node with no tools: list — an undeclared list means NO tools on claw but the full unrestricted toolset on a CLI backend, so the route silently changes what this node can do; declare an explicit tools: list (`tools: []` for none)"
}

// ToolRestrictionLossReason reports the non-security capability drift the
// Studio must disclose when an explicit launch override moves a claw node to
// a CLI backend. Claw enforces the lowercase tools: list; CLI backends running
// under their native permission mode do not, so the shared permission gate
// may remain intact while this independent restriction layer disappears.
//
// Fallback validation uses the stricter toolsInversionReason because a
// fallback is automatic. A launch picker may still offer this deliberate
// crossing, but must never present it as capability-neutral.
func ToolRestrictionLossReason(nodeBackend, routeBackend string, tools []string) string {
	if nodeBackend != clawBackendName || routeBackend == clawBackendName || !toolcatalog.ToolsDeclared(tools) {
		return ""
	}
	if !toolcatalog.ReceivesToolList(routeBackend) {
		// The crossing that really loses the list, and the one worth the
		// loudest sentence: pi, kimi and grok never receive it, so an EMPTY
		// declaration — the strictest thing an author can write — becomes
		// that CLI's whole toolset.
		return "this claw node's tools: list is never passed to the selected backend, which runs its own full toolset whatever the list says — an empty list included; the permission gate remains active"
	}
	if len(tools) == 0 {
		// claude_code turns it into --disallowedTools over its whole native
		// roster and codex drops to the read-only sandbox: an empty
		// declaration is narrower there, not wider, so there is nothing to
		// disclose. (What still reaches such a node — MCP tools, and the
		// natives outside claudeNativeTools — is C270's and the docs' subject.)
		return ""
	}
	return "this claw node's tools: list is narrowed rather than enforced by the selected backend: claude_code removes only the native names the list does not carry (MCP tools, and Agent/TaskOutput/Monitor, survive it), codex maps the list onto a sandbox mode; the permission gate remains active"
}

// sessionContinuityCrossingReason refuses inherit / inherit_if_available /
// fork / persist when a fallback changes backend (ADR-087 + ADR-089).
func sessionContinuityCrossingReason(session SessionMode, nodeBackend, routeBackend string) string {
	if nodeBackend == "" || routeBackend == "" || nodeBackend == routeBackend {
		return ""
	}
	switch session {
	case SessionInherit, SessionInheritIfAvailable, SessionFork, SessionPersist:
		return fmt.Sprintf("changes backend from %q to %q on a node with session: %s — session continuity has no cross-backend meaning", nodeBackend, routeBackend, session)
	default:
		return ""
	}
}

// backendReader is how a backend field is read. Two readings exist, and
// the difference is which environment answers a `${…}`:
//
//   - sourceBackend — what the SOURCE declares. A dial is read by its
//     DEFAULT and never by the shell that happens to be compiling: a
//     compile verdict that moved with the ambient environment would make
//     one artifact compile on one machine and fail on another, and the
//     compiler here runs in the server and runner pods, neither of which
//     is the process that will dispatch the node.
//   - runBackend — what the RUN resolves, process environment included.
//     The launch-time screen uses it, because there the environment IS
//     the route.
//
// Before either existed, every screen keyed on the literal text, so a
// backend written `${ITERION_SEC_AUDIT_BACKEND:-claude_code}` was "no
// opinion" and `""` short-circuited every comparison: the `tools:`
// inversion, the session-continuity refusal, the effort drift and the
// primary-route gate check all silently stopped applying to it (#1389).
// A dial's default is exactly as knowable as a literal.
type backendReader struct {
	lookup func(string) string
	// asWritten keeps a reference nothing answers as written, so
	// `${X}` with no `:-` reads as a field the SOURCE does not decide
	// rather than as an absent one.
	asWritten bool
}

var (
	sourceBackend = backendReader{lookup: noEnvLookup, asWritten: true}
	runBackend    = backendReader{lookup: lookupEnv}
)

// field answers what one backend field resolves to, and whether anything
// at this level DECIDES it:
//
//	("claw", true)  — a name
//	("", true)      — absent or `auto`: the next step of the chain decides
//	("", false)     — present, and this reading cannot decide it: a
//	                  `{{vars.x}}` a launch may override, a `${X}` or a
//	                  bare `$X` the source does not answer. Which branch
//	                  the run takes is not knowable here — an undeclared
//	                  `{{vars.zz}}` reaches the registry as that text,
//	                  while a set `${X}` names a backend — so a screen may
//	                  neither name one nor substitute the workflow default.
func (r backendReader) field(name string) (string, bool) {
	if resolved, ok := r.resolve(name); ok {
		if resolved == "auto" {
			return "", true
		}
		return resolved, true
	}
	return "", false
}

// resolve expands a backend field and says whether the result is a name at
// all. A reference the reading could not answer — `${X}` with no `:-`, a
// bare `$X`, a `{{vars.x}}` a launch may override, an unterminated `${` —
// is NOT a backend called that: the text is what the source wrote, and
// naming a backend `$MY_BACKEND` refused a gated workflow that was fine
// and passed a claw⇄CLI crossing that was not.
func (r backendReader) resolve(name string) (string, bool) {
	expanded, _ := expandWithDefault(name, r.lookup, expandPolicy{keepUnresolved: r.asWritten})
	expanded = strings.TrimSpace(expanded)
	if strings.ContainsRune(expanded, '$') || strings.Contains(expanded, "{{") {
		return "", false
	}
	return expanded, true
}

// name is field without the decision bit, for a site that only screens a
// backend it can name.
func (r backendReader) name(field string) string {
	n, _ := r.field(field)
	return n
}

// routeName is the reading a ROUTE's backend takes, where `auto` is a NAME
// rather than a step of a chain.
//
// The difference is the runtime's: resolveChain normalises `auto` on a
// route's `provider:` and never on its `backend:`, so an explicit
// `backend: "auto"` reaches the registry, which has no backend by that
// name — the route dies at the moment the chain is needed. Reading it as
// "the chain decides" would take every screen off a route that cannot run.
func (r backendReader) routeName(field string) string {
	n, _ := r.resolve(field)
	return n
}

// effective mirrors the runtime precedence: the node's own `backend:`
// wins, and only an ABSENT or `auto` one falls through to the workflow
// default — exactly the two cases resolveBackendName falls through on.
func (r backendReader) effective(nodeBackend, workflowDefault string) string {
	if n, decides := r.field(nodeBackend); n != "" || !decides {
		return n
	}
	return r.name(workflowDefault)
}

// SourceBackendName is the source reading of ONE backend field, for a host
// outside this package: the Studio's launch preview screens a node the same
// way the compiler does, and reading the raw text there made a dialled node
// look like a backend nobody has — so the capability-drift disclosure the
// picker owes the operator simply disappeared. "" means the source does not
// name a backend (empty, `auto`, or a reference this reading cannot answer).
func SourceBackendName(name string) string {
	return sourceBackend.name(name)
}

// SourceNodeBackendName is the source reading of a node's route, for a host
// outside this package: the node's own `backend:` wins, and only an absent or
// `auto` one falls through to the workflow's `default_backend:` — each field
// read as SourceBackendName reads it. "" means the source does not decide the
// route.
func SourceNodeBackendName(nodeBackend, workflowDefault string) string {
	return effectiveNodeBackend(nodeBackend, workflowDefault)
}

// effectiveNodeBackend is the compile-time reading of a node's route.
//
// Copied from validateCommand rather than validateProviders, which
// reads f.Backend raw and therefore under-fires on every node that
// inherits `default_backend:`.
func effectiveNodeBackend(nodeBackend, workflowDefault string) string {
	return sourceBackend.effective(nodeBackend, workflowDefault)
}

// fallbackLabel renders a route for a diagnostic message.
func fallbackLabel(fb Fallback) string {
	if fb.Name != "" {
		return fmt.Sprintf("%q", fb.Name)
	}
	return fmt.Sprintf("%q", strings.TrimSpace(fb.Backend+" "+fb.Model))
}
