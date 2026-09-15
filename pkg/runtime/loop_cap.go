package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func (e *Engine) resolveLoopMaxChecked(loop *ir.Loop, rs *runState) (int, error) {
	base, err := e.resolveLoopMaxBase(loop, rs)
	src := loop.MaxIterationsExpr
	if src == "" {
		src = strconv.Itoa(loop.MaxIterations)
	}
	if err != nil {
		return 0, fmt.Errorf("loop %q cap %q: %w", loop.Name, src, err)
	}
	if extra := rs.loopOverrides[loop.Name]; extra > 0 {
		if extra > int(^uint(0)>>1)-base {
			return 0, fmt.Errorf("loop %q cap %q: adding the override overflows an integer", loop.Name, src)
		}
		base += extra
	}
	return base, nil
}

func (e *Engine) resolveLoopMaxBase(loop *ir.Loop, rs *runState) (int, error) {
	if loop.Unbounded {
		if loop.FuelCap > 0 {
			return loop.FuelCap, nil
		}
		if e.workflow.Budget != nil && e.workflow.Budget.MaxIterations > 0 {
			return e.workflow.Budget.MaxIterations, nil
		}
		return defaultUnboundedFuel, nil
	}
	var value any = loop.MaxIterations
	if loop.MaxIterationsAST != nil {
		var err error
		value, err = loop.MaxIterationsAST.Eval(e.exprContext(rs, nil))
		if err != nil {
			return 0, err
		}
	} else if loop.MaxIterationsExpr != "" {
		if len(loop.MaxIterationsExprRefs) != 1 {
			return 0, fmt.Errorf("expected one integer reference or a compiled expression")
		}
		value = e.resolveRef(loop.MaxIterationsExprRefs[0], rs.scope())
	}
	return loopCapInteger(value)
}

// A missing, fractional, negative or overflowing value must never silently
// decline a back-edge as if the author had supplied zero. Numeric strings remain
// supported for legacy dynamically typed outputs that already emitted them.
func loopCapInteger(value any) (int, error) {
	if num, ok := value.(json.Number); ok {
		if n, err := num.Int64(); err == nil {
			value = n
		} else {
			f, err := num.Float64()
			if err != nil {
				return 0, fmt.Errorf("invalid numeric cap %q", num)
			}
			value = f
		}
	}
	var f float64
	var floating bool
	switch v := value.(type) {
	case float64:
		f, floating = v, true
	case float32:
		f, floating = float64(v), true
	case int64:
		if int64(int(v)) != v {
			return 0, fmt.Errorf("integer cap overflows this platform")
		}
	}
	if floating && (math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f || f < 0 || f >= math.Ldexp(1, strconv.IntSize-1)) {
		return 0, fmt.Errorf("cap %v is not a representable non-negative integer", value)
	}
	n, ok := coerceToInt(value)
	if !ok || n < 0 {
		return 0, fmt.Errorf("cap %v (%T) is not a non-negative integer", value, value)
	}
	return n, nil
}
