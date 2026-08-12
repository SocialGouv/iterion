package git

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitExec matches a git subprocess being built anywhere in the tree. It spans
// newlines: gofmt wraps a long call, and a line-oriented pattern would miss
// `exec.CommandContext(ctx,\n\t"git", …)` — precisely the shape a new call site
// is likely to take.
var gitExec = regexp.MustCompile(`(?s)exec\.Command(?:Context)?\(\s*(?:[\w.]+\s*,\s*)?"git"`)

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
	// The whole repository, not just pkg/: a git subprocess is as likely to be
	// added under cmd/ or e2e/, and a sweep that silently never looks there
	// reads as coverage it does not have.
	root := filepath.Join("..", "..")
	skipNames := map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Hidden directories hold scratch clones of other people's code
			// (.local, .works, .repos) — sweeping them reports on third-party
			// source and costs half a minute. pkg/git assigns through
			// gitEnv(), SanitizeEnv's own caller.
			name := info.Name()
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) ||
				path == filepath.Join("..", "..", "pkg", "git") {
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
		body := string(src)
		for _, loc := range gitExec.FindAllStringIndex(body, -1) {
			// A call quoted inside a doc comment is prose, not a call site.
			lineStart := strings.LastIndex(body[:loc[0]], "\n") + 1
			if strings.Contains(body[lineStart:loc[0]], "//") {
				continue
			}
			// The assignment may land well below the call: Dir, cancellation
			// hardening and timeouts are commonly wired in between.
			tail := body[loc[0]:min(loc[0]+700, len(body))]
			if strings.Contains(tail, ".Env = ") {
				continue
			}
			line := 1 + strings.Count(body[:loc[0]], "\n")
			offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(line)+"  "+strings.TrimSpace(body[loc[0]:min(loc[1]+40, len(body))]))
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
