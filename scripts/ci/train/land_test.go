package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeGitHub squashes on the fixture's bare origin the way GitHub does: the
// three-way merge of the pull request's head onto main, one parent, main
// advanced — and refuses a head that is not the pinned one.
type fakeGitHub struct {
	t        *testing.T
	f        *fixture
	prs      map[int]PR
	queue    []int
	merged   map[int]bool
	merges   []int
	bodies   map[int]string
	comments map[int]string
	cancels  []string
	// tamper, when set, lands a commit whose tree is not the merge: GitHub
	// landing something other than what was gated.
	tamper bool
	// interleave, when set, lands another commit on main just before the
	// squash: a push that slipped in between two landings.
	interleave bool
}

func newFakeGitHub(t *testing.T, f *fixture, prs []PR) *fakeGitHub {
	g := &fakeGitHub{t: t, f: f, prs: map[int]PR{}, merged: map[int]bool{}, bodies: map[int]string{}, comments: map[int]string{}}
	for _, pr := range prs {
		g.prs[pr.Number] = pr
	}
	return g
}

func (g *fakeGitHub) originGit(args ...string) (string, error) {
	out, err := (&gitRunner{}).run(context.Background(), g.f.origin, args...)
	return strings.TrimSpace(out), err
}

func (g *fakeGitHub) PullRequest(_ context.Context, n int) (PR, error) {
	pr, ok := g.prs[n]
	if !ok {
		return PR{}, fmt.Errorf("no pull request #%d", n)
	}
	head, err := g.originGit("rev-parse", "refs/heads/"+pr.HeadRef)
	if err != nil {
		return PR{}, err
	}
	pr.HeadSHA = head
	if g.merged[n] {
		pr.State = "merged"
	}
	return pr, nil
}

func (g *fakeGitHub) CommitMessages(_ context.Context, n int) ([]string, error) {
	return []string{fmt.Sprintf("change %d\n\nCo-Authored-By: Someone <s@example.com>", n), "fixup\n\nco-authored-by: someone <S@example.com>"}, nil
}

func (g *fakeGitHub) QueueEntries(context.Context) ([]int, error) { return g.queue, nil }

func (g *fakeGitHub) Merge(ctx context.Context, n int, headSHA, subject, body string) error {
	pr, err := g.PullRequest(ctx, n)
	if err != nil {
		return err
	}
	if pr.HeadSHA != headSHA {
		return fmt.Errorf("head of #%d is %s, not %s", n, pr.HeadSHA, headSHA)
	}
	main, err := g.originGit("rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	if g.interleave {
		mainTree, err := g.originGit("rev-parse", main+"^{tree}")
		if err != nil {
			return err
		}
		other, err := g.originGit("commit-tree", mainTree, "-p", main, "-m", "chore: release")
		if err != nil {
			return err
		}
		if _, err := g.originGit("update-ref", "refs/heads/main", other, main); err != nil {
			return err
		}
		main = other
	}
	tree, err := g.originGit("merge-tree", "--write-tree", "--no-messages", main, headSHA)
	if err != nil {
		return fmt.Errorf("#%d is not mergeable: %w", n, err)
	}
	tree = strings.SplitN(tree, "\n", 2)[0]
	if g.tamper {
		// A commit on main whose tree is not the merge: main's previous tree.
		if tree, err = g.originGit("rev-parse", main+"^{tree}"); err != nil {
			return err
		}
	}
	commit, err := g.originGit("commit-tree", tree, "-p", main, "-m", subject+"\n\n"+body)
	if err != nil {
		return err
	}
	if _, err := g.originGit("update-ref", "refs/heads/main", commit, main); err != nil {
		return err
	}
	g.merged[n] = true
	g.merges = append(g.merges, n)
	g.bodies[n] = body
	return nil
}

func (g *fakeGitHub) CancelPullRequestRuns(_ context.Context, sha string) (int, error) {
	g.cancels = append(g.cancels, sha)
	return 1, nil
}

func (g *fakeGitHub) Comment(_ context.Context, n int, body string) error {
	g.comments[n] = body
	return nil
}

func greenLot(t *testing.T, f *fixture, prs []PR) *Report {
	t.Helper()
	rep, err := f.train(&markerGate{}).Run(context.Background(), f.base, prs)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != verdictGreen {
		t.Fatalf("fixture lot is %s, want green", rep.Verdict)
	}
	return rep
}

func lander(f *fixture, gh GitHub) *Lander {
	return &Lander{Dir: f.dir, Remote: "origin", Git: &gitRunner{}, GH: gh, Log: io.Discard}
}

func originMain(t *testing.T, f *fixture) (commit, tree string) {
	t.Helper()
	return mustGit(t, f.origin, "rev-parse", "refs/heads/main"), mustGit(t, f.origin, "rev-parse", "refs/heads/main^{tree}")
}

func TestLandMergesEachPullRequestOntoTheGatedTree(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"}), f.pr(3, map[string]string{"d.txt": "d\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	if err := lander(f, gh).Land(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.merges) != "[1 2 3]" || len(rep.Landed) != 3 {
		t.Fatalf("merges %v, landed %d — want 1, 2, 3 in order", gh.merges, len(rep.Landed))
	}
	_, tree := originMain(t, f)
	if tree != rep.Steps[2].Tree {
		t.Fatalf("main's tree %s, want the gated %s", tree, rep.Steps[2].Tree)
	}
	for i, l := range rep.Landed {
		if l.Tree != rep.Steps[i].Tree || l.UpdatedBranch {
			t.Errorf("landing %d: %+v, want the gated tree with no branch update", i, l)
		}
	}
	body := gh.bodies[2]
	if !strings.Contains(body, "Landing-Train: t-test") || strings.Count(strings.ToLower(body), "co-authored-by: someone <s@example.com>") != 3 {
		t.Fatalf("squash body:\n%s\nwant the train trailer and the co-author once in the last paragraph (plus the two bullets)", body)
	}
	last := body[strings.LastIndex(body, "\n\n")+2:]
	if !strings.HasPrefix(last, "Landing-Train:") || strings.Count(strings.ToLower(last), "co-authored-by") != 1 {
		t.Fatalf("last paragraph %q: want Landing-Train then one deduplicated co-author", last)
	}
	if len(gh.comments) != 3 || len(gh.cancels) != 3 {
		t.Fatalf("comments %d, cancels %v — want one each per pull request", len(gh.comments), gh.cancels)
	}
}

func TestLandCarriesTheTrainsResolutionOntoTheBranch(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n", "docs/z.md": "z\n"})
	prs := []PR{f.pr(1, map[string]string{"docs/b.md": "b\n"}), f.pr(2, map[string]string{"docs/c.md": "c\n"})}
	rep := greenLot(t, f, prs)
	if len(rep.Steps[1].Regenerated) == 0 {
		t.Fatal("fixture: #2's step regenerated nothing, the test needs a golden conflict")
	}
	gh := newFakeGitHub(t, f, prs)
	if err := lander(f, gh).Land(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	_, tree := originMain(t, f)
	if tree != rep.Steps[1].Tree {
		t.Fatalf("main's tree %s, want the gated %s", tree, rep.Steps[1].Tree)
	}
	if !rep.Landed[1].UpdatedBranch || rep.Landed[0].UpdatedBranch {
		t.Fatalf("landings %+v: want only #2's branch updated", rep.Landed)
	}
	branch := mustGit(t, f.origin, "rev-parse", "refs/heads/pr-2")
	if branch == prs[1].HeadSHA {
		t.Fatal("#2's branch was not updated")
	}
	// The update descends from the author's head: a fast-forward, nothing
	// of theirs rewritten.
	if _, code, _ := (&gitRunner{}).runCode(context.Background(), f.origin, "merge-base", "--is-ancestor", prs[1].HeadSHA, branch); code != 0 {
		t.Fatal("#2's updated branch does not descend from its head")
	}
}

func TestLandRefusesBeforeTouchingAnything(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fixture, gh *fakeGitHub, rep *Report, prs []PR)
		want  string
	}{
		{"main moved", func(t *testing.T, f *fixture, _ *fakeGitHub, _ *Report, _ []PR) {
			mustGit(t, f.seed, "checkout", "--quiet", "-B", "other", f.base)
			writeFiles(t, f.seed, map[string]string{"x.txt": "x\n"})
			mustGit(t, f.seed, "add", "-A")
			mustGit(t, f.seed, "commit", "--quiet", "-m", "release")
			mustGit(t, f.seed, "push", "--quiet", "origin", "HEAD:refs/heads/main")
		}, "main moved"},
		{"queue busy", func(_ *testing.T, _ *fixture, gh *fakeGitHub, _ *Report, _ []PR) { gh.queue = []int{42} }, "merge queue"},
		{"head moved", func(t *testing.T, f *fixture, _ *fakeGitHub, _ *Report, prs []PR) {
			mustGit(t, f.seed, "checkout", "--quiet", "-B", "pr-2", prs[1].HeadSHA)
			writeFiles(t, f.seed, map[string]string{"late.txt": "x\n"})
			mustGit(t, f.seed, "add", "-A")
			mustGit(t, f.seed, "commit", "--quiet", "-m", "late push")
			mustGit(t, f.seed, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/pr-2")
		}, "#2 moved"},
		{"not green", func(_ *testing.T, _ *fixture, _ *fakeGitHub, rep *Report, _ []PR) { rep.Verdict = verdictRed }, "not green"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"a.txt": "a\n"})
			prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
			rep := greenLot(t, f, prs)
			gh := newFakeGitHub(t, f, prs)
			tc.setup(t, f, gh, rep, prs)
			before, _ := originMain(t, f)
			err := lander(f, gh).Land(context.Background(), rep)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one containing %q", err, tc.want)
			}
			after, _ := originMain(t, f)
			if len(gh.merges) != 0 || after != before {
				t.Fatalf("merges %v, main %s → %s: a refusal must touch nothing", gh.merges, short(before), short(after))
			}
		})
	}
}

func TestLandStopsWhenMainDoesNotCarryTheGatedTree(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.tamper = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "not the gated tree") {
		t.Fatalf("err %v, want the tree mismatch named", err)
	}
	if fmt.Sprint(gh.merges) != "[1]" || !strings.Contains(rep.Unlanded, "#1") {
		t.Fatalf("merges %v, unlanded %q — want the landing stopped right after #1, recorded", gh.merges, rep.Unlanded)
	}
}

func TestLandStopsWhenSomethingElseLandsInBetween(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.interleave = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "not a child of") {
		t.Fatalf("err %v, want the foreign commit named", err)
	}
	if fmt.Sprint(gh.merges) != "[1]" {
		t.Fatalf("merges %v, want the landing stopped after #1", gh.merges)
	}
}

func TestLandDryRunPushesAndMergesNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n", "docs/z.md": "z\n"})
	prs := []PR{f.pr(1, map[string]string{"docs/b.md": "b\n"}), f.pr(2, map[string]string{"docs/c.md": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	before, _ := originMain(t, f)
	l := lander(f, gh)
	l.DryRun = true
	if err := l.Land(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	after, _ := originMain(t, f)
	if len(gh.merges) != 0 || after != before || mustGit(t, f.origin, "rev-parse", "refs/heads/pr-2") != prs[1].HeadSHA || len(rep.Landed) != 0 {
		t.Fatalf("a dry run changed something: merges %v, main %s → %s", gh.merges, short(before), short(after))
	}
}

func TestLandReportsAMergeRefusedByGitHub(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	gh := &refusingMerge{fakeGitHub: newFakeGitHub(t, f, prs)}
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "landed so far: 0 of 1") {
		t.Fatalf("err %v, want the refusal with the count landed", err)
	}
}

type refusingMerge struct{ *fakeGitHub }

func (r *refusingMerge) Merge(context.Context, int, string, string, string) error {
	return errors.New("GraphQL: Pull request is not mergeable")
}
