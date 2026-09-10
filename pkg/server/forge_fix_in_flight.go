package server

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A FIXER run in flight is invisible on the pull request it is rewriting, and
// that invisibility costs whole runs.
//
// The fixer works for tens of minutes, then pushes. If the branch moved
// underneath in the meantime, push_back rebases; when the rebase conflicts it
// banks the commits on `iterion/banked/…` and asks for a manual reconcile.
// Everything the pass integrated after the other write is lost work.
//
// Nothing warned the other writer, because the fixer holds NO required check:
// markGateInFlight claims `gate_context`, and a fixer has none — it does not
// gate the merge, it answers a review. So the only signal that a fixer is
// working is a COMMENT, and only in one case (a quota park, which posts the
// pause notice whose fixer branch already says "don't push"). A fixer that is
// simply working says nothing at all.
//
// Measured 2026-09-09 on one pull request: two fixer passes, 11 and 12 commits,
// both banked on an auto-rebase conflict, both needing a hand reconcile — while
// a second writer pushed three times without ever seeing that anything was in
// flight. Four passes of integration work, thrown away for want of a signal
// that costs one status.
//
// So the launch claims a status of its OWN. Deliberately NOT the gate context:
//
//   - a fixer must never occupy a context branch protection may require —
//     writing there could blank a reviewer's verdict back to "running", the
//     exact harm markGateInFlight is written to avoid;
//   - and this claim must never be able to block a merge. On its own context
//     it is advisory unless a repo chooses otherwise, which stays the repo's
//     call, not the engine's.
//
// It is a status rather than a comment because a comment is what already
// existed and what was already missed: a status sits in the checks list the
// forge renders next to the merge button, which is where someone about to
// write is looking.
const fixInFlightContext = "iterion/fix-in-flight"

// fixInFlightDescription is the claim's identity as well as its text: the
// predicate below matches on it, so it must stay stable and distinct from
// gateInFlightDescription — two markers that read alike would let one lane
// clear the other's claim.
const fixInFlightDescription = "a fix run is rewriting this branch — a push now collides with what it pushes back"

// fixDoneDescription replaces the claim when the run reaches a terminal state.
// It resolves rather than deletes: a status that disappears reads as "never
// claimed", which is the ambiguity this whole file exists to remove.
const fixDoneDescription = "the fix run is done — pushing is safe again"

// isFixInFlight reports whether a status is this server's own fixer claim.
// Matched on state AND description, like isGateInFlight: a bare `pending`
// belongs to whoever posted it, and clearing someone else's is worse than
// leaving ours.
func isFixInFlight(st forge.CommitStatus) bool {
	return st.State == forge.CommitStatePending &&
		strings.TrimSpace(st.Description) == fixInFlightDescription
}

// markFixInFlight claims the fixer context on the revision a freshly launched
// fixer run is about to rewrite.
//
// Best-effort and silent on everything that is not a fixer on a pull request:
// the overwhelming majority of launches are neither, and a claim nobody can
// attribute is worse than none (same reasoning as the gate claim: ownership is
// read off the target URL).
func (s *Server) markFixInFlight(ctx context.Context, teamID, sourceTenant, botID string, vars map[string]string, runID string) {
	if s == nil || s.forgeConnections == nil || vars == nil {
		return
	}
	prURL := strings.TrimSpace(vars["pr_url"])
	sha := strings.TrimSpace(vars["head_sha"])
	if prURL == "" || sha == "" {
		return
	}
	// The ROLE decides, never a bot id — the engine names no bot (CLAUDE.md),
	// and a new fixer inherits this by declaring `consumes: review`, exactly
	// as it inherits the pause notice's push warning.
	if s.handoffRoleFor(ctx, sourceTenant, botID) != pauseNoticeRoleFixer {
		return
	}
	runURL := gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), runID)
	if runURL == "" {
		// Unattributable: the terminal clear below identifies its own claim by
		// target URL, so a claim posted without one could never be resolved
		// and would sit pending forever. Better to say nothing.
		return
	}
	gc, repo, ok := s.fixStatusClientFor(ctx, teamID, prURL)
	if !ok {
		return
	}
	// Read before write, and only over NOTHING or over our own claim. A
	// verdict some other tool posted on this context is not ours to blank.
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, fixInFlightContext)
	if err != nil || !readable {
		return
	}
	if cur.State != "" && !isFixInFlight(cur) {
		return
	}
	st := forge.CommitStatus{
		State:       forge.CommitStatePending,
		Context:     fixInFlightContext,
		Description: forge.TruncateStatusDescription(fixInFlightDescription),
		TargetURL:   runURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		s.fixInFlightDebug(runID, "claim: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge fix: run %s claimed %s on %s@%s — a push while it works collides with its push-back",
			runID, fixInFlightContext, repo, shortSHA(sha))
	}
}

// clearFixInFlight resolves this server's fixer claim once the run is terminal.
//
// Idempotent and narrow: it only ever replaces OUR OWN pending marker, so a
// double fire (the outcome event and the sweep both offering the same run)
// costs one read, and a status somebody else owns is never touched.
func (s *Server) clearFixInFlight(ctx context.Context, run *store.Run) {
	if s == nil || run == nil || s.forgeConnections == nil || !run.Status.IsTerminal() {
		return
	}
	prURL := runInputString(run, "pr_url")
	sha := runInputString(run, "head_sha")
	if prURL == "" || sha == "" {
		return
	}
	gc, repo, ok := s.fixStatusClientFor(ctx, run.TenantID, prURL)
	if !ok {
		return
	}
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, fixInFlightContext)
	if err != nil || !readable || !isFixInFlight(cur) {
		return
	}
	st := forge.CommitStatus{
		State:       forge.CommitStateSuccess,
		Context:     fixInFlightContext,
		Description: forge.TruncateStatusDescription(fixDoneDescription),
		TargetURL:   cur.TargetURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		s.fixInFlightDebug(run.ID, "clear: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge fix: run %s is %s — released %s on %s@%s",
			run.ID, run.Status, fixInFlightContext, repo, shortSHA(sha))
	}
}

// fixStatusClientFor resolves the (connection, repo) a fixer claim writes
// through. Shared by the claim and the clear so the two can never disagree
// about which repository they are speaking on.
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
