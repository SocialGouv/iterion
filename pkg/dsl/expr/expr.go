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
// Builtin functions: `length`, `concat`, `unique`, `contains`. See the
// builtins map below for signatures and semantics. Function calls are
// disambiguated from path lookups purely by the presence of `(` directly
// after the leading IDENT — there is no separate keyword set.
package expr

import (
	"fmt"
	"strconv"
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
		return t != ""
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
	lex *lexer
	cur token
	err error
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
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
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
	if op == "+" {
		if as, ok := a.(string); ok {
			return as + fmt.Sprintf("%v", b), nil
		}
		if bs, ok := b.(string); ok {
			return fmt.Sprintf("%v", a) + bs, nil
		}
	}
	ai, aiok := toInt(a)
	bi, biok := toInt(b)
	if aiok && biok {
		switch op {
		case "+":
			return ai + bi, nil
		case "-":
			return ai - bi, nil
		case "*":
			return ai * bi, nil
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
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	}
	return 0, false
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
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
}

func evalFuncCall(n *funcCallNode, ctx *Context) (interface{}, error) {
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
	return nil, fmt.Errorf("expr: length() expects array or string, got %T", args[0])
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
