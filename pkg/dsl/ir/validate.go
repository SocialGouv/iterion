package ir

import (
	"fmt"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// Additional diagnostic codes for static validation (P2-02)
// ---------------------------------------------------------------------------

const (
	DiagSessionAfterConvergence DiagCode = "C009" // session: inherit or fork on convergence point
	DiagMultipleDefaultEdges    DiagCode = "C010" // multiple unconditional edges from same non-fan_out source
	DiagAmbiguousCondition      DiagCode = "C011" // ambiguous conditional edges from same source
	DiagMissingFallback         DiagCode = "C012" // conditional edges with no default fallback
	DiagConditionNotBool        DiagCode = "C013" // when field is not boolean in output schema
	DiagConditionFieldNotFound  DiagCode = "C014" // when field not found in source output schema
	DiagUnreachableNode         DiagCode = "C016" // node unreachable from entry
	DiagHistoryRefNotInLoop     DiagCode = "C017" // outputs.<node>.history but node not in a loop
	DiagUndeclaredCycle         DiagCode = "C019" // cycle without a declared loop (infinite loop risk)
	DiagRoundRobinTooFewEdges   DiagCode = "C020" // round_robin router with fewer than 2 outgoing edges
	DiagLLMRouterTooFewEdges    DiagCode = "C021" // llm router with fewer than 2 outgoing edges
	DiagLLMRouterConditionEdge  DiagCode = "C022" // llm router edge has a 'when' condition
	DiagRouterLLMOnlyProperty   DiagCode = "C023" // LLM-only property on non-llm router
	DiagInvalidReasoningEffort  DiagCode = "C024" // invalid reasoning_effort value
	DiagInvalidLoopIterations   DiagCode = "C026" // loop max_iterations must be >= 1
	DiagDuplicateWithKey        DiagCode = "C028" // duplicate with-mapping key across edges to same target
	DiagUnknownRefNode          DiagCode = "C030" // outputs ref to non-existent node
	DiagRefFieldNotInSchema     DiagCode = "C031" // outputs ref field not in output schema
	DiagRefNodeNoSchema         DiagCode = "C032" // outputs ref field on node without output schema
	DiagUndeclaredVar           DiagCode = "C033" // vars ref to undeclared variable
	DiagInputFieldNotInSchema   DiagCode = "C034" // input ref field not in input schema
	DiagUnknownArtifact         DiagCode = "C035" // artifacts ref to unpublished artifact
	DiagRefNodeNotReachable     DiagCode = "C036" // outputs ref to node not reachable before consumer
	DiagNodeMaxTokensVsBudget   DiagCode = "C037" // node-level max_tokens exceeds workflow.budget.max_tokens
	DiagUnsupportedMCPAuth      DiagCode = "C038" // MCP server Auth.Type not supported (only "oauth2" is wired)
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
	c.validateConditionFields(w)
	c.validateDuplicateWithKeys(w)
	c.validateReachability(w)
	c.validateHistoryRefs(w)
	c.validateUndeclaredCycles(w)
	c.validateLoopIterations(w)
	c.validateReasoningEffort(w)
	c.validateTemplateRefs(w)
	c.validateNodeMaxTokensVsBudget(w)
	c.validateMCPAuth(w)
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
		case *AgentNode:
			checkLLM(nd.ID, nd.MaxTokens)
		case *JudgeNode:
			checkLLM(nd.ID, nd.MaxTokens)
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
		case *AgentNode:
			awaitMode, session = n.AwaitMode, n.Session
		case *JudgeNode:
			awaitMode, session = n.AwaitMode, n.Session
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
		if session == SessionInherit || session == SessionFork {
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
		case e.LoopName != "":
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
				if e.Condition != "" {
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
		if e.Condition == "" {
			continue
		}

		src, ok := w.Nodes[e.From]
		if !ok {
			continue
		}

		// Only validate if the source node has an output schema.
		outSchema := NodeOutputSchema(src)
		if outSchema == "" {
			continue
		}

		schema, ok := w.Schemas[outSchema]
		if !ok {
			continue // already reported by C002
		}

		// Find the field in the schema.
		field := findField(schema, e.Condition)
		if field == nil {
			c.errorfAt(DiagConditionFieldNotFound, e.From, edgeID(e.From, e.To),
				"edge %s -> %s: condition field %q not found in output schema %q of node %q",
				e.From, e.To, e.Condition, outSchema, e.From)
			continue
		}

		// C013: field must be boolean.
		if field.Type != FieldTypeBool {
			c.errorfAt(DiagConditionNotBool, e.From, edgeID(e.From, e.To),
				"edge %s -> %s: condition field %q is %s, not bool, in output schema %q",
				e.From, e.To, e.Condition, field.Type, outSchema)
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
	if w.Entry == "" {
		return
	}

	// Build set of nodes that participate in a declared loop.
	// A cycle is considered bounded if ANY edge in the cycle carries a
	// LoopName — the runtime enforces max_iterations on that edge.
	loopNodes := make(map[string]bool)
	for _, e := range w.Edges {
		if e.LoopName != "" {
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

	dfs(w.Entry)
}

// ---------------------------------------------------------------------------
// C026 — loop max_iterations must be >= 1
// ---------------------------------------------------------------------------

func (c *compiler) validateLoopIterations(w *Workflow) {
	for _, loop := range w.Loops {
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
var ValidReasoningEfforts = map[string]bool{
	"low":        true,
	"medium":     true,
	"high":       true,
	"extra_high": true,
}

func (c *compiler) validateReasoningEffort(w *Workflow) {
	for _, node := range w.Nodes {
		var effort string
		switch n := node.(type) {
		case *AgentNode:
			effort = n.ReasoningEffort
		case *JudgeNode:
			effort = n.ReasoningEffort
		case *RouterNode:
			effort = n.ReasoningEffort
		default:
			continue
		}
		if effort == "" {
			continue
		}
		if !ValidReasoningEfforts[effort] {
			c.errorf(DiagInvalidReasoningEffort,
				"node %q has invalid reasoning_effort %q; valid values are low, medium, high, extra_high",
				node.NodeID(), effort)
		}
	}
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

	// Tool node command refs.
	for _, n := range w.Nodes {
		if t, ok := n.(*ToolNode); ok {
			for _, ref := range t.CommandRefs {
				out = append(out, refContext{
					Ref:      ref,
					NodeID:   t.ID,
					Location: fmt.Sprintf("tool node %q command", t.ID),
				})
			}
		}
	}

	return out
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

	// Skip underscore-prefixed fields — these are runtime-injected internal
	// fields (e.g. _session_id) not declared in output schemas.
	if len(fieldName) > 0 && fieldName[0] == '_' {
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
