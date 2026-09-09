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

// The include budget is per FILE: a budget per prompt is multiplied by
// the prompt count, and 3 000 prompts of four includes each — every one
// inside its own budget — made 5 GB of heap out of 310 KB of source at
// cloud publish. Every prompt here stays inside a per-prompt budget; the
// file does not, and is refused once, quickly, on the export and on a
// compile.
func TestPromptIncludes_BudgetIsPerFileNotPerPrompt(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("y", 200*1024) // under the per-file cap of one include
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 40; i++ { // 40 prompts × 1 include × 200 KiB = 8 MiB, each prompt alone within budget
		fmt.Fprintf(&b, "prompt p%d:\n  {{include \"big.md\"}}\n\n", i)
	}
	b.WriteString("workflow w:\n  entry: done\n")
	src := b.String()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	err := InlinePromptIncludes(pr.File)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(maxPromptIncludeTotalBytes)) {
		t.Fatalf("want a refusal naming the byte budget, got %v", err)
	}
	if n := strings.Count(err.Error(), "expand past"); n != 1 {
		t.Fatalf("the budget was refused %d times; once is the message, the rest is noise:\n%s", n, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the refusal took %s", took)
	}

	cr := Compile(parser.Parse(filepath.Join(dir, "main.bot"), src).File)
	var refusals int
	for _, d := range cr.Diagnostics {
		if d.Code == DiagBadPromptInclude && strings.Contains(d.Message, "expand past") {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("compile refused the budget %d times, want exactly once: %v", refusals, cr.Diagnostics)
	}
	var total int
	for _, p := range cr.Workflow.Prompts {
		total += len(p.Body)
	}
	if total > maxPromptIncludeTotalBytes+len(src) {
		t.Fatalf("the compiled prompts hold %d bytes: the budget did not bound the file", total)
	}
}
