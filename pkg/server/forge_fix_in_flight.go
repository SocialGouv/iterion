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
//   - and this claim must never be able to block a merge. It is advisory —
//     and, since the context names the run (below), it cannot be pinned as a
//     required check even deliberately. That is the right outcome rather than
//     a lost option: the row is `pending` for the whole length of a fix, so
//     requiring it would block every merge on the branch while the fixer
//     works, which is the opposite of what it is for.
//
// It is a status rather than a comment because a comment is what already
// existed and what was already missed: a status sits in the checks list the
// forge renders next to the merge button, which is where someone about to
// write is looking.
//
// ONE CONTEXT PER RUN, and that is load-bearing rather than cosmetic. Several
// fixers genuinely share one head sha: each `/billy` comment carries its own
// idempotency key (`comment:<id>`), the auto-fix lane launches on the
// reviewer's own head, the merge-queue auto-heal launches on the dequeued
// head, and overlap=supersede — the only thing that would cancel the older run
// — is per-bot AND off unless a webhook sets it. On ONE shared row those runs
// cannot all be represented: whichever of them holds the row resolves it when
// IT ends, posting "pushing is safe again" over a sibling that is still
// rewriting the branch. That is the false all-clear this file calls worse than
// the silence it replaces, produced by the feature itself. Standing the second
// run down instead only moves the hole: the row then goes green when the FIRST
// run ends, with the second still working.
//
// A row per run is the shape where "green ⟺ that run is done" is decidable
// from the row alone, with no cross-run lookup and no state the forge would
// have to hold for us — and the aggregate a reader actually wants, "is any
// fixer working here", is then just "is any of these rows still pending",
// which is how the checks list reads anyway. The cost is one row per fixer run
// on a given revision (in practice one, occasionally two), against a green
// check that lies.
const fixInFlightContextPrefix = "iterion/fix-in-flight"

// fixInFlightContextFor is the context ONE run claims. The run id is spelled
// out in full, never abbreviated: run ids are UUIDv7, whose leading hex digits
// are the high bits of a millisecond timestamp, so any short prefix is SHARED
// by every run launched in the same ~65s window — precisely the concurrent
// fixers this per-run context exists to keep apart.
func fixInFlightContextFor(runID string) string {
	id := strings.TrimSpace(runID)
	if id == "" {
		return ""
	}
	return fixInFlightContextPrefix + "/" + id
}

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

// isFixDone recognises this server's own RESOLVED marker, so the claim can
// write over it. Since the context names the run, the only caller that ever
// meets one is reclaimFixInFlight — that same run coming back, replayed off
// the DLQ or resumed by an operator — and it must be able to raise the warning
// again instead of reading its own `done` as a foreign verdict and standing
// down for the whole second pass.
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
	ctxName := fixInFlightContextFor(runID)
	if ctxName == "" {
		return
	}
	gc, repo, ok := s.fixStatusClientFor(ctx, teamID, prURL)
	if !ok {
		return
	}
	// Read before write, and only over NOTHING or over our own claim. A
	// verdict some other tool posted on this context is not ours to blank.
	// The context is this run's own, so in practice the read finds nothing —
	// but the discipline costs one list call the clear pays anyway, and it is
	// what keeps a foreign writer's verdict from being silently replaced.
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, ctxName)
	if err != nil || !readable {
		return
	}
	// Ours to write over: nothing, a live claim, or a RESOLVED one. That last
	// case is not cosmetic — a run whose claim was released on a park nothing
	// owned (a DLQ park, an unknown continuation) can still be resumed by an
	// operator, and the resumed pass must be able to raise the warning again
	// rather than stay invisible behind its own `done` marker.
	if cur.State != "" && !isFixInFlight(cur) && !isFixDone(cur) {
		return
	}
	// AND never take over a live claim that speaks for a DIFFERENT run. On a
	// per-run context that can only happen if something else writes here under
	// our description; the test is kept because the alternative — blanking a
	// warning that is true — is the failure this file exists to prevent, and
	// because it is what makes the invariant "a pending row names a run that is
	// still working" hold for every row, not merely by construction.
	if isFixInFlight(cur) && !gateStatusSpeaksFor(cur, runURL) {
		return
	}
	st := forge.CommitStatus{
		State:       forge.CommitStatePending,
		Context:     ctxName,
		Description: forge.TruncateStatusDescription(fixInFlightDescription),
		TargetURL:   runURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		s.fixInFlightDebug(runID, "claim: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge fix: run %s claimed %s on %s@%s — a push while it works collides with its push-back",
			runID, ctxName, repo, shortSHA(sha))
	}
}

// reclaimFixInFlight re-raises a fixer's warning when a run that was already
// released comes back to life.
//
// The stand-down in the clear keeps the claim for a park the PLATFORM will
// resume by itself. Two revivals are not that: a DLQ replay and an operator
// resume both act on a run the clear has already announced done — and both
// wake the SAME run id, which then goes on rewriting the branch and pushing
// back. Without this the row stays `success` for that entire second pass, and
// then forever: the clear at the end of it reads a status that is no longer
// in-flight and does nothing. A green check over a live rewrite is the failure
// this file exists to prevent, arrived at from the other direction.
//
// It is markFixInFlight, fed from the run doc instead of from launch vars —
// including its read-before-write, which is what lets a claim be raised over
// this run's OWN resolved marker without touching anyone else's. The
// provenance mirrors the CLEAR's rather than the launch's (the run's tenant,
// and the tier its bot came from), because those two must agree about what
// this run is: a reclaim the clear could not later resolve would strand.
func (s *Server) reclaimFixInFlight(ctx context.Context, run *store.Run) {
	if s == nil || run == nil {
		return
	}
	prURL := runInputString(run, "pr_url")
	sha := runInputString(run, "head_sha")
	if prURL == "" || sha == "" {
		// The local field read that excludes almost every run before any
		// catalog walk or forge traffic — the same first guard as the clear.
		return
	}
	sourceTenant := run.BotSourceTenant
	if strings.TrimSpace(sourceTenant) == "" {
		sourceTenant = run.TenantID
	}
	s.markFixInFlight(ctx, run.TenantID, sourceTenant, run.BotID,
		map[string]string{"pr_url": prURL, "head_sha": sha}, run.ID)
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
	// The DLQ exception is the gate lane's, and is tested FIRST exactly as
	// forge_gate_reconcile.go tests it: a DLQ park is FINAL for automation
	// whatever RetryState still says — a retry_after can survive on such a doc
	// (the usage-window park that preceded an operator resume, when the clear
	// on resume did not land) — and standing down on it would leave the claim
	// pending for a run nothing will ever wake.
	//
	// What comes AFTER it is NOT the gate lane's test, and this comment says so
	// rather than claiming a parity it does not have (the first draft did, and
	// that is worse than having neither): the gate lane keys on an armed
	// RetryAfter, this one on the continuation. The two answer different
	// questions — the gate lane decides whether to post a synthetic failure for
	// a review that will never arrive, this one whether announcing a branch free
	// would be a lie — and only the second is wrong about a run the queue will
	// simply redeliver. See fixParkWillResume.
	if run.Status == store.RunStatusFailedResumable &&
		run.FailureCode != store.FailureDLQParked &&
		fixParkWillResume(run) {
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
	// And our own run's CONTEXT. This is what makes the release safe under
	// concurrency: the row this resolves names one run, so a sibling fixer
	// still rewriting the branch keeps its own pending row and its warning
	// stands. The reviewer's own reconcile pass — terminal, on the very same
	// head sha the auto-fix lane launched the fixer from — reads a context that
	// is not the fixer's and finds nothing to resolve.
	ctxName := fixInFlightContextFor(run.ID)
	if ctxName == "" {
		return
	}
	gc, repo, ok := s.fixStatusClientFor(ctx, run.TenantID, prURL)
	if !ok {
		return
	}
	cur, readable, err := gateStatusOn(ctx, gc, repo, sha, ctxName)
	if err != nil || !readable || !isFixInFlight(cur) {
		return
	}
	// AND this run's own claim, not merely "a claim shaped like ours".
	// Redundant with the per-run context and kept anyway: ownership by target
	// URL is what the gate lane next door tests, and the two markers must not
	// diverge on the one question — whose claim is this — that decides whether
	// a green check is a lie.
	if !gateStatusSpeaksFor(cur, runURL) {
		return
	}
	st := forge.CommitStatus{
		State:       forge.CommitStateSuccess,
		Context:     ctxName,
		Description: forge.TruncateStatusDescription(fixDoneDescription),
		TargetURL:   cur.TargetURL,
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, st); err != nil {
		s.fixInFlightDebug(run.ID, "clear: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge fix: run %s is %s — released %s on %s@%s",
			run.ID, run.Status, ctxName, repo, shortSHA(sha))
	}
}

// fixParkWillResume reports whether something will pick a parked fixer back up
// on its own — the half of the stand-down above that decides whether "the fix
// run is done" would be a lie.
//
// It reads the CONTINUATION, not the failure code, because a code list is
// wrong in both directions. Too narrow: a plain execution failure parks as
// failed_resumable with no RetryState at all, is Nak'd (pkg/runner/loop.go),
// and a fresh pod picks the SAME run back up — a usage-window-only predicate
// posts the all-clear over that whole second pass, which is the drain/sandbox
// class this very branch was reported on. Too wide: an interrupted run on its
// LAST permitted delivery "Naks into nothing", so an interrupted-means-resume
// rule would leave a dead run's claim pending forever. `ContinuationState` is
// the repo's own answer to exactly that question — the runner promotes it at
// the actual Nak, and outcome_router / stuckcard / boarddispatch already gate
// on this same pair.
//
// The UNKNOWN continuation (empty — the type's doc says never treat it as
// final) does clear here, and that is a decision, not an oversight. It has to
// clear: an interrupted run on its last delivery ends unknown for good, and
// refusing to release those would leave a warning standing over a branch
// nobody is rewriting — the ambiguity this whole file exists to remove, merely
// inverted. What makes it SAFE is that no reader can catch a nak-parked run
// during the window in which it still reads unknown: the outcome event never
// fires for a nak (outcomeSideEffectsFire is false for every nak action), and
// the sweep — the only other caller — skips anything updated within
// gateSweepGrace (3 minutes), while the runner's promote is a store write with
// a 10s timeout issued at the Nak itself. The residual is a promote that FAILS
// that write (a Warn in the runner log), which is a run whose future nothing
// records at all.
//
// RetryAfter stays as the compatibility fallback: a row parked before the
// typed bookkeeping carries the armed retry and no continuation.
func fixParkWillResume(run *store.Run) bool {
	switch run.ContinuationState {
	case store.ContinuationRedeliveryPending, store.ContinuationRetryArmed:
		return true
	}
	return run.RetryState != nil && run.RetryState.RetryAfter != nil
}

// fixRoleTTL bounds how long a manifest classification is reused. Short enough
// that a re-declared bot is picked up without a restart, long enough that the
// sweeper's repeated offers of the same runs cost one walk instead of one per
// pass.
const fixRoleTTL = 5 * time.Minute

// fixRoleMemoMax bounds the memo's SIZE, because its TTL does not: an entry
// expires but is only ever overwritten by a re-lookup of the same key, so a
// key seen once and never again is held for the life of the process. The key
// space is (tenant × bot) and the server is long-lived and multi-tenant, which
// is a slow one-way climb rather than a leak with a rate — the shape that is
// invisible until someone reads a heap profile.
//
// The cap is generous on purpose: it must never evict a WORKING set (a deploy
// where every team's fixer is offered by the sweep at once), only the tail of a
// process that has run for weeks. Past it, expired entries go first and the
// map is dropped whole only if that freed nothing — a memo is a performance
// filter, so the worst a reset can cost is one re-walk per live key.
const fixRoleMemoMax = 4096

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
	if len(s.fixRoleMemo) >= fixRoleMemoMax {
		for k, e := range s.fixRoleMemo {
			if !now.Before(e.expires) {
				delete(s.fixRoleMemo, k)
			}
		}
		if len(s.fixRoleMemo) >= fixRoleMemoMax {
			clear(s.fixRoleMemo)
		}
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
