package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// test-coverage's and e2e-coverage's gates converge only on a pass that
// wrote new test code, and read the working tree for an untracked test
// file. A repository that tracks anything under .claude/skills/ makes git
// list the mirrored skills one by one, and the bundle's own
// `verify-tests.md` matches the test-file regex: the scaffold must never
// satisfy that floor (#1364).
func TestNewTestDetectorIgnoresTheMirror(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	for _, rel := range []string{"test-coverage/main.bot", "e2e-coverage/main.bot"} {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			tpl := toolCommand(t, rel, "verify_run")
			ws, scratch := t.TempDir(), t.TempDir()
			gittest.Run(t, ws, "init", "-q")
			gittest.Run(t, ws, "config", "user.email", "t@example.invalid")
			gittest.Run(t, ws, "config", "user.name", "t")
			if err := os.MkdirAll(filepath.Join(ws, ".claude", "skills"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A tracked skill of the repository's own, so the mirror beside it
			// is listed file by file rather than as one collapsed directory.
			for rel, body := range map[string]string{
				"README.md":                     "baseline\n",
				".claude/skills/house-style.md": "# theirs\n",
			} {
				if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gittest.Run(t, ws, "add", "-f", "README.md", ".claude/skills/house-style.md")
			gittest.Run(t, ws, "commit", "-q", "-m", "baseline")
			if err := os.WriteFile(filepath.Join(ws, ".claude", "skills", "verify-tests.md"), []byte("# mirrored\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte("#!/bin/sh\nset -e\necho ok\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			expand := func() string {
				s := strings.ReplaceAll(tpl, "{{vars.workspace_dir}}", ws)
				s = strings.ReplaceAll(s, "{{vars.scratch_dir}}", scratch)
				s = strings.ReplaceAll(s, "{{vars.verify_timeout_s}}", "600")
				s = strings.ReplaceAll(s, "{{vars.matrix_path}}", "")
				return strings.ReplaceAll(s, "{{vars.target}}", "")
			}
			var res struct {
				Passed      bool `json:"passed"`
				NewTestCode bool `json:"new_test_code"`
			}
			runScaffoldJSON(t, expand(), &res)
			if !res.Passed || res.NewTestCode {
				t.Fatalf("a mirrored skill listed file by file must not count as new test code, got %+v", res)
			}
			// A test the pass wrote and committed in stride (the precheck refuses
			// an uncommitted one before this floor is read) still counts.
			if err := os.WriteFile(filepath.Join(ws, "test_greet.py"), []byte("def test_x(): pass\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, ws, "add", "test_greet.py")
			gittest.Run(t, ws, "commit", "-q", "-m", "test: greet")
			runScaffoldJSON(t, expand(), &res)
			if !res.Passed || !res.NewTestCode {
				t.Fatalf("a committed test file must still count as new test code, got %+v", res)
			}
		})
	}
}
