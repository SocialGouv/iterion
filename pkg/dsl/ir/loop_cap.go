package ir

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

func (c *compiler) compileLoopCap(loop *Loop, edge *ast.Edge) {
	src := strings.TrimSpace(loop.MaxIterationsExpr)
	if src == "" {
		return
	}
	bad := func(err error) { c.errorfOnEdge(DiagBadTemplateRef, edge, "loop %q cap %q: %v", loop.Name, src, err) }
	if n, err := strconv.Atoi(src); err == nil {
		loop.MaxIterations = n
		loop.MaxIterationsExpr = ""
		return
	}
	if strings.Contains(src, "{{") {
		refs, err := ParseRefs(src)
		if err != nil {
			bad(err)
			return
		}
		if len(refs) != 1 || refs[0].Raw != src {
			bad(fmt.Errorf("a template cap must be one integer reference"))
			return
		}
		if refs[0].Kind == RefLiteralOpen {
			bad(fmt.Errorf("cap has type string; expected an integer"))
			return
		}
		loop.MaxIterationsExprRefs = refs
		return
	}
	parsed, err := expr.Parse(src)
	if err != nil {
		bad(err)
		return
	}
	loop.MaxIterationsAST = parsed
}

func (c *compiler) validateLoopCapExpressions(w *Workflow) {
	predecessors := buildPredecessors(w)
	for _, edge := range w.Edges {
		loop := w.Loops[edge.LoopName]
		if loop == nil || loop.MaxIterationsExpr == "" {
			continue
		}
		loc := fmt.Sprintf("loop %q cap %q", loop.Name, loop.MaxIterationsExpr)
		rc := refContext{NodeID: edge.From, IncludeSelf: true, EdgeID: edgeID(edge.From, edge.To), Span: c.edgeSpans[edge], Location: loc}
		var refs []*Ref
		var snapshot *expr.Snapshot
		if loop.MaxIterationsAST != nil {
			snapshot = expr.ToSnapshot(loop.MaxIterationsAST)
			for _, r := range loop.MaxIterationsAST.Refs() {
				if r.Namespace != "vars" && r.Namespace != "outputs" {
					c.refErrorf(rc, DiagBadTemplateRef, "%s: expressions may reference vars and outputs, got %q", loc, r.Namespace)
					continue
				}
				refs = append(refs, refFromExpr(r))
			}
		} else {
			refs = loop.MaxIterationsExprRefs
			if len(refs) == 1 {
				snapshot = &expr.Snapshot{Kind: expr.SnapPath, Namespace: refs[0].Kind.String(), Path: refs[0].Path}
			}
		}
		for _, ref := range refs {
			rc.Ref = ref
			switch ref.Kind {
			case RefVars:
				c.validateVarsRef(w, rc)
			case RefOutputs:
				c.validateOutputsRef(w, rc, predecessors)
			default:
				c.refErrorf(rc, DiagBadTemplateRef, "%s: a cap must reference vars or outputs", loc)
			}
		}
		typ := loopCapType(exprEnv{w: w}, snapshot)
		if typ.known && typ.t != FieldTypeInt {
			c.refErrorf(rc, DiagBadTemplateRef, "%s: cap has type %s; expected an integer", loc, typ.t)
		}
	}
}

// Retain the conservative expression inference, but propagate definite operand
// failures through arithmetic so a string variable minus 1 is refused up front.
func loopCapType(env exprEnv, n *expr.Snapshot) inferredType {
	if n == nil {
		return unknownType
	}
	if n.Kind == expr.SnapBinary && len(n.Children) == 2 {
		switch n.Op {
		case "+", "-", "*", "/", "%":
			a, b := loopCapType(env, n.Children[0]), loopCapType(env, n.Children[1])
			for _, t := range []inferredType{a, b} {
				if t.known && t.t != FieldTypeInt && t.t != FieldTypeFloat {
					return t
				}
			}
			if a.known && b.known {
				if a.t == FieldTypeFloat || b.t == FieldTypeFloat {
					return knownT(FieldTypeFloat)
				}
				return knownT(FieldTypeInt)
			}
		}
	}
	return env.inferType(n)
}
