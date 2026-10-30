package parser

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain (open_blocks_test.go) holds every listBlockClosePoint answer of
// the suite to the from-scratch scan below: the memo of #2768 and the scan
// it stands for agree at every resync, on every source the suite parses, or
// the reading panics naming the token.
func checkClosePoint(p *parser, start, col int, cols []int, hosts []string, at, dedents int, ok bool) {
	wantAt, wantDedents, wantOk := referenceClosePoint(p, start, col, cols, hosts)
	if ok != wantOk || (ok && (at != wantAt || dedents != wantDedents)) {
		panic(fmt.Sprintf("listBlockClosePoint at token %d of %s (start %d, col %d, cols %v): memo gave (%d, %d, %v), a scan from scratch gives (%d, %d, %v)", p.lex.ti, p.file, start, col, cols, at, dedents, ok, wantAt, wantDedents, wantOk))
	}
}

// referenceClosePoint is the close-point scan as it stood before #2768 — the
// walk from the rescued line to the end of the enclosing block at every
// query — kept verbatim as the reference the memo is held to.
func referenceClosePoint(p *parser, start, col int, cols []int, hosts []string) (at, dedents int, ok bool) {
	lineStart := false
	lineIndent := false
	depth := 0
	pending := 0
	for j := start; ; j++ {
		t := p.lex.PeekAt(j)
		switch {
		case t.Type == TokenEOF:
			n := len(cols) - 1 + depth - pending
			if n < 0 {
				return 0, 0, false
			}
			return j, n, true
		case lineEnds(t):
			lineStart = true
			lineIndent = false
		case t.Type == TokenIndent:
			lineIndent = true
			depth++
		case t.Type == TokenDedent:
			pending++
		case t.Type == TokenError && lineStart:
			return 0, 0, false
		case lineStart && t.Column < col:
			if lineIndent || tokenAsIdent(t) == "" {
				return 0, 0, false
			}
			if idx := slices.Index(cols, t.Column); idx > 0 && p.rescueCannotClaim(t, j, hosts[idx]) {
				return 0, 0, false
			}
			loops := 0
			for _, c := range cols[1:] {
				if c > t.Column {
					loops++
				}
			}
			n := loops + depth - pending
			if n < 0 {
				return 0, 0, false
			}
			return j, n, true
		default:
			if lineStart && t.Column == col {
				if idx := slices.Index(cols, col); idx > 0 && p.rescueCannotClaim(t, j, hosts[idx]) {
					return 0, 0, false
				}
			}
			if lineStart {
				depth -= pending
			}
			pending = 0
			lineStart = false
		}
	}
}

// withoutClosePointCheck runs f with the suite's cross-checks off: the
// reference replay and scan are the quadratic costs whose absence f
// measures.
func withoutClosePointCheck(t *testing.T, f func()) {
	t.Helper()
	withoutOpenBlocksCheck(t, func() {
		saved := closePointCheck
		closePointCheck = nil
		defer func() { closePointCheck = saved }()
		f()
	})
}

// siblingBrokenListsSource is the ticket's shape: one nested block holding n
// sibling broken lists, a property of the enclosing block after them, so
// every rescue lands on the next sibling and scans for the close point.
func siblingBrokenListsSource(n int) string {
	var b strings.Builder
	b.Grow(n*14 + 60)
	b.WriteString("agent a:\n  model: \"m\"\n  sandbox:\n    network:\n")
	for i := 0; i < n; i++ {
		b.WriteString("      rules: [a,\n")
	}
	b.WriteString("    image: i\n")
	return b.String()
}

// The resync's close-point scan reads the region past a rescued line once,
// however many sibling broken lists ask: consecutive resyncs scan suffixes
// of one region, so the memo answers the later starts from the first scan.
// Mutate listBlockClosePoint to scan from scratch at every query — the
// memo's bypass — and the count goes quadratic (thousands of scans of a
// thousands-of-lines region), while every diagnostic stays the same.
func TestClosePointReadsTheRegionOnce(t *testing.T) {
	withoutClosePointCheck(t, func() {
		const lists = 2_000
		src := siblingBrokenListsSource(lists)
		p := &parser{lex: lexSource("lists.bot", src, sourceBudget()), file: "lists.bot"}
		n := len(p.lex.tokens)
		p.parseFile()
		if errs := p.report.Count(SeverityError); errs < lists {
			t.Fatalf("%d errors for %d broken lists: the shape never reached the resync", errs, lists)
		}
		if p.closePoint.read == 0 {
			t.Fatal("the resync never asked for the close point")
		}
		if p.closePoint.read > 2*n {
			t.Errorf("the close-point scans and memo walks read %d tokens of a stream of %d: the region is read once, however many sibling lists ask", p.closePoint.read, n)
		}
	})
}

// The ticket's measure: the close-point scan walked from each rescued line
// to the end of the enclosing block — 16 000 sibling broken lists in one
// nested block took 6.7 s of CPU, quadratic in the lists. With the memo the
// reading is linear, and held to five seconds — two orders of magnitude
// above its cost on a laptop — so the bound reddens on the class, never on
// the weather.
func TestSiblingBrokenListsParseInLinearTime(t *testing.T) {
	withoutClosePointCheck(t, func() {
		const lists = 16_000
		src := siblingBrokenListsSource(lists)
		start := time.Now()
		res := parseLenient("lists.bot", src)
		took := time.Since(start)
		if res.ErrorCount < lists {
			t.Fatalf("%d errors for %d broken lists: the shape never reached the resync", res.ErrorCount, lists)
		}
		if bound := 5 * time.Second; took > bound {
			t.Fatalf("%d sibling broken lists (%d bytes) parsed in %s, past the %s bound: the close-point scan is rescanning the region", lists, len(src), took, bound)
		}
		t.Logf("%d sibling broken lists (%d bytes) parsed in %s", lists, len(src), took)
	})
}

// Under the suite's cross-check, a reading of sibling broken lists exercises
// the memo: every resync after the first is answered without a scan, and
// each answer — hit or miss — was just checked against the scan from scratch
// by the TestMain hook.
func TestClosePointMemoHitsUnderTheCheck(t *testing.T) {
	saved := closePointCheck
	if saved == nil {
		t.Fatal("the TestMain hook is off: this test is only meaningful under the cross-check")
	}
	const lists = 40
	src := siblingBrokenListsSource(lists)
	p := parseKeeping("lists.bot", src)
	if p.closePoint.hits < lists-5 {
		t.Errorf("%d memo hits for %d sibling broken lists: the resyncs after the first scan suffixes of one region", p.closePoint.hits, lists)
	}
	if errs := p.report.Count(SeverityError); errs < lists {
		t.Fatalf("%d errors for %d broken lists: the shape never reached the resync", errs, lists)
	}
}
