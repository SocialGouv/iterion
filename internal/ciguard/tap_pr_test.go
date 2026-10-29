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

// scripts/brew-tap-pr.sh against a real remote, through brew_tap_test.go's
// stand-in gh. Tap branches outlive their merged, closed or never-opened PR
// here (delete_branch_on_merge is off): `branch` builds on one only while its
// PR is open and rebuilds it from main otherwise, and `publish` opens a PR
// only for what main does not carry yet and never pushes anything below
// main's versions.

const tapBranch = "chore/brew-tap-v1.1.0"

// tapVersions is a tap's formula and cask versions.
type tapVersions struct{ formula, cask string }

func (v tapVersions) files() map[string]string {
	return map[string]string{
		"Formula/iterion.rb":      "class Iterion < Formula\n  version \"" + v.formula + "\"\nend\n",
		"Cask/iterion-desktop.rb": "cask \"iterion-desktop\" do\n  version \"" + v.cask + "\"\nend\n",
	}
}

// tapRemote is a bare origin whose main carries onMain and whose v1.1.0 tap
// branch was left at onBranch, diverged from main as a squash merge leaves
// it, and a clone of it checked out on main.
func tapRemote(t *testing.T, onMain, onBranch tapVersions) (clone, origin string) {
	t.Helper()
	seed := t.TempDir()
	gittest.InitRepo(t, seed)
	commit(t, seed, "base", tapVersions{"1.0.0", "1.0.0"}.files())
	gittest.Run(t, seed, "checkout", "-q", "-b", tapBranch)
	commit(t, seed, "chore(brew): update tap to v1.1.0", onBranch.files())
	gittest.Run(t, seed, "checkout", "-q", "main")
	onMainFiles := onMain.files()
	onMainFiles["README.md"] = "main moved on\n" // never an empty commit, whatever onMain is
	commit(t, seed, "main moves on", onMainFiles)

	origin = filepath.Join(t.TempDir(), "origin.git")
	gittest.Run(t, seed, "clone", "-q", "--bare", seed, origin)
	for _, kv := range [][2]string{{"receive.autogc", "false"}, {"maintenance.auto", "false"}, {"gc.auto", "0"}} {
		gittest.Run(t, origin, "config", kv[0], kv[1])
	}
	clone = filepath.Join(t.TempDir(), "clone")
	gittest.Run(t, seed, "clone", "-q", origin, clone)
	for _, kv := range [][2]string{
		{"user.name", "fixture"}, {"user.email", "fixture@example.invalid"},
		{"commit.gpgsign", "false"}, {"maintenance.auto", "false"}, {"gc.auto", "0"},
	} {
		gittest.Run(t, clone, "config", kv[0], kv[1])
	}
	return clone, origin
}

// openPR is a pull request gh reports open; fork marks one whose head lives
// in another repository.
type openPR struct {
	number int
	head   string
	fork   bool
}

type tapPRRun struct {
	code  int
	out   string
	calls string // every gh call, one per line
}

// brewTapPR runs one subcommand of the script in clone, with open as the
// pull requests gh reports open.
func brewTapPR(t *testing.T, clone string, open []openPR, args ...string) tapPRRun {
	t.Helper()
	return brewTapPREnv(t, clone, open, nil, args...)
}

// brewTapPREnv is brewTapPR with extra environment, and a `git` that fails
// `ls-remote` when env carries FAIL_LS_REMOTE=1.
func brewTapPREnv(t *testing.T, clone string, open []openPR, env []string, args ...string) tapPRRun {
	t.Helper()
	dir := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"bin/git": "#!/usr/bin/env bash\n" +
		"if [ \"${FAIL_LS_REMOTE:-}\" = 1 ] && [ \"$1\" = ls-remote ]; then echo 'fatal: ls-remote failed (stand-in)' >&2; exit 128; fi\n" +
		"exec " + realGit + " \"$@\"\n"})
	if err := os.Chmod(filepath.Join(dir, "bin", "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	prs := []map[string]any{}
	for _, pr := range open {
		prs = append(prs, map[string]any{"number": pr.number, "headRefName": pr.head, "isCrossRepository": pr.fork})
	}
	raw, err := json.Marshal(prs)
	if err != nil {
		t.Fatal(err)
	}
	openFile, ghLog := filepath.Join(dir, "open-prs.json"), filepath.Join(dir, "gh.log")
	writeFiles(t, dir, map[string]string{"open-prs.json": string(raw), "bin/gh": standInTapGH})
	if err := os.Chmod(filepath.Join(dir, "bin", "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(brewTapScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{scriptUnderTest(t, script)}, args...)...)
	cmd.Dir = clone
	cmd.Env = scriptEnv(t)
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PATH=") {
			cmd.Env[i] = "PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
	}
	cmd.Env = append(cmd.Env, "TAP_OPEN_PRS="+openFile, "TAP_GH_LOG="+ghLog)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	r := tapPRRun{out: string(out)}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", script, err)
		}
		r.code = ee.ExitCode()
	}
	calls, _ := os.ReadFile(ghLog)
	r.calls = string(calls)
	return r
}

func mustSucceed(t *testing.T, what string, r tapPRRun) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("%s: exit %d\n%s", what, r.code, r.out)
	}
}

// remoteTap reads the versions origin's tap branch carries.
func remoteTap(t *testing.T, origin string) tapVersions {
	t.Helper()
	read := func(path string) string {
		m := regexpVersion.FindStringSubmatch(gittest.Run(t, origin, "show", tapBranch+":"+path))
		if m == nil {
			t.Fatalf("%s on %s carries no version", path, tapBranch)
		}
		return m[1]
	}
	return tapVersions{read("Formula/iterion.rb"), read("Cask/iterion-desktop.rb")}
}

func TestTapPROpensNothingFromALeftoverMainAlreadyCarries(t *testing.T) {
	clone, _ := tapRemote(t, tapVersions{"1.1.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	r := brewTapPR(t, clone, nil, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if strings.Contains(r.calls, "pr create") || strings.Contains(r.calls, "pr merge") {
		t.Fatalf("publish opened a pull request main already carries:\n%s\n%s", r.calls, r.out)
	}
	if !strings.Contains(r.out, "main already carries") {
		t.Fatalf("publish did not say why it opened nothing:\n%s", r.out)
	}
}

func TestTapPROpensWhatMainDoesNotCarry(t *testing.T) {
	// The late desktop ZIP's catch-up: main carries the formula, not the cask.
	clone, origin := tapRemote(t, tapVersions{"1.1.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	writeFiles(t, clone, map[string]string{"Cask/iterion-desktop.rb": tapVersions{"1.1.0", "1.1.0"}.files()["Cask/iterion-desktop.rb"]})
	r := brewTapPR(t, clone, nil, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if !strings.Contains(r.calls, "pr create") || !strings.Contains(r.calls, "pr merge https://github.com/SocialGouv/iterion/pull/4242 --auto --squash") {
		t.Fatalf("publish did not open and arm the pull request for the cask:\n%s\n%s", r.calls, r.out)
	}
	if got := remoteTap(t, origin); got != (tapVersions{"1.1.0", "1.1.0"}) {
		t.Fatalf("origin's tap branch carries %+v, want formula and cask at 1.1.0", got)
	}
}

func TestTapPRRebuildsALeftoverBranchFromMain(t *testing.T) {
	// The branch was pushed but its PR never opened, and main's cask has
	// moved past it since (a desktop-only release dispatched into it).
	// Building on it would refuse every night, or walk the cask back.
	clone, origin := tapRemote(t, tapVersions{"1.0.0", "1.2.0"}, tapVersions{"1.1.0", "1.1.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	if got := versionIn(t, clone, "Cask/iterion-desktop.rb"); got != "1.2.0" {
		t.Fatalf("branch built on the leftover: the cask reads %s, main's is 1.2.0", got)
	}
	writeFiles(t, clone, map[string]string{"Formula/iterion.rb": tapVersions{"1.1.0", "1.2.0"}.files()["Formula/iterion.rb"]})
	r := brewTapPR(t, clone, nil, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if !strings.Contains(r.calls, "pr create") {
		t.Fatalf("publish opened no pull request for the formula:\n%s\n%s", r.calls, r.out)
	}
	if got := remoteTap(t, origin); got != (tapVersions{"1.1.0", "1.2.0"}) {
		t.Fatalf("origin's tap branch carries %+v, want formula 1.1.0 over main's cask 1.2.0", got)
	}
}

func TestTapPRBuildsOnTheBranchOfAnOpenPR(t *testing.T) {
	clone, _ := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, []openPR{{7, tapBranch, false}}, "branch", "v1.1.0"))
	if got := versionIn(t, clone, "Formula/iterion.rb"); got != "1.1.0" {
		t.Fatalf("with PR #7 open, branch did not build on its branch: the formula reads %s", got)
	}
}

func TestTapPRRefusesAWalkBackWithoutSweeping(t *testing.T) {
	// PR #7's branch still carries cask 1.0.0; main's moved to 1.2.0 since.
	// PR #5, for an older tag, must survive a run that lands nothing.
	clone, origin := tapRemote(t, tapVersions{"1.0.0", "1.2.0"}, tapVersions{"1.1.0", "1.0.0"})
	open := []openPR{{7, tapBranch, false}, {5, "chore/brew-tap-v1.0.1", false}}
	before := gittest.Run(t, origin, "rev-parse", tapBranch)
	mustSucceed(t, "branch", brewTapPR(t, clone, open, "branch", "v1.1.0"))
	r := brewTapPR(t, clone, open, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if !strings.Contains(r.out, "walk the tap back") {
		t.Fatalf("publish did not refuse the walk-back:\n%s", r.out)
	}
	if strings.Contains(r.calls, "pr close") || strings.Contains(r.calls, "pr create") {
		t.Fatalf("a run that lands nothing still opened or closed pull requests:\n%s", r.calls)
	}
	if after := gittest.Run(t, origin, "rev-parse", tapBranch); after != before {
		t.Fatalf("a refused run pushed %s over %s", after, before)
	}
}

func TestTapPRIgnoresAForkPRNamedLikeANewerTap(t *testing.T) {
	// Anyone can open a pull request from a fork branch named
	// chore/brew-tap-v999.0.0: it is no newer tap work, and must not freeze
	// the tap.
	clone, _ := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	open := []openPR{{900, "chore/brew-tap-v999.0.0", true}}
	mustSucceed(t, "branch", brewTapPR(t, clone, open, "branch", "v1.1.0"))
	writeFiles(t, clone, map[string]string{"Formula/iterion.rb": tapVersions{"1.1.0", "1.0.0"}.files()["Formula/iterion.rb"]})
	r := brewTapPR(t, clone, open, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if !strings.Contains(r.calls, "pr create") || strings.Contains(r.calls, "pr close 900") {
		t.Fatalf("a fork PR froze the tap or was swept as a tap PR:\n%s\n%s", r.calls, r.out)
	}
}

func TestTapPRIgnoresAForkPRForTheSameTag(t *testing.T) {
	clone, _ := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	open := []openPR{{900, tapBranch, true}}
	b := brewTapPR(t, clone, open, "branch", "v1.1.0")
	mustSucceed(t, "branch", b)
	if strings.Contains(b.out, "open PR #900") {
		t.Fatalf("branch built on a fork PR's branch name:\n%s", b.out)
	}
	writeFiles(t, clone, map[string]string{"Formula/iterion.rb": tapVersions{"1.1.0", "1.0.0"}.files()["Formula/iterion.rb"]})
	r := brewTapPR(t, clone, open, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if !strings.Contains(r.calls, "pr create") {
		t.Fatalf("publish took the fork's PR #900 for its own and opened none:\n%s\n%s", r.calls, r.out)
	}
}

func TestTapPRNeverOverwritesAPushMadeDuringTheRun(t *testing.T) {
	clone, origin := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	open := []openPR{{7, tapBranch, false}}
	mustSucceed(t, "branch", brewTapPR(t, clone, open, "branch", "v1.1.0"))

	// A maintainer pushes to the PR's branch while the run downloads.
	other := filepath.Join(t.TempDir(), "other")
	gittest.Run(t, clone, "clone", "-q", "--branch", tapBranch, origin, other)
	gittest.Run(t, other, "config", "maintenance.auto", "false")
	commit(t, other, "a maintainer's fix", map[string]string{"NOTES.md": "by hand\n"})
	gittest.Run(t, other, "push", "-q", "origin", tapBranch)
	theirs := gittest.Run(t, other, "rev-parse", "HEAD")

	writeFiles(t, clone, map[string]string{"Cask/iterion-desktop.rb": tapVersions{"1.1.0", "1.1.0"}.files()["Cask/iterion-desktop.rb"]})
	r := brewTapPR(t, clone, open, "publish", "v1.1.0")
	if r.code == 0 {
		t.Fatalf("publish succeeded over a push it never saw:\n%s", r.out)
	}
	if tip := gittest.Run(t, origin, "rev-parse", tapBranch); tip != theirs {
		t.Fatalf("the maintainer's commit %s was overwritten by %s", theirs, tip)
	}
}

func TestTapPRTellsItsBranchFromOneThatEndsLikeIt(t *testing.T) {
	// origin holds backup/refs/heads/chore/brew-tap-v1.1.0 and no
	// chore/brew-tap-v1.1.0: `git ls-remote` matches a pattern against ref
	// tails, so even the full ref name finds the backup.
	clone, _ := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	gittest.Run(t, clone, "push", "-q", "origin", "origin/"+tapBranch+":refs/heads/backup/refs/heads/"+tapBranch)
	gittest.Run(t, clone, "push", "-q", "origin", "--delete", tapBranch)
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	writeFiles(t, clone, map[string]string{"Formula/iterion.rb": tapVersions{"1.1.0", "1.0.0"}.files()["Formula/iterion.rb"]})
	r := brewTapPR(t, clone, nil, "publish", "v1.1.0")
	mustSucceed(t, "publish", r)
	if !strings.Contains(r.calls, "pr create") {
		t.Fatalf("publish opened no pull request:\n%s\n%s", r.calls, r.out)
	}
}

func TestTapPRFailsWhenCreatePrintsNoURL(t *testing.T) {
	// Auto-merge is armed by the URL gh pr create printed; without one there
	// is nothing to arm, and the run must say so rather than end green.
	clone, _ := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	writeFiles(t, clone, map[string]string{"Formula/iterion.rb": tapVersions{"1.1.0", "1.0.0"}.files()["Formula/iterion.rb"]})
	r := brewTapPREnv(t, clone, nil, []string{"TAP_GH_CREATE_URL="}, "publish", "v1.1.0")
	if r.code == 0 || !strings.Contains(r.out, "printed no pull request URL") {
		t.Fatalf("publish ended %d without a pull request URL to arm:\n%s", r.code, r.out)
	}
}

func TestTapPRDoesNotTakeAFailedLsRemoteForAnAbsentBranch(t *testing.T) {
	// The push just landed; an ls-remote that fails after it must not read
	// as "nothing pushed" and end the run green with no pull request.
	clone, _ := tapRemote(t, tapVersions{"1.0.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	writeFiles(t, clone, map[string]string{"Formula/iterion.rb": tapVersions{"1.1.0", "1.0.0"}.files()["Formula/iterion.rb"]})
	r := brewTapPREnv(t, clone, nil, []string{"FAIL_LS_REMOTE=1"}, "publish", "v1.1.0")
	if r.code != 0 || !strings.Contains(r.calls, "pr create") {
		t.Fatalf("the branch was just pushed, yet publish asked ls-remote whether it exists and opened no pull request (exit %d):\n%s", r.code, r.out)
	}
}

func TestTapPRFailsLoudWhenItCannotReadTheBranch(t *testing.T) {
	// Nothing to push, and origin cannot be asked whether the branch exists:
	// "the tap was already current" would be a guess.
	clone, _ := tapRemote(t, tapVersions{"1.1.0", "1.0.0"}, tapVersions{"1.1.0", "1.0.0"})
	mustSucceed(t, "branch", brewTapPR(t, clone, nil, "branch", "v1.1.0"))
	r := brewTapPREnv(t, clone, nil, []string{"FAIL_LS_REMOTE=1"}, "publish", "v1.1.0")
	if r.code == 0 {
		t.Fatalf("publish ended green without knowing whether its branch exists:\n%s", r.out)
	}
}
