package botimport

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// wrapperName is the synthetic async function the source is wrapped in
// so top-level `await` and `return` parse (goja has no module goal).
const wrapperName = "__iterion_import__"

type stepKind int

const (
	stepMeta stepKind = iota
	stepSchema
	stepAgent
	stepFanOut
	stepPhase
	stepLog
	stepCondExit
	stepLoop
	stepReturn
	stepUnknown
)

// step is one recognized top-level construct, in source order.
type step struct {
	kind stepKind
	line int // 1-based line in the ORIGINAL .js

	meta   *metaInfo
	schema *schemaInfo
	agent  *agentInfo
	fan    *fanInfo
	cond   *condExitInfo
	loop   *loopInfo
	text   string // phase title / log text / unknown snippet
}

type metaInfo struct {
	name        string
	description string
	phases      []phaseMeta
}

type phaseMeta struct{ title, detail string }

type schemaInfo struct {
	constName string
	fields    []schemaField
}

type schemaField struct {
	name string
	typ  string // DSL type: string|bool|int|float|json|string[]
	desc string
	enum []string
}

type agentInfo struct {
	resultVar    string
	prompt       []promptPart
	label        string
	schemaConst  string      // opts.schema referenced a const
	inlineSchema *schemaInfo // opts.schema was an inline object literal
	phase        string
	model        string
	effort       string
	line         int
}

// promptPart is either literal text or a JS expression hole, resolved
// during lowering (static const → inlined; agent result → outputs ref;
// args-derived → vars ref; anything else → promoted hole var).
type promptPart struct {
	text string
	expr ast.Expression // nil for text parts
	line int
}

type fanInfo struct {
	// mode: fan_out_each (Promise.all(items.map(...)), pipeline) or
	// fan_out_all (parallel([...thunks])).
	mode      string
	overExpr  ast.Expression // items expression (fan_out_each only)
	param     string         // map/stage parameter name
	agents    []*agentInfo   // per-item stage chain, or per-thunk branches
	resultVar string
	line      int
}

type condExitInfo struct {
	condExpr ast.Expression
	condJS   string
	exit     string // "done" | "fail"
	line     int
}

type loopInfo struct {
	bounded   bool
	maxIter   int
	fuel      int // unbounded loops: fuel ceiling
	body      []*step
	breakCond ast.Expression // condition that exits the loop (nil = none found)
	breakJS   string
	line      int
}

type bindingKind int

const (
	bindStaticString bindingKind = iota
	bindNumber
	bindArgDerived // initializer references the `args` global
	bindAgent      // result of an agent() call
	bindSchema     // JSON-schema object literal
	bindLocal      // let/mutable or dynamic — a hole when referenced
)

type binding struct {
	kind bindingKind
	str  string
	num  int
}

type script struct {
	filename string
	stem     string
	src      string // wrapped source, for snippet slicing
	prog     *ast.Program
	steps    []*step
	bindings map[string]*binding
	report   *Report
}

// parseScript preprocesses (strip `export `, wrap in an async
// function), parses with the goja AST parser (never executes), and
// walks the top level into steps.
func parseScript(filename string, src []byte) (*script, error) {
	original := string(src)
	// Position-preserving strip: `export ` → 7 spaces wherever a line
	// starts with it (goja has no module goal symbol).
	stripped := stripExport(original)
	wrapped := "async function " + wrapperName + "() {\n" + stripped + "\n}"

	prog, err := parser.ParseFile(nil, filename, wrapped, 0)
	if err != nil {
		return nil, fmt.Errorf("javascript parse error: %w", err)
	}
	body, err := unwrapBody(prog)
	if err != nil {
		return nil, err
	}

	s := &script{
		filename: filename,
		stem:     strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename)),
		src:      wrapped,
		prog:     prog,
		bindings: map[string]*binding{},
		report:   &Report{SourceFile: filename},
	}
	s.steps = s.walkStatements(body, false)
	return s, nil
}

func stripExport(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(trimmed, "export ") {
			at := strings.Index(l, "export ")
			lines[i] = l[:at] + "       " + l[at+len("export "):]
		}
	}
	return strings.Join(lines, "\n")
}

func unwrapBody(prog *ast.Program) ([]ast.Statement, error) {
	for _, st := range prog.Body {
		fd, ok := st.(*ast.FunctionDeclaration)
		if !ok || fd.Function == nil || fd.Function.Name == nil {
			continue
		}
		if fd.Function.Name.Name.String() == wrapperName {
			return fd.Function.Body.List, nil
		}
	}
	return nil, fmt.Errorf("internal: wrapper function not found after preprocessing")
}

// line converts a node position to the ORIGINAL file's 1-based line
// (the wrapper adds exactly one line above the user source).
func (s *script) line(idx file.Idx) int {
	if s.prog.File == nil {
		return 0
	}
	l := s.prog.File.Position(int(idx)).Line - 1
	if l < 1 {
		l = 1
	}
	return l
}

// snippet slices the source text of a node (Idx is a 1-based offset).
func (s *script) snippet(from, to file.Idx) string {
	a, b := int(from)-1, int(to)-1
	if a < 0 || b > len(s.src) || a >= b {
		return ""
	}
	return strings.TrimSpace(s.src[a:b])
}

// firstLine truncates a snippet to its first line for report entries.
func firstLine(sn string) string {
	if i := strings.IndexByte(sn, '\n'); i >= 0 {
		return sn[:i] + " …"
	}
	return sn
}

// ---- statement walking ----

// walkStatements classifies statements into steps. inLoop switches the
// interpretation of `if (...) break` from a conditional exit to the
// loop's break condition.
func (s *script) walkStatements(list []ast.Statement, inLoop bool) []*step {
	var steps []*step
	for _, st := range list {
		steps = append(steps, s.walkStatement(st, inLoop)...)
	}
	return steps
}

func (s *script) walkStatement(st ast.Statement, inLoop bool) []*step {
	switch n := st.(type) {
	case *ast.LexicalDeclaration:
		return s.handleBindings(n.List, n.Token == token.LET)
	case *ast.VariableStatement:
		return s.handleBindings(n.List, true)
	case *ast.ExpressionStatement:
		return s.handleExpression(n.Expression)
	case *ast.IfStatement:
		return s.handleIf(n, inLoop)
	case *ast.ForStatement:
		return s.handleLoop(n.Body, boundedIterations(n, s.bindings), s.line(n.For))
	case *ast.WhileStatement:
		return s.handleLoop(n.Body, 0, s.line(n.While))
	case *ast.DoWhileStatement:
		return s.handleLoop(n.Body, 0, s.line(n.Do))
	case *ast.ReturnStatement:
		return []*step{{kind: stepReturn, line: s.line(n.Return)}}
	case *ast.BlockStatement:
		return s.walkStatements(n.List, inLoop)
	case *ast.TryStatement:
		// Inline the try body; catch/finally have no .bot equivalent
		// (recovery lives on Verified Action tool nodes instead).
		s.report.dropped(s.line(n.Try), "try/catch/finally: body inlined, handlers dropped (consider a Verified Action tool node for recovery)")
		return s.walkStatements(n.Body.List, inLoop)
	case *ast.EmptyStatement:
		return nil
	case *ast.FunctionDeclaration:
		name := "?"
		if n.Function != nil && n.Function.Name != nil {
			name = n.Function.Name.Name.String()
		}
		s.report.dropped(s.line(n.Function.Function), "helper function %s() dropped — inline its effect into prompts or a compute node", name)
		return nil
	case *ast.BranchStatement:
		if n.Token == token.BREAK && inLoop {
			// Unconditional break: the loop runs exactly once; noted by
			// the caller via the missing breakCond.
			s.report.dropped(s.line(n.Idx), "unconditional break dropped (loop lowered as bounded)")
			return nil
		}
		s.report.dropped(s.line(n.Idx), "%s statement dropped", n.Token.String())
		return nil
	case *ast.ForOfStatement:
		s.report.placeholder(s.line(n.For), "for…of over a collection: no direct sequential-iteration equivalent — consider a router `mode: fan_out_each` (parallel) and adapt")
		return []*step{{kind: stepUnknown, line: s.line(n.For), text: firstLine(s.snippet(n.Idx0(), n.Idx1()))}}
	default:
		ln := s.line(st.Idx0())
		sn := firstLine(s.snippet(st.Idx0(), st.Idx1()))
		s.report.placeholder(ln, "unmapped statement kept as comment: %s", sn)
		return []*step{{kind: stepUnknown, line: ln, text: sn}}
	}
}

// ---- const/let bindings ----

func (s *script) handleBindings(list []*ast.Binding, mutable bool) []*step {
	var steps []*step
	for _, b := range list {
		ident, ok := b.Target.(*ast.Identifier)
		if !ok {
			ln := s.line(b.Target.Idx0())
			s.report.placeholder(ln, "destructuring binding kept as comment")
			steps = append(steps, &step{kind: stepUnknown, line: ln, text: firstLine(s.snippet(b.Target.Idx0(), b.Target.Idx1()))})
			continue
		}
		name := ident.Name.String()
		if b.Initializer == nil {
			s.bindings[name] = &binding{kind: bindLocal}
			continue
		}
		steps = append(steps, s.handleBinding(name, b.Initializer, mutable)...)
	}
	return steps
}

func (s *script) handleBinding(name string, init ast.Expression, mutable bool) []*step {
	ln := s.line(init.Idx0())
	init = unwrapAwait(init)

	switch e := init.(type) {
	case *ast.ObjectLiteral:
		if name == "meta" {
			m := s.parseMeta(e)
			return []*step{{kind: stepMeta, line: ln, meta: m}}
		}
		if looksLikeJSONSchema(e) {
			sc := s.parseSchemaObject(name, e)
			s.bindings[name] = &binding{kind: bindSchema}
			return []*step{{kind: stepSchema, line: ln, schema: sc}}
		}
		s.bindings[name] = &binding{kind: bindLocal}
		s.report.dropped(ln, "object literal `%s` has no .bot equivalent (not a JSON schema)", name)
		return nil
	case *ast.CallExpression:
		if call := s.classifyCall(e, name); call != nil {
			return call
		}
		s.bindings[name] = &binding{kind: bindLocal}
		s.report.dropped(ln, "call binding `%s = %s` dropped — value becomes a hole where referenced", name, firstLine(s.snippet(e.Idx0(), e.Idx1())))
		return nil
	case *ast.StringLiteral:
		s.bindings[name] = &binding{kind: bindStaticString, str: e.Value.String()}
		return nil
	case *ast.TemplateLiteral:
		if len(e.Expressions) == 0 && len(e.Elements) == 1 {
			s.bindings[name] = &binding{kind: bindStaticString, str: e.Elements[0].Parsed.String()}
			return nil
		}
		s.bindings[name] = &binding{kind: bindLocal}
		return nil
	case *ast.NumberLiteral:
		if iv, ok := numberAsInt(e); ok {
			s.bindings[name] = &binding{kind: bindNumber, num: iv}
			return nil
		}
		s.bindings[name] = &binding{kind: bindLocal}
		return nil
	default:
		if mentionsIdentifier(init, "args") {
			// `const bug = args && args.bug || ''` — an input: promote
			// to a workflow var of the same name.
			s.bindings[name] = &binding{kind: bindArgDerived}
			s.report.hole(ln, "`%s` derived from args → var `%s` (fill at launch: --var %s=…)", name, sanitizeIdent(name), sanitizeIdent(name))
			return nil
		}
		s.bindings[name] = &binding{kind: bindLocal}
		if !mutable {
			s.report.dropped(ln, "const `%s` = <dynamic JS> dropped — becomes a hole where referenced", name)
		}
		return nil
	}
}

// classifyCall maps agent(...) / Promise.all(...) / parallel(...) /
// pipeline(...) bindings; returns nil when the call is none of them.
func (s *script) classifyCall(e *ast.CallExpression, resultVar string) []*step {
	ln := s.line(e.Idx0())
	switch calleeName(e.Callee) {
	case "agent":
		ag := s.parseAgentCall(e, resultVar)
		if resultVar != "" {
			s.bindings[resultVar] = &binding{kind: bindAgent}
		}
		return []*step{{kind: stepAgent, line: ln, agent: ag}}
	case "Promise.all":
		if fan := s.parsePromiseAll(e, resultVar); fan != nil {
			if resultVar != "" {
				s.bindings[resultVar] = &binding{kind: bindLocal}
			}
			return []*step{{kind: stepFanOut, line: ln, fan: fan}}
		}
	case "parallel":
		if fan := s.parseParallelThunks(e, resultVar); fan != nil {
			if resultVar != "" {
				s.bindings[resultVar] = &binding{kind: bindLocal}
			}
			return []*step{{kind: stepFanOut, line: ln, fan: fan}}
		}
	case "pipeline":
		if fan := s.parsePipeline(e, resultVar); fan != nil {
			if resultVar != "" {
				s.bindings[resultVar] = &binding{kind: bindLocal}
			}
			return []*step{{kind: stepFanOut, line: ln, fan: fan}}
		}
	}
	return nil
}

// ---- expression statements ----

func (s *script) handleExpression(e ast.Expression) []*step {
	ln := s.line(e.Idx0())
	e = unwrapAwait(e)
	switch n := e.(type) {
	case *ast.CallExpression:
		switch calleeName(n.Callee) {
		case "phase":
			if len(n.ArgumentList) >= 1 {
				if txt, ok := staticString(n.ArgumentList[0]); ok {
					return []*step{{kind: stepPhase, line: ln, text: txt}}
				}
			}
			return nil
		case "log":
			return []*step{{kind: stepLog, line: ln, text: s.logText(n)}}
		}
		if call := s.classifyCall(n, ""); call != nil {
			return call
		}
		sn := firstLine(s.snippet(n.Idx0(), n.Idx1()))
		s.report.placeholder(ln, "unmapped call kept as comment: %s", sn)
		return []*step{{kind: stepUnknown, line: ln, text: sn}}
	case *ast.AssignExpression:
		// Local mutation (lastFailure = …): .bot has no mutable state;
		// loop-carried context is the judge/agent's own previous output.
		s.report.dropped(ln, "local mutation `%s` dropped (loop-carried state: see loop.<name>.previous_output)", firstLine(s.snippet(n.Idx0(), n.Idx1())))
		return nil
	default:
		sn := firstLine(s.snippet(e.Idx0(), e.Idx1()))
		s.report.placeholder(ln, "unmapped expression kept as comment: %s", sn)
		return []*step{{kind: stepUnknown, line: ln, text: sn}}
	}
}

// logText renders a log() argument as best-effort static text.
func (s *script) logText(call *ast.CallExpression) string {
	if len(call.ArgumentList) == 0 {
		return ""
	}
	parts := s.promptParts(call.ArgumentList[0])
	var b strings.Builder
	for _, p := range parts {
		if p.expr == nil {
			b.WriteString(p.text)
		} else {
			b.WriteString("<" + firstLine(s.snippet(p.expr.Idx0(), p.expr.Idx1())) + ">")
		}
	}
	return strings.TrimSpace(b.String())
}

// ---- if statements ----

func (s *script) handleIf(n *ast.IfStatement, inLoop bool) []*step {
	// n.If is not reliably populated by the goja parser; the test
	// expression always is.
	ln := s.line(n.Test.Idx0())
	body := blockList(n.Consequent)

	if n.Alternate != nil {
		sn := firstLine(s.snippet(n.If, n.Test.Idx1()))
		s.report.placeholder(ln, "if/else branch kept as comment (only guard-style `if (…) return` maps): %s", sn)
		return []*step{{kind: stepUnknown, line: ln, text: sn}}
	}

	hasBreak := containsBreak(body)
	hasReturn := containsReturn(body)
	hasThrow := containsThrow(body)

	if inLoop && hasBreak {
		// The loop's exit condition; the caller (handleLoop) lifts it.
		return []*step{{kind: stepCondExit, line: ln, cond: &condExitInfo{
			condExpr: n.Test,
			condJS:   s.snippet(n.Test.Idx0(), n.Test.Idx1()),
			exit:     "break",
			line:     ln,
		}}}
	}
	if hasReturn || hasThrow {
		exit := "done"
		if hasThrow {
			exit = "fail"
		}
		return []*step{{kind: stepCondExit, line: ln, cond: &condExitInfo{
			condExpr: n.Test,
			condJS:   s.snippet(n.Test.Idx0(), n.Test.Idx1()),
			exit:     exit,
			line:     ln,
		}}}
	}
	// Side-effect-only guard (log, assignment): drop with a note.
	s.report.dropped(ln, "conditional side-effect dropped: if (%s) { … }", firstLine(s.snippet(n.Test.Idx0(), n.Test.Idx1())))
	return nil
}

// ---- loops ----

// handleLoop parses a for/while body. maxIter == 0 means unbounded.
func (s *script) handleLoop(body ast.Statement, maxIter int, ln int) []*step {
	inner := s.walkStatements(blockList(body), true)

	// Lift the break condition (an in-loop `if (…) break` produced a
	// stepCondExit with exit == "break").
	li := &loopInfo{bounded: maxIter > 0, maxIter: maxIter, fuel: 25, line: ln}
	var kept []*step
	for _, st := range inner {
		if st.kind == stepCondExit && st.cond.exit == "break" {
			if li.breakCond == nil {
				li.breakCond = st.cond.condExpr
				li.breakJS = st.cond.condJS
			} else {
				s.report.placeholder(st.line, "second break condition kept as comment: if (%s) break", firstLine(st.cond.condJS))
				kept = append(kept, &step{kind: stepUnknown, line: st.line, text: "if (" + firstLine(st.cond.condJS) + ") break"})
			}
			continue
		}
		kept = append(kept, st)
	}
	li.body = kept

	hasAgent := false
	for _, st := range kept {
		if st.kind == stepAgent {
			hasAgent = true
		}
	}
	if !hasAgent {
		// A compute-only JS loop has no .bot equivalent: import the
		// body once, visibly.
		s.report.dropped(ln, "loop without agent() calls: body imported once (compute loops have no .bot equivalent)")
		return kept
	}
	return []*step{{kind: stepLoop, line: ln, loop: li}}
}

// boundedIterations extracts a literal (or const-resolved) iteration
// count from a classic `for (let i = 1; i <= N; i++)` header; 0 when
// the bound is not statically known.
func boundedIterations(n *ast.ForStatement, bindings map[string]*binding) int {
	be, ok := n.Test.(*ast.BinaryExpression)
	if !ok {
		return 0
	}
	if be.Operator != token.LESS && be.Operator != token.LESS_OR_EQUAL {
		return 0
	}
	switch r := be.Right.(type) {
	case *ast.NumberLiteral:
		if iv, ok := numberAsInt(r); ok && iv > 0 {
			return iv
		}
	case *ast.Identifier:
		if b := bindings[r.Name.String()]; b != nil && b.kind == bindNumber && b.num > 0 {
			return b.num
		}
	}
	return 0
}

// ---- meta / schema / agent parsing ----

func (s *script) parseMeta(obj *ast.ObjectLiteral) *metaInfo {
	m := &metaInfo{}
	for _, p := range obj.Value {
		key, val, ok := keyedProperty(p)
		if !ok {
			continue
		}
		switch key {
		case "name":
			m.name, _ = staticString(val)
		case "description":
			m.description, _ = staticString(val)
		case "phases":
			arr, ok := val.(*ast.ArrayLiteral)
			if !ok {
				continue
			}
			for _, el := range arr.Value {
				po, ok := el.(*ast.ObjectLiteral)
				if !ok {
					continue
				}
				var ph phaseMeta
				for _, pp := range po.Value {
					k, v, ok := keyedProperty(pp)
					if !ok {
						continue
					}
					switch k {
					case "title":
						ph.title, _ = staticString(v)
					case "detail":
						ph.detail, _ = staticString(v)
					}
				}
				if ph.title != "" {
					m.phases = append(m.phases, ph)
				}
			}
		}
	}
	return m
}

func looksLikeJSONSchema(obj *ast.ObjectLiteral) bool {
	for _, p := range obj.Value {
		key, val, ok := keyedProperty(p)
		if !ok {
			continue
		}
		if key == "properties" {
			return true
		}
		if key == "type" {
			if tv, ok := staticString(val); ok && tv == "object" {
				return true
			}
		}
	}
	return false
}

// parseSchemaObject lowers a JSON-schema object literal to DSL fields.
func (s *script) parseSchemaObject(constName string, obj *ast.ObjectLiteral) *schemaInfo {
	sc := &schemaInfo{constName: constName}
	ln := s.line(obj.LeftBrace)
	for _, p := range obj.Value {
		key, val, ok := keyedProperty(p)
		if !ok {
			continue
		}
		switch key {
		case "type", "required":
			// `type: object` implied; DSL schema fields are all
			// required — a looser `required:` list is not expressible.
		case "properties":
			po, ok := val.(*ast.ObjectLiteral)
			if !ok {
				continue
			}
			for _, fp := range po.Value {
				fname, fval, ok := keyedProperty(fp)
				if !ok {
					continue
				}
				fobj, ok := fval.(*ast.ObjectLiteral)
				if !ok {
					sc.fields = append(sc.fields, schemaField{name: sanitizeIdent(fname), typ: "json"})
					continue
				}
				sc.fields = append(sc.fields, s.parseSchemaField(fname, fobj))
			}
		default:
			s.report.dropped(ln, "schema `%s`: JSON-schema keyword `%s` has no DSL equivalent", constName, key)
		}
	}
	return sc
}

func (s *script) parseSchemaField(name string, obj *ast.ObjectLiteral) schemaField {
	f := schemaField{name: sanitizeIdent(name), typ: "json"}
	var itemsType string
	for _, p := range obj.Value {
		key, val, ok := keyedProperty(p)
		if !ok {
			continue
		}
		switch key {
		case "type":
			tv, _ := staticString(val)
			switch tv {
			case "string":
				f.typ = "string"
			case "boolean":
				f.typ = "bool"
			case "integer":
				f.typ = "int"
			case "number":
				f.typ = "float"
			case "array":
				f.typ = "array" // refined by items below
			default:
				f.typ = "json"
			}
		case "description":
			f.desc, _ = staticString(val)
		case "enum":
			if arr, ok := val.(*ast.ArrayLiteral); ok {
				for _, el := range arr.Value {
					if sv, ok := staticString(el); ok {
						f.enum = append(f.enum, sv)
					}
				}
			}
		case "items":
			if io, ok := val.(*ast.ObjectLiteral); ok {
				for _, ip := range io.Value {
					k, v, ok := keyedProperty(ip)
					if ok && k == "type" {
						itemsType, _ = staticString(v)
					}
				}
			}
		}
	}
	if f.typ == "array" {
		if itemsType == "string" {
			f.typ = "string[]"
		} else {
			f.typ = "json"
		}
	}
	// Enums only make sense on strings in the DSL.
	if f.typ != "string" {
		f.enum = nil
	}
	return f
}

func (s *script) parseAgentCall(call *ast.CallExpression, resultVar string) *agentInfo {
	ag := &agentInfo{resultVar: resultVar, line: s.line(call.Idx0())}
	if len(call.ArgumentList) >= 1 {
		ag.prompt = s.promptParts(call.ArgumentList[0])
	}
	if len(call.ArgumentList) >= 2 {
		opts, ok := call.ArgumentList[1].(*ast.ObjectLiteral)
		if ok {
			for _, p := range opts.Value {
				key, val, ok := keyedProperty(p)
				if !ok {
					continue
				}
				switch key {
				case "label":
					ag.label = staticLabelPrefix(val)
				case "phase":
					ag.phase, _ = staticString(val)
				case "model":
					ag.model, _ = staticString(val)
				case "effort":
					ag.effort, _ = staticString(val)
				case "schema":
					switch sv := val.(type) {
					case *ast.Identifier:
						ag.schemaConst = sv.Name.String()
					case *ast.ObjectLiteral:
						if looksLikeJSONSchema(sv) {
							ag.inlineSchema = s.parseSchemaObject("", sv)
						}
					}
				default:
					s.report.dropped(ag.line, "agent option `%s` has no .bot equivalent", key)
				}
			}
		}
	}
	return ag
}

// promptParts flattens a prompt expression (string literals, template
// literals, `+` concatenations) into text and hole parts.
func (s *script) promptParts(e ast.Expression) []promptPart {
	ln := s.line(e.Idx0())
	switch n := e.(type) {
	case *ast.StringLiteral:
		return []promptPart{{text: n.Value.String(), line: ln}}
	case *ast.TemplateLiteral:
		var parts []promptPart
		for i, el := range n.Elements {
			if el.Parsed != "" {
				parts = append(parts, promptPart{text: el.Parsed.String(), line: ln})
			}
			if i < len(n.Expressions) {
				parts = append(parts, promptPart{expr: n.Expressions[i], line: s.line(n.Expressions[i].Idx0())})
			}
		}
		return parts
	case *ast.BinaryExpression:
		if n.Operator == token.PLUS {
			return append(s.promptParts(n.Left), s.promptParts(n.Right)...)
		}
	case *ast.Identifier:
		return []promptPart{{expr: n, line: ln}}
	}
	// Conditional fragments, function calls, … — a single hole; never
	// guess a plausible-but-wrong prompt.
	return []promptPart{{expr: e, line: ln}}
}

// ---- fan-out parsing ----

// parsePromiseAll recognizes `Promise.all(<items>.map(<p> => agent(…)))`.
func (s *script) parsePromiseAll(call *ast.CallExpression, resultVar string) *fanInfo {
	if len(call.ArgumentList) != 1 {
		return nil
	}
	mapCall, ok := call.ArgumentList[0].(*ast.CallExpression)
	if !ok {
		return nil
	}
	dot, ok := mapCall.Callee.(*ast.DotExpression)
	if !ok || dot.Identifier.Name.String() != "map" || len(mapCall.ArgumentList) != 1 {
		return nil
	}
	param, agents := s.stageAgents(mapCall.ArgumentList[0])
	if len(agents) == 0 {
		return nil
	}
	return &fanInfo{
		mode:      "fan_out_each",
		overExpr:  dot.Left,
		param:     param,
		agents:    agents,
		resultVar: resultVar,
		line:      s.line(call.Idx0()),
	}
}

// parseParallelThunks recognizes `parallel([() => agent(…), …])`.
func (s *script) parseParallelThunks(call *ast.CallExpression, resultVar string) *fanInfo {
	if len(call.ArgumentList) != 1 {
		return nil
	}
	arr, ok := call.ArgumentList[0].(*ast.ArrayLiteral)
	if !ok {
		return nil
	}
	var agents []*agentInfo
	for _, el := range arr.Value {
		_, ags := s.stageAgents(el)
		agents = append(agents, ags...)
	}
	if len(agents) == 0 {
		return nil
	}
	return &fanInfo{mode: "fan_out_all", agents: agents, resultVar: resultVar, line: s.line(call.Idx0())}
}

// parsePipeline recognizes `pipeline(<items>, stage1, stage2, …)` where
// each stage is an arrow function containing an agent() call.
func (s *script) parsePipeline(call *ast.CallExpression, resultVar string) *fanInfo {
	if len(call.ArgumentList) < 2 {
		return nil
	}
	var agents []*agentInfo
	param := ""
	for _, stage := range call.ArgumentList[1:] {
		p, ags := s.stageAgents(stage)
		if param == "" {
			param = p
		}
		agents = append(agents, ags...)
	}
	if len(agents) == 0 {
		return nil
	}
	return &fanInfo{
		mode:      "fan_out_each",
		overExpr:  call.ArgumentList[0],
		param:     param,
		agents:    agents,
		resultVar: resultVar,
		line:      s.line(call.Idx0()),
	}
}

// stageAgents extracts agent() calls from an arrow-function stage.
func (s *script) stageAgents(e ast.Expression) (param string, agents []*agentInfo) {
	arrow, ok := e.(*ast.ArrowFunctionLiteral)
	if !ok {
		return "", nil
	}
	if arrow.ParameterList != nil && len(arrow.ParameterList.List) > 0 {
		if id, ok := arrow.ParameterList.List[0].Target.(*ast.Identifier); ok {
			param = id.Name.String()
		}
	}
	collect := func(expr ast.Expression) {
		expr = unwrapAwait(expr)
		if c, ok := expr.(*ast.CallExpression); ok && calleeName(c.Callee) == "agent" {
			agents = append(agents, s.parseAgentCall(c, ""))
		}
	}
	switch body := arrow.Body.(type) {
	case *ast.ExpressionBody:
		collect(body.Expression)
	case *ast.BlockStatement:
		for _, st := range body.List {
			switch bs := st.(type) {
			case *ast.ExpressionStatement:
				collect(bs.Expression)
			case *ast.ReturnStatement:
				if bs.Argument != nil {
					collect(bs.Argument)
				}
			case *ast.LexicalDeclaration:
				for _, bd := range bs.List {
					if bd.Initializer != nil {
						collect(bd.Initializer)
					}
				}
			}
		}
	}
	return param, agents
}

// ---- small AST helpers ----

func unwrapAwait(e ast.Expression) ast.Expression {
	if aw, ok := e.(*ast.AwaitExpression); ok {
		return aw.Argument
	}
	return e
}

// calleeName renders `agent`, `Promise.all`, `budget.remaining`, ….
func calleeName(e ast.Expression) string {
	switch n := e.(type) {
	case *ast.Identifier:
		return n.Name.String()
	case *ast.DotExpression:
		return calleeName(n.Left) + "." + n.Identifier.Name.String()
	}
	return ""
}

func keyedProperty(p ast.Property) (string, ast.Expression, bool) {
	switch n := p.(type) {
	case *ast.PropertyKeyed:
		switch k := n.Key.(type) {
		case *ast.StringLiteral:
			return k.Value.String(), n.Value, true
		case *ast.Identifier:
			return k.Name.String(), n.Value, true
		}
	case *ast.PropertyShort:
		return n.Name.Name.String(), &n.Name, true
	}
	return "", nil, false
}

func staticString(e ast.Expression) (string, bool) {
	switch n := e.(type) {
	case *ast.StringLiteral:
		return n.Value.String(), true
	case *ast.TemplateLiteral:
		if len(n.Expressions) == 0 && len(n.Elements) == 1 {
			return n.Elements[0].Parsed.String(), true
		}
	}
	return "", false
}

// staticLabelPrefix extracts a usable node id from a label that may be
// templated (`implementer#${iter}` → `implementer`).
func staticLabelPrefix(e ast.Expression) string {
	if sv, ok := staticString(e); ok {
		return sv
	}
	if tl, ok := e.(*ast.TemplateLiteral); ok && len(tl.Elements) > 0 {
		return strings.TrimRight(tl.Elements[0].Parsed.String(), "#-_: ")
	}
	return ""
}

func numberAsInt(n *ast.NumberLiteral) (int, bool) {
	switch v := n.Value.(type) {
	case int64:
		return int(v), true
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	}
	return 0, false
}

func blockList(st ast.Statement) []ast.Statement {
	if b, ok := st.(*ast.BlockStatement); ok {
		return b.List
	}
	if st == nil {
		return nil
	}
	return []ast.Statement{st}
}

func containsBreak(list []ast.Statement) bool {
	for _, st := range list {
		if br, ok := st.(*ast.BranchStatement); ok && br.Token == token.BREAK {
			return true
		}
	}
	return false
}

func containsReturn(list []ast.Statement) bool {
	for _, st := range list {
		if _, ok := st.(*ast.ReturnStatement); ok {
			return true
		}
	}
	return false
}

func containsThrow(list []ast.Statement) bool {
	for _, st := range list {
		if _, ok := st.(*ast.ThrowStatement); ok {
			return true
		}
	}
	return false
}

// mentionsIdentifier reports whether the expression tree references a
// bare identifier with the given name (best-effort, covers the shapes
// promptParts and bindings meet).
func mentionsIdentifier(e ast.Expression, name string) bool {
	switch n := e.(type) {
	case nil:
		return false
	case *ast.Identifier:
		return n.Name.String() == name
	case *ast.DotExpression:
		return mentionsIdentifier(n.Left, name)
	case *ast.BracketExpression:
		return mentionsIdentifier(n.Left, name) || mentionsIdentifier(n.Member, name)
	case *ast.BinaryExpression:
		return mentionsIdentifier(n.Left, name) || mentionsIdentifier(n.Right, name)
	case *ast.UnaryExpression:
		return mentionsIdentifier(n.Operand, name)
	case *ast.ConditionalExpression:
		return mentionsIdentifier(n.Test, name) || mentionsIdentifier(n.Consequent, name) || mentionsIdentifier(n.Alternate, name)
	case *ast.CallExpression:
		if mentionsIdentifier(n.Callee, name) {
			return true
		}
		for _, a := range n.ArgumentList {
			if mentionsIdentifier(a, name) {
				return true
			}
		}
	case *ast.TemplateLiteral:
		for _, x := range n.Expressions {
			if mentionsIdentifier(x, name) {
				return true
			}
		}
	case *ast.SequenceExpression:
		for _, x := range n.Sequence {
			if mentionsIdentifier(x, name) {
				return true
			}
		}
	}
	return false
}

// sanitizeIdent lowers a name into a DSL-safe identifier.
func sanitizeIdent(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-' || r == ' ' || r == '.' || r == '#':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unnamed"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "n_" + out
	}
	return out
}
