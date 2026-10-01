package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The engine lands a run's output on the operator's branch — a squash commit
// or a fast-forward, at finalize or later through POST /merge, or once a
// conflict is resolved — in the operator's checkout, whose hooks directory
// and config a run shares through its worktree and can write. A hook there,
// or a `core.fsmonitor` program, is the run's code executed inside the
// landing, after every verdict the run's bots took: free to rewrite what
// lands, to commit again on top of it, or to refuse it. The landing runs
// none of them.

const landingPlan = "version: 1\nlots:\n  - id: L1\n    status: todo\n"

// landingFixture is an operator's repository and a run whose worktree
// committed its work and then the gate's `done`: what the verdict judged is
// the run's head.
type landingFixture struct {
	repo, wt, originalTip, judged string
}

func newLandingFixture(t *testing.T) landingFixture {
	t.Helper()
	repo, _ := initBareishRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".modernize"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".modernize", "plan.yaml"), landingPlan)
	gittest.Run(t, repo, "add", ".modernize/plan.yaml")
	gittest.Run(t, repo, "commit", "-m", "the contract")
	originalTip := gittest.Run(t, repo, "rev-parse", "HEAD")
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })
	addCommit(t, wt, "product.txt", "the lot's change\n", "feat: the lot's work")
	writeFile(t, filepath.Join(wt, ".modernize", "plan.yaml"), strings.Replace(landingPlan, "status: todo", "status: done", 1))
	gittest.Run(t, wt, "add", ".modernize/plan.yaml")
	gittest.Run(t, wt, "commit", "-m", "L1: done — gate, oracle and references green")
	return landingFixture{repo: repo, wt: wt, originalTip: originalTip, judged: gittest.Run(t, wt, "rev-parse", "HEAD")}
}

// plantHook writes a hook in the hooks directory the run's own tree
// resolves — the repository's, shared with the operator's checkout.
func plantHook(t *testing.T, dir, name, body string) {
	t.Helper()
	hooks := gittest.Run(t, dir, "rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(dir, hooks)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The run's forms of interference with a landing, each a hook a run can
// plant in its repository, with the gestures it fires in: a squash commits
// (and stages, and merges), a fast-forward only merges and moves a ref.
var landingInterference = []struct {
	name, hook, body string
	squash, ff       bool
}{
	{"a pre-commit hook rewriting the plan inside the landing commit", "pre-commit",
		"#!/bin/sh\nprintf 'version: 1\\nlots:\\n  - id: L1\\n    status: done\\n    exit_gate: [\"true\"]\\n' > .modernize/plan.yaml\ngit add .modernize/plan.yaml\n",
		true, false},
	{"a commit-msg hook refusing the landing", "commit-msg", "#!/bin/sh\nexit 1\n", true, false},
	{"a post-commit hook committing again on top of the landing", "post-commit",
		"#!/bin/sh\nprintf 'rewritten after the landing\\n' > .modernize/plan.yaml\ngit commit -qam 'after the landing'\n",
		true, false},
	{"a post-merge hook committing again on top of the landing", "post-merge",
		"#!/bin/sh\nprintf 'rewritten after the landing\\n' > .modernize/plan.yaml\ngit commit -qam 'after the landing'\n",
		true, true},
	// Once per checkout (the marker lives in each one's own git dir): the
	// run's worktree refreshes its index first, the landing's checkout next.
	{"a post-index-change hook staging a forged plan", "post-index-change",
		"#!/bin/sh\nm=\"$(git rev-parse --git-dir)/forged\"\n[ -f \"$m\" ] && exit 0\n: > \"$m\"\nb=$(printf 'forged\\n' | git hash-object -w --stdin)\ngit update-index --add --cacheinfo 100644,$b,.modernize/forged.yaml\n",
		true, false},
	{"a reference-transaction hook refusing every ref update", "reference-transaction",
		"#!/bin/sh\n[ \"$1\" = prepared ] && exit 1\nexit 0\n", true, true},
}

func appliesTo(squash, ff bool, strategy string) bool {
	if strategy == "merge" {
		return ff
	}
	return squash
}

// landedAsJudged asserts the one property: the target's tip is the commit the
// engine reports, and it carries exactly the tree the verdict judged.
func landedAsJudged(t *testing.T, repo, judged, reported string) {
	t.Helper()
	tip := gittest.Run(t, repo, "rev-parse", "refs/heads/main")
	if tip != reported {
		t.Fatalf("main is at %s, not at the landing commit the engine reports (%s): something committed on top of it", tip, reported)
	}
	if landed, want := gittest.Run(t, repo, "rev-parse", tip+"^{tree}"), gittest.Run(t, repo, "rev-parse", judged+"^{tree}"); landed != want {
		diff, _ := gittest.Try(repo, "diff", "--stat", judged, tip)
		t.Fatalf("main carries a tree the verdict never judged:\n%s", diff)
	}
}

func TestFinalizeWorktree_LandingRunsNoRepositoryHook(t *testing.T) {
	for _, strategy := range []string{"squash", "merge"} {
		for _, tc := range landingInterference {
			if !appliesTo(tc.squash, tc.ff, strategy) {
				continue
			}
			t.Run(strategy+": "+tc.name, func(t *testing.T) {
				f := newLandingFixture(t)
				plantHook(t, f.wt, tc.hook, tc.body)
				res := finalizeWorktree(worktreeContext{
					repoRoot:       f.repo,
					wtPath:         f.wt,
					originalBranch: "main",
					originalTip:    f.originalTip,
				}, finalizeOptions{runID: "run_landing", autoMerge: true, mergeStrategy: strategy}, nil)
				if res.MergeStatus != "merged" {
					t.Fatalf("the landing did not go through: merge_status=%q final=%q", res.MergeStatus, res.FinalCommit)
				}
				landedAsJudged(t, f.repo, f.judged, res.MergedCommit)
			})
		}
	}
}

// POST /merge lands a finalized run later, through the same gestures.
func TestPerformDeferredMerge_RunsNoRepositoryHook(t *testing.T) {
	for _, strategy := range []string{"squash", "merge"} {
		for _, tc := range landingInterference {
			if !appliesTo(tc.squash, tc.ff, strategy) {
				continue
			}
			t.Run(strategy+": "+tc.name, func(t *testing.T) {
				f := newLandingFixture(t)
				res := finalizeWorktree(worktreeContext{
					repoRoot:       f.repo,
					wtPath:         f.wt,
					originalBranch: "main",
					originalTip:    f.originalTip,
				}, finalizeOptions{runID: "run_deferred", autoMerge: false, mergeStrategy: strategy}, nil)
				if res.FinalBranch == "" || res.FinalCommit != f.judged {
					t.Fatalf("finalize did not keep the run's branch: %+v", res)
				}
				plantHook(t, f.repo, tc.hook, tc.body)
				merged, err := PerformDeferredMerge(DeferredMergeRequest{
					RepoRoot:      f.repo,
					BranchToMerge: res.FinalBranch,
					FinalSHA:      res.FinalCommit,
					Strategy:      strategy,
					Message:       "iterion run squash",
				}, nil)
				if err != nil {
					t.Fatalf("the deferred merge did not go through: %v", err)
				}
				landedAsJudged(t, f.repo, f.judged, merged.MergedCommit)
			})
		}
	}
}

// A squash that conflicts is landed once the conflict is resolved: the
// resolved file is staged, then the squash is committed.
func TestFinalizeConflictMerge_RunsNoRepositoryHook(t *testing.T) {
	for _, tc := range landingInterference {
		if tc.hook == "post-merge" {
			continue // the resolution commits; it does not merge
		}
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gittest.InitRepo(t, dir)
			writeFile(t, filepath.Join(dir, "file.txt"), "alpha\nbravo\ncharlie\n")
			if err := os.MkdirAll(filepath.Join(dir, ".modernize"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, ".modernize", "plan.yaml"), landingPlan)
			gittest.Run(t, dir, "add", "-A")
			gittest.Run(t, dir, "commit", "-qm", "base")
			gittest.Run(t, dir, "checkout", "-qb", "run")
			writeFile(t, filepath.Join(dir, "file.txt"), "alpha\nthe run's bravo\ncharlie\n")
			gittest.Run(t, dir, "commit", "-qam", "the run")
			gittest.Run(t, dir, "checkout", "-q", "main")
			writeFile(t, filepath.Join(dir, "file.txt"), "alpha\nthe operator's bravo\ncharlie\n")
			gittest.Run(t, dir, "commit", "-qam", "the operator")
			_, _ = gittest.Try(dir, "merge", "--squash", "run")
			if files, err := unmergedPaths(dir); err != nil || len(files) != 1 {
				t.Fatalf("the fixture must leave one conflicted file: %v %v", files, err)
			}

			plantHook(t, dir, tc.hook, tc.body)
			resolved := "alpha\nresolved\ncharlie\n"
			if err := StageResolvedFile(dir, "file.txt", resolved); err != nil {
				t.Fatalf("staging the resolution: %v", err)
			}
			sha, err := FinalizeConflictMerge(dir, "resolved squash")
			if err != nil {
				t.Fatalf("the resolved landing did not go through: %v", err)
			}
			if tip := gittest.Run(t, dir, "rev-parse", "refs/heads/main"); tip != sha {
				t.Fatalf("main is at %s, not at the landing commit the engine reports (%s)", tip, sha)
			}
			if got := gittest.Run(t, dir, "show", sha+":file.txt"); got != strings.TrimSuffix(resolved, "\n") {
				t.Fatalf("the landing carries a resolution nobody gave: %q", got)
			}
			if got := gittest.Run(t, dir, "show", sha+":.modernize/plan.yaml"); got != strings.TrimSuffix(landingPlan, "\n") {
				t.Fatalf("the landing rewrote the plan: %q", got)
			}
			if listed := gittest.Run(t, dir, "ls-tree", "-r", "--name-only", sha); strings.Contains(listed, "forged") {
				t.Fatalf("the landing carries a file nobody staged:\n%s", listed)
			}
		})
	}
}

// A configured program — `core.fsmonitor`, set in the repository's config a
// run can write — runs inside every git command that refreshes the index:
// the landing's own `git status` included.
func TestFinalizeWorktree_LandingRunsNoConfiguredMonitor(t *testing.T) {
	f := newLandingFixture(t)
	marker := filepath.Join(t.TempDir(), "monitor-ran")
	monitor := filepath.Join(t.TempDir(), "monitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\n: > '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, f.wt, "config", "core.fsmonitor", monitor)
	res := finalizeWorktree(worktreeContext{
		repoRoot:       f.repo,
		wtPath:         f.wt,
		originalBranch: "main",
		originalTip:    f.originalTip,
	}, finalizeOptions{runID: "run_monitor", autoMerge: true, mergeStrategy: "squash"}, nil)
	if res.MergeStatus != "merged" {
		t.Fatalf("the landing did not go through: merge_status=%q", res.MergeStatus)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the landing ran the program the repository's core.fsmonitor names")
	}
	landedAsJudged(t, f.repo, f.judged, res.MergedCommit)
}
