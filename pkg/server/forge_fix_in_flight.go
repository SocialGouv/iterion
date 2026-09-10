package server

import (
	"context"
	"strings"
	"time"

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

// isFixDone recognises this server's own RESOLVED marker. A later fixer on the
// same head must be able to write over it — it says no fixer is working, and a
// launch is about to make that false.
func isFixDone(st forge.CommitStatus) bool {
	return st.State == forge.CommitStateSuccess &&
		strings.TrimSpace(st.Description) == fixDoneDescription
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
	// Ours to write over: nothing, a live claim, or a RESOLVED one. That last
	// case is not cosmetic — after a first pass terminates the clear leaves
	// `done` on this sha, and a second `/billy` on an UNCHANGED head (a first
	// pass that banked instead of pushing: the 2026-09-09 incident itself) read
	// its predecessor's marker as a foreign verdict and posted nothing. The
	// second pass was then as invisible as before this change, in the very lane
	// it was written for. `done` means no fixer is working — which the launch
	// below is about to make false, whoever posted it.
	if cur.State != "" && !isFixInFlight(cur) && !isFixDone(cur) {
		return
	}
	// AND never take over ANOTHER run's live claim — the same ownership test
	// the clear applies, on the site it was first forgotten. Several runs share
	// one head sha, so a second fixer launching on a head a first already
	// claimed would replace the target URL with its own; from then on the claim
	// is the newcomer's, and whichever run ends FIRST posts the all-clear over
	// the other's live rewrite. Standing down instead leaves the warning up and
	// attributed to the run that raised it, which is what a reader about to
	// push needs — the warning is true whichever fixer is working.
	if isFixInFlight(cur) && !gateStatusSpeaksFor(cur, runURL) {
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

// clearFixInFlight resolves THIS RUN's fixer claim once the run is terminal.
//
// Idempotent and narrow: it only ever replaces the marker this very run
// posted, so a double fire (the outcome event and the sweep both offering the
// same run) costs one read, and neither another tool's status nor another
// RUN's claim is ever touched.
func (s *Server) clearFixInFlight(ctx context.Context, run *store.Run) {
	if s == nil || run == nil || s.forgeConnections == nil || !run.Status.IsTerminal() {
		return
	}
	// AN ARMED RETRY IS NOT AN ENDING. IsTerminal() includes failed_resumable,
	// and that is exactly where a fixer lands when it parks on a usage window
	// or a sandbox timeout — with a durable retry the sweeper will resume. The
	// run then goes on rewriting the branch and pushing back, and nothing
	// re-claims on the way: markFixInFlight is reachable only from the webhook
	// launch, while the resume runs through runview's Resume. Clearing here
	// would leave "pushing is safe again" standing for the whole second pass —
	// the false all-clear this file calls worse than the silence it replaces,
	// in the very lane (a quota park) the change was written for.
	//
	// The gate lane's predicate, INCLUDING its DLQ exception — the first draft
	// of this comment claimed parity it did not have, which is worse than
	// having neither. A DLQ park is FINAL for automation whatever RetryState
	// still says: a retry_after can survive on such a doc (the usage-window
	// park that preceded an operator resume, when the clear on resume did not
	// land), and standing down on it would leave the claim pending for a run
	// nothing will ever wake. So the exception is tested FIRST, exactly as
	// forge_gate_reconcile.go does, and only a park something will actually
	// resume keeps the claim.
	// AND AN ARMED RetryAfter IS ONLY ONE OF THE WAYS A RUN COMES BACK. The
	// runner turns ErrRunInterrupted (a rolling deploy draining a run
	// mid-flight), sandbox.ErrPhaseTimeout and sandbox.ErrCapacity into
	// failed_resumable plus a JetStream nak: a fresh pod picks the SAME run up
	// with no RetryAfter ever written — the only site that arms one is the
	// usage-window park. Keying on RetryAfter therefore announced "pushing is
	// safe again" on every DRAINED fixer, which is the commonest interruption
	// there is.
	//
	// So the test is inverted: failed_resumable KEEPS the claim, and only the
	// DLQ park releases it — a park whose deliveries the queue has exhausted
	// and that no automation will ever wake. The conservative direction is the
	// safe one: a claim left standing is advisory noise on a context nothing
	// gates, while a false all-clear is the harm this file exists to prevent.
	if run.Status == store.RunStatusFailedResumable &&
		run.FailureCode != store.FailureDLQParked {
		return
	}
	prURL := runInputString(run, "pr_url")
	sha := runInputString(run, "head_sha")
	if prURL == "" || sha == "" {
		return
	}
	// ROLE BEFORE THE NETWORK. `pr_url` + `head_sha` are set on every
	// forge-launched run — reviewer, brancher, implementer, docs-amender — so
	// without this the clear would pay a live ListCommitStatuses round trip for
	// each of them, on every sweep pass, for the whole lookback: a path that
	// used to exit on a local field read with zero forge traffic. Only a fixer
	// can ever have claimed, so only a fixer has anything to resolve.
	// Same provenance the CLAIM used, or as close as the run doc allows: the
	// claim resolves under the launching team, and a run records the tier its
	// bot came from. An empty BotSourceTenant (not recorded) falls back to the
	// run's own tenant rather than to none, so a team-forked fixer resolves on
	// both sides instead of claiming under one lookup and being unresolvable
	// under a stricter one.
	sourceTenant := run.BotSourceTenant
	if strings.TrimSpace(sourceTenant) == "" {
		sourceTenant = run.TenantID
	}
	if s.fixerRoleCached(ctx, sourceTenant, run.BotID) != pauseNoticeRoleFixer {
		return
	}
	// Our own run's URL, resolved before the read: without it there is nothing
	// to test ownership against and the claim must be left alone.
	runURL := gateRunURL(strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"), run.ID)
	if runURL == "" {
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
	// AND this run's own claim, not merely "a claim shaped like ours". Several
	// runs share ONE head sha by construction: the auto-fix lane launches the
	// fixer on the reviewer's own head_sha, and consecutive fixer passes reuse
	// it too. The sweeper re-offers every terminal run for the full lookback,
	// so without this test the reviewer's own reconcile pass — terminal, same
	// sha — reads the live fixer's claim, matches the description, and posts
	// "done" while the branch is still being rewritten. A false all-clear is
	// worse than the silence this replaces, and it would defeat the feature in
	// the exact lane it was written for. Ownership is the target URL, the same
	// test the gate lane uses.
	if !gateStatusSpeaksFor(cur, runURL) {
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

// fixRoleTTL bounds how long a manifest classification is reused. Short enough
// that a re-declared bot is picked up without a restart, long enough that the
// sweeper's repeated offers of the same runs cost one walk instead of one per
// pass.
const fixRoleTTL = 5 * time.Minute

type fixRoleEntry struct {
	role    pauseNoticeRole
	expires time.Time
}

// fixerRoleCached is handoffRoleFor with a memo, because the CLEAR calls it on
// a hot path the claim does not share: reconcileGateForRunID runs it for every
// terminal forge run the sweeper offers, every 60s, for a 60-minute lookback.
// Uncached, a single non-fixer run costs two Mongo reads (bot row, then the
// tenant's rows) plus a filesystem catalog discovery and a DSL parse of every
// bot — per offer. The role is a manifest fact that changes on deploy, not per
// run, so it is exactly the shape a memo is for.
//
// The memo is a PERFORMANCE filter only. Correctness never rests on it: the
// claim is bound to its run by target URL, and a stale role can at worst delay
// a claim's release by one TTL, never resolve one that is still live.
func (s *Server) fixerRoleCached(ctx context.Context, sourceTenant, botID string) pauseNoticeRole {
	key := strings.TrimSpace(sourceTenant) + "|" + strings.TrimSpace(botID)
	now := time.Now()
	s.fixRoleMu.Lock()
	if e, ok := s.fixRoleMemo[key]; ok && now.Before(e.expires) {
		s.fixRoleMu.Unlock()
		return e.role
	}
	s.fixRoleMu.Unlock()

	role := s.handoffRoleFor(ctx, sourceTenant, botID)

	s.fixRoleMu.Lock()
	if s.fixRoleMemo == nil {
		s.fixRoleMemo = map[string]fixRoleEntry{}
	}
	s.fixRoleMemo[key] = fixRoleEntry{role: role, expires: now.Add(fixRoleTTL)}
	s.fixRoleMu.Unlock()
	return role
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
