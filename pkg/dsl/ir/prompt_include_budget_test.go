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

// fanOutTree writes depth levels of files under dir, each including the
// next one fanOut times; the leaf holds one byte. Nine levels of four are
// 4^8 = 65 536 leaf expansions — the billion-laughs shape.
func fanOutTree(t *testing.T, dir string, fanOut, depth int) {
	t.Helper()
	for level := 0; level < depth; level++ {
		var b strings.Builder
		if level == depth-1 {
			b.WriteString("x")
		} else {
			for i := 0; i < fanOut; i++ {
				fmt.Fprintf(&b, "{{include \"l%d.md\"}}\n", level+1)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("l%d.md", level)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A nested include tree that repeats files grows as fan-out^depth; the
// per-file cap, the depth cap and the cycle check bound none of that.
// The expansion is budgeted in files read and in bytes produced, and
// refused — quickly — past the budget, on the export and on a compile.
func TestPromptIncludes_RefuseAnExponentialTree(t *testing.T) {
	dir := t.TempDir()
	fanOutTree(t, dir, 4, 9)
	src := "prompt p:\n  {{include \"l0.md\"}}\n\nworkflow w:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	err := InlinePromptIncludes(pr.File)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(maxPromptIncludeExpansions)) {
		t.Fatalf("want a refusal naming the expansion budget, got %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the refusal took %s: the tree was expanded before it was refused", took)
	}

	start = time.Now()
	cr := Compile(parser.Parse(filepath.Join(dir, "main.bot"), src).File)
	var refused bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagBadPromptInclude && strings.Contains(d.Message, fmt.Sprint(maxPromptIncludeExpansions)) {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("compile did not refuse the tree: %v", cr.Diagnostics)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the compile took %s: the tree was expanded before it was refused", took)
	}
}

// The byte budget catches a tree of big files before the file budget does.
func TestPromptIncludes_RefuseAnExpansionPastTheByteBudget(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("y", 200*1024) // under the per-file cap, six of them are over the total
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "prompt p:\n  " + strings.Repeat("{{include \"big.md\"}} ", 6) + "\n\nworkflow w:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	err := InlinePromptIncludes(pr.File)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(maxPromptIncludeTotalBytes)) {
		t.Fatalf("want a refusal naming the byte budget, got %v", err)
	}
}

// A file included twice beside itself — a shared preamble — is not a cycle
// and stays allowed.
func TestPromptIncludes_AllowASiblingRepeat(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"pre.md":   "PREAMBLE",
		"main.bot": "prompt p:\n  {{include \"pre.md\"}}\n  middle\n  {{include \"pre.md\"}}\n\nworkflow w:\n  entry: done\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
	pr := parser.Parse(filepath.Join(dir, "main.bot"), string(src))
	if err := InlinePromptIncludes(pr.File); err != nil {
		t.Fatalf("a sibling repeat was refused: %v", err)
	}
	if strings.Count(pr.File.Prompts[0].Body, "PREAMBLE") != 2 {
		t.Fatalf("both markers should expand: %q", pr.File.Prompts[0].Body)
	}
}
