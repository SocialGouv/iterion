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
var gitExec = regexp.MustCompile(`(?s)exec\.Command(?:Context)?\(\s*(?:[\w.]+(?:\([^()]*\))?\s*,\s*)?"git"`)

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
// git.SanitizeEnv appears within a few lines of the command being built.
// Accepting any `.Env` assignment would accept `cmd.Env = os.Environ()`, which
// is the very thing being kept out.
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
			// SanitizeEnv specifically, not any .Env assignment: writing
			// `cmd.Env = os.Environ()` (or appending to it) is the most likely
			// way a new call site gets written, and it is exactly the
			// unscrubbed environment this guard exists to keep out.
			// Bounded by the NEXT call site, not by a fixed span: in
			// pkg/dispatcher the four sites sit 107-406 characters apart, so a
			// fixed window let each one's assignment vouch for its neighbour —
			// deleting one and keeping the others still passed. Within that
			// bound the span is generous, since cancellation hardening,
			// timeouts and their comments routinely sit in between (the
			// runner's clone puts 706 characters of them there).
			end := min(loc[0]+1600, len(body))
			if next := gitExec.FindStringIndex(body[loc[1]:]); next != nil {
				end = min(end, loc[1]+next[0])
			}
			tail := body[loc[0]:end]
			if strings.Contains(tail, "SanitizeEnv(") {
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

// runHooksExempt names the files whose git subprocesses may run the
// repository's hooks, and why. Every other git subprocess built anywhere in
// the tree runs under NoRunHooks.
var runHooksExempt = map[string]string{
	// The studio's authoring commits FOR the user, in the user's own
	// checkout: the user's hooks are the user's, and run.
	"pkg/server/assistant_authoring.go":     "the user's own commits",
	"pkg/server/assistant_authoring_git.go": "the user's own commits",
	"pkg/server/assistant_dependencies.go":  "the user's own commits",
	// The test fixture's git: it plants the hooks the suites prove iterion
	// does not run.
	"internal/gittest/gittest.go": "the test fixture",
}

// TestEveryGitCallerRunsNoRepositoryHook sweeps the tree for git subprocesses
// that run the repository's hooks. A run writes its repository's hooks
// directory and config — a worktree shares them with the operator's checkout —
// so a hook there is the run's code, executed inside whichever iterion gesture
// git runs it in: the landing of a run on the operator's branch rewritten from
// inside its own commit, the bank's push refused, a fork's checkout altered.
// A rule applied site by site leaves the next site open, so the property is
// checked mechanically: a call site is satisfied when NoRunHooks builds its
// argv, inside the call itself.
func TestEveryGitCallerRunsNoRepositoryHook(t *testing.T) {
	root := filepath.Join("..", "..")
	skipNames := map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}
	var offenders []string
	sites := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if _, ok := runHooksExempt[rel]; ok {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		body := string(src)
		for _, loc := range gitExec.FindAllStringIndex(body, -1) {
			lineStart := strings.LastIndex(body[:loc[0]], "\n") + 1
			if strings.Contains(body[lineStart:loc[0]], "//") {
				continue
			}
			sites++
			open := loc[0] + strings.Index(body[loc[0]:], "(")
			if strings.Contains(body[loc[0]:callEnd(body, open)], "NoRunHooks(") {
				continue
			}
			line := 1 + strings.Count(body[:loc[0]], "\n")
			offenders = append(offenders, rel+":"+itoa(line)+"  "+strings.TrimSpace(body[loc[0]:min(loc[1]+40, len(body))]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if sites == 0 {
		t.Fatal("the sweep found no git subprocess at all: it is looking in the wrong place")
	}
	if len(offenders) > 0 {
		t.Errorf("git subprocess(es) that run the repository's hooks — a run writes its repository's hooks directory and config, so each of these executes the run's code inside an iterion gesture.\nBuild the argv with git.NoRunHooks(...), or name the file in runHooksExempt with the reason its hooks are the user's:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// callEnd returns the index just past the `)` that closes the call whose `(`
// is at open, string and rune literals skipped.
func callEnd(body string, open int) int {
	depth := 0
	for i := open; i < len(body); i++ {
		switch body[i] {
		case '"', '\'':
			q := body[i]
			for i++; i < len(body) && body[i] != q; i++ {
				if body[i] == '\\' {
					i++
				}
			}
		case '`':
			for i++; i < len(body) && body[i] != '`'; i++ {
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(body)
}
