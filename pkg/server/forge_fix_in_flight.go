package server

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A FIXER run holds no required check, so while it rewrites a branch NOTHING on
// the pull request says it is there.
//
// markGateInFlight claims `gate_context`, and a fixer has none — it answers a
// review rather than gating the merge. The only signal that ever existed is a
// comment, in one case only: a quota park, whose pause notice already tells the
// reader not to push. A fixer that is simply working says nothing at all.
//
// Measured 2026-09-09 on one pull request: two fixer passes, 11 and 12 commits,
// both banked on an auto-rebase conflict and needing a hand reconcile, while a
// second writer pushed three times without ever seeing that anything was in
// flight. Four passes of integration work, thrown away for want of a signal.
//
// # What this states, and what it deliberately does NOT
//
// The claim is a fact about the PAST: *a fix run took this revision as its
// base*. It is never retracted, and that is the whole design.
//
// An earlier version also RESOLVED the claim once the run reached a terminal
// state, so the check would read "pushing is safe again". That is a statement
// about the PRESENT — "no fixer is working right now" — and the engine cannot
// know it:
//
//   - several runs share one head sha BY CONSTRUCTION (the auto-fix lane
//     launches the fixer on the reviewer's own head_sha; consecutive passes
//     reuse it), so "this run ended" never means "no run is working";
//   - `failed_resumable` conflates a park that comes BACK (a usage window, and
//     the runner's nak paths for a drained pod, a sandbox phase timeout, a
//     capacity refusal — none of which arm a RetryAfter) with one that is
//     simply dead;
//   - and a status is a single slot per (sha, context): it cannot represent two
//     concurrent workers at all.
//
// Eight review findings on this branch were one defect wearing different
// clothes — the resolved marker announcing "done" while a fixer was still
// rewriting. Each fix moved the error rather than removing it, because the
// assertion itself was unwarranted. So it is gone.
//
// Nothing is lost by that. A fixer that SUCCEEDS pushes, which moves the head —
// and a status lives on a sha, so the claim stops being rendered next to the
// merge button exactly when the danger ends. A fixer that does NOT push leaves
// the claim standing, which is correct: its work is banked, a reconcile is
// owed, and a pusher would collide with it.
//
// # Why a status, and never the gate context
//
//   - a fixer must never occupy a context branch protection may require —
//     writing there could blank a reviewer's verdict back to "running", the
//     exact harm markGateInFlight is written to avoid;
//   - and this must never be able to block a merge. That promise is kept by the
//     STATE, not by the context: GitLab attaches a posted status to the head
//     sha's pipeline, so a `pending` there makes the pipeline non-successful and
//     an MR with only_allow_merge_if_pipeline_succeeds unmergeable until the
//     head moves — which is precisely what a fixer that BANKS never does. The
//     marker is therefore posted `success`, carrying its warning in the
//     description, on both forges and with no provider branch.
//
// A status rather than a comment because a comment is what already existed and
// what was already missed: a status sits in the checks list the forge renders
// next to the merge button, where someone about to write is looking.
const fixInFlightContext = "iterion/fix-in-flight"

// fixInFlightDescription is the claim's identity as well as its text: the
// predicate below matches on it, so it must stay stable and distinct from
// gateInFlightDescription — two markers that read alike would let one lane
// overwrite the other's.
//
// Worded as the fact it is. An earlier draft said "is rewriting this branch", a
// present-tense claim the marker cannot keep once the run ends; this one stays
// true forever, which is what lets it never need retracting.
const fixInFlightDescription = "a fix run took this revision — pushing on it collides with what it pushes back"

// isFixInFlight reports whether a status is this server's own fixer claim.
// Matched on state AND description, like isGateInFlight: a bare `pending`
// belongs to whoever posted it, and overwriting someone else's is worse than
// leaving ours.
func isFixInFlight(st forge.CommitStatus) bool {
	return st.State == forge.CommitStateSuccess &&
		strings.TrimSpace(st.Description) == fixInFlightDescription
}

// markFixInFlight claims the fixer context on the revision a freshly launched
// fixer run is about to rewrite.
//
// Best-effort and silent on everything that is not a fixer on a pull request:
// the overwhelming majority of launches are neither.
func (s *Server) markFixInFlight(ctx context.Context, teamID, sourceTenant, botID string, vars map[string]string, runID string) {
	if s == nil || s.forgeConnections == nil || vars == nil {
		return
	}
	prURL := strings.TrimSpace(vars["pr_url"])
	// `head_sha` where the lane already publishes it, else the fixer-only key.
	// Two names because the first also arms the GATE claim, and one lane must
	// reach this marker without reaching that one.
	sha := strings.TrimSpace(vars["head_sha"])
	if sha == "" {
		sha = strings.TrimSpace(vars["fix_head_sha"])
	}
	if prURL == "" || sha == "" {
		return
	}
	// THE WARNING IS ABOUT A PUSH-BACK, so it is posted only where one exists,
	// and `push_branch` is the var that creates it: it routes the fixer's
	// commits onto THIS pull request's head branch. Tested positively, on
	// purpose.
	//
	// A negative guard — stand down when `open_mr` is "true" — reads the same
	// on the two lanes anyone thinks of, and is wrong on the rest, because
	// `open_mr` is OPTIONAL. It is stamped by fixerPRVars (the heal and
	// gate-autofix lanes) and by stampBranchImprovePushBack, which returns
	// without stamping anything unless the bot IS the brancher role. So a
	// team's SECOND fixer — the case this design advertises as free, since any
	// bot inherits this marker by declaring `consumes: review` — arrives from a
	// `/command` invocation with a head sha and NEITHER var, and absence read
	// as "pushes back".
	//
	// The counterpart of stating a fact that is never withdrawn is that it must
	// be true when posted, and an optional var cannot carry that: what makes
	// the warning true is what has to be tested. As-PR mode is then covered by
	// the same line rather than by a clause of its own — it sets `mr_base` and
	// no `push_branch`, because it opens a separate pull request instead of
	// rewriting this branch.
	if strings.TrimSpace(vars["push_branch"]) == "" {
		return
	}
	// The ROLE decides, never a bot id — the engine names no bot (CLAUDE.md),
	// and a new fixer inherits this by declaring `consumes: review`, exactly as
	// it inherits the pause notice's push warning. Read at LAUNCH only, which
	// is rare: no sweep path pays for it.
	if s.handoffRoleFor(ctx, sourceTenant, botID) != pauseNoticeRoleFixer {
		return
	}
	gc, repo, ok := s.fixStatusClientFor(ctx, teamID, prURL)
	if !ok {
		return
	}
	// Read before write, and only over NOTHING or over a claim of our own kind:
	// a verdict some other tool posted on this context is not ours to blank.
	//
	// There is deliberately NO ownership test on our own kind. Refreshing a
	// claim with a newer run's URL cannot make it false — both runs took this
	// revision, both warnings are true, and the marker asserts nothing about
	// either still working. That is exactly what the removed resolve got wrong.
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, fixInFlightContext)
	if err != nil || !readable {
		return
	}
	if cur.State != "" && !isFixInFlight(cur) {
		return
	}
	// The target URL names the MOST RECENT claimant, so a reader lands on a
	// live console instead of guessing. Unlike the gate claim, nothing here has
	// to tell one run's marker from another's, so an unattributable claim (no
	// PublicURL configured) is still worth posting.
	st := forge.CommitStatus{
		// SUCCESS, not pending, and the description is the payload.
		//
		// `pending` was wrong twice. On GitLab a status posted through
		// POST /projects/:id/statuses/:sha becomes a GenericCommitStatus on the
		// sha's pipeline (creating an external one if none exists), so a pending
		// makes the head pipeline non-successful — and with
		// only_allow_merge_if_pipeline_succeeds the MR is unmergeable until the
		// head MOVES. That fires in this incident's own shape: a fixer that
		// BANKS leaves the head unmoved, so the claim would block the merge
		// forever. The promise two paragraphs up — that this never blocks a
		// merge — held for GitHub and Forgejo only.
		//
		// And on every forge `pending` also says "a check is running and will
		// report", which is false here: nothing will ever report, by design.
		// `success` says "this check has nothing blocking to say", which is
		// exactly right — the WARNING travels in the description, where both
		// forges render it beside the context name.
		State:       forge.CommitStateSuccess,
		Context:     fixInFlightContext,
		Description: forge.TruncateStatusDescription(fixInFlightDescription),
		TargetURL:   gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), runID),
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		s.fixInFlightDebug(runID, "claim: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge fix: run %s claimed %s on %s@%s — a push on this revision collides with its push-back",
			runID, fixInFlightContext, repo, shortSHA(sha))
	}
}

// fixStatusClientFor resolves the (connection, repo) a fixer claim writes
// through.
func (s *Server) fixStatusClientFor(ctx context.Context, teamID, prURL string) (forgeGateClient, string, bool) {
	host, repo, _, err := forge.ParsePullURL(prURL)
	if err != nil {
		return nil, "", false
	}
	conn, ok := s.forgeConnectionForPR(ctx, teamID, "", host, repo)
	if !ok {
		return nil, "", false
	}
	gc, err := s.gateClientFor(ctx, conn)
	if err != nil || gc == nil {
		return nil, "", false
	}
	return gc, repo, true
}

// fixInFlightDebug keeps every miss at Debug: a forge that cannot take the
// status changes nothing about the run, and this signal must never turn into
// an error a caller has to handle.
func (s *Server) fixInFlightDebug(runID, format string, args ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Debug("forge fix: run "+runID+" "+format, args...)
}
