package git

import (
	"path/filepath"
	"strings"
	"testing"
)

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
	// pkg/git assigns through gitEnv(), SanitizeEnv's own caller.
	ownDir := filepath.Join(root, "pkg", "git") + string(filepath.Separator)
	files := gitSitesInTree(t, root)

	var offenders []string
	resolved := 0
	for _, path := range sortedGitFiles(files) {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, ownDir) {
			continue
		}
		f := files[path]
		for i, site := range f.sites {
			if site.resolved {
				resolved++
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
			end := min(site.start+1600, len(f.body))
			if i+1 < len(f.sites) {
				end = min(end, f.sites[i+1].start)
			}
			if strings.Contains(f.body[site.start:end], "SanitizeEnv(") {
				continue
			}
			offenders = append(offenders, siteLabel(filepath.ToSlash(path), f.body, site))
		}
	}
	requireResolvedSites(t, resolved, "pkg/claw's runGit and /diff and internal/clawfold")
	if len(offenders) > 0 {
		t.Errorf("git subprocess(es) built with the caller's environment inherited whole — GIT_DIR, GIT_COMMON_DIR, GIT_INDEX_FILE and friends override the repository each of these names for itself.\nSet cmd.Env = git.SanitizeEnv(os.Environ()) (plus whatever else the call needs):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// requireResolvedSites fails a sweep that saw no git spawned by a value among
// the files it judges. where names the sites the tree holds today: a zero
// means the recognizer went blind to their shape, or, if every one was
// really rewritten to the literal, that this sweep's check has outlived its
// use.
func requireResolvedSites(t *testing.T, resolved int, where string) {
	t.Helper()
	if resolved == 0 {
		t.Fatalf("the sweep found no git spawned by a value, yet %s spawn one: the recognizer is blind to it — TestGitSpawnSitesRecognisesEveryResolvedSpelling says which shape. If none is left among the files this sweep judges, drop this sweep's check", where)
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

// siteLabel renders a site for a guard's report: file, line, and the call up
// to the expression naming git.
func siteLabel(file, body string, site gitSite) string {
	line := 1 + strings.Count(body[:site.start], "\n")
	return file + ":" + itoa(line) + "  " + strings.TrimSpace(body[site.start:min(site.end+40, len(body))])
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
	files := gitSitesInTree(t, root)
	var offenders []string
	sites, resolved := 0, 0
	for _, path := range sortedGitFiles(files) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if _, ok := runHooksExempt[rel]; ok {
			continue
		}
		f := files[path]
		for _, site := range f.sites {
			sites++
			if site.resolved {
				resolved++
			}
			open := site.start + strings.Index(f.body[site.start:], "(")
			if strings.Contains(f.body[site.start:callEnd(f.body, open)], "NoRunHooks(") {
				continue
			}
			offenders = append(offenders, siteLabel(rel, f.body, site))
		}
	}
	if sites == 0 {
		t.Fatal("the sweep found no git subprocess at all: it is looking in the wrong place")
	}
	requireResolvedSites(t, resolved, "pkg/claw's runGit and /diff and internal/clawfold")
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
