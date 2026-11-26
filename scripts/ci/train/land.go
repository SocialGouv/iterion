package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Landing is one pull request the train merged, and the proof it landed the
// gated tree.
type Landing struct {
	PR            int    `json:"pr"`
	Merge         string `json:"merge"`
	Tree          string `json:"tree"`
	MergedHead    string `json:"merged_head"`
	UpdatedBranch bool   `json:"updated_branch,omitempty"`
	CancelledRuns int    `json:"cancelled_runs,omitempty"`
}

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
}

func (l *Lander) logf(format string, args ...any) {
	fmt.Fprintf(l.Log, "ci:train: "+format+"\n", args...)
}

// mergeTree is what GitHub's squash of head onto onto would produce: the
// tree of their three-way merge, and whether it is clean.
func (l *Lander) mergeTree(ctx context.Context, onto, head string) (string, bool, error) {
	out, code, err := l.Git.runCode(ctx, l.Dir, "merge-tree", "--write-tree", "--no-messages", onto, head)
	if err != nil {
		return "", false, err
	}
	switch code {
	case 0:
		return strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), true, nil
	case 1:
		return "", false, nil
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

// Land refuses unless the lot is green, main is still its base, the merge
// queue is empty and every head is the one the train gated. Then, per pull
// request: if GitHub's squash of its head onto the current main would not
// produce the gated step's tree (a derived file the train regenerated), its
// branch first gets a merge commit carrying the train's resolution; the
// admin squash follows, pinned to that head; and main's new tip must be a
// child of the previous one whose tree is the gated tree. Any mismatch stops
// the landing at once, with what already landed recorded.
func (l *Lander) Land(ctx context.Context, rep *Report) error {
	if rep.Verdict != verdictGreen {
		return fmt.Errorf("train %s is %q, not green: nothing to land", rep.ID, rep.Verdict)
	}
	if len(rep.Steps) == 0 {
		return fmt.Errorf("train %s has no pull request left to land", rep.ID)
	}
	if len(rep.Landed) > 0 {
		return fmt.Errorf("train %s already landed %d pull request(s); a partial landing is finished by a new train on the current main", rep.ID, len(rep.Landed))
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
	for _, s := range rep.Steps {
		pr, err := l.GH.PullRequest(ctx, s.PR.Number)
		if err != nil {
			return err
		}
		if pr.State != "open" {
			return fmt.Errorf("#%d is %s now", pr.Number, pr.State)
		}
		if pr.HeadSHA != s.PR.HeadSHA {
			return fmt.Errorf("#%d moved since the train gated it (%s → %s): run the train again", pr.Number, short(s.PR.HeadSHA), short(pr.HeadSHA))
		}
	}

	for k, s := range rep.Steps {
		n := s.PR.Number
		mergeHead := s.PR.HeadSHA
		tree, clean, err := l.mergeTree(ctx, cur, mergeHead)
		if err != nil {
			return err
		}
		updated := false
		if !clean || tree != s.Tree {
			if s.PR.CrossRepo {
				return fmt.Errorf("#%d: GitHub's squash would not land the gated tree and its branch is on a fork the train cannot update — land it by hand", n)
			}
			msg := fmt.Sprintf("Merge main into %s for ci:train %s\n\nThe train squashed #%d onto main and the pull requests landed before it,\nand regenerated %s on that combination. This merge carries the\nresolution, so GitHub's squash lands the tree the train gated (%s).",
				s.PR.HeadRef, rep.ID, n, strings.Join(s.Regenerated, ", "), short(s.Tree))
			out, err := l.Git.run(ctx, l.Dir, "commit-tree", s.Tree, "-p", s.PR.HeadSHA, "-p", cur, "-m", msg)
			if err != nil {
				return err
			}
			mergeHead = strings.TrimSpace(out)
			tree, clean, err = l.mergeTree(ctx, cur, mergeHead)
			if err != nil {
				return err
			}
			if !clean || tree != s.Tree {
				return fmt.Errorf("#%d: even with the train's resolution, GitHub's squash would not land the gated tree (%s, clean=%v): stopping", n, short(tree), clean)
			}
			updated = true
			l.logf("#%d: branch %s gets the train's resolution as %s", n, s.PR.HeadRef, short(mergeHead))
			if !l.DryRun {
				if _, err := l.Git.run(ctx, l.Dir, "push", "--quiet", "--force-with-lease=refs/heads/"+s.PR.HeadRef+":"+s.PR.HeadSHA, l.Remote, mergeHead+":refs/heads/"+s.PR.HeadRef); err != nil {
					return err
				}
			}
		}
		msgs, err := l.GH.CommitMessages(ctx, n)
		if err != nil {
			return err
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
		if err := l.GH.Merge(ctx, n, mergeHead, subject, body); err != nil {
			return fmt.Errorf("#%d: %w (landed so far: %d of %d)", n, err, k, len(rep.Steps))
		}
		next, err := l.fetchMain(ctx)
		if err != nil {
			return err
		}
		parent, err := l.Git.revParse(ctx, l.Dir, next+"^")
		if err != nil {
			return err
		}
		nextTree, err := l.Git.revParse(ctx, l.Dir, next+"^{tree}")
		if err != nil {
			return err
		}
		land := Landing{PR: n, Merge: next, Tree: nextTree, MergedHead: mergeHead, UpdatedBranch: updated}
		rep.Landed = append(rep.Landed, land)
		if parent != cur {
			rep.Unlanded = fmt.Sprintf("stopped after #%d: main's new tip %s is not a child of %s — something else landed in between", n, short(next), short(cur))
			_ = l.saveOrLog(rep)
			return fmt.Errorf("%s", rep.Unlanded)
		}
		if nextTree != s.Tree {
			rep.Unlanded = fmt.Sprintf("stopped after #%d: main's tree %s is not the gated tree %s — main now carries an ungated tree; witness it before anything else lands", n, short(nextTree), short(s.Tree))
			_ = l.saveOrLog(rep)
			return fmt.Errorf("%s", rep.Unlanded)
		}
		cancelled := 0
		for _, sha := range uniq(mergeHead, s.PR.HeadSHA) {
			c, err := l.GH.CancelPullRequestRuns(ctx, sha)
			if err != nil {
				l.logf("#%d: cancelling its pull request runs: %v", n, err)
			}
			cancelled += c
		}
		rep.Landed[len(rep.Landed)-1].CancelledRuns = cancelled
		if err := l.GH.Comment(ctx, n, fmt.Sprintf("Landed by `ci:train` %s as %s — the train's local gate ran on the lot's tree %s (base %s). Evidence: `.iterion/ci-train/%s/` on the operator's machine.", rep.ID, next, short(s.Tree), short(rep.Base), rep.ID)); err != nil {
			l.logf("#%d: comment: %v", n, err)
		}
		l.logf("#%d landed as %s (tree %s = gated)%s", n, short(next), short(nextTree), cancelledNote(cancelled))
		if err := l.saveOrLog(rep); err != nil {
			return err
		}
		cur = next
	}
	if l.DryRun {
		l.logf("dry run: every pull request would land on the gated tree; nothing was pushed")
		return nil
	}
	out, err := l.Git.run(ctx, l.Dir, "ls-remote", l.Remote, "refs/heads/main")
	if err != nil {
		return err
	}
	if f := strings.Fields(out); len(f) == 0 || f[0] != cur {
		return fmt.Errorf("ls-remote: main is %q, not the last landing %s", strings.TrimSpace(out), short(cur))
	}
	rep.Witnessed = fmt.Sprintf("the witness is the first 🧪 Tests run that completes on a main tip containing %s (git merge-base --is-ancestor %s <tip>); the heavy layer runs on the nightly", cur, cur)
	l.logf("landed %d pull request(s); main is %s — %s", len(rep.Landed), short(cur), rep.Witnessed)
	return l.saveOrLog(rep)
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
	fmt.Fprintf(&b, "Landing-Train: %s (base %s, %d pull request(s), evidence .iterion/ci-train/%s/)", rep.ID, short(rep.Base), len(rep.Steps), rep.ID)
	for _, c := range coauthors {
		b.WriteString("\n" + c)
	}
	return b.String()
}

func cancelledNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d pull request run(s) cancelled", n)
}

func uniq(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

func joinPRs(ns []int) string {
	var s []string
	for _, n := range ns {
		s = append(s, fmt.Sprintf("#%d", n))
	}
	return strings.Join(s, ", ")
}
