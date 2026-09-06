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
//	unary    := "-" unary | postfix
//	postfix  := primary ( "[" expr "]" )*
//	primary  := number | string | bool | lambdaComb | funcCall | path | "(" expr ")"
//	funcCall := IDENT "(" ( expr ( "," expr )* )? ")"
//	lambdaComb := ("map"|"filter") "(" expr "," lambda ")"
//	             | "reduce" "(" expr "," expr "," lambda ")"
//	lambda   := ( IDENT | "(" IDENT ( "," IDENT )* ")" ) "=>" expr
//	path     := IDENT ( "." IDENT )*
//
// The path namespaces recognized by the evaluator depend on the Context:
// `vars`, `input`, `outputs`, `artifacts`, `loop.<name>.{iteration,max,previous_output[.field]}`,
// and `run.<member>` (the run's identity, consumption and effective budget
// caps — the vocabulary is pkg/runtime's RunNamespaceMembers) are the
// standard ones.
//
// Builtin functions: `length`, `concat`, `unique`, `contains`, `join`, `tail`,
// `if(cond, then, else)`, plus the total array/map helpers `sort`, `keys`,
// `values`, `slice`, `sum`, `min`, `max`, `flatten`, and the numeric
// `floor`, `round` (a number to the int64 an `int` field expects: floor
// towards negative infinity, round half away from zero). The bounded higher-order
// combinators `map`, `filter`, `reduce` take a `=>` lambda whose parameter is a
// local binding; the lambda is not a first-class value (it can only appear at a
// combinator call site, applies once per element of a finite slice, and cannot
// recurse), so the language stays total. Function calls are disambiguated from
// path lookups purely by the presence of `(` directly after the leading IDENT.
package expr

import (
	"fmt"
	"math"
	"reflect"
	"sort"
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
	Vars      func(path []string) any
	Input     func(path []string) any
	Outputs   func(path []string) any
	Artifacts func(path []string) any
	Loop      func(path []string) any // loop.<name>.<...>
	Run       func(path []string) any // run.<...>
}

// Eval evaluates the AST against the context and returns the resulting
// value (typed as bool, int64, float64, string, or nil for absent paths).
func (a *AST) Eval(ctx *Context) (any, error) {
	if a == nil || a.root == nil {
		return nil, nil
	}
	return evalNode(a.root, &evalState{ctx: ctx, visits: maxEvalVisits})
}

// maxEvalVisits bounds the total number of per-element visits a single
// evaluation may perform across the bounded combinators (map/filter/reduce).
// maxExprDepth caps the AST *shape*; this caps the AST *work*: a shallow
// `map(a, x => map(b, y => ...))` is finite but O(|a|·|b|), so an adversarial
// .bot under multitenant cloud could otherwise pin a CPU. This keeps the
// expression layer a total terminating function in both depth and work.
const maxEvalVisits = 100_000

// evalState carries per-evaluation state threaded through the recursive
// evaluator: the resolution Context, the lambda local-binding frame stack
// (innermost last), and the remaining element-visit budget.
type evalState struct {
	ctx    *Context
	locals []map[string]any
	visits int
}

// lookupLocal resolves a lambda-bound identifier from the innermost frame out.
func (st *evalState) lookupLocal(name string) (any, bool) {
	for i := len(st.locals) - 1; i >= 0; i-- {
		if v, ok := st.locals[i][name]; ok {
			return v, true
		}
	}
	return nil, false
}

// consume debits one element visit from the budget; errors when exhausted.
func (st *evalState) consume() error {
	st.visits--
	if st.visits < 0 {
		return fmt.Errorf("expr: evaluation budget exceeded (%d element visits) — combinator input too large", maxEvalVisits)
	}
	return nil
}

// evalBody pushes a single-binding frame, evaluates body, then pops it.
func (st *evalState) evalBody(name string, val any, body node) (any, error) {
	st.locals = append(st.locals, map[string]any{name: val})
	v, err := evalNode(body, st)
	st.locals = st.locals[:len(st.locals)-1]
	return v, err
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

func truthy(v any) bool {
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
	case []any:
		return len(t) > 0
	case map[string]any:
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
	tokAnd      // &&
	tokOr       // ||
	tokNot      // !
	tokEq       // ==
	tokNeq      // !=
	tokLt       // <
	tokLte      // <=
	tokGt       // >
	tokGte      // >=
	tokPlus     // +
	tokMinus    // -
	tokStar     // *
	tokSlash    // /
	tokPct      // %
	tokKwAnd    // and
	tokKwOr     // or
	tokKwNot    // not
	tokLBracket // [
	tokRBracket // ]
	tokArrow    // =>
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
	case c == '[':
		l.pos++
		return token{kind: tokLBracket, value: "["}, nil
	case c == ']':
		l.pos++
		return token{kind: tokRBracket, value: "]"}, nil
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
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '>' {
			l.pos += 2
			return token{kind: tokArrow, value: "=>"}, nil
		}
		return token{}, fmt.Errorf("expr: lone '=' at offset %d (use '==' for equality, or '=>' for a map/filter/reduce lambda)", l.pos)
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

// indexNode is a postfix subscript `recv[index]` — array element or map value
// access. Out-of-bounds / missing keys resolve to nil (consistent with absent
// paths); a non-integer array index or non-string map key on a present
// collection is a loud error.
type indexNode struct {
	recv  node
	index node
}

// lambdaCombNode is one of the bounded higher-order combinators `map`,
// `filter`, `reduce`. The lambda is NOT a first-class value: it can only appear
// here, is applied exactly once per element of an already-materialized finite
// slice, and cannot reference itself — so no fixpoint is constructible and the
// language stays total (terminating). `init` is non-nil only for `reduce`.
type lambdaCombNode struct {
	name   string // "map" | "filter" | "reduce"
	coll   node
	init   node // reduce only
	params []string
	body   node
}

func (litBool) exprNode()         {}
func (litInt) exprNode()          {}
func (litFloat) exprNode()        {}
func (litString) exprNode()       {}
func (pathNode) exprNode()        {}
func (*unaryNode) exprNode()      {}
func (*binaryNode) exprNode()     {}
func (*funcCallNode) exprNode()   {}
func (*indexNode) exprNode()      {}
func (*lambdaCombNode) exprNode() {}

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
// expression (e.g. `(((...)))` from an untrusted .bot under multitenant
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
	return p.parsePostfix()
}

// parsePostfix parses a primary followed by zero or more `[index]` subscripts
// and `.field` accesses, so `a[0]`, `m["k"]`, `outputs.x[i]`, `keys(m)[0]`, and
// `people[0].name` all chain left-to-right. A `.field` after a subscript is
// sugar for `["field"]` (string-keyed map access). The leading dotted path of a
// bare identifier is still consumed by parsePrimary; postfix only handles the
// `.field` that follows a subscript, where parsePrimary has already returned.
func (p *parser) parsePostfix() (node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == tokLBracket || p.cur.kind == tokDot {
		if err := p.enter(); err != nil {
			return nil, err
		}
		if p.cur.kind == tokDot {
			p.advance() // consume '.'
			if p.cur.kind != tokIdent {
				p.leave()
				return nil, fmt.Errorf("expr: expected identifier after '.', got %s", p.cur.value)
			}
			n = &indexNode{recv: n, index: litString{v: p.cur.value}}
			p.advance() // consume IDENT
			p.leave()
			continue
		}
		p.advance() // consume '['
		idx, err := p.parseExpr()
		if err != nil {
			p.leave()
			return nil, err
		}
		if p.cur.kind != tokRBracket {
			p.leave()
			return nil, fmt.Errorf("expr: expected ']' got %s", p.cur.value)
		}
		p.advance() // consume ']'
		p.leave()
		n = &indexNode{recv: n, index: idx}
	}
	return n, nil
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
			if isLambdaComb(ns) {
				return p.parseLambdaComb(ns)
			}
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

// isLambdaComb reports whether name is one of the bounded higher-order
// combinators whose final argument is a `=>` lambda (not a normal expression).
func isLambdaComb(name string) bool {
	return name == "map" || name == "filter" || name == "reduce"
}

// parseLambdaComb parses `map(coll, p => body)`, `filter(coll, p => body)`, or
// `reduce(coll, init, (acc, x) => body)`. Invoked with cur on the opening `(`.
func (p *parser) parseLambdaComb(name string) (node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	p.advance() // consume '('
	coll, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.cur.kind != tokComma {
		return nil, fmt.Errorf("expr: %s() expects a comma after the collection, got %s", name, p.cur.value)
	}
	p.advance()
	var initNode node
	if name == "reduce" {
		initNode, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.cur.kind != tokComma {
			return nil, fmt.Errorf("expr: reduce() expects a comma after the initial value, got %s", p.cur.value)
		}
		p.advance()
	}
	params, body, err := p.parseLambda(name)
	if err != nil {
		return nil, err
	}
	want := 1
	if name == "reduce" {
		want = 2
	}
	if len(params) != want {
		return nil, fmt.Errorf("expr: %s() lambda takes %d parameter(s), got %d", name, want, len(params))
	}
	if p.cur.kind != tokRParen {
		return nil, fmt.Errorf("expr: expected ')' to close %s(), got %s", name, p.cur.value)
	}
	p.advance() // consume ')'
	return &lambdaCombNode{name: name, coll: coll, init: initNode, params: params, body: body}, nil
}

// parseLambda parses `p => body` or `(p1, p2) => body`. Parameter names that
// collide with a reserved expression namespace are rejected so a body's
// `outputs`/`vars`/… reference can never be silently shadowed.
func (p *parser) parseLambda(ctxName string) ([]string, node, error) {
	var params []string
	switch p.cur.kind {
	case tokLParen:
		p.advance()
		for p.cur.kind == tokIdent {
			params = append(params, p.cur.value)
			p.advance()
			if p.cur.kind == tokComma {
				p.advance()
				continue
			}
			break
		}
		if p.cur.kind != tokRParen {
			return nil, nil, fmt.Errorf("expr: expected ')' after %s() lambda parameters, got %s", ctxName, p.cur.value)
		}
		p.advance()
	case tokIdent:
		params = append(params, p.cur.value)
		p.advance()
	default:
		return nil, nil, fmt.Errorf("expr: %s() expects a lambda (e.g. x => x.field), got %s", ctxName, p.cur.value)
	}
	if p.cur.kind != tokArrow {
		return nil, nil, fmt.Errorf("expr: %s() lambda expects '=>' after parameters, got %s", ctxName, p.cur.value)
	}
	p.advance()
	for _, pm := range params {
		if evalNamespaces[pm] {
			return nil, nil, fmt.Errorf("expr: lambda parameter %q collides with the reserved namespace %q", pm, pm)
		}
	}
	body, err := p.parseExpr()
	if err != nil {
		return nil, nil, err
	}
	return params, body, nil
}

// ---------------------------------------------------------------------------
// Evaluator
// ---------------------------------------------------------------------------

func evalNode(n node, st *evalState) (any, error) {
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
		return resolvePath(v.namespace, v.path, st)
	case *unaryNode:
		return evalUnary(v, st)
	case *binaryNode:
		return evalBinary(v, st)
	case *funcCallNode:
		return evalFuncCall(v, st)
	case *indexNode:
		return evalIndex(v, st)
	case *lambdaCombNode:
		return evalLambdaComb(v, st)
	}
	return nil, fmt.Errorf("expr: unknown node type %T", n)
}

func resolvePath(namespace string, path []string, st *evalState) (any, error) {
	// Lambda-bound locals shadow every namespace: a `map(arr, x => x.f)` body
	// resolves `x` from the active frame, never from the Context.
	if v, ok := st.lookupLocal(namespace); ok {
		return descendPath(v, path), nil
	}
	ctx := st.ctx
	if ctx == nil {
		return nil, nil
	}
	switch namespace {
	case "vars":
		if ctx.Vars == nil {
			return nil, nil
		}
		return ctx.Vars(path), nil
	case "input":
		if ctx.Input == nil {
			return nil, nil
		}
		return ctx.Input(path), nil
	case "outputs":
		if ctx.Outputs == nil {
			return nil, nil
		}
		return ctx.Outputs(path), nil
	case "artifacts":
		if ctx.Artifacts == nil {
			return nil, nil
		}
		return ctx.Artifacts(path), nil
	case "loop":
		if ctx.Loop == nil {
			return nil, nil
		}
		return ctx.Loop(path), nil
	case "run":
		if ctx.Run == nil {
			return nil, nil
		}
		return ctx.Run(path), nil
	}
	// Bare identifier (e.g. `approved` in a `when` clause): interpret as a
	// field of the implicit `input` namespace. This matches the legacy
	// `when <field>` ergonomics where the predicate references a field of
	// the source node's output.
	if ctx.Input != nil {
		fullPath := append([]string{namespace}, path...)
		return ctx.Input(fullPath), nil
	}
	return nil, fmt.Errorf("expr: unknown namespace %q", namespace)
}

func evalUnary(n *unaryNode, st *evalState) (any, error) {
	v, err := evalNode(n.child, st)
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

func evalBinary(n *binaryNode, st *evalState) (any, error) {
	switch n.op {
	case "&&":
		l, err := evalNode(n.left, st)
		if err != nil {
			return nil, err
		}
		if !truthy(l) {
			return false, nil
		}
		r, err := evalNode(n.right, st)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	case "||":
		l, err := evalNode(n.left, st)
		if err != nil {
			return nil, err
		}
		if truthy(l) {
			return true, nil
		}
		r, err := evalNode(n.right, st)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	}

	l, err := evalNode(n.left, st)
	if err != nil {
		return nil, err
	}
	r, err := evalNode(n.right, st)
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

func equals(a, b any) bool {
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

func compare(op string, a, b any) (bool, error) {
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

func arith(op string, a, b any) (any, error) {
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
			if ai == math.MinInt64 && bi == -1 {
				return nil, fmt.Errorf("expr: integer division overflow (%d / %d)", ai, bi)
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

func toInt(v any) (int64, bool) {
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

func toFloat(v any) (float64, bool) {
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
	// MinInt64 * -1 overflows, but Go's MinInt64 / -1 == MinInt64 spec quirk
	// fools the r/b != a check below, so guard that case explicitly.
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, false
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
var builtins = map[string]func(args []any) (any, error){
	"length":   builtinLength,
	"concat":   builtinConcat,
	"unique":   builtinUnique,
	"contains": builtinContains,
	"join":     builtinJoin,
	"tail":     builtinTail,
	"if":       builtinIf,
	"sort":     builtinSort,
	"keys":     builtinKeys,
	"values":   builtinValues,
	"slice":    builtinSlice,
	"sum":      builtinSum,
	"min":      builtinMin,
	"max":      builtinMax,
	"flatten":  builtinFlatten,
	"floor":    builtinFloor,
	"round":    builtinRound,
}

func evalFuncCall(n *funcCallNode, st *evalState) (any, error) {
	// Special form: if(cond, then, else) short-circuits. Only the
	// selected branch is evaluated, so the un-taken branch can safely
	// contain expressions that would otherwise trip a divide-by-zero
	// or similar arithmetic trap. The 2026-05-20 dogfood hit this
	// with `if(n > 0, total / n, 0)` — pre-special-form the `total/n`
	// arm evaluated eagerly when n=0 and crashed the compute node.
	// Ticket a3a9757b on the native board.
	if n.name == "if" && len(n.args) == 3 {
		condVal, err := evalNode(n.args[0], st)
		if err != nil {
			return nil, err
		}
		if truthy(condVal) {
			return evalNode(n.args[1], st)
		}
		return evalNode(n.args[2], st)
	}

	fn, ok := builtins[n.name]
	if !ok {
		// Belt-and-suspenders: parser already rejects unknown names, but
		// keep the runtime check in case an AST is constructed by other
		// means in the future.
		return nil, fmt.Errorf("expr: unknown function %q", n.name)
	}
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := evalNode(a, st)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fn(args)
}

func builtinLength(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: length() takes 1 argument, got %d", len(args))
	}
	switch v := args[0].(type) {
	case nil:
		return int64(0), nil
	case []any:
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
	// `when` edge condition — surfaced by an `.bot` workflow whose
	// streak_check edge guarded fix routing on length(blockers) > 0.
	if rv := reflect.ValueOf(args[0]); rv.IsValid() {
		switch rv.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			return int64(rv.Len()), nil
		}
	}
	return nil, fmt.Errorf("expr: length() expects array, string, or map, got %T", args[0])
}

func builtinConcat(args []any) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("expr: concat() takes at least 1 argument")
	}
	out := make([]any, 0)
	for i, a := range args {
		if a == nil {
			continue
		}
		arr, ok := a.([]any)
		if !ok {
			return nil, fmt.Errorf("expr: concat() argument %d is %T, want array", i+1, a)
		}
		out = append(out, arr...)
	}
	return out, nil
}

func builtinUnique(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: unique() takes 1 argument, got %d", len(args))
	}
	if args[0] == nil {
		return []any{}, nil
	}
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("expr: unique() expects array, got %T", args[0])
	}
	// Stringify for equality so heterogeneous arrays (which the runtime
	// cheerfully produces from JSON) don't blow up on map/slice keys.
	seen := make(map[string]struct{}, len(arr))
	out := make([]any, 0, len(arr))
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

func builtinContains(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("expr: contains() takes 2 arguments, got %d", len(args))
	}
	if args[0] == nil {
		return false, nil
	}
	arr, ok := args[0].([]any)
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

// builtinTail returns the last n elements of an array (like Unix `tail`).
// Its purpose is to BOUND accumulators that grow every loop iteration — the
// canonical case is a review loop's `cumulative_scanned_areas`, which is
// `unique(concat(prev, new))` and is fed verbatim into every reviewer's
// input prompt. Unbounded, it eventually overflows the model's context
// window (observed: a whole-repo whole_improve_loop run died at review pass
// 11 with reviewer_gpt context_length_exceeded; generation.go's reactive
// compaction can shrink conversation history but not one oversized INPUT
// field). Wrapping the accumulator as `tail(unique(concat(...)), N)` keeps a
// bounded, most-recent window. n <= 0 yields an empty array; n >= len
// returns the array unchanged. Order is preserved.
func builtinTail(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("expr: tail() takes 2 arguments (array, n), got %d", len(args))
	}
	n, ok := toInt(args[1])
	if !ok {
		return nil, fmt.Errorf("expr: tail() expects a number as second argument, got %T", args[1])
	}
	if args[0] == nil {
		return []any{}, nil
	}
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("expr: tail() expects array as first argument, got %T", args[0])
	}
	if n <= 0 {
		return []any{}, nil
	}
	if n >= int64(len(arr)) {
		return arr, nil
	}
	// Copy the last n elements so the result never aliases the input slice.
	out := make([]any, n)
	copy(out, arr[int64(len(arr))-n:])
	return out, nil
}

// builtinIf is the fallback for direct calls; the real evaluator
// special-cases "if" in evalFuncCall to skip the un-taken branch.
// Kept here so the function name still resolves in builtin-lookup
// paths that pre-date the special form.
//
// if(cond, then, else) returns then when cond is truthy, else
// otherwise. As of ticket a3a9757b the un-taken branch is NOT
// evaluated — `if(n > 0, total / n, 0)` is safe when n == 0.
func builtinIf(args []any) (any, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("expr: if() takes 3 arguments (cond, then, else), got %d", len(args))
	}
	if truthy(args[0]) {
		return args[1], nil
	}
	return args[2], nil
}

func builtinJoin(args []any) (any, error) {
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
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("expr: join() expects array as first argument, got %T", args[0])
	}
	parts := make([]string, len(arr))
	for i, v := range arr {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return strings.Join(parts, sep), nil
}

// builtinSort returns a new array sorted ascending. All-numeric arrays sort
// numerically; all-string arrays lexicographically; mixed/other arrays sort by
// their %v stringification. Ordering is deterministic for prompt-cache
// stability. The input is never mutated.
func builtinSort(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: sort() takes 1 argument, got %d", len(args))
	}
	arr, err := toElemSlice(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: sort() expects array, %w", err)
	}
	out := make([]any, len(arr))
	copy(out, arr)
	allNum, allStr := true, true
	for _, v := range out {
		if _, ok := toFloat(v); !ok {
			allNum = false
		}
		if _, ok := v.(string); !ok {
			allStr = false
		}
	}
	switch {
	case allNum:
		sort.SliceStable(out, func(i, j int) bool {
			a, _ := toFloat(out[i])
			b, _ := toFloat(out[j])
			return a < b
		})
	case allStr:
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].(string) < out[j].(string)
		})
	default:
		sort.SliceStable(out, func(i, j int) bool {
			return fmt.Sprintf("%v", out[i]) < fmt.Sprintf("%v", out[j])
		})
	}
	return out, nil
}

// builtinKeys returns a map's keys, sorted ascending (deterministic).
func builtinKeys(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: keys() takes 1 argument, got %d", len(args))
	}
	m, err := toStringMap(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: keys() expects a map, %w", err)
	}
	out := make([]any, 0, len(m))
	for _, k := range sortedMapKeys(m) {
		out = append(out, k)
	}
	return out, nil
}

// builtinValues returns a map's values ordered by sorted key (deterministic).
func builtinValues(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: values() takes 1 argument, got %d", len(args))
	}
	m, err := toStringMap(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: values() expects a map, %w", err)
	}
	out := make([]any, 0, len(m))
	for _, k := range sortedMapKeys(m) {
		out = append(out, m[k])
	}
	return out, nil
}

// builtinSlice returns arr[start:end) with bounds clamped to [0, len]. A
// negative index counts from the end (Python-style); end defaults past the last
// element when out of range. The result never aliases the input.
func builtinSlice(args []any) (any, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("expr: slice() takes 3 arguments (array, start, end), got %d", len(args))
	}
	arr, err := toElemSlice(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: slice() expects array as first argument, %w", err)
	}
	n := int64(len(arr))
	start, ok := toInt(args[1])
	if !ok {
		return nil, fmt.Errorf("expr: slice() start must be an integer, got %T", args[1])
	}
	end, ok := toInt(args[2])
	if !ok {
		return nil, fmt.Errorf("expr: slice() end must be an integer, got %T", args[2])
	}
	clamp := func(i int64) int64 {
		if i < 0 {
			i += n
		}
		if i < 0 {
			return 0
		}
		if i > n {
			return n
		}
		return i
	}
	s, e := clamp(start), clamp(end)
	if e < s {
		e = s
	}
	out := make([]any, e-s)
	copy(out, arr[s:e])
	return out, nil
}

// builtinSum totals a numeric array. All-integer inputs stay int64 (with
// overflow detection); a fractional element promotes the sum to float64. Empty
// is int64(0).
func builtinSum(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: sum() takes 1 argument, got %d", len(args))
	}
	arr, err := toElemSlice(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: sum() expects array, %w", err)
	}
	var isum int64
	var fsum float64
	isFloat := false
	for _, v := range arr {
		if !isFloat {
			if iv, ok := toInt(v); ok {
				if r, ok := addCheckedInt64(isum, iv); ok {
					isum = r
					continue
				}
				return nil, fmt.Errorf("expr: sum() integer overflow")
			}
			// First fractional value: promote accumulated total to float.
			isFloat = true
			fsum = float64(isum)
		}
		fv, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("expr: sum() expects numeric elements, got %T", v)
		}
		fsum += fv
	}
	if isFloat {
		return fsum, nil
	}
	return isum, nil
}

// builtinMin / builtinMax return the smallest / largest numeric element, or nil
// for an empty array.
func builtinMin(args []any) (any, error) { return minMax(args, "min") }
func builtinMax(args []any) (any, error) { return minMax(args, "max") }

func minMax(args []any, which string) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: %s() takes 1 argument, got %d", which, len(args))
	}
	arr, err := toElemSlice(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: %s() expects array, %w", which, err)
	}
	if len(arr) == 0 {
		return nil, nil
	}
	var best any
	var bestF float64
	for i, v := range arr {
		fv, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("expr: %s() expects numeric elements, got %T", which, v)
		}
		if i == 0 || (which == "min" && fv < bestF) || (which == "max" && fv > bestF) {
			best, bestF = v, fv
		}
	}
	return best, nil
}

// builtinFloor / builtinRound turn a number into the int64 an `int` field
// expects — floor towards negative infinity, round half away from zero — so
// a division's rounding is written in the expression rather than guessed by
// the engine when the compute output meets its schema. An integer passes
// through; a non-number, or a float with no finite integer (NaN, ±Inf,
// beyond the int64 range), is an error.
func builtinFloor(args []any) (any, error) { return numberToInt(args, "floor", math.Floor) }
func builtinRound(args []any) (any, error) { return numberToInt(args, "round", math.Round) }

func numberToInt(args []any, name string, fn func(float64) float64) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: %s() takes 1 argument, got %d", name, len(args))
	}
	if n, ok := toInt(args[0]); ok {
		return n, nil
	}
	f, ok := toFloat(args[0])
	if !ok {
		return nil, fmt.Errorf("expr: %s() expects a number, got %T", name, args[0])
	}
	r := fn(f)
	if math.IsNaN(r) || math.IsInf(r, 0) || r < math.MinInt64 || r >= math.MaxInt64 {
		return nil, fmt.Errorf("expr: %s(%v) is not a finite integer", name, f)
	}
	return int64(r), nil
}

// builtinFlatten concatenates one level of nesting: each array element is
// spliced in, each non-array element is kept as-is. Deeper nesting is left
// intact (deep flatten is excluded to keep output size bounded by input).
func builtinFlatten(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expr: flatten() takes 1 argument, got %d", len(args))
	}
	arr, err := toElemSlice(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: flatten() expects array, %w", err)
	}
	out := make([]any, 0, len(arr))
	for _, v := range arr {
		if inner, ok := v.([]any); ok {
			out = append(out, inner...)
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// toStringMap normalizes a value to map[string]interface{} (nil → empty),
// converting concrete string-keyed maps via reflection.
func toStringMap(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
		out := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			out[k.String()] = rv.MapIndex(k).Interface()
		}
		return out, nil
	}
	return nil, fmt.Errorf("got %T", v)
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// descendPath walks a dotted field path into a (possibly nested) map value,
// returning nil at the first non-map segment or missing key. Used to resolve
// `x.field` against a lambda-bound local `x`.
func descendPath(v any, path []string) any {
	for _, seg := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[seg]
	}
	return v
}

// evalIndex resolves `recv[index]`. Out-of-bounds array indices and missing map
// keys yield nil (consistent with absent paths); a wrong-typed subscript on a
// present collection, or indexing a scalar, is a loud error.
func evalIndex(n *indexNode, st *evalState) (any, error) {
	recv, err := evalNode(n.recv, st)
	if err != nil {
		return nil, err
	}
	idx, err := evalNode(n.index, st)
	if err != nil {
		return nil, err
	}
	if recv == nil {
		return nil, nil
	}
	switch c := recv.(type) {
	case []any:
		i, ok := toInt(idx)
		if !ok {
			return nil, fmt.Errorf("expr: array index must be an integer, got %T", idx)
		}
		if i < 0 || i >= int64(len(c)) {
			return nil, nil
		}
		return c[i], nil
	case map[string]any:
		key, ok := idx.(string)
		if !ok {
			return nil, fmt.Errorf("expr: map key must be a string, got %T", idx)
		}
		return c[key], nil
	}
	// Reflection fallback for concrete slice/array/map types produced by stubs
	// or backend-specific output shapes (mirrors builtinLength).
	rv := reflect.ValueOf(recv)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		i, ok := toInt(idx)
		if !ok {
			return nil, fmt.Errorf("expr: array index must be an integer, got %T", idx)
		}
		if i < 0 || i >= int64(rv.Len()) {
			return nil, nil
		}
		return rv.Index(int(i)).Interface(), nil
	case reflect.Map:
		key, ok := idx.(string)
		if !ok {
			return nil, fmt.Errorf("expr: map key must be a string, got %T", idx)
		}
		mv := rv.MapIndex(reflect.ValueOf(key))
		if !mv.IsValid() {
			return nil, nil
		}
		return mv.Interface(), nil
	}
	return nil, fmt.Errorf("expr: cannot index %T", recv)
}

// evalLambdaComb evaluates a bounded higher-order combinator. The loop count is
// fixed before iteration to the materialized slice length; the body is applied
// once per element under a fresh local frame; each element debits the visit
// budget. No element can extend the collection, so the construct is total.
func evalLambdaComb(n *lambdaCombNode, st *evalState) (any, error) {
	collVal, err := evalNode(n.coll, st)
	if err != nil {
		return nil, err
	}
	arr, err := toElemSlice(collVal)
	if err != nil {
		return nil, fmt.Errorf("expr: %s() expects an array as its collection: %w", n.name, err)
	}
	switch n.name {
	case "map":
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			if err := st.consume(); err != nil {
				return nil, err
			}
			v, err := st.evalBody(n.params[0], el, n.body)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case "filter":
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			if err := st.consume(); err != nil {
				return nil, err
			}
			v, err := st.evalBody(n.params[0], el, n.body)
			if err != nil {
				return nil, err
			}
			if truthy(v) {
				out = append(out, el)
			}
		}
		return out, nil
	case "reduce":
		acc, err := evalNode(n.init, st)
		if err != nil {
			return nil, err
		}
		for _, el := range arr {
			if err := st.consume(); err != nil {
				return nil, err
			}
			frame := map[string]any{n.params[0]: acc, n.params[1]: el}
			st.locals = append(st.locals, frame)
			acc, err = evalNode(n.body, st)
			st.locals = st.locals[:len(st.locals)-1]
			if err != nil {
				return nil, err
			}
		}
		return acc, nil
	}
	return nil, fmt.Errorf("expr: unknown combinator %q", n.name)
}

// toElemSlice normalizes a value to []interface{} for combinators/helpers. A
// nil value is an empty collection; a concrete slice/array is converted via
// reflection; anything else is an error.
func toElemSlice(v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	if arr, ok := v.([]any); ok {
		return arr, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	}
	return nil, fmt.Errorf("got %T", v)
}

// walkRefs surfaces the namespace.path references an expression depends on.
// Lambda-bound parameters are NOT references (they are locals), so the body of
// a combinator is walked with those parameter names excluded.
func walkRefs(n node, fn func(Ref)) {
	walkRefsBound(n, nil, fn)
}

func walkRefsBound(n node, bound map[string]bool, fn func(Ref)) {
	switch v := n.(type) {
	case pathNode:
		if bound[v.namespace] {
			return // lambda-bound local, not an external reference
		}
		fn(Ref{Namespace: v.namespace, Path: append([]string(nil), v.path...)})
	case *unaryNode:
		walkRefsBound(v.child, bound, fn)
	case *binaryNode:
		walkRefsBound(v.left, bound, fn)
		walkRefsBound(v.right, bound, fn)
	case *funcCallNode:
		for _, a := range v.args {
			walkRefsBound(a, bound, fn)
		}
	case *indexNode:
		walkRefsBound(v.recv, bound, fn)
		walkRefsBound(v.index, bound, fn)
	case *lambdaCombNode:
		walkRefsBound(v.coll, bound, fn)
		if v.init != nil {
			walkRefsBound(v.init, bound, fn)
		}
		nb := make(map[string]bool, len(bound)+len(v.params))
		for k := range bound {
			nb[k] = true
		}
		for _, p := range v.params {
			nb[p] = true
		}
		walkRefsBound(v.body, nb, fn)
	}
}

// IsBoolAlgebraOverRefs reports whether the AST is a pure boolean
// combination of path references: refs combined by "!", "&&", "||" and
// parentheses, nothing else — no literals, comparisons, arithmetic,
// indexing or combinators. Consumers whose evaluation must be STRICT
// (a policy contract where an absent field must never coerce into a
// verdict) restrict their grammar to this shape: every leaf is then a
// ref they can pre-resolve and type-check individually, which the
// truthy coercion inside "!"/"&&"/"||" would otherwise defeat.
func (a *AST) IsBoolAlgebraOverRefs() bool {
	if a == nil || a.root == nil {
		return false
	}
	return isBoolAlgebraNode(a.root)
}

func isBoolAlgebraNode(n node) bool {
	switch t := n.(type) {
	case pathNode:
		return true
	case *unaryNode:
		return t.op == "!" && isBoolAlgebraNode(t.child)
	case *binaryNode:
		return (t.op == "&&" || t.op == "||") && isBoolAlgebraNode(t.left) && isBoolAlgebraNode(t.right)
	default:
		return false
	}
}
