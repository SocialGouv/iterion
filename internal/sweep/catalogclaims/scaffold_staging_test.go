package catalogclaims

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// Every site the catalogue ships that stages the whole tree — a
// deterministic commit command, an instruction an agent copies, a skill the
// prompt names as the authority, the DSL cheatsheet's example, the doctrine
// page for bot authors — carries the exclusion, so the next `git add -A`
// written from the pattern cannot lose it silently. The class is DISCOVERED
// by walking every .bot and .md under bots/ plus the authoring doctrine: a
// new bot, skill or example that stages everything without the exclusion
// fails here without anyone having to enumerate it.
func TestWholeTreeStagingExcludesTheScaffold(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	// Every file is named relative to the module root, as the exemptions
	// below are keyed.
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "bots"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".bot") || strings.HasSuffix(path, ".md") {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "docs/agents/bots/bot-authoring.md", "docs/agents/bots/convergence.md", "docs/workflow_authoring_pitfalls.md")
	// A site that FORBIDS the practice is not a site of it.
	exempt := map[string]string{
		"bots/product-docs/main.bot":                     "never `git add -A`",
		"bots/whats-next/skills/iterion-dsl-quickref.md": "never `git add -N .`",
	}
	// `git add -A`, `git -C <dir> add -A`, `git add -N` (intent-to-add: a
	// marked scaffold keeps `git diff --exit-code` red), `git clean -fd`
	// (which would delete the mirror outright), in shell or as a subprocess
	// list: any whole-tree staging, marking or cleaning, wherever the
	// repository is named; the pathspec anchors the tree at the repository
	// root so a copy run from a subdirectory covers everything.
	// A clean stays scoped to the workspace as a bare `git clean` is (`-- .`,
	// the mirror excluded relative to it); staging and marking are repo-wide
	// as a bare `git add -A` is (`':/'`, the root mirror excluded).
	// A whole-tree checkout (`checkout -- .`, the revert of every tracked
	// file) spares the engine's tree the same way: the settings hooks it
	// injects and the operator's own tracked files under .claude/ survive.
	staging := regexp.MustCompile(`\badd -(?:[AN]|--all)\b|\bclean -fd\b|'clean', '-fd'|"checkout", "--", "\."|'checkout', '--', '\.'|\bcheckout -- \.(\s|$)`)
	excluded := regexp.MustCompile(`add -(?:[AN]|--all) -- ':/' ':\(exclude,top\)\.claude'|clean -fd -- \. ':\(exclude\)\.claude'|'clean', '-fd', '--', '\.', ':\(exclude\)\.claude'|"checkout", "--", "\.", ":\(exclude\)\.claude"|'checkout', '--', '\.', ':\(exclude\)\.claude'|checkout -- \. ':\(exclude\)\.claude'|add -(?:[AN]|--all) -- ':/' \{\{run\.tree_noise\}\}|add -(?:[AN]|--all) -- ':/' \$ITERION_TREE_NOISE`)
	sites := 0
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for n, line := range strings.Split(string(src), "\n") {
			if !staging.MatchString(line) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue // prose about the practice inside a script or a comment
			}
			if marker, ok := exempt[filepath.ToSlash(rel)]; ok && strings.Contains(line, marker) {
				continue
			}
			sites++
			if !excluded.MatchString(line) {
				t.Errorf("%s:%d stages the whole tree without excluding the scaffold: %s", rel, n+1, trimmed)
			}
		}
	}
	// The class this test was written for: nine deterministic or copied
	// commands and instructions in the bots, five skills, two cheatsheet
	// examples, two doctrine sentences. Fewer means the walk lost a file.
	if sites < 20 {
		t.Errorf("only %d whole-tree staging sites found — the walk no longer covers the class", sites)
	}
}
