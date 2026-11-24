package parser

import (
	"fmt"
	"math"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// MaxReported is how many diagnostics of one code, errors and warnings
// apart, one reading of a source reports, and MaxReportedOfASeverity how
// many errors, and how many warnings, it reports in all. A dense shape
// draws one diagnostic per token — a line of commas in a list, a thousand
// anchors in a document — and each costs the reader more than the token
// did, the list being handed back whole; no author reads the thousandth of
// the same mistake, so past either cap a diagnostic is counted, not kept,
// and the last one kept of its code says how many more there were. Every
// code a reading meets keeps its first, past the second cap too: a reader
// that passes one code and refuses on another still sees each, however
// many of the first came before. A reading keeps at most 2000 errors and
// 2000 warnings and the first of each other code — some 1.5 MB of
// diagnostics answered whole. The operator who removes the read budget
// (ITERION_DSL_READ_BUDGET=0) removes both caps with it.
const (
	MaxReported            = 1000
	MaxReportedOfASeverity = 2000
)

// Report holds the diagnostics of one reading of a source: up to
// MaxReported of each code and severity and MaxReportedOfASeverity of each
// severity — the first of every code whatever the second — in the order
// they were added, and how many of each severity were added in all.
type Report struct {
	diags []Diagnostic
	// limit and total are the caps in force, read at the first Add
	// (reportLimits), and codes the tally of each code.
	limit, total int
	codes        map[reportKey]*codeCount
	added        [2]int // per severity (severityIndex): every Add, kept or not
	kept         [2]int // per severity: every Add kept
	// lastKept is the index in diags of the diagnostic the latest Add
	// kept, -1 when the latest Add was counted only.
	lastKept int
}

// reportKey is a code of one severity.
type reportKey struct {
	sev  int
	code DiagCode
}

// codeCount is one code's tally: every Add of it, the ones kept, and the
// index in diags of the last one kept.
type codeCount struct{ added, kept, last int }

// reportLimits are MaxReported and MaxReportedOfASeverity, or no cap at all
// once the operator removed the read budget.
func reportLimits() (perCode, perSeverity int) {
	if ReadBudget() == math.MaxInt {
		return math.MaxInt, math.MaxInt
	}
	return MaxReported, MaxReportedOfASeverity
}

// Bounded is d with its Message and Hint held to expr.MaxDiagnosticText —
// the one door every diagnostic that reaches a user passes, at the
// recording doors (Report.Add, the compiler's emit) and at the constructors
// that join no report (the YAML twin's docDiag, the unit loader's first
// refusal, the server's rewrite of a recorded message): whatever the site
// composed — an anchor name, an import path, a workflow name of any length
// — a message is a bounded reading of it, never the text whole (#2801).
func Bounded(d Diagnostic) Diagnostic {
	d.Message, d.Hint = expr.Clip(d.Message, expr.MaxDiagnosticText), expr.Clip(d.Hint, expr.MaxDiagnosticText)
	return d
}

// Add records d and reports whether it was kept. The message and the hint
// are held to expr.MaxDiagnosticText here — the one door every reader of a
// source records through (the parser, the YAML twin's speller and guard,
// the unit loader) — whatever the site composed.
func (r *Report) Add(d Diagnostic) bool {
	d = Bounded(d)
	if r.codes == nil {
		r.codes = map[reportKey]*codeCount{}
		r.limit, r.total = reportLimits()
	}
	s := severityIndex(d.Severity)
	r.added[s]++
	k := reportKey{s, d.Code}
	c := r.codes[k]
	if c == nil {
		c = &codeCount{}
		r.codes[k] = c
	}
	c.added++
	if c.kept >= r.limit || c.kept > 0 && r.kept[s] >= r.total {
		r.lastKept = -1
		return false
	}
	c.kept++
	r.kept[s]++
	c.last = len(r.diags)
	r.lastKept = len(r.diags)
	r.diags = append(r.diags, d)
	return true
}

// Count is how many diagnostics of severity s were added, kept or not.
func (r *Report) Count(s Severity) int {
	return r.added[severityIndex(s)]
}

// Latest is the diagnostic the latest Add kept, for its site to complete;
// nil when that Add was counted only, or when nothing was added.
func (r *Report) Latest() *Diagnostic {
	if len(r.diags) == 0 || r.lastKept < 0 {
		return nil
	}
	return &r.diags[r.lastKept]
}

// Diagnostics is what the report kept, in the order it was added; the last
// one kept of a code that went past the cap says how many more of that
// code there were. The report itself is left as it was.
func (r *Report) Diagnostics() []Diagnostic {
	if len(r.diags) == 0 {
		return nil
	}
	out := append([]Diagnostic(nil), r.diags...)
	for k, c := range r.codes {
		if more := c.added - c.kept; more > 0 {
			out[c.last].Message += moreNote(k, more)
		}
	}
	return out
}

// moreNote is what the last diagnostic kept of a code says of the ones the
// report counted only: how many, never where — the ones kept are the first
// the reading met, not the first in the file.
func moreNote(k reportKey, more int) string {
	what := "error"
	if k.sev == severityIndex(SeverityWarning) {
		what = "warning"
	}
	if more > 1 {
		what += "s"
	}
	if k.code != "" {
		what = string(k.code) + " " + what
	}
	return fmt.Sprintf(" — and %d more %s not reported: a reading reports %d of each code and %d of each severity, every code's first kept", more, what, MaxReported, MaxReportedOfASeverity)
}

// severityIndex is the slot of severity s in a Report's counts.
func severityIndex(s Severity) int {
	if s == SeverityWarning {
		return 1
	}
	return 0
}
