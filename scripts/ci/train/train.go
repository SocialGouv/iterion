package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// PR is the part of a pull request the train reads.
type PR struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	BaseRef   string   `json:"base_ref"`
	HeadRef   string   `json:"head_ref"`
	HeadSHA   string   `json:"head_sha"`
	CrossRepo bool     `json:"cross_repo"`
	Author    string   `json:"author"`
	Files     []string `json:"files"`
}

// Step is one pull request squashed onto the lot: Commit is the lot's head
// after it, Tree that commit's tree. Regenerated names the derived files the
// train rewrote in this step — a conflict it resolved by regeneration, or a
// file the combination left stale. A step with a regenerated file is one
// GitHub's own squash of the pull request would not reproduce, so landing it
// first updates the pull request's branch with the train's resolution.
type Step struct {
	PR          PR       `json:"pr"`
	Commit      string   `json:"commit"`
	Tree        string   `json:"tree"`
	Regenerated []string `json:"regenerated,omitempty"`
}

// Ejection is a pull request the train took out of the lot, and why.
type Ejection struct {
	PR       int      `json:"pr"`
	Reason   string   `json:"reason"`
	Members  []string `json:"members,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
}

// Round is one full gate of the lot and what the train did with its result.
type Round struct {
	N        int      `json:"n"`
	Head     string   `json:"head"`
	Code     int      `json:"code"`
	Red      []string `json:"red,omitempty"`
	Rerun    []string `json:"rerun_red,omitempty"`
	BaseRed  []string `json:"base_red,omitempty"`
	Culprit  int      `json:"culprit,omitempty"`
	Evidence string   `json:"evidence"`
}

// Verdicts of a run.
const (
	verdictGreen      = "green"
	verdictRed        = "red"
	verdictRefused    = "refused"
	verdictIncomplete = "incomplete"
	verdictEmpty      = "empty"
)

// Report is the train's evidence: written to lot.json, read back by land.
type Report struct {
	ID        string     `json:"id"`
	Created   time.Time  `json:"created"`
	Base      string     `json:"base"`
	BaseTree  string     `json:"base_tree"`
	Steps     []Step     `json:"steps"`
	Ejected   []Ejection `json:"ejected,omitempty"`
	Rounds    []Round    `json:"rounds"`
	Verdict   string     `json:"verdict"`
	Flakes    []string   `json:"flakes,omitempty"`
	BaseRed   []string   `json:"base_red_accepted,omitempty"`
	Worktree  string     `json:"worktree"`
	Evidence  string     `json:"evidence"`
	Landed    []Landing  `json:"landed,omitempty"`
	Unlanded  string     `json:"unlanded,omitempty"`
	Witnessed string     `json:"witness_hint,omitempty"`
}

// Head is the lot's head commit, or the base when every pull request left.
func (r *Report) Head() string {
	if len(r.Steps) == 0 {
		return r.Base
	}
	return r.Steps[len(r.Steps)-1].Commit
}

// soloPrefixes are the paths a pull request touching them must land alone:
// the local gate cannot prove a change to the CI itself, and its witness on
// main has to be attributable to that one change.
var soloPrefixes = []string{".github/", "scripts/ci/"}

// soloExempt are files under soloPrefixes that ride a lot. The quarantine
// list only takes tests out of the CI legs — the local gate runs them all —
// and internal/ciguard and scripts/ci/testquarantine validate the committed
// list inside `task test`, so the gate proves the one thing that could break.
var soloExempt = []string{"scripts/ci/test-quarantine.tsv"}

// soloPaths returns the files of pr that make it land alone.
func soloPaths(pr PR) []string {
	var out []string
	for _, f := range pr.Files {
		if slices.Contains(soloExempt, f) {
			continue
		}
		for _, p := range soloPrefixes {
			if strings.HasPrefix(f, p) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// checkLot refuses a lot the train must not run: a pull request that is not
// open, is a draft, does not target main, or — in a lot of more than one —
// touches the CI.
func checkLot(prs []PR) error {
	if len(prs) == 0 {
		return errors.New("no pull request given")
	}
	seen := map[int]bool{}
	var errs []error
	for _, pr := range prs {
		if seen[pr.Number] {
			errs = append(errs, fmt.Errorf("#%d is listed twice", pr.Number))
		}
		seen[pr.Number] = true
		switch {
		case pr.State != "open":
			errs = append(errs, fmt.Errorf("#%d is %s, not open", pr.Number, pr.State))
		case pr.Draft:
			errs = append(errs, fmt.Errorf("#%d is a draft", pr.Number))
		case pr.BaseRef != "main":
			errs = append(errs, fmt.Errorf("#%d targets %q, not main", pr.Number, pr.BaseRef))
		}
		if len(prs) > 1 {
			if s := soloPaths(pr); len(s) > 0 {
				errs = append(errs, fmt.Errorf("#%d touches the CI (%s): it lands in a lot of its own", pr.Number, strings.Join(s, ", ")))
			}
		}
	}
	return errors.Join(errs...)
}

// Train runs one lot in a worktree it owns.
type Train struct {
	ID            string
	Dir           string // the train's worktree, under Root
	Root          string // the directory the train owns: <repo>/.iterion/ci-train/<id>
	Git           *gitRunner
	Gate          Gate
	Regen         Regenerator
	Log           io.Writer
	MaxEject      int
	AcceptBaseRed []string
	Setup         func(ctx context.Context, dir string) error
}

func (t *Train) logf(format string, args ...any) {
	fmt.Fprintf(t.Log, "ci:train: "+format+"\n", args...)
}

// assertOwnTree is the guard before every gesture that discards state in
// Dir (reset --hard, clean): Dir must be absolute, resolved, strictly under
// Root, and the top level of its own git worktree. Anything else is a fatal
// error, never a warning — a train that cleaned the operator's checkout would
// destroy work that is not its own.
func (t *Train) assertOwnTree(ctx context.Context) error {
	if !filepath.IsAbs(t.Dir) || !filepath.IsAbs(t.Root) {
		return fmt.Errorf("refusing to clean: %q or %q is not absolute", t.Dir, t.Root)
	}
	dir, err := filepath.EvalSymlinks(t.Dir)
	if err != nil {
		return fmt.Errorf("refusing to clean: %w", err)
	}
	root, err := filepath.EvalSymlinks(t.Root)
	if err != nil {
		return fmt.Errorf("refusing to clean: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("refusing to clean %s: not strictly under the train's directory %s", dir, root)
	}
	top, err := t.Git.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("refusing to clean %s: %w", dir, err)
	}
	if top = strings.TrimSpace(top); top != dir {
		return fmt.Errorf("refusing to clean %s: its git top level is %s", dir, top)
	}
	return nil
}

// checkout moves the worktree to rev, discarding what a gate member left
// behind (vendor-check rewrites go.mod and vendor/). Ignored files stay:
// node_modules and build caches are the train's to keep.
func (t *Train) checkout(ctx context.Context, rev string) error {
	if err := t.assertOwnTree(ctx); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"reset", "--hard", "--quiet"},
		{"clean", "-fd", "--quiet"},
		{"checkout", "--detach", "--force", "--quiet", rev},
	} {
		if _, err := t.Git.run(ctx, t.Dir, args...); err != nil {
			return err
		}
	}
	return nil
}

// compose squashes prs, in order, onto from. A pull request whose squash
// conflicts on anything but a derived file, or that adds nothing, is ejected
// with the reason; the others become steps.
func (t *Train) compose(ctx context.Context, from string, prs []PR) ([]Step, []Ejection, error) {
	if err := t.checkout(ctx, from); err != nil {
		return nil, nil, err
	}
	var steps []Step
	var ejected []Ejection
	for _, pr := range prs {
		step, ej, err := t.squash(ctx, pr)
		if err != nil {
			return nil, nil, fmt.Errorf("#%d: %w", pr.Number, err)
		}
		if ej != nil {
			t.logf("#%d ejected: %s", pr.Number, ej.Reason)
			ejected = append(ejected, *ej)
			continue
		}
		t.logf("#%d squashed as %s%s", pr.Number, short(step.Commit), regenNote(step.Regenerated))
		steps = append(steps, *step)
	}
	return steps, ejected, nil
}

func regenNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return " (regenerated " + strings.Join(paths, ", ") + ")"
}

func (t *Train) squash(ctx context.Context, pr PR) (*Step, *Ejection, error) {
	out, code, err := t.Git.runCode(ctx, t.Dir, "merge", "--squash", "--no-commit", pr.HeadSHA)
	if err != nil {
		return nil, nil, err
	}
	var resolved []string
	if code != 0 {
		unmerged, err := t.Git.lines(ctx, t.Dir, "diff", "--name-only", "--diff-filter=U")
		if err != nil {
			return nil, nil, err
		}
		if len(unmerged) == 0 {
			return nil, nil, fmt.Errorf("merge --squash exited %d with no unmerged path: %s", code, strings.TrimSpace(out))
		}
		var foreign []string
		for _, p := range unmerged {
			if !isDerived(p) {
				foreign = append(foreign, p)
			}
		}
		if len(foreign) > 0 {
			if err := t.checkout(ctx, "HEAD"); err != nil {
				return nil, nil, err
			}
			return nil, &Ejection{PR: pr.Number, Reason: "conflicts with main or an earlier pull request of the lot in " + strings.Join(foreign, ", ")}, nil
		}
		// A derived file is never resolved by picking a side: the lot's
		// version is checked out, then its generator rewrites it from the
		// sources both sides now agree on.
		if _, err := t.Git.run(ctx, t.Dir, append([]string{"checkout", "--ours", "--"}, unmerged...)...); err != nil {
			return nil, nil, err
		}
		if _, err := t.Git.run(ctx, t.Dir, append([]string{"add", "--"}, unmerged...)...); err != nil {
			return nil, nil, err
		}
		resolved = unmerged
	}
	regenerated, err := t.regenerate(ctx)
	if err != nil {
		return nil, nil, err
	}
	staged, err := t.Git.lines(ctx, t.Dir, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, nil, err
	}
	if len(staged) == 0 {
		if err := t.checkout(ctx, "HEAD"); err != nil {
			return nil, nil, err
		}
		return nil, &Ejection{PR: pr.Number, Reason: "adds nothing to the lot: its changes are already on main"}, nil
	}
	msg := fmt.Sprintf("%s (#%d)\n\nci:train %s: squash of #%d at %s.", pr.Title, pr.Number, t.ID, pr.Number, pr.HeadSHA)
	if _, err := t.Git.run(ctx, t.Dir, "commit", "--quiet", "-m", msg); err != nil {
		return nil, nil, err
	}
	commit, err := t.Git.revParse(ctx, t.Dir, "HEAD")
	if err != nil {
		return nil, nil, err
	}
	tree, err := t.Git.revParse(ctx, t.Dir, "HEAD^{tree}")
	if err != nil {
		return nil, nil, err
	}
	all := append(slices.Clone(resolved), regenerated...)
	slices.Sort(all)
	return &Step{PR: pr, Commit: commit, Tree: tree, Regenerated: slices.Compact(all)}, nil, nil
}

// regenerate runs the derived-file generators on the index's state and
// stages what they rewrite. A generator touching anything but a derived file
// is an error: the train never commits a change no pull request made.
func (t *Train) regenerate(ctx context.Context) ([]string, error) {
	if t.Regen == nil {
		return nil, nil
	}
	if err := t.Regen.Regenerate(ctx, t.Dir); err != nil {
		return nil, err
	}
	changed, err := t.Git.lines(ctx, t.Dir, "diff", "--name-only")
	if err != nil {
		return nil, err
	}
	untracked, err := t.Git.lines(ctx, t.Dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	changed = append(changed, untracked...)
	var foreign []string
	for _, p := range changed {
		if !isDerived(p) {
			foreign = append(foreign, p)
		}
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf("the derived-file generators changed files that are not derived: %s", strings.Join(foreign, ", "))
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if _, err := t.Git.run(ctx, t.Dir, append([]string{"add", "--"}, changed...)...); err != nil {
		return nil, err
	}
	return changed, nil
}

// Run composes the lot on base, gates it, and bisects a red lot until what
// is left is green, the train gives up, or the gate cannot judge.
func (t *Train) Run(ctx context.Context, base string, prs []PR) (*Report, error) {
	baseTree, err := t.Git.revParse(ctx, t.Dir, base+"^{tree}")
	if err != nil {
		return nil, err
	}
	rep := &Report{ID: t.ID, Created: time.Now().UTC(), Base: base, BaseTree: baseTree, Worktree: t.Dir, Evidence: t.Root}

	// Base first: a base whose derived files are already stale would hide
	// main's own red inside the first pull request's step.
	if err := t.checkout(ctx, base); err != nil {
		return nil, err
	}
	if stale, err := t.regenerate(ctx); err != nil {
		return nil, err
	} else if len(stale) > 0 {
		return nil, fmt.Errorf("main's derived files are stale before any pull request (%s): fix main first", strings.Join(stale, ", "))
	}
	if t.Setup != nil {
		if err := t.Setup(ctx, t.Dir); err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
	}

	steps, ejected, err := t.compose(ctx, base, prs)
	if err != nil {
		return nil, err
	}
	rep.Steps, rep.Ejected = steps, ejected

	knownBaseRed := map[string]bool{}
	accepted := map[string]bool{}
	for _, m := range t.AcceptBaseRed {
		accepted[m] = true
	}
	for round := 1; ; round++ {
		if len(rep.Steps) == 0 {
			rep.Verdict = verdictEmpty
			return rep, nil
		}
		head := rep.Head()
		if err := t.checkout(ctx, head); err != nil {
			return nil, err
		}
		ev := filepath.Join(t.Root, fmt.Sprintf("round-%d", round))
		t.logf("round %d: full gate on %s (%d pull requests)", round, short(head), len(rep.Steps))
		res, err := t.Gate.Full(ctx, t.Dir, ev)
		if err != nil {
			return nil, err
		}
		r := Round{N: round, Head: head, Code: res.Code, Red: res.Red, Evidence: ev}
		switch res.Code {
		case 0:
			rep.Rounds = append(rep.Rounds, r)
			rep.Verdict = verdictGreen
			return rep, nil
		case 2:
			rep.Rounds = append(rep.Rounds, r)
			rep.Verdict = verdictRefused
			return rep, fmt.Errorf("the gate refused to judge (exit 2, refused: %s) — no verdict, see %s", strings.Join(res.Refused, ", "), ev)
		case 3:
			rep.Rounds = append(rep.Rounds, r)
			rep.Verdict = verdictIncomplete
			return rep, fmt.Errorf("the gate is incomplete (exit 3): green members, a required context uncovered — see %s", ev)
		case 1:
		default:
			rep.Rounds = append(rep.Rounds, r)
			return rep, fmt.Errorf("the gate exited %d, which it does not define — see %s", res.Code, ev)
		}
		if len(res.Red) == 0 {
			rep.Rounds = append(rep.Rounds, r)
			return rep, fmt.Errorf("the gate exited 1 with no red member in its matrix — see %s", ev)
		}

		// Rerun the red members once on the same head: green again is a
		// flake, recorded and not held against any pull request.
		rerun, err := t.Gate.Probe(ctx, t.Dir, filepath.Join(ev, "rerun"), res.Red)
		if err != nil {
			return nil, err
		}
		r.Rerun = rerun
		for _, m := range res.Red {
			if !slices.Contains(rerun, m) {
				rep.Flakes = appendUnique(rep.Flakes, m)
				t.logf("round %d: %s red then green on the rerun — a flake, recorded", round, m)
			}
		}
		if len(rerun) == 0 {
			rep.Rounds = append(rep.Rounds, r)
			rep.Verdict = verdictGreen
			return rep, nil
		}

		// Members red on the base are main's, not the lot's.
		var unknown []string
		for _, m := range rerun {
			if !knownBaseRed[m] {
				unknown = append(unknown, m)
			}
		}
		if len(unknown) > 0 {
			if err := t.checkout(ctx, rep.Base); err != nil {
				return nil, err
			}
			baseRed, err := t.Gate.Probe(ctx, t.Dir, filepath.Join(ev, "base"), unknown)
			if err != nil {
				return nil, err
			}
			for _, m := range baseRed {
				knownBaseRed[m] = true
			}
		}
		var newRed, onBase []string
		for _, m := range rerun {
			if knownBaseRed[m] {
				onBase = append(onBase, m)
			} else {
				newRed = append(newRed, m)
			}
		}
		r.BaseRed = onBase
		if len(newRed) == 0 {
			rep.Rounds = append(rep.Rounds, r)
			var refused []string
			for _, m := range onBase {
				if !accepted[m] {
					refused = append(refused, m)
				}
			}
			if len(refused) > 0 {
				rep.Verdict = verdictRed
				return rep, fmt.Errorf("every red member is red on main too (%s): main's red, not the lot's — fix main, or pass -accept-base-red %s to land over it", strings.Join(onBase, ", "), strings.Join(refused, ","))
			}
			rep.BaseRed = onBase
			rep.Verdict = verdictGreen
			t.logf("round %d: red only on members main is red on too, accepted: %s", round, strings.Join(onBase, ", "))
			return rep, nil
		}

		// Bisect the prefixes on the members the lot turned red.
		k, culpritRed, err := bisect(ctx, len(rep.Steps), newRed, func(ctx context.Context, k int, members []string) ([]string, error) {
			rev := rep.Base
			if k > 0 {
				rev = rep.Steps[k-1].Commit
			}
			if err := t.checkout(ctx, rev); err != nil {
				return nil, err
			}
			t.logf("round %d: probe prefix %d (%s) on %s", round, k, short(rev), strings.Join(members, ", "))
			return t.Gate.Probe(ctx, t.Dir, filepath.Join(ev, fmt.Sprintf("prefix-%d", k)), members)
		})
		if err != nil {
			return nil, err
		}
		culprit := rep.Steps[k-1]
		r.Culprit = culprit.PR.Number
		rep.Rounds = append(rep.Rounds, r)
		rep.Ejected = append(rep.Ejected, Ejection{
			PR:       culprit.PR.Number,
			Reason:   fmt.Sprintf("turns %s red: green with the %d pull request(s) before it, red with it", strings.Join(culpritRed, ", "), k-1),
			Members:  culpritRed,
			Evidence: filepath.Join(ev, fmt.Sprintf("prefix-%d", k)),
		})
		t.logf("round %d: #%d ejected — first prefix on which %s goes red", round, culprit.PR.Number, strings.Join(culpritRed, ", "))
		if t.MaxEject > 0 && countBisected(rep.Ejected) > t.MaxEject {
			rep.Verdict = verdictRed
			return rep, fmt.Errorf("%d pull requests ejected by bisection, above -max-eject %d: something systemic, stopping", countBisected(rep.Ejected), t.MaxEject)
		}

		// Recompose what followed the culprit on the prefix before it, then
		// gate the remaining lot in full again: nothing lands on a probe.
		from := rep.Base
		if k > 1 {
			from = rep.Steps[k-2].Commit
		}
		var rest []PR
		for _, s := range rep.Steps[k:] {
			rest = append(rest, s.PR)
		}
		restSteps, restEjected, err := t.compose(ctx, from, rest)
		if err != nil {
			return nil, err
		}
		rep.Steps = append(slices.Clone(rep.Steps[:k-1]), restSteps...)
		rep.Ejected = append(rep.Ejected, restEjected...)
	}
}

func countBisected(ej []Ejection) int {
	n := 0
	for _, e := range ej {
		if len(e.Members) > 0 {
			n++
		}
	}
	return n
}

func appendUnique(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func writeReport(path string, rep *Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &rep, nil
}
