package runtime

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// evalComputeExpr runs a single compute-node AST under a recover()
// guard so a panic in the expression evaluator (e.g. an unguarded
// type assertion in a future evalNode extension) surfaces as a
// structured error and terminates the node cleanly, instead of
// crashing the daemon mid-run.
func evalComputeExpr(ast *expr.AST, exprCtx *expr.Context) (val any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("compute expression panicked: %v", r)
		}
	}()
	return ast.Eval(exprCtx)
}

// exprContext builds an expression context against the run's trunk scope
// (rs.vars / rs.outputs / rs.artifacts). For a node running inside a fan-out
// branch — which must see branch-local outputs not yet merged into rs — use
// exprContextScoped with the branch's merged scope.
func (e *Engine) exprContext(rs *runState, input map[string]any) *expr.Context {
	return e.exprContextScoped(rs, rs.scope(), input)
}

// exprContextScoped builds an expression context against an explicit
// resolveScope for vars/outputs/artifacts, while loop/run namespaces still
// resolve against rs (read-only, parent-owned even inside a branch). The trunk
// case (sc == rs.scope()) is behaviorally identical to the old exprContext.
func (e *Engine) exprContextScoped(rs *runState, sc resolveScope, input map[string]any) *expr.Context {
	mapResolver := func(m map[string]any) func([]string) any {
		return func(path []string) any {
			if len(path) == 0 {
				return m
			}
			return drillPath(m, path)
		}
	}
	keyedMapResolver := func(byKey map[string]map[string]any) func([]string) any {
		return func(path []string) any {
			if len(path) == 0 {
				return byKey
			}
			return drillPath(byKey[path[0]], path[1:])
		}
	}
	loopResolver := func(path []string) any {
		if len(path) < 2 {
			return nil
		}
		return e.resolveLoopPath(path, rs)
	}
	runResolver := func(path []string) any {
		if len(path) == 1 && path[0] == "id" {
			return rs.runID
		}
		return nil
	}
	return &expr.Context{
		Vars:      mapResolver(sc.vars),
		Input:     mapResolver(input),
		Outputs:   keyedMapResolver(sc.outputs),
		Artifacts: keyedMapResolver(sc.artifacts),
		Loop:      loopResolver,
		Run:       runResolver,
	}
}
