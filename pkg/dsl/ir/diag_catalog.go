package ir

// DiagInfo is the catalogue entry of one diagnostic code: what the check is
// and the one-line fix an author can act on without opening the reference.
// The Fix line is what `Diagnostic.Hint` carries when the emitting site did
// not set a more specific one, so every diagnostic the compiler produces
// arrives with an actionable next step — in `iterion validate`, in the
// studio's inline badge and in the MCP `local_validate` result alike.
//
// docs/references/diagnostics.md remains the long-form reference (cause,
// context, examples); TestDiagCatalogCoversEveryCode keeps the two in step
// by refusing a code that exists in the compiler but not here.
type DiagInfo struct {
	Title string // the check, as a short noun phrase
	Fix   string // the imperative one-line remedy
}

// Catalog maps every compile/validation diagnostic code the ir package can
// emit to its title and fix line. Bundle-consistency codes (C2xx, pkg/bundlelint)
// carry their own hints and are not listed here.
var Catalog = map[DiagCode]DiagInfo{
	// Compilation.
	DiagUnknownNode:           {"Unknown node reference", "Declare the node or fix the name — an edge, `entry:` or `watches:` may only name a declared node."},
	DiagUnknownSchema:         {"Unknown schema reference", "Declare `schema <name>:` or fix the name in `input:` / `output:`."},
	DiagUnknownPrompt:         {"Unknown prompt reference", "Declare `prompt <name>:` (or drop a `prompts/<name>.md` in the bundle) or fix the name in `system:` / `user:`."},
	DiagBadTemplateRef:        {"Bad template reference", "Use `{{vars.X}}`, `{{input.X}}`, `{{outputs.node.field}}`, `{{artifacts.X}}`, `{{attachments.X}}`, `{{loop.name.iteration}}` or `{{run.id}}`; a literal example needs prose, not braces."},
	DiagDuplicateLoop:         {"Conflicting loop definitions", "Edges sharing a loop name must agree on the cap; use one cap or two loop names."},
	DiagNoWorkflow:            {"No workflow found", "Add a `workflow <name>:` block with an `entry:` and the edges."},
	DiagMultipleWorkflow:      {"Multiple workflows", "Keep one `workflow` block per file; split the others into their own `.bot` (a `subbot` can run them)."},
	DiagMissingEntry:          {"Missing entry node", "Point `entry:` at a declared node."},
	DiagMissingModelOrBackend: {"Missing model or backend", "Add `model: \"...\"` or `backend: \"...\"` to the node (an LLM-backed human node needs `interaction_model:` and `output:`)."},
	DiagDuplicateMCPServer:    {"Duplicate MCP server", "Give each `mcp_server` declaration a unique name."},
	DiagInvalidMCPServer:      {"Invalid MCP server config", "stdio servers need `command:`; http/sse servers need `url:` and must not set `command:`/`args:`."},
	DiagComputeNoExpr:         {"Compute node has no expressions", "Add an `expr:` block mapping each output field to an expression, or remove the node."},
	DiagBadExpr:               {"Expression failed to parse", "`expr:` values and quoted `when` clauses are expressions, not templates: write `input.x`, not `{{input.x}}`; check operators, parentheses and builtin names."},
	DiagDuplicateNodeID:       {"Duplicate node id", "Rename one of the declarations — node ids share one global namespace across all kinds."},
	DiagReservedNodeName:      {"Reserved node name", "`done` and `fail` are the reserved terminal targets; pick another name."},
	DiagInvalidSandboxMode:    {"Invalid sandbox mode", "Use `sandbox: auto`, `sandbox: none`, or a block with exactly one of `image:` / `build:`."},
	DiagSandboxAutoNoConfig:   {"Sandbox auto without config", "Add a `.devcontainer/devcontainer.json`, a default image, or an inline `sandbox:` block with `image:`/`build:`."},
	DiagBudgetCostInvalid:     {"Invalid budget cost", "Use a non-negative finite USD amount for `max_cost_usd`, or omit it."},

	// Validation — edges, sessions and reachability.
	DiagSessionAfterConvergence:  {"Session at convergence point", "A node with several incoming branches cannot `session: inherit` or `fork`; use `fresh`, `artifacts_only` or `persist`."},
	DiagMultipleDefaultEdges:     {"Multiple unconditional edges", "Keep one default edge per node (a loop back-edge `as name(N)` plus one exhaustion fall-through is the allowed pair); fan out with a router."},
	DiagAmbiguousCondition:       {"Ambiguous conditions", "Two edges from one node test the same field with the same polarity; remove the duplicate or change the condition."},
	DiagMissingFallback:          {"Missing fallback", "Complete `when X` with `when not X`, an `else` edge, or an unconditional edge from the same node."},
	DiagConditionNotBool:         {"Condition field not boolean", "A bare `when field` needs a `bool` field in the source's `output:` schema; use a quoted expression (`when \"count > 0\"`) for other types."},
	DiagConditionFieldNotFound:   {"Condition field not found", "Add the field to the source node's `output:` schema, or fix the name."},
	DiagElseWithoutConditional:   {"Else without conditional sibling", "`else` needs a `when` sibling from the same node; otherwise write a plain `src -> dst` edge."},
	DiagMultipleElseEdges:        {"Multiple else edges", "Keep exactly one `else` edge per source node."},
	DiagElseWithUnconditional:    {"Else alongside unconditional", "`else` IS the fallback: remove the bare unconditional edge, or drop the `else` keyword."},
	DiagSandboxOptOut:            {"Sandbox opt-out", "Remove `sandbox: none` to run sandboxed; keep the opt-out only when the flow genuinely needs the host environment."},
	DiagUnreachableNode:          {"Unreachable node", "Wire an edge from a reachable node, or remove the declaration."},
	DiagHistoryRefNotInLoop:      {"History ref not in loop", "`{{outputs.node.history}}` only exists inside a declared loop (`as name(N)` on the back-edge); add the loop or drop `.history`."},
	DiagUndeclaredCycle:          {"Undeclared cycle", "Declare the back-edge as a bounded loop: `src -> dst as name(N)` (or `as name(unbounded N)`), and give the loop an exhaustion exit edge."},
	DiagRoundRobinTooFewEdges:    {"Round-robin too few edges", "A `round_robin` router needs at least two unconditional outgoing edges."},
	DiagLLMRouterTooFewEdges:     {"LLM router too few edges", "An `llm` router needs at least two outgoing edges to choose from."},
	DiagLLMRouterConditionEdge:   {"LLM router edge has condition", "Remove `when` from the edges of an `llm` router; the model selects the target."},
	DiagRouterLLMOnlyProperty:    {"LLM-only property on non-LLM router", "`model`, `backend`, `system`, `user`, `multi` and `reasoning_effort` belong to `mode: llm`; remove them or change the mode."},
	DiagInvalidLoopIterations:    {"Invalid loop iterations", "A loop cap must be at least 1."},
	DiagInvalidReasoningEffort:   {"Invalid reasoning effort", "Use `low`, `medium`, `high`, `xhigh`, `max` or `ultracode` (or a quoted `${VAR}` string)."},
	DiagDuplicateWithKey:         {"Duplicate with-mapping key", "The same `with` key reaches one target from several unconditional edges; use distinct keys or make the edges conditional."},
	DiagUnknownRefNode:           {"Unknown outputs node reference", "`{{outputs.<node>}}` names a node that is not declared; declare it or fix the typo."},
	DiagRefFieldNotInSchema:      {"Outputs ref field not in output schema", "Reference a field the node's `output:` schema declares, or add it to the schema."},
	DiagRefNodeNoSchema:          {"Outputs ref on schemaless node", "Add an `output:` schema to the referenced node so the field can be verified, or use `{{vars.x}}` for a launch-time value."},
	DiagUndeclaredVar:            {"Undeclared variable", "Declare the variable in `vars:` (top level or workflow), or fix the name."},
	DiagInputFieldNotInSchema:    {"Input ref field not in the validated namespace", "In a prompt/command/expr, `{{input.x}}` must be in the node's `input:` schema; in an edge `with`, it is the SOURCE node's `output:` — use `{{outputs.<node>.x}}` or `{{vars.x}}` otherwise."},
	DiagUnknownArtifact:          {"Unknown artifact", "Add `publish: <name>` on an earlier node, or fix the artifact name."},
	DiagRefNodeNotReachable:      {"Reference to non-reachable node", "`{{outputs.<node>}}` must name a node that runs before the consumer; wire the graph so the producer comes first."},
	DiagNodeMaxTokensVsBudget:    {"Node max_tokens exceeds workflow budget", "Lower the node's `max_tokens` or raise `budget.max_tokens`."},
	DiagUnsupportedMCPAuth:       {"Unsupported MCP auth type", "Only `oauth2` is wired; drop the `auth:` block or set `type: \"oauth2\"`."},
	DiagInvalidCompaction:        {"Invalid compaction values", "`threshold` is a fraction in (0, 1] and `preserve_recent` an integer >= 1."},
	DiagMemoryNotSupported:       {"Memory enabled on unsupported backend", "Only `backend: \"claw\"` wires the memory tools today; switch the backend or drop the `memory:` block."},
	DiagMemoryMissingScope:       {"Memory missing scope", "Add `scope: <name>` to the `memory:` block."},
	DiagArtifactLabelsNoPublish:  {"Artifact labels without publish", "Add `publish: <name>` so the labels have an artifact to attach to, or remove `artifact_labels:`."},
	DiagMemoryInvalidVisibility:  {"Invalid memory visibility", "Use one of `bot`, `project`, `cross_project`, `user`, `org`, `global`."},
	DiagMemoryVisibilityConflict: {"Memory visibility conflict", "Use `visibility:` alone; drop the legacy `project_root:`."},
	DiagBadPromptInclude:         {"Bad prompt include", "Point `{{include \"...\"}}` at an existing file, with a path relative to the file that contains the include (the `.bot`'s directory for a prompt declared in it, a bundle's `prompts/` for a `prompts/*.md`), never escaping it, under 256 KiB."},

	// Attachments.
	DiagDuplicateAttachment:       {"Duplicate attachment", "Rename or merge the duplicate `attachments:` entry."},
	DiagAttachmentVarConflict:     {"Attachment / var name collision", "Rename one of them — attachments and vars share a template namespace."},
	DiagInvalidAttachmentMIME:     {"Invalid attachment MIME", "Use `type/subtype` MIME values (`image/png`, `application/pdf`, `image/*`)."},
	DiagUnknownAttachment:         {"Unknown attachment reference", "Declare the attachment in an `attachments:` block, or fix the name."},
	DiagAttachmentSubfieldUnknown: {"Unknown attachment sub-field", "Use `.path`, `.url`, `.mime`, `.size` or `.sha256`, or drop the sub-field."},

	// Browser pane.
	DiagPlaywrightNeedsBrowserImage: {"Playwright MCP server requires a browser-capable sandbox image", "Use a browser-capable image (e.g. `ghcr.io/socialgouv/iterion-sandbox-browser`) or remove the Playwright MCP server."},

	// Presets.
	DiagPresetUnknownVar:   {"Preset references unknown variable", "Declare the variable in `vars:`, or fix/remove the preset key."},
	DiagPresetTypeMismatch: {"Preset value type mismatch", "Match the preset value to the variable's declared type."},
	DiagDuplicatePreset:    {"Duplicate preset name", "Rename or merge the duplicate preset."},

	// Capabilities, cursors, providers, ultracode.
	DiagUnknownCapability:    {"Unknown capability", "Use a registered capability (`board.read`, `board.create`, `board.move`, `board.assign`, `board.label`, `board.close`, `board.comment`, `watch.subscribe`, `watch.unsubscribe`) or accept the warning for an extension."},
	DiagMalformedCapability:  {"Malformed capability", "Use the lowercase `domain.action` shape, e.g. `board.create`."},
	DiagBoardCapInSandbox:    {"Board capability inside sandbox", "No action if the iterion HTTP server is reachable from the sandbox; otherwise drop the capability or disable the sandbox for the node."},
	DiagUnknownCursor:        {"Unknown cursor reference", "Declare `cursor <name>:` at top level, or drop the setting."},
	DiagInvalidCursorVal:     {"Invalid cursor value", "Use a declared enum value, or a number in [0, 1] that falls in a declared band."},
	DiagMalformedCursor:      {"Malformed cursor declaration", "Declare exactly one of `values:` or `bands:`; bands must be disjoint sub-ranges of [0, 1]."},
	DiagDuplicateCursor:      {"Duplicate cursor name", "Rename or merge the duplicate `cursor` declaration."},
	DiagUnknownProvider:      {"Unknown provider", "Fix the provider name, or accept the warning for a newly added provider."},
	DiagProviderChainIgnored: {"Provider chain ignored", "Only `claude_code` honours a multi-element `provider:` chain; drop the extra elements or switch the backend."},
	DiagUltracodeModelGate:   {"Ultracode model gate", "Use an Opus 4.8 / Claude 5 model for full `ultracode`, or accept the degrade to `xhigh`."},

	// Secrets.
	DiagDuplicateSecret:   {"Duplicate secret", "Rename or merge the duplicate `secrets:` entry."},
	DiagSecretVarConflict: {"Secret / var name collision", "Rename one of them — secrets and vars share a template namespace."},
	DiagInvalidSecretHost: {"Invalid secret host", "Use hostnames or domains in `hosts:` (quoted strings)."},
	DiagUnknownSecret:     {"Unknown secret reference", "Declare the secret in the `secrets:` block, or fix the name."},
	DiagInvalidSecretFile: {"Malformed file secret", "An `as: file` secret takes an optional `value:`/`env:`/`mount_path:`; a bare `as: file` (+ `optional: true`) resolves the stored secret by name."},
	DiagSecretSubfield:    {"Unsupported secret sub-field", "Use `{{secrets.X}}` or `{{secrets.X.path}}` (file secrets only)."},

	// Unbounded loops.
	DiagUnboundedNoFuel: {"Unbounded loop without fuel", "Give the loop a fuel ceiling: `as name(unbounded 200)` or a workflow `budget.max_iterations`."},
	DiagUnboundedNoExit: {"Unbounded loop without exit", "Add a `when` exit edge so the loop can terminate by its own logic, not only by fuel."},

	// Review gate, compress, memory switch, guards.
	DiagReviewNeedsWorktree:    {"Review without worktree", "Add `worktree: auto` to the workflow (the review gate merges the run's worktree), or drop `interaction: review`."},
	DiagReviewURLUnknownRef:    {"Review URL unknown ref", "Point `review_url` at a declared node's output, or remove it."},
	DiagInvalidCompress:        {"Invalid compress value", "Use `on`, `off` or `ultra`."},
	DiagQuotedCommandRef:       {"Tool command quotes a ref the runtime already quotes", "Remove the quotes around `{{ref}}` in the command — the runtime shell-escapes every ref; build optional flags with `${VAR:+--flag \"$VAR\"}` from a bare `VAR={{ref}}`."},
	DiagInvalidAutoMemory:      {"Invalid auto_memory value", "Use `on` or `off`, or drop the field to inherit."},
	DiagAutoMemoryNotSupported: {"auto_memory on an unsupported backend", "Use `claude_code`, `claw` or `pi`, or drop `auto_memory:`."},
	DiagInvalidLoopBudgetGuard: {"Invalid loop_budget_guard value", "Use `on` or `off`, or drop the field to inherit."},
	DiagInvalidRepoDevbox:      {"Invalid repo_devbox value", "Use `on` or `off`, or drop the field to inherit."},

	DiagInvalidWorkspaceCheckpoint: {"Invalid workspace_checkpoint value", "Use `on` or `off`, or drop the field to inherit. The default is `on`, so a typo keeps pushing the run's tree to the repository it was pointed at."},

	// Verified actions (ADR-044).
	DiagInvalidPolicy:        {"Invalid policy", "Use `required`, `recover` or `best_effort`."},
	DiagRecoveryNoPostcond:   {"Recovery without postcondition", "Add a `postcondition:` (the deterministic oracle) or drop the recovery."},
	DiagRecoveryOnGate:       {"Recovery on a gate", "Remove `recovery:` — a node whose recipe IS its postcondition is a gate and stays deterministic."},
	DiagRecoveryWithoutRecov: {"Recovery without recover policy", "Set `policy: recover`, or remove the `recovery:` bounds."},

	// Typing.
	DiagEnumLiteralMismatch:     {"Enum literal never matches", "Compare against one of the field's declared `enum:` values, or extend the enum."},
	DiagExprOperandTypeMismatch: {"Expression operand type mismatch", "Compare operands of compatible types (a `string[]` is not an `int`)."},
	DiagWhenExprNotBoolish:      {"when-expression not boolean", "Write a comparison (`when \"count > 0\"`) instead of a bare number."},
	DiagVarDefaultTypeMismatch:  {"Var default type mismatch", "Make the default literal match the declared type (`count: int = 3`)."},
	DiagInvalidPermission:       {"Invalid permission", "Use `off`, `ask` or `deny`."},
	DiagPermissionRulesNoGate:   {"Permission rules without gate", "Set `permission: ask` or `deny` on the workflow so `allow`/`ask`/`deny` rules apply, or remove the lists."},
	DiagToolNodePermissionInert: {"Tool-node permission inert", "Remove `permission:` from the tool node; gate the agent nodes instead."},
	DiagGatedCLIBackendSandbox:  {"Gated backend needs a host-side run", "Declare `sandbox: none` (workflow or node), launch with `--sandbox none`, or use a deny-shaped policy on claw."},
	DiagIndexOnScalar:           {"Index on scalar", "Index an array or map; drop the subscript on a string/bool/number."},
	DiagInvalidNodeTimeout:      {"Invalid node timeout", "Use a positive Go duration string, e.g. `timeout: \"20m\"`."},
	DiagFileFieldNotHuman:       {"file field outside a human pause", "Move the `file` field to a human node's `output:` with `interaction: human` (or `llm_or_human`), or use `string` for a path the node computes."},
	DiagReservedAnswerKey:       {"Reserved answer key", "Rename the field — `_attachments` is written by the engine on resume."},
	DiagVarEnumNonString:        {"Var enum on non-string type", "Declare the var as `string`, or drop the `[enum: ...]` constraint."},
	DiagVarDefaultNotInEnum:     {"Var default not in enum", "Use one of the enum values as the default, or extend the list."},
	DiagVarEnumDuplicate:        {"Duplicate var enum value", "Remove the duplicate value."},
	DiagBuiltinArity:            {"Builtin call the evaluator cannot satisfy", "Fix the argument count (`length`/`sort`/`floor`/`round` take 1; `contains`/`join`/`tail` take 2; `if`/`slice` take 3; `concat`/`min`/`max` take 1+), or run on an engine that accepts this shape (`requires.iterion`)."},
	DiagFanOutEachMissingOver:   {"fan_out_each without over", "Add `over: \"{{...array...}}\"` to the router."},
	DiagFanOutEachOnlyProperty:  {"fan_out_each property on non-fan_out_each", "Remove `over`/`as`/`key`/`depends_on`, or set `mode: fan_out_each`."},
	DiagFanOutEachEdges:         {"fan_out_each edge count", "Keep exactly one unconditional template edge from the router."},
	DiagUseUnknownGroup:         {"Use references unknown group", "Declare `group <name>(...)`, or fix the name."},
	DiagUseParamMismatch:        {"Use param mismatch", "Bind exactly the group's declared params in `with { ... }`."},
	DiagForeachConflictsLoop:    {"foreach conflicts with loop", "Use one iteration form per edge: `as foreach` OR `as <loop>(N)`."},
	DiagSubbotNoSource:          {"subbot without source", "Add `source: \"<child>.bot\"` (relative to this file)."},

	// Fallback chains (ADR-087).
	DiagMalformedProviderStep: {"Malformed provider step", "Write both parts of a `provider:model` element, e.g. `anthropic:claude-sonnet-4-6`."},
	DiagFallbackMalformed:     {"Malformed fallback route", "Give the route a distinct name and target; a route that changes `backend:` must also pin its own `model:`."},
	DiagCommandIgnored:        {"Command ignored", "Only `claude_code` honours a per-node `command:` override; switch the backend or drop it."},
	DiagFallbackUnknownOn:     {"Unknown fallback trigger", "Use `usage_window`, `auth`, `unavailable`, `transient_exhausted` or `any` in `on:`."},
	DiagFallbackUnsafeCross:   {"Unsafe fallback crossing", "Route to a gate-enforcing backend (`claude_code`/`claw`/`pi`), declare an explicit `tools:` list, or drop session continuity when crossing backends."},
	DiagFallbackDrift:         {"Fallback capability drift", "Accept the degradation, or pin the route to a backend that honours the setting."},

	// Supervisors, resources, events, skills.
	DiagUnknownWatchedNode:      {"Supervisor watches non-agent", "Watch an agent node, or fix the node id."},
	DiagMalformedSupervisor:     {"Malformed supervisor", "Use a valid Go duration for `cooldown:`."},
	DiagDuplicateSupervisor:     {"Duplicate supervisor", "Rename or merge the duplicate `supervisor` declaration."},
	DiagUnknownSupervisorPrompt: {"Unknown supervisor prompt", "Declare the prompt named by `system:`, or fix the name."},
	DiagResourceCapInvalid:      {"Invalid resource capacity", "Use a capacity >= 1 (or a non-empty list of named members)."},
	DiagUnknownResourceInNeeds:  {"Unknown resource in needs", "Declare the resource in the workflow's `resources:` block, or fix the name."},
	DiagEventNoName:             {"Event node without name", "Add `event: \"<name>\"` to the emit/wait node."},
	DiagWaitNoTimeout:           {"Wait without timeout", "Add `timeout: \"30s\"` (a positive Go duration) — no silent infinity."},
	DiagEventNoListener:         {"Dangling event", "Pair each `wait` with an `emit` of the same event name (and vice versa)."},
	DiagInvalidSkillRef:         {"Invalid skill reference", "Use a single path segment of letters/digits/`.`/`-`/`_`; quote kebab-case names (`skills: [\"changelog-writer\"]`)."},
	DiagUnknownTool:             {"Unknown tool name", "Use a claw built-in (`read_file`, `write_file`, `file_edit`, `glob`, `grep`, `bash`, `web_fetch`, ...) or an MCP tool by its `mcp.<server>.<tool>` name; the message names the nearest match."},

	// Async interaction, parallel branches, typed fails (C24x band).
	DiagAsyncOnHuman:          {"Async interaction on human node", "Move `interaction: async` to the asking agent/judge and add an `await_answers` node as the sync point."},
	DiagAwaitAnswersNoTimeout: {"await_answers without timeout", "Add `timeout: \"30m\"` (a positive Go duration) — no silent infinity."},
	DiagAwaitAnswersBadFrom:   {"await_answers with dead from", "Point `from:` at an `interaction: async` agent/judge, or drop it to await the whole run."},
	DiagPersistInFanOut:       {"Persist in fan-out body", "Move the `session: persist` node to the trunk after the join, or use `session: fresh` inside the branch."},
	DiagLoopInExecBranch:      {"Bounded iteration crosses a parallel-branch boundary", "Keep every node of the cycle inside one branch with its own loop name, move the loop to the trunk (wrap the router from the join), or use a `subbot`."},
	DiagHumanModeInExecBranch: {"Trunk-only human mode in parallel branch", "Use a plain `interaction: human` gate inside the branch, or move the review / llm_or_human gate after the collector."},
	DiagImplicitCollectorMove: {"Implicit fan-out collector execution moved into branches", "Add `await: wait_all` or `await: best_effort` to the intended collector, or keep it unmarked when per-branch execution is intended."},
	DiagInvalidFailCode:       {"Malformed fail code", "Use an UPPER_SNAKE identifier: `code: PLAN_BUDGET_EXHAUSTED`."},
	DiagReservedFailCode:      {"Fail code collides with an engine code", "Pick a code of the bot's own (`BUDGET_EXCEEDED`, `TIMEOUT`, `USAGE_LIMIT_BLOCKED`, ... are the engine's)."},
	DiagDuplicateFanOutTarget: {"Duplicate fan-out target", "Remove the duplicate edge; use a `fan_out_each` router when the intent is N executions of one node."},
}

// HintFor returns the catalogue fix line for code, or "" when the code has
// no entry (the emitting site is then expected to have set its own hint).
func HintFor(code DiagCode) string {
	return Catalog[code].Fix
}
