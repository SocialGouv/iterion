package expr

import (
	"errors"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/readbudget"
)

// sumOf is an expression of n operands joined by `+` — 2n-1 tokens, the
// end of the expression one more — its operands random names of a letter
// and, half the time, a digit (never a keyword), so no two readings of the
// shape share their text and nothing an interning or a cache could do
// hides what a node costs.
func sumOf(n int, seed uint64) string {
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

// The budget counts the tokens of an expression, its end included: on a
// budget of eight, `a+b+c+d` — seven tokens and the end — reads, and
// `a+b+c+d+e` is refused as a *BudgetError naming the count. 0 removes
// the budget; the knob is the read budget's, ITERION_DSL_READ_BUDGET.
func TestParseIsHeldToTheReadBudget(t *testing.T) {
	t.Setenv(readbudget.Env, "8")
	if _, err := Parse("a+b+c+d"); err != nil {
		t.Fatalf("eight records read on a budget of eight: %v", err)
	}
	_, err := Parse("a+b+c+d+e")
	var past *BudgetError
	if !errors.As(err, &past) {
		t.Fatalf("ten records on a budget of eight: %v, want a *BudgetError", err)
	}
	if past.Limit != 8 || past.Before != 0 || !strings.Contains(err.Error(), "read budget of 8") || strings.Contains(err.Error(), "read before it") {
		t.Errorf("the refusal names %d after %d and says %q, want the budget of 8 alone", past.Limit, past.Before, err)
	}
	// The end of the expression is a record: on a budget of seven the
	// same seven tokens are refused at their end.
	t.Setenv(readbudget.Env, "7")
	if _, err := Parse("a+b+c+d"); !errors.As(err, &past) {
		t.Errorf("eight records on a budget of seven: %v, want the refusal at the expression's end", err)
	}
	// A lexer's diagnosis outranks the shape error the cut reading would
	// say, as the budget's refusal does.
	t.Setenv(readbudget.Env, "")
	if _, err := Parse("a & b"); err == nil || !strings.Contains(err.Error(), "lone '&'") {
		t.Errorf("`a & b`: %v, want the lexer's cause, lone '&'", err)
	}
	// The refusal outranks the shape error the cut reading would say: on a
	// budget of two, `vars.n` runs out after the dot, where the parser
	// would otherwise expect an identifier.
	t.Setenv(readbudget.Env, "2")
	if _, err := Parse("vars.n"); !errors.As(err, &past) {
		t.Errorf("`vars.n` on a budget of two: %v, want the budget's refusal, not the shape error it caused", err)
	}
	t.Setenv(readbudget.Env, "0")
	if _, err := Parse(sumOf(100_000, 1)); err != nil {
		t.Fatalf("no budget, yet refused: %v", err)
	}
}

// ParseOn spends one budget across readings: two expressions of 2 500
// records each read alone on 4 096; on one budget the second is refused.
func TestParseOnSharesTheBudget(t *testing.T) {
	a, b := sumOf(1_250, 2), sumOf(1_250, 3)
	for _, src := range []string{a, b} {
		if _, err := ParseOn(src, NewBudget(4096)); err != nil {
			t.Fatalf("alone on 4096: %v", err)
		}
	}
	shared := NewBudget(4096)
	if _, err := ParseOn(a, shared); err != nil {
		t.Fatalf("first on the shared budget: %v", err)
	}
	if spent := shared.Spent(); spent != 2500 {
		t.Errorf("spent %d after 2 499 tokens and the end, want 2500", spent)
	}
	var past *BudgetError
	if _, err := ParseOn(b, shared); !errors.As(err, &past) {
		t.Fatalf("second on the shared budget: %v, want a *BudgetError", err)
	}
	if past.Before != 2500 || !strings.Contains(past.Error(), "spent 2500 of the count") {
		t.Errorf("the refusal names %d spent before it and says %q, want the first reading's 2500", past.Before, past.Error())
	}
	// Spend takes a reading already made — a text read again — and refuses
	// when fewer records are left, the count then spent whole.
	again := NewBudget(4096)
	if err := again.Spend(2500); err != nil || again.Spent() != 2500 {
		t.Fatalf("Spend(2500) on 4096: %v, spent %d", err, again.Spent())
	}
	if err := again.Spend(2500); !errors.As(err, &past) || past.Before != 2500 || again.Spent() != 4096 {
		t.Errorf("Spend(2500) with 1596 left: %v, spent %d; want the refusal naming 2500 and the count spent whole", err, again.Spent())
	}
	// A count of none or a negative one takes nothing and gives nothing back.
	fresh := NewBudget(10)
	if err := fresh.Spend(-5); err != nil || fresh.Spent() != 0 {
		t.Errorf("Spend(-5): %v, spent %d; want nothing taken, nothing given back", err, fresh.Spent())
	}
}

// The ticket's shape: millions of operators in one string allocated 480 MiB,
// all retained, at compile time. Held to the budget, the reading stops at
// 2^20 tokens, so what it allocates is bounded whatever the expression's
// length — under 128 MiB here, three times less than the same reading
// allocates with the spend mutated out of advance.
func TestAnExpressionPastTheBudgetAllocatesUnderTheBound(t *testing.T) {
	t.Setenv(readbudget.Env, "")
	src := sumOf(3_000_000, 4)
	var err error
	alloc := allocatedBy(func() { _, err = Parse(src) })
	var past *BudgetError
	if !errors.As(err, &past) {
		t.Fatalf("%d bytes of operands on the default budget: %v, want a *BudgetError", len(src), err)
	}
	t.Logf("%d bytes, %d MiB allocated", len(src), alloc>>20)
	if bound := uint64(128 << 20); alloc > bound {
		t.Errorf("reading %d bytes past the budget allocated %d MiB, want under %d MiB: the budget is not stopping the reading", len(src), alloc>>20, bound>>20)
	}
}

// Excerpt quotes a short text whole and a long one by its first 64 runes
// and an ellipsis, cut on a rune boundary.
func TestExcerpt(t *testing.T) {
	if got := Excerpt("héllo"); got != "héllo" {
		t.Errorf("a short excerpt changed: %q", got)
	}
	if got := Excerpt(strings.Repeat("é", 64)); got != strings.Repeat("é", 64) {
		t.Errorf("a 64-rune text is quoted whole, got %d runes", len([]rune(got)))
	}
	if got := Excerpt(strings.Repeat("é", 70)); got != strings.Repeat("é", 64)+"…" {
		t.Errorf("a long excerpt is %d runes: %q", len([]rune(got)), got)
	}
	if got := Excerpt(""); got != "" {
		t.Errorf("an empty text is quoted empty, got %q", got)
	}
}

// ExcerptAt quotes the 64 runes that start sixteen runes before the offset,
// an ellipsis on each side it does not reach; an offset at the text's start
// or past its end is Excerpt.
func TestExcerptAt(t *testing.T) {
	src := strings.Repeat("a", 100) + "X" + strings.Repeat("b", 100)
	got := ExcerptAt(src, 100)
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, strings.Repeat("a", 16)+"X"+strings.Repeat("b", 47)) || len([]rune(got)) != 66 {
		t.Errorf("ExcerptAt(100) = %q (%d runes)", got, len([]rune(got)))
	}
	if got := ExcerptAt(src, 0); got != Excerpt(src) {
		t.Errorf("ExcerptAt(0) = %q, want Excerpt", got)
	}
	if got := ExcerptAt(src, len(src)); !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, strings.Repeat("b", 16)) || strings.HasSuffix(got, "…") {
		t.Errorf("ExcerptAt(end) = %q, want the last sixteen runes", got)
	}
	if got := ExcerptAt("short", 3); got != "short" {
		t.Errorf("ExcerptAt on a short text = %q", got)
	}
}

// Clip holds a text to a byte bound on a rune boundary, marked when cut;
// the bound every diagnostic is held to is the four kibibytes the docs say.
func TestClip(t *testing.T) {
	if MaxDiagnosticText != 4096 {
		t.Errorf("MaxDiagnosticText = %d, the documentation says 4 KiB", MaxDiagnosticText)
	}
	if got := Clip("héllo", 100); got != "héllo" {
		t.Errorf("under the bound: %q", got)
	}
	long := strings.Repeat("é", 100) // 200 bytes
	got := Clip(long, 101)
	if len(got) > 101+len("…") || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) || len([]rune(got)) != 51 {
		t.Errorf("Clip(200 bytes of é, 101) = %d bytes, %d runes, valid %v", len(got), len([]rune(got)), utf8.ValidString(got))
	}
}
