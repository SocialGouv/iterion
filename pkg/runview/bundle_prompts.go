package runview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// MergeBundlePrompts injects every `*.md` file from the bundle's
// `prompts/` directory into the AST File's Prompts slice, keyed by
// the file stem (e.g. `prompts/helper.md` → `helper`).
//
// Workflow-declared prompts already in f.Prompts keep precedence —
// bundle files only fill in names the workflow author did not declare
// in source. This lets a bundle ship reusable instructions without
// bloating the .bot while still allowing the workflow to override
// any name locally.
//
// Operating at the AST level (rather than on the compiled IR) means
// downstream validation sees the merged prompts and can resolve
// node-level `system:`/`user:` references against them.
//
// Returns nil when bundle is nil or has no prompts/ directory.
func MergeBundlePrompts(f *ast.File, b *bundle.Bundle) error {
	if f == nil || b == nil || b.PromptsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(b.PromptsDir)
	if err != nil {
		return fmt.Errorf("bundle: read prompts dir %s: %w", b.PromptsDir, err)
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		// Only a .md becomes a prompt: skip everything else BEFORE any I/O,
		// so a stray entry in prompts/ (a broken symlink `notes.txt`, a
		// fifo, a .DS_Store on a mounted share) cannot fail a launch, nor
		// blind the studio's live validation with a 422. MergePromptFiles
		// keeps its own filter as the rule's; this one guards the I/O.
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		// A .md IS a prompt the bundle promised, so one that cannot be read
		// fails the merge by name — and one that is not a regular file is
		// refused BEFORE the read rather than read: os.ReadFile on a fifo
		// blocks until a writer shows up, an unbounded wait inside an HTTP
		// handler. Stat follows a symlink, so a linked prompt file stays one.
		p := filepath.Join(b.PromptsDir, entry.Name())
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("bundle: read prompt %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundle: prompt %s is not a regular file (mode %s)", entry.Name(), info.Mode().Type())
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("bundle: read prompt %s: %w", entry.Name(), err)
		}
		files[bundle.DirPrompts+"/"+entry.Name()] = string(body)
	}
	// The origin is derived from PromptsDir, not Dir: a diagnostic inside a
	// prompt names the file on disk whichever way the bundle was assembled.
	MergePromptFiles(f, files, filepath.Dir(b.PromptsDir))
	return nil
}

// MergePromptFiles is the ONE rule every surface merges bundle prompts by;
// it lives in pkg/bundle (a leaf) so the scaffold and the server reach it
// without importing the run engine. Kept here as the name the run-side
// callers use.
func MergePromptFiles(f *ast.File, files map[string]string, originDir string) {
	bundle.MergePromptFiles(f, files, originDir)
}
