package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// iterion lays the bundle's skills and plugin files under
// <workspace>/.claude/ at run start. A deterministic commit that stages
// the whole tree on a repository that does not ignore `.claude/` commits
// that scaffold as if the run had written it — measured on bmady: 16 of
// the 43 files of one feature commit were mirrored skills (#1364). The
// commit stages everything the run produced and nothing iterion laid there.
func TestBmadyCommitLeavesTheScaffoldOut(t *testing.T) {
	for _, bin := range []string{"git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	cmd := toolCommand(t, "bmady/main.bot", "commit_changes")

	ws := t.TempDir()
	gittest.Run(t, ws, "init", "-q")
	gittest.Run(t, ws, "config", "user.email", "t@example.invalid")
	gittest.Run(t, ws, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "add", "README.md")
	gittest.Run(t, ws, "commit", "-q", "-m", "baseline")
	// What the run produced: a new file and an edit.
	if err := os.WriteFile(filepath.Join(ws, "greet.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("baseline\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScaffold(t, ws)

	expanded := strings.ReplaceAll(cmd, "{{input.workspace_dir}}", ws)
	expanded = strings.ReplaceAll(expanded, "{{input.message}}", "'feat: greet'")
	if out, err := exec.Command("sh", "-c", expanded).CombinedOutput(); err != nil {
		t.Fatalf("commit_changes failed: %v\n%s", err, out)
	}
	shown := gittest.Run(t, ws, "show", "--name-only", "--format=", "HEAD")
	for _, want := range []string{"greet.py", "README.md"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the commit must carry the run's %s, got:\n%s", want, shown)
		}
	}
	if strings.Contains(shown, ".claude") {
		t.Errorf("the commit swept iterion's scaffold in:\n%s", shown)
	}
}

// Every site the catalogue ships that stages the whole tree — a
// deterministic commit command, an instruction an agent copies, a skill the
// prompt names as the authority, the DSL cheatsheet's example, the doctrine
// page for bot authors — carries the exclusion, so the next `git add -A`
// written from the pattern cannot lose it silently. The class is DISCOVERED
// by walking every .bot and .md under bots/ plus the authoring doctrine: a
// new bot, skill or example that stages everything without the exclusion
// fails here without anyone having to enumerate it.
func TestWholeTreeStagingExcludesTheScaffold(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".bot") || strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "../docs/agents/bot-authoring.md")
	// A site that FORBIDS the practice is not a site of it.
	exempt := map[string]string{
		"product-docs/main.bot":                     "never `git add -A`",
		"whats-next/skills/iterion-dsl-quickref.md": "never `git add -N .`",
	}
	// `git add -A`, `git -C <dir> add -A`, `git add -N` (intent-to-add: a
	// marked scaffold keeps `git diff --exit-code` red), `git clean -fd`
	// (which would delete the mirror outright), in shell or as a subprocess
	// list: any whole-tree staging, marking or cleaning, wherever the
	// repository is named; the pathspec anchors the tree at the repository
	// root so a copy run from a subdirectory covers everything.
	staging := regexp.MustCompile(`\badd -[AN]\b|\bclean -fd\b|'clean', '-fd'`)
	excluded := regexp.MustCompile(`(?:add -[AN]|clean -fd) -- ':/' ':\(exclude,top\)\.claude'|'clean', '-fd', '--', ':/', ':\(exclude,top\)\.claude'`)
	sites := 0
	for _, rel := range files {
		src, err := os.ReadFile(rel)
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
