package ir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// writeFanOutGraph lays down levels+1 files where each level includes the
// next fanOut times — the billion-laughs shape. Every file is a few dozen
// bytes and no file repeats along any single path, so neither the per-file
// size cap nor the depth limit nor the cycle stack sees anything wrong; the
// expansion is fanOut^levels leaves.
func writeFanOutGraph(t *testing.T, dir string, levels, fanOut int) {
	t.Helper()
	for lvl := 0; lvl < levels; lvl++ {
		var b strings.Builder
		for i := 0; i < fanOut; i++ {
			fmt.Fprintf(&b, "{{include \"lvl%d.md\"}}\n", lvl+1)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("lvl%d.md", lvl)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("lvl%d.md", levels)), []byte("leaf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fan-out include graph amplifies without bound: depth and the per-file
// cap do not charge the TOTAL, and the path stack only refuses a file that
// repeats along the current path. This runs in the server process on the
// cloud publish path over files a tenant controls, so the operation-wide
// budget is what keeps one tenant from pinning a shared pod.
func TestPromptIncludeBudgetRefusesAFanOutGraph(t *testing.T) {
	dir := t.TempDir()
	// 8 levels of fan-out 8 = 16.7M leaf expansions if nothing charges it.
	writeFanOutGraph(t, dir, 8, 8)
	src := "prompt p:\n  {{include \"lvl0.md\"}}\n"
	path := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(path, src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}

	start := time.Now()
	err := InlinePromptIncludes(pr.File)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a fan-out graph expanded to %d bytes with no refusal", len(pr.File.Prompts[0].Body))
	}
	if !strings.Contains(err.Error(), "expansion budget") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}
	if got := len(pr.File.Prompts[0].Body); got > maxPromptIncludeTotalBytes+len(src) {
		t.Errorf("the body grew to %d bytes, past the %d byte budget", got, maxPromptIncludeTotalBytes)
	}
	if elapsed > 30*time.Second {
		t.Errorf("the refusal took %s — the budget is not cutting the walk short", elapsed)
	}
	// One cause, one error: a body whose markers all fail must not report
	// one refusal per marker (the marker count is attacker-controlled).
	if n := strings.Count(err.Error(), "expansion budget"); n != 1 {
		t.Errorf("the budget was reported %d times, want once: %v", n, err)
	}
}

// The budget is per OPERATION, not per prompt: a document with many prompts
// must not be able to multiply the cap by its prompt count.
func TestPromptIncludeBudgetIsSharedAcrossPrompts(t *testing.T) {
	dir := t.TempDir()
	// A single 200 KiB file, well under the per-file cap, included by each
	// of 40 prompts: 8 MiB total, four times the operation budget.
	big := strings.Repeat("x", 200*1024)
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "prompt p%d:\n  {{include \"big.md\"}}\n\n", i)
	}
	path := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(path, []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(path, src.String())
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	if err := InlinePromptIncludes(pr.File); err == nil {
		t.Fatal("40 prompts each inlining 200 KiB stayed within a 2 MiB budget — the budget is per prompt")
	}
	total := 0
	for _, p := range pr.File.Prompts {
		total += len(p.Body)
	}
	if total > maxPromptIncludeTotalBytes+len(src.String()) {
		t.Errorf("the prompts hold %d bytes, past the %d byte budget", total, maxPromptIncludeTotalBytes)
	}
}

// Compile carries its own budget, seeded once per compile — the local path
// the studio and the CLI take, where the same amplification would burn the
// operator's own machine.
func TestPromptIncludeBudgetAppliesToCompile(t *testing.T) {
	dir := t.TempDir()
	writeFanOutGraph(t, dir, 8, 8)
	src := "prompt p:\n  {{include \"lvl0.md\"}}\n\nagent a:\n  model: \"m\"\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
	path := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(path, src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	cr := Compile(pr.File)
	var named bool
	for _, d := range cr.Diagnostics {
		if strings.Contains(d.Message, "expansion budget") {
			named = true
		}
	}
	if !named {
		t.Fatalf("compile inlined a fan-out graph with no budget diagnostic: %v", cr.Diagnostics)
	}
}

// An honest graph — a handful of files, nested — still resolves whole. The
// budget is a ceiling on abuse, not a new limit on ordinary authoring.
func TestPromptIncludeBudgetLeavesAnHonestGraphAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "top.md"), []byte("TOP\n{{include \"mid.md\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mid.md"), []byte("MID\n{{include \"leaf.md\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leaf.md"), []byte("LEAF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The same leaf reached twice from two prompts: a sibling repeat is not
	// a cycle and must stay legal.
	src := "prompt a:\n  {{include \"top.md\"}}\n\nprompt b:\n  {{include \"mid.md\"}}\n"
	path := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(path, src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	if err := InlinePromptIncludes(pr.File); err != nil {
		t.Fatalf("an honest nested graph was refused: %v", err)
	}
	for _, p := range pr.File.Prompts {
		if !strings.Contains(p.Body, "LEAF") {
			t.Errorf("prompt %q did not resolve to the leaf: %q", p.Name, p.Body)
		}
	}
}
