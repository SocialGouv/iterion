package author

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// moreAfter is the count the last diagnostic kept of a flood of one code
// says the report left out, -1 when it says none.
func moreAfter(t *testing.T, msg, word string) int {
	t.Helper()
	m := regexp.MustCompile(`and (\d+) more E\d+ ` + word + ` not reported: a reading reports 1000 of each code and 2000 of each severity, every code's first kept$`).FindStringSubmatch(msg)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("count %q: %v", m[1], err)
	}
	return n
}

// The guard reports its first thousand refusals and its first thousand
// warnings, each kept apart: a document of 1500 anchors draws 1000 of the
// refusal and the count of the rest.
func TestGuardReportsItsFirstThousandRefusals(t *testing.T) {
	res := Parse("anchors.bot.yaml", []byte("dsl: 2\nx: ["+strings.Repeat("&a a,", 1500)+"a]\n"))
	var anchors []parser.Diagnostic
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "an anchor (&a) is not read") {
			anchors = append(anchors, d)
		}
	}
	if len(anchors) != 1000 {
		t.Fatalf("1500 anchors: want 1000 refusals reported, got %d", len(anchors))
	}
	if n := moreAfter(t, anchors[999].Message, "errors"); n != 500 {
		t.Errorf("the last refusal kept counts %d more, want 500: %q", n, anchors[999].Message)
	}
}

func TestGuardReportsItsFirstThousandWarnings(t *testing.T) {
	res := Parse("comments.bot.yaml", []byte("dsl: 2\nx:\n"+strings.Repeat("  - a # c\n", 1500)))
	var readings []parser.Diagnostic
	for _, d := range res.Diagnostics {
		if IsCommentReading(d) {
			readings = append(readings, d)
		}
	}
	if len(readings) != 1000 {
		t.Fatalf("1500 values ending in a comment: want 1000 warnings reported, got %d", len(readings))
	}
	if n := moreAfter(t, readings[999].Message, "warnings"); n != 500 {
		t.Errorf("the last warning kept counts %d more, want 500: %q", n, readings[999].Message)
	}
}

// The speller reports its first thousand refusals too: 1500 edges that are
// not edge lines draw 1000 and the count of the rest.
func TestSpellerReportsItsFirstThousandRefusals(t *testing.T) {
	doc := "dsl: 2\nworkflow:\n  name: w\n  entry: a\n  edges:\n" + strings.Repeat("    - a b\n", 1500)
	res := Parse("edges.bot.yaml", []byte(doc))
	var edges []parser.Diagnostic
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError && d.Line >= 6 {
			edges = append(edges, d)
		}
	}
	if len(edges) != 1000 {
		t.Fatalf("1500 broken edges: want 1000 refusals reported, got %d", len(edges))
	}
	if n := moreAfter(t, edges[999].Message, "errors"); n != 500 {
		t.Errorf("the last refusal kept counts %d more, want 500: %q", n, edges[999].Message)
	}
}

// The readers that work off a node's position on its line read the line
// once, however many nodes it holds: a tag refusal quotes the value from
// there (excerpt), an edge's position is checked against it (edgeAt), and
// the comment reader finds where a quoted scalar ends or which `#` is a
// comment. Each shape is a few thousand values — or twenty thousand `#` —
// on one line of some 10 to 40 KB: linear, it costs a few MiB.
func TestReadersOfOneLongLineAreLinear(t *testing.T) {
	for _, c := range []struct {
		name string
		read func()
	}{
		{"tag refusals", func() {
			Parse("tags.bot.yaml", []byte("dsl: 2\nx: ["+strings.Repeat("! a,", 4000)+"! a]\n"))
		}},
		// `a -> b c` is refused past its first column (the trailing `c`),
		// which is where the speller checks the position against the line.
		{"edge positions", func() {
			Parse("edges.bot.yaml", []byte("dsl: 2\nworkflow:\n  name: w\n  entry: a\n  edges: ["+strings.Repeat("'a -> b c',", 2000)+"'a -> b c']\n"))
		}},
		{"quoted scalar ends", func() {
			Comments([]byte("dsl: 2\nx: [" + strings.Repeat("'a',", 3000) + "'a']\n"))
		}},
		{"comment candidates", func() {
			Comments([]byte("dsl: 2\nx: {a: 1, # c\n  b: \"" + strings.Repeat(" #", 20_000) + "\"} # c\n"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if alloc := allocatedBy(c.read); alloc > 16<<20 {
				t.Errorf("allocated %d MiB, want under 16 MiB: the work grows with the line, not the values", alloc>>20)
			}
		})
	}
}

// The separator notice counts every invisible separator and lists the lines
// of the first thousand: the list says how many it left out.
func TestSeparatorNoticeListsTheFirstThousandLines(t *testing.T) {
	src := "dsl: 2\n" + strings.Repeat("\u0085", 1500)
	n := separatorNotice("nel.bot.yaml", []byte(src))
	if n == nil {
		t.Fatal("no notice for 1500 separators")
	}
	if !strings.Contains(n.Message, "1500 invisible line separators") {
		t.Errorf("the notice does not count every separator: %q", excerpt(n.Message))
	}
	if !strings.Contains(n.Message, ", 1001 and 500 more)") {
		t.Errorf("the notice does not stop its list at the thousandth line and count the rest: …%q", n.Message[max(0, len(n.Message)-200):])
	}
}

// A `fallbacks:` written as a mapping is refused with the remedy of a list
// of routes: the speller completes the refusal its sequence reader just
// recorded, and no other one.
func TestFallbacksWrittenAsAMappingSayRoutes(t *testing.T) {
	doc := "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    fallbacks:\n      route: spare\n      model: x\n"
	res := Parse("fallbacks.bot.yaml", []byte(doc))
	var hit []parser.Diagnostic
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "is a list of routes (order is the try order), each `{route: <name>, …}`, not a mapping") {
			hit = append(hit, d)
		}
	}
	if len(hit) != 1 || hit[0].Line != 6 {
		t.Fatalf("want the one routes refusal on the mapping's line, got %v", res.Diagnostics)
	}
}

// stepsOf is the characters the position readers decode while f runs
// (readSteps) — the process's count, so the witnesses that read it hold only
// while nothing else of this package reads beside them
// (TestTheStepWitnessesRunAlone).
func stepsOf(f func()) int64 {
	before := readSteps.Load()
	f()
	return readSteps.Load() - before
}

// The step witnesses read a count of the whole process: a test of this
// package running in parallel (t.Parallel) or reading from a goroutine of
// its own would add its steps to theirs, and their ratios would stop
// saying what they say. None does, and the first that would is refused
// here, by name.
func TestTheStepWitnessesRunAlone(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test file of the package found: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := goparser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GoStmt:
				t.Errorf("%s: a goroutine in a test of this package: its readings add to the steps the witnesses read (readSteps)", fset.Position(n.Pos()))
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" {
					t.Errorf("%s: a parallel test in this package: its readings add to the steps the witnesses read (readSteps)", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
}

// The readers that work off a node's position on its line decode the line
// once however many values it holds: counted in the characters they decode,
// four times the values on the line is four times the work — never the
// sixteen that reading the line again for each value costs. The count is
// the readers' own, not a clock: a shared runner reads it the same.
func TestLongLineReadersDecodeTheLineOnce(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	parse := func(src string) { Parse("line.bot.yaml", []byte(src)) }
	comments := func(src string) { Comments([]byte(src)) }
	for _, c := range []struct {
		name string
		src  func(n int) string
		read func(string)
	}{
		{"tag refusals", func(n int) string { return "dsl: 2\nx: [" + strings.Repeat("! a,", n) + "! a]\n" }, parse},
		{"plain scalars", func(n int) string { return "dsl: 2\nx: [" + strings.Repeat("a,", n) + "a]\n" }, parse},
		{"quoted scalars", func(n int) string { return "dsl: 2\nx: [" + strings.Repeat("'a',", n) + "'a']\n" }, parse},
		{"edge positions", func(n int) string {
			return "dsl: 2\nworkflow:\n  name: w\n  entry: a\n  edges: [" + strings.Repeat("'a -> b c',", n) + "'a -> b c']\n"
		}, parse},
		{"quoted scalar ends", func(n int) string { return "dsl: 2\nx: [" + strings.Repeat("'a',", n) + "'a']\n" }, comments},
		{"comment candidates", func(n int) string {
			return "dsl: 2\nx: {a: 1, # c\n  b: \"" + strings.Repeat(" #", n) + "\"} # c\n"
		}, comments},
	} {
		t.Run(c.name, func(t *testing.T) {
			small, large := c.src(5000), c.src(20000)
			s := stepsOf(func() { c.read(small) })
			l := stepsOf(func() { c.read(large) })
			t.Logf("%d characters decoded for %d bytes, %d for %d", s, len(small), l, len(large))
			if s == 0 || l > 6*s || l > 4*int64(len(large)) {
				t.Errorf("reading four times the values decoded %d characters against %d (%d bytes): the work grows with the values times the line", l, s, len(large))
			}
		})
	}
}

// The foot of a flow collection holding the comment written inside it:
// yaml.v3 hangs every own-line comment below the collection on its foot,
// and the count of each hung copy reads that block once for them all — a
// foot of four times the lines is four times the work.
func TestAFlowFootIsReadOnce(t *testing.T) {
	doc := func(k int) []byte {
		return []byte("dsl: 2\nnodes:\n  - {agent: a, # c\n    model: m}\n" + strings.Repeat("  # c\n", k) +
			"workflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n")
	}
	var listed []string
	s := stepsOf(func() { Comments(doc(1000)) })
	l := stepsOf(func() { listed = Comments(doc(4000)) })
	t.Logf("%d characters decoded for a foot of 1000 lines, %d for 4000", s, l)
	if s == 0 || l > 6*s {
		t.Errorf("a foot of four times the lines decoded %d characters against %d: the foot is read again for each of its lines", l, s)
	}
	if len(listed) != 4001 {
		t.Errorf("listed %d comments, want the inside one and the foot's 4000", len(listed))
	}
}
