// Package scripts holds the tests for the shell scripts in this directory.
// It carries no non-test Go source on purpose — the artefacts under test are
// the scripts themselves.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// brew-tap-pr.sh closes superseded tap PRs with `gh pr close --delete-branch`.
// That is a destructive action driven by a predicate with no test coverage of
// its own, and the release pipeline is the only place it ever runs — so a
// wrong predicate is discovered by a release that silently never reaches
// Homebrew. This file is the author's fake-`gh` dry run made permanent: a
// throwaway git repo, a `gh` stub on PATH, and the open-PR set as the oracle.
//
// The stub reproduces the two behaviours the sweep depends on: `gh pr list`
// answers NEWEST-FIRST and truncates at `--limit` (30 when unset), and `--jq`
// is handed to the real jq. Drop either and the >30-PRs row stops proving
// anything.
const fakeGH = `#!/usr/bin/env bash
set -eu
# State: $LAB/prs holds the open PRs NEWEST-FIRST, one "number<TAB>headRefName"
# per line. Actions are appended to $LAB/closed, $LAB/created, $LAB/merged.
sub="$1 $2"; shift 2
case "$sub" in
"pr list")
  head=""; limit=30; jqexpr='.'
  while [ $# -gt 0 ]; do
    case "$1" in
      --head)  head="$2";   shift 2;;
      --limit) limit="$2";  shift 2;;
      --jq)    jqexpr="$2"; shift 2;;
      --json|--state) shift 2;;
      *) shift;;
    esac
  done
  awk -v h="$head" -F'\t' 'h=="" || $2==h' "$LAB/prs" |
    head -n "$limit" |
    awk -F'\t' 'BEGIN{printf "["; s=""} {printf "%s{\"number\":%s,\"headRefName\":\"%s\"}", s, $1, $2; s=","} END{print "]"}' |
    jq -r "$jqexpr"
  ;;
"pr create")
  ref=""
  while [ $# -gt 0 ]; do case "$1" in --head) ref="$2"; shift 2;; *) shift;; esac; done
  printf '%s\t%s\n' 900 "$ref" | cat - "$LAB/prs" > "$LAB/prs.new"
  mv "$LAB/prs.new" "$LAB/prs"
  echo "$ref" >> "$LAB/created"
  echo "created PR #900 for $ref"
  ;;
"pr merge")
  echo "$1" >> "$LAB/merged"
  ;;
"pr close")
  echo "$1" >> "$LAB/closed"
  awk -F'\t' -v n="$1" '$1!=n' "$LAB/prs" > "$LAB/prs.new"
  mv "$LAB/prs.new" "$LAB/prs"
  ;;
*) echo "fake gh: unhandled: $sub $*" >&2; exit 3;;
esac
`

// TestBrewTapSupersedeSweep drives the publish path against the stub and
// asserts on the open-PR set the sweep leaves behind.
//
// Every row here fails on the pre-fix script (which selected on
// `headRefName != "$branch"` alone, over an untruncated-by-luck 30-entry
// page, and returned before the sweep when nothing was pushed), so the file
// is not vacuous.
func TestBrewTapSupersedeSweep(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "git", "jq", "awk", "sort"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	script, err := filepath.Abs("brew-tap-pr.sh")
	if err != nil {
		t.Fatal(err)
	}

	// noise is 40 unrelated open PRs, newer than the tap slot, enough to push
	// it off `gh pr list`'s default 30-entry page.
	noise := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		noise = append(noise, "unrelated/pr")
	}

	for _, tc := range []struct {
		name string
		tag  string
		// open tap/other branches, OLDEST first (the newest is listed last).
		open []string
		// rewrite is false for the no-op release: the tap is already current,
		// so nothing is staged and no branch is pushed.
		rewrite   bool
		wantClose []string
		wantOpen  []string
	}{{
		name:      "a newer release supersedes the older slot",
		tag:       "v3.112.25",
		open:      []string{"chore/brew-tap-v3.112.23"},
		rewrite:   true,
		wantClose: []string{"chore/brew-tap-v3.112.23"},
		wantOpen:  []string{"chore/brew-tap-v3.112.25"},
	}, {
		// The destructive one: a backfill run reaches the sweep with an OLD
		// tag as $branch, and closing "every branch but mine" deletes the
		// current release's slot.
		name:      "a backfill of an old tag leaves the newer slot alone",
		tag:       "v0.5.0",
		open:      []string{"chore/brew-tap-v3.112.25"},
		rewrite:   true,
		wantClose: nil,
		wantOpen:  []string{"chore/brew-tap-v3.112.25", "chore/brew-tap-v0.5.0"},
	}, {
		// gh sorts newest-first, so the entries that fall off an unset
		// --limit are precisely the ones the sweep exists to close.
		name:      "the older slot is found behind 40 newer PRs",
		tag:       "v3.112.25",
		open:      append([]string{"chore/brew-tap-v3.112.9"}, noise...),
		rewrite:   true,
		wantClose: []string{"chore/brew-tap-v3.112.9"},
	}, {
		name:      "a release with nothing to rewrite still prunes",
		tag:       "v3.112.25",
		open:      []string{"chore/brew-tap-v3.112.9"},
		rewrite:   false,
		wantClose: []string{"chore/brew-tap-v3.112.9"},
	}, {
		name:      "a tap branch this script cannot order is left alone",
		tag:       "v3.112.25",
		open:      []string{"chore/brew-tap-nightly"},
		rewrite:   true,
		wantClose: nil,
		wantOpen:  []string{"chore/brew-tap-nightly", "chore/brew-tap-v3.112.25"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lab := newTapLab(t, script, tc.open)

			lab.run(t, "branch", tc.tag)
			if tc.rewrite {
				lab.write(t, filepath.Join("Formula", "iterion.rb"), tc.tag)
			}
			out := lab.run(t, "publish", tc.tag)

			closed := lab.refsFor(t, "closed")
			if !sameSet(closed, tc.wantClose) {
				t.Errorf("closed = %v, want %v\n--- script output ---\n%s", closed, tc.wantClose, out)
			}
			if tc.wantOpen != nil {
				if open := lab.openRefs(t); !sameSet(open, tc.wantOpen) {
					t.Errorf("open = %v, want %v\n--- script output ---\n%s", open, tc.wantOpen, out)
				}
			}
			// `main` is the whole reason this script exists: a direct push to
			// it rebuilds the merge group at the head of the queue.
			if got, want := lab.rev(t, "refs/heads/main"), lab.baseSHA; got != want {
				t.Errorf("origin/main moved to %s (was %s) — the tap must never push to main", got, want)
			}
		})
	}
}

// tapLab is one throwaway checkout + bare origin + gh stub.
type tapLab struct {
	dir     string // the working checkout
	state   string // the gh stub's state dir ($LAB)
	remote  string
	script  string
	env     []string
	baseSHA string
	// number assigned to each seeded branch, so refsFor can map back.
	numbers map[string]string
}

func newTapLab(t *testing.T, script string, open []string) *tapLab {
	t.Helper()
	root := t.TempDir()
	l := &tapLab{
		dir:     filepath.Join(root, "repo"),
		state:   filepath.Join(root, "lab"),
		remote:  filepath.Join(root, "remote"),
		script:  script,
		numbers: map[string]string{},
	}
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{l.dir, l.state, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(fakeGH), 0o700); err != nil { //nolint:gosec // a test stub that must be executable
		t.Fatal(err)
	}
	l.env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LAB="+l.state,
		"HOME="+root, // never read the operator's ~/.gitconfig
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"),
	)
	for _, f := range []string{"prs", "closed", "created", "merged"} {
		if err := os.WriteFile(filepath.Join(l.state, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	l.git(t, root, "init", "-q", "--bare", l.remote)
	l.git(t, l.dir, "init", "-q", "-b", "main")
	l.git(t, l.dir, "remote", "add", "origin", l.remote)
	if err := os.MkdirAll(filepath.Join(l.dir, "Formula"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.dir, "Cask"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.write(t, filepath.Join("Formula", "iterion.rb"), "base")
	l.write(t, filepath.Join("Cask", "iterion-desktop.rb"), "base")
	l.git(t, l.dir, "add", "-A")
	l.git(t, l.dir, "commit", "-qm", "base")
	l.git(t, l.dir, "push", "-q", "-u", "origin", "main")
	l.baseSHA = l.rev(t, "refs/heads/main")

	// Seed the open PRs. The stub's file is NEWEST-FIRST, and `open` is given
	// oldest-first, so write it in reverse.
	var lines []string
	for i, ref := range open {
		num := "10" + string(rune('0'+i%10)) + string(rune('0'+i/10))
		l.numbers[num] = ref
		lines = append([]string{num + "\t" + ref}, lines...)
		if strings.HasPrefix(ref, "chore/brew-tap-") {
			l.git(t, l.dir, "checkout", "-q", "-b", ref)
			l.write(t, filepath.Join("Formula", "iterion.rb"), ref)
			l.git(t, l.dir, "commit", "-qam", ref)
			l.git(t, l.dir, "push", "-q", "origin", ref)
			l.git(t, l.dir, "checkout", "-q", "main")
		}
	}
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(l.state, "prs"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *tapLab) git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = l.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (l *tapLab) rev(t *testing.T, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", l.remote, "rev-parse", ref)
	cmd.Env = l.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v\n%s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (l *tapLab) write(t *testing.T, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(l.dir, rel), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (l *tapLab) run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{l.script}, args...)...)
	cmd.Dir = l.dir
	cmd.Env = l.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("brew-tap-pr.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// refsFor maps the numbers recorded in an action file back to branch names,
// so the assertions read in branch terms rather than in stub-assigned ids.
func (l *tapLab) refsFor(t *testing.T, action string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.state, action))
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, line := range strings.Fields(string(b)) {
		if ref, ok := l.numbers[line]; ok {
			refs = append(refs, ref)
			continue
		}
		refs = append(refs, line)
	}
	return refs
}

func (l *tapLab) openRefs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.state, "prs"))
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// sameSet compares ignoring order and duplicates — the sweep's contract is
// "which PRs", not "in what sequence".
func sameSet(got, want []string) bool {
	index := func(ss []string) map[string]bool {
		m := map[string]bool{}
		for _, s := range ss {
			m[s] = true
		}
		return m
	}
	g, w := index(got), index(want)
	if len(g) != len(w) {
		return false
	}
	for k := range w {
		if !g[k] {
			return false
		}
	}
	return true
}
