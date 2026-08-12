package ir

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
)

// validateEvents cross-checks emit/wait event names (ADR-051): a wait on an
// event no emit produces can only ever time out, and an emit no wait consumes is
// dead. Both are C198 warnings (not errors) — a wait may legitimately await an
// event emitted outside the run once external-event support lands.
func (c *compiler) validateEvents(w *Workflow) {
	emitted := make(map[string]bool)
	waited := make(map[string]bool)
	for _, n := range w.Nodes {
		switch x := n.(type) {
		case *EmitNode:
			emitted[x.Event] = true
		case *WaitNode:
			waited[x.Event] = true
		}
	}
	for _, n := range w.Nodes {
		switch x := n.(type) {
		case *WaitNode:
			if x.Event != "" && !emitted[x.Event] {
				c.warnfAt(DiagEventNoListener, x.ID, "",
					"wait %q awaits event %q which no emit node produces — it can only ever time out", x.ID, x.Event)
			}
		case *EmitNode:
			if x.Event != "" && !waited[x.Event] {
				c.warnfAt(DiagEventNoListener, x.ID, "",
					"emit %q produces event %q which no wait node consumes — the event is dead", x.ID, x.Event)
			}
		}
	}
}

// validateAwaitAnswers cross-checks await_answers nodes (ADR-081): a `from:`
// that names a missing node, or a node that is not an interaction: async
// agent/judge, can never see a pending question — the await can only ever
// time out. C202 warnings (not errors), mirroring the C198 dangling-event
// discipline.
func (c *compiler) validateAwaitAnswers(w *Workflow) {
	for _, n := range w.Nodes {
		aa, ok := n.(*AwaitAnswersNode)
		if !ok || aa.From == "" {
			continue
		}
		src, exists := w.Nodes[aa.From]
		if !exists {
			c.warnfAt(DiagAwaitAnswersBadFrom, aa.ID, "",
				"await_answers %q `from:` references unknown node %q — it can only ever time out", aa.ID, aa.From)
			continue
		}
		llm, isLLM := src.(LLMNode)
		if !isLLM || llm.GetInteractionFields().Interaction != InteractionAsync {
			c.warnfAt(DiagAwaitAnswersBadFrom, aa.ID, "",
				"await_answers %q `from:` references %q which is not an interaction: async agent/judge — no async question can originate there, so it can only ever time out", aa.ID, aa.From)
		}
	}
}

// validateFileFields enforces that a `file` schema field is only ever
// reachable as the output of a node that can actually PAUSE for an
// operator upload (C129, error).
//
// A `file` field is filled by an operator uploading bytes through the run
// console; the runtime promotes those bytes to a run attachment on resume.
// Nothing else in the engine can produce one — an LLM emits JSON, a tool
// node emits parsed stdout — so a `file` on an agent/judge/tool/compute
// output_schema is guaranteed to arrive empty at run time. That is a
// silent-nothing failure (the downstream node reads a missing path and
// improvises), which is exactly the failure class worth catching at
// compile time rather than three minutes into a paid run.
//
// Schemas are shared by name, so the check is per USE, not per
// declaration: the same schema may legitimately be a human node's output
// and another node's *input* (an input schema is advisory and never
// produced, so it is not flagged).
func (c *compiler) validateFileFields(w *Workflow) {
	for _, n := range w.Nodes {
		// Two human interaction modes never produce operator bytes:
		// `llm`, auto-answered by a model the same way an agent node is,
		// and `review`, whose output is the verdict map the ENGINE
		// builds. Both are human nodes in name only for this check.
		// `llm_or_human` is left alone: it can escalate to a real pause,
		// which is the only way the file arrives, so the author still has
		// a working path.
		if n.NodeKind() == NodeHuman && !fileImpossibleInteraction(NodeInteraction(n)) {
			continue
		}
		schemaName := NodeOutputSchema(n)
		if schemaName == "" {
			continue
		}
		schema := w.Schemas[schemaName]
		if schema == nil {
			continue
		}
		for _, f := range schema.Fields {
			if f.Type != FieldTypeFile {
				continue
			}
			if n.NodeKind() == NodeHuman {
				c.errorfAt(DiagFileFieldNotHuman, n.NodeID(), "",
					"human %q declares interaction: %s and an output_schema %q with the file field %q — that mode never collects operator bytes (an %s gate produces its output without a pause), so the field would arrive empty or with an invented path; use interaction: human (or llm_or_human, which can escalate to a real pause)",
					n.NodeID(), NodeInteraction(n).String(), schemaName, f.Name, NodeInteraction(n).String())
				continue
			}
			c.errorfAt(DiagFileFieldNotHuman, n.NodeID(), "",
				"%s %q declares output_schema %q whose field %q has type file — only a human node can produce a file (the operator uploads it at the pause); this field would always be empty",
				n.NodeKind().String(), n.NodeID(), schemaName, f.Name)
		}
	}
}

// reservedAnswerKeys are answer keys the ENGINE owns on a human node's
// output. The resume path writes them itself, so a schema declaring one
// does not get an optional field — it gets a field whose value is
// silently replaced.
//
// `_attachments` carries the gate's ad-hoc uploads (the paperclip
// button, no DSL involved). It was documented as collision-proof because
// authored field names "never start with '_' by convention", but the
// lexer accepts a leading underscore, so the convention was never
// enforced — hence this check rather than a comment.
var reservedAnswerKeys = map[string]string{
	"_attachments": "carries the gate's ad-hoc operator uploads",
}

// fileImpossibleInteraction reports whether a human node's interaction
// mode resolves the gate WITHOUT an operator upload: `llm` auto-answers
// with a model, `review` outputs the engine-built verdict map. Both make
// a `file` field unfillable by construction.
func fileImpossibleInteraction(mode InteractionMode) bool {
	return mode == InteractionLLM || mode == InteractionReview
}

// validateReservedAnswerKeys rejects (C130) a human node whose output
// schema declares a key the engine writes on resume. Human nodes only:
// on any other node these names are ordinary fields nothing overwrites.
func (c *compiler) validateReservedAnswerKeys(w *Workflow) {
	for _, n := range w.Nodes {
		if n.NodeKind() != NodeHuman {
			continue
		}
		schemaName := NodeOutputSchema(n)
		if schemaName == "" {
			continue
		}
		schema := w.Schemas[schemaName]
		if schema == nil {
			continue
		}
		for _, f := range schema.Fields {
			why, reserved := reservedAnswerKeys[f.Name]
			if !reserved {
				continue
			}
			c.errorfAt(DiagReservedAnswerKey, n.NodeID(), "",
				"human %q declares output_schema %q with the field %q, which the engine reserves (it %s) — the operator's answer would be silently overwritten on resume; rename the field",
				n.NodeID(), schemaName, f.Name, why)
		}
	}
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
// dependency-free; keep in sync with the accepted values in
// rewrite.ParseMode.
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
	forEachAgentJudgeToolValue(w,
		LLMNode.GetCompress,
		func(nn *ToolNode) string { return nn.Compress },
		func(n Node, kind, compress string) {
			if !valid(compress) {
				c.errorf(DiagInvalidCompress,
					"%s %q has invalid compress %q; valid values are on, off, ultra",
					kind, n.NodeID(), compress)
			}
		})
}

// validateLoopBudgetGuard enforces that loop_budget_guard is one of the
// accepted barewords. A typo would silently read as "unset" — i.e. the
// default, which is ON — so an operator writing `off` and mistyping it
// would keep a guard they meant to lift, with nothing said. Empty ("")
// is unset and always valid; the comparison is case-insensitive and
// whitespace-trimmed.
func (c *compiler) validateLoopBudgetGuard(w *Workflow) {
	switch strings.ToLower(strings.TrimSpace(w.LoopBudgetGuard)) {
	case "", "on", "off":
		return
	}
	c.errorf(DiagInvalidLoopBudgetGuard,
		"workflow %q has invalid loop_budget_guard %q; valid values are on, off",
		w.Name, w.LoopBudgetGuard)
}

// validateAutoMemory enforces that every auto_memory value (workflow-level +
// every agent/judge node) is one of the accepted barewords, and warns when a
// node asks for it on a backend that cannot deliver it. A typo would silently
// read as "inherit" — i.e. off — so an invalid value is an ERROR. Empty ("")
// means unset/inherit and is always valid; the comparison is case-insensitive
// and whitespace-trimmed.
//
// The accepted values are kept inline (pkg/backend/model would cycle), but
// the BACKEND allowlist is not: it comes from automemory.SupportsBackend, so
// the warning and the engine can never disagree about who honours the knob.
func (c *compiler) validateAutoMemory(w *Workflow) {
	norm := func(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
	valid := func(v string) bool {
		switch norm(v) {
		case "", "on", "off":
			return true
		}
		return false
	}
	if !valid(w.AutoMemory) {
		c.errorf(DiagInvalidAutoMemory,
			"workflow %q has invalid auto_memory %q; valid values are on, off",
			w.Name, w.AutoMemory)
	}
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		kind, value := nn.NodeKind().String(), nn.GetAutoMemory()
		if !valid(value) {
			c.errorf(DiagInvalidAutoMemory,
				"%s %q has invalid auto_memory %q; valid values are on, off",
				kind, n.NodeID(), value)
			continue
		}
		// The workflow default reaches every node, so a mixed-backend
		// workflow would warn on each node that cannot honour it. Only an
		// explicit per-node `on` is worth a diagnostic.
		if norm(value) != "on" {
			continue
		}
		// The EFFECTIVE backend, mirroring the runtime's resolution: a node
		// with no `backend:` inherits the workflow's `default_backend:`.
		// Reading the node's own field alone missed the likeliest shape by
		// far — the backend declared once at the workflow level rather than
		// repeated on every node — and the author then got nothing at all:
		// no warning here, and a runtime that (correctly) skips the mirror in
		// silence. An empty effective backend is left alone: the resolver
		// falls through to env and host credential detection, so the compiler
		// genuinely cannot know.
		backend := nn.GetLLMFields().Backend
		if backend == "" {
			backend = w.DefaultBackend
		}
		if backend != "" && !automemory.SupportsBackend(backend) {
			c.warnf(DiagAutoMemoryNotSupported,
				"%s %q: auto_memory: on has NO effect on backend=%q — MEMORY.md is wired for claude_code, claw and pi only",
				kind, n.NodeID(), backend)
		}
	}
	c.warnIfWorkflowAutoMemoryIsInert(w)
}

// warnIfWorkflowAutoMemoryIsInert covers the one shape the per-node rule above
// deliberately stays silent on: `auto_memory: on` declared ONCE at the
// workflow level, where no node can honour it.
//
// The per-node rule fires only on an explicit per-node `on`, because a
// workflow default reaches every node and a mixed-backend workflow would warn
// on each one that cannot use it — noise that trains an author to ignore the
// diagnostic. But the same silence covers the case where the author's single
// `on` does nothing at all, anywhere, and the runtime then skips the mirror
// without a word: memory is simply always empty, with nothing to search for.
//
// So the warning is emitted only when NOTHING in the workflow could honour the
// setting — one message about the workflow, not one per node. A node whose
// effective backend is unresolved counts as "could": the resolver falls
// through to env and host credential detection, so the compiler genuinely
// cannot know, and guessing would produce a false warning on a workflow that
// works.
func (c *compiler) warnIfWorkflowAutoMemoryIsInert(w *Workflow) {
	if strings.ToLower(strings.TrimSpace(w.AutoMemory)) != "on" {
		return
	}
	// The two ways a node can fail to use the setting are counted apart,
	// because they need different words. "No backend supports it" is a wiring
	// fact; "every node opted out" is the author's own `auto_memory: off` on a
	// backend that would have honoured it — telling them their backends are
	// unsupported there sends them looking for a problem that is not real,
	// which is how a diagnostic teaches people to ignore diagnostics.
	unsupported, optedOut := 0, 0
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		backend := nn.GetLLMFields().Backend
		if backend == "" {
			backend = w.DefaultBackend
		}
		// Unresolved: the runtime falls through to env and host credential
		// detection, so the compiler genuinely cannot know and must not guess.
		supported := backend == "" || automemory.SupportsBackend(backend)
		if strings.ToLower(strings.TrimSpace(nn.GetAutoMemory())) == "off" {
			if supported {
				optedOut++
			}
			continue
		}
		if supported {
			return // something honours it; the setting is doing its job
		}
		unsupported++
	}
	switch {
	case unsupported > 0 && optedOut > 0:
		// Both causes at once. Naming only the first would contradict itself —
		// "MEMORY.md is wired for claw only" is a strange thing to read on a
		// workflow that HAS a claw node, and it points the author at the
		// backends when half the answer is their own per-node `off`.
		c.warnf(DiagAutoMemoryNotSupported,
			"workflow %q sets auto_memory: on but nothing honours it: %d agent/judge node(s) override it with auto_memory: off, and the remaining %d are on a backend that ignores it (MEMORY.md is wired for claude_code, claw and pi only)",
			w.Name, optedOut, unsupported)
	case unsupported > 0:
		c.warnf(DiagAutoMemoryNotSupported,
			"workflow %q sets auto_memory: on but NO agent/judge node can honour it — MEMORY.md is wired for claude_code, claw and pi only",
			w.Name)
	case optedOut > 0:
		c.warnf(DiagAutoMemoryNotSupported,
			"workflow %q sets auto_memory: on but every agent/judge node overrides it with auto_memory: off — the workflow default has no effect",
			w.Name)
	}
	// Neither: the workflow has no agent/judge node at all. Nothing to honour
	// it either way, and nothing worth telling the author.
}

// forEachAgentJudgeToolValue visits every agent/judge/tool node in the
// workflow, extracting a per-node string field (compress, permission, ...)
// via the supplied getters — the two DSL-level node kinds that carry such
// overrides. Other node kinds are skipped.
func forEachAgentJudgeToolValue(w *Workflow, llmGet func(LLMNode) string, toolGet func(*ToolNode) string, fn func(n Node, kind, value string)) {
	for _, n := range w.Nodes {
		var value, kind string
		switch nn := n.(type) {
		case LLMNode:
			value, kind = llmGet(nn), nn.NodeKind().String()
		case *ToolNode:
			value, kind = toolGet(nn), "tool"
		default:
			continue
		}
		fn(n, kind, value)
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
	forEachAgentJudgeToolValue(w,
		LLMNode.GetPermission,
		func(nn *ToolNode) string { return nn.Permission },
		func(n Node, kind, perm string) {
			if !valid(perm) {
				c.errorf(DiagInvalidPermission,
					"%s %q has invalid permission %q; valid values are off, ask, deny",
					kind, n.NodeID(), perm)
				return
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
		})

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

// validateNodeTimeout checks the per-node `timeout:` on LLM nodes (agent/judge)
// is a well-formed, positive Go duration. Env-substituted forms
// ("${VAR:-20m}") are expanded first so the default value is validated; a bare
// unset ${VAR} expands to "" and is deferred to the runtime resolver rather
// than flagged as a false positive.
func (c *compiler) validateNodeTimeout(w *Workflow) {
	for _, node := range w.Nodes {
		n, ok := node.(LLMNode)
		if !ok {
			continue
		}
		raw := n.GetLLMFields().Timeout
		if raw == "" {
			continue
		}
		expanded := ExpandEnvWithDefault(raw)
		if expanded == "" {
			continue
		}
		d, err := time.ParseDuration(expanded)
		if err != nil {
			c.errorf(DiagInvalidNodeTimeout,
				"node %q has an invalid timeout %q: %v", node.NodeID(), raw, err)
			continue
		}
		if d <= 0 {
			c.errorf(DiagInvalidNodeTimeout,
				"node %q timeout must be positive, got %q", node.NodeID(), raw)
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
