package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeGitHub squashes on the fixture's bare origin the way GitHub does: the
// three-way merge of the pull request's head onto main, one parent, main
// advanced — and refuses a head that is not the pinned one. Like gh, its
// Merge returns nil without merging a pull request already in a merge queue.
type fakeGitHub struct {
	t        *testing.T
	f        *fixture
	prs      map[int]PR
	queue    []int
	merged   map[int]string // PR → its merge commit
	merges   []int
	bodies   map[int]string
	comments map[int]string
	cancels  []string
	// autoMerge armed per pull request; disabled records DisableAutoMerge.
	autoMerge map[int]bool
	disabled  []int
	// disarmNoop makes DisableAutoMerge exit 0 and leave it armed.
	disarmNoop bool
	// queued pull requests make Merge a silent no-op, as gh does for a pull
	// request already in a merge queue.
	queued map[int]bool
	// tamper lands a commit whose tree is not the merge.
	tamper bool
	// interleave lands another commit on main just before the squash.
	interleave bool
	// afterMerge runs right after a squash lands (a release pushed then).
	afterMerge func(n int)
	// headLag[n] reads keep returning the old head after a branch update.
	headLag map[int]int
	seen    map[int]string
	// failReadAfterMerge makes PullRequest fail once n merged.
	failReadAfterMerge map[int]bool
	// onRead runs at every PullRequest read (a push slipped in mid-landing).
	onRead func(n int)
	// errAfterMerge makes Merge squash, then report an error (a 502 after
	// the mutation went through).
	errAfterMerge map[int]bool
	// refuseWithPush: another push lands on main and GitHub refuses the
	// merge ("Base branch was modified") without merging.
	refuseWithPush map[int]bool
	// lateMerge: GitHub answers a timeout, then finishes the merge before
	// its next read of the pull request.
	lateMerge    map[int]bool
	pendingMerge map[int]string
	pendingReads map[int]int
	// mergedReadLag: main carries n's squash, but GitHub's read-back still
	// says not merged.
	mergedReadLag map[int]bool
	// refuseBreakRemote: GitHub refuses, and main can no longer be read.
	refuseBreakRemote map[int]bool
}

func newFakeGitHub(t *testing.T, f *fixture, prs []PR) *fakeGitHub {
	g := &fakeGitHub{t: t, f: f, prs: map[int]PR{}, merged: map[int]string{}, bodies: map[int]string{}, comments: map[int]string{},
		autoMerge: map[int]bool{}, queued: map[int]bool{}, headLag: map[int]int{}, seen: map[int]string{}, failReadAfterMerge: map[int]bool{}, errAfterMerge: map[int]bool{},
		refuseWithPush: map[int]bool{}, lateMerge: map[int]bool{}, pendingMerge: map[int]string{}, refuseBreakRemote: map[int]bool{},
		pendingReads: map[int]int{}, mergedReadLag: map[int]bool{}}
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
	if g.onRead != nil {
		g.onRead(n)
	}
	if head, ok := g.pendingMerge[n]; ok && g.pendingReads[n] >= 2 {
		delete(g.pendingMerge, n)
		if err := g.squash(n, head, "late merge", ""); err != nil {
			return PR{}, err
		}
	}
	if _, done := g.merged[n]; done && g.failReadAfterMerge[n] {
		return PR{}, errors.New("HTTP 502")
	}
	head, err := g.originGit("rev-parse", "refs/heads/"+pr.HeadRef)
	if err != nil {
		return PR{}, err
	}
	if prev, ok := g.seen[n]; ok && prev != head && g.headLag[n] > 0 {
		g.headLag[n]--
		head = prev
	} else {
		g.seen[n] = head
	}
	pr.HeadSHA = head
	pr.AutoMerge = g.autoMerge[n]
	if _, ok := g.pendingMerge[n]; ok {
		g.pendingReads[n]++
	}
	if mc, done := g.merged[n]; done && !g.mergedReadLag[n] {
		pr.State, pr.Merged, pr.MergeCommit = "merged", true, mc
	}
	return pr, nil
}

func (g *fakeGitHub) CommitMessages(_ context.Context, n int) ([]string, error) {
	return []string{fmt.Sprintf("change %d\n\nCo-Authored-By: Someone <s@example.com>", n), "fixup\n\nco-authored-by: someone <S@example.com>"}, nil
}

// QueueEntries: the queue the test set, plus — once a merge happened — the
// pull requests that entered the queue mid-landing.
func (g *fakeGitHub) QueueEntries(context.Context) ([]int, error) {
	q := slices.Clone(g.queue)
	if len(g.merges) > 0 {
		for n, in := range g.queued {
			if in {
				q = append(q, n)
			}
		}
	}
	return q, nil
}

func (g *fakeGitHub) DisableAutoMerge(_ context.Context, n int) error {
	if !g.disarmNoop {
		g.autoMerge[n] = false
	}
	g.disabled = append(g.disabled, n)
	return nil
}

func (g *fakeGitHub) pushOnMain(subject string) {
	g.t.Helper()
	main, err := g.originGit("rev-parse", "refs/heads/main")
	if err != nil {
		g.t.Fatal(err)
	}
	tree, err := g.originGit("rev-parse", main+"^{tree}")
	if err != nil {
		g.t.Fatal(err)
	}
	c, err := g.originGit("commit-tree", tree, "-p", main, "-m", subject)
	if err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.originGit("update-ref", "refs/heads/main", c, main); err != nil {
		g.t.Fatal(err)
	}
}

func (g *fakeGitHub) Merge(ctx context.Context, n int, headSHA, subject, body string) error {
	if g.queued[n] {
		return nil
	}
	switch {
	case g.refuseWithPush[n]:
		g.pushOnMain("chore(release): v9.9.9")
		return errors.New("GraphQL: Base branch was modified. Review and try the merge again. (mergePullRequest)")
	case g.lateMerge[n]:
		g.pendingMerge[n] = headSHA
		return errors.New("GraphQL: timeout (mergePullRequest)")
	case g.refuseBreakRemote[n]:
		mustGit(g.t, g.f.dir, "remote", "set-url", "origin", "/nonexistent/origin.git")
		return errors.New("GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)")
	}
	return g.mergeNow(ctx, n, headSHA, subject, body)
}

// squash applies the merge on origin the way GitHub does.
func (g *fakeGitHub) squash(n int, headSHA, subject, body string) error {
	main, err := g.originGit("rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	tree, err := g.originGit("merge-tree", "--write-tree", "--no-messages", main, headSHA)
	if err != nil {
		return fmt.Errorf("#%d is not mergeable: %w", n, err)
	}
	commit, err := g.originGit("commit-tree", strings.SplitN(tree, "\n", 2)[0], "-p", main, "-m", subject+"\n\n"+body)
	if err != nil {
		return err
	}
	if _, err := g.originGit("update-ref", "refs/heads/main", commit, main); err != nil {
		return err
	}
	g.merged[n] = commit
	g.merges = append(g.merges, n)
	return nil
}

func (g *fakeGitHub) mergeNow(ctx context.Context, n int, headSHA, subject, body string) error {
	pr, err := g.PullRequest(ctx, n)
	if err != nil {
		return err
	}
	if pr.HeadSHA != headSHA {
		return fmt.Errorf("head of #%d is %s, not %s", n, pr.HeadSHA, headSHA)
	}
	if g.interleave {
		g.pushOnMain("chore: release")
	}
	main, err := g.originGit("rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	tree, err := g.originGit("merge-tree", "--write-tree", "--no-messages", main, headSHA)
	if err != nil {
		return fmt.Errorf("#%d is not mergeable: %w", n, err)
	}
	tree = strings.SplitN(tree, "\n", 2)[0]
	if g.tamper {
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
	g.merged[n] = commit
	g.merges = append(g.merges, n)
	g.bodies[n] = body
	if g.afterMerge != nil {
		g.afterMerge(n)
	}
	if g.errAfterMerge[n] {
		return errors.New("HTTP 502: Bad Gateway")
	}
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
	return &Lander{Dir: f.dir, Remote: "origin", Git: &gitRunner{}, GH: gh, Log: io.Discard, HeadPoll: time.Millisecond, HeadWait: 5 * time.Second, MergeWait: 50 * time.Millisecond}
}

func originMain(t *testing.T, f *fixture) (commit, tree string) {
	t.Helper()
	return mustGit(t, f.origin, "rev-parse", "refs/heads/main"), mustGit(t, f.origin, "rev-parse", "refs/heads/main^{tree}")
}

func threePRs(f *fixture) []PR {
	return []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"}), f.pr(3, map[string]string{"d.txt": "d\n"})}
}

func TestLandMergesEachPullRequestOntoTheGatedTree(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := threePRs(f)
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
		if !l.Verified || l.Tree != rep.Steps[i].Tree || l.Merge != gh.merged[l.PR] || l.UpdatedBranch {
			t.Errorf("landing %d: %+v, want verified on the gated tree, merge = GitHub's merge commit, no branch update", i, l)
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
	if len(gh.comments) != 3 || len(gh.cancels) != 3 || rep.Witnessed == "" {
		t.Fatalf("comments %d, cancels %v, witness %q — want one comment and one cancel per pull request, and the witness hint", len(gh.comments), gh.cancels, rep.Witnessed)
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
	gh.headLag[2] = 3 // GitHub reports the update a few reads late
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
	if _, code, _ := (&gitRunner{}).runCode(context.Background(), f.origin, "merge-base", "--is-ancestor", prs[1].HeadSHA, branch); code != 0 {
		t.Fatal("#2's updated branch does not descend from its head")
	}
	// Both the author's head and the update are swept for runs.
	if !slices.Contains(gh.cancels, prs[1].HeadSHA) || !slices.Contains(gh.cancels, branch) {
		t.Fatalf("cancels %v, want #2's head and its update", gh.cancels)
	}
}

func TestLandRefusesBeforeTouchingAnything(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fixture, gh *fakeGitHub, rep *Report, prs []PR)
		want  string
	}{
		{"main moved", func(t *testing.T, f *fixture, gh *fakeGitHub, _ *Report, _ []PR) { gh.pushOnMain("chore: release") }, "main moved"},
		{"queue busy", func(_ *testing.T, _ *fixture, gh *fakeGitHub, _ *Report, _ []PR) { gh.queue = []int{42} }, "merge queue"},
		{"head moved", func(t *testing.T, f *fixture, _ *fakeGitHub, _ *Report, prs []PR) {
			mustGit(t, f.seed, "checkout", "--quiet", "-B", "pr-2", prs[1].HeadSHA)
			writeFiles(t, f.seed, map[string]string{"late.txt": "x\n"})
			mustGit(t, f.seed, "add", "-A")
			mustGit(t, f.seed, "commit", "--quiet", "-m", "late push")
			mustGit(t, f.seed, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/pr-2")
		}, "#2 moved"},
		{"retargeted", func(_ *testing.T, _ *fixture, gh *fakeGitHub, _ *Report, _ []PR) {
			pr := gh.prs[2]
			pr.BaseRef = "release"
			gh.prs[2] = pr
		}, "#2 targets"},
		{"turned draft", func(_ *testing.T, _ *fixture, gh *fakeGitHub, _ *Report, _ []PR) {
			pr := gh.prs[1]
			pr.Draft = true
			gh.prs[1] = pr
		}, "#1 is a draft"},
		{"closed", func(_ *testing.T, _ *fixture, gh *fakeGitHub, _ *Report, _ []PR) {
			pr := gh.prs[1]
			pr.State = "closed"
			gh.prs[1] = pr
		}, "#1 is closed"},
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
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, errPartial) {
				t.Fatalf("err %v, want a refusal containing %q, not a partial landing", err, tc.want)
			}
			after, _ := originMain(t, f)
			if len(gh.merges) != 0 || after != before || len(gh.comments) != 0 {
				t.Fatalf("merges %v, comments %d, main %s → %s: a refusal must touch nothing", gh.merges, len(gh.comments), short(before), short(after))
			}
		})
	}
}

func TestLandDisarmsAutoMergeOnTheLot(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := threePRs(f)
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.autoMerge[2] = true
	if err := lander(f, gh).Land(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.disabled) != "[2]" || fmt.Sprint(gh.merges) != "[1 2 3]" {
		t.Fatalf("disarmed %v, merges %v — want #2 disarmed before the lot lands", gh.disabled, gh.merges)
	}
}

func TestLandDisarmVerification(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := threePRs(f)
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.autoMerge[3] = true
	gh.disarmNoop = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "still armed") || len(gh.merges) != 0 {
		t.Fatalf("err %v, merges %v — want a refusal before any merge when disarming did not take", err, gh.merges)
	}
}

// gh exits 0 for a pull request already in a merge queue: the landing must
// read the pull request back, never trust the exit code.
func TestLandStopsWhenGhMergesNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := threePRs(f)
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.queued[2] = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "#2 reads not merged") || !strings.Contains(err.Error(), "sits in the merge queue") || !strings.Contains(err.Error(), "nothing of it landed") || !errors.Is(err, errPartial) {
		t.Fatalf("err %v, want #2 named as not merged, as a partial landing", err)
	}
	if len(rep.Landed) != 1 || rep.Landed[0].PR != 1 || !rep.Landed[0].Verified || fmt.Sprint(gh.merges) != "[1]" {
		t.Fatalf("landed %+v, merges %v — want only #1, verified", rep.Landed, gh.merges)
	}
}

// A release pushed right after a squash: the landed commit is still named
// exactly, and the next pull request is NOT merged onto a tree nobody gated.
func TestLandStopsBeforeMergingOnceMainMoves(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := threePRs(f)
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.afterMerge = func(n int) {
		if n == 1 {
			gh.pushOnMain("chore: release v9.9.9")
		}
	}
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "stopped before #2") || !strings.Contains(err.Error(), "chore: release v9.9.9") || !errors.Is(err, errPartial) {
		t.Fatalf("err %v, want the landing stopped before #2, naming the release", err)
	}
	if fmt.Sprint(gh.merges) != "[1]" || len(rep.Landed) != 1 || rep.Landed[0].Merge != gh.merged[1] || !rep.Landed[0].Verified {
		t.Fatalf("merges %v, landed %+v — want #1 recorded as GitHub's merge commit %s, verified, and nothing else merged", gh.merges, rep.Landed, short(gh.merged[1]))
	}
	if len(gh.comments) != 1 {
		t.Fatalf("comments %v, want #1's landing comment even though the lot stopped", gh.comments)
	}
}

// A push that slips in between the check and GitHub's squash is caught after
// the fact and named for what it is: an ungated tree on main.
func TestLandNamesAnUngatedTreeWhenSomethingSlipsIn(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.interleave = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "landed on main between") || !strings.Contains(err.Error(), "UNGATED") {
		t.Fatalf("err %v, want the foreign commit named and the tree called ungated", err)
	}
	if fmt.Sprint(gh.merges) != "[1]" {
		t.Fatalf("merges %v, want the landing stopped after #1", gh.merges)
	}
}

func TestLandStopsWhenMainDoesNotCarryTheGatedTree(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.tamper = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "carries an UNGATED tree") {
		t.Fatalf("err %v, want the tree mismatch named", err)
	}
	if fmt.Sprint(gh.merges) != "[1]" || !strings.Contains(rep.Unlanded, "#1") || rep.Landed[0].Verified {
		t.Fatalf("merges %v, unlanded %q, landed %+v — want the landing stopped right after #1, recorded unverified", gh.merges, rep.Unlanded, rep.Landed)
	}
}

// A push inside the window between the first look at main and the merge —
// here while the merge's own re-read runs — must stop the lot before the
// squash, for the first pull request as for the others.
func TestLandChecksMainRightBeforeEachMerge(t *testing.T) {
	for _, at := range []int{1, 2} {
		f := newFixture(t, map[string]string{"a.txt": "a\n"})
		prs := threePRs(f)
		rep := greenLot(t, f, prs)
		gh := newFakeGitHub(t, f, prs)
		reads := map[int]int{}
		gh.onRead = func(n int) {
			reads[n]++
			// Pre-flight reads each pull request once; the second read of
			// #at is the re-read right before its merge.
			if n == at && reads[n] == 2 {
				gh.pushOnMain("chore: release v9.9.9")
			}
		}
		err := lander(f, gh).Land(context.Background(), rep)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("stopped before #%d: main moved", at)) {
			t.Fatalf("push before #%d's merge: err %v, want the lot stopped before #%d", at, err, at)
		}
		if len(gh.merges) != at-1 {
			t.Fatalf("push before #%d's merge: merges %v — want only the %d before it", at, gh.merges, at-1)
		}
		if strings.Contains(err.Error(), "UNGATED") {
			t.Fatalf("push before #%d's merge: an ungated tree landed: %v", at, err)
		}
	}
}

// gh can report a failure after the merge went through: main is read back,
// and the landing is recorded, unverified, as a partial one.
func TestLandRecordsAMergeThatWentThroughBehindAnError(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.errAfterMerge[1] = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !errors.Is(err, errPartial) || !strings.Contains(err.Error(), "recorded as an unverified landing") {
		t.Fatalf("err %v, want a partial landing that records #1", err)
	}
	if len(rep.Landed) != 1 || rep.Landed[0].PR != 1 || rep.Landed[0].Verified || rep.Landed[0].Merge != gh.merged[1] {
		t.Fatalf("landed %+v — want #1 recorded unverified, as main's new tip %s", rep.Landed, short(gh.merged[1]))
	}
}

// GitHub refuses a merge because another push landed on main meanwhile: the
// push is not the pull request's landing — never recorded, commented or
// charged to it.
func TestLandNeverCreditsAForeignPushToARefusedMerge(t *testing.T) {
	for _, at := range []int{1, 2} {
		f := newFixture(t, map[string]string{"a.txt": "a\n"})
		prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
		rep := greenLot(t, f, prs)
		gh := newFakeGitHub(t, f, prs)
		gh.refuseWithPush[at] = true
		err := lander(f, gh).Land(context.Background(), rep)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("nothing of #%d landed", at)) || !strings.Contains(err.Error(), "chore(release): v9.9.9") {
			t.Fatalf("at=%d: err %v, want the foreign push named and nothing of #%d landed", at, err, at)
		}
		for _, l := range rep.Landed {
			if l.PR == at {
				t.Fatalf("at=%d: #%d recorded as landed: %+v", at, at, rep.Landed)
			}
		}
		if _, commented := gh.comments[at]; commented {
			t.Fatalf("at=%d: a comment was posted on the unmerged #%d", at, at)
		}
		if slices.Contains(gh.cancels, prs[at-1].HeadSHA) {
			t.Fatalf("at=%d: #%d's runs were cancelled", at, at)
		}
		if partial := errors.Is(err, errPartial); partial != (at == 2) {
			t.Fatalf("at=%d: partial=%v — want partial only once the train itself landed something", at, partial)
		}
	}
}

func TestLandWaitsForAMergeGitHubFinishesAfterATimeout(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.lateMerge[1] = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !errors.Is(err, errPartial) || !strings.Contains(err.Error(), "#1 reads merged") {
		t.Fatalf("err %v, want the late merge found and recorded", err)
	}
	if len(rep.Landed) != 1 || rep.Landed[0].Merge != gh.merged[1] {
		t.Fatalf("landed %+v, want #1 recorded as %s", rep.Landed, short(gh.merged[1]))
	}
}

// GitHub squashed, but its read-back still says not merged: main's tip IS
// the pull request's squash, and that is what is recorded.
func TestLandRecognisesItsOwnSquashOnMainWhenTheReadBackLags(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.errAfterMerge[1] = true
	gh.mergedReadLag[1] = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !errors.Is(err, errPartial) || !strings.Contains(err.Error(), "is #1's squash") {
		t.Fatalf("err %v, want main's tip recognised as #1's squash", err)
	}
	if len(rep.Landed) != 1 || rep.Landed[0].Merge != gh.merged[1] {
		t.Fatalf("landed %+v, want #1 recorded as %s", rep.Landed, short(gh.merged[1]))
	}
}

func TestLandKeepsAnUnreadableOutcomeOutOfTheLandings(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.refuseBreakRemote[1] = true
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !errors.Is(err, errPartial) || !strings.Contains(err.Error(), "UNCERTAIN") {
		t.Fatalf("err %v, want an uncertain, partial stop", err)
	}
	if len(rep.Landed) != 0 || !slices.Equal(rep.Uncertain, []int{1}) || len(gh.comments) != 0 || len(gh.cancels) != 0 {
		t.Fatalf("landed %+v, uncertain %v, comments %d, cancels %v — want #1 uncertain only, no side effect", rep.Landed, rep.Uncertain, len(gh.comments), gh.cancels)
	}
}

func TestLandRecordsTheBranchUpdateWhereverItStops(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n", "docs/z.md": "z\n"})
	prs := []PR{f.pr(1, map[string]string{"docs/b.md": "b\n"}), f.pr(2, map[string]string{"docs/c.md": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	reads := 0
	gh.onRead = func(n int) {
		if n != 2 {
			return
		}
		reads++
		// pre-flight 1, then waitHead reads after the push; push on main at
		// the merge re-read, once the branch update is done.
		if b := mustGit(t, f.origin, "rev-parse", "refs/heads/pr-2"); b != prs[1].HeadSHA && reads == 3 {
			gh.pushOnMain("chore(release): v9.9.9")
		}
	}
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "stopped before #2") || !strings.Contains(err.Error(), "its branch already carries the train's resolution") {
		t.Fatalf("err %v, want the stop to name #2's rewritten branch", err)
	}
	if len(rep.BranchUpdates) != 1 || rep.BranchUpdates[0].PR != 2 || rep.BranchUpdates[0].From != prs[1].HeadSHA {
		t.Fatalf("branch updates %+v, want #2's push recorded", rep.BranchUpdates)
	}
}

func TestLandSucceedsWhenAReleaseFollowsTheLastLanding(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.afterMerge = func(n int) {
		if n == 2 {
			gh.pushOnMain("chore: release v9.9.9")
		}
	}
	if err := lander(f, gh).Land(context.Background(), rep); err != nil {
		t.Fatalf("a release after the last landing failed the landing: %v", err)
	}
	if rep.Witnessed == "" || len(rep.Landed) != 2 {
		t.Fatalf("witness %q, landed %d — want the full landing recorded", rep.Witnessed, len(rep.Landed))
	}
}

func TestLandRecordsAMergeItCouldNotVerify(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.failReadAfterMerge[1] = true
	saves := 0
	l := lander(f, gh)
	l.Save = func(*Report) error { saves++; return nil }
	err := l.Land(context.Background(), rep)
	if err == nil || !errors.Is(err, errPartial) || !strings.Contains(err.Error(), "reading it back failed") {
		t.Fatalf("err %v, want a partial landing that says the read-back failed", err)
	}
	if len(rep.Landed) != 1 || rep.Landed[0].PR != 1 || rep.Landed[0].Verified || saves == 0 {
		t.Fatalf("landed %+v, saves %d — want #1 recorded as merged and unverified, and saved", rep.Landed, saves)
	}
}

func TestLandDryRunPushesAndMergesNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n", "docs/z.md": "z\n"})
	prs := []PR{f.pr(1, map[string]string{"docs/b.md": "b\n"}), f.pr(2, map[string]string{"docs/c.md": "c\n"})}
	rep := greenLot(t, f, prs)
	gh := newFakeGitHub(t, f, prs)
	gh.autoMerge[1] = true
	before, _ := originMain(t, f)
	l := lander(f, gh)
	l.DryRun = true
	if err := l.Land(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	after, _ := originMain(t, f)
	if len(gh.merges) != 0 || after != before || mustGit(t, f.origin, "rev-parse", "refs/heads/pr-2") != prs[1].HeadSHA || len(rep.Landed) != 0 || len(gh.disabled) != 0 || len(gh.comments) != 0 {
		t.Fatalf("a dry run changed something: merges %v, disarmed %v, comments %d, main %s → %s", gh.merges, gh.disabled, len(gh.comments), short(before), short(after))
	}
}

func TestLandReportsAMergeRefusedByGitHub(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep := greenLot(t, f, prs)
	gh := &refusingMerge{fakeGitHub: newFakeGitHub(t, f, prs)}
	err := lander(f, gh).Land(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "gh reported the merge failed") || !strings.Contains(err.Error(), "nothing of it landed") || errors.Is(err, errPartial) {
		t.Fatalf("err %v, want the refusal named, and not as a partial landing (nothing reached main)", err)
	}
}

type refusingMerge struct{ *fakeGitHub }

func (r *refusingMerge) Merge(context.Context, int, string, string, string) error {
	return errors.New("GraphQL: Pull request is not mergeable")
}

func TestLockLandingIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "land.lock")
	a, err := lockLanding(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockLanding(path); err == nil || !strings.Contains(err.Error(), "another land") {
		t.Fatalf("second lock: %v, want a refusal", err)
	}
	a.Close()
	b, err := lockLanding(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	b.Close()
}
