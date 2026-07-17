package botimport

import (
	"fmt"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
)

// ---- output model ----

type varDecl struct {
	name    string
	comment string
}

type schemaOut struct {
	name   string
	fields []schemaField
}

type promptOut struct {
	name string
	text string
}

type nodeOut struct {
	kind     string // "agent" | "router" | "tool"
	id       string
	comments []string
	// agent
	userPrompt string
	output     string
	model      string
	effort     string
	awaitAll   bool
	// router
	routerMode string
	over       string
	alias      string
	// tool
	command string
}

type edgeOut struct {
	comments      []string
	src, dst      string
	when          string
	whenBare      bool // when is a bare field / `not field`, not a quoted expr
	isElse        bool
	loopName      string
	loopN         int
	loopUnbounded bool
	loopFuel      int
}

type model struct {
	WorkflowName   string
	Description    string
	HeaderComments []string
	Vars           []varDecl
	Schemas        []schemaOut
	Prompts        []promptOut
	Nodes          []*nodeOut
	Edges          []edgeOut
	Entry          string
	Report         *Report
}

// pendingEdge is the dangling forward edge out of the last lowered
// node, waiting for its destination.
type pendingEdge struct {
	src      string
	when     string
	whenBare bool
	isElse   bool
	comments []string
}

// fanCtx resolves stage-parameter references inside a fan-out body.
type fanCtx struct {
	param    string
	routerID string
	alias    string
}

type lowerer struct {
	s     *script
	m     *model
	rep   *Report
	nodes map[string]bool   // used node ids
	vars  map[string]string // var name → comment ("" = declared, no comment)
	// resultVar → node id, for {{outputs.…}} refs and conditions.
	resultNode map[string]string
	// node id → output schema (for condition translatability).
	nodeSchema map[string]string

	pending    *pendingEdge
	comments   []string // pending ## comments for the next node/edge
	terminated bool     // a top-level return was emitted

	holeN, agentN, fanN, loopN, joinN int
}

func lower(s *script) *model {
	lo := &lowerer{
		s:          s,
		rep:        s.report,
		nodes:      map[string]bool{},
		vars:       map[string]string{},
		resultNode: map[string]string{},
		nodeSchema: map[string]string{},
		m: &model{
			Report:       s.report,
			WorkflowName: sanitizeIdent(s.stem),
		},
	}
	lo.lowerSteps(s.steps, nil)

	// Close the graph: whatever is still dangling flows to done.
	if lo.pending != nil {
		lo.emitEdge(lo.pending, "done")
		lo.pending = nil
	}
	if len(lo.comments) > 0 {
		lo.m.HeaderComments = append(lo.m.HeaderComments, lo.comments...)
		lo.comments = nil
	}
	return lo.m
}

func (lo *lowerer) lowerSteps(steps []*step, fan *fanCtx) {
	for _, st := range steps {
		switch st.kind {
		case stepMeta:
			lo.lowerMeta(st.meta)
		case stepSchema:
			lo.lowerSchema(st.schema)
		case stepAgent:
			lo.lowerAgent(st.agent, fan)
		case stepFanOut:
			lo.lowerFanOut(st.fan)
		case stepPhase:
			lo.comments = append(lo.comments, "## Phase: "+st.text)
		case stepLog:
			if st.text != "" {
				lo.comments = append(lo.comments, "## log: "+st.text)
			}
		case stepCondExit:
			lo.lowerCondExit(st.cond)
		case stepLoop:
			lo.lowerLoop(st.loop)
		case stepReturn:
			lo.lowerReturn()
		case stepUnknown:
			lo.comments = append(lo.comments, fmt.Sprintf("## IMPORT (js:%d): %s", st.line, st.text))
		}
	}
}

func (lo *lowerer) lowerMeta(mi *metaInfo) {
	if mi.name != "" {
		lo.m.WorkflowName = sanitizeIdent(mi.name)
	}
	lo.m.Description = mi.description
	for _, ph := range mi.phases {
		c := "## phase: " + ph.title
		if ph.detail != "" {
			c += " — " + ph.detail
		}
		lo.m.HeaderComments = append(lo.m.HeaderComments, c)
	}
	lo.rep.mapped(0, "meta → workflow `%s`", lo.m.WorkflowName)
}

func (lo *lowerer) lowerSchema(si *schemaInfo) string {
	name := sanitizeIdent(si.constName)
	if name == "unnamed" {
		name = fmt.Sprintf("schema_%d", len(lo.m.Schemas)+1)
	}
	lo.m.Schemas = append(lo.m.Schemas, schemaOut{name: name, fields: si.fields})
	lo.rep.mapped(0, "JSON schema `%s` → schema %s (%d fields)", si.constName, name, len(si.fields))
	return name
}

// schemaOutName finds the DSL name a schema const was lowered to.
func (lo *lowerer) schemaOutName(constName string) string {
	want := sanitizeIdent(constName)
	for _, sc := range lo.m.Schemas {
		if sc.name == want {
			return sc.name
		}
	}
	return ""
}

func (lo *lowerer) lowerAgent(ag *agentInfo, fan *fanCtx) *nodeOut {
	if lo.terminated {
		lo.rep.dropped(ag.line, "agent after a top-level return is unreachable — dropped")
		return nil
	}
	id := lo.nodeID(ag)
	node := &nodeOut{kind: "agent", id: id, model: ag.model, effort: ag.effort}

	// Output schema.
	switch {
	case ag.schemaConst != "":
		if name := lo.schemaOutName(ag.schemaConst); name != "" {
			node.output = name
		} else {
			lo.rep.placeholder(ag.line, "agent %s references schema `%s` not declared above — output dropped", id, ag.schemaConst)
		}
	case ag.inlineSchema != nil:
		ag.inlineSchema.constName = id + "_out"
		node.output = lo.lowerSchema(ag.inlineSchema)
	}
	if node.output != "" {
		lo.nodeSchema[id] = node.output
	}

	// Prompt.
	text := lo.resolvePrompt(ag.prompt, fan)
	pname := id + "_user"
	lo.m.Prompts = append(lo.m.Prompts, promptOut{name: pname, text: text})
	node.userPrompt = pname

	node.comments = lo.takeComments()
	lo.m.Nodes = append(lo.m.Nodes, node)
	if lo.m.Entry == "" {
		lo.m.Entry = id
	}
	if ag.resultVar != "" {
		lo.resultNode[ag.resultVar] = id
	}
	if lo.pending != nil {
		lo.emitEdge(lo.pending, id)
	}
	lo.pending = &pendingEdge{src: id}
	lo.rep.mapped(ag.line, "agent() → agent %s", id)
	return node
}

func (lo *lowerer) nodeID(ag *agentInfo) string {
	base := sanitizeIdent(ag.label)
	if base == "unnamed" || base == "" {
		base = sanitizeIdent(ag.resultVar)
	}
	if base == "unnamed" || base == "" {
		lo.agentN++
		base = fmt.Sprintf("agent_%d", lo.agentN)
	}
	// done/fail are terminal node ids in every workflow.
	if base == "done" || base == "fail" {
		base = base + "_step"
	}
	id := base
	for n := 2; lo.nodes[id]; n++ {
		id = fmt.Sprintf("%s_%d", base, n)
	}
	lo.nodes[id] = true
	return id
}

func (lo *lowerer) takeComments() []string {
	c := lo.comments
	lo.comments = nil
	return c
}

// ---- conditional exits ----

func (lo *lowerer) lowerCondExit(ce *condExitInfo) {
	if lo.pending == nil || lo.pending.src == "" {
		lo.comments = append(lo.comments, fmt.Sprintf("## IMPORT TODO (js:%d): pre-flight guard dropped: if (%s) %s", ce.line, firstLine(ce.condJS), ce.exit))
		lo.rep.placeholder(ce.line, "guard before any agent has no source node: if (%s) → %s", firstLine(ce.condJS), ce.exit)
		return
	}
	src := lo.pending.src
	cond, ok := lo.translateCond(ce.condExpr, src)
	if !ok {
		lo.comments = append(lo.comments, fmt.Sprintf("## IMPORT TODO (js:%d): conditional exit dropped (untranslatable): if (%s) → %s", ce.line, firstLine(ce.condJS), ce.exit))
		lo.rep.placeholder(ce.line, "conditional exit untranslatable, dropped: if (%s) → %s", firstLine(ce.condJS), ce.exit)
		return
	}
	lo.m.Edges = append(lo.m.Edges, edgeOut{
		comments: lo.takeComments(),
		src:      src, dst: ce.exit,
		when: cond.expr, whenBare: cond.bare,
	})
	// The surviving forward path is the explicit fallback.
	lo.pending = &pendingEdge{src: src, isElse: true}
	lo.rep.mapped(ce.line, "if (%s) return → %s -> %s when %s (+ else fallback)", firstLine(ce.condJS), src, ce.exit, cond.expr)
}

func (lo *lowerer) lowerReturn() {
	if lo.pending != nil {
		lo.emitEdge(lo.pending, "done")
		lo.pending = nil
	}
	lo.terminated = true
}

// ---- loops ----

func (lo *lowerer) lowerLoop(li *loopInfo) {
	if lo.terminated {
		lo.rep.dropped(li.line, "loop after a top-level return is unreachable — dropped")
		return
	}
	entryPending := lo.pending
	lo.pending = nil

	// Lower the body linearly, tracking its first/last nodes.
	before := len(lo.m.Nodes)
	lo.lowerSteps(li.body, nil)
	bodyNodes := lo.m.Nodes[before:]
	if len(bodyNodes) == 0 {
		lo.pending = entryPending
		lo.rep.dropped(li.line, "loop body produced no nodes — dropped")
		return
	}
	first, last := bodyNodes[0].id, bodyNodes[len(bodyNodes)-1].id

	if entryPending != nil {
		lo.emitEdge(entryPending, first)
	}

	var breakCond condOut
	translated := false
	if li.breakCond != nil {
		breakCond, translated = lo.translateCond(li.breakCond, last)
	}
	if !translated {
		// No expressible exit condition: a .bot loop would only exit by
		// exhaustion, which FAILS the run (LOOP_EXHAUSTED) — not the JS
		// fall-through. Keep the chain linear, visibly.
		note := "loop not lowered: no translatable break condition (a .bot loop exits by condition; exhaustion fails the run)"
		if li.breakCond != nil {
			note = fmt.Sprintf("loop not lowered: break condition untranslatable: %s", firstLine(li.breakJS))
		}
		lo.comments = append(lo.comments, fmt.Sprintf("## IMPORT TODO (js:%d): %s", li.line, note))
		lo.rep.placeholder(li.line, "%s", note)
		lo.pending = &pendingEdge{src: last}
		return
	}

	lo.loopN++
	name := fmt.Sprintf("loop_%d", lo.loopN)
	back := edgeOut{
		src: last, dst: first,
		when: breakCond.negated, whenBare: breakCond.bare,
		loopName: name,
	}
	if li.bounded {
		back.loopN = li.maxIter
	} else {
		back.loopUnbounded = true
		back.loopFuel = li.fuel
		lo.rep.placeholder(li.line, "while loop → unbounded loop with fuel %d — adjust the ceiling", li.fuel)
	}
	back.comments = lo.takeComments()
	lo.m.Edges = append(lo.m.Edges, back)

	// Loop exit: the back edge repeats while the break condition does
	// NOT hold, so the forward path is exactly its `else` fallback —
	// same semantics, and C012 (conditional needs a default) holds.
	lo.pending = &pendingEdge{src: last, isElse: true}
	n := li.maxIter
	if !li.bounded {
		n = li.fuel
	}
	lo.rep.mapped(li.line, "loop → %s -> %s as %s(%d), exit when %s", last, first, name, n, breakCond.expr)
	lo.rep.placeholder(li.line, "JS loop falls through after %d iterations; the .bot loop FAILS on exhaustion (LOOP_EXHAUSTED) — add an exhaustion path if fall-through was load-bearing", n)
}

// ---- fan-outs ----

func (lo *lowerer) lowerFanOut(fi *fanInfo) {
	if lo.terminated {
		lo.rep.dropped(fi.line, "fan-out after a top-level return is unreachable — dropped")
		return
	}
	lo.fanN++
	routerID := fmt.Sprintf("fan_%d", lo.fanN)
	lo.nodes[routerID] = true
	router := &nodeOut{kind: "router", id: routerID, routerMode: fi.mode, comments: lo.takeComments()}

	var fctx *fanCtx
	if fi.mode == "fan_out_each" {
		alias := sanitizeIdent(fi.param)
		if alias == "unnamed" {
			alias = "item"
		}
		router.alias = alias
		router.over = lo.resolveOver(fi.overExpr, fi.line)
		fctx = &fanCtx{param: fi.param, routerID: routerID, alias: alias}
	}
	lo.m.Nodes = append(lo.m.Nodes, router)
	if lo.m.Entry == "" {
		lo.m.Entry = routerID
	}
	if lo.pending != nil {
		lo.emitEdge(lo.pending, routerID)
	}

	// Convergence: everything joins on a deterministic no-op tool.
	lo.joinN++
	joinID := fmt.Sprintf("join_%d", lo.joinN)
	lo.nodes[joinID] = true
	join := &nodeOut{kind: "tool", id: joinID, command: "true", awaitAll: true}

	switch fi.mode {
	case "fan_out_each":
		// Per-item stage chain: router -> s1 -> s2 -> … -> join.
		prev := routerID
		for _, ag := range fi.agents {
			lo.pending = &pendingEdge{src: prev}
			node := lo.lowerAgent(ag, fctx)
			if node == nil {
				continue
			}
			prev = node.id
		}
		lo.pending = &pendingEdge{src: prev}
		lo.m.Nodes = append(lo.m.Nodes, join)
		lo.emitEdge(lo.pending, joinID)
	case "fan_out_all":
		// Independent branches: router -> each agent -> join.
		lo.m.Nodes = append(lo.m.Nodes, join)
		for _, ag := range fi.agents {
			lo.pending = &pendingEdge{src: routerID}
			node := lo.lowerAgent(ag, nil)
			if node == nil {
				continue
			}
			lo.pending = &pendingEdge{src: node.id}
			lo.emitEdge(lo.pending, joinID)
		}
	}
	lo.pending = &pendingEdge{src: joinID}
	lo.rep.mapped(fi.line, "%s over %d agent stage(s) → router %s (+ %s convergence, await wait_all)", fi.mode, len(fi.agents), routerID, joinID)
	if fi.resultVar != "" {
		lo.rep.placeholder(fi.line, "fan-out results bound to `%s` are not aggregated — downstream nodes read {{outputs.<stage>}} per branch", fi.resultVar)
	}
}

// resolveOver maps the fan-out items expression to a template ref.
func (lo *lowerer) resolveOver(e ast.Expression, line int) string {
	if e == nil {
		return lo.holeRef(nil, line, "fan-out items")
	}
	switch n := e.(type) {
	case *ast.Identifier:
		name := n.Name.String()
		if nodeID, ok := lo.resultNode[name]; ok {
			return "{{outputs." + nodeID + "}}"
		}
		if b := lo.s.bindings[name]; b != nil && b.kind == bindArgDerived {
			return "{{vars." + lo.ensureVar(name, "items for the fan-out (from args)") + "}}"
		}
		return lo.holeRef(e, line, "fan-out items `"+name+"`")
	case *ast.DotExpression:
		if base, ok := n.Left.(*ast.Identifier); ok {
			if nodeID, ok := lo.resultNode[base.Name.String()]; ok {
				return "{{outputs." + nodeID + "." + n.Identifier.Name.String() + "}}"
			}
		}
	}
	return lo.holeRef(e, line, "fan-out items")
}

// ---- prompt resolution ----

func (lo *lowerer) resolvePrompt(parts []promptPart, fan *fanCtx) string {
	var b strings.Builder
	for _, p := range parts {
		if p.expr == nil {
			b.WriteString(p.text)
			continue
		}
		b.WriteString(lo.resolveHole(p.expr, p.line, fan))
	}
	return strings.TrimSpace(b.String())
}

// resolveHole maps one JS expression inside a prompt to a template
// reference, inlined static text, or a promoted hole var.
func (lo *lowerer) resolveHole(e ast.Expression, line int, fan *fanCtx) string {
	switch n := e.(type) {
	case *ast.Identifier:
		name := n.Name.String()
		if fan != nil && name == fan.param {
			return "{{outputs." + fan.routerID + "." + fan.alias + "}}"
		}
		if b := lo.s.bindings[name]; b != nil {
			switch b.kind {
			case bindStaticString:
				return b.str
			case bindNumber:
				return fmt.Sprintf("%d", b.num)
			case bindArgDerived:
				return "{{vars." + lo.ensureVar(name, "input derived from args in the source script") + "}}"
			case bindAgent:
				if nodeID, ok := lo.resultNode[name]; ok {
					return "{{outputs." + nodeID + "}}"
				}
			}
		}
		return lo.holeRef(e, line, "`"+name+"`")
	case *ast.DotExpression:
		path := dotPath(n)
		if len(path) >= 2 {
			if fan != nil && path[0] == fan.param {
				return "{{outputs." + fan.routerID + "." + fan.alias + "." + strings.Join(path[1:], ".") + "}}"
			}
			if nodeID, ok := lo.resultNode[path[0]]; ok {
				return "{{outputs." + nodeID + "." + strings.Join(path[1:], ".") + "}}"
			}
		}
		return lo.holeRef(e, line, "`"+strings.Join(path, ".")+"`")
	default:
		return lo.holeRef(e, line, "dynamic fragment")
	}
}

// holeRef promotes an unresolvable expression to a var the operator
// must fill, and records it.
func (lo *lowerer) holeRef(e ast.Expression, line int, what string) string {
	lo.holeN++
	name := fmt.Sprintf("hole_%d", lo.holeN)
	js := ""
	if e != nil {
		js = firstLine(lo.s.snippet(e.Idx0(), e.Idx1()))
	}
	comment := "IMPORT hole: " + what
	if js != "" {
		comment += " — JS: " + js
	}
	lo.vars[name] = comment
	lo.m.Vars = append(lo.m.Vars, varDecl{name: name, comment: comment})
	lo.rep.hole(line, "%s → var %s (fill before running)", what, name)
	return "{{vars." + name + "}}"
}

// ensureVar declares a named workflow var once and returns its name.
func (lo *lowerer) ensureVar(jsName, comment string) string {
	name := sanitizeIdent(jsName)
	if _, ok := lo.vars[name]; !ok {
		lo.vars[name] = comment
		lo.m.Vars = append(lo.m.Vars, varDecl{name: name, comment: comment})
	}
	return name
}

// ---- condition translation ----

type condOut struct {
	expr    string // DSL condition (bare field form or expr string)
	negated string
	bare    bool // use `when field` / `when not field` (no quotes)
}

// translateCond lowers a JS test to a `when` condition on srcNode's
// output. Only shapes that reference the node's own result var are
// expressible; anything else returns ok=false (the caller drops the
// edge VISIBLY rather than guessing).
func (lo *lowerer) translateCond(e ast.Expression, srcNode string) (condOut, bool) {
	// The node must produce structured output for field refs to mean
	// anything at run time.
	if lo.nodeSchema[srcNode] == "" {
		return condOut{}, false
	}
	resultVar := ""
	for v, id := range lo.resultNode {
		if id == srcNode {
			resultVar = v
			break
		}
	}
	if resultVar == "" {
		return condOut{}, false
	}
	return lo.translateCondExpr(e, resultVar)
}

func (lo *lowerer) translateCondExpr(e ast.Expression, resultVar string) (condOut, bool) {
	switch n := e.(type) {
	case *ast.BinaryExpression:
		switch n.Operator {
		case token.LOGICAL_OR:
			// `!x || x.f !== 'v'` — the null-guard is redundant once the
			// output is schema-validated; translate the meaningful side.
			if isNullGuard(n.Left, resultVar, true) {
				return lo.translateCondExpr(n.Right, resultVar)
			}
		case token.LOGICAL_AND:
			// `x && x.f === 'v'` — same, positive guard.
			if isNullGuard(n.Left, resultVar, false) {
				return lo.translateCondExpr(n.Right, resultVar)
			}
		case token.STRICT_EQUAL, token.EQUAL, token.STRICT_NOT_EQUAL, token.NOT_EQUAL,
			token.LESS, token.LESS_OR_EQUAL, token.GREATER, token.GREATER_OR_EQUAL:
			field, ok := fieldOn(n.Left, resultVar)
			if !ok {
				return condOut{}, false
			}
			rhs, ok := condLiteral(n.Right)
			if !ok {
				return condOut{}, false
			}
			op, neg := condOp(n.Operator)
			return condOut{
				expr:    fmt.Sprintf("%s %s %s", field, op, rhs),
				negated: fmt.Sprintf("%s %s %s", field, neg, rhs),
			}, true
		}
	case *ast.UnaryExpression:
		if n.Operator == token.NOT {
			if field, ok := fieldOn(n.Operand, resultVar); ok {
				return condOut{expr: "not " + field, negated: field, bare: true}, true
			}
		}
	case *ast.DotExpression:
		if field, ok := fieldOn(n, resultVar); ok {
			return condOut{expr: field, negated: "not " + field, bare: true}, true
		}
	}
	return condOut{}, false
}

// isNullGuard matches `!x` (negated=true) or `x` (negated=false) for
// the given result var.
func isNullGuard(e ast.Expression, resultVar string, negated bool) bool {
	if negated {
		un, ok := e.(*ast.UnaryExpression)
		if !ok || un.Operator != token.NOT {
			return false
		}
		e = un.Operand
	}
	id, ok := e.(*ast.Identifier)
	return ok && id.Name.String() == resultVar
}

// fieldOn extracts `resultVar.field[.sub…]` as a dotted field path.
func fieldOn(e ast.Expression, resultVar string) (string, bool) {
	dot, ok := e.(*ast.DotExpression)
	if !ok {
		return "", false
	}
	path := dotPath(dot)
	if len(path) < 2 || path[0] != resultVar {
		return "", false
	}
	return strings.Join(path[1:], "."), true
}

func dotPath(e ast.Expression) []string {
	switch n := e.(type) {
	case *ast.Identifier:
		return []string{n.Name.String()}
	case *ast.DotExpression:
		left := dotPath(n.Left)
		if left == nil {
			return nil
		}
		return append(left, n.Identifier.Name.String())
	}
	return nil
}

func condOp(t token.Token) (op, negated string) {
	switch t {
	case token.STRICT_EQUAL, token.EQUAL:
		return "==", "!="
	case token.STRICT_NOT_EQUAL, token.NOT_EQUAL:
		return "!=", "=="
	case token.LESS:
		return "<", ">="
	case token.LESS_OR_EQUAL:
		return "<=", ">"
	case token.GREATER:
		return ">", "<="
	case token.GREATER_OR_EQUAL:
		return ">=", "<"
	}
	return "", ""
}

// condLiteral renders a JS literal as a DSL expr literal.
func condLiteral(e ast.Expression) (string, bool) {
	switch n := e.(type) {
	case *ast.StringLiteral:
		v := n.Value.String()
		if strings.ContainsAny(v, "'\n") {
			return "", false
		}
		return "'" + v + "'", true
	case *ast.NumberLiteral:
		return strings.TrimSpace(n.Literal), true
	case *ast.BooleanLiteral:
		return n.Literal, true
	}
	return "", false
}

// ---- edges ----

func (lo *lowerer) emitEdge(p *pendingEdge, dst string) {
	lo.m.Edges = append(lo.m.Edges, edgeOut{
		comments: append(p.comments, lo.takeComments()...),
		src:      p.src, dst: dst,
		when: p.when, whenBare: p.whenBare, isElse: p.isElse,
	})
}
