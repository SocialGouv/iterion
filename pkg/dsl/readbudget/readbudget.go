// Package readbudget resolves the read budget: how much one bot may ask its
// readers to record. Every reader of a bot spends it — the .bot lexer its
// records (pkg/dsl/parser), the YAML twin its values and comment lines
// (pkg/dsl/author), the expression parser the names, values, operators and
// brackets of an expression (pkg/dsl/expr), which live inside one string the
// lexer counts as a single record. One resolver, one knob: the count the
// operator sets in ITERION_DSL_READ_BUDGET, the default otherwise, 0 removing
// the budget. A resolution level added later — a deployment setting above
// the process environment, the environment then a floor — lands here and
// reaches every reader at once, so no parallel knob can undercut it.
package readbudget

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Env names the operator's override of the read budget.
const Env = "ITERION_DSL_READ_BUDGET"

// Default is how much one bot may ask its readers to record: 2^20 records
// of its .bot files together — every word, value, punctuation mark, prompt
// line (blank or not), line end, indentation step and the end of each file
// is one, a comment two — 2^20 of a YAML twin's reading, a value one and a
// comment line two, and 2^20 tokens of its expressions together, a name,
// value, operator or bracket one and the end of an expression one. The size
// cap bounds each file's bytes; the budget bounds what they turn into, which
// a dense shape makes hundreds of bytes of memory per byte of source (one
// flat list of one-letter names, a line of commas, a run of comment lines,
// one expression of a million operators). Each reader spends it as it reads,
// so a bot past it is refused before the work it asks for, not after.
const Default = 1 << 20

// Read is the budget in force: Default, or the count the operator sets in
// ITERION_DSL_READ_BUDGET where the source is read — 0 removes the budget
// (math.MaxInt). A value that is not a count (a unit, a decimal, a number
// past the int range, a negative one) keeps the default, and the process
// says so once on stderr,
// naming the value and the budget it applied — as an error, which no
// ITERION_LOG_LEVEL silences: the operator meant something, and silence
// would replace it unseen.
func Read() int {
	v := strings.TrimSpace(os.Getenv(Env))
	if v == "" {
		return Default
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil || n < 0:
		if _, warned := invalidSaid.LoadOrStore(v, true); !warned {
			iterlog.NewFromEnv(os.Stderr).Error("dsl: invalid %s=%q (a count of records, 0 for none); using the default %d", Env, v, Default)
		}
		return Default
	case n == 0:
		return math.MaxInt
	}
	return n
}

// invalidSaid holds the invalid values already said, once each per process:
// every source read would otherwise say it again.
var invalidSaid sync.Map
