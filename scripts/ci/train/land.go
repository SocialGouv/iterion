package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Landing is one pull request the train merged, and the proof it landed the
// gated tree. It is recorded the moment GitHub accepts the merge, Verified
// false, and completed once the landed commit is read back: an interruption
// between the two leaves an honest "merged, not verified" line, never none.
type Landing struct {
	PR            int    `json:"pr"`
	MergedHead    string `json:"merged_head"`
	Merge         string `json:"merge,omitempty"`
	Tree          string `json:"tree,omitempty"`
	Verified      bool   `json:"verified"`
	UpdatedBranch bool   `json:"updated_branch,omitempty"`
	CancelledRuns int    `json:"cancelled_runs,omitempty"`
}

// errPartial marks a landing that stopped after main had changed — the
// caller's exit code says main was touched (see cmdLand).
var errPartial = errors.New("partial landing")

// Lander lands a green lot, one admin squash per pull request, in order.
type Lander struct {
	Dir    string // the train's worktree: every git gesture happens there
	Remote string
	Git    *gitRunner
	GH     GitHub
	Log    io.Writer
	DryRun bool
	// Save rewrites the report after each landing, so an interrupted landing
	// leaves an exact record of what reached main.
	Save func(*Report) error
	// HeadWait bounds the wait for GitHub to report a pushed branch update
	// as the pull request's head (0 = 60 s); HeadPoll is the polling step
	// (0 = 2 s) — also the step of MergeWait.
	HeadWait, HeadPoll time.Duration
	// MergeWait bounds the wait, after a merge error that is not a known
	// refusal, for GitHub to finish a merge it may still be running
	// (0 = 30 s).
	MergeWait time.Duration
}

func (l *Lander) logf(format string, args ...any) {
	fmt.Fprintf(l.Log, "ci:train: "+format+"\n", args...)
}

// mergeTree is what GitHub's squash of head onto onto would produce: the
// tree of their three-way merge, and whether it is clean. A conflicted merge
// still has a tree (conflict markers in the conflicted files): git prints it
// first either way.
func (l *Lander) mergeTree(ctx context.Context, onto, head string) (string, bool, error) {
	out, code, err := l.Git.runCode(ctx, l.Dir, "merge-tree", "--write-tree", "--no-messages", onto, head)
	if err != nil {
		return "", false, err
	}
	switch code {
	case 0, 1:
		return strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), code == 0, nil
	default:
		return "", false, fmt.Errorf("git merge-tree %s %s: exit %d: %s", short(onto), short(head), code, strings.TrimSpace(out))
	}
}

func (l *Lander) fetchMain(ctx context.Context) (string, error) {
	if _, err := l.Git.run(ctx, l.Dir, "fetch", "--quiet", l.Remote, "main"); err != nil {
		return "", err
	}
	return l.Git.revParse(ctx, l.Dir, "FETCH_HEAD")
}

func (l *Lander) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, code, err := l.Git.runCode(ctx, l.Dir, "merge-base", "--is-ancestor", a, b)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: exit %d", short(a), short(b), code)
	}
}

func (l *Lander) subject(ctx context.Context, rev string) string {
	out, err := l.Git.run(ctx, l.Dir, "log", "-1", "--format=%s", rev)
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(out)
}

// stop records why the landing stopped, saves the report, and returns the
// error — errPartial-wrapped once anything reached main.
func (l *Lander) stop(rep *Report, msg string) error {
	msg += l.updatedBranches(rep)
	rep.Unlanded = msg
	err := errors.New(msg)
	if len(rep.Landed) > 0 {
		err = fmt.Errorf("%w: %s", errPartial, msg)
	}
	if serr := l.saveOrLog(rep); serr != nil {
		return errors.Join(err, serr)
	}
	return err
}

// Land refuses unless the lot is green, main is still its base, the merge
// queue is empty and every pull request is still what the train gated
// (open, not a draft, on main, same head, the lot rule). Auto-merge armed on
// a pull request of the lot is disarmed first. Then, per pull request:
// main must still be the previous landing, or the landing stops BEFORE
// merging — the next squash would land on a tree nobody gated; if GitHub's
// squash of its head would not produce the gated step's tree (a derived file
// the train regenerated), its branch first gets a merge commit carrying the
// train's resolution, and the merge waits until GitHub reports that head;
// the admin squash is pinned to that head; the pull request must then read
// merged, and its merge commit must be a child of the previous landing that
// carries the gated tree. The pull request runs to cancel and the comments
// wait for the end of the lot, so the window in which another push can slip
// between two landings stays as short as the merges themselves.
func (l *Lander) Land(ctx context.Context, rep *Report) error {
	if rep.Verdict != verdictGreen {
		return fmt.Errorf("train %s is %q, not green: nothing to land", rep.ID, rep.Verdict)
	}
	if len(rep.Steps) == 0 {
		return fmt.Errorf("train %s has no pull request left to land", rep.ID)
	}
	if len(rep.Landed) > 0 || len(rep.Uncertain) > 0 {
		return fmt.Errorf("train %s already landed %d pull request(s) (%d uncertain); a partial landing is finished by a new train on the current main, after checking the uncertain ones by hand", rep.ID, len(rep.Landed), len(rep.Uncertain))
	}
	cur, err := l.fetchMain(ctx)
	if err != nil {
		return err
	}
	if cur != rep.Base {
		return fmt.Errorf("main moved since the train ran (gated on %s, main is %s): run the train again on the current main", short(rep.Base), short(cur))
	}
	queued, err := l.GH.QueueEntries(ctx)
	if err != nil {
		return err
	}
	if len(queued) > 0 {
		return fmt.Errorf("the merge queue has %d entr(ies) in flight (%s): a push to main would rebuild them — land on an empty queue (dequeue the lot's own entries first)", len(queued), joinPRs(queued))
	}
	var fresh []PR
	for _, s := range rep.Steps {
		pr, err := l.GH.PullRequest(ctx, s.PR.Number)
		if err != nil {
			return err
		}
		if pr.HeadSHA != s.PR.HeadSHA {
			return fmt.Errorf("#%d moved since the train gated it (%s → %s): run the train again", pr.Number, short(s.PR.HeadSHA), short(pr.HeadSHA))
		}
		if pr.CrossRepo != s.PR.CrossRepo {
			return fmt.Errorf("#%d changed repository since the train gated it: run the train again", pr.Number)
		}
		// The files are the run's, read from the gated commits: the head is
		// unchanged, so they are too — and GitHub's list may be truncated.
		pr.Files = s.PR.Files
		fresh = append(fresh, pr)
	}
	if err := checkLot(fresh, rep.AllowFork); err != nil {
		return fmt.Errorf("the lot no longer holds: %w", err)
	}
	// lot.json is a file: what it claims about the lot is checked against the
	// commits the train made — each step's tree is its commit's, and each
	// commit sits on the one before it.
	prev := rep.Base
	for _, s := range rep.Steps {
		tree, err := l.Git.revParse(ctx, l.Dir, s.Commit+"^{tree}")
		if err != nil {
			return fmt.Errorf("#%d: its step commit %s: %w", s.PR.Number, short(s.Commit), err)
		}
		parent, err := l.Git.revParse(ctx, l.Dir, s.Commit+"^")
		if err != nil {
			return fmt.Errorf("#%d: its step commit %s: %w", s.PR.Number, short(s.Commit), err)
		}
		if tree != s.Tree || parent != prev {
			return fmt.Errorf("lot.json does not match the train's commits at #%d (tree %s vs %s, parent %s vs %s): refusing to land it", s.PR.Number, short(tree), short(s.Tree), short(parent), short(prev))
		}
		prev = s.Commit
	}
	for _, pr := range fresh {
		if !pr.AutoMerge {
			continue
		}
		if l.DryRun {
			l.logf("dry run: would disarm #%d's auto-merge", pr.Number)
			continue
		}
		if err := l.GH.DisableAutoMerge(ctx, pr.Number); err != nil {
			return fmt.Errorf("#%d: disarming its auto-merge: %w", pr.Number, err)
		}
		// An exit code is not the state: read it back.
		again, err := l.GH.PullRequest(ctx, pr.Number)
		if err != nil {
			return err
		}
		if again.AutoMerge {
			return fmt.Errorf("#%d: its auto-merge is still armed after disarming it — armed, GitHub would enqueue it mid-landing", pr.Number)
		}
		l.logf("#%d: auto-merge disarmed — armed, GitHub would enqueue it mid-landing", pr.Number)
	}

	defer l.afterLanding(ctx, rep)
	for _, s := range rep.Steps {
		n := s.PR.Number
		if !l.DryRun {
			if err := l.mainStill(ctx, rep, n, cur); err != nil {
				return err
			}
		}
		mergeHead := s.PR.HeadSHA
		tree, clean, err := l.mergeTree(ctx, cur, mergeHead)
		if err != nil {
			return l.stop(rep, fmt.Sprintf("stopped before #%d: %v", n, err))
		}
		updated := false
		if !clean || tree != s.Tree {
			// The train pushes its own resolution to the pull request's
			// branch only where it is the train's to make: the files GitHub's
			// squash would get differently must all be derived files this
			// step regenerated. Anything else — a generator's stray write, an
			// edited lot.json — never reaches a branch.
			// --no-renames: with rename detection, a non-derived file moved
			// onto a derived path would list only the derived one.
			diff, err := l.Git.lines(ctx, l.Dir, "diff", "--name-only", "--no-renames", tree, s.Tree)
			if err != nil {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: %v", n, err))
			}
			var stray []string
			for _, p := range diff {
				if !isDerived(p) || !slices.Contains(s.Regenerated, p) {
					stray = append(stray, p)
				}
			}
			if len(diff) == 0 || len(stray) > 0 {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: the gated step differs from GitHub's squash in files the train did not regenerate (%s) — refusing to push it", n, strings.Join(stray, ", ")))
			}
			if s.PR.CrossRepo {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: GitHub's squash would not land the gated tree and its branch is on a fork the train cannot update — land it by hand", n))
			}
			msg := fmt.Sprintf("Merge main into %s for ci:train %s\n\nThe train squashed #%d onto main and the pull requests landed before it,\nand regenerated %s on that combination. This merge carries the\nresolution, so GitHub's squash lands the tree the train gated (%s).",
				s.PR.HeadRef, rep.ID, n, strings.Join(s.Regenerated, ", "), short(s.Tree))
			out, err := l.Git.run(ctx, l.Dir, "commit-tree", s.Tree, "-p", s.PR.HeadSHA, "-p", cur, "-m", msg)
			if err != nil {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: %v", n, err))
			}
			mergeHead = strings.TrimSpace(out)
			tree, clean, err = l.mergeTree(ctx, cur, mergeHead)
			if err != nil {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: %v", n, err))
			}
			if !clean || tree != s.Tree {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: even with the train's resolution, GitHub's squash would not land the gated tree (%s, clean=%v)", n, short(tree), clean))
			}
			updated = true
			l.logf("#%d: branch %s gets the train's resolution as %s", n, s.PR.HeadRef, short(mergeHead))
			if !l.DryRun {
				if _, err := l.Git.run(ctx, l.Dir, "push", "--quiet", "--force-with-lease=refs/heads/"+s.PR.HeadRef+":"+s.PR.HeadSHA, l.Remote, mergeHead+":refs/heads/"+s.PR.HeadRef); err != nil {
					return l.stop(rep, fmt.Sprintf("stopped before #%d: pushing the train's resolution: %v", n, err))
				}
				rep.BranchUpdates = append(rep.BranchUpdates, BranchUpdate{PR: n, From: s.PR.HeadSHA, To: mergeHead})
				if err := l.saveOrLog(rep); err != nil {
					return l.stop(rep, fmt.Sprintf("stopped before #%d: saving the report: %v", n, err))
				}
				if err := l.waitHead(ctx, n, mergeHead); err != nil {
					return l.stop(rep, fmt.Sprintf("stopped before #%d: its branch now carries the train's resolution %s (a fast-forward of its head), but %v", n, short(mergeHead), err))
				}
			}
		}
		if !l.DryRun {
			// Re-read right before the merge: a base retargeted, a head
			// pushed or an auto-merge re-armed since the pre-flight.
			now, err := l.GH.PullRequest(ctx, n)
			if err != nil {
				return l.stop(rep, fmt.Sprintf("stopped before #%d: %v", n, err))
			}
			switch {
			case now.State != "open" || now.Draft:
				return l.stop(rep, fmt.Sprintf("stopped before #%d: it is now %s (draft %v)", n, now.State, now.Draft))
			case now.BaseRef != "main":
				return l.stop(rep, fmt.Sprintf("stopped before #%d: it now targets %q, not main", n, now.BaseRef))
			case now.HeadSHA != mergeHead:
				return l.stop(rep, fmt.Sprintf("stopped before #%d: its head is now %s, not %s", n, short(now.HeadSHA), short(mergeHead)))
			case now.AutoMerge:
				return l.stop(rep, fmt.Sprintf("stopped before #%d: its auto-merge was re-armed", n))
			}
		}
		msgs, err := l.GH.CommitMessages(ctx, n)
		if err != nil {
			return l.stop(rep, fmt.Sprintf("stopped before #%d: %v", n, err))
		}
		subject := fmt.Sprintf("%s (#%d)", s.PR.Title, n)
		body := squashBody(msgs, rep)
		if l.DryRun {
			l.logf("dry run: would merge #%d at %s as %q", n, short(mergeHead), subject)
			fake, err := l.Git.run(ctx, l.Dir, "commit-tree", s.Tree, "-p", cur, "-m", subject)
			if err != nil {
				return err
			}
			cur = strings.TrimSpace(fake)
			continue
		}
		// The guard that matters is this one, the last gesture before the
		// merge: the resolution push, the wait for GitHub to report it and
		// the re-read above all take time, and a push to main inside that
		// window would have GitHub squash onto a tree nobody gated.
		if err := l.mainStill(ctx, rep, n, cur); err != nil {
			return err
		}
		mergeErr := l.GH.Merge(ctx, n, mergeHead, subject, body)
		// gh's exit code is not the outcome — it exits 0 for a pull request
		// already in a merge queue, and GitHub can finish a merge after
		// answering with an error. The one authority is GitHub's own merged
		// state, read back, waited for when gh's answer was not a merge.
		pr, merged, err := l.awaitMerged(ctx, n)
		if !merged {
			return l.unmerged(ctx, rep, n, cur, mergeErr, pr, err)
		}
		rep.Landed = append(rep.Landed, Landing{PR: n, MergedHead: mergeHead, UpdatedBranch: updated})
		land := &rep.Landed[len(rep.Landed)-1]
		if err := l.saveOrLog(rep); err != nil {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: saving the report: %v", n, err))
		}
		land.Merge = pr.MergeCommit
		tip, err := l.fetchMain(ctx)
		if err != nil {
			return l.stop(rep, fmt.Sprintf("stopped after #%d (merged as %s): reading main: %v", n, short(pr.MergeCommit), err))
		}
		if ok, err := l.isAncestor(ctx, pr.MergeCommit, tip); err != nil || !ok {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: its merge commit %s is not on main %s (%v)", n, short(pr.MergeCommit), short(tip), err))
		}
		parent, err := l.Git.revParse(ctx, l.Dir, pr.MergeCommit+"^")
		if err != nil {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: %v", n, err))
		}
		landedTree, err := l.Git.revParse(ctx, l.Dir, pr.MergeCommit+"^{tree}")
		if err != nil {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: %v", n, err))
		}
		land.Tree = landedTree
		if parent != cur {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: %s (%q) landed on main between the last landing %s and #%d's merge %s — main now carries an UNGATED tree %s (gated: %s); witness main before anything else lands", n, short(parent), l.subject(ctx, parent), short(cur), n, short(pr.MergeCommit), short(landedTree), short(s.Tree)))
		}
		if landedTree != s.Tree {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: its merge %s carries an UNGATED tree %s, not the gated %s — witness main before anything else lands", n, short(pr.MergeCommit), short(landedTree), short(s.Tree)))
		}
		land.Verified = true
		l.logf("#%d landed as %s (tree %s = gated)", n, short(pr.MergeCommit), short(landedTree))
		if err := l.saveOrLog(rep); err != nil {
			return l.stop(rep, fmt.Sprintf("stopped after #%d: saving the report: %v", n, err))
		}
		cur = pr.MergeCommit
	}
	if l.DryRun {
		l.logf("dry run: every pull request would land on the gated tree; nothing was pushed")
		return nil
	}
	// The last landing must be on main; main may already be ahead of it (a
	// release, another landing): ancestry, not equality.
	out, err := l.Git.run(ctx, l.Dir, "ls-remote", l.Remote, "refs/heads/main")
	if err != nil {
		return l.stop(rep, fmt.Sprintf("landed %d pull request(s), but ls-remote failed: %v", len(rep.Landed), err))
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return l.stop(rep, "landed every pull request, but ls-remote shows no main")
	}
	if f[0] != cur {
		if _, err := l.Git.run(ctx, l.Dir, "fetch", "--quiet", l.Remote, "main"); err != nil {
			return l.stop(rep, fmt.Sprintf("landed every pull request, but fetching main failed: %v", err))
		}
		if ok, err := l.isAncestor(ctx, cur, f[0]); err != nil || !ok {
			return l.stop(rep, fmt.Sprintf("ls-remote: main is %s, which does not contain the last landing %s (%v)", short(f[0]), short(cur), err))
		}
	}
	rep.Witnessed = fmt.Sprintf("the witness is the first 🧪 Tests run that completes on a main tip containing %s (git merge-base --is-ancestor %s <tip>); the heavy layer runs on the nightly", cur, cur)
	l.logf("landed %d pull request(s); last landing %s — %s", len(rep.Landed), short(cur), rep.Witnessed)
	return l.saveOrLog(rep)
}

// mainStill stops the landing unless main is still cur, the last landing
// (the lot's base before the first one).
func (l *Lander) mainStill(ctx context.Context, rep *Report, n int, cur string) error {
	tip, err := l.fetchMain(ctx)
	if err != nil {
		return l.stop(rep, fmt.Sprintf("stopped before #%d: reading main: %v", n, err))
	}
	if tip != cur {
		return l.stop(rep, fmt.Sprintf("stopped before #%d: main moved to %s (%q) after %s — merging now would land a tree nobody gated; the rest needs a new train on the current main", n, short(tip), l.subject(ctx, tip), short(cur)))
	}
	return nil
}

// awaitMerged reads n back from GitHub. Merged at once is the ordinary
// path. Otherwise it polls for MergeWait, whatever gh answered, because
// GitHub can complete a merge after answering. No error text is read to decide anything: a PR's
// own title and commits travel in gh's arguments, and a message cannot tell
// a refusal from a merge still running.
func (l *Lander) awaitMerged(ctx context.Context, n int) (PR, bool, error) {
	wait := l.MergeWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	poll := l.HeadPoll
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	var last PR
	var lastErr error
	for {
		pr, err := l.GH.PullRequest(ctx, n)
		if err == nil && pr.Merged {
			return pr, true, nil
		}
		last, lastErr = pr, err
		if time.Now().After(deadline) {
			return last, false, lastErr
		}
		select {
		case <-ctx.Done():
			return last, false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// unmerged ends a landing whose pull request GitHub does not report merged
// after the wait. Nothing is inferred from main's shape or from an error
// message:
//   - main is still cur: a merge moves main, so nothing of n landed;
//   - main moved, or cannot be read: n is UNCERTAIN — kept out of Landed (no
//     comment, no cancelled runs), named, and the exit is partial (3), since
//     main did change or may have.
func (l *Lander) unmerged(ctx context.Context, rep *Report, n int, cur string, mergeErr error, pr PR, readErr error) error {
	why := "gh exited 0"
	if mergeErr != nil {
		why = "gh failed: " + firstLine(mergeErr.Error())
	}
	state := "it does not read merged"
	if readErr != nil {
		state = fmt.Sprintf("reading it back failed (%v)", readErr)
	} else if pr.AutoMerge {
		state += " and its auto-merge is armed"
	}
	if queued, qerr := l.GH.QueueEntries(ctx); qerr == nil && slices.Contains(queued, n) {
		state += " — it sits in the merge queue, where it may still land on the queue's terms: dequeue it"
	}
	tip, ferr := l.fetchMain(ctx)
	if ferr == nil && tip == cur {
		return l.stop(rep, fmt.Sprintf("stopped at #%d: %s, %s after the wait; main is still %s — nothing of it landed", n, why, state, short(cur)))
	}
	rep.Uncertain = append(rep.Uncertain, n)
	moved := fmt.Sprintf("main could not be read back (%v)", ferr)
	if ferr == nil {
		moved = fmt.Sprintf("main moved to %s (%q)", short(tip), l.subject(ctx, tip))
	}
	msg := fmt.Sprintf("stopped at #%d: %s, %s after the wait, and %s — #%d is UNCERTAIN: check #%d and main by hand", n, why, state, moved, n, n)
	rep.Unlanded = msg + l.updatedBranches(rep)
	if serr := l.saveOrLog(rep); serr != nil {
		return errors.Join(fmt.Errorf("%w: %s", errPartial, rep.Unlanded), serr)
	}
	return fmt.Errorf("%w: %s", errPartial, rep.Unlanded)
}

// updatedBranches names, for a stop message, every branch the train
// rewrote with its resolution and did not land: whoever picks up the
// stopped lot must know those branches moved.
func (l *Lander) updatedBranches(rep *Report) string {
	var notes []string
	for _, u := range rep.BranchUpdates {
		landed := false
		for _, ld := range rep.Landed {
			if ld.PR == u.PR {
				landed = true
				break
			}
		}
		if !landed {
			notes = append(notes, fmt.Sprintf("#%d %s→%s", u.PR, short(u.From), short(u.To)))
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return " (branches already carrying the train's resolution: " + strings.Join(notes, ", ") + ")"
}

// afterLanding cancels the pull request runs still in flight for each landed
// head and comments on each landed pull request — after the merges, whatever
// stopped them, so they never widen the window between two landings.
func (l *Lander) afterLanding(ctx context.Context, rep *Report) {
	if l.DryRun {
		return
	}
	for i := range rep.Landed {
		land := &rep.Landed[i]
		heads := []string{land.MergedHead}
		for _, s := range rep.Steps {
			if s.PR.Number == land.PR && s.PR.HeadSHA != land.MergedHead {
				heads = append(heads, s.PR.HeadSHA)
			}
		}
		for _, sha := range heads {
			c, err := l.GH.CancelPullRequestRuns(ctx, sha)
			if err != nil {
				l.logf("#%d: cancelling its pull request runs: %v", land.PR, err)
			}
			land.CancelledRuns += c
		}
		body := fmt.Sprintf("Landed by `ci:train` %s as %s — the train's local gate ran on the lot's tree (base %s). Evidence: the train's directory `%s` on the operator's machine.", rep.ID, land.Merge, short(rep.Base), rep.ID)
		if !land.Verified {
			body = fmt.Sprintf("Merged by `ci:train` %s, but the landing stopped before it was verified: %s", rep.ID, rep.Unlanded)
		}
		if err := l.GH.Comment(ctx, land.PR, body); err != nil {
			l.logf("#%d: comment: %v", land.PR, err)
		}
	}
	_ = l.saveOrLog(rep)
}

// waitHead waits until GitHub reports head as n's head: a merge pinned to a
// head GitHub has not registered yet is refused.
func (l *Lander) waitHead(ctx context.Context, n int, head string) error {
	wait, poll := l.HeadWait, l.HeadPoll
	if wait == 0 {
		wait = 60 * time.Second
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		pr, err := l.GH.PullRequest(ctx, n)
		if err != nil {
			return err
		}
		if pr.HeadSHA == head {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("GitHub still reports %s as its head after %s", short(pr.HeadSHA), wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (l *Lander) saveOrLog(rep *Report) error {
	if l.Save == nil {
		return nil
	}
	if err := l.Save(rep); err != nil {
		l.logf("saving the report: %v", err)
		return err
	}
	return nil
}

// lockLanding takes an exclusive, non-blocking lock on path for the life of
// the process: two `land` runs of one train would merge, comment and write
// lot.json over each other. The kernel releases it if the process dies.
func lockLanding(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another land of this train holds %s: %w", path, err)
	}
	return f, nil
}

// squashBody renders the landed commit's body the way GitHub renders a
// squash — one bullet per commit — and ends it with the train's trailer and
// the co-authors, deduplicated, as the last paragraph.
func squashBody(msgs []string, rep *Report) string {
	var b strings.Builder
	var coauthors []string
	for _, m := range msgs {
		m = strings.TrimSpace(m)
		b.WriteString("* " + m + "\n\n")
		for _, line := range strings.Split(m, "\n") {
			if strings.HasPrefix(strings.ToLower(line), "co-authored-by:") {
				line = strings.TrimSpace(line)
				if !slices.ContainsFunc(coauthors, func(c string) bool { return strings.EqualFold(c, line) }) {
					coauthors = append(coauthors, line)
				}
			}
		}
	}
	fmt.Fprintf(&b, "Landing-Train: %s (base %s, %d pull request(s), evidence in the train directory %s)", rep.ID, short(rep.Base), len(rep.Steps), rep.ID)
	for _, c := range coauthors {
		b.WriteString("\n" + c)
	}
	return b.String()
}

func joinPRs(ns []int) string {
	var s []string
	for _, n := range ns {
		s = append(s, fmt.Sprintf("#%d", n))
	}
	return strings.Join(s, ", ")
}
