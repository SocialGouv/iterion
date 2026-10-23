package parser

import (
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"testing"
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

// listSource is an agent whose unknown property holds one flat list of n
// one-letter identifiers: the ticket's shape, 3 bytes and 2 tokens an element.
func listSource(n int) string {
	return "agent a:\n  model: \"m\"\n  bogus: [" + strings.Repeat("a, ", n) + "a]\n"
}

// The budget speaks while the lexer reads, not once the source is read: a
// list forty times past it costs what the budget lets through, and the
// source is refused whole, at the token the budget ran out on — E001 naming
// the count, never the E012 the property would draw once the list was read.
func TestReadBudgetRefusesBeforeTheListIsRead(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	src := listSource(80_000)
	var res *ParseResult
	alloc := allocatedBy(func() { res = parseLenient("big.bot", src) })
	if len(res.Diagnostics) != 1 {
		t.Fatalf("want the one refusal, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
	d := res.Diagnostics[0]
	if d.Code != DiagUnexpectedToken || d.Severity != SeverityError || !strings.Contains(d.Message, "past the read budget of 4096") {
		t.Errorf("refusal = %s %s %q, want an E001 error naming the budget of 4096 tokens", d.Code, d.Severity, d.Message)
	}
	// The tokens before the list are agent a : NEWLINE INDENT model : "m"
	// NEWLINE bogus : [ — 12 — so the list spends the rest: the 4097th token
	// is the element at index (4096-12)/2 = 2042. `  bogus: [` is 10
	// columns, an element 3: that `a` stands at column 11 + 3·2042.
	if d.Line != 3 || d.Column != 11+3*2042 {
		t.Errorf("refusal at %d:%d, want 3:%d — the token the budget ran out on", d.Line, d.Column, 11+3*2042)
	}
	t.Logf("allocated %d KiB", alloc>>10)
	// The source is 240 KB: its runes and line table are a few MiB; the
	// 160 000 tokens past the budget would cost tens of MiB more.
	if limit := uint64(8 << 20); alloc > limit {
		t.Errorf("reading a source 40× past the budget allocated %d MiB, want under %d MiB: the budget speaks only after the list is read", alloc>>20, limit>>20)
	}
}

// The ticket's own probe: one flat list of 3.49 M one-letter identifiers just
// under the 10 MiB size cap, read on the default budget of 2^20 tokens. It
// cost 6 GiB before the budget existed (3.8 GiB peak); now the reader stops
// at the budget, so what it allocates no longer grows with the list.
func TestReadBudgetTicketProbe(t *testing.T) {
	if raceEnabled {
		t.Skip("the probe allocates what 2^20 tokens cost; the race detector multiplies it, and the budget-lowered witness above runs under -race")
	}
	t.Setenv(readBudgetEnv, "")
	src := listSource((maxSourceSize - 64) / 3)
	if len(src) > maxSourceSize {
		t.Fatalf("probe source is %d bytes, over the size cap", len(src))
	}
	var res *ParseResult
	alloc := allocatedBy(func() { res = parseLenient("probe.bot", src) })
	t.Logf("allocated %d MiB", alloc>>20)
	if limit := uint64(768 << 20); alloc > limit {
		t.Errorf("the ticket's probe allocated %d MiB, want under %d MiB", alloc>>20, limit>>20)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 1048576") {
		t.Errorf("want the one refusal naming the default budget of 1048576 tokens, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
}

// A blank line inside a profile-1 prompt body is no token — profile 1 drops
// it — but the lexer still records it (a ProfileRead, until a further body
// line proves it interior), so it spends the budget as profile 2's empty
// prompt line does.
func TestReadBudgetCountsProfileOneBlankPromptLines(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	src := "prompt p:\n  first\n" + strings.Repeat("\n", 20_000) + "  last\n"
	res := parseLenient("blank.bot", src)
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 4096") {
		t.Fatalf("want the one refusal naming the budget, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
	// Interior blank lines are profile reads by the time the budget runs
	// out: a source refused whole carries none of them.
	res = parseLenient("paragraphs.bot", "prompt p:\n"+strings.Repeat("  x\n\n", 5000))
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 4096") {
		t.Fatalf("paragraphs: want the one refusal naming the budget, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
	if len(res.ProfileReads) != 0 {
		t.Errorf("a refused source carries %d profile reads, want none", len(res.ProfileReads))
	}
	// Profile 2 keeps the same blank lines as tokens: the same count.
	res = parseLenient("blank2.bot", "dsl: 2\n"+src)
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 4096") {
		t.Fatalf("profile 2: want the one refusal naming the budget, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
}

// A source refused whole is read no further: profile 1 notes every quoted
// value holding a backslash (an escape profile 2 would read), a note the
// budget does not count, so a lexer that went on reading would collect the
// notes of every value past the refusal.
func TestARefusedSourceIsReadNoFurther(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	res := parseLenient("escapes.bot", "agent a:\n  model: \"m\"\n  tools: ["+strings.Repeat(`"\\x", `, 10_000)+"\"a\"]\n")
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 4096") {
		t.Fatalf("want the one refusal naming the budget, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
	if len(res.ProfileReads) != 0 {
		t.Errorf("the refused source carries %d escape notes: the lexer read on past the refusal", len(res.ProfileReads))
	}
}

// A diagnosis the lexer records — an unexpected character, an unclosed
// quote — is a record like any token: a run of them spends the budget.
func TestReadBudgetCountsLexerDiagnoses(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	res := parseLenient("chars.bot", "agent a:\n  model: \"m\"\n  x: "+strings.Repeat("?", 20_000)+"\n")
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 4096") {
		t.Fatalf("20 000 refused characters: want the one refusal naming the budget, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
}

// The lexers other readers build on their own (the canonical form, fix,
// migrate, the comment scanner) read through the same budget: a refused
// source is the one error token, as a source past the size cap is.
func TestReadBudgetHoldsForEveryLexer(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	toks := NewLexer("big.bot", listSource(80_000)).All()
	if len(toks) != 1 || toks[0].Type != TokenError || !strings.Contains(toks[0].Value, "past the read budget of 4096") {
		n := len(toks)
		if n > 3 {
			n = 3
		}
		t.Fatalf("want the one error token, got %d tokens (head: %v)", len(toks), toks[:n])
	}
}

// A source within the budget reads as before — the budget counts what the
// lexer records, and a bot the size of this repository's largest is far
// below it.
func TestReadBudgetLeavesASourceWithinItAlone(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	res := parseLenient("small.bot", listSource(2000))
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagUnknownProperty {
		t.Fatalf("want the E012 the unknown property draws, got %v", res.Diagnostics)
	}
}

// The line table holds one entry per line; sized from the newline count up
// front, it is allocated once rather than grown — a source of blank lines
// otherwise allocates five times the table it ends with.
func TestLineTableIsAllocatedOnce(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n" + strings.Repeat("\n", 2<<20)
	var res *ParseResult
	alloc := allocatedBy(func() { res = parseLenient("blank-lines.bot", src) })
	if len(res.Diagnostics) != 0 {
		t.Fatalf("blank lines are no error, got %v", res.Diagnostics)
	}
	t.Logf("allocated %d MiB", alloc>>20)
	// 2 Mi lines: 8 MiB of runes and 16 MiB of line table once; grown, the
	// table alone allocates about 80 MiB.
	if limit := uint64(40 << 20); alloc > limit {
		t.Errorf("reading 2 Mi blank lines allocated %d MiB, want under %d MiB", alloc>>20, limit>>20)
	}
}

// A source the reader refuses whole — past the size cap, past the budget —
// is no misplaced line: its remedy is the bound's own, never E001's
// catalogued advice about a line's indentation. The size cap is a file's,
// and a bot splits into fragments under it; the budget is the bot's, its
// files share it, and the operator's count is the way past it.
func TestBoundRefusalsCarryTheirOwnRemedy(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	for _, c := range []struct {
		name, src, message string
		hint               []string
		not                string
	}{
		{"size cap", strings.Repeat("#\n", maxSourceSize/2+1), "exceeds maximum size", []string{"import", "lib/", "the read budget stays the bot's"}, ""},
		{"read budget", listSource(80_000), "past the read budget of 4096", []string{readBudgetEnv, "the files of a bot share it"}, "import"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := parseLenient("big.bot", c.src)
			if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, c.message) {
				t.Fatalf("want the one refusal %q, got %d diagnostics (first: %v)", c.message, len(res.Diagnostics), first(res.Diagnostics))
			}
			d := res.Diagnostics[0]
			if d.Code != DiagUnexpectedToken || d.Severity != SeverityError {
				t.Errorf("the refusal is %s %s, want an E001 error", d.Code, d.Severity)
			}
			if d.Hint == HintFor(DiagUnexpectedToken) {
				t.Errorf("the refusal carries E001's catalogued hint %q, which is about a line's indentation", d.Hint)
			}
			for _, want := range c.hint {
				if !strings.Contains(d.Hint, want) {
					t.Errorf("hint %q does not name %q", d.Hint, want)
				}
			}
			if c.not != "" && strings.Contains(d.Hint, c.not) {
				t.Errorf("hint %q names %q: splitting a bot is no way past a budget its files share", d.Hint, c.not)
			}
		})
	}
}

// stderrOf is what f writes on the process's stderr.
func stderrOf(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ITERION_DSL_READ_BUDGET reads as the documentation says: a count, 0 for
// no budget at all, the default when unset or empty — and the default for
// a value that is no count, said once on stderr with the value and the
// budget applied: a sign, a unit, a number past the int range.
func TestReadBudgetFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
		warn bool
	}{
		{"", DefaultReadBudget, false},
		{"  77 ", 77, false},
		{"1048576", 1 << 20, false},
		{"0", math.MaxInt, false},
		{"-3", DefaultReadBudget, true},
		{"2M", DefaultReadBudget, true},
		{"1e6", DefaultReadBudget, true},
		{"99999999999999999999", DefaultReadBudget, true},
	} {
		t.Setenv(readBudgetEnv, c.env)
		var got int
		said := stderrOf(t, func() { got = ReadBudget() })
		if got != c.want {
			t.Errorf("%s=%q reads as %d, want %d", readBudgetEnv, c.env, got, c.want)
		}
		v := strings.TrimSpace(c.env)
		switch {
		case !c.warn && said != "":
			t.Errorf("%s=%q is a count, yet stderr says %q", readBudgetEnv, c.env, said)
		case c.warn && (!strings.Contains(said, `invalid ITERION_DSL_READ_BUDGET="`+v+`"`) || !strings.Contains(said, "using the default 1048576")):
			t.Errorf("%s=%q is no count: stderr says %q, want the value and the default applied", readBudgetEnv, c.env, said)
		}
		if again := stderrOf(t, func() { ReadBudget() }); again != "" {
			t.Errorf("%s=%q read a second time says %q: once per value is enough", readBudgetEnv, c.env, again)
		}
	}
	// Said as an error: a log level that keeps errors alone still says it.
	t.Setenv("ITERION_LOG_LEVEL", "error")
	t.Setenv(readBudgetEnv, "3x")
	if said := stderrOf(t, func() { ReadBudget() }); !strings.Contains(said, `invalid ITERION_DSL_READ_BUDGET="3x"`) {
		t.Errorf("at ITERION_LOG_LEVEL=error an invalid budget is replaced in silence: stderr says %q", said)
	}
	if DefaultReadBudget != 1<<20 || ReadBudgetEnv != readBudgetEnv {
		t.Errorf("the budget is %d under %s, the documentation says 1048576 under %s", DefaultReadBudget, ReadBudgetEnv, readBudgetEnv)
	}
}

// A comment spends two records of the budget, every other record one: a run
// of comment lines is refused at half the count of a run of names, and a
// source of comments under half the budget reads.
func TestACommentSpendsTwoRecords(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	res := parseLenient("comments.bot", strings.Repeat("## note\n", 2100))
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 4096") {
		t.Fatalf("2100 comments on a budget of 4096: want the one refusal, got %d diagnostics (first: %v)", len(res.Diagnostics), first(res.Diagnostics))
	}
	// 2048 comments spend 4096: the 2049th is the one with no room.
	if d := res.Diagnostics[0]; d.Line != 2049 || d.Column != 1 {
		t.Errorf("refusal at %d:%d, want 2049:1 — the comment the budget ran out on", d.Line, d.Column)
	}
	res = parseLenient("comments.bot", strings.Repeat("## note\n", 2047))
	if len(res.Diagnostics) != 0 || res.File == nil || len(res.File.Comments) != 2047 {
		t.Fatalf("2047 comments and the end of the file spend 4095: want them read, got %v", res.Diagnostics)
	}
	// 2048 comments spend the whole budget: the end of the file is a
	// record too, and the one with no room — named at the last line's end.
	res = parseLenient("comments.bot", strings.Repeat("## note\n", 2048))
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Line != 2048 || res.Diagnostics[0].Column != 8 {
		t.Fatalf("2048 comments and the end of the file on a budget of 4096: want the refusal at 2048:8, got %v", res.Diagnostics)
	}
}

// The structure the end of the file closes — its dedents, the end itself —
// stands on the line after the final newline; a refusal there names the end
// of the last line, a place the file holds.
func TestARefusalAtTheEndNamesTheLastLine(t *testing.T) {
	// agent a : NEWLINE INDENT model : "m" NEWLINE — 9 — then DEDENT, EOF.
	t.Setenv(readBudgetEnv, "9")
	res := parseLenient("end.bot", "agent a:\n  model: \"m\"\n")
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "past the read budget of 9") {
		t.Fatalf("want the one refusal, got %v", res.Diagnostics)
	}
	if d := res.Diagnostics[0]; d.Line != 2 || d.Column != 13 {
		t.Errorf("refusal at %d:%d, want 2:13 — the end of the last line, not the line after it", d.Line, d.Column)
	}
}

// The refusal says what a record is: the words and marks an author counts,
// and the line ends, indentation steps and the end of the file the lexer
// records too.
func TestTheRefusalSaysWhatARecordIs(t *testing.T) {
	msg := ReadBudgetMessage(7)
	for _, want := range []string{"past the read budget of 7", "word", "value", "punctuation mark", "prompt line", "line end", "indentation step", "the file's end", "a comment two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal %q does not say %q", msg, want)
		}
	}
}

// A source refused whole says which bound refused it, to the parser's
// callers and to a reader of the lexer alone.
func TestARefusedSourceNamesItsBound(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	if res := parseLenient("big.bot", listSource(80_000)); res.Refused != BoundBudget || res.ErrorCount != 1 {
		t.Errorf("past the budget: Refused = %v, ErrorCount = %d, want BoundBudget and 1", res.Refused, res.ErrorCount)
	}
	if l := NewLexer("big.bot", listSource(80_000)); l.Refused() != BoundBudget {
		t.Errorf("the lexer past the budget says %v", l.Refused())
	}
	big := strings.Repeat("#\n", maxSourceSize/2+1)
	if res := parseLenient("huge.bot", big); res.Refused != BoundSize {
		t.Errorf("past the size cap: Refused = %v, want BoundSize", res.Refused)
	}
	if res := parseLenient("small.bot", listSource(10)); res.Refused != BoundNone {
		t.Errorf("a source that reads: Refused = %v, want BoundNone", res.Refused)
	}
}

// The nesting bound refuses the line past it (E003), not the file: what
// follows is still read.
func TestNestingBoundRefusesTheDeeperLine(t *testing.T) {
	var b strings.Builder
	b.WriteString("agent a:\n  model: \"m\"\n  sandbox:\n")
	for i := 0; i < 102; i++ {
		b.WriteString(strings.Repeat(" ", 4+i) + "x:\n")
	}
	b.WriteString("agent b:\n  model: \"m\"\n")
	res := parseLenient("nest.bot", b.String())
	var depth *Diagnostic
	for i, d := range res.Diagnostics {
		if d.Code == DiagBadIndentation && strings.Contains(d.Message, "maximum nesting depth exceeded (100 levels)") {
			depth = &res.Diagnostics[i]
		}
	}
	if depth == nil || depth.Line != 102 {
		t.Fatalf("want E003 on the 101st level (line 102), got %v", res.Diagnostics)
	}
	if res.File == nil || len(res.File.Agents) != 2 {
		t.Errorf("the declaration after the deep line is not read: %+v", res.File)
	}
}

func first(ds []Diagnostic) string {
	if len(ds) == 0 {
		return "none"
	}
	return ds[0].Error()
}
