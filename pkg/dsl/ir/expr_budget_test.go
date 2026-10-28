package ir

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/readbudget"
)

// operands joins n copies of ref with `+`: four tokens an operand, less one,
// the end of the expression one more.
func operands(ref string, n int) string {
	return strings.Repeat(ref+"+", n-1) + ref
}

// randomSum is an expression of n random operands — a letter and, half the
// time, a digit, never a keyword — joined by `+`: the ticket's shape, with
// no two readings sharing their text, so nothing an interning or a cache
// could do hides what a node costs.
func randomSum(n int, seed uint64) string {
	r := rand.New(rand.NewPCG(seed, 1))
	var b strings.Builder
	b.Grow(4 * n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte('+')
		}
		b.WriteByte(byte('a' + r.IntN(26)))
		if r.IntN(2) == 0 {
			b.WriteByte(byte('0' + r.IntN(10)))
		}
	}
	return b.String()
}

// allocatedBy is the number of bytes the heap handed out while f ran.
func allocatedBy(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func computeBot(fields string) string {
	return "schema out:\n  v: int\n\ncompute c:\n  output: out\n  expr:\n" + fields + "\nworkflow w:\n  entry: c\n  c -> done\n"
}

// The ticket's shape: one string of a .bot holding an expression of
// hundreds of thousands of operators — the lexer counts the string as one
// record, so the read budget never saw it, and the compile built the whole
// tree (480 MiB retained at 5 million; past a million levels the compiler's
// walks overflow the goroutine stack). Held to the budget, the compile
// refuses the expression at the token the count ran out on, positioned at
// the compute node and naming the remedy, and allocates under 128 MiB
// whatever the expression's length (the parse of the source is measured
// apart: its runes, its one string token). Mutate the spend out of the
// expression parser's advance and the same compile builds the 650 000
// operands — past 600 MiB, no refusal — and fails here on both counts.
func TestAnExpressionPastTheBudgetIsRefusedAtTheComputeNode(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	src := computeBot("    v: \"" + randomSum(650_000, 2627) + "\"\n")
	var pr *parser.ParseResult
	parsed := allocatedBy(func() { pr = parsertest.Parse("big.bot", src) })
	if pr.File == nil {
		t.Fatalf("no File for a %d-byte source: %+v", len(src), pr.Diagnostics)
	}
	var cr *CompileResult
	alloc := allocatedBy(func() { cr = Compile(pr.File) })
	t.Logf("%d bytes: parse allocated %d MiB, compile %d MiB", len(src), parsed>>20, alloc>>20)
	if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
		t.Fatalf("C320 count = %d, want 1\ndiagnostics: %d", got, len(cr.Diagnostics))
	}
	d := diagFor(cr, DiagExprPastReadBudget)
	if d.NodeID != "c" || d.Line != 4 {
		t.Errorf("C320 at node %q line %d, want the compute node c at line 4", d.NodeID, d.Line)
	}
	if d.Hint != expr.BudgetHint || d.Hint != HintFor(DiagExprPastReadBudget) {
		t.Errorf("C320 hint %q, want the budget's remedy", d.Hint)
	}
	if len(d.Message) > 1000 {
		t.Errorf("C320 message is %d bytes: it echoes the expression", len(d.Message))
	}
	if bound := uint64(128 << 20); alloc > bound {
		t.Errorf("compiling an expression of %d bytes past the budget allocated %d MiB, want under %d MiB", len(src), alloc>>20, bound>>20)
	}
}

// Every site the compiler parses an expression at refuses one past the
// budget with C320, positioned where the expression stands: a `when`
// clause, a loop cap, a fallback route's `when:`. Each on a budget of 4 096
// records, an expression of 5 000 — the loop cap of 3 000, which the parser
// let through on the cap's own budget, after a compute field spent 2 000 of
// the bot's: the compiler reads computes before edges.
func TestEveryExpressionSiteRefusesPastTheBudget(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	long := operands("input.n", 1_250)
	for _, c := range []struct {
		name, src, nodeID, edgeID string
	}{
		{"when", "agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b when \"" + long + "\"\n  a -> done else\n  b -> done\n", "", "a->b"},
		{"loop cap", "schema out:\n  v: int\n\ncompute c:\n  output: out\n  expr:\n    v: \"" + operands("input.n", 500) + "\"\n\nagent a:\n  model: \"m\"\n\nworkflow w:\n  entry: c\n  c -> a\n  a -> done else\n  a -> a as l(\"" + operands("input.n", 750) + "\")\n", "", "a->a"},
		{"fallback when", "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, bash]\n  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n      when: \"" + operands("vars.n", 1_250) + "\"\n\nprompt p:\n  hi\n\nworkflow w:\n  vars:\n    n: int = 1\n  entry: x\n  x -> done\n", "x", ""},
	} {
		cr := compileText(t, c.src)
		if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
			t.Errorf("%s: C320 count = %d, want 1\ndiagnostics: %+v", c.name, got, cr.Diagnostics)
			continue
		}
		d := diagFor(cr, DiagExprPastReadBudget)
		if d.NodeID != c.nodeID || d.EdgeID != c.edgeID || d.Line == 0 {
			t.Errorf("%s: C320 at node %q edge %q line %d, want node %q edge %q on a line", c.name, d.NodeID, d.EdgeID, d.Line, c.nodeID, c.edgeID)
		}
		if d.Hint != expr.BudgetHint || strings.Contains(d.Message, "input.n+input.n") || strings.Contains(d.Message, "vars.n+vars.n") {
			t.Errorf("%s: hint %q, message of %d bytes: want the remedy, the expression's text left out", c.name, d.Hint, len(d.Message))
		}
	}
}

// The expressions of a bot share one budget: two compute fields of 2 400
// records each read alone on 4 096 and together the second is refused —
// the bot is held to the count whole, however it splits its expressions.
func TestTheExpressionsOfABotShareTheBudget(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	field := operands("input.n", 600)
	one := compileText(t, computeBot("    v: \""+field+"\"\n"))
	if got := countByCode(one, string(DiagExprPastReadBudget)); got != 0 {
		t.Fatalf("one field of 2 400 records on 4 096: C320 count = %d, want 0", got)
	}
	two := compileText(t, computeBot("    v: \""+field+"\"\n    w: \""+field+"\"\n"))
	if got := countByCode(two, string(DiagExprPastReadBudget)); got != 1 {
		t.Fatalf("two fields of 2 400 records on 4 096: C320 count = %d, want 1 (the second)\ndiagnostics: %+v", got, two.Diagnostics)
	}
	if d := diagFor(two, DiagExprPastReadBudget); !strings.Contains(d.Message, `field "w"`) || !strings.Contains(d.Message, "spent 2400 of the count") {
		t.Errorf("C320 says %q, want the second field named and the first's 2 400 records blamed", d.Message)
	}
}

// A loop cap past the budget on its own is refused by the compiler at the
// edge, as C320 and nothing else: the parser kept the cap's text for it, so
// the loop is not read as capless (no C026).
func TestALoopCapAlonePastTheBudgetIsRefusedAtTheEdge(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	src := "agent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> done else\n  a -> a as l(\"" + operands("input.n", 1_250) + "\")\n"
	cr := compileText(t, src)
	if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
		t.Fatalf("C320 count = %d, want 1\ndiagnostics: %+v", got, cr.Diagnostics)
	}
	if d := diagFor(cr, DiagExprPastReadBudget); d.EdgeID != "a->a" || d.Line != 7 {
		t.Errorf("C320 at edge %q line %d, want the loop edge a->a at line 7", d.EdgeID, d.Line)
	}
	if got := countByCode(cr, "C026"); got != 0 {
		t.Errorf("C026 count = %d: the refused cap was read as no cap at all\ndiagnostics: %+v", got, cr.Diagnostics)
	}
}

// A loop's cap is read once per edge that carries the loop
// (validateLoopCapExpressions walks it per edge), so every edge spends it:
// two edges sharing a cap of 2 400 records on a budget of 4 096 are refused
// at the second, and compile whole on 8 192.
func TestASharedLoopCapIsSpentOncePerEdge(t *testing.T) {
	cap := operands("input.n", 600)
	src := "agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b as l(\"" + cap + "\")\n  b -> a as l(\"" + cap + "\")\n  a -> done else\n"
	t.Setenv(readbudget.Env, "8192")
	if cr := compileText(t, src); countByCode(cr, string(DiagExprPastReadBudget)) != 0 {
		t.Fatalf("two readings of 2 400 on 8 192: refused\ndiagnostics: %+v", cr.Diagnostics)
	}
	t.Setenv(readbudget.Env, "4096")
	cr := compileText(t, src)
	if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
		t.Fatalf("two readings of 2 400 on 4 096: C320 count = %d, want 1 (the second edge)\ndiagnostics: %+v", got, cr.Diagnostics)
	}
	if d := diagFor(cr, DiagExprPastReadBudget); d.EdgeID != "b->a" || !strings.Contains(d.Message, "read again for this edge") || !strings.Contains(d.Message, "spent 2400 of the count") {
		t.Errorf("C320 at edge %q says %q, want the second edge b->a, read again, the first's 2 400 blamed", d.EdgeID, d.Message)
	}
}

// Which node's fallback `when:` the budget runs out on is decided by the
// nodes' ids, never by a map's order: three agents of 6 001 records each on
// a budget of 16 000 refuse the third, x2, on every compile.
func TestTheRefusedFallbackWhenIsTheSameOnEveryCompile(t *testing.T) {
	t.Setenv(readbudget.Env, "16000")
	var b strings.Builder
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, "agent x%d:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, bash]\n  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n      when: \"%s\"\n\n", i, operands("vars.n", 1_500))
	}
	b.WriteString("prompt p:\n  hi\n\nworkflow w:\n  vars:\n    n: int = 1\n  entry: x0\n  x0 -> x1\n  x1 -> x2\n  x2 -> done\n")
	src := b.String()
	for i := 0; i < 5; i++ {
		cr := compileText(t, src)
		if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
			t.Fatalf("compile %d: C320 count = %d, want 1\ndiagnostics: %+v", i, got, cr.Diagnostics)
		}
		if d := diagFor(cr, DiagExprPastReadBudget); d.NodeID != "x2" {
			t.Fatalf("compile %d: C320 at node %q, want x2, the last in id order", i, d.NodeID)
		}
	}
}

// A `when` past the budget is positioned on its own edge's line: two edges
// may share their endpoints, and the id alone would name the first.
func TestAWhenPastTheBudgetIsPositionedOnItsOwnEdge(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	src := "agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b when \"input.x\"\n  a -> b when \"" + operands("input.n", 1_250) + "\"\n  a -> done else\n  b -> done\n"
	cr := compileText(t, src)
	if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
		t.Fatalf("C320 count = %d, want 1\ndiagnostics: %+v", got, cr.Diagnostics)
	}
	if d := diagFor(cr, DiagExprPastReadBudget); d.Line != 9 || d.EdgeID != "a->b" {
		t.Errorf("C320 at line %d edge %q, want the second a->b edge's line 9", d.Line, d.EdgeID)
	}
}

// A diagnostic quotes an expression by an excerpt: a per-reference message
// that quoted a long `when` whole once per reference grew as references ×
// bytes — 2 000 `run.*` references in a 20 KB clause made 400 MB of
// diagnostics. Now each quotes 64 runes and an ellipsis, and a long invalid
// expression's message is bounded the same way.
func TestDiagnosticsQuoteAnExpressionByExcerpt(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	var when strings.Builder
	for i := 0; i < 2_000; i++ {
		if i > 0 {
			when.WriteString(" || ")
		}
		fmt.Fprintf(&when, "run.q%d", i)
	}
	src := "agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b when \"" + when.String() + "\"\n  a -> done else\n  b -> done\n"
	cr := compileText(t, src)
	total := 0
	for _, d := range cr.Diagnostics {
		total += len(d.Message)
		if len(d.Message) > 2_000 {
			t.Fatalf("a %d-byte diagnostic: %s echoes the clause", len(d.Message), d.Message[:200])
		}
	}
	if len(cr.Diagnostics) < 100 || total > 1<<20 {
		t.Errorf("%d diagnostics, %d bytes in all: want one per reference, under a MiB together", len(cr.Diagnostics), total)
	}
	bad := compileText(t, computeBot("    v: \""+strings.Repeat("input.n+", 10_000)+"\"\n"))
	d := diagFor(bad, DiagBadExpr)
	if d == nil {
		t.Fatalf("a dangling `+` drew no C040: %+v", bad.Diagnostics)
	}
	if len(d.Message) > 500 || !strings.Contains(d.Message, "…") {
		t.Errorf("C040 is %d bytes: %.160s — want the expression quoted by a 64-rune excerpt", len(d.Message), d.Message)
	}
}

// A fallback route's `when:` is parsed twice by the compiler — held to the
// bot's budget by checkFallbackWhen, read again by validateVarListDefaults
// when a var's default shifts (C155 says the second reading ran) — and spent
// once: an expression of 3 006 records on a budget of 4 096 compiles. Mutate
// the second reading onto the shared budget and the bot its first reading
// accepted is refused.
func TestAFallbackWhenIsSpentOnce(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	src := "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, bash]\n  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n      when: \"vars.tags == 'a,b' || " + operands("vars.n", 750) + "\"\n\nprompt p:\n  hi\n\nworkflow w:\n  vars:\n    n: int = 1\n    tags: string[] = \"a,b\"\n  entry: x\n  x -> done\n"
	cr := compileText(t, src)
	if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 0 {
		t.Fatalf("a fallback when of 3 001 records on 4 096: C320 count = %d, want 0 — the second reading is spending the bot's budget\ndiagnostics: %+v", got, cr.Diagnostics)
	}
	if got := countByCode(cr, string(DiagVarListDefaultRouting)); got == 0 {
		t.Fatalf("no C155: the var default did not shift, so the second reading never ran\ndiagnostics: %+v", cr.Diagnostics)
	}
}

// Every site that quotes what an author wrote — an expression, a reference,
// a name — quotes it by excerpt (expr.Excerpt). A name or a string of any
// length is one token of the read budget, and a message said once per
// reference or once per edge that quoted it whole grew as references ×
// bytes: one probe per site, a 100 000-rune text, every message and hint
// held under a kilobyte.
func TestEverySiteQuotesByExcerpt(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	long := strings.Repeat("x", 100_000)
	agents := "agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\n"
	for _, c := range []struct{ name, src, said string }{
		{"C005 conflicting caps", agents + "workflow w:\n  entry: a\n  a -> b as l(\"vars." + long + "\")\n  b -> a as l(\"vars." + long + "y\")\n  a -> done else\n", "conflicting max_iterations"},
		{"a vars reference per edge", agents + "workflow w:\n  entry: a\n  a -> b as l(\"vars." + long + "\")\n  b -> a as l(\"vars." + long + "\")\n  a -> done else\n", "vars." + long[:10]},
		{"an outputs reference per edge", agents + "workflow w:\n  entry: a\n  a -> b as l(\"outputs.a." + long + "\")\n  b -> a as l(\"outputs.a." + long + "\")\n  a -> done else\n", "outputs.a." + long[:10]},
		{"the cap's own location", agents + "workflow w:\n  entry: a\n  a -> b as l(\"input." + long + "\")\n  a -> done else\n  b -> done\n", "expressions may reference vars and outputs"},
		{"a template cap", agents + "workflow w:\n  entry: a\n  a -> b as l(\"{{vars." + long + "}} {{vars.y}}\")\n  a -> done else\n  b -> done\n", "a template cap must be one integer reference"},
		{"an invalid when", agents + "workflow w:\n  entry: a\n  a -> b when \"input." + long + " +\"\n  a -> done else\n  b -> done\n", "invalid `when` expression"},
		{"a cap's namespace", agents + "workflow w:\n  entry: a\n  a -> b as l(\"" + long + ".x\")\n  a -> done else\n  b -> done\n", "expressions may reference vars and outputs, got"},
		{"C031 a field not in the schema", "schema s:\n  v: string\n\nagent a:\n  model: \"m\"\n  output: s\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b as l(\"outputs.a." + long + "\")\n  a -> done else\n  b -> done\n", "not found in output schema"},
		{"C308 a long field name", "schema s:\n  v: string\n\nprompt p:\n  {{input." + long + "}}\n\nagent a:\n  model: \"m\"\nagent z:\n  model: \"m\"\n  input: s\n  system: p\n\nworkflow w:\n  entry: a\n  a -> z\n  z -> done\n", "none of the node's incoming edges maps"},
		{"a legacy when field in an edge's name", "schema s:\n  v: string\n  ok: bool\n\nprompt p:\n  {{input.x}}\n\nagent a:\n  model: \"m\"\n  output: s\nagent z:\n  model: \"m\"\n  input: s\n  system: p\n\nworkflow w:\n  entry: a\n  a -> z when " + long + "\n  a -> done else\n  z -> done\n", "none of the node's incoming edges maps"},
		{"C155 a list default", "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, bash]\n  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n      when: \"vars.tags == 'a'\"\n\nprompt p:\n  hi\n\nworkflow w:\n  vars:\n    tags: string[] = \"" + long + ",b\"\n  entry: x\n  x -> done\n", "whose default the run reads as"},
		{"C121 an enum compared to a literal", "schema s:\n  kind: string [enum: \"a\", \"b\"]\n\nagent a:\n  model: \"m\"\n  output: s\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b when \"outputs.a.kind == '" + long + "'\"\n  a -> done else\n  b -> done\n", "whose enum is"},
		{"C040 a trailing token", agents + "workflow w:\n  entry: a\n  a -> b when \"input.x " + long + "\"\n  a -> done else\n  b -> done\n", "unexpected trailing"},
		{"C269 two foreach collections", agents + "workflow w:\n  entry: a\n  a -> b as foreach scan(item in \"{{outputs.a." + long + "}}\")\n  b -> a as foreach scan(item in \"{{outputs.a." + long + "y}}\")\n  a -> done else\n", "declared twice over different iteration"},
		{"the vars hint on a source with a schema", "schema s:\n  v: string\n\nagent a:\n  model: \"m\"\n  output: s\nagent b:\n  model: \"m\"\n\nworkflow w:\n  vars:\n    " + long + ": string = \"x\"\n  entry: a\n  a -> b with { k: \"{{input." + long + "}}\" }\n  b -> done\n", "use {{vars."},
		{"C137 a quoted reference", "tool t:\n  command: \"echo '{{vars." + long + "}}'\"\n\nworkflow w:\n  vars:\n    " + long + ": string = \"x\"\n  entry: t\n  t -> done\n", "sits inside quotes"},
		{"C137 a raw quoted reference", "tool t:\n  command: \"echo '{{!vars." + long + "}}'\"\n\nworkflow w:\n  vars:\n    " + long + ": string = \"x\"\n  entry: t\n  t -> done\n", "is raw"},
		{"C137 a quoted artifact reference", "tool t:\n  command: \"echo '{{artifacts." + long + "}}'\"\n\nworkflow w:\n  entry: t\n  t -> done\n", "command execution"},
	} {
		cr := compileText(t, c.src)
		found := false
		for _, d := range cr.Diagnostics {
			if strings.Contains(d.Message, c.said) {
				found = true
			}
			if len(d.Message) > 1_000 || len(d.Hint) > 1_000 {
				t.Errorf("%s: a %d-byte message / %d-byte hint (%s): %.120s…", c.name, len(d.Message), len(d.Hint), d.Code, d.Message)
			}
		}
		if !found {
			t.Errorf("%s: no diagnostic says %q\ndiagnostics: %+v", c.name, c.said[:min(len(c.said), 40)], cr.Diagnostics)
		}
	}
}

// C308 names a node's incoming edges once per unmapped input reference: the
// first five by name — an edge's `when` quoted by excerpt — and the rest by
// count, so a node of many edges and many references is warned about in
// kilobytes, not megabytes.
func TestInputNeverMappedNamesFiveEdgesAndCountsTheRest(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	var b strings.Builder
	b.WriteString("schema s:\n  x: string\n\nprompt p:\n  {{input.x}} {{input.y}}\n\nagent z:\n  model: \"m\"\n  input: s\n  system: p\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  model: \"m\"\n", i)
	}
	b.WriteString("\nworkflow w:\n  entry: a0\n")
	for i := 0; i < 12; i++ {
		if i > 0 {
			fmt.Fprintf(&b, "  a%d -> a%d\n", i-1, i)
		}
		if i == 3 {
			fmt.Fprintf(&b, "  a%d -> z when \"input.%s\"\n", i, strings.Repeat("w", 50_000))
		} else {
			fmt.Fprintf(&b, "  a%d -> z\n", i)
		}
	}
	b.WriteString("  z -> done\n")
	cr := compileText(t, b.String())
	n := 0
	for _, d := range cr.Diagnostics {
		if d.Code != DiagInputFieldNeverMapped {
			continue
		}
		n++
		if !strings.Contains(d.Message, "and 7 more") || !strings.Contains(d.Hint, "and 7 more") || len(d.Message) > 1_500 || len(d.Hint) > 1_500 {
			t.Errorf("C308 is %d bytes with a %d-byte hint: %.300s… / hint %.300s…", len(d.Message), len(d.Hint), d.Message, d.Hint)
		}
	}
	if n < 2 {
		t.Fatalf("%d C308 for two unmapped references\ndiagnostics: %+v", n, cr.Diagnostics)
	}
}

// Each fallback route's `when:` reading is kept under its own key — the node
// and the route's index — so a refused second route does not erase the
// first's reading: the first, which reads a shifted var, still draws C155,
// and the second its C320.
func TestEachFallbackRouteKeepsItsOwnReading(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	src := "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, bash]\n  fallbacks:\n    first:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n      when: \"vars.tags == 'a,b'\"\n    second:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n      when: \"" + operands("vars.n", 1_250) + "\"\n\nprompt p:\n  hi\n\nworkflow w:\n  vars:\n    n: int = 1\n    tags: string[] = \"a,b\"\n  entry: x\n  x -> done\n"
	cr := compileText(t, src)
	if got := countByCode(cr, string(DiagExprPastReadBudget)); got != 1 {
		t.Fatalf("C320 count = %d, want 1 (the second route)\ndiagnostics: %+v", got, cr.Diagnostics)
	}
	if got := countByCode(cr, string(DiagVarListDefaultRouting)); got == 0 {
		t.Fatalf("no C155 for the first route's reading of vars.tags: the second route's refusal erased it\ndiagnostics: %+v", cr.Diagnostics)
	}
}

// Two caps that differ only past the first 64 runes are quoted around their
// first difference, so C005 does not show two identical excerpts as
// "conflicting" — and it stands on the edge that re-declares the loop.
func TestConflictingCapsAreQuotedAroundTheirDifference(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	head := "vars." + strings.Repeat("n", 66)
	src := "agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b as l(\"" + head + " + 1\")\n  b -> a as l(\"" + head + " + 2\")\n  a -> done else\n"
	cr := compileText(t, src)
	d := diagFor(cr, DiagDuplicateLoop)
	if d == nil {
		t.Fatalf("no C005: %+v", cr.Diagnostics)
	}
	if !strings.Contains(d.Message, "+ 1") || !strings.Contains(d.Message, "+ 2") {
		t.Errorf("C005 %q: the two caps read the same, want them quoted around their difference", d.Message)
	}
	if d.Line != 9 || d.EdgeID != "b->a" {
		t.Errorf("C005 at line %d edge %q, want the re-declaring edge b->a at line 9", d.Line, d.EdgeID)
	}
}

// The six shapes the fourth review round measured to an out-of-memory on a
// valid bot, each held under a kilobyte of diagnostic text.
func boundedDiagnosticsOf(t *testing.T, name, src string, code DiagCode) *CompileResult {
	t.Helper()
	cr := compileText(t, src)
	n := 0
	for _, d := range cr.Diagnostics {
		if d.Code != code {
			continue
		}
		n++
		if len(d.Message) > 1_000 || len(d.Hint) > 1_000 {
			t.Errorf("%s: %s is %d bytes with a %d-byte hint", name, d.Code, len(d.Message), len(d.Hint))
		}
	}
	if n == 0 {
		t.Errorf("%s: no %s\ndiagnostics: %.600v", name, code, cr.Diagnostics)
	}
	return cr
}

func TestAGroupWithMappingOfALongVarIsBounded(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	v := "v" + strings.Repeat("x", 100_000)
	src := "group g:\n  agent a:\n    model: \"m\"\n  agent b:\n    model: \"m\"\n  a -> b with { k: \"{{input." + v + "}}\" }\n\nuse g as i0\nuse g as i1\n\nworkflow w:\n  vars:\n    " + v + ": string = \"x\"\n  entry: i0.a\n  i0.b -> i1.a\n  i1.b -> done\n"
	boundedDiagnosticsOf(t, "group with-mapping of a declared var", src, DiagRefNodeNoSchema)
}

func TestTheRouterHintOfALongFieldIsBounded(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	f := "f" + strings.Repeat("x", 100_000)
	src := "agent s:\n  model: \"m\"\nagent a:\n  model: \"m\"\nrouter r:\n  mode: condition\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: s\n  s -> a\n  a -> r\n  r -> b with { k: \"{{input." + f + "}}\" }\n  b -> done\n"
	boundedDiagnosticsOf(t, "router C032 hint", src, DiagRefNodeNoSchema)
}

func TestAnUnknownArtifactInASharedPromptIsBounded(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	a := "a" + strings.Repeat("x", 100_000)
	src := "prompt p:\n  {{artifacts." + a + "}}\n\nagent s:\n  model: \"m\"\nagent a0:\n  model: \"m\"\n  system: p\nagent a1:\n  model: \"m\"\n  system: p\n\nworkflow w:\n  entry: s\n  s -> a0\n  a0 -> a1\n  a1 -> done\n"
	boundedDiagnosticsOf(t, "C035 unknown artifact", src, DiagUnknownArtifact)
}

func TestAPresetOfALongEnumIsBounded(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	src := "vars:\n  mode: string [enum: \"" + strings.Repeat("A", 100_000) + "\", \"" + strings.Repeat("B", 100_000) + "\"]\n\npresets:\n  p0:\n    mode: \"z0\"\n  p1:\n    mode: \"z1\"\n\nagent runner:\n  model: \"m\"\n\nworkflow demo:\n  entry: runner\n  runner -> done\n"
	boundedDiagnosticsOf(t, "C165 preset enum", src, DiagPresetViolatesConstraint)
}

func TestForeachItemsAreQuotedAroundTheirDifference(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	i1 := "item_for_the_release_operator_review_iteration_budget_staging_pipeline_v1"
	i2 := "item_for_the_release_operator_review_iteration_budget_staging_pipeline_v2"
	src := fmt.Sprintf("agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b as foreach scan(%s in \"{{outputs.a.l}}\")\n  b -> a as foreach scan(%s in \"{{outputs.a.l}}\")\n  a -> done else\n", i1, i2)
	cr := compileText(t, src)
	d := diagFor(cr, DiagDuplicateForeach)
	if d == nil {
		t.Fatalf("no C269: %+v", cr.Diagnostics)
	}
	if !strings.Contains(d.Message, "v1") || !strings.Contains(d.Message, "v2") {
		t.Errorf("C269 %q: the two items read the same", d.Message)
	}
}

func TestAnEnumMismatchNamesTheMisspelledMember(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	src := "schema s:\n  cat: string [enum: \"companion_bootstrap\", \"broad_content_cleanup\", \"repository_wide_refactor\", \"architecture_expansion\", \"new_workstream\", \"external_system_change\", \"dependency_addition\", \"broad_purge\"]\n\nagent a:\n  model: \"m\"\n  output: s\nagent b:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> b when \"outputs.a.cat == 'broad_purg'\"\n  a -> done else\n  b -> done\n"
	cr := compileText(t, src)
	d := diagFor(cr, DiagEnumLiteralMismatch)
	if d == nil {
		t.Fatalf("no C121: %+v", cr.Diagnostics)
	}
	if !strings.Contains(d.Message, "broad_purge") {
		t.Errorf("C121 %q: the member the literal misspells is not shown", d.Message)
	}
}

// Every diagnostic's message and hint are held to expr.MaxDiagnosticText
// where they are recorded, whatever a site composed: the one bound no
// quoting site can undo.
func TestEveryDiagnosticTextIsBoundedAtEmit(t *testing.T) {
	c := &compiler{}
	huge := strings.Repeat("é", 1<<20)
	c.emit(SeverityError, DiagBadExpr, "n", "", ast.Span{}, huge, "%s", huge)
	d := c.diags[0]
	if len(d.Message) > expr.MaxDiagnosticText+len("…") || !strings.HasSuffix(d.Message, "…") || !utf8.ValidString(d.Message) {
		t.Errorf("a %d-byte message was recorded as %d bytes (valid UTF-8: %v)", len(huge), len(d.Message), utf8.ValidString(d.Message))
	}
	if len(d.Hint) > expr.MaxDiagnosticText+len("…") || !strings.HasSuffix(d.Hint, "…") {
		t.Errorf("a %d-byte hint was recorded as %d bytes", len(huge), len(d.Hint))
	}
	c.errorf(DiagBadExpr, "%s", "short")
	if c.diags[1].Message != "short" {
		t.Errorf("a short message changed: %q", c.diags[1].Message)
	}
}

// enumList names an enum's first sixteen members and counts the rest.
func TestEnumListNamesSixteenAndCountsTheRest(t *testing.T) {
	members := make([]string, 17)
	for i := range members {
		members[i] = fmt.Sprintf("m%02d", i)
	}
	got := enumList(members)
	if !strings.HasSuffix(got, "and 1 more") || strings.Contains(got, "m16") || !strings.Contains(got, `"m15"`) {
		t.Errorf("enumList(17) = %q", got)
	}
	if got := enumList(members[:16]); strings.Contains(got, "more") {
		t.Errorf("enumList(16) counts: %q", got)
	}
}
