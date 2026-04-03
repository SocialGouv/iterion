package ir

import "fmt"

// ---------------------------------------------------------------------------
// Additional diagnostic codes for static validation (P2-02)
// ---------------------------------------------------------------------------

const (
	DiagInheritAfterJoin       DiagCode = "C009" // session: inherit on node immediately after join
	DiagMultipleDefaultEdges   DiagCode = "C010" // multiple unconditional edges from non-fan_out_all source
	DiagAmbiguousCondition     DiagCode = "C011" // ambiguous conditional edges from same source
	DiagMissingFallback        DiagCode = "C012" // conditional edges with no default fallback
	DiagConditionNotBool       DiagCode = "C013" // when field is not boolean in output schema
	DiagConditionFieldNotFound DiagCode = "C014" // when field not found in source output schema
	DiagJoinRequireUnknown     DiagCode = "C015" // join require references unknown node
	DiagUnreachableNode        DiagCode = "C016" // node unreachable from entry
	DiagHistoryRefNotInLoop    DiagCode = "C017" // outputs.<node>.history but node not in a loop
	DiagUndeclaredCycle        DiagCode = "C019" // cycle without a declared loop (infinite loop risk)
	DiagRoundRobinTooFewEdges  DiagCode = "C020" // round_robin router with fewer than 2 outgoing edges
	DiagLLMRouterTooFewEdges   DiagCode = "C021" // llm router with fewer than 2 outgoing edges
	DiagLLMRouterConditionEdge DiagCode = "C022" // llm router edge has a 'when' condition
	DiagRouterLLMOnlyProperty  DiagCode = "C023" // LLM-only property on non-llm router
	DiagInvalidReasoningEffort DiagCode = "C024" // invalid reasoning_effort value
	DiagInvalidLoopIterations  DiagCode = "C026" // loop max_iterations must be >= 1
)

// validate performs static validation on a compiled workflow.
// It is called after all nodes, edges, loops and schemas are compiled.
func (c *compiler) validate(w *Workflow) {
	if w == nil {
		return
	}

	c.validateInheritAfterJoin(w)
	c.validateEdgeRouting(w)
	c.validateRoundRobinEdges(w)
	c.validateLLMRouterEdges(w)
	c.validateConditionFields(w)
	c.validateJoinRequire(w)
	c.validateReachability(w)
	c.validateHistoryRefs(w)
	c.validateUndeclaredCycles(w)
	c.validateLoopIterations(w)
	c.validateReasoningEffort(w)
}

// ---------------------------------------------------------------------------
// C009 — session: inherit forbidden immediately after a join
// ---------------------------------------------------------------------------

func (c *compiler) validateInheritAfterJoin(w *Workflow) {
	// Build set of nodes that are direct targets of a join node.
	afterJoin := make(map[string]string) // target node -> join node ID
	for _, e := range w.Edges {
		src, ok := w.Nodes[e.From]
		if !ok {
			continue
		}
		if src.Kind == NodeJoin {
			afterJoin[e.To] = e.From
		}
	}

	for targetID, joinID := range afterJoin {
		target, ok := w.Nodes[targetID]
		if !ok {
			continue
		}
		if target.Session == SessionInherit {
			c.errorf(DiagInheritAfterJoin,
				"node %q has session: inherit but follows join %q; only fresh or artifacts_only are allowed after a join",
				targetID, joinID)
		}
	}
}

// ---------------------------------------------------------------------------
// C010, C011, C012 — edge routing validation
// ---------------------------------------------------------------------------

func (c *compiler) validateEdgeRouting(w *Workflow) {
	// Group outgoing edges by source node.
	type edgeGroup struct {
		unconditional []*Edge
		conditional   []*Edge
	}
	groups := make(map[string]*edgeGroup)
	for _, e := range w.Edges {
		g, ok := groups[e.From]
		if !ok {
			g = &edgeGroup{}
			groups[e.From] = g
		}
		if e.Condition == "" {
			g.unconditional = append(g.unconditional, e)
		} else {
			g.conditional = append(g.conditional, e)
		}
	}

	for nodeID, g := range groups {
		node, ok := w.Nodes[nodeID]
		if !ok {
			continue
		}

		// Router fan_out_all, round_robin, and llm are allowed multiple unconditional edges.
		if node.Kind == NodeRouter && (node.RouterMode == RouterFanOutAll || node.RouterMode == RouterRoundRobin || node.RouterMode == RouterLLM) {
			continue
		}

		// C010: multiple unconditional edges from a non-fan_out_all node.
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

		// C012: conditional edges but no fallback (unconditional) edge.
		if len(g.unconditional) == 0 {
			// Check if conditions cover true/false exhaustively.
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
		if node.Kind != NodeRouter || node.RouterMode != RouterRoundRobin {
			continue
		}
		count := 0
		for _, e := range w.Edges {
			if e.From == node.ID && e.Condition == "" {
				count++
			}
		}
		if count < 2 {
			c.errorf(DiagRoundRobinTooFewEdges,
				"round_robin router %q has %d unconditional outgoing edge(s); at least 2 are needed for alternation",
				node.ID, count)
		}
	}
}

// ---------------------------------------------------------------------------
// C021, C022 — llm router validation
// ---------------------------------------------------------------------------

func (c *compiler) validateLLMRouterEdges(w *Workflow) {
	for _, node := range w.Nodes {
		if node.Kind != NodeRouter || node.RouterMode != RouterLLM {
			continue
		}
		count := 0
		for _, e := range w.Edges {
			if e.From == node.ID {
				count++
				if e.Condition != "" {
					c.errorf(DiagLLMRouterConditionEdge,
						"llm router %q edge to %q has a 'when' condition; LLM routers select targets directly",
						node.ID, e.To)
				}
			}
		}
		if count < 2 {
			c.errorf(DiagLLMRouterTooFewEdges,
				"llm router %q has %d outgoing edge(s); at least 2 are needed",
				node.ID, count)
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
// on the same source node.
func (c *compiler) checkAmbiguousConditions(nodeID string, edges []*Edge) {
	type condKey struct {
		field   string
		negated bool
	}
	seen := make(map[condKey]*Edge)
	for _, e := range edges {
		key := condKey{field: e.Condition, negated: e.Negated}
		if prev, ok := seen[key]; ok {
			label := e.Condition
			if e.Negated {
				label = "not " + label
			}
			c.errorf(DiagAmbiguousCondition,
				"node %q has ambiguous edges: both %s->%s and %s->%s trigger on %q",
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
		if src.OutputSchema == "" {
			continue
		}

		schema, ok := w.Schemas[src.OutputSchema]
		if !ok {
			continue // already reported by C002
		}

		// Find the field in the schema.
		field := findField(schema, e.Condition)
		if field == nil {
			c.errorf(DiagConditionFieldNotFound,
				"edge %s -> %s: condition field %q not found in output schema %q of node %q",
				e.From, e.To, e.Condition, src.OutputSchema, e.From)
			continue
		}

		// C013: field must be boolean.
		if field.Type != FieldTypeBool {
			c.errorf(DiagConditionNotBool,
				"edge %s -> %s: condition field %q is %s, not bool, in output schema %q",
				e.From, e.To, e.Condition, field.Type, src.OutputSchema)
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
// C015 — join require references unknown node
// ---------------------------------------------------------------------------

func (c *compiler) validateJoinRequire(w *Workflow) {
	for _, node := range w.Nodes {
		if node.Kind != NodeJoin {
			continue
		}
		for _, req := range node.Require {
			if _, ok := w.Nodes[req]; !ok {
				c.errorf(DiagJoinRequireUnknown,
					"join %q requires unknown node %q", node.ID, req)
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
		if node.Kind == NodeDone || node.Kind == NodeFail {
			continue
		}
		c.errorf(DiagUnreachableNode,
			"node %q (%s) is unreachable from entry %q",
			id, node.Kind, w.Entry)
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
		if node.ReasoningEffort == "" {
			continue
		}
		if !ValidReasoningEfforts[node.ReasoningEffort] {
			c.errorf(DiagInvalidReasoningEffort,
				"node %q has invalid reasoning_effort %q; valid values are low, medium, high, extra_high",
				node.ID, node.ReasoningEffort)
		}
	}
}
