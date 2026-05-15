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
// bloating the .iter while still allowing the workflow to override
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
	declared := make(map[string]struct{}, len(f.Prompts))
	for _, p := range f.Prompts {
		declared[p.Name] = struct{}{}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if _, exists := declared[stem]; exists {
			// Workflow-declared prompt wins on name collision.
			continue
		}
		body, err := os.ReadFile(filepath.Join(b.PromptsDir, name))
		if err != nil {
			return fmt.Errorf("bundle: read prompt %s: %w", name, err)
		}
		f.Prompts = append(f.Prompts, &ast.PromptDecl{
			Name: stem,
			Body: string(body),
		})
		declared[stem] = struct{}{}
	}
	return nil
}
