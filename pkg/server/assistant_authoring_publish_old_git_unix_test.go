//go:build linux || darwin

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitOlderThan240 puts first on PATH a git that answers `git version` as
// 2.39.5 and refuses `git hook` as that git refuses `git hook run
// --to-stdin`; every other subcommand goes to the real git. It returns the
// file that records each subcommand it was asked.
func gitOlderThan240(t *testing.T) (asked string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	asked = filepath.Join(t.TempDir(), "subcommands")
	bin := t.TempDir()
	writeProgram(t, filepath.Join(bin, "git"), `sub=
skip=
for a do
  if [ -n "$skip" ]; then skip=; continue; fi
  case "$a" in
    -C|-c) skip=1 ;;
    -*) ;;
    *) sub=$a; break ;;
  esac
done
printf '%s\n' "$sub" >> `+shellWord(asked)+`
case "$sub" in
  version) echo 'git version 2.39.5'; exit 0 ;;
  hook) echo "error: unknown option 'to-stdin'" >&2; exit 129 ;;
esac
exec `+shellWord(realGit)+` "$@"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return asked
}

// readSubcommands is what the git of gitOlderThan240 was asked.
func readSubcommands(t *testing.T, asked string) []string {
	t.Helper()
	body, err := os.ReadFile(asked)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// A publication on a git older than 2.40, which has no `git hook run
// --to-stdin`, pushes as that git's own push does. With no pre-push hook it
// asks no `git hook` and pushes plainly. With one at the effective hooks
// path — core.hooksPath honoured — git's push runs it, once, as it runs it
// for any push, and the publication holds the bot tree's lock through that
// push: a writer of the tree that comes in while the hook runs waits. The
// hook is never skipped.
func TestAuthoringGitPublicationOnAGitOlderThan240(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hooksPath is core.hooksPath, empty for git's default.
		hooksPath string
		hooked    bool
	}{
		{name: "without a pre-push hook"},
		{name: "with a pre-push hook", hooked: true},
		{name: "with a pre-push hook under core.hooksPath", hooksPath: ".githooks", hooked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, editor := authoringFixture(t)
			root := s.cfg.WorkDir
			initAuthoringGit(t, root)
			origin := filepath.Join(t.TempDir(), "origin.git")
			gitTestRead(t, root, "clone", "-q", "--bare", root, origin)
			gitTestRead(t, root, "remote", "add", "origin", origin)
			if tc.hooksPath != "" {
				gitTestRead(t, root, "config", "core.hooksPath", tc.hooksPath)
			}
			pinned := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))
			request := treeJSON(t, http.MethodPost, "/git-push", authoringGitPublishRequest{EditorPath: editor, Commit: pinned, Branch: "copi/published"})

			if !tc.hooked {
				asked := gitOlderThan240(t)
				rec := httptest.NewRecorder()
				s.handleAuthoringGitPublish(rec, request)
				requireTreeAnswer(t, "publication", rec, http.StatusOK, "")
				if got := readSubcommands(t, asked); slices.Contains(got, "hook") || !slices.Contains(got, "push") {
					t.Fatalf("the publication asked git %q; want a push and no `git hook`", got)
				}
			} else {
				gate := newProgramGate(t)
				record := filepath.Join(t.TempDir(), "pre-push")
				writeProgram(t, gitHookPath(t, root, "pre-push"), gate.prelude()+`{ printf '%s\n' "$*"; cat; } >> `+shellWord(record)+"\n")
				asked := gitOlderThan240(t)
				c := newTreeChoreography(t)
				publication := c.start(s.handleAuthoringGitPublish, request)
				gate.reached(t, publication, "the publication's pre-push hook")
				outcome := c.probeTree(s, "a writer of the tree")
				gate.let(t)
				requireTreeAnswer(t, "publication", c.answer(publication, "the publication"), http.StatusOK, "")
				if outcome != treeQueued {
					t.Fatalf("a writer of the tree %s while the publication's push ran its pre-push hook", probeOutcome(outcome))
				}
				if got := readSubcommands(t, asked); slices.Contains(got, "hook") {
					t.Fatalf("the publication asked git %q; this git has no `git hook run --to-stdin`", got)
				}
				ran, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				if want := "origin " + origin + "\n" + pinned + " " + pinned + " refs/heads/copi/published " + strings.Repeat("0", len(pinned)) + "\n"; string(ran) != want {
					t.Fatalf("the pre-push hook ran as\n%s\nwant it once, as\n%s", ran, want)
				}
			}
			if pushed := strings.TrimSpace(gitTestRead(t, origin, "rev-parse", "refs/heads/copi/published")); pushed != pinned {
				t.Fatalf("the publication pushed %s; it pinned %s", pushed, pinned)
			}
		})
	}
}
