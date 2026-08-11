package git

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitExec matches a git subprocess being built anywhere in the tree.
var gitExec = regexp.MustCompile(`exec\.Command(?:Context)?\([^)]*?"git"`)

// TestEveryGitCallerSanitizesEnv sweeps the tree for git subprocesses whose
// environment is left inherited.
//
// Reviewing this by hand does not work: applying the scrub across the packages
// that shell out to git, I set it on two of the four call sites in
// pkg/dispatcher and missed the other two — one of them a `worktree remove
// --force`, where an inherited GIT_COMMON_DIR deregisters a worktree in
// somebody else's repository. `--git-dir` looks like it covers that and does
// not: git takes non-worktree files, the worktree registry among them, from
// GIT_COMMON_DIR.
//
// So the property is checked mechanically. A call site is satisfied when
// `.Env` is assigned within a few lines of the command being built; the point
// is to force the decision, not to prove the value is right.
func TestEveryGitCallerSanitizesEnv(t *testing.T) {
	root := filepath.Join("..", "..", "pkg")
	// pkg/git assigns through gitEnv(), which is SanitizeEnv's own caller.
	skipDirs := map[string]bool{filepath.Join("..", "..", "pkg", "git"): true}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDirs[path] || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !gitExec.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			// The assignment may land well below the command: Dir, cancellation
			// hardening and timeouts are commonly wired in between.
			window := lines[i:min(i+16, len(lines))]
			if strings.Contains(strings.Join(window, "\n"), ".Env = ") {
				continue
			}
			offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("git subprocess(es) built with the caller's environment inherited whole — GIT_DIR, GIT_COMMON_DIR, GIT_INDEX_FILE and friends override the repository each of these names for itself.\nSet cmd.Env = git.SanitizeEnv(os.Environ()) (plus whatever else the call needs):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
