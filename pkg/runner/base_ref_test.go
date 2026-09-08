package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// A STACKED pull request — base = another feature branch, not the repo's
// default — used to reach a bot as a `base_ref` naming a ref its workspace
// did not hold: a clone creates a LOCAL branch only for the default branch,
// and git's revision lookup does not fall back from a bare name to
// `refs/remotes/origin/<name>`. Measured on `/billy` against PR #850: the
// campaign's plan step ran `git merge-base fix/board-… HEAD` and exited 128
// on "Not a valid object name" before reading a line of the diff.
//
// The fixture is prepareRepoWorkspace's own shape — clone --no-tags, fetch
// the PR head, `checkout -B` it — because that is what produces the missing
// ref. (prepareRepoWorkspace itself cannot be driven here: its SSRF guard
// only accepts https/ssh forge URLs, never a local fixture path.)
func TestEnsureBaseRef_MakesAStackedPRsBaseResolvable(t *testing.T) {
	f := newStackedPRFixture(t)

	// The defect: the base branch is unreachable under the name every bot
	// writes, on the clone the runner just made.
	if err := gitErr(f.clone, "merge-base", f.base, "HEAD"); err == nil {
		t.Fatal("precondition: the base already resolved — the fixture does not reproduce a stacked PR")
	}

	f.r.ensureBaseRef(context.Background(), f.clone, "", nil, f.msg)

	// The bare name — the form the bots' `git merge-base <base_ref> HEAD`
	// and `git diff <base_ref>..HEAD` use.
	if err := gitErr(f.clone, "merge-base", f.base, "HEAD"); err != nil {
		t.Errorf("git merge-base %s HEAD still fails after the fetch: %v", f.base, err)
	}
	// And `origin/<base>` — the form the review scope anchors on.
	if err := gitErr(f.clone, "rev-parse", "--verify", "origin/"+f.base); err != nil {
		t.Errorf("origin/%s does not resolve after the fetch: %v", f.base, err)
	}
	// The base's own tip is what was fetched, not the head's.
	if got := gitOut(t, f.clone, "rev-parse", f.base); got != f.baseTip {
		t.Errorf("%s = %s, want the base branch tip %s", f.base, got, f.baseTip)
	}
	// The run stays on the PR head — fetching the base must not move it.
	if got := gitOut(t, f.clone, "rev-parse", "HEAD"); got != f.headTip {
		t.Errorf("HEAD = %s, want the PR head %s — the base fetch moved the checkout", got, f.headTip)
	}
}

// The cases that must NOT spend a network fetch, and the one that must not
// be handed to git at all.
func TestEnsureBaseRef_SkipsWhatNeedsNoFetch(t *testing.T) {
	cases := []struct {
		name    string
		baseRef string
		repoSHA string
	}{
		// The overwhelmingly common shape: the PR targets the default
		// branch, which the clone already checked out locally.
		{"default branch is already local", "main", "featurehead"},
		// A base that IS the checked-out ref: git refuses to fetch into the
		// current branch, and there is nothing to resolve.
		{"base equals the checked-out head", "featurehead", "featurehead"},
		// No base_ref at all (a launch that is not about a PR).
		{"no base_ref", "", "featurehead"},
		// Flag-shaped: base_ref comes from a forge payload and reaches a git
		// subprocess, so it is refused rather than passed through.
		{"flag-shaped ref", "--upload-pack=/evil", "featurehead"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStackedPRFixture(t)
			f.msg.Vars["base_ref"] = tc.baseRef
			f.msg.RepoSHA = tc.repoSHA

			before := gitOut(t, f.clone, "for-each-ref", "--format=%(refname)")
			f.r.ensureBaseRef(context.Background(), f.clone, "", nil, f.msg)
			if after := gitOut(t, f.clone, "for-each-ref", "--format=%(refname)"); after != before {
				t.Errorf("refs changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

type stackedPRFixture struct {
	r                *Runner
	msg              *queue.RunMessage
	clone            string
	base             string
	baseTip, headTip string
}

// newStackedPRFixture builds `origin` with main → featurebase → featurehead
// and clones it exactly as prepareRepoWorkspace does for a run whose
// RepoSHA is the PR head and whose base_ref is featurebase.
func newStackedPRFixture(t *testing.T) *stackedPRFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	gitOut(t, tmp, "init", "--quiet", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(tmp, "seed")
	gitOut(t, tmp, "clone", "--quiet", origin, seed)
	gitOut(t, seed, "config", "user.email", "t@test.invalid")
	gitOut(t, seed, "config", "user.name", "t")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "baseline")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:main")
	gitOut(t, seed, "checkout", "--quiet", "-b", "featurebase")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "the base branch's own work")
	baseTip := gitOut(t, seed, "rev-parse", "HEAD")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:refs/heads/featurebase")
	gitOut(t, seed, "checkout", "--quiet", "-b", "featurehead")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "the PR's own work")
	headTip := gitOut(t, seed, "rev-parse", "HEAD")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:refs/heads/featurehead")

	clone := filepath.Join(tmp, "run")
	gitOut(t, tmp, "clone", "--no-tags", "--quiet", origin, clone)
	gitOut(t, clone, "fetch", "--no-tags", "--quiet", "origin", "featurehead")
	gitOut(t, clone, "checkout", "--quiet", "-B", "featurehead", "FETCH_HEAD")

	return &stackedPRFixture{
		r: &Runner{cfg: Config{Logger: iterlog.Nop()}},
		msg: &queue.RunMessage{
			RunID:   "run-stacked-pr",
			RepoURL: origin,
			RepoSHA: "featurehead",
			Vars:    map[string]any{"base_ref": "featurebase"},
		},
		clone:   clone,
		base:    "featurebase",
		baseTip: baseTip,
		headTip: headTip,
	}
}

// gitErr runs git and returns only whether it failed — for asserting that a
// revision does (or does not) resolve.
func gitErr(dir string, args ...string) error {
	if out, err := gittest.Try(dir, args...); err != nil {
		return &gitFailure{args: strings.Join(args, " "), out: out}
	}
	return nil
}

type gitFailure struct{ args, out string }

func (e *gitFailure) Error() string { return "git " + e.args + ": " + e.out }
