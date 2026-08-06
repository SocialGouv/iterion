package ir

// This file holds the static-validation entry point and orchestration.
// The individual validation groups live in sibling files:
//   - diagnostics_codes.go      — the DiagCode constant block
//   - validate_edges.go         — convergence, edge routing, conditions, with-keys
//   - validate_reachability.go  — reachability, cycles, loop bounds
//   - validate_nodes.go         — per-node/config checks + reasoning effort
//   - validate_refs.go          — template reference + secrets validation

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
	c.validateNodeTimeout(w)
	c.validateSecrets(w)
	c.validateTemplateRefs(w)
	c.validateNodeMaxTokensVsBudget(w)
	c.validateMCPAuth(w)
	c.validateCompaction(w)
	c.validateMemory(w)
	c.validatePlaywrightMCP(w)
	c.validateCapabilities(w)
	c.validateSkillRefs(w)
	c.validateProviders(w)
	c.validateCommand(w)
	c.validateFallbacks(w)
	c.validateCursorInvocations(w)
	c.validateReviewGates(w)
	c.validateCompress(w)
	c.validateAutoMemory(w)
	c.validateResources(w)
	c.validatePermission(w)
	c.validateVerifiedActions(w)
	c.validateArtifactLabels(w)
	c.validateFileFields(w)
	c.validateReservedAnswerKeys(w)
	c.validateEvents(w)
	c.validateAwaitAnswers(w)
	c.validateSandboxOptOut(w)
}
