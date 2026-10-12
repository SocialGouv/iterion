package ciguard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// brewTapScript lands a tap update as its own PR; the brew-update workflow
// runs its `branch` then `publish` subcommands for one tag.
const brewTapScript = "../../scripts/brew-tap-pr.sh"

// standInTapGH answers `gh pr list` from $TAP_OPEN_PRS (a JSON array of
// {number, headRefName}), honouring --head and --jq the way the script uses
// them, and records every call in $TAP_GH_LOG.
const standInTapGH = `#!/usr/bin/env bash
set -eu
echo "$*" >>"$TAP_GH_LOG"
[ "${1:-}" = "pr" ] || { echo "stand-in gh only knows 'pr', got: $*" >&2; exit 64; }
sub="${2:-}"
shift 2
case "$sub" in
list)
  head=""; jqexpr="."
  while [ $# -gt 0 ]; do
    case "$1" in
      --head) head="$2"; shift ;;
      --jq) jqexpr="$2"; shift ;;
      --state|--json|--limit) shift ;;
    esac
    shift
  done
  jq --arg head "$head" '[.[] | select($head == "" or .headRefName == $head)]' "$TAP_OPEN_PRS" | jq -r "$jqexpr"
  ;;
create|merge|close) ;;
*) echo "stand-in gh: unexpected 'pr $sub'" >&2; exit 64 ;;
esac
`

// tapWorld is the forge a tap run meets: the formula version main publishes
// and the PRs open when it runs.
type tapWorld struct {
	published string
	open      map[int]string // PR number → head branch
}

type tapRun struct {
	code   int
	out    string
	gh     []string // every gh call, argv joined by spaces
	pushed bool     // the tag's tap branch reached origin
}

func (r tapRun) called(prefix string) bool {
	for _, c := range r.gh {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func tapFiles(t *testing.T, dir, version string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, "Formula", "iterion.rb"), "class Iterion < Formula\n  version \""+version+"\"\nend\n")
	mustWriteFile(t, filepath.Join(dir, "Cask", "iterion-desktop.rb"), "cask \"iterion-desktop\" do\n  version \""+version+"\"\nend\n")
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runTap plays the workflow for tag against w: `branch`, the tap files
// rewritten to the tag (scripts/update-brew-tap.sh's part), then `publish`.
func runTap(t *testing.T, w tapWorld, tag string) tapRun {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("jq is required by this witness's stand-in gh; it is declared in devbox.json, so run through `devbox run --`: %v", err)
	}
	dir := t.TempDir()

	origin := filepath.Join(dir, "origin.git")
	gittest.Run(t, dir, "init", "-q", "--bare", "-b", "main", origin)
	for _, kv := range [][2]string{{"receive.autogc", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"}} {
		gittest.Run(t, origin, "config", kv[0], kv[1])
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitRepo(t, work)
	tapFiles(t, work, w.published)
	gittest.Run(t, work, "add", "Formula", "Cask")
	gittest.Run(t, work, "commit", "-q", "-m", "tap at "+w.published)
	gittest.Run(t, work, "remote", "add", "origin", origin)
	gittest.Run(t, work, "push", "-q", "origin", "main")

	prs := []map[string]any{}
	for number, head := range w.open {
		prs = append(prs, map[string]any{"number": number, "headRefName": head})
	}
	raw, err := json.Marshal(prs)
	if err != nil {
		t.Fatal(err)
	}
	openFile := filepath.Join(dir, "open-prs.json")
	mustWriteFile(t, openFile, string(raw))
	bin := filepath.Join(dir, "bin")
	mustWriteFile(t, filepath.Join(bin, "gh"), standInTapGH)
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	ghLog := filepath.Join(dir, "gh.log")

	script, err := filepath.Abs(brewTapScript)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	step := func(args ...string) int {
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Dir = work
		cmd.Env = append(gittest.Env(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"TAP_OPEN_PRS="+openFile,
			"TAP_GH_LOG="+ghLog,
		)
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			t.Fatalf("running %s %v: %v", brewTapScript, args, err)
		}
		return 0
	}
	if code := step("branch", tag); code != 0 {
		t.Fatalf("branch %s: exit %d\n%s", tag, code, out.String())
	}
	tapFiles(t, work, strings.TrimPrefix(tag, "v"))
	r := tapRun{code: step("publish", tag)}
	r.out = out.String()
	if b, err := os.ReadFile(ghLog); err == nil {
		r.gh = strings.Split(strings.TrimSpace(string(b)), "\n")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_, err = gittest.Try(origin, "rev-parse", "--verify", "--quiet", "refs/heads/chore/brew-tap-"+tag)
	r.pushed = err == nil
	return r
}

// The control: a run for the newest tag publishes its PR and closes the tap
// PR of an older one. 3.10 against 3.9 is where a string order and a version
// order disagree.
func TestBrewTap_ARunForTheNewestTagClosesTheOlderTapPR(t *testing.T) {
	r := runTap(t, tapWorld{published: "3.8.0", open: map[int]string{11: "chore/brew-tap-v3.9.0"}}, "v3.10.0")
	if r.code != 0 || !r.pushed || !r.called("pr create") || !r.called("pr merge chore/brew-tap-v3.10.0") || !r.called("pr close 11") {
		t.Fatalf("exit %d, pushed %v, gh calls %q: the newest tag's run must publish its PR and close the older one\n%s", r.code, r.pushed, r.gh, r.out)
	}
}

// A run for an older tag that comes after a newer tag's PR was opened — a
// re-run, a dispatch, a delayed run — must not close that PR nor queue its
// own older formula behind it: the tap would go back a version.
func TestBrewTap_ARunForAnOlderTagLeavesTheNewerTapPROpen(t *testing.T) {
	r := runTap(t, tapWorld{published: "3.8.0", open: map[int]string{12: "chore/brew-tap-v3.10.0"}}, "v3.9.0")
	if r.code != 0 || r.pushed || r.called("pr close") || r.called("pr create") || r.called("pr merge") {
		t.Fatalf("exit %d, pushed %v, gh calls %q: a run for v3.9.0 closed or raced the open v3.10.0 tap PR\n%s", r.code, r.pushed, r.gh, r.out)
	}
}

// A run for an older tag than the one main already publishes opens nothing.
func TestBrewTap_ARunForAnOlderTagThanMainPublishesNothing(t *testing.T) {
	r := runTap(t, tapWorld{published: "3.10.0"}, "v3.9.0")
	if r.code != 0 || r.pushed || r.called("pr create") || r.called("pr merge") {
		t.Fatalf("exit %d, pushed %v, gh calls %q: a run for v3.9.0 queued a tap PR behind a main that publishes 3.10.0\n%s", r.code, r.pushed, r.gh, r.out)
	}
}

// The second pass of the same tag (the other release pipeline) adds to its
// own open PR: it neither opens a second one nor closes its own, and still
// closes an older tag's.
func TestBrewTap_TheSecondPassOfATagKeepsItsOwnPR(t *testing.T) {
	r := runTap(t, tapWorld{published: "3.8.0", open: map[int]string{13: "chore/brew-tap-v3.9.0", 10: "chore/brew-tap-v3.8.1"}}, "v3.9.0")
	if r.code != 0 || !r.pushed || r.called("pr create") || r.called("pr close 13") || !r.called("pr close 10") {
		t.Fatalf("exit %d, pushed %v, gh calls %q: the second pass must reuse PR #13, keep it, and close the older #10\n%s", r.code, r.pushed, r.gh, r.out)
	}
}
