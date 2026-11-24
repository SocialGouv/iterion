package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// compileLoopSize compiles the loop clause's work signal (`as name(3)
// sized_by "outputs.probe.pending"`) into the AST the runtime evaluates
// at each budget mark and each crossing. The surface is the expression
// one, like the un-braced quoted cap: no template braces, vars/outputs
// references only (validateLoopSizeExpressions), and a number at run
// time — a signal that resolves to nothing simply leaves the pricing
// unscaled, so an absent reading never invents a decline (#2677).
func (c *compiler) compileLoopSize(loop *Loop, edge *ast.Edge) {
	src := strings.TrimSpace(loop.SizedByExpr)
	if src == "" {
		return
	}
	bad := func(err error) {
		c.errorfOnEdge(DiagInvalidLoopSizedBy, edge, "loop %q sized_by %q: %v", loop.Name, expr.Excerpt(src), err)
	}
	if strings.Contains(src, "{{") {
		bad(fmt.Errorf("the signal is a bare expression — drop the template braces"))
		return
	}
	before := c.exprBudget.Spent()
	parsed, err := expr.ParseOn(src, c.exprBudget)
	loop.sizeRecords = c.exprBudget.Spent() - before
	if err != nil {
		if pastReadBudget(err) {
			c.errorfOnEdge(DiagExprPastReadBudget, edge, "loop %q sized_by: %v", loop.Name, err)
			return
		}
		bad(err)
		return
	}
	loop.SizedByAST = parsed
}

// validateLoopSizeExpressions holds every loop's work signal to the same
// references as the cap's: vars and outputs, resolved per edge carrying
// the loop, with a known-type refusal when the signal is provably not
// numeric. A signal whose type is unknown passes — the runtime treats an
// unreadable or non-numeric reading as "no signal" and prices unscaled,
// so the check refuses only what is KNOWN to be wrong.
func (c *compiler) validateLoopSizeExpressions(w *Workflow) {
	predecessors := newPredecessorIndex(w)
	for _, edge := range w.Edges {
		loop := w.Loops[edge.LoopName]
		if loop == nil || loop.SizedByAST == nil {
			continue
		}
		loc := fmt.Sprintf("loop %q sized_by %q", loop.Name, expr.Excerpt(loop.SizedByExpr))
		rc := refContext{NodeID: edge.From, IncludeSelf: true, EdgeID: edgeID(edge.From, edge.To), Span: c.edgeSpans[edge], Location: loc}
		for _, r := range loop.SizedByAST.Refs() {
			if r.Namespace != "vars" && r.Namespace != "outputs" {
				c.refErrorf(rc, DiagInvalidLoopSizedBy, "%s: the signal may reference vars and outputs, got %q", loc, expr.Excerpt(r.Namespace))
				continue
			}
			ref := refFromExpr(r)
			rc.Ref = ref
			switch ref.Kind {
			case RefVars:
				c.validateVarsRef(w, rc)
			case RefOutputs:
				c.validateOutputsRef(w, rc, predecessors)
			}
		}
		typ := loopCapType(exprEnv{w: w}, expr.ToSnapshot(loop.SizedByAST))
		if typ.known && typ.t != FieldTypeInt && typ.t != FieldTypeFloat {
			c.refErrorf(rc, DiagInvalidLoopSizedBy, "%s: signal has type %s; expected a number", loc, typ.t)
		}
	}
}
