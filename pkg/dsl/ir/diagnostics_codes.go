package ir

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
	DiagElseWithoutConditional   DiagCode = "C015" // else edge with no conditional (when) sibling
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
	DiagUnknownRefNode           DiagCode = "C029" // outputs ref to non-existent node
	DiagRefFieldNotInSchema      DiagCode = "C031" // outputs ref field not in output schema
	DiagRefNodeNoSchema          DiagCode = "C032" // outputs ref field on node without output schema
	DiagUndeclaredVar            DiagCode = "C033" // vars ref to undeclared variable
	DiagInputFieldNotInSchema    DiagCode = "C034" // input ref field not in input schema
	DiagUnknownResourceInNeeds   DiagCode = "C195" // needs: references a resource not declared in resources:
	DiagUnknownArtifact          DiagCode = "C035" // artifacts ref to unpublished artifact
	DiagRefNodeNotReachable      DiagCode = "C036" // outputs ref to node not reachable before consumer
	DiagNodeMaxTokensVsBudget    DiagCode = "C037" // node-level max_tokens exceeds workflow.budget.max_tokens
	DiagUnsupportedMCPAuth       DiagCode = "C038" // MCP server Auth.Type not supported (only "oauth2" is wired)
	DiagMultipleElseEdges        DiagCode = "C123" // more than one else edge from the same source
	DiagElseWithUnconditional    DiagCode = "C124" // else edge alongside a bare unconditional sibling
	DiagInvalidCompaction        DiagCode = "C043" // compaction.threshold or compaction.preserve_recent out of range
	DiagMemoryNotSupported       DiagCode = "C047" // memory: enabled on a backend that does not consume it (only claw does today)
	DiagMemoryMissingScope       DiagCode = "C048" // memory: enabled without a scope: name
	DiagArtifactLabelsNoPublish  DiagCode = "C049" // artifact_labels: set on a node with no publish: (nothing to attach to)
	DiagMemoryInvalidVisibility  DiagCode = "C170" // memory: unknown visibility value
	DiagMemoryVisibilityConflict DiagCode = "C171" // memory: visibility: with the legacy project_root:
	DiagBadPromptInclude         DiagCode = "C055" // prompt {{include "..."}} marker could not be resolved

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

	// Unbounded-loop diagnostics (Turing-completeness Layer B).
	DiagUnboundedNoFuel DiagCode = "C097" // `as X(unbounded)` without a fuel ceiling (clause fuel or budget.max_iterations) (error)
	DiagUnboundedNoExit DiagCode = "C098" // unbounded loop whose back-edges have no sibling when-exit (warning)

	// Review-gate diagnostics (interaction: review).
	DiagReviewNeedsWorktree DiagCode = "C100" // interaction: review without worktree: auto — nothing to merge (error)
	DiagReviewURLUnknownRef DiagCode = "C101" // review_url references an output node that does not exist (warning)

	// Compress output-compression mode diagnostics.
	DiagInvalidCompress DiagCode = "C102" // compress: value not one of on|off|ultra (error)

	// Backend auto-memory (MEMORY.md) switch diagnostics.
	DiagInvalidAutoMemory      DiagCode = "C131" // auto_memory: value not one of on|off (error)
	DiagAutoMemoryNotSupported DiagCode = "C132" // auto_memory: on for a backend that does not consume it (warning)

	// Loop back-edge affordability guard diagnostics.
	DiagInvalidLoopBudgetGuard DiagCode = "C133" // loop_budget_guard: value not one of on|off (error)

	// Target-repo devbox provisioning switch diagnostics.
	DiagInvalidRepoDevbox DiagCode = "C134" // repo_devbox: value not one of on|off (error)

	// Static cross-node typing diagnostics (Phase 2). These resist the
	// looseness that makes the rest of the validator a graph linter: they
	// fire ONLY on genuinely-typed slots (enum literals compared against an
	// enum-typed field, typed operands inside compute/when expressions),
	// never on template stringification. A json (= any) field or an
	// unknown ref always bails to "no opinion" so legitimate looseness
	// keeps passing.
	//
	// NOTE: an earlier draft also checked edge with-mapping keys/types
	// against the target node's input schema. That was dropped: the runtime
	// (engine.buildNodeInputRS) passes EVERY with-key through verbatim and
	// never validates node input against the declared input schema — the
	// schema is advisory, not a contract a with-mapping must satisfy — so
	// such a check rests on a false premise.
	//
	// C103-C106 belong to the Verified Action family (ADR-044, see
	// validate_verified_action.go); the enum-literal check below is C121 so
	// it joins the expr-type cluster (C107/C108/C120/C121) and does not
	// collide with DiagInvalidPolicy.
	DiagEnumLiteralMismatch     DiagCode = "C121" // comparison literal outside the target field's enum set (error)
	DiagExprOperandTypeMismatch DiagCode = "C107" // compute/when expression operands incompatible under the operator (warning)
	DiagWhenExprNotBoolish      DiagCode = "C108" // when-expression result clearly not bool-coercible (warning)
	DiagVarDefaultTypeMismatch  DiagCode = "C109" // a var's default literal type does not match its declared type (error)
	DiagInvalidPermission       DiagCode = "C110" // permission: value not one of off|ask|deny (error)
	DiagPermissionRulesNoGate   DiagCode = "C111" // allow/ask/deny rules declared but the resolved permission mode is "" or off (warning)
	DiagToolNodePermissionInert DiagCode = "C112" // permission: on a tool node — parsed but not enforced (warning)
	DiagIndexOnScalar           DiagCode = "C120" // subscript `[...]` applied to a statically-scalar value (warning) — C113-C119 taken by the fan_out_each/groups epic
	DiagInvalidNodeTimeout      DiagCode = "C122" // LLM node `timeout:` is not a valid Go duration (error) — C121 taken, C199 is skill-ref on main
	DiagFileFieldNotHuman       DiagCode = "C129" // `file` schema field on the output of a node that never pauses for an operator (error — no LLM can produce a binary)
	DiagReservedAnswerKey       DiagCode = "C130" // human output schema declares an engine-reserved answer key (error — the engine overwrites it on resume)
	// Var enum constraints (`name: string [enum: "a", "b"] = "a"`).
	DiagVarEnumNonString    DiagCode = "C125" // enum constraint on a non-string var type (error)
	DiagVarDefaultNotInEnum DiagCode = "C126" // var default value not in the enum list (error)
	DiagVarEnumDuplicate    DiagCode = "C127" // duplicate enum values in a var constraint (warning; deduped)
	// Event-driven primitives (ADR-051): emit/wait nodes.
	DiagEventNoName     DiagCode = "C196" // emit/wait node with no `event:` name (error)
	DiagWaitNoTimeout   DiagCode = "C197" // wait node with no `timeout:` (error — the no-silent-infinity invariant)
	DiagEventNoListener DiagCode = "C198" // wait on an event no emit produces, or emit no wait consumes (warning — dangling event)
	// Skill library (ADR-059): `skills:` references on nodes / workflow.
	DiagInvalidSkillRef DiagCode = "C199" // malformed skill-library reference name (warning; existence is resolved at run time)
	// Async human interaction (ADR-081): interaction: async + await_answers
	// nodes. C240 band — C200–C230 are claimed by pkg/bundlelint's manifest
	// lint codes (same Cnnn namespace, guarded by TestDiagCodesAreUnique).
	DiagAsyncOnHuman          DiagCode = "C240" // interaction: async on a human node — only agent/judge can post async questions (error)
	DiagAwaitAnswersNoTimeout DiagCode = "C241" // await_answers node with no `timeout:` (error — the no-silent-infinity invariant)
	DiagAwaitAnswersBadFrom   DiagCode = "C242" // await_answers `from:` names a node that is missing or not interaction: async (warning — it can only ever time out)
	DiagPersistInFanOut       DiagCode = "C243" // session: persist on a node inside a fan_out_all / fan_out_each / llm-multi body (error — v1 is trunk-only)
	// Parallel-branch bodies (fan_out_all / fan_out_each / llm multi) run
	// through execBranch, which has no local loop counters. C244 refuses a
	// bounded-iteration edge (loop or foreach) whose source sits in that
	// body; the runtime skip of IsBoundedIteration() is defence in depth.
	DiagLoopInExecBranch DiagCode = "C244"
)
