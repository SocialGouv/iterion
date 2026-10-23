package cli

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// allocatedWhile is the number of bytes the heap handed out while f ran.
func allocatedWhile(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// jsonPromptBot is a bot whose system prompt holds a JSON example of n
// rows: punctuation in TEXT, which the .bot reader charges a line, and a
// count of YAML indicators would charge for every `:` `,` `{` of it.
func jsonPromptBot(n int) string {
	var b strings.Builder
	b.WriteString("dsl: 2\n\nschema r:\n  ok: bool\n\nprompt sys:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  {\"id\": %d, \"name\": \"row-%d\", \"ok\": true},\n", i, i)
	}
	b.WriteString("\nagent a:\n  model: \"anthropic/claude-sonnet-4-6\"\n  backend: \"claw\"\n  output: r\n  system: sys\n\nworkflow w:\n\n  entry: a\n\n  a -> done\n")
	return b.String()
}

// A bot whose prompt holds a JSON example of 56 000 rows — 2.8 MB, some 56
// thousand records of the 2^20 budget — converts to its YAML twin (proven
// the same program before it is written), and the twin reads back,
// canonical as written. Its `|` block holds over a million of the
// indicators a count of the text would read as values; the document holds
// some forty.
func TestAPromptHoldingAJSONExampleConvertsAndReadsBack(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	inTempWorkspace(t)
	jp, _ := jsonPrinter()
	bot := writeBot(t, "d/p.bot", jsonPromptBot(56_000))
	if _, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("fmt --to yaml of a bot whose prompt holds a JSON example: %v", err)
	}
	doc := "d/p.bot.yaml"
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("the document was not written: %v", err)
	}
	if res := author.Parse(doc, raw); res.HasErrors() {
		t.Fatalf("the document written does not read: %v", res.Diagnostics[0])
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, Check: true, Printer: jp}); err != nil {
		t.Errorf("fmt --check of the document written: %v", err)
	}
}

// denseDocument is a document of a flow mapping of n null-valued keys: a
// value a byte, the densest text there is.
func denseDocument(n int) string {
	return "dsl: 2\nx: {" + strings.Repeat("a,", n) + "a}\n"
}

// `fmt --to yaml --force` replaces a twin it says what was carried by: a
// twin whose indicators could open more values than its reader decodes is
// not decoded — not as one YAML document, not for its comments — and the
// note says what it carries is not counted. Decoded, 1.6 million values a
// reading would cost the two readings some 700 MiB.
func TestForceOnATwinPastTheDecodeCeilingDecodesNothing(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	inTempWorkspace(t)
	jp, _ := jsonPrinter()
	bot := writeBot(t, "d/x.bot", "dsl: 2\n\nagent hello:\n  model: \"m\"\n  system: \"Say hello.\"\n\nworkflow hello:\n  entry: hello\n\n  hello -> done\n")
	writeBot(t, "d/x.bot.yaml", denseDocument(1_600_000))
	var res FmtResult
	var err error
	alloc := allocatedWhile(func() {
		res, err = RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Force: true, Check: true, Printer: jp})
	})
	t.Logf("allocated %d MiB", alloc>>20)
	if !errors.Is(err, ErrFmtWouldChange) {
		t.Fatalf("--to yaml --force --check over a dense twin: %v", err)
	}
	said := false
	for _, n := range res.Notices {
		said = said || strings.Contains(n, "d/x.bot.yaml: --force replaces it whole — it does not read as one YAML document within its reader's bounds") && strings.Contains(n, "is not counted")
	}
	if !said {
		t.Errorf("the note does not say the dense twin's comments are not counted: %q", res.Notices)
	}
	if limit := uint64(64 << 20); alloc > limit {
		t.Errorf("--force over a dense twin allocated %d MiB, want under %d MiB: the twin was decoded", alloc>>20, limit>>20)
	}
}

// A `.bot` frontmatter block is held to the read budget by what its decode
// costs, not by the two records each of its lines spends as a comment: one
// dense line of a block — some sixty records of the .bot — is not decoded,
// by the writer or by the note that says why the twin carries no
// `catalog:`. Decoded, its 1.6 million values would cost a reading some
// 500 MiB.
func TestADenseFrontmatterIsNotDecoded(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	inTempWorkspace(t)
	jp, _ := jsonPrinter()
	botOf := func(line string) string {
		return "## ---\n## name: f\n## x: " + line + "\n## ---\n\ndsl: 2\n\nagent hello:\n  model: \"m\"\n  system: \"Say hello.\"\n\nworkflow hello:\n  entry: hello\n\n  hello -> done\n"
	}
	convert := func(path, src string) (FmtResult, uint64, error) {
		writeBot(t, path, src)
		if pr := parsertest.Parse(path, src); pr.ErrorCount != 0 || pr.Spent > 100 {
			t.Fatalf("%s reads with %d errors, %d records spent: not the case under test", path, pr.ErrorCount, pr.Spent)
		}
		var res FmtResult
		var err error
		alloc := allocatedWhile(func() {
			res, err = RunFmt(FmtOptions{Paths: []string{path}, To: "yaml", Check: true, Printer: jp})
		})
		return res, alloc, err
	}
	// The control: a line of the same length holding one plain value.
	_, plain, _ := convert("d/plain.bot", botOf(strings.Repeat("ab", 1_600_001)))
	res, dense, err := convert("d/f.bot", botOf("{"+strings.Repeat("a,", 1_600_000)+"a}"))
	t.Logf("allocated %d MiB, %d MiB for the plain line of the same length", dense>>20, plain>>20)
	if !errors.Is(err, ErrFmtWouldChange) {
		t.Fatalf("--to yaml --check of a bot with a dense frontmatter: %v", err)
	}
	said := false
	for _, n := range res.Notices {
		said = said || strings.Contains(n, "the frontmatter is not read") && strings.Contains(n, parser.ReadBudgetEnv)
	}
	if !said {
		t.Errorf("the note does not say the frontmatter is not read, nor the operator's count: %q", res.Notices)
	}
	if dense > plain+64<<20 {
		t.Errorf("a dense frontmatter allocated %d MiB, the plain line of its length %d MiB: the block was decoded", dense>>20, plain>>20)
	}
}
