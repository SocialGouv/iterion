// Package expr implements a small expression language used by iterion's
// `compute` nodes and `when` edge clauses.
//
// Grammar (informal):
//
//	expr     := or
//	or       := and ( "||" and )*
//	and      := not ( "&&" not )*
//	not      := "!" not | cmp
//	cmp      := add ( ( "==" | "!=" | "<" | "<=" | ">" | ">=" ) add )?
//	add      := mul ( ( "+" | "-" ) mul )*
//	mul      := unary ( ( "*" | "/" | "%" ) unary )*
//	unary    := "-" unary | primary
//	primary  := number | string | bool | funcCall | path | "(" expr ")"
//	funcCall := IDENT "(" ( expr ( "," expr )* )? ")"
//	path     := IDENT ( "." IDENT )*
//
// The path namespaces recognized by the evaluator depend on the Context:
// `vars`, `input`, `outputs`, `artifacts`, `loop.<name>.{iteration,max,previous_output[.field]}`,
// and `run.{id}` are the standard ones.
//
// Builtin functions: `length`, `concat`, `unique`, `contains`, `join`,
// `if(cond, then, else)`. See the builtins map below for signatures and
// semantics. Function calls are disambiguated from path lookups purely by
// the presence of `(` directly after the leading IDENT — there is no
// separate keyword set.
package expr

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// AST is the parsed form of an expression. It is opaque outside the package.
type AST struct {
	root node
	src  string
}

// Source returns the original source string for debugging.
func (a *AST) Source() string { return a.src }

// Parse parses an expression string and returns its AST. Returns an error on
// malformed input. The returned AST can be evaluated many times against
// different contexts.
func Parse(src string) (*AST, error) {
	p := &parser{lex: newLexer(src)}
	p.advance()
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.cur.kind != tokEOF {
		return nil, fmt.Errorf("expr: unexpected trailing %s", p.cur.value)
	}
	return &AST{root: root, src: src}, nil
}

// MustParse is like Parse but panics on error. Intended for tests / constants.
func MustParse(src string) *AST {
	a, err := Parse(src)
	if err != nil {
		panic(err)
	}
	return a
}

// Context provides values that path expressions resolve against.
//
// Each callback receives the dotted path *after* the namespace prefix and
// returns the resolved value (or nil if not found). The evaluator never
// inspects the structure beyond what the callback returns.
type Context struct {
	Vars      func(path []string) interface{}
	Input     func(path []string) interface{}
	Outputs   func(path []string) interface{}
	Artifacts func(path []string) interface{}
	Loop      func(path []string) interface{} // loop.<name>.<...>
	Run       func(path []string) interface{} // run.<...>
}

// Eval evaluates the AST against the context and returns the resulting
// value (typed as bool, int64, float64, string, or nil for absent paths).
func (a *AST) Eval(ctx *Context) (interface{}, error) {
	if a == nil || a.root == nil {
		return nil, nil
	}
	return evalNode(a.root, ctx)
}

// EvalBool evaluates and coerces the result to bool. Non-bool truthy values
// follow standard rules: nil → false, "" → false, 0 → false, others → true.
func (a *AST) EvalBool(ctx *Context) (bool, error) {
	v, err := a.Eval(ctx)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// Refs returns the unique namespace.path tuples referenced by the expression.
// Used by the compiler to validate that all references resolve.
func (a *AST) Refs() []Ref {
	if a == nil || a.root == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var refs []Ref
	walkRefs(a.root, func(r Ref) {
		key := r.Namespace + ":" + joinPath(r.Path)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, r)
	})
	return refs
}

// Ref is a single namespace.path reference extracted from an expression.
type Ref struct {
	Namespace string
	Path      []string
}

func joinPath(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "."
		}
		out += s
	}
	return out
}

func truthy(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		// LLM tool outputs and JSON-decoded env vars commonly carry
		// boolean-shaped data as strings. Treat "false" / "0" / "no"
		// as falsy so `when not approved` with approved="false" behaves
		// the way the workflow author wrote it. Empty string stays
		// falsy (consistent with the prior contract).
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "", "false", "no", "0":
			return false
		}
		return true
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []interface{}:
		return len(t) > 0
	case map[string]interface{}:
		return len(t) > 0
	}
	return true
}

// ---------------------------------------------------------------------------
// Token / Lexer
// ---------------------------------------------------------------------------

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokInt
	tokFloat
	tokString
	tokTrue
	tokFalse
	tokDot
	tokComma
	tokLParen
	tokRParen
	tokAnd   // &&
	tokOr    // ||
	tokNot   // !
	tokEq    // ==
	tokNeq   // !=
	tokLt    // <
	tokLte   // <=
	tokGt    // >
	tokGte   // >=
	tokPlus  // +
	tokMinus // -
	tokStar  // *
	tokSlash // /
	tokPct   // %
	tokKwAnd // and
	tokKwOr  // or
	tokKwNot // not
)

type token struct {
	kind  tokKind
	value string
}

type lexer struct {
	src string
	pos int
}

func newLexer(src string) *lexer {
	return &lexer{src: src}
}

func (l *lexer) next() (token, error) {
	// Skip whitespace.
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			l.pos++
			continue
		}
		break
	}
	if l.pos >= len(l.src) {
		return token{kind: tokEOF}, nil
	}

	c := l.src[l.pos]
	switch {
	case c == '.':
		l.pos++
		return token{kind: tokDot, value: "."}, nil
	case c == ',':
		l.pos++
		return token{kind: tokComma, value: ","}, nil
	case c == '(':
		l.pos++
		return token{kind: tokLParen, value: "("}, nil
	case c == ')':
		l.pos++
		return token{kind: tokRParen, value: ")"}, nil
	case c == '+':
		l.pos++
		return token{kind: tokPlus, value: "+"}, nil
	case c == '-':
		l.pos++
		return token{kind: tokMinus, value: "-"}, nil
	case c == '*':
		l.pos++
		return token{kind: tokStar, value: "*"}, nil
	case c == '/':
		l.pos++
		return token{kind: tokSlash, value: "/"}, nil
	case c == '%':
		l.pos++
		return token{kind: tokPct, value: "%"}, nil
	case c == '&':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '&' {
			l.pos += 2
			return token{kind: tokAnd, value: "&&"}, nil
		}
		return token{}, fmt.Errorf("expr: lone '&' at offset %d", l.pos)
	case c == '|':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '|' {
			l.pos += 2
			return token{kind: tokOr, value: "||"}, nil
		}
		return token{}, fmt.Errorf("expr: lone '|' at offset %d", l.pos)
	case c == '!':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tokNeq, value: "!="}, nil
		}
		l.pos++
		return token{kind: tokNot, value: "!"}, nil
	case c == '=':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tokEq, value: "=="}, nil
		}
		return token{}, fmt.Errorf("expr: lone '=' at offset %d (use '==' for equality)", l.pos)
	case c == '<':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tokLte, value: "<="}, nil
		}
		l.pos++
		return token{kind: tokLt, value: "<"}, nil
	case c == '>':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tokGte, value: ">="}, nil
		}
		l.pos++
		return token{kind: tokGt, value: ">"}, nil
	case c == '"' || c == '\'':
		return l.readString(c)
	case c >= '0' && c <= '9':
		return l.readNumber()
	case isIdentStart(c):
		return l.readIdent()
	}
	return token{}, fmt.Errorf("expr: unexpected character %q at offset %d", c, l.pos)
}

func (l *lexer) readString(quote byte) (token, error) {
	start := l.pos
	l.pos++ // skip opening quote
	var sb []byte
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\\' && l.pos+1 < len(l.src) {
			next := l.src[l.pos+1]
			switch next {
			case '\\':
				sb = append(sb, '\\')
			case '"':
				sb = append(sb, '"')
			case '\'':
				sb = append(sb, '\'')
			case 'n':
				sb = append(sb, '\n')
			case 't':
				sb = append(sb, '\t')
			default:
				sb = append(sb, next)
			}
			l.pos += 2
			continue
		}
		if c == quote {
			l.pos++
			return token{kind: tokString, value: string(sb)}, nil
		}
		sb = append(sb, c)
		l.pos++
	}
	return token{}, fmt.Errorf("expr: unterminated string starting at offset %d", start)
}

func (l *lexer) readNumber() (token, error) {
	start := l.pos
	isFloat := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c >= '0' && c <= '9' {
			l.pos++
			continue
		}
		if c == '.' && !isFloat && l.pos+1 < len(l.src) && l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9' {
			isFloat = true
			l.pos++
			continue
		}
		break
	}
	value := l.src[start:l.pos]
	if isFloat {
		return token{kind: tokFloat, value: value}, nil
	}
	return token{kind: tokInt, value: value}, nil
}

func (l *lexer) readIdent() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isIdentCont(l.src[l.pos]) {
		l.pos++
	}
	value := l.src[start:l.pos]
	switch value {
	case "true":
		return token{kind: tokTrue, value: value}, nil
	case "false":
		return token{kind: tokFalse, value: value}, nil
	case "and":
		return token{kind: tokKwAnd, value: value}, nil
	case "or":
		return token{kind: tokKwOr, value: value}, nil
	case "not":
		return token{kind: tokKwNot, value: value}, nil
	}
	return token{kind: tokIdent, value: value}, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// ---------------------------------------------------------------------------
// AST node types
// ---------------------------------------------------------------------------

type node interface{ exprNode() }

type litBool struct{ v bool }
type litInt struct{ v int64 }
type litFloat struct{ v float64 }
type litString struct{ v string }
type pathNode struct {
	namespace string
	path      []string
}
type unaryNode struct {
	op    string // "!" or "-"
	child node
}
type binaryNode struct {
	op          string
	left, right node
}
type funcCallNode struct {
	name string
	args []node
}

func (litBool) exprNode()       {}
func (litInt) exprNode()        {}
func (litFloat) exprNode()      {}
func (litString) exprNode()     {}
func (pathNode) exprNode()      {}
func (*unaryNode) exprNode()    {}
func (*binaryNode) exprNode()   {}
func (*funcCallNode) exprNode() {}

// ---------------------------------------------------------------------------
// Parser (recursive-descent)
// ---------------------------------------------------------------------------

type parser struct {
	lex   *lexer
	cur   token
	err   error
	depth int
}

// maxExprDepth caps recursive descent depth so a pathologically nested
// expression (e.g. `(((...)))` from an untrusted .iter under multitenant
// cloud) can't blow the goroutine stack. Generous enough that any
// hand-written expression fits, tight enough that malicious input is
// rejected before it can exhaust the stack.
const maxExprDepth = 256

func (p *parser) enter() error {
	p.depth++
	if p.depth > maxExprDepth {
		return fmt.Errorf("expr: maximum expression depth exceeded (%d levels)", maxExprDepth)
	}
	return nil
}

func (p *parser) leave() {
	p.depth--
}

func (p *parser) advance() {
	if p.err != nil {
		return
	}
	t, err := p.lex.next()
	if err != nil {
		p.err = err
		return
	}
	p.cur = t
}

func (p *parser) parseExpr() (node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	return p.parseOr()
}

func (p *parser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tokOr || p.cur.kind == tokKwOr {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "||", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tokAnd || p.cur.kind == tokKwAnd {
		p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "&&", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseNot() (node, error) {
	if p.cur.kind == tokNot || p.cur.kind == tokKwNot {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		p.advance()
		child, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "!", child: child}, nil
	}
	return p.parseCmp()
}

func (p *parser) parseCmp() (node, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	switch p.cur.kind {
	case tokEq, tokNeq, tokLt, tokLte, tokGt, tokGte:
		op := p.cur.value
		p.advance()
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return &binaryNode{op: op, left: left, right: right}, nil
	}
	return left, nil
}

func (p *parser) parseAdd() (node, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tokPlus || p.cur.kind == tokMinus {
		op := p.cur.value
		p.advance()
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseMul() (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tokStar || p.cur.kind == tokSlash || p.cur.kind == tokPct {
		op := p.cur.value
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (node, error) {
	if p.cur.kind == tokMinus {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		p.advance()
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "-", child: child}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	if p.err != nil {
		return nil, p.err
	}
	switch p.cur.kind {
	case tokInt:
		v, err := strconv.ParseInt(p.cur.value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expr: invalid integer %q", p.cur.value)
		}
		p.advance()
		return litInt{v: v}, nil
	case tokFloat:
		v, err := strconv.ParseFloat(p.cur.value, 64)
		if err != nil {
			return nil, fmt.Errorf("expr: invalid float %q", p.cur.value)
		}
		p.advance()
		return litFloat{v: v}, nil
	case tokString:
		v := p.cur.value
		p.advance()
		return litString{v: v}, nil
	case tokTrue:
		p.advance()
		return litBool{v: true}, nil
	case tokFalse:
		p.advance()
		return litBool{v: false}, nil
	case tokLParen:
		p.advance()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.cur.kind != tokRParen {
			return nil, fmt.Errorf("expr: expected ')' got %s", p.cur.value)
		}
		p.advance()
		return inner, nil
	case tokIdent:
		ns := p.cur.value
		p.advance()
		// `IDENT(` (with no intervening dot) is a function call. Reject
		// unknown names at parse time so authoring errors surface up front.
		if p.cur.kind == tokLParen {
			if _, ok := builtins[ns]; !ok {
				return nil, fmt.Errorf("expr: unknown function %q", ns)
			}
			return p.parseFuncCallArgs(ns)
		}
		var path []string
		for p.cur.kind == tokDot {
			p.advance()
			if p.cur.kind != tokIdent {
				return nil, fmt.Errorf("expr: expected identifier after '.', got %s", p.cur.value)
			}
			path = append(path, p.cur.value)
			p.advance()
		}
		return pathNode{namespace: ns, path: path}, nil
	}
	return nil, fmt.Errorf("expr: unexpected token %s", p.cur.value)
}

// parseFuncCallArgs is invoked with `cur` sitting on the opening `(` of a
// function call. It consumes the argument list and the closing `)`.
func (p *parser) parseFuncCallArgs(name string) (node, error) {
	p.advance() // consume '('
	var args []node
	if p.cur.kind != tokRParen {
		for {
			arg, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			if p.cur.kind == tokComma {
				p.advance()
				continue
			}
			break
		}
	}
	if p.cur.kind != tokRParen {
		return nil, fmt.Errorf("expr: expected ')' or ',' in call to %s, got %s", name, p.cur.value)
	}
	p.advance() // consume ')'
	return &funcCallNode{name: name, args: args}, nil
}

// ---------------------------------------------------------------------------
// Evaluator
// ---------------------------------------------------------------------------

func evalNode(n node, ctx *Context) (interface{}, error) {
	switch v := n.(type) {
	case litBool:
		return v.v, nil
	case litInt:
		return v.v, nil
	case litFloat:
		return v.v, nil
	case litString:
		return v.v, nil
	case pathNode:
		return resolvePath(v.namespace, v.path, ctx)
	case *unaryNode:
		return evalUnary(v, ctx)
	case *binaryNode:
		return evalBinary(v, ctx)
	case *funcCallNode:
		return evalFuncCall(v, ctx)
	}
	return nil, fmt.Errorf("expr: unknown node type %T", n)
}

func resolvePath(namespace string, path []string, ctx *Context) (interface{}, error) {
	if ctx == nil {
		return nil, nil
	}
	switch namespace {
	case "vars":
		if ctx.Vars == nil {
			return nil, nil
		}
		return drill(ctx.Vars(path), nil), nil
	case "input":
		if ctx.Input == nil {
			return nil, nil
		}
		return drill(ctx.Input(path), nil), nil
	case "outputs":
		if ctx.Outputs == nil {
			return nil, nil
		}
		return drill(ctx.Outputs(path), nil), nil
	case "artifacts":
		if ctx.Artifacts == nil {
			return nil, nil
		}
		return drill(ctx.Artifacts(path), nil), nil
	case "loop":
		if ctx.Loop == nil {
			return nil, nil
		}
		return drill(ctx.Loop(path), nil), nil
	case "run":
		if ctx.Run == nil {
			return nil, nil
		}
		return drill(ctx.Run(path), nil), nil
	}
	// Bare identifier (e.g. `approved` in a `when` clause): interpret as a
	// field of the implicit `input` namespace. This matches the legacy
	// `when <field>` ergonomics where the predicate references a field of
	// the source node's output.
	if ctx.Input != nil {
		fullPath := append([]string{namespace}, path...)
		return drill(ctx.Input(fullPath), nil), nil
	}
	return nil, fmt.Errorf("expr: unknown namespace %q", namespace)
}

// drill is a placeholder — callbacks already perform path traversal. Kept as
// a hook in case future evaluators need shallow/deep resolution.
func drill(v interface{}, _ []string) interface{} {
	return v
}

func evalUnary(n *unaryNode, ctx *Context) (interface{}, error) {
	v, err := evalNode(n.child, ctx)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "!":
		return !truthy(v), nil
	case "-":
		switch t := v.(type) {
		case int64:
			return -t, nil
		case float64:
			return -t, nil
		}
		return nil, fmt.Errorf("expr: cannot negate %T", v)
	}
	return nil, fmt.Errorf("expr: unknown unary op %q", n.op)
}

func evalBinary(n *binaryNode, ctx *Context) (interface{}, error) {
	switch n.op {
	case "&&":
		l, err := evalNode(n.left, ctx)
		if err != nil {
			return nil, err
		}
		if !truthy(l) {
			return false, nil
		}
		r, err := evalNode(n.right, ctx)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	case "||":
		l, err := evalNode(n.left, ctx)
		if err != nil {
			return nil, err
		}
		if truthy(l) {
			return true, nil
		}
		r, err := evalNode(n.right, ctx)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	}

	l, err := evalNode(n.left, ctx)
	if err != nil {
		return nil, err
	}
	r, err := evalNode(n.right, ctx)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return equals(l, r), nil
	case "!=":
		return !equals(l, r), nil
	case "<", "<=", ">", ">=":
		return compare(n.op, l, r)
	case "+", "-", "*", "/", "%":
		return arith(n.op, l, r)
	}
	return nil, fmt.Errorf("expr: unknown binary op %q", n.op)
}

func equals(a, b interface{}) bool {
	// Numeric coercion: int64 vs float64.
	ai, aok := toInt(a)
	bi, bok := toInt(b)
	if aok && bok {
		return ai == bi
	}
	af, afok := toFloat(a)
	bf, bfok := toFloat(b)
	if afok && bfok {
		return af == bf
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// Type-aware string compare. The prior fallback used
	// fmt.Sprintf("%v", ...) on both sides, which coerced 5 == "5" and
	// true == "true" to true via lexical equality — producing silent
	// type confusion whenever an LLM stringified a value the schema
	// declared numeric/boolean. Require both operands to be strings
	// before comparing; otherwise return false (heterogeneous types
	// are not equal under the new contract). Bools compare via direct
	// equality already (Go interface compare handles same-typed bools).
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return as == bs
	}
	ab, aIsBool := a.(bool)
	bb, bIsBool := b.(bool)
	if aIsBool && bIsBool {
		return ab == bb
	}
	return false
}

func compare(op string, a, b interface{}) (bool, error) {
	af, afok := toFloat(a)
	bf, bfok := toFloat(b)
	if afok && bfok {
		switch op {
		case "<":
			return af < bf, nil
		case "<=":
			return af <= bf, nil
		case ">":
			return af > bf, nil
		case ">=":
			return af >= bf, nil
		}
	}
	as, asok := a.(string)
	bs, bsok := b.(string)
	if asok && bsok {
		switch op {
		case "<":
			return as < bs, nil
		case "<=":
			return as <= bs, nil
		case ">":
			return as > bs, nil
		case ">=":
			return as >= bs, nil
		}
	}
	return false, fmt.Errorf("expr: cannot compare %T %s %T", a, op, b)
}

func arith(op string, a, b interface{}) (interface{}, error) {
	// String concatenation for "+" with at least one string operand.
	// Both operands must be strings OR numerics — concatenating a
	// string with an array/map used to produce Go's debug format
	// `[a b c]`, surprising the workflow author. Reject mixed types
	// explicitly so the failure is loud (F-DSL-8).
	if op == "+" {
		if as, aok := a.(string); aok {
			if bs, bok := b.(string); bok {
				return as + bs, nil
			}
			if _, bnum := toFloat(b); bnum {
				return as + fmt.Sprintf("%v", b), nil
			}
			return nil, fmt.Errorf("expr: cannot concatenate string with %T", b)
		}
		if bs, bok := b.(string); bok {
			if _, anum := toFloat(a); anum {
				return fmt.Sprintf("%v", a) + bs, nil
			}
			return nil, fmt.Errorf("expr: cannot concatenate %T with string", a)
		}
	}
	ai, aiok := toInt(a)
	bi, biok := toInt(b)
	if aiok && biok {
		switch op {
		case "+":
			if r, ok := addCheckedInt64(ai, bi); ok {
				return r, nil
			}
			return nil, fmt.Errorf("expr: integer addition overflow (%d + %d)", ai, bi)
		case "-":
			if r, ok := subCheckedInt64(ai, bi); ok {
				return r, nil
			}
			return nil, fmt.Errorf("expr: integer subtraction overflow (%d - %d)", ai, bi)
		case "*":
			if r, ok := mulCheckedInt64(ai, bi); ok {
				return r, nil
			}
			return nil, fmt.Errorf("expr: integer multiplication overflow (%d * %d)", ai, bi)
		case "/":
			if bi == 0 {
				return nil, fmt.Errorf("expr: integer division by zero")
			}
			return ai / bi, nil
		case "%":
			if bi == 0 {
				return nil, fmt.Errorf("expr: integer modulo by zero")
			}
			return ai % bi, nil
		}
	}
	af, afok := toFloat(a)
	bf, bfok := toFloat(b)
	if afok && bfok {
		switch op {
		case "+":
			return af + bf, nil
		case "-":
			return af - bf, nil
		case "*":
			return af * bf, nil
		case "/":
			if bf == 0 {
				return nil, fmt.Errorf("expr: float division by zero")
			}
			return af / bf, nil
		}
	}
	return nil, fmt.Errorf("expr: cannot apply %s to %T and %T", op, a, b)
}

func toInt(v interface{}) (int64, bool) {
	// Cover the common numeric types JSON or Context.* callbacks can
	// produce. The prior implementation only handled int / int64 — so
	// a uint64(0) was "non-numeric" (truthy by default), a float32
	// value couldn't be added, and a json.Number always failed
	// integer coercion. Defaults to the JSON-decoded shape
	// (float64) when the value is fractional; for exact-int float
	// inputs we still report ok so `truthy(2.0)` matches `truthy(2)`.
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int8:
		return int64(t), true
	case int16:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case uint:
		if uint64(t) > uint64(1)<<63-1 {
			return 0, false
		}
		return int64(t), true
	case uint8:
		return int64(t), true
	case uint16:
		return int64(t), true
	case uint32:
		return int64(t), true
	case uint64:
		if t > uint64(1)<<63-1 {
			return 0, false
		}
		return int64(t), true
	case float32:
		if t != float32(int64(t)) {
			return 0, false
		}
		return int64(t), true
	case float64:
		if t != float64(int64(t)) {
			return 0, false
		}
		return int64(t), true
	}
	return 0, false
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float32:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

// addCheckedInt64 / subCheckedInt64 / mulCheckedInt64 perform int64
// arithmetic with overflow detection. The DSL surfaces overflow as a
// loud runtime error rather than the silent wraparound the bare
// operators would produce — a templated loop cap that overflows used
// to come out tiny/negative without explanation (F-DSL-7).
func addCheckedInt64(a, b int64) (int64, bool) {
	r := a + b
	if (b > 0 && r < a) || (b < 0 && r > a) {
		return 0, false
	}
	return r, true
}

func subCheckedInt64(a, b int64) (int64, bool) {
	r := a - b
	if (b > 0 && r > a) || (b < 0 && r < a) {
		return 0, false
	}
	return r, true
}

func mulCheckedInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

// ---------------------------------------------------------------------------
// Reference walker
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Builtin functions
// ---------------------------------------------------------------------------

// builtins is the function registry. Kept private — extending the language
// is a deliberate act, not an accidental side-effect of importing the
// package. Future additions should live here.
var builtins = map[string]func(args []interface{}) (interface{}, error){
	"length":   builtinLength,
	"concat":   builtinConcat,
	"unique":   builtinUnique,
	"contains": builtinContains,
	"join":     builtinJoin,
	"if":       builtinIf,
}

func evalFuncCall(n *funcCallNode, ctx *Context) (interface{}, error) {
	// Special form: if(cond, then, else) short-circuits. Only the
	// selected branch is evaluated, so the un-taken branch can safely
	// contain expressions that would otherwise trip a divide-by-zero
	// or similar arithmetic trap. The 2026-05-20 dogfood hit this
	// with `if(n > 0, total / n, 0)` — pre-special-form the `total/n`
	// arm evaluated eagerly when n=0 and crashed the compute node.
	// Ticket a3a9757b on the native board.
	if n.name == "if" && len(n.args) == 3 {
		condVal, err := evalNode(n.args[0], ctx)
		if err != nil {
			return nil, err
		}
		if truthy(condVal) {
			return evalNode(n.args[1], ctx)
		}
		return evalNode(n.args[2], ctx)
	}

	fn, ok := builtins[n.name]
	if !ok {
		// Belt-and-suspenders: parser already rejects unknown names, but
		// keep the runtime check in case an AST is constructed by other
		// means in the future.
		return nil, fmt.Errorf("expr: unknown function %q", n.name)
	}
	args := make([]interface{}, len(n.args))
	for i, a := range n.args {
		v, err := evalNode(a, ctx)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fn(args)
}

func builtinLength(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: length() takes 1 argument, got %d", len(args))
	}
	switch v := args[0].(type) {
	case nil:
		return int64(0), nil
	case []interface{}:
		return int64(len(v)), nil
	case string:
		return int64(len(v)), nil
	}
	// Fall back to reflection so concrete slice/array/map types coming
	// from runtime stubs or backend-specific output shapes (e.g. a
	// reviewer node returning blockers as []string instead of the
	// generic []interface{}) still measure correctly. Without this,
	// the legacy type-switch errored on every concrete-typed slice
	// and the failing `length()` silently disabled the enclosing
	// `when` edge condition — surfaced by an `.iter` workflow whose
	// streak_check edge guarded fix routing on length(blockers) > 0.
	if rv := reflect.ValueOf(args[0]); rv.IsValid() {
		switch rv.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			return int64(rv.Len()), nil
		}
	}
	return nil, fmt.Errorf("expr: length() expects array, string, or map, got %T", args[0])
}

func builtinConcat(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("expr: concat() takes at least 1 argument")
	}
	out := make([]interface{}, 0)
	for i, a := range args {
		if a == nil {
			continue
		}
		arr, ok := a.([]interface{})
		if !ok {
			return nil, fmt.Errorf("expr: concat() argument %d is %T, want array", i+1, a)
		}
		out = append(out, arr...)
	}
	return out, nil
}

func builtinUnique(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: unique() takes 1 argument, got %d", len(args))
	}
	if args[0] == nil {
		return []interface{}{}, nil
	}
	arr, ok := args[0].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expr: unique() expects array, got %T", args[0])
	}
	// Stringify for equality so heterogeneous arrays (which the runtime
	// cheerfully produces from JSON) don't blow up on map/slice keys.
	seen := make(map[string]struct{}, len(arr))
	out := make([]interface{}, 0, len(arr))
	for _, v := range arr {
		key := fmt.Sprintf("%v", v)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out, nil
}

func builtinContains(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("expr: contains() takes 2 arguments, got %d", len(args))
	}
	if args[0] == nil {
		return false, nil
	}
	arr, ok := args[0].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expr: contains() expects array as first argument, got %T", args[0])
	}
	target := fmt.Sprintf("%v", args[1])
	for _, v := range arr {
		if fmt.Sprintf("%v", v) == target {
			return true, nil
		}
	}
	return false, nil
}

// builtinIf is the fallback for direct calls; the real evaluator
// special-cases "if" in evalFuncCall to skip the un-taken branch.
// Kept here so the function name still resolves in builtin-lookup
// paths that pre-date the special form.
//
// if(cond, then, else) returns then when cond is truthy, else
// otherwise. As of ticket a3a9757b the un-taken branch is NOT
// evaluated — `if(n > 0, total / n, 0)` is safe when n == 0.
func builtinIf(args []interface{}) (interface{}, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("expr: if() takes 3 arguments (cond, then, else), got %d", len(args))
	}
	if truthy(args[0]) {
		return args[1], nil
	}
	return args[2], nil
}

func builtinJoin(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("expr: join() takes 2 arguments, got %d", len(args))
	}
	sep, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("expr: join() expects string as second argument, got %T", args[1])
	}
	if args[0] == nil {
		return "", nil
	}
	arr, ok := args[0].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expr: join() expects array as first argument, got %T", args[0])
	}
	parts := make([]string, len(arr))
	for i, v := range arr {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return strings.Join(parts, sep), nil
}

func walkRefs(n node, fn func(Ref)) {
	switch v := n.(type) {
	case pathNode:
		fn(Ref{Namespace: v.namespace, Path: append([]string(nil), v.path...)})
	case *unaryNode:
		walkRefs(v.child, fn)
	case *binaryNode:
		walkRefs(v.left, fn)
		walkRefs(v.right, fn)
	case *funcCallNode:
		for _, a := range v.args {
			walkRefs(a, fn)
		}
	}
}
