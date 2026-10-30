package ir

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// groupSubstSrc builds a bot whose group carries fieldsTool tool nodes, each
// command holding markersPerField `{{params.v}}` markers, bound to a value of
// valueBytes. It returns the source and the line of its `use` line.
func groupSubstSrc(fieldsTool, markersPerField, valueBytes int) (string, int) {
	var b strings.Builder
	b.WriteString("schema s:\n  ok: bool\n\ngroup g(v):\n")
	for i := 0; i < fieldsTool; i++ {
		fmt.Fprintf(&b, "  tool t%d:\n    command: \"%s\"\n    output: s\n", i, strings.Repeat("{{params.v}}", markersPerField))
	}
	b.WriteString("\n")
	useLine := strings.Count(b.String(), "\n") + 1
	fmt.Fprintf(&b, "use g as r1 with { v: \"%s\" }\n\n", strings.Repeat("a", valueBytes))
	b.WriteString("workflow w:\n  entry: r1.t0\n  r1.t0 -> done\n")
	return b.String(), useLine
}

func groupSubstDiag(t *testing.T, src string) Diagnostic {
	t.Helper()
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Code == DiagGroupExpansionPastBudget {
			return d
		}
	}
	t.Fatalf("no C327 in %v", r.Diagnostics)
	return Diagnostic{}
}

// The lexer counts a `{{params.v}}` marker and its bound value as one record
// each, so 10 000 markers × a 100 KB value built a 1 GB string at compile
// time before any budget saw it — and at 50 000 × 200 KB the compiler died
// out of memory inside substParams (#2774). One field may not expand past
// the 10 MiB size cap: the refusal names the `use` line and writes at most
// the cap.
func TestGroupSubstitutionFieldExpansionIsCapped(t *testing.T) {
	src, useLine := groupSubstSrc(1, 10_000, 100_000)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	d := groupSubstDiag(t, src)
	el := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc

	if d.Line != useLine {
		t.Errorf("C327 lands on line %d, want the use line %d", d.Line, useLine)
	}
	if !strings.Contains(d.Message, "10") || !strings.Contains(d.Message, "field") {
		t.Errorf("C327 does not name the field cap: %q", d.Message)
	}
	t.Logf("refused after %s, %d MiB allocated", el.Round(time.Millisecond), alloc>>20)
	if limit := uint64(256 << 20); alloc > limit {
		t.Errorf("the refused substitution allocated %d MiB, want under %d MiB: the cap must speak before the string is built", alloc>>20, limit>>20)
	}
	if limit := 10 * time.Second; el > limit {
		t.Errorf("refusal took %s, want under %s", el, limit)
	}
}

// Spread over fields under the per-field cap, the same multiplication is
// spent against the read budget the bot's files already share — one record
// per substituted marker, one per 64 bytes of value — and refused on the
// `use` line.
func TestGroupSubstitutionSpendsTheReadBudget(t *testing.T) {
	// 11 fields × 1 000 markers × 6 500 bytes: every field expands to
	// 6.5 MB (under the per-field cap), the spend is 11 000 × 102 records —
	// past the 2^20 the bot's reading shares.
	src, useLine := groupSubstSrc(11, 1_000, 6_500)
	d := groupSubstDiag(t, src)
	if d.Line != useLine {
		t.Errorf("C327 lands on line %d, want the use line %d", d.Line, useLine)
	}
	if !strings.Contains(d.Message, "read budget") {
		t.Errorf("C327 does not name the read budget: %q", d.Message)
	}
	if d.Hint == "" {
		t.Error("C327 carries no remedy")
	}
}

// The reviewer's probe of the first cut: the budget was PER USE, so 40 uses
// individually under each bound multiplied — 1 200 KiB of source allocated
// 11.5 GiB and compiled clean. The uses of a bot now share ONE substitution
// budget: every field every instance writes spends it, so the 40 uses sum
// past the 2^20 count and the use that crosses is refused (C327).
func TestGroupSubstitutionBudgetIsSharedAcrossUses(t *testing.T) {
	// Each use writes 2 fields × 1 000 markers × 1 000 bytes = 2 MB, under
	// the per-field cap and under a per-use reading of the budget; 40 of
	// them write 80 MB, past the shared 2^20 records (~64 MiB).
	var b strings.Builder
	b.WriteString("schema s:\n  ok: bool\n\ngroup g(v):\n")
	for _, name := range []string{"t0", "t1"} {
		fmt.Fprintf(&b, "  tool %s:\n    command: \"%s\"\n    output: s\n", name, strings.Repeat("{{params.v}}", 1_000))
	}
	b.WriteString("\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "use g as r%d with { v: \"%s\" }\n", i, strings.Repeat("a", 1_000))
	}
	b.WriteString("\nworkflow w:\n  entry: r0.t0\n  r0.t0 -> done\n")
	src := b.String()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	r := compileFile(t, src)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc

	var c325 int
	for _, d := range r.Diagnostics {
		if d.Code == DiagGroupExpansionPastBudget {
			c325++
		}
	}
	if c325 == 0 {
		t.Fatalf("40 uses each under the per-use bounds compiled clean: the budget is per use, not the bot's (%v)", r.Diagnostics)
	}
	t.Logf("refused with %d C327, %d MiB allocated", c325, alloc>>20)
	if limit := uint64(512 << 20); alloc > limit {
		t.Errorf("the refused compile allocated %d MiB, want under %d MiB", alloc>>20, limit>>20)
	}
}

// The fourth measured site of #2774: N `use` lines WITHOUT parameters clone
// every string of the group, so 569 KB of source became 1.3 GB of IR,
// accepted. The same shared budget holds it: a param-less group's fields
// spend it as written.
func TestGroupCloningWithoutParamsSpendsTheSharedBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("schema s:\n  ok: bool\n\ngroup g():\n")
	fmt.Fprintf(&b, "  tool t:\n    command: \"%s\"\n    output: s\n\n", strings.Repeat("x", 40_000))
	for i := 0; i < 3_000; i++ {
		fmt.Fprintf(&b, "use g as r%d\n", i)
	}
	b.WriteString("\nworkflow w:\n  entry: r0.t\n  r0.t -> done\n")
	r := compileFile(t, b.String())
	var c325 int
	for _, d := range r.Diagnostics {
		if d.Code == DiagGroupExpansionPastBudget {
			c325++
		}
	}
	if c325 == 0 {
		t.Fatalf("3 000 param-less uses cloning 40 KB each (120 MB of IR) compiled clean: %v", r.Diagnostics)
	}
	t.Logf("refused with %d C327", c325)
}
