package unit

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// allocatedBy is the number of bytes the heap handed out while f ran.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A bot of three files, each a run of comment lines, costs its load what
// the three files' ASTs keep of the comments, beyond what the lexers cost:
// no comment is held twice while a file is read, and the merge adds the
// unit's one list of them.
func TestCommentLinesCostTheUnitLoadTheirASTOnly(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	const n = 100_000
	frag := strings.Repeat("#\n", n)
	files := map[string]string{
		"main.bot":   "import \"lib/f1.bot\"\nimport \"lib/f2.bot\"\n" + frag,
		"lib/f1.bot": frag,
		"lib/f2.bot": frag,
	}
	lex := allocatedBy(func() {
		for name, src := range files {
			parser.NewLexer(name, src)
		}
	})
	var u *Unit
	load := allocatedBy(func() { u = salvageUnit(LoadMap(files, "main.bot")) })
	if len(u.Files) != 3 || u.HasErrors() || u.Merged == nil || len(u.Merged.Comments) != 3*n {
		t.Fatalf("the three files do not load as one bot of %d comments: %d files, %d diagnostics", 3*n, len(u.Files), len(u.Diagnostics))
	}
	extra := int64(load) - int64(lex)
	t.Logf("the load allocated %d MiB beyond its lexers' %d MiB", extra>>20, lex>>20)
	// Three files of 100 000 comments: 16 MiB each beyond the lexer, and
	// the merged list of 300 000 pointers grown file by file. Held twice
	// while a file is read, each file costs over 100 MiB more.
	if extra > 96<<20 {
		t.Errorf("the load allocated %d MiB beyond its lexers for %d comment lines, want under 96 MiB: the comments are held twice", extra>>20, 3*n)
	}
}

// prompts is n prompt declarations named prefix0, prefix1, …
func prompts(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "prompt %s%d:\n  text\n", prefix, i)
	}
	return b.String()
}

// The files of a bot share one read budget: two files each within it, past
// it together, are refused where it ran out — in the fragment, with the
// bot's refusal and the operator's count for remedy, splitting being no way
// past a budget the files share. Within it, the same two files load whole.
func TestTheFilesOfABotShareOneBudget(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "4096")
	files := map[string]string{"main.bot": "import \"lib/frag.bot\"\n" + prompts("m", 450), "lib/frag.bot": prompts("f", 450)}
	for name, src := range files {
		if res := parsertest.Parse(name, src); len(res.Diagnostics) != 0 {
			t.Fatalf("%s alone is past the budget: %v", name, res.Diagnostics[0])
		}
	}
	u := salvageUnit(LoadMap(files, "main.bot"))
	var refusals []parser.Diagnostic
	for _, d := range u.Diagnostics {
		if strings.Contains(d.Message, "the bot is past the read budget of 4096, which its files share") {
			refusals = append(refusals, d)
		}
	}
	if len(refusals) != 1 {
		t.Fatalf("two files within the budget each, past it together: want the bot refused once, got %v", u.Diagnostics)
	}
	if d := refusals[0]; d.File != "lib/frag.bot" || !strings.Contains(d.Hint, "the files of a bot share it") || strings.Contains(d.Hint, "import") {
		t.Errorf("the refusal is %s with remedy %q, want it in lib/frag.bot, the operator's count for remedy", d.Error(), d.Hint)
	}
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	if u := salvageUnit(LoadMap(files, "main.bot")); u.HasErrors() || u.Merged == nil || len(u.Merged.Prompts) != 900 {
		t.Fatalf("within the budget the bot does not load whole: %v", u.Diagnostics)
	}
}

// Once the budget is spent no further file is parsed: a fragment whose
// import comes after files that spent all of it is refused at that import,
// once — read to know it is there, never parsed — and none after it is read.
func TestAFragmentPastTheSpentBudgetIsNotRead(t *testing.T) {
	src := map[string]string{
		"main.bot":  "import \"lib/a.bot\"\nimport \"lib/b.bot\"\nimport \"lib/c.bot\"\n" + prompts("m", 10),
		"lib/a.bot": prompts("a", 10),
		"lib/b.bot": prompts("b", 10),
		"lib/c.bot": prompts("c", 10),
	}
	// The budget main and a spend to the last record.
	t.Setenv("ITERION_DSL_READ_BUDGET", "0")
	exact := parsertest.Parse("main.bot", src["main.bot"]).Spent + parsertest.Parse("lib/a.bot", src["lib/a.bot"]).Spent
	t.Setenv("ITERION_DSL_READ_BUDGET", strconv.Itoa(exact))
	reads := map[string]int{}
	u := salvageUnit(Load(func(rel string) ([]byte, error) {
		reads[rel]++
		return []byte(src[rel]), nil
	}, "main.bot", func(rel string) string { return rel }))
	// The fragment at whose import the bot is refused is read to know it is
	// there, never parsed; none after it is read at all.
	if reads["lib/b.bot"] > 1 || reads["lib/c.bot"] != 0 || len(u.Files) != 2 {
		t.Errorf("fragments past the spent budget were read %v and %d files parsed, want lib/b.bot read once at most, lib/c.bot never, two files parsed", reads, len(u.Files))
	}
	if len(u.Diagnostics) != 1 || u.ErrorCount() != 1 {
		t.Fatalf("want the bot refused once, got %v", u.Diagnostics)
	}
	if d := u.Diagnostics[0]; d.File != "main.bot" || d.Line != 2 || d.Message != parser.UnitBudgetMessage(exact) || d.Hint != parser.ReadBudgetHint {
		t.Errorf("refusal = %s (hint %q), want the bot's refusal at the import of lib/b.bot (main.bot:2)", d.Error(), d.Hint)
	}
}

// One load of a bot costs its reader what the budget lets through, however
// many files it is written in: ten fragments of comment lines, each within
// the budget alone, cost the load what one of them does — the one that
// runs out spends the budget, the ones after it are not read.
func TestTenFragmentsCostTheLoadWhatOneDoes(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "65536")
	frag := strings.Repeat("#\n", 32768-8)
	if res := parsertest.Parse("f.bot", frag); len(res.Diagnostics) != 0 {
		t.Fatalf("a fragment alone is past the budget: %v", res.Diagnostics[0])
	}
	files := map[string]string{}
	var main strings.Builder
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("lib/f%d.bot", i)
		files[name] = frag
		fmt.Fprintf(&main, "import %q\n", name)
	}
	files["main.bot"] = main.String()
	one := allocatedBy(func() {
		salvageUnit(LoadMap(map[string]string{"main.bot": "import \"lib/f0.bot\"\n", "lib/f0.bot": frag}, "main.bot"))
	})
	var u *Unit
	reads := map[string]int{}
	all := allocatedBy(func() {
		u = salvageUnit(Load(func(rel string) ([]byte, error) {
			reads[rel]++
			return []byte(files[rel]), nil
		}, "main.bot", func(rel string) string { return rel }))
	})
	t.Logf("one fragment: %d MiB, ten: %d MiB", one>>20, all>>20)
	if !u.HasErrors() {
		t.Fatal("ten fragments each within the budget, past it together, load whole")
	}
	if all > 2*one {
		t.Errorf("ten fragments cost the load %d MiB, one %d MiB: each file was read on a budget of its own", all>>20, one>>20)
	}
	// The main spends its imports, so the first fragment runs out of what
	// is left: it carries the bot's one refusal, and no fragment after it
	// is read.
	if len(reads) != 2 || u.ErrorCount() != 1 {
		t.Errorf("read %v and counted %d errors, want main.bot and lib/f0.bot read, the one refusal", reads, u.ErrorCount())
	}
}

// A main its caller parsed — an author document — spent part of the budget
// in its own reading: the fragments it imports share what is left.
func TestAParsedMainSharesTheBudgetWithItsFragments(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "frag.bot"), []byte(prompts("f", 450)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_DSL_READ_BUDGET", "4096")
	mainSrc := "import \"lib/frag.bot\"\n" + prompts("m", 450)
	pr := parsertest.Parse("main.bot.yaml", mainSrc)
	if len(pr.Diagnostics) != 0 || pr.Spent == 0 {
		t.Fatalf("the main alone: %v, spent %d", pr.Diagnostics, pr.Spent)
	}
	u := salvageUnit(LoadDirWithMainAST(filepath.Join(root, "main.bot"), "main.bot.yaml", pr.File, []byte(mainSrc), pr.Spent))
	refused := false
	for _, d := range u.Diagnostics {
		refused = refused || strings.Contains(d.Message, "the bot is past the read budget of 4096")
	}
	if !refused {
		t.Fatalf("a main spending %d and a fragment within the budget alone, past it together: want the bot refused, got %v", pr.Spent, u.Diagnostics)
	}
}

// The loader reports a thousand of each code of its own, as the parser
// does of a file: a block declaring one key 1501 times draws 1000 of its
// 1500 E010, the last saying how many more there were, and the load's
// error count is all of them. Its other codes keep their own.
func TestTheLoaderReportsAThousandOfEachCode(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	src := "import \"lib/missing.bot\"\nvars:\n" + strings.Repeat("  a: string\n", 1501) + "\nworkflow w:\n  entry: x\n"
	u := salvageUnit(LoadMap(map[string]string{"main.bot": src}, "main.bot"))
	var dups, missing []parser.Diagnostic
	for _, d := range u.Diagnostics {
		switch d.Code {
		case parser.DiagDuplicateDecl:
			dups = append(dups, d)
		case parser.DiagImportUnreadable:
			missing = append(missing, d)
		}
	}
	if len(dups) != 1000 || len(missing) != 1 {
		t.Fatalf("1500 duplicate keys and one missing fragment: want 1000 E010 and the E046 reported, got %d and %d", len(dups), len(missing))
	}
	if !strings.HasSuffix(dups[999].Message, "— and 500 more E010 errors not reported: a reading reports 1000 of each code and 2000 of each severity, every code's first kept") {
		t.Errorf("the last E010 kept says %q", dups[999].Message)
	}
	if got := u.ErrorCount(); got != 1501 {
		t.Errorf("ErrorCount() = %d, want 1501: every error of the load, reported or not", got)
	}
	if got := u.Files[0].ParseErrorCount(); got != 0 {
		t.Errorf("the main's ParseErrorCount() = %d, want 0: the duplicates are the loader's", got)
	}
	t.Setenv("ITERION_DSL_READ_BUDGET", "0")
	u = salvageUnit(LoadMap(map[string]string{"main.bot": src}, "main.bot"))
	dups = dups[:0]
	for _, d := range u.Diagnostics {
		if d.Code == parser.DiagDuplicateDecl {
			dups = append(dups, d)
		}
	}
	if len(dups) != 1500 {
		t.Errorf("with no budget the loader reports %d of its 1500 E010", len(dups))
	}
}

// A file's parse error count is the parse's own: a fragment of 1500
// unknown properties counts 1500 errors where its list holds 1000, and
// the load counts them too.
func TestAFileCountsItsParseErrors(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	main := "import \"lib/frag.bot\"\nworkflow w:\n  entry: a\n  a -> done\n"
	frag := "agent a:\n" + strings.Repeat("  bogus: 1\n", 1500)
	u := salvageUnit(LoadMap(map[string]string{"main.bot": main, "lib/frag.bot": frag}, "main.bot"))
	var f *File
	for i := range u.Files {
		if u.Files[i].Rel == "lib/frag.bot" {
			f = &u.Files[i]
		}
	}
	if f == nil {
		t.Fatalf("the fragment is not loaded: %+v", u.Files)
	}
	if len(f.ParseDiagnostics) != 1000 || f.ParseErrorCount() != 1500 {
		t.Errorf("the fragment reports %d and counts %d errors, want 1000 and 1500", len(f.ParseDiagnostics), f.ParseErrorCount())
	}
	if got := u.ErrorCount(); got < 1500 {
		t.Errorf("the load counts %d errors, want the fragment's 1500 among them", got)
	}
}

// spentOf is what src spends of the read budget, read alone with none.
func spentOf(t *testing.T, name, src string) int {
	t.Helper()
	t.Setenv("ITERION_DSL_READ_BUDGET", "0")
	res := parsertest.Parse(name, src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("%s alone: %v", name, res.Diagnostics)
	}
	return res.Spent
}

// A load the read budget cut short says so as a state of its own, the one
// every writer of a bot's files reads (Whole): the unit is not whole, its
// refusal is the diagnostic the bot was refused with — in the file the
// budget ran out in, which is read as nothing (File.Refused), or at the
// import of the first file it had none left for — and its revision is not
// the whole load's of the same files. A file past the size cap leaves the
// unit short the same way. Within the budget the load is whole.
func TestAUnitTheBudgetCutShortIsNotWhole(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n" + prompts("m", 10),
		"lib/a.bot": prompts("a", 10),
		"lib/b.bot": prompts("b", 10),
	}
	sm, sa, sb := spentOf(t, "main.bot", files["main.bot"]), spentOf(t, "lib/a.bot", files["lib/a.bot"]), spentOf(t, "lib/b.bot", files["lib/b.bot"])
	t.Setenv("ITERION_DSL_READ_BUDGET", strconv.Itoa(sm+sa+sb))
	whole := salvageUnit(LoadMap(files, "main.bot"))
	if !whole.Whole() || whole.HasErrors() {
		t.Fatalf("within the budget: Whole() = %v, diagnostics %v", whole.Whole(), whole.Diagnostics)
	}
	if _, ok := whole.Refusal(); ok {
		t.Error("a whole load reports a refusal")
	}
	for _, f := range whole.Files {
		if f.Refused != parser.BoundNone {
			t.Errorf("%s read whole reports Refused = %d", f.Rel, f.Refused)
		}
	}
	cases := []struct {
		name      string
		budget    int
		file      string
		line      int
		refusedIn string
		filesHeld int
	}{
		// b's own reading spends the last record: b is read as nothing.
		{"inside lib/b.bot", sm + sa + sb - 1, "lib/b.bot", 0, "lib/b.bot", 3},
		// main and a spend it all: b is refused at its import, unread.
		{"at the import of lib/b.bot", sm + sa, "main.bot", 2, "", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ITERION_DSL_READ_BUDGET", strconv.Itoa(c.budget))
			u := salvageUnit(LoadMap(files, "main.bot"))
			if u.Whole() {
				t.Fatalf("a load past the budget is whole: %v", u.Diagnostics)
			}
			d, ok := u.Refusal()
			if !ok || d.File != c.file || (c.line != 0 && d.Line != c.line) || d.Message != parser.UnitBudgetMessage(c.budget) || d.Hint != parser.ReadBudgetHint {
				t.Fatalf("Refusal() = %v %s (hint %q), want the bot's refusal in %s (line %d)", ok, d.Error(), d.Hint, c.file, c.line)
			}
			if len(u.Files) != c.filesHeld {
				t.Errorf("%d files held, want %d", len(u.Files), c.filesHeld)
			}
			for _, f := range u.Files {
				want := parser.BoundNone
				if f.Rel == c.refusedIn {
					want = parser.BoundBudget
				}
				if f.Refused != want {
					t.Errorf("%s: Refused = %d, want %d", f.Rel, f.Refused, want)
				}
			}
			if u.Digest == whole.Digest {
				t.Error("the short load of the files names the whole load's revision")
			}
		})
	}
	t.Run("a fragment past the size cap", func(t *testing.T) {
		t.Setenv("ITERION_DSL_READ_BUDGET", "0")
		big := map[string]string{"main.bot": "import \"lib/big.bot\"\n", "lib/big.bot": "#" + strings.Repeat("x", 10*1024*1024)}
		u := salvageUnit(LoadMap(big, "main.bot"))
		d, ok := u.Refusal()
		if u.Whole() || !ok || d.File != "lib/big.bot" || d.Line != 1 {
			t.Fatalf("a fragment past the size cap: Whole() = %v, Refusal() = %v %s", u.Whole(), ok, d.Error())
		}
		if f := u.Files[1]; f.Refused != parser.BoundSize {
			t.Errorf("lib/big.bot: Refused = %d, want BoundSize", f.Refused)
		}
	})
	t.Run("on disk, cut to relative names", func(t *testing.T) {
		root := t.TempDir()
		for rel, src := range files {
			full := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("ITERION_DSL_READ_BUDGET", strconv.Itoa(sm+sa+sb-1))
		u := salvageUnit(LoadDir(filepath.Join(root, "main.bot")))
		u.RelDiagnostics()
		if d, ok := u.Refusal(); !ok || d.File != "lib/b.bot" {
			t.Errorf("Refusal() after the root cut = %v %s, want lib/b.bot by its relative name", ok, d.Error())
		}
	})
}

// At exactly the budget spent, an import naming no file is said as that:
// the fragment is read before the budget refuses it, so the diagnostic
// names the defect the author can fix, never a budget the bot did not
// overspend — and the load, which no bound of the reader cut short, is
// whole.
func TestAMissingImportAtTheSpentBudgetIsSaidAsMissing(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/a.bot\"\nimport \"lib/missing.bot\"\n" + prompts("m", 10),
		"lib/a.bot": prompts("a", 10),
	}
	exact := spentOf(t, "main.bot", files["main.bot"]) + spentOf(t, "lib/a.bot", files["lib/a.bot"])
	t.Setenv("ITERION_DSL_READ_BUDGET", strconv.Itoa(exact))
	u := salvageUnit(LoadMap(files, "main.bot"))
	if len(u.Diagnostics) != 1 {
		t.Fatalf("want one diagnostic, got %v", u.Diagnostics)
	}
	if d := u.Diagnostics[0]; d.Code != parser.DiagImportUnreadable || !strings.Contains(d.Message, "no such fragment") || d.Line != 2 {
		t.Errorf("the missing import at the spent budget is said as %s, want E046 no such fragment at main.bot:2", d.Error())
	}
	if !u.Whole() || len(u.Unread) != 1 || u.Unread[0] != "lib/missing.bot" {
		t.Errorf("Whole() = %v, Unread = %v: want a whole load naming lib/missing.bot unread", u.Whole(), u.Unread)
	}
}
