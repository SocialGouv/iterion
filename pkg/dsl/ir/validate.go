package ir

import (
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ---------------------------------------------------------------------------
// Additional diagnostic codes for static validation (P2-02)
// ---------------------------------------------------------------------------

const (
	DiagSessionAfterConvergence  DiagCode = "C009" // session: inherit or fork on convergence point
	DiagMultipleDefaultEdges     DiagCode = "C010" // multiple unconditional edges from same non-fan_out source
	DiagAmbiguousCondition       DiagCode = "C011" // ambiguous conditional edges from same source
	DiagMissingFallback          DiagCode = "C012" // conditional edges with no default fallback
	DiagConditionNotBool         DiagCode = "C013" // when field is not boolean in output schema
	DiagConditionFieldNotFound   DiagCode = "C014" // when field not found in source output schema
	DiagUnreachableNode          DiagCode = "C016" // node unreachable from entry
	DiagHistoryRefNotInLoop      DiagCode = "C017" // outputs.<node>.history but node not in a loop
	DiagUndeclaredCycle          DiagCode = "C019" // cycle without a declared loop (infinite loop risk)
	DiagRoundRobinTooFewEdges    DiagCode = "C020" // round_robin router with fewer than 2 outgoing edges
	DiagLLMRouterTooFewEdges     DiagCode = "C021" // llm router with fewer than 2 outgoing edges
	DiagLLMRouterConditionEdge   DiagCode = "C022" // llm router edge has a 'when' condition
	DiagRouterLLMOnlyProperty    DiagCode = "C023" // LLM-only property on non-llm router
	DiagFanOutEachMissingOver    DiagCode = "C113" // fan_out_each router without an 'over:' array source (was C102, clashed with DiagInvalidRTK on main)
	DiagFanOutEachOnlyProperty   DiagCode = "C114" // 'over'/'as'/'key'/'depends_on' property on a non-fan_out_each router (was C103)
	DiagFanOutEachEdges          DiagCode = "C115" // fan_out_each router must have exactly one outgoing template edge (was C104)
	DiagUseUnknownGroup          DiagCode = "C116" // use references a group that is not declared (error)
	DiagUseParamMismatch         DiagCode = "C117" // use provides an unknown param, or omits a declared one (error)
	DiagForeachConflictsLoop     DiagCode = "C118" // edge combines `as foreach` with `as <loop>` (error)
	DiagSubbotNoSource           DiagCode = "C119" // subbot node without a `source:` child .bot (error)
	DiagInvalidReasoningEffort   DiagCode = "C027" // invalid reasoning_effort value (was C024, clashed with DiagDuplicateMCPServer)
	DiagUltracodeModelGate       DiagCode = "C089" // reasoning_effort: ultracode on a model that isn't claude-opus-4-8 (warning)
	DiagInvalidLoopIterations    DiagCode = "C026" // loop max_iterations must be >= 1
	DiagDuplicateWithKey         DiagCode = "C028" // duplicate with-mapping key across edges to same target
	DiagUnknownRefNode           DiagCode = "C029" // outputs ref to non-existent node (was C030, clashed with DiagCodexDiscouraged)
	DiagRefFieldNotInSchema      DiagCode = "C031" // outputs ref field not in output schema
	DiagRefNodeNoSchema          DiagCode = "C032" // outputs ref field on node without output schema
	DiagUndeclaredVar            DiagCode = "C033" // vars ref to undeclared variable
	DiagInputFieldNotInSchema    DiagCode = "C034" // input ref field not in input schema
	DiagUnknownResourceInNeeds   DiagCode = "C195" // needs: references a resource not declared in resources:
	DiagUnknownArtifact          DiagCode = "C035" // artifacts ref to unpublished artifact
	DiagRefNodeNotReachable      DiagCode = "C036" // outputs ref to node not reachable before consumer
	DiagNodeMaxTokensVsBudget    DiagCode = "C037" // node-level max_tokens exceeds workflow.budget.max_tokens
	DiagUnsupportedMCPAuth       DiagCode = "C038" // MCP server Auth.Type not supported (only "oauth2" is wired)
	DiagInvalidCompaction        DiagCode = "C043" // compaction.threshold or compaction.preserve_recent out of range
	DiagMemoryNotSupported       DiagCode = "C047" // memory: enabled on a backend that does not consume it (only claw does today)
	DiagMemoryMissingScope       DiagCode = "C048" // memory: enabled without a scope: name
	DiagArtifactLabelsNoPublish  DiagCode = "C049" // artifact_labels: set on a node with no publish: (nothing to attach to)
	DiagMemoryInvalidVisibility  DiagCode = "C170" // memory: unknown visibility value
	DiagMemoryVisibilityConflict DiagCode = "C171" // memory: visibility: with the legacy project_root:

	// Attachments diagnostics
	DiagDuplicateAttachment       DiagCode = "C050" // attachment name declared more than once
	DiagAttachmentVarConflict     DiagCode = "C051" // attachment name collides with a declared var
	DiagInvalidAttachmentMIME     DiagCode = "C052" // accept_mime entry not in type/subtype form
	DiagUnknownAttachment         DiagCode = "C053" // {{attachments.X}} but X not declared
	DiagAttachmentSubfieldUnknown DiagCode = "C054" // attachments.<name>.<subfield> sub-field unknown

	// Browser-pane diagnostics (PR 3 of the browser-simulation
	// feature). Reserve C060+ for future browser/Playwright checks.
	DiagPlaywrightNeedsBrowserImage DiagCode = "C060" // Playwright MCP server requires a browser-capable sandbox image

	// Presets diagnostics (in-source `presets:` block).
	DiagPresetUnknownVar   DiagCode = "C070" // preset references a variable not declared in vars:
	DiagPresetTypeMismatch DiagCode = "C071" // preset value type does not match the declared variable type
	DiagDuplicatePreset    DiagCode = "C072" // preset name declared more than once

	// Secrets diagnostics (in-source `secrets:` block).
	DiagDuplicateSecret   DiagCode = "C090" // secret name declared more than once
	DiagSecretVarConflict DiagCode = "C091" // secret name collides with a declared var
	DiagInvalidSecretHost DiagCode = "C092" // secret egress host scoping ill-formed (Layer 2)
	DiagUnknownSecret     DiagCode = "C093" // {{secrets.X}} but X not declared
	DiagInvalidSecretFile DiagCode = "C094" // file secret declaration is malformed
	DiagSecretSubfield    DiagCode = "C095" // unsupported {{secrets.X.<subfield>}}

	// Review-gate diagnostics (interaction: review).
	DiagReviewNeedsWorktree DiagCode = "C100" // interaction: review without worktree: auto — nothing to merge (error)
	DiagReviewURLUnknownRef DiagCode = "C101" // review_url references an output node that does not exist (warning)

	// Compress output-compression mode diagnostics.
	DiagInvalidCompress DiagCode = "C102" // compress: value not one of on|off|ultra (error)

	// Static cross-node typing diagnostics (Phase 2). These resist the
	// looseness that makes the rest of the validator a graph linter: they
	// fire ONLY on genuinely-typed slots (enum literals compared against an
	// enum-typed field, typed operands inside compute/when expressions),
	// never on template stringification. A json (= any) field or an
	// unknown ref always bails to "no opinion" so legitimate looseness
	// keeps passing.
	//
	// NOTE: an earlier draft also checked edge with-mapping keys/types
	// against the target node's input schema (C105/C106). That was dropped:
	// the runtime (engine.buildNodeInputRS) passes EVERY with-key through
	// verbatim and never validates node input against the declared input
	// schema — the schema is advisory, not a contract a with-mapping must
	// satisfy — so such a check rests on a false premise. C104/C105/C106
	// are intentionally left unallocated.
	DiagEnumLiteralMismatch     DiagCode = "C103" // comparison literal outside the target field's enum set (error)
	DiagExprOperandTypeMismatch DiagCode = "C107" // compute/when expression operands incompatible under the operator (warning)
	DiagWhenExprNotBoolish      DiagCode = "C108" // when-expression result clearly not bool-coercible (warning)
	DiagVarDefaultTypeMismatch  DiagCode = "C109" // a var's default literal type does not match its declared type (error)
	DiagInvalidPermission       DiagCode = "C110" // permission: value not one of off|ask|deny (error)
	DiagPermissionRulesNoGate   DiagCode = "C111" // allow/ask/deny rules declared but the resolved permission mode is "" or off (warning)
	DiagToolNodePermissionInert DiagCode = "C112" // permission: on a tool node — parsed but not enforced (warning)
)

// validate performs static validation on a compiled workflow.
// It is called after all nodes, edges, loops and schemas are compiled.
func (c *compiler) validate(w *Workflow) {
	if w == nil {
		return
	}

	c.validateInheritAtConvergence(w)
	c.validateEdgeRouting(w)
	c.validateRoundRobinEdges(w)
	c.validateLLMRouterEdges(w)
	c.validateFanOutEachEdges(w)
	c.validateConditionFields(w)
	c.validateExprTypes(w)
	c.validateDuplicateWithKeys(w)
	c.validateReachability(w)
	c.validateHistoryRefs(w)
	c.validateUndeclaredCycles(w)
	c.validateLoopIterations(w)
	c.validateReasoningEffort(w)
	c.validateSecrets(w)
	c.validateTemplateRefs(w)
	c.validateNodeMaxTokensVsBudget(w)
	c.validateMCPAuth(w)
	c.validateCompaction(w)
	c.validateMemory(w)
	c.validatePlaywrightMCP(w)
	c.validateCapabilities(w)
	c.validateProviders(w)
	c.validateCursorInvocations(w)
	c.validateReviewGates(w)
	c.validateCompress(w)
	c.validateResources(w)
	c.validatePermission(w)
	c.validateVerifiedActions(w)
	c.validateArtifactLabels(w)
}

// validateArtifactLabels warns (C049) when a node declares artifact_labels:
// but no publish: — the labels have no artifact to attach to (only a node's
// *published* output is labelled). Judge nodes never publish, so their
// artifact_labels are dropped at compile time and not checked here.
func (c *compiler) validateArtifactLabels(w *Workflow) {
	for _, n := range w.Nodes {
		if len(NodePublishLabels(n)) > 0 && NodePublish(n) == "" {
			c.warnfAt(DiagArtifactLabelsNoPublish, n.NodeID(), "",
				"%s %q declares artifact_labels but no publish: — the labels have nothing to attach to (only a node's published output is labelled)",
				n.NodeKind().String(), n.NodeID())
		}
	}
}

// validateCompress enforces that every compress value (workflow-level + every
// agent/judge/tool node) is one of the accepted barewords. A typo
// would silently fall back to "inherit" instead of compressing — so
// this is an ERROR, not a warning. Empty ("") means unset/inherit
// and is always valid; the comparison is case-insensitive and
// whitespace-trimmed.
//
// Kept inline (no import of pkg/backend/rewrite) so the dsl layer stays
// dependency-free; keep in sync with rewrite.IsValidValue.
func (c *compiler) validateCompress(w *Workflow) {
	valid := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "on", "off", "ultra":
			return true
		}
		return false
	}
	if !valid(w.Compress) {
		c.errorf(DiagInvalidCompress,
			"workflow %q has invalid compress %q; valid values are on, off, ultra",
			w.Name, w.Compress)
	}
	for _, n := range w.Nodes {
		var compress string
		var kind string
		switch nn := n.(type) {
		case LLMNode:
			compress, kind = nn.GetCompress(), nn.NodeKind().String()
		case *ToolNode:
			compress, kind = nn.Compress, "tool"
		default:
			continue
		}
		if !valid(compress) {
			c.errorf(DiagInvalidCompress,
				"%s %q has invalid compress %q; valid values are on, off, ultra",
				kind, n.NodeID(), compress)
		}
	}
}

// validatePermission enforces that every permission gate mode (workflow-level
// + every agent/judge/tool node override) is one of the accepted barewords
// off|ask|deny. Empty ("") means unset/inherit and is always valid; the
// comparison is case-insensitive and whitespace-trimmed (C110, error).
//
// It also warns (C111) when the workflow declares allow/ask/deny rules but the
// resolved workflow permission mode is "" or "off" — the rules are inert
// because the gate is disabled.
func (c *compiler) validatePermission(w *Workflow) {
	valid := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "off", "ask", "deny":
			return true
		}
		return false
	}
	if !valid(w.Permission) {
		c.errorf(DiagInvalidPermission,
			"workflow %q has invalid permission %q; valid values are off, ask, deny",
			w.Name, w.Permission)
	}
	for _, n := range w.Nodes {
		var perm string
		var kind string
		switch nn := n.(type) {
		case LLMNode:
			perm, kind = nn.GetPermission(), nn.NodeKind().String()
		case *ToolNode:
			perm, kind = nn.Permission, "tool"
		default:
			continue
		}
		if !valid(perm) {
			c.errorf(DiagInvalidPermission,
				"%s %q has invalid permission %q; valid values are off, ask, deny",
				kind, n.NodeID(), perm)
			continue
		}
		// C112: the gate evaluates LLM-issued tool calls; a tool node is a
		// direct, deterministic shell action (governed by the Verified
		// Action quad), so its permission: is parsed but never enforced.
		// Warn so an operator doesn't ship an inert security control.
		if kind == "tool" {
			if m := strings.ToLower(strings.TrimSpace(perm)); m == "ask" || m == "deny" {
				c.warnf(DiagToolNodePermissionInert,
					"tool node %q sets permission: %s, but the gate only governs agent/judge LLM tool calls; a tool node's permission is not enforced (use goal/postcondition/policy/recovery to gate the action)",
					n.NodeID(), m)
			}
		}
	}

	// C111: rules declared but the gate is disabled. The resolved workflow
	// mode is "" or "off" → the allow/ask/deny lists never take effect.
	mode := strings.ToLower(strings.TrimSpace(w.Permission))
	gateDisabled := mode == "" || mode == "off"
	hasRules := len(w.PermissionAllow) > 0 || len(w.PermissionAsk) > 0 || len(w.PermissionDeny) > 0
	if gateDisabled && hasRules {
		c.warnf(DiagPermissionRulesNoGate,
			"workflow %q declares allow/ask/deny permission rules but the permission gate is %s; rules are inert",
			w.Name, modeLabel(mode))
	}
}

// modeLabel renders an empty permission mode as "off (unset)" for a clearer
// diagnostic message.
func modeLabel(mode string) string {
	if mode == "" {
		return "off (unset)"
	}
	return mode
}

// validateReviewGates enforces the review-&-merge gate's preconditions.
// A review gate squash-merges the run's worktree during the human pause,
// so it is meaningless without worktree: auto (C100, error). Its optional
// review_url may reference an upstream node output; a dangling reference is
// a warning (C101) since the URL simply renders empty at runtime.
func (c *compiler) validateReviewGates(w *Workflow) {
	worktreeAuto := strings.EqualFold(strings.TrimSpace(w.Worktree), "auto")
	for _, node := range w.Nodes {
		h, ok := node.(*HumanNode)
		if !ok || h.Interaction != InteractionReview {
			continue
		}
		if !worktreeAuto {
			c.errorf(DiagReviewNeedsWorktree,
				"human %q uses interaction: review but the workflow does not declare worktree: auto — a review gate squash-merges the run's worktree, so there is nothing to merge without one",
				h.NodeID())
		}
		for _, ref := range h.ReviewURLRefs {
			if ref.Kind != RefOutputs || len(ref.Path) == 0 {
				continue
			}
			if _, exists := w.Nodes[ref.Path[0]]; !exists {
				c.warnf(DiagReviewURLUnknownRef,
					"human %q review_url references output of unknown node %q",
					h.NodeID(), ref.Path[0])
			}
		}
	}
}

// validateMemory enforces shape on the per-node `memory:` block and
// warns on backends that do not consume it. Scope is mandatory when
// enabled. C047 is a warning (run still proceeds); C048 is an error.
func (c *compiler) validateMemory(w *Workflow) {
	check := func(scope, id, backend string, m *Memory) {
		if m == nil || !m.Enabled {
			return
		}
		if m.Scope == "" {
			c.errorf(DiagMemoryMissingScope,
				"%s %q: memory: enabled requires a scope: name", scope, id)
		}
		if m.Visibility != "" {
			if !knownMemoryVisibilities[m.Visibility] {
				c.errorf(DiagMemoryInvalidVisibility,
					"%s %q: memory: unknown visibility %q (bot|project|cross_project|user|org|global)", scope, id, m.Visibility)
			}
			if m.ProjectRoot {
				c.errorf(DiagMemoryVisibilityConflict,
					"%s %q: memory: visibility: and the legacy project_root: are mutually exclusive", scope, id)
			}
		}
		if backend != "" && backend != "claw" {
			c.warnf(DiagMemoryNotSupported,
				"%s %q: memory: has NO effect on backend=%q — memory_read/memory_write/memory_list and autoload are claw-only; switch to backend: \"claw\" or remove the memory: block",
				scope, id, backend)
		}
	}
	for _, n := range w.Nodes {
		if nn, ok := n.(LLMNode); ok {
			check(nn.NodeKind().String(), nn.NodeID(), nn.GetLLMFields().Backend, nn.GetMemory())
		}
	}
}

// validatePlaywrightMCP checks that any declared MCP server which
// resembles the Playwright MCP package (npx + @playwright/mcp, or
// a `playwright-mcp`/`playwright_mcp` binary) is paired with a
// sandbox image that ships Chromium — but only when the workflow
// has actually opted into a sandbox. Workflows running on the host
// rely on the operator's own Chromium install (typical for
// dev-loop examples that use playwright_visual_qa or
// dogfood_editor_ui_loop) and we don't second-guess that.
//
// Catching the sandboxed case at compile time keeps the failure
// loud and obvious instead of surfacing as a cryptic mid-run error
// when the MCP child crashes on the first `browser_*` call.
func (c *compiler) validatePlaywrightMCP(w *Workflow) {
	// Skip when the workflow doesn't use a sandbox: host runs are
	// the operator's responsibility (they presumably ran
	// `playwright install chromium` ahead of time).
	if !w.Sandbox.IsActive() {
		return
	}
	for name, srv := range w.MCPServers {
		if srv == nil || !looksLikePlaywrightMCP(srv) {
			continue
		}
		if !sandboxHasBrowserImage(w.Sandbox) {
			c.errorf(
				DiagPlaywrightNeedsBrowserImage,
				"mcp_servers.%s: Playwright MCP requires a sandbox image that bundles Chromium "+
					"(e.g. ghcr.io/socialgouv/iterion-sandbox-browser); "+
					"workflow.sandbox.image is %q",
				name, sandboxImageOrEmpty(w.Sandbox),
			)
		}
	}
}

// looksLikePlaywrightMCP returns true when the server config looks
// like the official Playwright MCP package, or a wrapper that runs
// it. The matcher is conservative: false negatives are fine (the
// real failure happens at run time anyway), false positives would be
// disruptive (workflows that legitimately use a different "browser"
// MCP would be flagged), so we look for the very specific package
// signature.
func looksLikePlaywrightMCP(srv *MCPServer) bool {
	if srv == nil {
		return false
	}
	cmd := strings.ToLower(srv.Command)
	if strings.Contains(cmd, "playwright-mcp") || strings.Contains(cmd, "playwright_mcp") {
		return true
	}
	if cmd == "npx" {
		for _, arg := range srv.Args {
			lower := strings.ToLower(arg)
			if strings.Contains(lower, "@playwright/mcp") {
				return true
			}
		}
	}
	return false
}

// sandboxHasBrowserImage returns true when the sandbox image name
// suggests a browser-capable variant. The matcher is intentionally
// loose so internal forks (`my-corp-iterion-sandbox-browser:edge`)
// also satisfy it. Setting `image:` empty (or omitting the sandbox
// block entirely) yields false — Phase 0 sandbox modes (none/auto)
// don't ship Chromium today.
func sandboxHasBrowserImage(spec *SandboxSpec) bool {
	if spec == nil {
		return false
	}
	img := strings.ToLower(spec.Image)
	if img == "" {
		return false
	}
	return strings.Contains(img, "sandbox-browser") || strings.Contains(img, "sandbox-full-browser")
}

func sandboxImageOrEmpty(spec *SandboxSpec) string {
	if spec == nil {
		return ""
	}
	return spec.Image
}

// validateCompaction enforces the value ranges for the compaction block at
// both workflow and per-node level: threshold must be in (0, 1] and
// preserve_recent must be >= 1 when set. A 0 value means "inherit" and is
// always accepted — only out-of-range explicit values are flagged.
func (c *compiler) validateCompaction(w *Workflow) {
	check := func(scope, id string, cp *Compaction) {
		if cp == nil {
			return
		}
		if cp.Threshold != 0 && (math.IsNaN(cp.Threshold) || math.IsInf(cp.Threshold, 0) || cp.Threshold <= 0 || cp.Threshold > 1) {
			c.errorf(DiagInvalidCompaction, "%s %q: compaction.threshold must be in (0, 1], got %g", scope, id, cp.Threshold)
		}
		if cp.PreserveRecent < 0 {
			c.errorf(DiagInvalidCompaction, "%s %q: compaction.preserve_recent must be >= 1 when set (0 = inherit), got %d", scope, id, cp.PreserveRecent)
		}
	}
	check("workflow", w.Name, w.Compaction)
	for _, n := range w.Nodes {
		if nn, ok := n.(LLMNode); ok {
			check(nn.NodeKind().String(), nn.NodeID(), nn.GetCompaction())
		}
	}
}

// ---------------------------------------------------------------------------
// C038 — MCP server Auth.Type validation
// ---------------------------------------------------------------------------

// validateMCPAuth catches workflows that declare an MCP server with an
// unsupported Auth.Type at compile time, instead of waiting for runtime
// init to fail with the same message.
func (c *compiler) validateMCPAuth(w *Workflow) {
	if w == nil {
		return
	}
	check := func(name string, server *MCPServer) {
		if server == nil || server.Auth == nil {
			return
		}
		a := server.Auth
		if a.Type == "" {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: auth block missing 'type'", name)
			return
		}
		if a.Type != "oauth2" {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: auth type %q is not supported (only \"oauth2\" is wired)", name, a.Type)
			return
		}
		if a.AuthURL == "" {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: oauth2 auth requires 'auth_url'", name)
		} else if err := validateHTTPURL(a.AuthURL); err != nil {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: invalid 'auth_url' %q: %v", name, a.AuthURL, err)
		}
		if a.TokenURL == "" {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: oauth2 auth requires 'token_url'", name)
		} else if err := validateHTTPURL(a.TokenURL); err != nil {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: invalid 'token_url' %q: %v", name, a.TokenURL, err)
		}
		if a.RevokeURL != "" {
			if err := validateHTTPURL(a.RevokeURL); err != nil {
				c.errorf(DiagUnsupportedMCPAuth,
					"mcp server %q: invalid 'revoke_url' %q: %v", name, a.RevokeURL, err)
			}
		}
		if a.ClientID == "" {
			c.errorf(DiagUnsupportedMCPAuth,
				"mcp server %q: oauth2 auth requires 'client_id'", name)
		}
	}
	for name, server := range w.MCPServers {
		check(name, server)
	}
	for name, server := range w.ResolvedMCPServers {
		// Skip resolved entries already covered by the explicit map
		// to avoid duplicate diagnostics on the same source.
		if _, dup := w.MCPServers[name]; dup {
			continue
		}
		check(name, server)
	}
}

// ---------------------------------------------------------------------------
// C037 — per-node max_tokens vs workflow budget
// ---------------------------------------------------------------------------

// validateNodeMaxTokensVsBudget warns when an LLM node's per-node max_tokens
// exceeds the workflow-level Budget.MaxTokens cap. Not blocking — the node may
// still legitimately want a larger ceiling, but it signals likely budget
// pressure to the author.
func (c *compiler) validateNodeMaxTokensVsBudget(w *Workflow) {
	if w == nil || w.Budget == nil || w.Budget.MaxTokens <= 0 {
		return
	}
	cap := w.Budget.MaxTokens
	checkLLM := func(id string, mt int) {
		if mt > 0 && mt > cap {
			c.warnf(DiagNodeMaxTokensVsBudget,
				"node %q has max_tokens=%d which exceeds workflow.budget.max_tokens=%d", id, mt, cap)
		}
	}
	for _, n := range w.Nodes {
		switch nd := n.(type) {
		case LLMNode:
			checkLLM(nd.NodeID(), nd.GetLLMFields().MaxTokens)
		case *RouterNode:
			if nd.RouterMode == RouterLLM {
				checkLLM(nd.ID, nd.MaxTokens)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// C009 — session: inherit/fork forbidden on convergence points
// ---------------------------------------------------------------------------

func (c *compiler) validateInheritAtConvergence(w *Workflow) {
	// Only check nodes explicitly marked with await — they are declared
	// convergence points. Implicit multi-source detection (e.g. loop
	// re-entry) is left to runtime since static analysis can't distinguish
	// parallel convergence from sequential re-entry.
	for nodeID, node := range w.Nodes {
		var awaitMode AwaitMode
		var session SessionMode
		switch n := node.(type) {
		case LLMNode:
			awaitMode, session = n.GetAwaitMode(), n.GetSession()
		case *HumanNode:
			awaitMode = n.AwaitMode
		case *ToolNode:
			awaitMode, session = n.AwaitMode, n.Session
		default:
			continue
		}
		if awaitMode == AwaitNone {
			continue
		}
		if session == SessionInherit || session == SessionInheritIfAvailable || session == SessionFork {
			c.errorf(DiagSessionAfterConvergence,
				"node %q has session: %s but has await: %s (convergence point); only fresh or artifacts_only are allowed",
				nodeID, session, awaitMode)
		}
	}
}

// findConvergenceNodes returns the set of node IDs that are convergence points.
// A node is a convergence point if it has AwaitMode != AwaitNone OR
// if it receives unconditional edges from multiple distinct sources.
func (c *compiler) findConvergenceNodes(w *Workflow) map[string]bool {
	result := make(map[string]bool)

	// Nodes explicitly marked with await.
	for id, node := range w.Nodes {
		if NodeAwaitMode(node) != AwaitNone {
			result[id] = true
		}
	}

	// Nodes receiving edges from multiple distinct sources.
	incomingSources := make(map[string]map[string]bool) // target -> set of source IDs
	for _, e := range w.Edges {
		if _, ok := incomingSources[e.To]; !ok {
			incomingSources[e.To] = make(map[string]bool)
		}
		incomingSources[e.To][e.From] = true
	}
	for nodeID, sources := range incomingSources {
		if len(sources) > 1 {
			result[nodeID] = true
		}
	}

	return result
}

// ---------------------------------------------------------------------------
// C010, C011, C012 — edge routing validation
// ---------------------------------------------------------------------------

func (c *compiler) validateEdgeRouting(w *Workflow) {
	// Group outgoing edges by source node. We distinguish three classes:
	//   - conditional: has a `when` (boolean field or expression)
	//   - loopBearing: has `as <name>(N)` but no `when`
	//   - unconditional: neither
	//
	// Loop-bearing edges sit between the two: at runtime they are taken
	// while the loop counter is below max and skipped once exhausted. So:
	//   - For C010 (too many fallbacks): only PURE unconditional edges
	//     count — loop-bearing edges are not duplicate fallbacks.
	//   - For C012 (no fallback): a loop-bearing edge counts as a
	//     fallback (it's reached while the loop is alive); the existing
	//     `streak_check -> alt as l(6)` + `streak_check -> done` pattern
	//     is the canonical graceful-exhaustion shape.
	type edgeGroup struct {
		unconditional []*Edge
		loopBearing   []*Edge
		conditional   []*Edge
	}
	groups := make(map[string]*edgeGroup)
	for _, e := range w.Edges {
		g, ok := groups[e.From]
		if !ok {
			g = &edgeGroup{}
			groups[e.From] = g
		}
		switch {
		case e.IsConditional():
			g.conditional = append(g.conditional, e)
		case e.IsBoundedIteration():
			// Loop and foreach back-edges are bounded iteration edges, not
			// default fall-through edges — they don't count toward the
			// "one default edge" rule.
			g.loopBearing = append(g.loopBearing, e)
		default:
			g.unconditional = append(g.unconditional, e)
		}
	}

	for nodeID, g := range groups {
		node, ok := w.Nodes[nodeID]
		if !ok {
			continue
		}

		// Router fan_out_all, round_robin, and llm are allowed multiple unconditional edges.
		// fan_out_each is excluded here so its dedicated "exactly one template
		// edge" rule (validateFanOutEachEdges) owns the error message.
		if r, ok := node.(*RouterNode); ok && (r.RouterMode == RouterFanOutAll || r.RouterMode == RouterRoundRobin || r.RouterMode == RouterLLM) {
			continue
		}

		// C010: multiple PURE unconditional edges from a non-fan_out_all node.
		if len(g.unconditional) > 1 {
			targets := make([]string, len(g.unconditional))
			for i, e := range g.unconditional {
				targets[i] = e.To
			}
			c.errorf(DiagMultipleDefaultEdges,
				"node %q has %d unconditional edges (targets: %v); only one default edge is allowed",
				nodeID, len(g.unconditional), targets)
		}

		// Only validate conditions for nodes that have conditional edges.
		if len(g.conditional) == 0 {
			continue
		}

		// C012: conditional edges but no fallback. A loop-bearing edge counts
		// as a fallback for this purpose.
		if len(g.unconditional) == 0 && len(g.loopBearing) == 0 {
			if !isExhaustive(g.conditional) {
				c.errorf(DiagMissingFallback,
					"node %q has conditional edges but no default (unconditional) fallback edge",
					nodeID)
			}
		}

		// C011: ambiguous conditions — same field appears twice with same polarity.
		c.checkAmbiguousConditions(nodeID, g.conditional)
	}
}

// ---------------------------------------------------------------------------
// C020 — round_robin router must have at least 2 outgoing edges
// ---------------------------------------------------------------------------

func (c *compiler) validateRoundRobinEdges(w *Workflow) {
	for _, node := range w.Nodes {
		r, ok := node.(*RouterNode)
		if !ok || r.RouterMode != RouterRoundRobin {
			continue
		}
		count := 0
		for _, e := range w.Edges {
			if e.From == r.ID && !e.IsConditional() {
				count++
			}
		}
		if count < 2 {
			c.errorf(DiagRoundRobinTooFewEdges,
				"round_robin router %q has %d unconditional outgoing edge(s); at least 2 are needed for alternation",
				r.ID, count)
		}
	}
}

// ---------------------------------------------------------------------------
// C021, C022 — llm router validation
// ---------------------------------------------------------------------------

func (c *compiler) validateLLMRouterEdges(w *Workflow) {
	for _, node := range w.Nodes {
		r, ok := node.(*RouterNode)
		if !ok || r.RouterMode != RouterLLM {
			continue
		}
		count := 0
		for _, e := range w.Edges {
			if e.From == r.ID {
				count++
				if e.IsConditional() {
					c.errorf(DiagLLMRouterConditionEdge,
						"llm router %q edge to %q has a 'when' condition; LLM routers select targets directly",
						r.ID, e.To)
				}
			}
		}
		if count < 2 {
			c.errorf(DiagLLMRouterTooFewEdges,
				"llm router %q has %d outgoing edge(s); at least 2 are needed",
				r.ID, count)
		}
	}
}

// ---------------------------------------------------------------------------
// C104 — fan_out_each router must have exactly one outgoing (template) edge
// ---------------------------------------------------------------------------
//
// A fan_out_each router re-executes ONE statically-declared template subgraph
// once per element of the runtime array `over`. The single outgoing edge is
// the head of that template; the branch then runs the existing static graph
// until it reaches a convergence point. Multiple outgoing edges are ambiguous
// (which template?) and zero means nothing to iterate.
func (c *compiler) validateFanOutEachEdges(w *Workflow) {
	for _, node := range w.Nodes {
		r, ok := node.(*RouterNode)
		if !ok || r.RouterMode != RouterFanOutEach {
			continue
		}
		count := 0
		for _, e := range w.Edges {
			if e.From == r.ID {
				count++
				if e.IsConditional() {
					c.errorf(DiagFanOutEachEdges,
						"fan_out_each router %q edge to %q has a 'when' condition; the single template edge must be unconditional",
						r.ID, e.To)
				}
			}
		}
		if count != 1 {
			c.errorf(DiagFanOutEachEdges,
				"fan_out_each router %q has %d outgoing edge(s); exactly one (the per-item template head) is required",
				r.ID, count)
		}
	}
}

// validateResources flags any node whose `needs:` references a resource the
// workflow doesn't declare in its `resources:` block. Without this the acquire
// is a silent no-op and the intended bound (e.g. on Godot sessions) never
// applies — exactly the failure mode the feature exists to prevent.
func (c *compiler) validateResources(w *Workflow) {
	for _, node := range w.Nodes {
		for _, r := range NodeNeeds(node) {
			if _, ok := w.Resources[r]; !ok {
				c.errorf(DiagUnknownResourceInNeeds,
					"node %q needs resource %q, which is not declared in the workflow's resources: block",
					node.NodeID(), r)
			}
		}
	}
}

// isExhaustive returns true if the conditional edges exhaustively cover
// a boolean field (one edge for the field, one for its negation).
func isExhaustive(edges []*Edge) bool {
	// Build map: field -> has_positive, has_negative
	type polarity struct {
		pos bool
		neg bool
	}
	fields := make(map[string]*polarity)
	for _, e := range edges {
		p, ok := fields[e.Condition]
		if !ok {
			p = &polarity{}
			fields[e.Condition] = p
		}
		if e.Negated {
			p.neg = true
		} else {
			p.pos = true
		}
	}

	// Exhaustive if at least one field has both polarities.
	for _, p := range fields {
		if p.pos && p.neg {
			return true
		}
	}
	return false
}

// checkAmbiguousConditions detects duplicate conditions with the same polarity
// on the same source node. Expression-form edges (`when "<expr>"`) are keyed
// by their full source so two distinct expressions are treated as different
// conditions; the validator can't statically prove disjointness or overlap of
// arbitrary boolean expressions, so we trust the author and only flag exact
// duplicates of the same expression source.
func (c *compiler) checkAmbiguousConditions(nodeID string, edges []*Edge) {
	type condKey struct {
		field      string
		negated    bool
		expression string
	}
	seen := make(map[condKey]*Edge)
	for _, e := range edges {
		key := condKey{field: e.Condition, negated: e.Negated, expression: e.ExpressionSrc}
		if prev, ok := seen[key]; ok {
			var label string
			switch {
			case e.ExpressionSrc != "":
				label = `"` + e.ExpressionSrc + `"`
			case e.Negated:
				label = "not " + e.Condition
			default:
				label = e.Condition
			}
			c.errorf(DiagAmbiguousCondition,
				"node %q has ambiguous edges: both %s->%s and %s->%s trigger on %s",
				nodeID, prev.From, prev.To, e.From, e.To, label)
		} else {
			seen[key] = e
		}
	}
}

// ---------------------------------------------------------------------------
// C013, C014 — condition field validation against output schema
// ---------------------------------------------------------------------------

func (c *compiler) validateConditionFields(w *Workflow) {
	for _, e := range w.Edges {
		src, ok := w.Nodes[e.From]
		if !ok {
			continue
		}
		outSchema := NodeOutputSchema(src)
		schema, _ := w.Schemas[outSchema]

		switch {
		case e.Condition != "":
			if outSchema == "" || schema == nil {
				continue
			}
			field := findField(schema, e.Condition)
			if field == nil {
				c.errorfAt(DiagConditionFieldNotFound, e.From, edgeID(e.From, e.To),
					"edge %s -> %s: condition field %q not found in output schema %q of node %q",
					e.From, e.To, e.Condition, outSchema, e.From)
				continue
			}
			if field.Type != FieldTypeBool {
				c.errorfAt(DiagConditionNotBool, e.From, edgeID(e.From, e.To),
					"edge %s -> %s: condition field %q is %s, not bool, in output schema %q",
					e.From, e.To, e.Condition, field.Type, outSchema)
			}

		case e.Expression != nil:
			// Expression form (`when "expr"`). Walk every
			// outputs.<source>.<field> reference and check the
			// field exists on the source node's schema. Other
			// namespaces (vars, input, attachments) are checked
			// by validateTemplateRefs via collectAllRefs.
			if outSchema == "" || schema == nil {
				continue
			}
			for _, r := range e.Expression.Refs() {
				if r.Namespace != "outputs" {
					continue
				}
				// Path is [<node>, <field>, ...]. Only validate the field
				// when the reference targets the source node itself —
				// cross-node refs are validated elsewhere.
				if len(r.Path) < 2 || r.Path[0] != e.From {
					continue
				}
				if findField(schema, r.Path[1]) == nil {
					c.errorfAt(DiagConditionFieldNotFound, e.From, edgeID(e.From, e.To),
						"edge %s -> %s: expression references field %q not found in output schema %q of node %q",
						e.From, e.To, r.Path[1], outSchema, e.From)
				}
			}
		}
	}
}

func findField(s *Schema, name string) *SchemaField {
	for _, f := range s.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// isRuntimeInjectedField reports whether a field name is a runtime-injected
// internal field — underscore-prefixed (e.g. _session_id, _session_fingerprint)
// — that is deliberately absent from declared schemas. Both the outputs-ref
// validator (C031/C032) and the static type checker skip these so threading
// session metadata through edges/refs never trips a "field not in schema"
// diagnostic.
func isRuntimeInjectedField(name string) bool {
	return len(name) > 0 && name[0] == '_'
}

// ---------------------------------------------------------------------------
// C028 — duplicate with-mapping keys across edges to same target
// ---------------------------------------------------------------------------

func (c *compiler) validateDuplicateWithKeys(w *Workflow) {
	// Detect duplicate with-mapping keys on edges to the same target node,
	// but only when the edges can fire simultaneously. Skip:
	// - Conditional edges (when/when not) — mutually exclusive at runtime
	// - Loop edges and edges from loop re-entry nodes — they replace initial entry
	// - Edges targeting convergence points — multiple branches legitimately
	//   send the same context data to a convergence node
	type keySource struct {
		key  string
		from string
	}

	convergence := c.findConvergenceNodes(w)

	// Build set of nodes that are targets of loop-bearing edges (loop re-entry points).
	loopReentryNodes := make(map[string]bool)
	for _, e := range w.Edges {
		if e.LoopName != "" {
			loopReentryNodes[e.To] = true
		}
	}

	targetKeys := make(map[string][]keySource) // target -> list of (key, source)
	for _, e := range w.Edges {
		if e.Condition != "" {
			continue // skip conditional edges — they're mutually exclusive
		}
		if e.LoopName != "" {
			continue // skip loop edges — they re-enter an already-visited node
		}
		if loopReentryNodes[e.From] {
			continue // skip edges from loop re-entry nodes
		}
		if convergence[e.To] {
			continue // skip edges to convergence points — duplicate context is expected
		}
		for _, dm := range e.With {
			targetKeys[e.To] = append(targetKeys[e.To], keySource{key: dm.Key, from: e.From})
		}
	}

	for targetID, keys := range targetKeys {
		seen := make(map[string]string) // key -> first source
		for _, ks := range keys {
			if prevFrom, ok := seen[ks.key]; ok && prevFrom != ks.from {
				c.errorf(DiagDuplicateWithKey,
					"node %q receives with-mapping key %q from both %q and %q; keys must be unique across incoming edges",
					targetID, ks.key, prevFrom, ks.from)
			} else if !ok {
				seen[ks.key] = ks.from
			}
		}
	}
}

// ---------------------------------------------------------------------------
// C016 — unreachable nodes
// ---------------------------------------------------------------------------

func (c *compiler) validateReachability(w *Workflow) {
	if w.Entry == "" {
		return
	}

	// Build adjacency list from edges.
	adj := make(map[string][]string)
	for _, e := range w.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// BFS from entry.
	visited := make(map[string]bool)
	queue := []string{w.Entry}
	visited[w.Entry] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}

	// Report unreachable non-terminal nodes.
	// Skip "done" and "fail" if they have no incoming edges — they're always present.
	for id, node := range w.Nodes {
		if visited[id] {
			continue
		}
		// Terminal nodes are always added; skip them if unreachable — it's fine.
		switch node.(type) {
		case *DoneNode, *FailNode:
			continue
		}
		c.errorfAt(DiagUnreachableNode, id, "",
			"node %q (%s) is unreachable from entry %q",
			id, node.NodeKind(), w.Entry)
	}
}

// ---------------------------------------------------------------------------
// C017 — outputs.<node>.history reference requires node to be in a loop
// ---------------------------------------------------------------------------

func (c *compiler) validateHistoryRefs(w *Workflow) {
	// Build set of nodes that participate in a loop (appear on a loop-bearing edge).
	loopNodes := make(map[string]bool)
	for _, e := range w.Edges {
		if e.LoopName != "" {
			loopNodes[e.From] = true
			loopNodes[e.To] = true
		}
	}

	// Check all refs in prompts and edge with-mappings.
	checkRef := func(ctx string, ref *Ref) {
		if ref.Kind != RefOutputs {
			return
		}
		// outputs.<node>.history pattern: Path = [node, "history"]
		if len(ref.Path) >= 2 && ref.Path[len(ref.Path)-1] == "history" {
			nodeID := ref.Path[0]
			if _, ok := w.Nodes[nodeID]; !ok {
				return // unknown node already reported by other checks
			}
			if !loopNodes[nodeID] {
				c.errorf(DiagHistoryRefNotInLoop,
					"%s: reference %s uses .history but node %q is not in any loop",
					ctx, ref.Raw, nodeID)
			}
		}
	}

	// Check prompts.
	for _, p := range w.Prompts {
		for _, ref := range p.TemplateRefs {
			checkRef(fmt.Sprintf("prompt %q", p.Name), ref)
		}
	}

	// Check edge with-mappings.
	for _, e := range w.Edges {
		for _, dm := range e.With {
			for _, ref := range dm.Refs {
				checkRef(fmt.Sprintf("edge %s -> %s, with %q", e.From, e.To, dm.Key), ref)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// C019 — undeclared cycles (back-edges without a loop declaration)
// ---------------------------------------------------------------------------

// validateUndeclaredCycles uses DFS to detect cycles that have no declared
// loop on any of their edges. Such cycles would cause infinite execution
// if no budget is set.
func (c *compiler) validateUndeclaredCycles(w *Workflow) {
	// Build set of nodes that participate in a declared loop.
	// A cycle is considered bounded if ANY edge in the cycle carries a
	// LoopName — the runtime enforces max_iterations on that edge.
	loopNodes := make(map[string]bool)
	for _, e := range w.Edges {
		// A foreach back-edge is a bounded cycle too — the runtime stops it
		// when the collection is exhausted.
		if e.IsBoundedIteration() {
			loopNodes[e.From] = true
			loopNodes[e.To] = true
		}
	}

	// Build adjacency list.
	adj := make(map[string][]string)
	for _, e := range w.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// DFS with three-color marking: white (unseen), gray (in stack), black (done).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int) // default white

	var dfs func(node string)
	dfs = func(node string) {
		color[node] = gray
		for _, to := range adj[node] {
			switch color[to] {
			case gray:
				// Back-edge found — cycle. Only report if neither endpoint
				// participates in a declared loop (which bounds the cycle).
				if !loopNodes[node] && !loopNodes[to] {
					c.errorf(DiagUndeclaredCycle,
						"cycle detected: edge %s -> %s forms a cycle without a declared loop; add a loop with max_iterations to bound it",
						node, to)
				}
			case white:
				dfs(to)
			}
		}
		color[node] = black
	}

	// Walk from Entry first (preserves prior visit order so the same
	// graph still emits the same diagnostic for the same back-edge),
	// then sweep over every other node so cycles in components that
	// aren't directly reachable from Entry are also detected. The
	// reachability check (C016) handles "cycle in unreachable region"
	// separately — both diagnostics may now fire on the same workflow.
	if w.Entry != "" {
		dfs(w.Entry)
	}
	for n := range adj {
		if color[n] == white {
			dfs(n)
		}
	}
}

// ---------------------------------------------------------------------------
// C026 — loop max_iterations must be >= 1
// ---------------------------------------------------------------------------

func (c *compiler) validateLoopIterations(w *Workflow) {
	for _, loop := range w.Loops {
		// Templated caps (`as fix_loop("{{outputs.X.cap}}")`) carry
		// MaxIterations=0 by design — the real bound is resolved at
		// runtime from the referenced output/var. The runtime falls
		// back to MaxIterations (0) if resolution fails, which produces
		// a "loop exhausted on iteration 0" log line: the operator
		// sees the wiring problem without compile-time blocking
		// otherwise-valid templated declarations.
		if loop.MaxIterationsExpr != "" {
			continue
		}
		if loop.MaxIterations < 1 {
			c.errorf(DiagInvalidLoopIterations,
				"loop %q has max_iterations=%d; must be >= 1",
				loop.Name, loop.MaxIterations)
		}
	}
}

// ---------------------------------------------------------------------------
// C024 — invalid reasoning_effort value
// ---------------------------------------------------------------------------

// ValidReasoningEfforts is the set of accepted reasoning effort levels.
// Mirrors the Anthropic effort spec (platform.claude.com/docs/en/build-with-claude/effort)
// and the CLAUDE_CODE_EFFORT_LEVEL env var (code.claude.com/docs/en/model-config).
// Per-model availability is curated upstream in claw-code-go's ModelEntry; this
// set is the union across all models.
var ValidReasoningEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
	// "ultracode" is not an API effort value — Anthropic only accepts up to
	// xhigh/max on the wire. It is a *mode* (Claude Code's "Ultracode"):
	// xhigh reasoning + standing consent to orchestrate multi-agent
	// workflows, prompt-engineered and reliable only on Opus 4.8. The
	// runtime remaps it to "xhigh" before the wire (see model.wireEffort)
	// and injects the orchestration prerogative. Authoring it in the
	// reasoning_effort field mirrors how Claude Code surfaces it.
	"ultracode": true,
}

// IsEnvSubstitutedEffort reports whether an effort literal is an
// env-substituted form (e.g. "${VAR}" or "${VAR:-default}") that must
// be resolved at runtime. The "$" guard is intentionally permissive —
// the runtime resolver handles malformed forms by falling back to the
// empty string.
func IsEnvSubstitutedEffort(s string) bool {
	return strings.ContainsRune(s, '$')
}

// ResolveEffortLiteral expands env-substituted forms ("${VAR}",
// "${VAR:-default}") against the process env and validates the result
// against ValidReasoningEfforts. Non-env-substituted values are
// returned unchanged. Invalid expansions return "" so callers can fall
// back to the provider's documented default.
func ResolveEffortLiteral(s string) string {
	if !IsEnvSubstitutedEffort(s) {
		return s
	}
	expanded := ExpandEnvWithDefault(s)
	if ValidReasoningEfforts[expanded] {
		return expanded
	}
	return ""
}

// ExpandEnvWithDefault expands ${VAR} and ${VAR:-default} forms in s.
// Mirrors the shell parameter-expansion default-value syntax that
// stdlib os.ExpandEnv does not support: when ${VAR} is unset or empty,
// the part after :- is returned instead. Exported so the executor and
// other callers stay in sync with the validator's expansion semantics
// — anything that defaults a model spec or env-tunable field via
// `${VAR:-default}` in a recipe relies on this rather than the bare
// stdlib helper, which would expand `${X:-y}` to "" (treating the
// whole `X:-y` as the variable name).
//
// Supports nested fallbacks (`${A:-${B:-c}}`): we parse `${...}`
// segments with brace-counting so nested defaults are resolved
// inside-out. os.Expand isn't recursive and would stop at the first
// `}`, leaving a trailing brace literal — so we cannot rely on it.
func ExpandEnvWithDefault(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		// Bare `$NAME` form (no braces) — delegate to os.Expand for
		// just this fragment.
		if s[i] == '$' && i+1 < len(s) && s[i+1] != '{' {
			end := i + 1
			for end < len(s) && (isAlnum(s[end]) || s[end] == '_') {
				end++
			}
			if end > i+1 {
				b.WriteString(os.Getenv(s[i+1 : end]))
				i = end
				continue
			}
		}
		// `${...}` form — scan to the matching closing brace with
		// depth counting so nested ${...} segments stay paired.
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				if j+1 < len(s) && s[j] == '$' && s[j+1] == '{' {
					depth++
					j += 2
					continue
				}
				if s[j] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
				j++
			}
			if depth == 0 {
				inner := s[i+2 : j]
				// Recurse so a nested ${...} inside the fallback
				// gets expanded before we apply the default-value
				// rule on this level.
				expanded := ExpandEnvWithDefault(inner)
				if idx := strings.Index(expanded, ":-"); idx >= 0 {
					name, fallback := expanded[:idx], expanded[idx+2:]
					if v := os.Getenv(name); v != "" {
						b.WriteString(v)
					} else {
						b.WriteString(fallback)
					}
				} else {
					b.WriteString(os.Getenv(expanded))
				}
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (c *compiler) validateReasoningEffort(w *Workflow) {
	for _, node := range w.Nodes {
		var effort, model string
		switch n := node.(type) {
		case LLMNode:
			f := n.GetLLMFields()
			effort, model = f.ReasoningEffort, f.Model
		case *RouterNode:
			effort, model = n.ReasoningEffort, n.Model
		default:
			continue
		}
		if effort == "" {
			continue
		}
		// Env-substituted forms ("${VAR}", "${VAR:-default}") are
		// resolved + validated at runtime. Skip the enum check here;
		// the runtime resolver clamps invalid expansions to "" so
		// the provider applies its own default.
		if IsEnvSubstitutedEffort(effort) {
			continue
		}
		if !ValidReasoningEfforts[effort] {
			c.errorf(DiagInvalidReasoningEffort,
				"node %q has invalid reasoning_effort %q; valid values are low, medium, high, xhigh, max, ultracode",
				node.NodeID(), effort)
			continue
		}
		// ultracode (xhigh + workflow-orchestration prerogative) relies on
		// mid-conversation system messages, which Anthropic ships on Opus 4.8
		// only. On any other model it degrades to plain xhigh — warn so the
		// author knows the orchestration half won't be reliable.
		if effort == "ultracode" && !modelIsOpus48(model) {
			shown := model
			if shown == "" {
				shown = "(default)"
			}
			c.warnf(DiagUltracodeModelGate,
				"node %q uses reasoning_effort: ultracode but model %q is not claude-opus-4-8; ultracode's workflow-orchestration prerogative is reliable only on Opus 4.8 and will degrade to plain xhigh elsewhere",
				node.NodeID(), shown)
		}
	}
}

// modelIsOpus48 reports whether a model spec resolves to claude-opus-4-8.
// An empty spec is treated as the default (Opus 4.8 when Anthropic is the
// resolved backend) and env-substituted forms are deferred to runtime — both
// suppress the ultracode gate warning. The bare "opus" alias resolves to the
// newest Opus (4.8) in claw's registry.
func modelIsOpus48(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || IsEnvSubstitutedEffort(m) {
		return true
	}
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m == "opus" || strings.Contains(m, "opus-4-8")
}

// ---------------------------------------------------------------------------
// C030–C036 — deep template reference validation
// ---------------------------------------------------------------------------

// refContext associates a Ref with the node that consumes it and a
// human-readable location string for diagnostics.
type refContext struct {
	Ref         *Ref
	NodeID      string // consuming node ID
	Location    string // e.g. "prompt 'sys' (node 'a')"
	IncludeSelf bool   // true for edge with-mappings: the source node itself is available
}

// collectAllRefs gathers every template reference in the workflow together
// with the node that consumes it.
func collectAllRefs(w *Workflow) []refContext {
	// Build reverse map: prompt name → list of consuming node IDs.
	promptUsers := make(map[string][]string)
	for _, n := range w.Nodes {
		for _, pname := range nodePromptRefs(n) {
			promptUsers[pname] = append(promptUsers[pname], n.NodeID())
		}
	}

	var out []refContext

	// Prompt template refs.
	for _, p := range w.Prompts {
		consumers := promptUsers[p.Name]
		for _, ref := range p.TemplateRefs {
			for _, nodeID := range consumers {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   nodeID,
					Location: fmt.Sprintf("prompt %q (node %q)", p.Name, nodeID),
				})
			}
		}
	}

	// Edge with-mapping refs. The with-mapping is evaluated when the
	// edge fires, so the source node (From) and all its predecessors
	// have already produced their outputs. We use From as the consumer
	// context and include From itself as an additional "self" predecessor.
	for _, e := range w.Edges {
		for _, dm := range e.With {
			for _, ref := range dm.Refs {
				out = append(out, refContext{
					Ref:         ref,
					NodeID:      e.From,
					Location:    fmt.Sprintf("edge %s -> %s, with %q", e.From, e.To, dm.Key),
					IncludeSelf: true,
				})
			}
		}
	}

	// Tool node command + script refs. ScriptRefs used to be skipped,
	// so {{outputs.X.history}} inside a tool's script never went
	// through C030–C036 validation — typos were caught only at
	// runtime, after the script had already started executing.
	for _, n := range w.Nodes {
		if t, ok := n.(*ToolNode); ok {
			for _, ref := range t.CommandRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q command", t.ID),
				})
			}
			for _, ref := range t.ScriptRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q script", t.ID),
				})
			}
		}
	}

	// Compute node expressions. Each ComputeExpr.AST exposes its
	// vars/input/outputs/... references — convert them to ir.Ref
	// shape and feed them into the same C030–C036 pipeline so a
	// typo'd `outputs.unknown.field` in a compute expression is
	// caught at compile time instead of at first evaluation.
	for _, n := range w.Nodes {
		cn, ok := n.(*ComputeNode)
		if !ok {
			continue
		}
		for _, e := range cn.Exprs {
			if e.AST == nil {
				continue
			}
			for _, r := range e.AST.Refs() {
				ref := refFromExpr(r)
				if ref == nil {
					continue
				}
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   cn.ID,
					Location: fmt.Sprintf("compute node %q expr %q", cn.ID, e.Key),
				})
			}
		}
	}

	return out
}

// refFromExpr converts an [expr.Ref] (namespace + path) to an [ir.Ref]
// so the shared template-ref validator can check compute-node refs
// alongside prompt / edge / tool refs. Returns nil when the namespace
// isn't one of the kinds the template validator handles (e.g. `loop`,
// `run` — both legitimate but consumed by separate validators).
func refFromExpr(r expr.Ref) *Ref {
	var kind RefKind
	switch r.Namespace {
	case "vars":
		kind = RefVars
	case "input":
		kind = RefInput
	case "outputs":
		kind = RefOutputs
	case "artifacts":
		kind = RefArtifacts
	case "attachments":
		kind = RefAttachments
	case "secrets":
		// So C093 (unknown secret) fires for {{secrets.X}} in compute exprs too.
		kind = RefSecrets
	default:
		return nil
	}
	raw := r.Namespace
	for _, p := range r.Path {
		raw += "." + p
	}
	return &Ref{
		Kind: kind,
		Path: append([]string(nil), r.Path...),
		Raw:  "{{" + raw + "}}",
	}
}

// nodePromptRefs delegates to the exported NodePromptRefs in ir.go.
// Kept as a local alias for readability in this file.
var nodePromptRefs = NodePromptRefs

// buildPredecessors computes, for each node, the set of all nodes that
// can execute before it (i.e. whose outputs are available). This follows
// ALL edges (including conditional and loop back-edges) to ensure zero
// false positives.
func buildPredecessors(w *Workflow) map[string]map[string]bool {
	// Build reverse adjacency list.
	revAdj := make(map[string][]string)
	for _, e := range w.Edges {
		revAdj[e.To] = append(revAdj[e.To], e.From)
	}

	// Identify nodes that are targets of loop back-edges.
	// These nodes are effectively their own predecessors because
	// a prior iteration's output is available on re-entry.
	loopTargets := make(map[string]bool)
	for _, e := range w.Edges {
		if e.LoopName != "" {
			loopTargets[e.To] = true
		}
	}

	result := make(map[string]map[string]bool)
	for id := range w.Nodes {
		preds := computePredecessors(id, revAdj)
		if loopTargets[id] {
			preds[id] = true
		}
		result[id] = preds
	}
	return result
}

// computePredecessors returns all transitive predecessors of nodeID via
// reverse BFS.
func computePredecessors(nodeID string, revAdj map[string][]string) map[string]bool {
	visited := make(map[string]bool)
	queue := revAdj[nodeID]
	for i := 0; i < len(queue); i++ {
		pred := queue[i]
		if visited[pred] || pred == nodeID {
			continue
		}
		visited[pred] = true
		queue = append(queue, revAdj[pred]...)
	}
	return visited
}

// buildArtifactProducers maps artifact names to their producing node IDs.
func buildArtifactProducers(w *Workflow) map[string]string {
	producers := make(map[string]string)
	for _, n := range w.Nodes {
		if pub := NodePublish(n); pub != "" {
			producers[pub] = n.NodeID()
		}
	}
	return producers
}

func (c *compiler) validateSecrets(w *Workflow) {
	if w == nil || len(w.Secrets) == 0 {
		return
	}
	for name, s := range w.Secrets {
		if s == nil {
			continue
		}
		switch s.As {
		case "", "value", "file":
			// ok
		default:
			c.errorf(DiagInvalidSecretFile,
				"secret %q: as must be \"value\" or \"file\" (got %q)", name, s.As)
		}
		if s.As != "file" && (s.MountPath != "" || s.Env != "") {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: mount_path/env require as: file", name)
		}
		if s.MountPath != "" && !strings.HasPrefix(s.MountPath, "/") {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: mount_path %q must be absolute", name, s.MountPath)
		}
		if s.MountPath != "" && (path.Clean(s.MountPath) != s.MountPath || s.MountPath == "/") {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: mount_path %q must be a clean absolute file path", name, s.MountPath)
		}
		if s.Env != "" && !validEnvName(s.Env) {
			c.errorf(DiagInvalidSecretFile,
				"secret %q: env %q is not a valid environment variable name", name, s.Env)
		}
		for _, h := range s.Hosts {
			if !validSecretHost(h) {
				c.errorf(DiagInvalidSecretHost,
					"secret %q: hosts entry %q must be a bare hostname, parent domain, or IP without scheme/path", name, h)
			}
		}
	}
}

func validSecretHost(h string) bool {
	h = strings.TrimSpace(h)
	if h == "" || strings.Contains(h, "://") || strings.ContainsAny(h, "/?#@\\ \t\n\r\x00%") {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if strings.Contains(h, ":") || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !validHostnameLabel(label) {
			return false
		}
	}
	return true
}

func validHostnameLabel(label string) bool {
	if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func (c *compiler) validateTemplateRefs(w *Workflow) {
	refs := collectAllRefs(w)
	if len(refs) == 0 {
		return
	}

	predecessors := buildPredecessors(w)
	artifactProducers := buildArtifactProducers(w)

	for _, rc := range refs {
		switch rc.Ref.Kind {
		case RefOutputs:
			c.validateOutputsRef(w, rc, predecessors)
		case RefVars:
			c.validateVarsRef(w, rc)
		case RefInput:
			c.validateInputRef(w, rc)
		case RefArtifacts:
			c.validateArtifactsRef(w, rc, predecessors, artifactProducers)
		case RefAttachments:
			c.validateAttachmentsRef(w, rc)
		case RefSecrets:
			c.validateSecretsRef(w, rc)
		}
	}
}

// validateSecretsRef flags a {{secrets.X}} reference whose secret X is
// not declared in the workflow's `secrets:` block.
func (c *compiler) validateSecretsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	secret, ok := w.Secrets[name]
	if !ok {
		c.errorf(DiagUnknownSecret,
			"%s: reference %s targets undeclared secret %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	if len(rc.Ref.Path) == 1 {
		return
	}
	sub := rc.Ref.Path[1]
	if sub != "path" {
		c.errorf(DiagSecretSubfield,
			"%s: reference %s uses unknown secret sub-field %q (expected: path)",
			rc.Location, rc.Ref.Raw, sub)
		return
	}
	if !secret.IsFile() {
		c.errorf(DiagSecretSubfield,
			"%s: reference %s uses .path on non-file secret %q",
			rc.Location, rc.Ref.Raw, name)
	}
}

func (c *compiler) validateAttachmentsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	name := rc.Ref.Path[0]
	if _, ok := w.Attachments[name]; !ok {
		c.errorf(DiagUnknownAttachment,
			"%s: reference %s targets undeclared attachment %q",
			rc.Location, rc.Ref.Raw, name)
		return
	}
	if len(rc.Ref.Path) >= 2 {
		sub := rc.Ref.Path[1]
		if _, ok := AttachmentSubFields[sub]; !ok {
			c.errorf(DiagAttachmentSubfieldUnknown,
				"%s: reference %s uses unknown sub-field %q (expected one of: path, url, mime, size, sha256)",
				rc.Location, rc.Ref.Raw, sub)
		}
	}
}

func (c *compiler) validateOutputsRef(w *Workflow, rc refContext, predecessors map[string]map[string]bool) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	targetNodeID := rc.Ref.Path[0]

	// C030: referenced node must exist.
	targetNode, ok := w.Nodes[targetNodeID]
	if !ok {
		c.errorf(DiagUnknownRefNode,
			"%s: reference %s targets unknown node %q",
			rc.Location, rc.Ref.Raw, targetNodeID)
		return
	}

	// C036: referenced node must be reachable before consumer.
	if preds, ok := predecessors[rc.NodeID]; ok {
		reachable := preds[targetNodeID]
		// For edge with-mappings, the source node itself has finished, so
		// it and its predecessors are all available.
		if !reachable && rc.IncludeSelf && targetNodeID == rc.NodeID {
			reachable = true
		}
		if !reachable {
			c.errorf(DiagRefNodeNotReachable,
				"%s: reference %s targets node %q which is not reachable before %q",
				rc.Location, rc.Ref.Raw, targetNodeID, rc.NodeID)
			return
		}
	}

	// Field-level validation (only when accessing a specific field).
	if len(rc.Ref.Path) < 2 {
		return
	}
	fieldName := rc.Ref.Path[1]

	// Skip .history — already covered by C017.
	if fieldName == "history" {
		return
	}

	// Skip runtime-injected fields (e.g. _session_id) not declared in schemas.
	if isRuntimeInjectedField(fieldName) {
		return
	}

	// C032: node has no output schema — warn that field access can't be verified.
	outSchema := NodeOutputSchema(targetNode)
	if outSchema == "" {
		c.warnf(DiagRefNodeNoSchema,
			"%s: reference %s accesses field %q on node %q which has no output schema; cannot verify",
			rc.Location, rc.Ref.Raw, fieldName, targetNodeID)
		return
	}

	// C031: field must exist in the output schema.
	schema, ok := w.Schemas[outSchema]
	if !ok {
		return // already reported by C002
	}
	if findField(schema, fieldName) == nil {
		c.errorf(DiagRefFieldNotInSchema,
			"%s: reference %s accesses field %q not found in output schema %q of node %q",
			rc.Location, rc.Ref.Raw, fieldName, outSchema, targetNodeID)
	}
}

func (c *compiler) validateVarsRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	varName := rc.Ref.Path[0]
	if _, ok := w.Vars[varName]; !ok {
		c.errorf(DiagUndeclaredVar,
			"%s: reference %s targets undeclared variable %q",
			rc.Location, rc.Ref.Raw, varName)
	}
}

func (c *compiler) validateInputRef(w *Workflow, rc refContext) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	fieldName := rc.Ref.Path[0]

	node, ok := w.Nodes[rc.NodeID]
	if !ok {
		return
	}

	// Can only validate if the consuming node has an input schema.
	inSchema := NodeInputSchema(node)
	if inSchema == "" {
		return
	}

	schema, ok := w.Schemas[inSchema]
	if !ok {
		return // already reported by C002
	}

	if findField(schema, fieldName) == nil {
		c.errorf(DiagInputFieldNotInSchema,
			"%s: reference %s accesses field %q not found in input schema %q of node %q",
			rc.Location, rc.Ref.Raw, fieldName, inSchema, rc.NodeID)
	}
}

func (c *compiler) validateArtifactsRef(w *Workflow, rc refContext, predecessors map[string]map[string]bool, producers map[string]string) {
	if len(rc.Ref.Path) == 0 {
		return
	}
	artifactName := rc.Ref.Path[0]

	// C035: artifact must be published by some node.
	producerID, ok := producers[artifactName]
	if !ok {
		c.errorf(DiagUnknownArtifact,
			"%s: reference %s targets artifact %q which is not published by any node",
			rc.Location, rc.Ref.Raw, artifactName)
		return
	}

	// C036: producer must be reachable before consumer.
	if preds, ok := predecessors[rc.NodeID]; ok {
		reachable := preds[producerID]
		if !reachable && rc.IncludeSelf && producerID == rc.NodeID {
			reachable = true
		}
		if !reachable {
			c.errorf(DiagRefNodeNotReachable,
				"%s: reference %s targets artifact %q published by node %q which is not reachable before %q",
				rc.Location, rc.Ref.Raw, artifactName, producerID, rc.NodeID)
		}
	}
}

// validateHTTPURL returns nil when raw parses as an absolute http(s) URL
// with a non-empty host. It rejects schemes other than http/https
// (e.g. typos like "htps://"), relative refs, and missing hosts.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}
