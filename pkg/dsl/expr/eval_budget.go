package expr

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// EvalEnv names the operator's override of the evaluation work budget.
const EvalEnv = "ITERION_DSL_EVAL_BUDGET"

// defaultEvalBudget is how much work one evaluation may do: 2^24 units, a
// unit being one node evaluation or one byte of a string the evaluation
// builds. The read budget (C320) bounds what an expression's TEXT becomes at
// parse time and maxEvalVisits bounds a combinator's fan-out; neither sees
// the work a bounded fan-out multiplies — 100 000 visits each evaluating a
// body of thousands of nodes is hours of CPU, and a left-deep string `+`
// chain re-copies its prefix at every step while spending no visit (#2775).
// This is the third bound, on the work itself. A bound with no override is
// a defect: the operator's count raises it, 0 removes it.
const defaultEvalBudget = 1 << 24

// evalBudget is the work budget in force: defaultEvalBudget, or the count
// the operator sets in ITERION_DSL_EVAL_BUDGET where an expression is
// evaluated — 0 removes the budget (math.MaxInt). Same reading discipline
// as the read budget (pkg/dsl/readbudget): a value that is not a count
// keeps the default and the process says so once on stderr, as an error.
func evalBudget() int {
	v := strings.TrimSpace(os.Getenv(EvalEnv))
	if v == "" {
		return defaultEvalBudget
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil || n < 0:
		if _, warned := invalidEvalSaid.LoadOrStore(v, true); !warned {
			iterlog.NewFromEnv(os.Stderr).Error("expr: invalid %s=%q (a count of work units, 0 for none); using the default %d", EvalEnv, v, defaultEvalBudget)
		}
		return defaultEvalBudget
	case n == 0:
		return math.MaxInt
	}
	return n
}

// invalidEvalSaid holds the invalid values already said, once each per
// process: every evaluation would otherwise say it again.
var invalidEvalSaid sync.Map
