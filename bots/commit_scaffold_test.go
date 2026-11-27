package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/treenoise"
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
	// The engine provisions ITERION_TREE_NOISE on every tool process it
	// spawns (host and sandbox); the bare `sh -c` here stands in for that
	// spawn, so it carries the same environment.
	cmd2 := exec.Command("sh", "-c", expanded)
	cmd2.Env = append(os.Environ(), treenoise.TreeNoiseEnvVar+"="+treenoise.EnvValue())
	if out, err := cmd2.CombinedOutput(); err != nil {
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
