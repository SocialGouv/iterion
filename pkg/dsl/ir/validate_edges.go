package ir

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
				"node %q has session: %s but has await: %s (convergence point); only fresh, artifacts_only, or persist are allowed",
				nodeID, session, awaitMode)
		}
	}
}

// validatePersistNotInFanOut refuses session: persist on any node that
// can run inside execBranch (ADR-089 v1 is trunk-only). The join itself
// (await != none) is not in the body.
func (c *compiler) validatePersistNotInFanOut(w *Workflow) {
	body := fanOutBodyNodes(w)
	for id, node := range w.Nodes {
		llm, ok := node.(LLMNode)
		if !ok || llm.GetSession() != SessionPersist || !body[id] {
			continue
		}
		c.errorf(DiagPersistInFanOut,
			"node %q has session: persist but sits in a fan_out_all/fan_out_each body; persist is trunk-only in v1 (C243)",
			id)
	}
}

// fanOutBodyNodes is the set of nodes reachable from a fan_out_all or
// fan_out_each router along outgoing edges, stopping at a join. A join
// is an explicit `await:` OR a node with multiple distinct incoming
// sources — the same predicate the runtime's findConvergencePoint uses,
// so persist on the trunk after an implicit merge is not C243.
func fanOutBodyNodes(w *Workflow) map[string]bool {
	out := map[string][]string{}
	inSources := map[string]map[string]bool{}
	for _, e := range w.Edges {
		out[e.From] = append(out[e.From], e.To)
		if e.LoopName != "" {
			// A back-edge is a cycle, not a fan-out join.
			continue
		}
		if inSources[e.To] == nil {
			inSources[e.To] = map[string]bool{}
		}
		inSources[e.To][e.From] = true
	}
	body := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		n, ok := w.Nodes[id]
		if !ok || body[id] {
			return
		}
		if NodeAwaitMode(n) != AwaitNone || len(inSources[id]) > 1 {
			return
		}
		body[id] = true
		for _, next := range out[id] {
			walk(next)
		}
	}
	for id, node := range w.Nodes {
		r, ok := node.(*RouterNode)
		if !ok {
			continue
		}
		if r.RouterMode != RouterFanOutAll && r.RouterMode != RouterFanOutEach {
			continue
		}
		for _, next := range out[id] {
			walk(next)
		}
	}
	return body
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
		elseEdges     []*Edge
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
		case e.IsElse:
			// Explicit fallback sugar: same runtime role as a bare
			// unconditional next to conditional siblings, but validated
			// with its own stricter contract (C015/C039/C040 below).
			g.elseEdges = append(g.elseEdges, e)
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

		// C039: at most one `else` per source — two fallbacks firing on
		// the same miss would be the C010 ambiguity under a new name.
		if len(g.elseEdges) > 1 {
			targets := make([]string, len(g.elseEdges))
			for i, e := range g.elseEdges {
				targets[i] = e.To
			}
			c.errorf(DiagMultipleElseEdges,
				"node %q has %d `else` edges (targets: %v); only one else fallback is allowed",
				nodeID, len(g.elseEdges), targets)
		}
		// C040: `else` REPLACES the bare unconditional fallback — having
		// both is two competing defaults, pick one form.
		if len(g.elseEdges) > 0 && len(g.unconditional) > 0 {
			c.errorf(DiagElseWithUnconditional,
				"node %q has both an `else` edge (-> %s) and an unconditional edge (-> %s); `else` IS the fallback — remove one",
				nodeID, g.elseEdges[0].To, g.unconditional[0].To)
		}
		// C015: a stray `else` with no conditional sibling can never
		// mean anything — it would just be an unconditional edge wearing
		// a misleading keyword.
		if len(g.elseEdges) > 0 && len(g.conditional) == 0 {
			c.errorf(DiagElseWithoutConditional,
				"node %q has an `else` edge (-> %s) but no conditional (`when`) sibling; use a plain edge",
				nodeID, g.elseEdges[0].To)
		}

		// Only validate conditions for nodes that have conditional edges.
		if len(g.conditional) == 0 {
			continue
		}

		// C012: conditional edges but no fallback. A loop-bearing edge
		// counts as a fallback for this purpose, and so does an `else`
		// edge (it is the explicit fallback form).
		if len(g.unconditional) == 0 && len(g.loopBearing) == 0 && len(g.elseEdges) == 0 {
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

// validateBoundedIterationInExecBranch refuses loop/foreach edges whose
// source sits in a subgraph executed by execBranch (C244). Those branches
// share no local loop counters; the branch edge selector skips
// IsBoundedIteration() edges, so a declared loop would compile and then
// silently not run (and a foreach would be taken as an unguarded
// unconditional back-edge). A loop whose source is the fan-out / llm-multi
// router itself is the same class: launchBranches never applies loop
// bookkeeping to those outgoing edges. A loop that wraps the fan-out from
// the join is on the trunk and is allowed.
func (c *compiler) validateBoundedIterationInExecBranch(w *Workflow) {
	body := execBranchBodyNodes(w)
	for _, e := range w.Edges {
		if !e.IsBoundedIteration() {
			continue
		}
		if !body[e.From] && !edgeFromExecBranchRouter(w, e) {
			continue
		}
		kind, name := "loop", e.LoopName
		if e.ForeachName != "" {
			kind, name = "foreach", e.ForeachName
		}
		c.errorfAt(DiagLoopInExecBranch, e.From, edgeID(e.From, e.To),
			"edge %s -> %s is a %s edge (%s) inside a fan_out_all/fan_out_each/llm-multi body; branches have no local loop counters (C244)",
			e.From, e.To, kind, name)
	}
}

func edgeFromExecBranchRouter(w *Workflow, e *Edge) bool {
	r, ok := w.Nodes[e.From].(*RouterNode)
	return ok && routerSpawnsExecBranch(r)
}

// execBranchBodyNodes is the set of nodes a parallel branch runs before
// it hits a join. For each fan_out_all / fan_out_each / llm-multi router
// we elect the same convergence findConvergencePoint would (every
// incoming edge, loop back-edges included) AND we also stop at any
// structural join: await != none, or >1 non-iteration predecessors.
//
// The election stop keeps C244 from rejecting a loop head the runtime
// hoists to the trunk (the back-edge makes that node the elected join).
// The structural stop keeps a sibling branch from walking past the
// intended join when election picked an earlier two-source node (evolve:
// review_claude is targeted by both the topology router and the fan-out,
// so it is elected, and the gpt branch would otherwise swallow the rest
// of the bot). Loops after a non-elected await are a remaining
// execBranch hole, not claimed by C244.
func execBranchBodyNodes(w *Workflow) map[string]bool {
	out := map[string][]string{}
	nonIterIn := map[string]map[string]bool{}
	for _, e := range w.Edges {
		out[e.From] = append(out[e.From], e.To)
		if e.IsBoundedIteration() {
			continue
		}
		if nonIterIn[e.To] == nil {
			nonIterIn[e.To] = map[string]bool{}
		}
		nonIterIn[e.To][e.From] = true
	}
	isStructuralJoin := func(id string) bool {
		n, ok := w.Nodes[id]
		if !ok {
			return false
		}
		if NodeAwaitMode(n) != AwaitNone {
			return true
		}
		return len(nonIterIn[id]) > 1
	}
	body := map[string]bool{}
	var walk func(id, elected string)
	walk = func(id, elected string) {
		if id == "" || id == elected || body[id] || isStructuralJoin(id) {
			return
		}
		if _, ok := w.Nodes[id]; !ok {
			return
		}
		body[id] = true
		for _, next := range out[id] {
			walk(next, elected)
		}
	}
	for _, node := range w.Nodes {
		r, ok := node.(*RouterNode)
		if !ok || !routerSpawnsExecBranch(r) {
			continue
		}
		var fan []*Edge
		for _, e := range w.Edges {
			if e.From == r.ID {
				fan = append(fan, e)
			}
		}
		elected := compileFindConvergence(w, fan)
		for _, e := range fan {
			walk(e.To, elected)
		}
	}
	return body
}

// compileFindConvergence mirrors pkg/runtime.findConvergencePoint: BFS from
// each fan edge in declaration order; first node with await != none OR
// more than one distinct incoming source (loop back-edges included) wins.
func compileFindConvergence(w *Workflow, fan []*Edge) string {
	inSources := map[string]map[string]bool{}
	for _, e := range w.Edges {
		if inSources[e.To] == nil {
			inSources[e.To] = map[string]bool{}
		}
		inSources[e.To][e.From] = true
	}
	maxVisits := len(w.Nodes) + 1
	for _, start := range fan {
		visited := map[string]bool{}
		queue := []string{start.To}
		for len(queue) > 0 {
			if len(visited) > maxVisits {
				break
			}
			id := queue[0]
			queue = queue[1:]
			if visited[id] {
				continue
			}
			visited[id] = true
			n, ok := w.Nodes[id]
			if !ok {
				continue
			}
			if NodeAwaitMode(n) != AwaitNone || len(inSources[id]) > 1 {
				return id
			}
			for _, e := range w.Edges {
				if e.From == id {
					queue = append(queue, e.To)
				}
			}
		}
	}
	return ""
}

func routerSpawnsExecBranch(r *RouterNode) bool {
	switch r.RouterMode {
	case RouterFanOutAll, RouterFanOutEach:
		return true
	case RouterLLM:
		return r.RouterMulti
	default:
		return false
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
		schema := w.Schemas[outSchema]

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
