package server

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A required check has three states an operator can act on — green, red, and
// "running". A review only ever posted the first two, at the very END of a run
// that takes minutes. Between the push and the verdict the context is simply
// ABSENT, which the forge renders as "Expected — waiting for status to be
// reported": the same rendering as a review that was never launched, a bot
// that crashed on boot, and a webhook that never fired.
//
// The reconciler next door already names this exact ambiguity ("an absent
// required check is indistinguishable from one still running") and closes it
// from one side only, once the run is dead. This closes it from the other: the
// launch itself claims the context, so the absence of a status once again
// means what it says — nothing is reviewing this revision.
//
// The marker is deliberately a real `pending`, not a comment: it carries the
// run URL, so the operator lands on the live console instead of guessing which
// run owes the check.
const gateInFlightDescription = "review in progress — the verdict will replace this"

// isGateInFlight reports whether a status is this server's own in-flight
// marker rather than a verdict a bot posted.
//
// Every consumer that asks "does this head already have an answer?" MUST route
// through here. The marker is a `pending` with a non-empty description, so a
// guard written as `state != "" → someone already answered` reads it as a
// verdict and stands down — which would make posting the marker actively
// harmful: it would silence the very reconciler that exists to un-stick the
// PR. The predicate keeps "claimed" and "answered" distinct.
func isGateInFlight(st forge.CommitStatus) bool {
	return st.State == forge.CommitStatePending &&
		strings.TrimSpace(st.Description) == gateInFlightDescription
}

// markGateInFlight claims the repo's gate context on the revision a freshly
// launched run is about to review.
//
// Best-effort and deliberately silent about launches that owe no gate: the
// vast majority carry no gate_context at all. It only ever writes over NOTHING
// or over a previous in-flight marker — never over a verdict. That guard is
// what makes it safe under a gate context SHARED by several bots (the repo
// pins one context precisely so a required check can span them): a second bot
// launching on a head another bot already judged must not blank that judgment
// back to "running".
// The run URL is built from cfg.PublicURL rather than the request, so a claim
// and the reconciler's later failure name the run identically — the target URL
// is what tells "already answered" from "must escalate" over there.
func (s *Server) markGateInFlight(ctx context.Context, teamID, botID string, vars map[string]string, runID string) {
	if s == nil || s.forgeConnections == nil || vars == nil {
		return
	}
	gateCtx := strings.TrimSpace(vars["gate_context"])
	prURL := strings.TrimSpace(vars["pr_url"])
	if gateCtx == "" || prURL == "" {
		return
	}
	// The revision, not the PR. A status is posted on a sha, and the head can
	// move between this launch and the verdict; claiming the head the run was
	// handed keeps the marker and the verdict on the same commit.
	sha := strings.TrimSpace(vars["head_sha"])
	if sha == "" {
		return
	}
	// An UNATTRIBUTABLE claim must never exist. Ownership of a status is read
	// off its target URL (gateStatusSpeaksFor), so a claim posted without one
	// cannot be told from another run's — and the reconciler would have to
	// choose between painting "review died" over a live review and leaving a
	// PR stuck on a pending nothing resolves. Neither is acceptable, so with
	// no PublicURL configured the launch simply does not claim: the check
	// behaves exactly as it did before this feature existed.
	runURL := gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), runID)
	if runURL == "" {
		if s.logger != nil {
			s.logger.Warn("forge gate: not claiming %s for run %s — PublicURL is unset, so the claim could not name the run and nothing could later tell it from another run's",
				gateCtx, runID)
		}
		return
	}
	host, repo, _, err := forge.ParsePullURL(prURL)
	if err != nil {
		return
	}
	conn, ok := s.forgeConnectionForPR(ctx, teamID, "", host, repo)
	if !ok {
		return
	}
	gc, err := s.gateClientFor(ctx, conn)
	if err != nil || gc == nil {
		if s.logger != nil && err != nil {
			s.logger.Warn("forge gate: cannot claim %s on %s@%s for run %s: %v — the check stays absent while the review runs",
				gateCtx, repo, shortSHA(sha), runID, err)
		}
		return
	}
	// Read before write: an unreadable status list means we cannot tell a
	// verdict from an absence, and blanking a real verdict is worse than
	// leaving the in-flight window unlabelled.
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, gateCtx)
	if err != nil || !readable {
		return
	}
	if cur.State != "" && !isGateInFlight(cur) && !isSyntheticGateInterruption(cur.Description) {
		return
	}
	st := forge.CommitStatus{
		State:       forge.CommitStatePending,
		Context:     gateCtx,
		Description: gateInFlightDescription,
		TargetURL:   runURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		if s.logger != nil {
			s.logger.Warn("forge gate: run %s could not claim %s on %s@%s: %v — the check stays absent while the review runs",
				runID, gateCtx, repo, shortSHA(sha), err)
		}
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s claimed %s=pending on %s@%s (bot %s)", runID, gateCtx, repo, shortSHA(sha), botID)
	}
}
