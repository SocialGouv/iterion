package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pipelineAdmissionInterval is the backstop cadence of the ready-ticket
// launch loop. It is not latency-critical (a couple of seconds to pick up a
// freshly-readied ticket is fine), so it stays lazy.
const pipelineAdmissionInterval = 2 * time.Second

// pipelineAdmissionEnabled reports whether the studio should run its
// built-in launch loop for ready pipeline tickets: a local native board +
// a run service. The studio always wires an *idle* dispatcher Manager for
// the /dispatcher dashboard, so we do NOT gate on Dispatcher==nil; instead
// admitReadyPipelines backs off while that dispatcher is actively running
// (only then would two launchers race the same board).
func (s *Server) pipelineAdmissionEnabled() bool {
	return s.cfg.Mode != "cloud" &&
		s.cfg.NativeTrackerStore != nil &&
		s.runs != nil
}

// dispatcherActivelyLaunching reports whether an external/SPA-started
// dispatcher is currently polling the board — in which case the admission
// loop must stand down so the two don't double-launch the same ticket.
func (s *Server) dispatcherActivelyLaunching() bool {
	return s.cfg.Dispatcher != nil &&
		s.cfg.Dispatcher.Status().State == dispatcher.ManagerStateRunning
}

// runPipelineAdmissionLoop is the studio's minimal, pipeline-cap-scoped
// dispatcher: it launches tickets the operator marked ready (staged into
// Ready) whenever a concurrency slot is free. Without it, "ready" tickets
// would sit forever unless the operator ran a full `iterion dispatch`.
// Stops when the server shuts down.
func (s *Server) runPipelineAdmissionLoop() {
	s.admitReadyPipelines() // drain any boot-time backlog immediately
	t := time.NewTicker(pipelineAdmissionInterval)
	defer t.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-t.C:
			s.admitReadyPipelines()
		}
	}
}

// admitReadyPipelines launches ready tickets (StateReady, a bot, hard
// blockers all done, no active run) oldest-first while concurrency slots
// remain free. The concurrency gate in runview.Service.Launch is the
// authority — this loop only decides WHICH ready ticket to submit next and
// stops offering more once the cap is reached. Hard-dep admission shares
// native.CanLaunch with the dispatcher adapter so a ticket with open
// blockers can never launch from /pipelines while being skipped by
// iterion dispatch.
func (s *Server) admitReadyPipelines() {
	board := s.cfg.NativeTrackerStore
	if board == nil {
		return
	}
	// Stand down while an operator-started dispatcher owns the board.
	if s.dispatcherActivelyLaunching() {
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	if runs == nil {
		return
	}

	issues, err := board.List(native.ListFilter{})
	if err != nil {
		s.logger.Warn("pipeline admission: list tickets: %v", err)
		return
	}
	// File tickets whose run finished while nobody was watching BEFORE
	// computing the ready set. SetState(done) cascades the waiting_deps
	// promotion of satisfied dependents — an auto_ready dependent unblocked
	// here becomes launchable on the next tick (this tick's `issues` view is
	// already stale for it).
	s.reconcileFinishedTickets(context.Background(), board, runs.RunStore(), issues)
	ready := make([]*native.Issue, 0)
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		// Board-side gate: bot + StateReady + blockers all StateDone.
		if !native.CanLaunch(board, iss) {
			continue
		}
		if !pipelineTicketLaunchable(context.Background(), runs.RunStore(), iss) {
			continue
		}
		ready = append(ready, iss)
	}
	sortReadyTickets(ready)

	if len(ready) == 0 {
		// Nothing to admit — bail BEFORE the reservation set, which lists the
		// whole board and the whole run store. Its 1s memo is shorter than the
		// admission interval, so it is always cold at the tick: computing it
		// unconditionally would make an idle studio with a large store pay a
		// full run-store scan every interval, forever, for no decision.
		return
	}
	// Slots held open for pipelines that died and need a human. Computed once
	// per tick (the provider memoizes anyway); runview.Service.Launch remains
	// the real authority — this gate exists to preserve launch ORDER and to
	// avoid burning a claim on a ticket the queue would only park.
	reservedSet := s.pipelineReservedSet(board, runs)
	for _, iss := range ready {
		st := runs.PipelineConcurrency()
		// A ticket that holds a reservation is spending its OWN slot here, so
		// its entry must not count against it — otherwise the needs-attention
		// card is refused by the very slot it is holding for its restart.
		_, holdsOwn := reservedSet[iss.ID]
		reserved := pipelineReservedForGate(st.Reserved, st.Max, holdsOwn)
		if st.Enabled && st.Active+reserved >= st.Max {
			// `continue`, not `return`: reservations are KEYED, so a
			// lower-priority ticket further down this list may own the very
			// slot being counted against the one in hand. Bailing out of the
			// loop would leave its own reserved slot unusable until the tick
			// after the higher-priority ticket resolves.
			continue
		}
		s.launchReadyTicket(runs, board, iss)
	}
}

// sortReadyTickets orders the launch candidates the admission loop submits:
// highest Priority first (the operator's ranking dial, same field /board
// sorts on), oldest CreatedAt as the tie-break so equal-priority tickets
// launch first-come-first-served.
//
// This is the RANKING policy, not the whole "which ticket goes next" answer:
// a ticket holding its own reserved slot can start while a higher-priority
// one waits (see the gate above), and blocked tickets never reach this list
// at all (native.CanLaunch filters first). The studio's Opened sort mirrors
// both — blocked-last, then this order; keep the three aligned
// (lessIssueByPriorityThenAge here, compareLaunchOrder in
// studio/src/views/PipelineBoard/cardPredicates.ts).
func sortReadyTickets(ready []*native.Issue) {
	sort.SliceStable(ready, func(i, j int) bool {
		return lessIssueByPriorityThenAge(ready[i], ready[j])
	})
}

// lessIssueByPriorityThenAge is the single Go definition of the pipeline
// launch order, shared by the admission loop and the board projection so the
// list the operator reads matches the order the server actually launches in.
// Mirrored in TypeScript by compareLaunchOrder.
func lessIssueByPriorityThenAge(a, b *native.Issue) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// pipelineTicketLaunchable reports whether a ready ticket has no run that is
// active or already finished — i.e. it is fresh, or its last run failed and
// the operator retried it to Ready.
func pipelineTicketLaunchable(ctx context.Context, rs store.RunStore, iss *native.Issue) bool {
	if rs == nil {
		return true
	}
	if iss.LastRunID == "" {
		return true
	}
	r, err := rs.LoadRun(ctx, iss.LastRunID)
	if err != nil {
		if store.RunAbsent(err) {
			// Pruned, or deleted behind a durable tombstone: both are
			// PROOF of absence, not lack of information — refusing them
			// bricked a ticket whose operator deleted its run, with no
			// studio exit (delete does not clear the card's LastRunID).
			return true
		}
		// No information is not no run: a run record that EXISTS but
		// cannot be read may be alive. The dispatcher's
		// lastRunHoldBeforeClaim HOLDS on exactly this input — failing
		// open here made the two authorities disagree on the same card
		// and minted a sibling for a live run on a store blip.
		return false
	}
	if r == nil {
		return true
	}
	switch r.Status {
	case store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusQueued,
		store.RunStatusFinished:
		return false
	default:
		// failed / failed_resumable / cancelled → retry-able.
		return true
	}
}

// reconcileFinishedTickets files tickets whose last run reached a clean
// finish into done. launchTicketNow moves a ticket to in_progress at launch
// and nothing ever moves it out: the run's terminal status drives the
// /pipelines column, but the TICKET state is what hard blockers count
// (native.BlockerSatisfied accepts only done) — so without this sweep a
// finished ticket strands in in_progress forever and every dependent parks
// in waiting_deps. Mirrors the cloud board dispatcher's processCard
// (success → doneState) and the local dispatcher's finishRun. Only a clean
// finish closes the ticket: failed/cancelled runs stay with the operator
// (needs-attention retry, or close), and an unreadable run record is left
// alone (best-effort).
func (s *Server) reconcileFinishedTickets(ctx context.Context, board native.BoardStore, rs store.RunStore, issues []*native.Issue) {
	if rs == nil {
		return
	}
	// First pass: file tickets whose pointer run finished, and collect
	// the stuck ones (in_progress with a TERMINAL-but-not-finished
	// pointer) for fork adoption. One LoadRun per ticket — the
	// pre-existing per-tick cost, NOT a store scan.
	var stuck []*native.Issue
	pointers := map[string]*store.Run{}
	for _, iss := range issues {
		if iss == nil || iss.State != native.StateInProgress || iss.LastRunID == "" {
			continue
		}
		r, err := rs.LoadRun(ctx, iss.LastRunID)
		if err != nil || r == nil {
			continue
		}
		if r.Status != store.RunStatusFinished {
			if r.Status.IsTerminal() {
				stuck = append(stuck, iss)
				pointers[iss.ID] = r
			}
			continue
		}
		s.fileFinishedTicket(board, iss, r.ID)
	}
	if len(stuck) == 0 {
		// No stuck ticket means no fork adoption is possible. Bailing
		// here keeps an idle board at ZERO store scans per tick — the
		// same guard admitReadyPipelines applies below, and it must hold
		// even though this sweep runs before it.
		return
	}
	// The pointer may have been superseded by a recovery fork: a fork
	// never becomes LastRunID on its own (the dispatcher only stamps
	// its own attempts), so a finished one would strand the ticket in
	// in_progress forever — the card reads Closed while every dependent
	// parks in waiting_deps. The index is shared by every stuck ticket
	// and rebuilt at most once per finishedForksIndexTTL.
	forks := s.finishedForksByIssue(ctx, rs)
	for _, iss := range stuck {
		fork := newestFinishedIssueFork(forks[iss.ID], pointers[iss.ID])
		if fork == nil {
			continue
		}
		// Adopt the fork as the current attempt so the pointer converges
		// with what the card already shows. The fork's own workdir is
		// stamped too — unlike launch-time call sites passing "", the run
		// has already executed, and LastWorkdir feeds the studio's
		// inspect-the-diff link.
		if err := board.SetLastRun(iss.ID, fork.ID, fork.WorkDir); err != nil {
			s.logger.Warn("pipeline admission: adopt finished fork %s for ticket %s: %v", fork.ID, iss.ID, err)
			continue
		}
		s.fileFinishedTicket(board, iss, fork.ID)
	}
}

// fileFinishedTicket moves the ticket to done (cascading the waiting_deps
// promotion of its dependents) and logs the run the ticket was ACTUALLY
// filed for — after a fork adoption that is the fork, not the dead
// parent iss.LastRunID still names (the board mutates its own copy of
// the issue on SetLastRun).
func (s *Server) fileFinishedTicket(board native.BoardStore, iss *native.Issue, runID string) {
	_, changed, err := board.SetStateFrom(iss.ID, iss.State, native.StateDone)
	if err != nil {
		s.logger.Warn("pipeline admission: file finished ticket %s (run %s): %v", iss.ID, runID, err)
		return
	}
	if !changed {
		// The CAS found the card already moved out of the state the sweep
		// saw — an operator's (or the bot's) decision that predates this
		// filing. Say THAT: an operator debugging a ticket that never
		// reached done must not read a filing that did not happen.
		s.logger.Info("pipeline admission: ticket %s finished cleanly (run %s) but already left %s — leaving it where it was moved", iss.ID, runID, iss.State)
		return
	}
	s.logger.Info("pipeline admission: ticket %s finished cleanly (run %s) — filed as done", iss.ID, runID)
}

// finishedForksIndexTTL bounds how often the by-issue index of finished
// recovery forks is rebuilt while at least one ticket is stuck on a
// failed pointer: one full store scan per TTL instead of one per stuck
// ticket per 2s admission tick.
const finishedForksIndexTTL = 30 * time.Second

// finishedForksByIssue indexes every fork (ForkedFrom != "") that
// ACTUALLY ran to completion, keyed by its Source.IssueID. FinishedAt
// must be set: Fork() parks every child as cancelled via SaveRun without
// it, and a parked shell has delivered nothing. Memoized for
// finishedForksIndexTTL; the caller's stuck-ticket bail keeps an idle
// board from ever reaching here.
func (s *Server) finishedForksByIssue(ctx context.Context, rs store.RunStore) map[string][]*store.Run {
	s.finishedForksMu.Lock()
	defer s.finishedForksMu.Unlock()
	if s.finishedForks != nil && time.Since(s.finishedForksAt) < finishedForksIndexTTL {
		return s.finishedForks
	}
	byIssue := map[string][]*store.Run{}
	ids, err := rs.ListRuns(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("pipeline admission: list runs for the fork index: %v", err)
		}
		return byIssue
	}
	for _, id := range ids {
		r, err := rs.LoadRun(ctx, id)
		if err != nil || r == nil {
			continue
		}
		if r.ForkedFrom == "" || r.Source == nil || r.Source.IssueID == "" {
			continue
		}
		if r.Status != store.RunStatusFinished || r.FinishedAt == nil {
			continue
		}
		byIssue[r.Source.IssueID] = append(byIssue[r.Source.IssueID], r)
	}
	s.finishedForks = byIssue
	s.finishedForksAt = time.Now()
	return byIssue
}

// newestFinishedIssueFork picks the newest candidate newer than the run
// the ticket's pointer names — anything older belongs to a previous
// attempt chain. Candidates are pre-filtered by finishedForksByIssue.
// Returns nil when no fork qualifies.
func newestFinishedIssueFork(candidates []*store.Run, current *store.Run) *store.Run {
	var best *store.Run
	for _, r := range candidates {
		if !r.CreatedAt.After(current.CreatedAt) {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

// warnAdmissionSkipOnce logs a ready ticket whose bot the current catalog
// can't launch (unknown or disabled), deduped per (ticket, bot) so the 2s
// admission tick doesn't repeat it. A later tick with a DIFFERENT bot (the
// ticket was re-stamped) or a resolvable one re-arms the warning.
func (s *Server) warnAdmissionSkipOnce(ticketID, bot string, found bool) {
	s.admissionSkipMu.Lock()
	defer s.admissionSkipMu.Unlock()
	if s.admissionSkipWarned == nil {
		s.admissionSkipWarned = make(map[string]string)
	}
	if s.admissionSkipWarned[ticketID] == bot {
		return
	}
	s.admissionSkipWarned[ticketID] = bot
	reason := "is disabled"
	if !found {
		reason = "is not in the current workspace's catalog"
	}
	s.logger.Warn("pipeline admission: ticket %s stays in Ready — bot %q %s", ticketID, bot, reason)
}

// launchReadyTicket moves a ready ticket out of StateReady (so a slow launch
// can't be double-picked by the next tick), launches its bot through the
// concurrency gate, and stamps the resulting run onto the ticket so the
// projection folds them into one card. A launch failure reverts the ticket
// to Backlog.
func (s *Server) launchReadyTicket(runs *runview.Service, board native.BoardStore, iss *native.Issue) {
	// Atomically claim the Ready ticket before launching so a live dispatcher
	// and this admission loop can't both win the same one in the check-then-act
	// window (PR #193 M2). The CAS also refuses a card under a live claim —
	// the dispatcher's own in_progress move lands after its claim, so state
	// alone never proves a Ready card is free. Both board twins implement
	// it; a backend without it degrades to the best-effort SetState claim
	// inside launchTicketNow, which is exactly the double-launch window.
	// When the CAS wins, launchTicketNow's own SetState(InProgress) is an
	// idempotent no-op (already in that state).
	if claimer := native.AsLaunchClaimer(board); claimer != nil {
		_, won, err := claimer.ClaimForLaunch(iss.ID)
		if err != nil {
			s.logger.Warn("pipeline admission: claim ticket %s: %v", iss.ID, err)
			return
		}
		if !won {
			// Another launcher (dispatcher or a concurrent tick) took it first.
			return
		}
	}
	// The loop is local-only (pipelineAdmissionEnabled refuses cloud), so
	// the board it drives carries no tenant: the bot resolves
	// platform-over-baked with an empty team, and there is none to invent.
	if _, err := s.launchTicketNow(context.Background(), "", runs, board, iss); err != nil {
		// launchTicketNow already logged the specifics; the loop is
		// best-effort and simply retries on the next tick.
		return
	}
}

// boardNow resolves the instant a lease is measured against. The lease
// is stamped with the DATABASE clock ($$NOW), so comparing it to this
// pod's clock re-opens the cross-clock hole from the other end: a pod
// running Δ fast reads every lease younger than Δ as lapsed and admits a
// launch past a LIVE holder (at Δ ≥ the lease duration the guard is
// fully disarmed). Backends without a server clock (the single-process
// FS twin) fall back to the local one, which is then the only clock.
func (s *Server) boardNow(board native.BoardStore) time.Time {
	reason := ""
	defer func() {
		// Degradation is LOGGED, on its edge — the reaper's own rule ("a
		// watchdog silently measuring against a suspect clock is worse
		// than one that logs its degradation"), copied here WITH the
		// property this time. The exemption keys on the CONCRETE FS twin
		// (single process, one clock — its fallback is the design), never
		// on method presence: keyed on the method, a decorator around the
		// cloud board that drops ServerNow would lose the server clock AND
		// the warn in the same move — the silent disarm this log exists
		// to catch.
		if _, isFS := board.(*native.Store); isFS {
			return
		}
		s.stateMu.Lock()
		prev := s.boardClockWarned
		s.boardClockWarned = reason
		s.stateMu.Unlock()
		if reason != prev && reason != "" {
			s.logger.Warn("pipeline admission: board clock unavailable (%s) — measuring the claim lease against this pod's clock", reason)
		}
		// The recovery edge is logged too — like its latch siblings
		// (noteRunReadFailure): a clock that came back silently leaves the
		// last message on record saying the guard is degraded.
		if reason == "" && prev != "" {
			s.logger.Info("pipeline admission: board clock recovered — the claim-lease guard measures against the server clock again")
		}
	}()
	if sn, ok := board.(interface {
		ServerNow(context.Context) (time.Time, error)
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv, err := sn.ServerNow(ctx)
		switch {
		case err != nil:
			reason = err.Error()
		case srv.IsZero():
			reason = "empty collection"
		default:
			return srv
		}
	} else {
		reason = fmt.Sprintf("board type %T exposes no server clock", board)
	}
	return time.Now().UTC()
}

// The production board type must keep exposing the server clock. This pin
// holds the METHOD on *boardmongo.Store only — a decorator inserted at the
// CloudBoardFor return site still compiles past it; the runtime warn in
// boardNow (whose exemption keys on the FS twin, not on method presence)
// is what makes that wrapper loud instead of a silent disarm.
var _ interface {
	ServerNow(context.Context) (time.Time, error)
} = (*boardmongo.Store)(nil)

// pipelineBot is one bot resolution serving BOTH halves of the pipelines
// lane: the metadata its admission checks read — the canonical name a card
// is stamped with, and whether the bot is enabled — and the bundle its
// launch runs. Resolving the two separately is what let the control center
// admit a card on the baked catalog's metadata and then launch the baked
// catalog's bundle for a team that had forked that very bot.
type pipelineBot struct {
	// Name is the canonical bot id: what the card records and what the
	// upsert key is built from. A card may carry a tolerated spelling
	// (`feature_dev` for a `feature-dev` bundle) — every tier resolves it,
	// and the board keeps the canonical one.
	Name string
	// Enabled is the catalog-visibility decision: the manifest `enabled:`
	// default, composed with the workspace overlay for baked entries.
	Enabled bool
	// Launch is the launch-ready bundle. The caller OWNS it and must
	// Cleanup() it — including the check-only callers, which pay the
	// resolution so that a card that cannot be launched is never created
	// (the admitBoardCard bargain).
	Launch *launchBot
}

// resolvePipelineBot resolves the bot a pipeline ticket names through the
// full tier order — teamID's own botsource row, then a platform override,
// then the baked catalog — and answers with what BOTH halves of the lane
// need. (zero, false, nil) is a genuine absence each caller maps to its own
// refusal; an error is a resolver failure, never a fall-through to a bundle
// nobody chose.
//
// teamID is the tenant the CARD belongs to. The cloud pipeline board is
// selected from the request's active team (cloudBoardResolve), so a card
// carried by a team that forked its bot must run that fork, and a bot only
// that team authored must be cardable at all — a stored row has no
// filesystem path to launch from. It is a parameter and not a ctx read for
// bot_resolver.go's reason: the tier must follow "who is this launch for",
// not whichever tenant the ctx happens to carry. Empty is legitimate for
// the local studio, whose board has no tenancy.
func (s *Server) resolvePipelineBot(ctx context.Context, teamID, botID string) (pipelineBot, bool, error) {
	lb, err := s.resolveBotTiered(ctx, teamID, botID, "")
	if err != nil {
		return pipelineBot{}, false, err
	}
	if lb == nil {
		// "Nothing resolved" is not always "no such bot": the baked tier
		// reports an UNREADABLE catalog as an absence — resolveBotTieredRaw
		// collapses every ResolveBotPath failure into (nil, nil), and one
		// malformed manifest.yaml anywhere under the discovery roots fails
		// the whole walk, for every bot. The metadata read crosses the same
		// catalog and propagates that error, so ask it before answering
		// "absent": the operator is then told his catalog will not parse
		// (what findBot used to say, before this lane went through the
		// tiers) instead of being told the bot he just typed does not exist.
		// A genuine absence stays a genuine absence. It is the SAME read the
		// found path makes (the sweep's tenant-aware form, so this probe
		// cannot answer from a tier the launch would not have served); only
		// its error is used here, since nothing resolved to describe.
		if _, _, catErr := s.effectiveFindByNameForTeam(ctx, teamID, botID); catErr != nil {
			return pipelineBot{}, false, catErr
		}
		return pipelineBot{}, false, nil
	}
	// The metadata half of the SAME resolution. It must describe the
	// artifact the launch SELECTED, so a STORED tier answers for itself:
	// the row that served, read by its own tenant and canonical slug, which
	// is the same live teamBotRow read the launch made.
	//
	// The platform tier is why this is not just `teamID`. Its launch read is
	// live (botSources.GetBySlug), while effectiveFindByName's overlay comes
	// from the 30s platformBotSetCached set, invalidated only on the replica
	// that served the write. Asking for the platform tenant by name lands on
	// teamBotRow — live — instead, so the two halves cannot straddle that
	// window: a freshly pushed override no longer reads as "a launchable
	// bundle nothing describes" (a 500 on card create), and an override with
	// its own `enabled:` no longer inherits the flag of the baked twin it
	// shadows. Which is this chokepoint's whole point: metadata from one
	// tier and a bundle from another is the divergence it exists to close.
	//
	// A stored tier reads STRICTLY for the same reason. effectiveFindByNameForTeam
	// falls THROUGH when the row it found will not materialize — a store blip
	// on the second read, a bundle discovery cannot describe (no manifest.yaml
	// and more than one workflow), a manifest whose schema_version this build
	// no longer accepts — and its fall-through lands on the origin the fork
	// replaces: exactly the pairing the paragraph above forbids, arriving
	// through a helper instead of a tier choice. Here it is an error naming
	// the row the operator has to fix. (A hand-broken manifest is NOT on that
	// list: botsource.Validate decodes it at write time.)
	entry, found, err := s.pipelineBotEntry(ctx, botID, lb)
	if err != nil {
		lb.Cleanup()
		return pipelineBot{}, false, err
	}
	if !found {
		// The two halves disagree: a launchable bundle nothing describes.
		// Say so. Guessing "enabled" is how a disabled bot launches;
		// guessing "absent" is how a launchable bot becomes uncardable.
		lb.Cleanup()
		return pipelineBot{}, false, fmt.Errorf(
			"bot %q resolves to a launchable bundle on the %s tier but no catalog metadata describes it", botID, lb.Origin)
	}
	// The catalog tier answers to a tolerated spelling and hands back the
	// REQUESTED one; the canonical name is the metadata's. Carry it into
	// the launch too, so the card, the upsert key and the run all agree.
	lb.BotID = entry.Name
	return pipelineBot{Name: entry.Name, Enabled: entry.Enabled, Launch: lb}, true, nil
}

// pipelineBotEntry reads the catalog metadata describing the bundle `lb`
// selected — from the tier that selected it, and from no other. A stored row
// (team or platform) answers through its own live row; the baked tier answers
// from the baked catalog with the platform overlay OFF.
//
// That last part is the leg the tier order cannot close by itself. Reaching
// the baked tier means the live store served neither a team row nor a
// platform one, while platformBotSetCached — a 30s TTL invalidated only on
// the replica that wrote — may still carry an override that was just DELETED.
// The overlaid read would then describe the running baked bundle with the
// deleted override's `enabled` and name: a disabled override making an
// enabled bot unlaunchable, or worse the reverse. Each tier answering for
// itself is the whole shape of this chokepoint; this is its third leg.
//
// It takes no team: `lb` already names the tier that won, and a team id here
// could only be used to consult a tier that did not.
func (s *Server) pipelineBotEntry(ctx context.Context, botID string, lb *launchBot) (botregistry.EntryWithSchema, bool, error) {
	if lb.Ref != nil && lb.Ref.TenantID != "" {
		return s.storedBotEntry(ctx, lb.Ref.TenantID, lb.Ref.Slug)
	}
	return s.bakedFindByName(botID)
}

// launchTicketNow claims a ticket and launches its bot, returning the run
// id. It is the shared body of the admission loop (which ignores the error
// and retries next tick) and of the operator's explicit "launch now" drag,
// which needs the failure reported back over HTTP. The bot-not-in-catalog
// case is a *skip*, not a failure, for the loop — hence the dedicated
// error the caller can distinguish.
//
// ctx scopes the bot RESOLUTION (a store read in cloud, where this endpoint
// is reachable); the launch itself keeps its own background context, so a
// client that hangs up mid-request cannot cancel a run already in flight.
func (s *Server) launchTicketNow(ctx context.Context, teamID string, runs *runview.Service, board native.BoardStore, iss *native.Issue) (string, error) {
	// A ticket held under a LIVE claim already has a launcher — the
	// dispatcher wins with the CLAIM, and its move out of Ready is
	// offloaded, so the state alone cannot say the ticket is free. This is
	// the choke point BOTH callers cross (the admission loop's
	// ClaimForLaunch refuses the same card earlier — atomically; the
	// operator's explicit "launch now" reaches here directly), so the
	// guard lives here, on a FRESH read: launching past it minted a second
	// run while the claim holder was mid-launch. Read-then-move, not a
	// CAS, because this path must stay legal for a non-Ready
	// needs-attention relaunch (ClaimForLaunch requires Ready); the
	// residual µs window is the documented V1 shape, down from the whole
	// launch flight.
	//
	// LIVE is the lease, not the marker. The dispatcher PARKS an
	// awaiting-input card with its claim retained and its heartbeat
	// STOPPED (ADR-014), and DecideStuckCard conserves that claim for
	// ever on a paused/cancelled run — so a bare `Claim != ""` refused
	// the operator's own escape hatch (this endpoint has no Ready
	// precondition precisely to serve it) and pointed them at a watchdog
	// that would never come. A claim with NO lease is admitted too: that
	// is what a release N-1 binary writes, the population the
	// expand/contract rollout GUARANTEES during release N — and it has
	// zero release path (the FS reaper has no un-leased arm at all; the
	// cloud one only lists it gate ON), so refusing it bricked the card
	// for ever on both twins. The cost is a seconds-wide double-launch
	// window against a legacy daemon mid-launch, during the mixed-fleet
	// release only; ClaimForLaunch (marker-unconditional) still protects
	// the admission loop, and pipelineTicketLaunchable the active-run
	// case.
	cur, err := board.Get(iss.ID)
	if err != nil {
		return "", fmt.Errorf("read ticket: %w", err)
	}
	if cur.Claim != "" && !cur.ClaimLeaseUntil.IsZero() && cur.ClaimLeaseUntil.After(s.boardNow(board)) {
		return "", fmt.Errorf("ticket %s is claimed by %q under a live lease — its launcher is already on it; wait for the lease to lapse (or for the watchdog to reclaim it)", iss.ID, cur.Claim)
	}
	bot, found, err := s.resolvePipelineBot(ctx, teamID, iss.Bot)
	if err != nil {
		s.logger.Warn("pipeline admission: resolve bot %q: %v", iss.Bot, err)
		return "", fmt.Errorf("resolve bot %q: %w", iss.Bot, err)
	}
	// The materialized bundle dir belongs to this call: Launch compiles from
	// it synchronously (compileForLaunch, on both the cloud-publisher and
	// the in-process path) before returning, and nothing downstream reads
	// it. The defer sits ABOVE the skip so a DISABLED bot's bundle is
	// reclaimed too — a resolution that FOUND the bot materialized it (and
	// in cloud snapshotted the whole collection to a second temp dir), and
	// nothing ever comes back for it. Cleanup is nil-safe, so the not-found
	// branch costs nothing.
	//
	// The one path where "synchronously" does not hold is runview's LOCAL
	// pipeline-concurrency queue: over the cap, admitOrEnqueue keeps the
	// whole LaunchSpec and startQueuedRun compiles from it later, after this
	// defer has run. That borrow predates this lane (every caller of
	// Launch defers Cleanup the same way) and the fix belongs to the queue,
	// which must own what it retains — not here, where the alternative is
	// leaking the dir forever. Cloud is unaffected: the publisher path
	// returns before the queue branch, and s.pipelineQueue is nil whenever
	// a publisher is wired.
	defer bot.Launch.Cleanup()
	if !found || !bot.Enabled {
		// Unknown/disabled bot: leave the ticket in Ready, surfaced as-is.
		// Say so (once per ticket+bot, not every tick) — after a studio
		// project switch the boot-scoped board can reference bots the
		// current workspace's catalog no longer resolves, and a silent
		// skip reads as a stuck pipeline.
		s.warnAdmissionSkipOnce(iss.ID, iss.Bot, found)
		// The two cases are not the same fix, and this string is what the
		// operator reads: launch-now returns it over HTTP. It must also name
		// the tiers the resolution actually covers now — "this workspace's
		// catalog" describes the loop's local board, but a cloud card that
		// resolves nowhere failed on its TEAM's bots too.
		if !found {
			return "", fmt.Errorf("bot %q resolves on none of the tiers this card can launch from — the team's own bots, a platform override, or the baked catalog", iss.Bot)
		}
		return "", fmt.Errorf("bot %q is disabled", iss.Bot)
	}
	// Leave the launch column BEFORE launching so the next tick won't
	// re-pick this ticket while Launch is in flight. StateInProgress is not
	// StateReady, so admitReadyPipelines skips it; the run's status then
	// drives the column.
	//
	// The CAS is anchored on the state this call READ (cur), never on a
	// hardcoded Ready: this endpoint deliberately has NO ready precondition
	// — it is the operator's relaunch of a needs-attention card, which the
	// Opened lane folds inbox/waiting_deps into — so a Ready-anchored CAS
	// was a SILENT no-op (SetStateFrom returns changed=false, nil on drift)
	// for every non-Ready launch. The run then started while the card sat
	// where it was: reconcileFinishedTickets only files a ticket that is
	// in_progress, so it was never filed done, and BlockerSatisfied accepts
	// only done — its dependents parked in waiting_deps for ever.
	//
	// changed==false is a REFUSAL the caller surfaces, not something to
	// walk past: the ticket moved between the read and the write, and
	// whoever moved it may already be launching it. A terminal source is
	// refused too — SetStateFrom raises ErrTerminalStateExit rather than
	// resurrecting a closed card behind the operator's back; reopening is
	// their explicit gesture, not a launch's side effect.
	sourceState := cur.State
	if sourceState != native.StateInProgress {
		_, changed, err := board.SetStateFrom(iss.ID, sourceState, native.StateInProgress)
		if err != nil {
			s.logger.Warn("pipeline admission: claim ticket %s: %v", iss.ID, err)
			return "", fmt.Errorf("claim ticket: %w", err)
		}
		if !changed {
			return "", fmt.Errorf("ticket %s moved out of %q while it was being launched — nothing was started; re-read the board and retry", iss.ID, sourceState)
		}
	}
	spec := runview.LaunchSpec{
		Vars: iss.BotArgs,
		// Stamp the ticket onto the run IMMEDIATELY. Without this the run is
		// undiscoverable from its ticket until SetLastRun lands below — and
		// between the SetState above and that stamp sit compileForLaunch,
		// BuildExecutor and worktree creation, i.e. seconds during which a
		// live run is invisible to close, reset AND delete. issueRunRoots
		// already looks for Source.IssueID; nothing was ever setting it here.
		//
		// Kind is deliberately LEFT EMPTY. deriveSourceKind
		// (pkg/runview/service_runs.go) returns Source.Kind verbatim when set,
		// so stamping RunSourceKindDispatcher here would relabel every
		// studio-launched ticket run as "dispatcher" in the run list's
		// grouping and filtering. It is a studio launch; only the issue link
		// is new information.
		SourceRef: &store.RunSource{IssueID: iss.ID},
		// Consumed by the concurrency gate: a needs-attention ticket holds a
		// reserved slot, and this is what lets its own relaunch spend that
		// reservation instead of being refused by it.
		PipelineTicketID: iss.ID,
	}
	// The resolved bundle carries the source, the compile dir, the runner
	// ref and the tier that served it — so a run says which tier it came
	// from here exactly as on the other launch surfaces.
	bot.Launch.Stamp(&spec)
	res, err := runs.Launch(context.Background(), spec)
	if err != nil {
		s.logger.Warn("pipeline admission: launch ticket %s: %v", iss.ID, err)
		// Put the ticket back where it came FROM, not in a fixed column: a
		// card launched from blocked/waiting_deps that failed to start is
		// not a backlog item, and dumping it in Backlog silently moved it
		// somewhere its operator never put it. The one exception is Ready,
		// which the admission loop re-picks every tick — parking it in
		// Backlog is what breaks that relaunch loop, and is why the fixed
		// target was there.
		revertTo := sourceState
		if revertTo == native.StateReady {
			revertTo = native.StateInbox
		}
		if revertTo != native.StateInProgress {
			if _, _, rErr := board.SetStateFrom(iss.ID, native.StateInProgress, revertTo); rErr != nil {
				s.logger.Warn("pipeline admission: revert ticket %s: %v", iss.ID, rErr)
			}
		}
		return "", fmt.Errorf("launch: %w", err)
	}
	if err := board.SetLastRun(iss.ID, res.RunID, ""); err != nil {
		s.logger.Warn("pipeline admission: link run %s to ticket %s: %v", res.RunID, iss.ID, err)
	}
	s.logger.Info("pipeline admission: started ticket %s as run %s (bot %s, %s tier)", iss.ID, res.RunID, bot.Name, bot.Launch.Tier())
	return res.RunID, nil
}
