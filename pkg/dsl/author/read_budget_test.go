package author

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// readBudgetEnv is the operator's hatch, spelled as the documentation spells
// it (docs/environment-variables.md).
const readBudgetEnv = "ITERION_DSL_READ_BUDGET"

// allocatedBy is the number of bytes the heap handed out while f ran.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The guard reads the source at each node's position; a flow sequence
// written on one line puts every node on the same line, so a read that
// copies the line per node costs the line's length times the node count —
// quadratic before the value count can speak. Each shape here is a few
// thousand values on one line of a few KB: linear, it costs about a MiB.
func TestGuardReadsOneLongLineInLinearTime(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"plain scalars", "dsl: 2\nx: [" + strings.Repeat("a,", 4000) + "a]\n"},
		// A quoted scalar is also scanned for a raw CR or NEL between its
		// quotes (rawFoldBreak).
		{"quoted scalars", "dsl: 2\nx: [" + strings.Repeat("'a',", 3000) + "'a']\n"},
		// A mapping key that is itself a mapping is read for a leading `{{`.
		{"flow mappings", "dsl: 2\nx: [" + strings.Repeat("{a: b},", 2000) + "{a: b}]\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			alloc := allocatedBy(func() { Parse("one-line.bot.yaml", []byte(c.src)) })
			if limit := uint64(16 << 20); alloc > limit {
				t.Errorf("reading %d bytes on one line allocated %d MiB, want under %d MiB: the guard's work grows with the line, not the values", len(c.src), alloc>>20, limit>>20)
			}
		})
	}
}

// The document's reading is held to the read budget as the .bot's is:
// ITERION_DSL_READ_BUDGET moves it for the YAML twin, and every value of the
// tree spends one record, every comment line two. The values are counted on
// the decoded tree, so the refusal says what the document holds, placed on
// the value the budget ran out on, with the document's remedy.
func TestTheDocumentsReadingIsHeldToTheReadBudget(t *testing.T) {
	t.Setenv(readBudgetEnv, "1000")
	// mapping, dsl, 2, x, the list: 5 values before the items.
	doc := []byte("dsl: 2\nx:\n" + strings.Repeat("- a\n", 1500))
	res := Parse("count.bot.yaml", doc)
	if len(res.Diagnostics) != 1 {
		t.Fatalf("a document of 1505 values on a budget of 1000: want the one E050 refusal, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != parser.DiagAuthorDocument || !strings.Contains(d.Message, "past the read budget of 1000") || d.Hint != docReadBudgetHint {
		t.Fatalf("refusal = %s (hint %q), want E050 past the read budget of 1000, the document's remedy", d.Error(), d.Hint)
	}
	// The 1001st value is item 996, on line 2 + 996.
	if d.Line != 2+996 || d.Column != 3 {
		t.Errorf("refusal at %d:%d, want %d:3 — the value the budget ran out on", d.Line, d.Column, 2+996)
	}
	// A comment line spends two: 400 of them and seven values fit 1000,
	// 500 do not.
	comments := func(n int) []byte {
		return []byte("dsl: 2\n" + strings.Repeat("# c\n", n) + "prompts:\n  p: hi\n")
	}
	if res := Parse("comments.bot.yaml", comments(400)); res.HasErrors() {
		t.Errorf("400 comment lines and seven values on a budget of 1000: %v", res.Diagnostics)
	}
	if res := Parse("comments.bot.yaml", comments(500)); !res.HasErrors() || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 1000") {
		t.Errorf("500 comment lines on a budget of 1000: want the budget's refusal, got %v", res.Diagnostics)
	}
	t.Setenv(readBudgetEnv, "0")
	if res := Parse("count.bot.yaml", doc); res.HasErrors() {
		for _, d := range res.Diagnostics {
			if d.Code == parser.DiagAuthorDocument {
				t.Errorf("with no budget the document is refused: %v", d)
			}
		}
	}
}

// Before the decode runs, its cost is bounded off the text: the values its
// indicators could open, at most twice the budget — a flow list of 1500
// values counts 3008, past 2000 on a budget of 1000 — refused on the
// indicator the count passed the ceiling at, with the remedy that says
// what is counted and what never is. The text of a `|` block is never
// counted: a prompt of 4000 colons is one value; text in quotes is.
func TestTheDecodeIsBoundedBeforeItRuns(t *testing.T) {
	t.Setenv(readBudgetEnv, "1000")
	doc := []byte("dsl: 2\nx: [" + strings.Repeat("a,", 1499) + "a]\n")
	res := Parse("count.bot.yaml", doc)
	if len(res.Diagnostics) != 1 {
		t.Fatalf("want the one E050 refusal, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != parser.DiagAuthorDocument || !strings.Contains(d.Message, "may hold more values than its reader decodes, 2000") {
		t.Fatalf("refusal = %s, want E050 naming the ceiling of 2000", d.Error())
	}
	// dsl: and x: count 3 each, the `[` 2, the document 2: 10 before the
	// list, each `,` 2, so the 996th `,` is the one with no room: `x: [`
	// is 4 columns, an `a,` 2 — column 4 + 2·996.
	if d.Line != 2 || d.Column != 4+2*996 {
		t.Errorf("refusal at %d:%d, want 2:%d — the indicator the count passed the ceiling at", d.Line, d.Column, 4+2*996)
	}
	for _, want := range []string{readBudgetEnv, "never in the text of a `|` or `>` block", "twice the read budget"} {
		if !strings.Contains(d.Hint, want) {
			t.Errorf("the refusal's remedy %q does not name %q", d.Hint, want)
		}
	}
	// A mapping's indicators count one and a half times its values: 400
	// `key: value` lines count 1208, past the budget, and hold 805 values,
	// within it — read to its values, not refused before them.
	var pairs strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&pairs, "  k%d: v\n", i)
	}
	for _, d := range Parse("pairs.bot.yaml", []byte("dsl: 2\nx:\n"+pairs.String())).Diagnostics {
		if d.Code == parser.DiagAuthorDocument && (strings.Contains(d.Message, "reader decodes") || strings.Contains(d.Message, "read budget")) {
			t.Errorf("a mapping of 805 values on a budget of 1000 is refused for its values: %v", d)
		}
	}
	colons := []byte("dsl: 2\nprompts:\n  p: |\n    " + strings.Repeat(":", 4000) + "\n")
	for _, d := range Parse("colons.bot.yaml", colons).Diagnostics {
		if d.Code == parser.DiagAuthorDocument {
			t.Errorf("a prompt of 4000 colons in a `|` block on a budget of 1000 is refused: %v", d)
		}
	}
	quoted := []byte("dsl: 2\nprompts:\n  p: \"" + strings.Repeat(":", 1000) + "\"\n")
	if res := Parse("quoted.bot.yaml", quoted); len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "may hold more values than its reader decodes") {
		t.Errorf("a quoted value of 1000 colons on a budget of 1000: want the decode's refusal, got %v", res.Diagnostics)
	}
	// The block's text set aside, the structure beside it can still pass
	// the ceiling: the refusal says what it counted.
	both := []byte("dsl: 2\nprompts:\n  p: |\n    " + strings.Repeat(":", 4000) + "\nx: [" + strings.Repeat("a,", 1499) + "a]\n")
	if res := Parse("both.bot.yaml", both); len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "outside the text of its `|` and `>` blocks") || res.Diagnostics[0].Line != 5 {
		t.Errorf("structure past the ceiling beside a block of text: want the refusal on line 5, counting outside the blocks, got %v", res.Diagnostics)
	}
}

// A prompt is one value of the document and one line per line of the .bot
// it spells: the lines spend the .bot reader's budget, and its refusal
// lands on the document, inside the prompt.
func TestPromptLinesSpendTheBotReadBudget(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	doc := []byte("dsl: 2\nprompts:\n  p: |\n" + strings.Repeat("    a\n", 10_000))
	res := Parse("prompt.bot.yaml", doc)
	var refusal *parser.Diagnostic
	for i, d := range res.Diagnostics {
		if d.Code == parser.DiagUnexpectedToken && strings.Contains(d.Message, "past the read budget of 4096") {
			refusal = &res.Diagnostics[i]
		}
	}
	if refusal == nil {
		t.Fatalf("a prompt of 10 000 lines on a budget of 4096: want the reader's E001 refusal, got %v", res.Diagnostics)
	}
	if refusal.Line < 4 || refusal.Line > 10_003 {
		t.Errorf("the refusal is at line %d of the document, want a line of the prompt (4 to 10003)", refusal.Line)
	}
}

// A document past a bound breaks no rule of YAML: each refusal carries the
// remedy of its bound, never E050's catalogued advice about separators,
// anchors and tags.
func TestDocumentBoundsCarryTheirOwnRemedy(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	for _, c := range []struct {
		name, src, message, hint string
	}{
		{"size cap", "dsl: 2\n" + strings.Repeat("#\n", maxSourceSize/2+1), "exceeds the maximum size", "`imports:`"},
		{"nesting", "dsl: 2\nx: " + strings.Repeat("[", 102) + strings.Repeat("]", 102) + "\n", "maximum nesting depth exceeded (100 levels)", "flatten"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("bound.bot.yaml", []byte(c.src))
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, c.message) {
				t.Fatalf("want the one refusal %q, got %v", c.message, res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Code != parser.DiagAuthorDocument || d.Hint == parser.HintFor(parser.DiagAuthorDocument) || !strings.Contains(d.Hint, c.hint) {
				t.Errorf("refusal %s with hint %q, want E050 with the bound's own remedy naming %q", d.Code, d.Hint, c.hint)
			}
		})
	}
}

// The cursor finds a column from the last one it found on the same line,
// never from the line's start: the walk reads a line of a million nodes once.
// A line read again under a prefix of another byte length shows where the
// search started from — a cursor that starts over lands on the new prefix's
// offsets, one that resumes on the old ones.
func TestLineCursorResumesWhereItStopped(t *testing.T) {
	var c lineCursor
	line := strings.Repeat("ab", 1000)
	if rest, ok := c.tail([]string{line}, 1, 1001); !ok || rest != line[1000:] {
		t.Fatalf("column 1001 of the line reads %q…, want the line from byte 1000", rest[:min(len(rest), 10)])
	}
	// "é" is two bytes: resumed at byte 1000, one character on is byte
	// 1002; started over, column 1002 is byte 2001.
	moved := strings.Repeat("é", 1000) + line[1000:]
	rest, ok := c.tail([]string{moved}, 1, 1002)
	if !ok || rest != moved[1002:] {
		t.Errorf("the second search did not resume where the first stopped: got %d bytes left, want %d", len(rest), len(moved[1002:]))
	}
	// A column before the cursor, or another line, starts over.
	if rest, ok := c.tail([]string{line}, 1, 3); !ok || rest != line[2:] {
		t.Errorf("an earlier column reads %q…, want the line from byte 2", rest[:min(len(rest), 10)])
	}
	if rest, ok := c.tail([]string{line}, 1, 2001); !ok || rest != "" {
		t.Errorf("the column just past the line's last character reads (%q, %v), want (\"\", true)", rest, ok)
	}
	if _, ok := c.tail([]string{line}, 1, 2002); ok {
		t.Error("a column two past the line's end is on the line")
	}
}

// A prompt's lines spend the read budget as the speller splits them, the
// blank ones included, before it records one: a prompt of a hundred
// thousand blank lines is refused where the budget runs out, at what its
// lines cost, never after a record of each. A literal block keeps its
// lines, so the refusal stands on the line with no room; any other body is
// placed at its scalar.
func TestPromptLinesSpendTheBudgetBeforeTheyAreSplit(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	blank := strings.Repeat("\n", 100_000)
	for _, c := range []struct {
		name, doc string
		line, col int
	}{
		{"literal", "dsl: 2\nprompts:\n  p: |\n    a\n" + blank + "    a\n", 3 + 1 + 4096, 6},
		{"folded", "dsl: 2\nprompts:\n  p: >\n    a\n" + blank + "    a\n", 3, 6},
		{"double-quoted", "dsl: 2\nprompts:\n  p: \"a\n" + blank + "  a\"\n", 3, 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			var res *Result
			alloc := allocatedBy(func() { res = Parse("blank.bot.yaml", []byte(c.doc)) })
			var refusal *parser.Diagnostic
			for i, d := range res.Diagnostics {
				if d.Code == parser.DiagUnexpectedToken && strings.Contains(d.Message, "past the read budget of 4096") {
					refusal = &res.Diagnostics[i]
				}
			}
			if refusal == nil {
				t.Fatalf("want the reader's refusal of the budget, got %v", res.Diagnostics)
			}
			if refusal.Line != c.line || refusal.Column != c.col {
				t.Errorf("refusal at %d:%d, want %d:%d", refusal.Line, refusal.Column, c.line, c.col)
			}
			if refusal.Hint != docReadBudgetHint {
				t.Errorf("the refusal's remedy %q is not the document's", refusal.Hint)
			}
			t.Logf("allocated %d MiB", alloc>>20)
			// The document is 100 KB: its decode and its lines cost a few
			// MiB; a record of each of its 100 000 lines, split three
			// times over, costs tens.
			if limit := uint64(8 << 20); alloc > limit {
				t.Errorf("a prompt of 100 000 lines past the budget allocated %d MiB, want under %d MiB: its lines were split before they were counted", alloc>>20, limit>>20)
			}
		})
	}
}

// The catalog's text is written into the .bot as one comment line per line
// of it: those lines spend the budget before they are split too.
func TestCatalogLinesSpendTheBudget(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	doc := "dsl: 2\ncatalog:\n  name: n\n  description: |\n    a\n" + strings.Repeat("\n", 100_000) + "    a\n"
	var res *Result
	alloc := allocatedBy(func() { res = Parse("catalog.bot.yaml", []byte(doc)) })
	var refusal *parser.Diagnostic
	for i, d := range res.Diagnostics {
		if d.Code == parser.DiagUnexpectedToken && strings.Contains(d.Message, "past the read budget of 4096") {
			refusal = &res.Diagnostics[i]
		}
	}
	if refusal == nil || refusal.Line != 3 {
		t.Fatalf("want the reader's refusal at the catalog (line 3), got %v", res.Diagnostics)
	}
	t.Logf("allocated %d MiB", alloc>>20)
	if limit := uint64(8 << 20); alloc > limit {
		t.Errorf("a catalog of 100 000 lines past the budget allocated %d MiB, want under %d MiB", alloc>>20, limit>>20)
	}
}

// The .bot a document spells is read on the budget too — a list element is
// two of its records, the name and its comma — and refused there with the
// document's remedy: the author wrote YAML, where a document is split
// under `imports:`, not with an `import` line.
func TestTheSpelledBotIsRefusedWithTheDocumentsRemedy(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	doc := "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    tools:\n" + strings.Repeat("      - t\n", 3000)
	res := Parse("tools.bot.yaml", []byte(doc))
	if len(res.Diagnostics) != 1 {
		t.Fatalf("a list of 3000 values the budget admits, spelled past it: want the one refusal, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != parser.DiagUnexpectedToken || d.Message != parser.ReadBudgetMessage(4096) {
		t.Fatalf("refusal = %s %q, want the reader's own E001", d.Code, d.Message)
	}
	if !strings.Contains(d.Hint, "wherever the document is read") || !strings.Contains(d.Hint, "share it with the document") || strings.Contains(d.Hint, `import "lib/`) {
		t.Errorf("the refusal's remedy %q is not the document's", d.Hint)
	}
	if d.Line < 6 || d.Line > 3005 {
		t.Errorf("the refusal is at line %d, want a line of the list (6 to 3005)", d.Line)
	}
}

// A value spelled longer than the document wrote it — a quote is escaped
// in the .bot's string — can put the spelled text past the 10 MiB the
// reader takes while the document is under it: the refusal says so of the
// spelling, with the document's remedy.
func TestASpellingPastTheSizeCapIsSaidOfTheDocument(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	doc := "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    description: '" + strings.Repeat(`"`, 6<<20) + "'\n"
	res := Parse("quotes.bot.yaml", []byte(doc))
	if len(res.Diagnostics) != 1 {
		t.Fatalf("want the one refusal, got %d diagnostics", len(res.Diagnostics))
	}
	d := res.Diagnostics[0]
	if !strings.HasPrefix(d.Message, "the .bot text the document spells exceeds the maximum size (") || d.Hint != docSizeHint {
		t.Errorf("refusal = %q (hint %q), want the spelling's size with the document's remedy", d.Message, d.Hint)
	}
}

// Every comment reading of a document is read, reported or not: the
// report keeps a thousand, the canonical form compares them all — and only
// comment readings, never another reading E053 names.
func TestEveryCommentReadingIsKept(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	doc := "dsl: 2\nprompts:\n  p: |\n    a\n\n\nx:\n" + strings.Repeat("  - a # c\n", 1500)
	res := Parse("readings.bot.yaml", []byte(doc))
	reported := 0
	for _, d := range res.Diagnostics {
		if IsCommentReading(d) {
			reported++
		}
	}
	if reported != 1000 || len(res.CommentReadings) != 1500 {
		t.Fatalf("1500 comment readings: %d reported and %d kept, want 1000 and 1500", reported, len(res.CommentReadings))
	}
	for _, d := range res.CommentReadings {
		if !IsCommentReading(d) || strings.Contains(d.Message, "not reported") {
			t.Fatalf("a kept reading is %q: want each comment reading as read", d.Message)
		}
	}
}
