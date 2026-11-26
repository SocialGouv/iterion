package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// evilRegen is the golden stand-in plus a generator that, built from a tree
// carrying stage-evil.txt, stages a file that is not derived — straight into
// the index, where the worktree checks cannot see it.
type evilRegen struct{ goldenRegen }

func (r *evilRegen) Regenerate(ctx context.Context, dir string) error {
	if err := r.goldenRegen.Regenerate(ctx, dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "stage-evil.txt")); err != nil {
		return nil
	}
	p := filepath.Join(dir, ".github", "workflows", "injected.yml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte("on: push\n"), 0o644); err != nil {
		return err
	}
	_, err := (&gitRunner{}).run(ctx, dir, "add", "--", ".github/workflows/injected.yml")
	return err
}

func TestRunEjectsAPullRequestWhoseGeneratorsStageANonDerivedFile(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	p1 := f.pr(1, map[string]string{"b.txt": "b\n"})
	p2 := f.pr(2, map[string]string{"stage-evil.txt": "x\n"})
	tr := f.train(&markerGate{})
	tr.Regen = &evilRegen{}
	rep, err := tr.Run(context.Background(), f.base, []PR{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stepNumbers(rep.Steps), []int{1}) || len(rep.Ejected) != 1 || rep.Ejected[0].PR != 2 || !strings.Contains(rep.Ejected[0].Reason, "staged files that are not derived: .github/workflows/injected.yml") {
		t.Fatalf("steps %v, ejected %+v — want #2 ejected for the file its generators staged", stepNumbers(rep.Steps), rep.Ejected)
	}
	for _, s := range rep.Steps {
		if _, code, _ := (&gitRunner{}).runCode(context.Background(), f.dir, "cat-file", "-e", s.Commit+":.github/workflows/injected.yml"); code == 0 {
			t.Fatalf("step #%d carries the staged file", s.PR.Number)
		}
	}
}

// tamperGate exits 0 after rewriting a tracked file, as a member that
// "repairs" what the static members after it judge.
type tamperGate struct {
	markerGate
	skipWorktree bool
}

func (g *tamperGate) Full(ctx context.Context, dir, ev string) (GateResult, error) {
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("rewritten\n"), 0o644); err != nil {
		return GateResult{}, err
	}
	if g.skipWorktree {
		if _, err := (&gitRunner{}).run(ctx, dir, "update-index", "--skip-worktree", "a.txt"); err != nil {
			return GateResult{}, err
		}
	}
	return GateResult{Code: 0}, nil
}

func TestRunGivesNoVerdictWhenTheGateLeftTheTreeChanged(t *testing.T) {
	for _, skip := range []bool{false, true} {
		f := newFixture(t, map[string]string{"a.txt": "a\n"})
		prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
		rep, err := f.train(&tamperGate{skipWorktree: skip}).Run(context.Background(), f.base, prs)
		want := "the gate left tracked files changed"
		if skip {
			want = "skip-worktree or assume-unchanged"
		}
		if err == nil || !strings.Contains(err.Error(), want) || rep.Verdict == verdictGreen {
			t.Fatalf("skip-worktree=%v: verdict %q, err %v — want no verdict, naming %q", skip, rep.Verdict, err, want)
		}
	}
}

func TestLandRefusesToPushADifferenceTheTrainDidNotRegenerate(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	// A lot.json whose step carries one more file than #1's squash — with a
	// commit to match, so the chain check alone cannot tell.
	mustGit(t, f.dir, "checkout", "--quiet", "--detach", rep.Steps[0].Commit)
	writeFiles(t, f.dir, map[string]string{".github/workflows/forged.yml": "on: push\n"})
	mustGit(t, f.dir, "add", "-A")
	tree := mustGit(t, f.dir, "write-tree")
	forged := mustGit(t, f.dir, "commit-tree", tree, "-p", rep.Base, "-m", "forged")
	rep.Steps[0].Commit, rep.Steps[0].Tree = forged, tree
	gh := newFakeGitHub(t, f, prs)
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "did not regenerate (.github/workflows/forged.yml)") || len(gh.merges) != 0 {
		t.Fatalf("err %v, merges %v — want the forged file refused before any push or merge", err, gh.merges)
	}
	if mustGit(t, f.origin, "rev-parse", "refs/heads/pr-1") != prs[0].HeadSHA {
		t.Fatal("the forged tree reached #1's branch")
	}
}

func TestLandRefusesALotJSONThatDoesNotMatchItsCommits(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	rep.Steps[0].Tree = rep.BaseTree
	gh := newFakeGitHub(t, f, prs)
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "lot.json does not match") || len(gh.merges) != 0 {
		t.Fatalf("err %v, merges %v — want the edited lot.json refused", err, gh.merges)
	}
}

func TestPRCodeEnvDropsTheSecretsAndKeepsTheRest(t *testing.T) {
	t.Setenv("GH_TOKEN", "fake-gh")
	t.Setenv("ANTHROPIC_API_KEY", "fake-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-aws")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/fake.sock")
	t.Setenv("GH_CONFIG_DIR", "/home/someone/.config/gh")
	t.Setenv("KEEP_ME", "yes")
	env := prCodeEnv("/empty", "EXTRA=1")
	for _, kv := range env {
		for _, bad := range []string{"fake-gh", "fake-key", "fake-aws", "/tmp/fake.sock", "/home/someone/.config/gh"} {
			if strings.Contains(kv, bad) {
				t.Errorf("a secret reached pull request code: %s", kv)
			}
		}
	}
	for _, want := range []string{"KEEP_ME=yes", "GH_CONFIG_DIR=/empty", "EXTRA=1", "PATH=" + os.Getenv("PATH")} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q from the scrubbed environment", want)
		}
	}
}

func TestStateBaseIsOutsideTheCheckout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, _ := stateBase(); got != "/state/iterion/ci-train" {
		t.Fatalf("stateBase = %q, want under XDG_STATE_HOME", got)
	}
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	if got, _ := stateBase(); got != filepath.Join(home, ".local", "state", "iterion", "ci-train") {
		t.Fatalf("stateBase = %q, want ~/.local/state/iterion/ci-train", got)
	}
}

func TestParseQueue(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []int
		err      string
	}{
		{"entries", `{"data":{"repository":{"mergeQueue":{"entries":{"nodes":[{"pullRequest":{"number":7}},{"pullRequest":{"number":9}}]}}}}}`, []int{7, 9}, ""},
		{"empty queue", `{"data":{"repository":{"mergeQueue":{"entries":{"nodes":[]}}}}}`, nil, ""},
		{"no queue on main", `{"data":{"repository":{"mergeQueue":null}}}`, nil, ""},
		{"no repository", `{"data":{"repository":null}}`, nil, "cannot tell"},
		{"an error answer", `{"errors":[{"message":"API rate limit exceeded"}]}`, nil, "rate limit"},
	} {
		got, err := parseQueue([]byte(tc.in))
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v %v, want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestTouchedPathsReadsTheCommitNotGitHubsList(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	pr := f.pr(1, map[string]string{"b.txt": "b\n", "scripts/ci/x.sh": "x\n"})
	f.train(&markerGate{})
	got, err := touchedPaths(context.Background(), &gitRunner{}, f.dir, f.base, pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "scripts/ci/x.sh") || !slices.Contains(got, "b.txt") {
		t.Fatalf("touched %v, want b.txt and scripts/ci/x.sh", got)
	}
	_ = io.Discard
}
